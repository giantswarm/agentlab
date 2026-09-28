package forms

import (
	"os"

	"charm.land/huh/v2"

	"github.com/giantswarm/agentlab/internal/config"
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

// UseDefaults is the one question a first run asks — no agentlab.yaml yet, on
// a terminal: take the canonical lab the discovery just fitted to this
// machine (what `configure --defaults` writes), or customize every option in
// the form. The question names what "defaults" means, so the answer needs no
// docs. True is the defaults.
func UseDefaults(accessible bool) (bool, error) {
	value := true
	err := newForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Use the detected defaults, or customize every option?").
			Description("There is no " + config.File + " here yet. The defaults: the agent platform with\n" +
				"agents, observability and Backstage, " +
				"three users (admin, dev, viewer), the free ports above.\n" +
				"`agentlab configure` changes any of it later.").
			Affirmative("Use the defaults").
			Negative("Customize").
			Value(&value),
	)).WithAccessible(accessible || Accessible()).Run()
	return value, err
}
