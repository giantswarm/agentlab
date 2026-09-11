package forms

import (
	"os"

	"charm.land/huh/v2"
)

// ErrAborted is huh's user abort (Ctrl-C, Esc), re-exported so callers need
// no huh import of their own.
var ErrAborted = huh.ErrUserAborted

// Confirm asks one yes/no question on the terminal — the lab's questions
// outside `configure`: the trust offer before a browser opens, the two
// questions at the end of `up`. Built through newForm, so this package's test
// hook drives it like every other form here.
func Confirm(title, description, affirmative, negative string, value bool) (bool, error) {
	err := newForm(huh.NewGroup(
		huh.NewConfirm().
			Title(title).
			Description(description).
			Affirmative(affirmative).
			Negative(negative).
			Value(&value),
	)).WithAccessible(Accessible()).Run()
	return value, err
}

// Accessible reports huh's prompt-per-question mode (also what screen readers
// want): the ACCESSIBLE environment variable. `configure`'s --accessible flag
// ORs with it.
func Accessible() bool { return os.Getenv("ACCESSIBLE") != "" }
