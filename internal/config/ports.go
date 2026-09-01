package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
	"time"
)

// PortChange records one port of a freshly created configuration that was
// moved off its default because something on this machine already listens
// there. Field is the agentlab.yaml path, What names the component for the
// human-facing message, Note carries an extra caveat where a non-default
// port has consequences beyond the number itself.
type PortChange struct {
	Field    string
	What     string
	From, To int
	Note     string
}

func (ch PortChange) String() string {
	s := fmt.Sprintf("%s: %d is in use, using %d instead (%s", ch.Field, ch.From, ch.To, ch.What)
	if ch.Note != "" {
		s += "; " + ch.Note
	}
	return s + ")"
}

// ChooseFreePorts probes every host-side port of a freshly created
// configuration on 127.0.0.1 — the address all kind port mappings bind — and
// moves each occupied one to a nearby free port, returning what changed so
// the caller can tell the user. Meant for new configs only: an existing
// agentlab.yaml describes a cluster whose mappings are already fixed, where
// silently renumbering would only mislead.
func (c *Config) ChooseFreePorts() []PortChange {
	return c.chooseFreePorts(portTaken)
}

// chooseFreePorts is ChooseFreePorts with the probe injected, so tests can
// run against a fixed set of "occupied" ports instead of the machine.
func (c *Config) chooseFreePorts(taken func(int) bool) []PortChange {
	// Every port with a fixed meaning in the lab, so replacements never
	// collide with each other or with it: the config's own host ports, the
	// browser-login callback (host-side, pre-registered in Dex), and the
	// pinned NodePorts (they share the node's port space with DexPort's
	// NodePort, and keeping host ports off them avoids same-number
	// coincidences in debugging).
	reserved := map[int]bool{
		c.DexPort:              true,
		c.Backstage.Port:       true,
		c.Platform.MusterPort:  true,
		c.Platform.AgentsPort:  true,
		c.Platform.GatewayPort: true,
		BrowserCallbackPort:    true,
		MusterNodePort:         true,
		KagentUINodePort:       true,
		GatewayNodePort:        true,
	}

	scan := func(lo, hi int) (int, bool) {
		for p := lo; p <= hi; p++ {
			if !reserved[p] && !taken(p) {
				return p, true
			}
		}
		return 0, false
	}

	var changes []PortChange
	move := func(field, what string, port *int, pick func() (int, bool), note func(to int) string) {
		if !taken(*port) {
			return
		}
		to, ok := pick()
		if !ok {
			// Nowhere to go (the range is exhausted); leave the port alone
			// and let `agentlab up` fail with the real bind error.
			return
		}
		reserved[to] = true
		ch := PortChange{Field: field, What: what, From: *port, To: to}
		if note != nil {
			ch.Note = note(to)
		}
		changes = append(changes, ch)
		*port = to
	}
	noNote := func(int) string { return "" }

	// Dex must stay in the NodePort range (host port == NodePort); scan
	// upward from the default and wrap around the range.
	move("dexPort", "the Dex issuer", &c.DexPort, func() (int, bool) {
		if p, ok := scan(c.DexPort+1, 32767); ok {
			return p, true
		}
		return scan(30000, c.DexPort-1)
	}, noNote)

	move("platform.musterPort", "muster's direct debug access", &c.Platform.MusterPort, func() (int, bool) {
		return scan(c.Platform.MusterPort+1, 65535)
	}, noNote)

	move("platform.agentsPort", "the kagent UI", &c.Platform.AgentsPort, func() (int, bool) {
		return scan(c.Platform.AgentsPort+1, 65535)
	}, noNote)

	// The edge prefers 8443 over 444: dodging the whole privileged range
	// keeps later probes honest (they can bind instead of dial), and 8443 is
	// the conventional alternate HTTPS port.
	move("platform.gatewayPort", "the agentgateway edge", &c.Platform.GatewayPort, func() (int, bool) {
		lo := c.Platform.GatewayPort + 1
		if c.Platform.GatewayPort < 1024 {
			lo = 8443
		}
		return scan(lo, 65535)
	}, func(to int) string {
		return fmt.Sprintf("public URLs gain :%d; the chart's Backstage app-config assumes a port-free baseUrl, expect rough edges", to)
	})

	move("backstage.port", "Backstage's direct debug access", &c.Backstage.Port, func() (int, bool) {
		return scan(c.Backstage.Port+1, 65535)
	}, noNote)

	return changes
}

// portTaken reports whether anything on this machine already occupies
// 127.0.0.1:<port>, the exact address every kind port mapping binds. A bind
// probe catches every listener (including 0.0.0.0 binds, which conflict with
// a later 127.0.0.1 bind); privileged ports (the gateway's 443) may refuse
// the bind with EACCES even when free — docker binds those as root — so they
// fall back to dialing, where only an actual listener answers.
func portTaken(port int) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	l, err := net.Listen("tcp", addr)
	if err == nil {
		_ = l.Close()
		return false
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
