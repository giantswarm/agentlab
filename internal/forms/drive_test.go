package forms

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
)

// driveTimeout bounds how long the driver waits for the form to reach the
// state its next key targets. A drive that stalls (an enter refused by a
// validator, a key meant for a field the form never focuses) fails the form
// run naming the stalled key instead of hanging the test.
const driveTimeout = 10 * time.Second

// exhaustedTimeout bounds how long the driver waits for the form to return
// once the script is exhausted. A form still open then has a field the script
// does not answer: the driver fails the form run naming it, so a new field
// breaks the drive in seconds instead of at go test's timeout.
const exhaustedTimeout = 3 * time.Second

// driver feeds a huh form a scripted keystroke stream at the form's own pace.
// huh moves between fields and groups through asynchronous bubbletea commands,
// so a key that follows an enter too closely lands on the field the enter is
// still leaving; a delay between keys only makes that race rarer. The driver
// is the form's input reader and its view hook at once: bubbletea renders on
// its event-loop goroutine after every update, so the hook sees the focused
// field exactly when it changes, and Read hands over the next key only once
// the form has moved on from the field the previous enter or tab left.
//
// A reader error kills the bubbletea program, and huh reports any kill as
// ErrTimeout, so the driver keeps its own reason: failure recovers it.
type driver struct {
	keys []string
	sent int // keys handed over in full

	mu      sync.Mutex
	focused huh.Field     // the field focused at the last render
	title   string        // the focused field's title at the last render
	state   huh.FormState // the form's state at the last render
	target  huh.Field     // the field the previous key was handed to
	advance bool          // whether the previous key moves the focus
	err     error         // why the driver ended the form run
	changed chan struct{}
}

func newDriver(keys ...string) *driver {
	return &driver{keys: keys, changed: make(chan struct{}, 1)}
}

// attach makes the driver the form's input, silences its output and hooks its
// renders. Its method value is the shape of testHook, so the real form is
// driven the same way as a form built in a test.
func (d *driver) attach(form *huh.Form) *huh.Form {
	return form.WithInput(d).WithOutput(io.Discard).WithViewHook(func(v tea.View) tea.View {
		focused := form.GetFocusedField()
		d.mu.Lock()
		d.focused, d.title, d.state = focused, fieldTitle(focused), form.State
		d.mu.Unlock()
		select {
		case d.changed <- struct{}{}:
		default:
		}
		return v
	})
}

// failure is the driver's reason for ending the form run when it ended it,
// else the run's own err.
func (d *driver) failure(err error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	return err
}

// Read hands over the next key once the form is ready for it, and io.EOF once
// the script is exhausted and the form has returned.
func (d *driver) Read(b []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.keys) == 0 {
		if !d.await(d.returned, exhaustedTimeout) {
			d.err = fmt.Errorf("keystroke script exhausted after %d keys with the form still on field %q: extend the script", d.sent, d.title)
			return 0, d.err
		}
		return 0, io.EOF
	}
	if !d.await(d.ready, driveTimeout) {
		d.err = fmt.Errorf("form still on field %q after %s, key %d (%q) not delivered", d.title, driveTimeout, d.sent+1, d.keys[0])
		return 0, d.err
	}
	key := d.keys[0]
	n := copy(b, key)
	d.target, d.advance = d.focused, false
	if n < len(key) {
		d.keys[0] = key[n:]
	} else {
		d.keys = d.keys[1:]
		d.sent++
		d.advance = movesFocus(key)
	}
	return n, nil
}

// await waits, with d.mu held, until cond holds after a render, and reports
// false if it does not within bound.
func (d *driver) await(cond func() bool, bound time.Duration) bool {
	deadline := time.After(bound)
	for !cond() {
		d.mu.Unlock()
		select {
		case <-d.changed:
		case <-deadline:
			d.mu.Lock()
			return false
		}
		d.mu.Lock()
	}
	return true
}

// returned reports whether the form has left its normal state: submitted or
// aborted.
func (d *driver) returned() bool {
	return d.state != huh.StateNormal
}

// fieldTitle is the first line a field renders without the focus border, its
// title: huh fields have no title getter.
func fieldTitle(f huh.Field) string {
	if f == nil {
		return ""
	}
	line, _, _ := strings.Cut(ansi.Strip(f.View()), "\n")
	return strings.TrimSpace(strings.TrimLeft(line, "┃│ "))
}

// ready reports whether the form is where the next key belongs: a field is
// focused and, when the previous key moves the focus, it has moved.
func (d *driver) ready() bool {
	return d.focused != nil && (!d.advance || d.focused != d.target)
}

// movesFocus reports whether huh leaves the focused field on a key: enter and
// tab go forward, shift+tab back. The drives answer confirms with enter, so
// the y/n shortcuts, which advance as well, are not listed.
func movesFocus(key string) bool {
	switch key {
	case "\r", "\t", "\x1b[Z":
		return true
	}
	return false
}
