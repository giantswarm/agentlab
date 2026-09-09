package forms

import (
	"fmt"
	"io"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

// driveTimeout bounds how long the driver waits for the form to reach the
// state its next key targets. A drive that stalls (an enter refused by a
// validator, a key meant for a field the form never focuses) fails the form
// run naming the stalled key instead of hanging the test.
const driveTimeout = 10 * time.Second

// driver feeds a huh form a scripted keystroke stream at the form's own pace.
// huh moves between fields and groups through asynchronous bubbletea commands,
// so a key that follows an enter too closely lands on the field the enter is
// still leaving; a delay between keys only makes that race rarer. The driver
// is the form's input reader and its view hook at once: bubbletea renders on
// its event-loop goroutine after every update, so the hook sees the focused
// field exactly when it changes, and Read hands over the next key only once
// the form has moved on from the field the previous enter or tab left.
type driver struct {
	keys []string

	mu      sync.Mutex
	focused huh.Field // the field focused at the last render
	target  huh.Field // the field the previous key was handed to
	advance bool      // whether the previous key moves the focus
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
		d.mu.Lock()
		d.focused = form.GetFocusedField()
		d.mu.Unlock()
		select {
		case d.changed <- struct{}{}:
		default:
		}
		return v
	})
}

// Read hands over the next key once the form is ready for it, and io.EOF once
// the script is exhausted.
func (d *driver) Read(b []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.keys) == 0 {
		return 0, io.EOF
	}
	deadline := time.After(driveTimeout)
	for !d.ready() {
		d.mu.Unlock()
		select {
		case <-d.changed:
		case <-deadline:
			d.mu.Lock()
			return 0, fmt.Errorf("form still on %T after %s, key %q not delivered", d.target, driveTimeout, d.keys[0])
		}
		d.mu.Lock()
	}
	key := d.keys[0]
	n := copy(b, key)
	d.target, d.advance = d.focused, false
	if n < len(key) {
		d.keys[0] = key[n:]
	} else {
		d.keys = d.keys[1:]
		d.advance = movesFocus(key)
	}
	return n, nil
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
