//go:build linux

package shaper

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// nlTimeout bounds every answer: a tick never waits on the kernel for long.
const nlTimeout = 5 * time.Second

// rtnl is a route netlink socket in the shaper's network namespace.
type rtnl struct {
	fd  int
	pid uint32
	seq uint32
}

func dialRoute() (*rtnl, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("opening a route netlink socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("binding the route netlink socket: %w", err)
	}
	tv := unix.NsecToTimeval(nlTimeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	nl, ok := sa.(*unix.SockaddrNetlink)
	if !ok {
		_ = unix.Close(fd)
		return nil, errors.New("the route netlink socket has no netlink address")
	}
	return &rtnl{fd: fd, pid: nl.Pid}, nil
}

func (c *rtnl) close() { _ = unix.Close(c.fd) }

// do sends one request and collects its answer: for a dump every message up
// to NLMSG_DONE, else the messages up to the acknowledgement the request asks
// for. An error answer is the kernel's errno.
func (c *rtnl) do(typ, flags uint16, body []byte) ([]nlMsg, error) {
	c.seq++
	dump := flags&nlmFDump == nlmFDump
	flags |= nlmFRequest
	if !dump {
		flags |= nlmFAck
	}
	req := append(header(sizeofNlMsghdr+len(body), typ, flags, c.seq, c.pid), body...)
	if err := unix.Sendto(c.fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, err
	}
	var out []nlMsg
	buf := make([]byte, 1<<16)
	for {
		n, _, err := unix.Recvfrom(c.fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				return nil, fmt.Errorf("the kernel did not answer within %s", nlTimeout)
			}
			return nil, err
		}
		msgs, seqs, pids := splitMessages(buf[:n])
		for i, m := range msgs {
			if seqs[i] != c.seq || pids[i] != c.pid {
				continue
			}
			switch m.typ {
			case nlmsgDone:
				if e := errnoOf(m.body); e < 0 {
					return nil, syscall.Errno(-e)
				}
				return out, nil
			case nlmsgError:
				if e := errnoOf(m.body); e < 0 {
					return nil, syscall.Errno(-e)
				}
				if !dump {
					return out, nil
				}
			default:
				out = append(out, nlMsg{typ: m.typ, body: append([]byte(nil), m.body...)})
			}
		}
	}
}

// links dumps the network devices of the shaper's namespace. The dump also
// gives every namespace a veth peer lives in an id here, which nsidOf reads.
func (c *rtnl) links() ([]netLink, error) {
	msgs, err := c.do(rtmGetLink, nlmFDump, make([]byte, sizeofIfInfomsg))
	if err != nil {
		return nil, fmt.Errorf("listing the network devices: %w", err)
	}
	var links []netLink
	for _, m := range msgs {
		if m.typ != rtmNewLink {
			continue
		}
		if l, ok := parseLink(m.body); ok {
			links = append(links, l)
		}
	}
	return links, nil
}

// nsidOf is the id this namespace knows pid's network namespace by; -1 when
// it has none (the process shares this namespace, or no device here leads
// there).
func (c *rtnl) nsidOf(pid int) (int32, error) {
	msgs, err := c.do(rtmGetNsid, 0, nsidRequest(pid))
	if err != nil {
		return 0, fmt.Errorf("reading the network namespace id of process %d: %w", pid, err)
	}
	for _, m := range msgs {
		if m.typ != rtmNewNsid {
			continue
		}
		if id, ok := parseNsid(m.body); ok {
			return id, nil
		}
	}
	return 0, fmt.Errorf("the kernel answered no network namespace id for process %d", pid)
}

// mirrorVeth finds the host side of the veth whose other end is in the
// network namespace of pid: the mirror's own process, PID 1 of the mirror's
// PID namespace, which the shaper's container shares.
func (c *rtnl) mirrorVeth(pid int) (netLink, error) {
	links, err := c.links()
	if err != nil {
		return netLink{}, err
	}
	nsid, err := c.nsidOf(pid)
	if err != nil {
		return netLink{}, err
	}
	if nsid < 0 {
		return netLink{}, fmt.Errorf("process %d's network namespace has no id here: it shares this namespace or no veth leads there", pid)
	}
	return peerVeth(links, nsid)
}

// rootQdisc reads a device's root qdisc with its counters (unprivileged).
// The kernel answers a GET of one qdisc only when asked to echo it (it
// unicast the answer unasked before); one that answers neither way is asked
// for the dump of every qdisc, filtered here.
func (c *rtnl) rootQdisc(ifindex int) (qdisc, error) {
	msgs, err := c.do(rtmGetQdisc, nlmFEcho, tcMsg(ifindex))
	if err != nil {
		return qdisc{}, fmt.Errorf("reading the root qdisc: %w", err)
	}
	if q, ok := rootOf(msgs, ifindex); ok {
		return q, nil
	}
	if msgs, err = c.do(rtmGetQdisc, nlmFDump, make([]byte, sizeofTcMsg)); err != nil {
		return qdisc{}, fmt.Errorf("listing the qdiscs: %w", err)
	}
	if q, ok := rootOf(msgs, ifindex); ok {
		return q, nil
	}
	return qdisc{}, errors.New("reading the root qdisc: the kernel reported none")
}

// replaceCake makes CAKE the device's root qdisc at a rate, with the
// shaper's settings.
func (c *rtnl) replaceCake(ifindex int, bps float64) error {
	_, err := c.do(rtmNewQdisc, nlmFCreate|nlmFReplace, cakeRequest(ifindex, bps, true))
	return err
}

// changeCake changes only the bandwidth of the device's root CAKE.
func (c *rtnl) changeCake(ifindex int, bps float64) error {
	_, err := c.do(rtmNewQdisc, 0, cakeRequest(ifindex, bps, false))
	return err
}

// deleteRoot removes the device's root qdisc: the device goes back to the
// one the kernel gives it (noqueue for a veth).
func (c *rtnl) deleteRoot(ifindex int) error {
	_, err := c.do(rtmDelQdisc, 0, tcMsg(ifindex))
	return err
}
