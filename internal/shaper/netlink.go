package shaper

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
)

// The shaper's kernel side over rtnetlink: one CAKE root qdisc on the
// mirror's host-side veth (replace, change its bandwidth, read its counters,
// delete it) and the lookup that finds that veth. Spelled out here with the
// standard library, so the shaper's image needs no tc and agentlab no
// netlink dependency. This file builds the messages and reads the answers;
// netlink_linux.go talks to the kernel.

// Netlink message types and flags (linux/netlink.h, linux/rtnetlink.h).
const (
	nlmsgError = 2
	nlmsgDone  = 3

	rtmNewLink  = 16
	rtmGetLink  = 18
	rtmNewQdisc = 36
	rtmDelQdisc = 37
	rtmGetQdisc = 38
	rtmNewNsid  = 88
	rtmGetNsid  = 90

	nlmFRequest = 0x1
	nlmFAck     = 0x4
	nlmFEcho    = 0x8
	nlmFReplace = 0x100
	nlmFCreate  = 0x400
	nlmFDump    = 0x300

	sizeofNlMsghdr  = 16
	sizeofIfInfomsg = 16
	sizeofTcMsg     = 20
	sizeofRtGenmsg  = 4 // one byte, aligned to NLMSG_ALIGNTO

	// nlaTypeMask strips NLA_F_NESTED and NLA_F_NET_BYTEORDER off a type.
	nlaTypeMask = 0x3fff
)

// Link attributes (linux/if_link.h) and the network-namespace ids
// (linux/net_namespace.h).
const (
	iflaIfname      = 3
	iflaLinkinfo    = 18
	iflaLinkNetnsid = 37
	iflaInfoKind    = 1

	netnsaNsid = 1
	netnsaPid  = 2
)

// Traffic-control attributes (linux/rtnetlink.h, linux/gen_stats.h,
// linux/pkt_sched.h).
const (
	tcHRoot = 0xffffffff

	tcaKind    = 1
	tcaOptions = 2
	tcaStats2  = 7

	tcaStatsBasic = 1
	tcaStatsQueue = 3
	tcaStatsApp   = 4

	tcaCakeBaseRate64   = 2
	tcaCakeDiffservMode = 3
	tcaCakeFlowMode     = 5
	tcaCakeNat          = 11
	tcaCakeIngress      = 15

	cakeDiffservBesteffort = 3
	cakeFlowDualDst        = 6

	tcaCakeStatsTinStats            = 10
	tcaCakeTinStatsEcnMarkedPackets = 8
)

// cakeKind is the qdisc's name in the kernel, vethKind the mirror's link's.
const (
	cakeKind = "cake"
	vethKind = "veth"
)

var nativeEndian = binary.NativeEndian

// nlAttr is one netlink attribute: its type without the flag bits, and its
// payload.
type nlAttr struct {
	typ  uint16
	data []byte
}

// nlAlign rounds a length up to netlink's four-byte alignment.
func nlAlign(n int) int { return (n + 3) &^ 3 }

// encodeAttr is one attribute, padded.
func encodeAttr(typ uint16, data []byte) []byte {
	b := make([]byte, nlAlign(4+len(data)))
	nativeEndian.PutUint16(b[0:], uint16(4+len(data))) // #nosec G115 -- the shaper's own small attributes
	nativeEndian.PutUint16(b[2:], typ)
	copy(b[4:], data)
	return b
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	nativeEndian.PutUint32(b, v)
	return b
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	nativeEndian.PutUint64(b, v)
	return b
}

// cString is a NUL-terminated string attribute's payload.
func cString(s string) []byte { return append([]byte(s), 0) }

// parseAttrs reads a run of attributes; a truncated one ends the run.
func parseAttrs(b []byte) []nlAttr {
	var out []nlAttr
	for len(b) >= 4 {
		l := int(nativeEndian.Uint16(b[0:]))
		if l < 4 || l > len(b) {
			break
		}
		out = append(out, nlAttr{typ: nativeEndian.Uint16(b[2:]) & nlaTypeMask, data: b[4:l]})
		b = b[min(nlAlign(l), len(b)):]
	}
	return out
}

// attr is the last attribute of a type in a run; nil when there is none.
func attr(attrs []nlAttr, typ uint16) []byte {
	var data []byte
	for _, a := range attrs {
		if a.typ == typ {
			data = a.data
		}
	}
	return data
}

// attrString reads a NUL-terminated string payload.
func attrString(b []byte) string { return strings.TrimRight(string(b), "\x00") }

