package forms

import (
	"io"
	"strings"
	"testing"

	"charm.land/huh/v2"
)

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

// Reduced copy of the real form driven at the form's pace: input -> confirm ->
// multiselect across three groups. Both spaces must reach the multi-select,
// none the confirm.
func TestReducedFormDrivePaced(t *testing.T) {
	name := "agentlab"
	customize := false
	var comps []string
	form := newDriver("\r", "\r", " ", "\x1b[B", " ", "\r").attach(huh.NewForm(
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
	))
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
