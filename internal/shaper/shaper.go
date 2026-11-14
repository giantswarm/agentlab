// Package shaper is agentlab's bandwidth shaper (docs/bandwidth.md): it
// lets the registry mirror's downloads take what the person's uplink leaves
// idle and give it back within seconds. One CAKE qdisc on the host side of
// the mirror's veth caps everything the mirror downloads from gsoci, and a
// controller sets its bandwidth every tick from the uplink's latency: while
// the latency to a few public reflectors stays at its baseline the mirror
// may take more, when it rises (the link's queue is filling, whoever fills
// it) the mirror yields.
package shaper

import (
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// Config is the shaper's configuration, from `agentlab shaper run`'s flags.
type Config struct {
	// MirrorPID is a process in the mirror's network namespace: PID 1 of
	// the mirror's PID namespace, which the shaper's container joins.
	MirrorPID int
	// Reflectors are the IPv4 hosts the latency is measured against.
	Reflectors []netip.Addr
	// StateDir is where the shaper publishes its state and reads the
	// person's mode; "" publishes nothing.
	StateDir string
	// Interval is the control tick and the probe interval; Target the
	// queue above the baseline at which the mirror backs off.
	Interval, Target time.Duration
	// Min and Max bound auto's cap, Start is where it starts on a network,
	// Pause what `agentlab shaper pause` holds the mirror at; bits per
	// second.
	Min, Max, Start, Pause float64
}

// DefaultReflectors are four public resolvers on separate networks.
var DefaultReflectors = []netip.Addr{
	netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("9.9.9.9"),
	netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("208.67.222.222"),
}

// DefaultConfig is the shaper as agentlab runs it.
func DefaultConfig() Config {
	return Config{
		MirrorPID:  1,
		Reflectors: DefaultReflectors,
		Interval:   500 * time.Millisecond,
		Target:     30 * time.Millisecond,
		Min:        256e3,
		Max:        2.5e9,
		Start:      8e6,
		Pause:      128e3,
	}
}

// Check refuses a configuration the controller cannot run.
func (c Config) Check() error {
	switch {
	case c.MirrorPID <= 0:
		return errors.New("the mirror's process id must be positive")
	case len(c.Reflectors) == 0:
		return errors.New("no reflectors")
	case c.Min <= 0 || c.Min > c.Max:
		return fmt.Errorf("the floor %s is not between 0 and the ceiling %s", FormatRate(c.Min), FormatRate(c.Max))
	case c.Start < c.Min || c.Start > c.Max:
		return fmt.Errorf("the start %s is outside %s..%s", FormatRate(c.Start), FormatRate(c.Min), FormatRate(c.Max))
	case c.Target < 5*time.Millisecond:
		return errors.New("a target below 5ms is noise")
	case c.Interval < 100*time.Millisecond:
		return errors.New("an interval below 100ms")
	}
	for _, r := range c.Reflectors {
		if !r.Is4() {
			return fmt.Errorf("reflector %s is not an IPv4 address", r)
		}
	}
	return nil
}
