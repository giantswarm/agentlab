package forms

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/huh"
)

// pacedReader delivers one keystroke chunk per Read with a small delay,
// mimicking human typing: huh group transitions run asynchronous commands and
// can drop keys that arrive in the same read burst.
type pacedReader struct {
	chunks []string
	delay  time.Duration
}

func newPacedReader(delay time.Duration, chunks ...string) *pacedReader {
	return &pacedReader{chunks: chunks, delay: delay}
}

func (p *pacedReader) Read(b []byte) (int, error) {
	if len(p.chunks) == 0 {
		return 0, io.EOF
	}
	time.Sleep(p.delay)
	n := copy(b, p.chunks[0])
	if n < len(p.chunks[0]) {
		p.chunks[0] = p.chunks[0][n:]
	} else {
		p.chunks = p.chunks[1:]
	}
	return n, nil
}

// Minimal probe: can a huh TUI form be driven by a plain reader at all?
func TestMinimalFormDrive(t *testing.T) {
	val := "start"
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("t").Value(&val),
	)).WithInput(strings.NewReader("\r")).WithOutput(io.Discard)
	if err := form.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if val != "start" {
		t.Fatalf("val = %q", val)
	}
}

// Reduced copy of the real form with paced input: input -> confirm ->
// multiselect across three groups.
func TestReducedFormDrivePaced(t *testing.T) {
	name := "dexlab"
	customize := false
	var comps []string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("Cluster name").Value(&name),
		).Title("Cluster"),
		huh.NewGroup(
			huh.NewConfirm().Title("Customize?").Affirmative("Edit them").Negative("Keep as is").Value(&customize),
		).Title("Users"),
		huh.NewGroup(
			huh.NewMultiSelect[string]().Title("Components").
				Options(huh.NewOption("platform", "platform"), huh.NewOption("backstage", "backstage")).
				Value(&comps),
		).Title("Components"),
	).WithInput(newPacedReader(30*time.Millisecond,
		"\r", "\r", " ", "\x1b[B", " ", "\r")).WithOutput(io.Discard)
	if err := form.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if customize {
		t.Errorf("customize flipped to true")
	}
	if len(comps) != 2 {
		t.Errorf("comps = %v, want both", comps)
	}
}
