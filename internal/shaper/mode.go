package shaper

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// The modes a person sets for the machine (`agentlab shaper auto|max|pause|
// off`): one shaper serves every lab, so the override lives with it, in the
// mode file of its state directory, not in a lab's agentlab.yaml.
const (
	ModeAuto  = "auto"
	ModePause = "pause"
	ModeOff   = "off"
)

// Mode is what the person asked for.
type Mode struct {
	// Kind is ModeAuto, ModePause or ModeOff.
	Kind string
	// Max is auto's cap below the shaper's own ceiling; 0 is none.
	Max float64
}

// String is the mode as the mode file holds it: "auto", "auto 5mbit",
// "pause", "off".
func (m Mode) String() string {
	if m.Kind == ModeAuto && m.Max > 0 {
		return ModeAuto + " " + RateArg(m.Max)
	}
	return m.Kind
}

// ParseMode reads a mode: auto [<max>], pause or off; empty is auto.
func ParseMode(s string) (Mode, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return Mode{Kind: ModeAuto}, nil
	}
	switch f[0] {
	case ModeAuto:
		if len(f) == 1 {
			return Mode{Kind: ModeAuto}, nil
		}
		if len(f) > 2 {
			return Mode{}, fmt.Errorf("auto takes one cap, got %q", s)
		}
		v, err := ParseRate(f[1])
		if err != nil {
			return Mode{}, err
		}
		return Mode{Kind: ModeAuto, Max: v}, nil
	case ModePause, ModeOff:
		if len(f) > 1 {
			return Mode{}, fmt.Errorf("%s takes no argument, got %q", f[0], s)
		}
		return Mode{Kind: f[0]}, nil
	default:
		return Mode{}, fmt.Errorf("unknown mode %q (want auto [<max>], pause or off)", s)
	}
}

// readModeFile reads the mode in path; no file is auto. The file is the
// person's and the shaper runs as root: it is opened without following links
// and without blocking (a FIFO there would hang the tick loop, and with it the
// shaper's stop), only a regular file counts, only its first bytes are read,
// and a bad one is never echoed into the error.
func readModeFile(path string) (Mode, error) {
	auto := Mode{Kind: ModeAuto}
	f, err := os.OpenFile(path, os.O_RDONLY|modeFileFlags, 0) // #nosec G304 -- the shaper's own mode file
	if errors.Is(err, os.ErrNotExist) {
		return auto, nil
	}
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return auto, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return auto, err
	}
	if !fi.Mode().IsRegular() {
		return auto, fmt.Errorf("not a regular file (%s)", fi.Mode().Type())
	}
	b, _ := io.ReadAll(io.LimitReader(f, 64))
	line, _, _ := strings.Cut(string(b), "\n")
	m, err := ParseMode(line)
	if err != nil {
		return auto, errors.New("holds no mode")
	}
	return m, nil
}