// header is a request's netlink header.
func header(length int, typ, flags uint16, seq, pid uint32) []byte {
	h := make([]byte, sizeofNlMsghdr)
	nativeEndian.PutUint32(h[0:], uint32(length)) // #nosec G115 -- the shaper's own small requests
	nativeEndian.PutUint16(h[4:], typ)
	nativeEndian.PutUint16(h[6:], flags)
	nativeEndian.PutUint32(h[8:], seq)
	nativeEndian.PutUint32(h[12:], pid)
	return h
}

// nlMsg is one message of an answer: its type and its payload after the
// header.
type nlMsg struct {
	typ  uint16
	body []byte
}

// splitMessages cuts a datagram into its messages, with each one's sequence
// number and port id; a truncated one ends the datagram.
func splitMessages(b []byte) (msgs []nlMsg, seqs, pids []uint32) {
	for len(b) >= sizeofNlMsghdr {
		l := int(nativeEndian.Uint32(b[0:]))
		if l < sizeofNlMsghdr || l > len(b) {
			break
		}
		msgs = append(msgs, nlMsg{typ: nativeEndian.Uint16(b[4:]), body: b[sizeofNlMsghdr:l]})
		seqs = append(seqs, nativeEndian.Uint32(b[8:]))
		pids = append(pids, nativeEndian.Uint32(b[12:]))
		b = b[min(nlAlign(l), len(b)):]
	}
	return msgs, seqs, pids
}

// errnoOf reads an NLMSG_ERROR or NLMSG_DONE payload: the negative errno the
// kernel answered, 0 for an acknowledgement.
func errnoOf(body []byte) int32 {
	if len(body) < 4 {
		return 0
	}
	return int32(nativeEndian.Uint32(body)) // #nosec G115 -- the kernel's signed error code
}

// tcMsg is a struct tcmsg for a device's root qdisc.
func tcMsg(ifindex int) []byte {
	b := make([]byte, sizeofTcMsg)
	nativeEndian.PutUint32(b[4:], uint32(ifindex)) // #nosec G115 -- an interface index
	nativeEndian.PutUint32(b[12:], tcHRoot)
	return b
}

// bytesPerSecond is CAKE's bandwidth attribute for a rate in bits per second:
// tc's bandwidth is bytes per second, 0 unlimited.
func bytesPerSecond(bps float64) uint64 {
	if bps <= 0 {
		return 0
	}
	return uint64(max(math.Round(bps/8), 1))
}

// cakeOptions is CAKE's TCA_OPTIONS payload at a rate. full adds the
// shaper's settings, which a change leaves as they are: besteffort (the
// mirror's downloads have no priorities of their own), dual-dsthost nat (fair
// per flow, and per host by its address before NAT), ingress (the downloads
// arrive here, and drops count as used bandwidth).
func cakeOptions(bps float64, full bool) []byte {
	opts := encodeAttr(tcaCakeBaseRate64, u64(bytesPerSecond(bps)))
	if full {
		opts = append(opts, encodeAttr(tcaCakeDiffservMode, u32(cakeDiffservBesteffort))...)
		opts = append(opts, encodeAttr(tcaCakeFlowMode, u32(cakeFlowDualDst))...)
		opts = append(opts, encodeAttr(tcaCakeNat, u32(1))...)
		opts = append(opts, encodeAttr(tcaCakeIngress, u32(1))...)
	}
	return opts
}

// cakeRequest is the payload of an RTM_NEWQDISC for CAKE as the root qdisc
// of a device.
func cakeRequest(ifindex int, bps float64, full bool) []byte {
	b := tcMsg(ifindex)
	b = append(b, encodeAttr(tcaKind, cString(cakeKind))...)
	return append(b, encodeAttr(tcaOptions, cakeOptions(bps, full))...)
}

// qdisc is a device's root qdisc as the kernel reports it.
type qdisc struct {
	// ifindex and parent say where it hangs: a root qdisc's parent is
	// tcHRoot.
	ifindex int
	parent  uint32
	kind    string
	// rate is CAKE's bandwidth in bits per second, 0 unlimited.
	rate float64
	// bytes is what it delivered; dropped what it dropped or ECN-marked:
	// the mirror wanted more than the cap.
	bytes, dropped uint64
}

