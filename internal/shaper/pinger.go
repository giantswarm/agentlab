package shaper

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

// The latency probes: one ICMP echo to every reflector per interval, over a
// raw socket (CAP_NET_RAW, which Podman's default capabilities lack and the
// shaper's container asks for).

func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func echoRequest(id, seq uint16) []byte {
	b := make([]byte, 8+16)
	b[0] = 8 // echo request
	binary.BigEndian.PutUint16(b[4:], id)
	binary.BigEndian.PutUint16(b[6:], seq)
	binary.BigEndian.PutUint16(b[2:], icmpChecksum(b))
	return b
}

func parseEchoReply(b []byte) (id, seq uint16, ok bool) {
	if len(b) < 8 || b[0] != 0 || b[1] != 0 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint16(b[4:]), binary.BigEndian.Uint16(b[6:]), true
}

type pendingProbe struct {
	target int
	sent   time.Time
}

// pinger sends an ICMP echo to every reflector once per interval and collects
// the answers per reflector until the next tick takes them. Its echo id is
// random: every raw ICMP socket on the host sees every echo reply, a host
// daemon's probes included, and the id keeps them apart.
type pinger struct {
	conn    net.PacketConn
	targets []netip.Addr
	id      uint16

	mu      sync.Mutex
	seq     uint16
	pending map[uint16]pendingProbe
	acc     []probeAcc
}

func newPinger(targets []netip.Addr) (*pinger, error) {
	c, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("opening the ICMP socket for the latency probes (needs CAP_NET_RAW): %w", err)
	}
	var id [2]byte
	_, _ = rand.Read(id[:])
	return &pinger{
		conn: c, targets: targets, id: binary.BigEndian.Uint16(id[:]),
		pending: map[uint16]pendingProbe{}, acc: make([]probeAcc, len(targets)),
	}, nil
}

// send spreads the probes evenly over each interval until ctx ends.
func (p *pinger) send(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval / time.Duration(len(p.targets)))
	defer t.Stop()
	for i := 0; ; i = (i + 1) % len(p.targets) {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.mu.Lock()
		p.seq++
		seq := p.seq
		p.pending[seq] = pendingProbe{target: i, sent: time.Now()}
		p.mu.Unlock()
		// A failed send (no route) stays pending and counts as lost.
		_, _ = p.conn.WriteTo(echoRequest(p.id, seq), &net.IPAddr{IP: p.targets[i].AsSlice()})
	}
}

// receive collects the answers until the socket is closed.
func (p *pinger) receive() {
	buf := make([]byte, 1500)
	for {
		n, from, err := p.conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		now := time.Now()
		id, seq, ok := parseEchoReply(buf[:n])
		if !ok || id != p.id {
			continue
		}
		ipAddr, isIP := from.(*net.IPAddr)
		if !isIP {
			continue
		}
		src, _ := netip.AddrFromSlice(ipAddr.IP)
		p.mu.Lock()
		if pp, found := p.pending[seq]; found && p.targets[pp.target] == src.Unmap() {
			delete(p.pending, seq)
			rtt, a := now.Sub(pp.sent), &p.acc[pp.target]
			if a.answers == 0 || rtt < a.min {
				a.min = rtt
			}
			a.answers++
		}
		p.mu.Unlock()
	}
}

// collect hands over what came in since the last call; probes older than
// probeTimeout count as lost.
func (p *pinger) collect(now time.Time) []probeAcc {
	p.mu.Lock()
	defer p.mu.Unlock()
	for seq, pp := range p.pending {
		if now.Sub(pp.sent) > probeTimeout {
			delete(p.pending, seq)
			p.acc[pp.target].lost++
		}
	}
	out := p.acc
	p.acc = make([]probeAcc, len(p.targets))
	return out
}

func (p *pinger) close() { _ = p.conn.Close() }
