package lab

import (
	"bufio"
	"net"
	"testing"
	"time"
)

// TestTCPGate: open, the gate pipes a connection to upstream both ways;
// closed, a new connection is closed at once and reaches nothing; open again,
// connections pass once more.
func TestTCPGate(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upstream.Close() }()
	go func() {
		for {
			conn, err := upstream.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				line, _ := bufio.NewReader(conn).ReadString('\n')
				_, _ = conn.Write([]byte("echo " + line))
			}()
		}
	}()
	gate, err := startTCPGate("127.0.0.1:0", upstream.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.stop()

	roundTrip := func() (string, error) {
		conn, err := net.DialTimeout("tcp", gate.ln.Addr().String(), time.Second)
		if err != nil {
			return "", err
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write([]byte("ping\n")); err != nil {
			return "", err
		}
		return bufio.NewReader(conn).ReadString('\n')
	}
	if got, err := roundTrip(); err != nil || got != "echo ping\n" {
		t.Fatalf("open gate: %q, %v", got, err)
	}
	gate.setOpen(false)
	if got, err := roundTrip(); err == nil {
		t.Fatalf("closed gate passed %q", got)
	}
	gate.setOpen(true)
	if got, err := roundTrip(); err != nil || got != "echo ping\n" {
		t.Fatalf("reopened gate: %q, %v", got, err)
	}
}
