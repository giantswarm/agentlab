package lab

import "testing"

func TestRosterLine(t *testing.T) {
	for name, tc := range map[string]struct {
		in   templateListing
		want string
	}{
		"ready": {
			in:   templateListing{Namespace: "kagent", Name: "sre", Harness: "kagent", DisplayName: "SRE"},
			want: `  kagent/sre  harness=kagent  "SRE"`,
		},
		"unavailable": {
			in:   templateListing{Namespace: "kagent", Name: "narrow", Harness: "kagent", Unavailable: "Harness kagent has not compiled a ready revision: booting"},
			want: "  kagent/narrow  harness=kagent  unavailable: Harness kagent has not compiled a ready revision: booting",
		},
		"no harness": {
			in:   templateListing{Namespace: "kagent", Name: "orphan", Unavailable: "no Harness admits this AgentTemplate"},
			want: "  kagent/orphan  unavailable: no Harness admits this AgentTemplate",
		},
	} {
		if got := rosterEntry(tc.in); got != tc.want {
			t.Errorf("%s: rosterEntry = %q, want %q", name, got, tc.want)
		}
	}
}
