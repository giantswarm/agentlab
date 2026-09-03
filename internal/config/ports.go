package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
	"time"
)

// PortChange records one port of the configuration that was moved off its
// value because something on this machine already listens there. Field is
// the agentlab.yaml path, What names the component for the human-facing
// message, Note carries an extra caveat where a non-default port has
// consequences beyond the number itself.
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

// PortConflict records one port of an existing cluster's configuration that
// another process on this machine holds — the lab cannot renumber it (the
// kind mappings are fixed at node creation), so it is reported instead.
type PortConflict struct {
	Field string
	What  string
	Port  int
}

func (pc PortConflict) String() string {
	return fmt.Sprintf("%s: %d is held by another process (%s)", pc.Field, pc.Port, pc.What)
}

// labPort is one host-side port of the configuration: where it lives, what it
// serves, and how to pick a replacement when it is occupied.
type labPort struct {
	field, what string
	port        *int
	// pick chooses a free replacement given the scanner; a nil note means
	// the move has no caveat beyond the number.
	pick func(scan func(lo, hi int) (int, bool)) (int, bool)
	note func(to int) string
}

// ports lists every host-side port the configuration owns, in the order they
// are probed and reported.
func (c *Config) ports() []labPort {
	return []labPort{
		// Dex must stay in the NodePort range (host port == NodePort); scan
		// upward from the current value and wrap around the range.
		{"dexPort", "the Dex issuer", &c.DexPort, func(scan func(int, int) (int, bool)) (int, bool) {
			if p, ok := scan(c.DexPort+1, 32767); ok {
				return p, true
			}
			return scan(30000, c.DexPort-1)
		}, nil},
		{"platform.musterPort", "muster's direct debug access", &c.Platform.MusterPort, func(scan func(int, int) (int, bool)) (int, bool) {
			return scan(c.Platform.MusterPort+1, 65535)
		}, nil},
		{"platform.agentsPort", "the kagent UI", &c.Platform.AgentsPort, func(scan func(int, int) (int, bool)) (int, bool) {
			return scan(c.Platform.AgentsPort+1, 65535)
		}, nil},
		// The edge prefers 8443 over 444: dodging the whole privileged range
		// keeps later probes honest (they can bind instead of dial), and
		// 8443 is the conventional alternate HTTPS port.
		{"platform.gatewayPort", "the agentgateway edge", &c.Platform.GatewayPort, func(scan func(int, int) (int, bool)) (int, bool) {
			lo := c.Platform.GatewayPort + 1
			if c.Platform.GatewayPort < 1024 {
				lo = 8443
			}
			return scan(lo, 65535)
		}, func(to int) string {
			return fmt.Sprintf("public URLs gain :%d, valid from the host only; platform-test's lab-oauth-fixture step will fail (muster cannot reach its own ported URL in-cluster)", to)
		}},
		{"backstage.port", "Backstage's direct debug access", &c.Backstage.Port, func(scan func(int, int) (int, bool)) (int, bool) {
			return scan(c.Backstage.Port+1, 65535)
		}, nil},
	}
}

// ChooseFreePorts probes every host-side port of the configuration on
// 127.0.0.1 — the address all kind port mappings bind — and moves each
// occupied one to a nearby free port, returning what changed so the caller
// can tell the user. Ports in `ours` are the lab's own (published by an
// existing kind node of this very configuration) and never count as
// occupied. Meant for a configuration whose cluster does not exist (yet, or
// any more): an existing cluster's mappings are fixed at node creation, where
// renumbering would only mislead — PortConflicts is the read-only check for
// that case.
func (c *Config) ChooseFreePorts(ours map[int]bool) []PortChange {
	return c.chooseFreePorts(func(p int) bool { return !ours[p] && portTaken(p) })
}

// PortConflicts reports which host-side ports of the configuration another
// process holds, ignoring the lab's own (`ours`, the ports the existing kind
// node publishes). Nothing is changed.
func (c *Config) PortConflicts(ours map[int]bool) []PortConflict {
	return c.portConflicts(func(p int) bool { return !ours[p] && portTaken(p) })
}

func (c *Config) portConflicts(taken func(int) bool) []PortConflict {
	var conflicts []PortConflict
	for _, lp := range c.ports() {
		if taken(*lp.port) {
			conflicts = append(conflicts, PortConflict{Field: lp.field, What: lp.what, Port: *lp.port})
		}
	}
	return conflicts
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
	for _, lp := range c.ports() {
		if !taken(*lp.port) {
			continue
		}
		to, ok := lp.pick(scan)
		if !ok {
			// Nowhere to go (the range is exhausted); leave the port alone
			// and let `agentlab up` fail with the real bind error.
			continue
		}
		reserved[to] = true
		ch := PortChange{Field: lp.field, What: lp.what, From: *lp.port, To: to}
		if lp.note != nil {
			ch.Note = lp.note(to)
		}
		changes = append(changes, ch)
		*lp.port = to
	}
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