// parseQdisc reads an RTM_NEWQDISC payload.
func parseQdisc(body []byte) (qdisc, error) {
	if len(body) < sizeofTcMsg {
		return qdisc{}, errors.New("short qdisc message")
	}
	attrs := parseAttrs(body[sizeofTcMsg:])
	q := qdisc{
		ifindex: int(int32(nativeEndian.Uint32(body[4:]))), // #nosec G115 -- the kernel's signed ifindex
		parent:  nativeEndian.Uint32(body[12:]),
		kind:    attrString(attr(attrs, tcaKind)),
	}
	if q.kind != cakeKind {
		return q, nil
	}
	if rate := attr(parseAttrs(attr(attrs, tcaOptions)), tcaCakeBaseRate64); len(rate) >= 8 {
		q.rate = float64(nativeEndian.Uint64(rate)) * 8
	}
	stats := parseAttrs(attr(attrs, tcaStats2))
	if basic := attr(stats, tcaStatsBasic); len(basic) >= 8 {
		q.bytes = nativeEndian.Uint64(basic)
	}
	if queue := attr(stats, tcaStatsQueue); len(queue) >= 12 {
		q.dropped = uint64(nativeEndian.Uint32(queue[8:]))
	}
	for _, tin := range parseAttrs(attr(parseAttrs(attr(stats, tcaStatsApp)), tcaCakeStatsTinStats)) {
		if marks := attr(parseAttrs(tin.data), tcaCakeTinStatsEcnMarkedPackets); len(marks) >= 4 {
			q.dropped += uint64(nativeEndian.Uint32(marks))
		}
	}
	return q, nil
}

// rootOf picks a device's root qdisc out of an answer.
func rootOf(msgs []nlMsg, ifindex int) (qdisc, bool) {
	for _, m := range msgs {
		if m.typ != rtmNewQdisc {
			continue
		}
		if q, err := parseQdisc(m.body); err == nil && q.ifindex == ifindex && q.parent == tcHRoot {
			return q, true
		}
	}
	return qdisc{}, false
}

// netLink is a network device as the link dump reports it.
type netLink struct {
	index int
	name  string
	kind  string
	// netnsid is the id, in this namespace, of the namespace the device's
	// peer lives in; hasNetnsid is false for a device without one there.
	netnsid    int32
	hasNetnsid bool
}

// parseLink reads an RTM_NEWLINK payload.
func parseLink(body []byte) (netLink, bool) {
	if len(body) < sizeofIfInfomsg {
		return netLink{}, false
	}
	l := netLink{index: int(int32(nativeEndian.Uint32(body[4:])))} // #nosec G115 -- the kernel's signed ifindex
	attrs := parseAttrs(body[sizeofIfInfomsg:])
	l.name = attrString(attr(attrs, iflaIfname))
	l.kind = attrString(attr(parseAttrs(attr(attrs, iflaLinkinfo)), iflaInfoKind))
	if id := attr(attrs, iflaLinkNetnsid); len(id) >= 4 {
		l.netnsid, l.hasNetnsid = int32(nativeEndian.Uint32(id)), true // #nosec G115 -- a signed nsid
	}
	return l, true
}

// nsidRequest asks for the id this namespace knows another namespace by,
// named by a process in it.
func nsidRequest(pid int) []byte {
	return append(make([]byte, sizeofRtGenmsg), encodeAttr(netnsaPid, u32(uint32(pid)))...) // #nosec G115 -- a process id
}

// parseNsid reads an RTM_NEWNSID payload; -1 is "not assigned".
func parseNsid(body []byte) (int32, bool) {
	if len(body) < sizeofRtGenmsg {
		return 0, false
	}
	id := attr(parseAttrs(body[sizeofRtGenmsg:]), netnsaNsid)
	if len(id) < 4 {
		return 0, false
	}
	return int32(nativeEndian.Uint32(id)), true // #nosec G115 -- a signed nsid
}

// peerVeth picks, from a link dump, the veth whose peer lives in the
// namespace nsid: the host side of the mirror's eth0.
func peerVeth(links []netLink, nsid int32) (netLink, error) {
	var found []netLink
	for _, l := range links {
		if l.kind == vethKind && l.hasNetnsid && l.netnsid == nsid {
			found = append(found, l)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return netLink{}, errors.New("no veth here has its peer in the mirror's network namespace")
	default:
		names := make([]string, len(found))
		for i, l := range found {
			names[i] = l.name
		}
		return netLink{}, fmt.Errorf("the mirror's network namespace has %d veths here (%s), want one", len(found), strings.Join(names, ", "))
	}
}
