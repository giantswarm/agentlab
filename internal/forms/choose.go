package forms

import "charm.land/huh/v2"

// Choose asks the person to pick one of options on the terminal and returns
// its index — the lab picker, when a command runs outside a lab directory
// and several labs are registered. Built through newForm, so this package's
// test hook drives it like every other form here.
func Choose(title, description string, options []string) (int, error) {
	opts := make([]huh.Option[int], len(options))
	for i, o := range options {
		opts[i] = huh.NewOption(o, i)
	}
	choice := 0
	err := newForm(huh.NewGroup(
		huh.NewSelect[int]().
			Title(title).
			Description(description).
			Options(opts...).
			Value(&choice),
	)).WithAccessible(Accessible()).Run()
	return choice, err
}
