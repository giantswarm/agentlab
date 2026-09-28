package forms

import "testing"

// TestChooseDrive drives the real select (the lab picker) with a scripted
// keystroke stream: enter takes the first option, one down arrow the second.
func TestChooseDrive(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
		want int
	}{
		{name: "enter takes the first", keys: []string{"\r"}, want: 0},
		{name: "down then enter takes the second", keys: []string{"\x1b[B", "\r"}, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testHook = newDriver(tc.keys...).attach
			defer func() { testHook = nil }()

			got, err := Choose("Which lab?", "two are registered", []string{"a  /labs/a", "b  /labs/b"})
			if err != nil {
				t.Fatalf("Choose: %v", err)
			}
			if got != tc.want {
				t.Errorf("Choose() = %d, want %d", got, tc.want)
			}
		})
	}
}
