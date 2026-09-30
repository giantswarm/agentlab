package lab

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// tcpGate is a loopback TCP forwarder the proof puts between the host
// gateway and the lab's edge, so it can take the controller out of the
// gateway's reach for a while without touching the lab: open, it pipes every
// connection to upstream unchanged (TLS passes through, the edge's hostname
// stays the target's, since the lab's domain resolves to loopback); closed,
// it refuses new connections by closing them at once. Connections already
// piped are left alone.
type tcpGate struct {
	ln       net.Listener
	upstream string
	open     atomic.Bool
	wg       sync.WaitGroup
}

// startTCPGate listens on listen and forwards to upstream, open.
func startTCPGate(listen, upstream string) (*tcpGate, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	g := &tcpGate{ln: ln, upstream: upstream}
	g.open.Store(true)
	g.wg.Add(1)
	go g.serve()
	return g, nil
}

// setOpen opens or closes the gate for the connections that follow.
func (g *tcpGate) setOpen(open bool) { g.open.Store(open) }

// stop closes the listener and waits for the accept loop; piped connections
// end with their peers.
func (g *tcpGate) stop() {
	_ = g.ln.Close()
	g.wg.Wait()
}

func (g *tcpGate) serve() {
	defer g.wg.Done()
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		if !g.open.Load() {
			_ = conn.Close()
			continue
		}
		go g.pipe(conn)
	}
}

func (g *tcpGate) pipe(down net.Conn) {
	defer func() { _ = down.Close() }()
	up, err := net.Dial("tcp", g.upstream)
	if err != nil {
		return
	}
	defer func() { _ = up.Close() }()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, down); done <- struct{}{} }()
	go func() { _, _ = io.Copy(down, up); done <- struct{}{} }()
	<-done
}
