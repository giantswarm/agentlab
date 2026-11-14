package shaper

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The shaper's state directory: the shaper publishes what it does there
// (StateFile), and `agentlab shaper auto|max|pause|off` leaves the person's
// mode there (ModeFile). On the host it is agentlab's state folder's
// `shaper/`, mounted into the shaper's container.
const (
	StateFile = "state.json"
	ModeFile  = "mode"
)

// StaleAfter is how old the published state may be before a reader calls
// the shaper gone: it publishes every second.
const StaleAfter = 10 * time.Second

// State is what the shaper publishes.
type State struct {
	// Mode is the person's mode (auto, auto <max>, pause, off).
	Mode string `json:"mode"`
	// Device is the mirror's host-side veth the shaper works on; "" while it
	// finds none.
	Device string `json:"device,omitempty"`
	// Shaping is whether CAKE is in place on Device.
	Shaping bool `json:"shaping"`
	// CapBps is the mirror's cap in bits per second, 0 unlimited.
	CapBps float64 `json:"capBps"`
	// MirrorBps is what the mirror downloaded over the last second.
	MirrorBps float64 `json:"mirrorBps"`
	// DelayMs is the link's queue as the latency probes see it, over the
	// lowest baseline; DelayKnown false while no probe answered.
	DelayMs    float64 `json:"delayMs"`
	DelayKnown bool    `json:"delayKnown"`
	// TargetMs is the queue at which the mirror backs off.
	TargetMs float64 `json:"targetMs"`
	// Reason is why the cap is where it is.
	Reason string `json:"reason"`
	// Network is the host's network (the default route), whose change
	// starts the baselines over.
	Network    string           `json:"network"`
	Reflectors []ReflectorState `json:"reflectors"`
	// Problem is what keeps the shaper from shaping, "" when nothing does.
	Problem string    `json:"problem,omitempty"`
	Updated time.Time `json:"updated"`
}

// ReflectorState is one latency reflector.
type ReflectorState struct {
	Addr   string  `json:"addr"`
	BaseMs float64 `json:"baseMs"` // 0 = no baseline on this network yet
	LastMs float64 `json:"lastMs"` // 0 = no answer yet
	Lost   int     `json:"lost"`   // probes lost in a row
}

// ReadState reads the state the shaper published in dir.
func ReadState(dir string) (State, error) {
	var st State
	b, err := os.ReadFile(filepath.Join(dir, StateFile)) // #nosec G304 -- agentlab's own state folder
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// writeState publishes the state atomically: readers never see half a file.
func writeState(dir string, st State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	file := filepath.Join(dir, StateFile)
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil { // #nosec G306 -- the shaper's status, read by the person's agentlab
		return err
	}
	return os.Rename(tmp, file)
}

// WriteMode leaves the person's mode for the shaper.
func WriteMode(dir string, m Mode) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- mounted into the shaper's container, which writes its state there
		return err
	}
	return os.WriteFile(filepath.Join(dir, ModeFile), []byte(m.String()+"\n"), 0o644) // #nosec G306 -- read by the shaper as root
}

// Summary is the state in one line, for `agentlab status`.
func (st State) Summary() string {
	if !st.Shaping {
		if st.Problem != "" {
			return "not shaping: " + st.Problem
		}
		return "not shaping yet: " + st.Reason
	}
	limit := "the mirror capped at " + FormatRate(st.CapBps)
	if st.CapBps <= 0 {
		limit = "the mirror not limited"
	}
	line := fmt.Sprintf("%s (%s), getting %s", limit, st.Mode, formatThroughput(st.MirrorBps))
	if st.DelayKnown {
		line += fmt.Sprintf(", the link +%.0f ms over its baseline", st.DelayMs)
	}
	return line + ": " + st.Reason
}

// Describe is the state for people, `agentlab shaper status`.
func (st State) Describe() string {
	var b strings.Builder
	limit := "the mirror capped at " + FormatRate(st.CapBps)
	if st.CapBps <= 0 {
		limit = "the mirror not limited"
	}
	device := st.Device
	if device == "" {
		device = "no veth found yet"
	}
	shaping := "CAKE on " + device
	if !st.Shaping {
		shaping = "not shaping (" + device + ")"
	}
	fmt.Fprintf(&b, "shaper    %s · %s · getting %s · %s\n", st.Mode, limit, formatThroughput(st.MirrorBps), shaping)
	if st.DelayKnown {
		fmt.Fprintf(&b, "latency   +%.0f ms over baseline (backs off above +%.0f ms) · %s\n", st.DelayMs, st.TargetMs, st.Reason)
	} else {
		fmt.Fprintf(&b, "latency   unknown · %s\n", st.Reason)
	}
	fmt.Fprintf(&b, "network   %s\n", st.Network)
	var refl []string
	for _, r := range st.Reflectors {
		switch {
		case r.LastMs == 0:
			refl = append(refl, r.Addr+" no answer")
		case r.Lost > 0:
			refl = append(refl, fmt.Sprintf("%s lost %d", r.Addr, r.Lost))
		default:
			refl = append(refl, fmt.Sprintf("%s %.0f/%.0f ms", r.Addr, r.LastMs, r.BaseMs))
		}
	}
	if len(refl) > 0 {
		fmt.Fprintf(&b, "probes    %s (last/baseline)\n", strings.Join(refl, " · "))
	}
	if st.Problem != "" {
		fmt.Fprintf(&b, "problem   %s\n", st.Problem)
	}
	return b.String()
}
