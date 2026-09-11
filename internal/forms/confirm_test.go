package forms

import "testing"

// TestConfirmDrive drives the real confirm (the question `agentlab open` and
// the end of `up` ask) with a scripted keystroke stream: enter keeps the
// default, and one right arrow moves to the negative first.
func TestConfirmDrive(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
		dflt bool
		want bool
	}{
		{name: "enter keeps the default", keys: []string{"\r"}, dflt: true, want: true},
		{name: "enter keeps a negative default", keys: []string{"\r"}, dflt: false, want: false},
		{name: "arrow moves off the default", keys: []string{"\x1b[C", "\r"}, dflt: true, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testHook = newDriver(tc.keys...).attach
			defer func() { testHook = nil }()

			got, err := Confirm("Trust the lab CA now?", "one sudo prompt", "Trust it", "Not now", tc.dflt)
			if err != nil {
				t.Fatalf("Confirm: %v", err)
			}
			if got != tc.want {
				t.Errorf("Confirm() = %v, want %v", got, tc.want)
			}
		})
	}
}
