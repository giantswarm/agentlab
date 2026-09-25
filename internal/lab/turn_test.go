package lab

import (
	"strings"
	"testing"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

func TestRosterLine(t *testing.T) {
	for name, tc := range map[string]struct {
		in   templateListing
		want string
	}{
		"ready": {
			in:   templateListing{Namespace: kagentNamespace, Name: "sre", Harness: platformHarness, DisplayName: "SRE"},
			want: `  kagent/sre  harness=kagent  "SRE"`,
		},
		"unavailable": {
			in:   templateListing{Namespace: kagentNamespace, Name: "narrow", Harness: platformHarness, Unavailable: "Harness kagent has not compiled a ready revision: booting"},
			want: "  kagent/narrow  harness=kagent  unavailable: Harness kagent has not compiled a ready revision: booting",
		},
		"no harness": {
			in:   templateListing{Namespace: kagentNamespace, Name: "orphan", Unavailable: "no Harness admits this AgentTemplate"},
			want: "  kagent/orphan  unavailable: no Harness admits this AgentTemplate",
		},
	} {
		if got := rosterEntry(tc.in); got != tc.want {
			t.Errorf("%s: rosterEntry = %q, want %q", name, got, tc.want)
		}
	}
}

// TestAdmittingHarness: a turn is created on the admitting Harness that
// reports the template Ready, whichever runtime that Harness is; a template
// nobody runs, or one outside the roster, is refused with the reason.
func TestAdmittingHarness(t *testing.T) {
	templates := []*apiv1alpha1.AgentTemplate{
		fakeTemplate(t, a2aTestAgent, nil, []string{kagentHarness}, []any{fakeHarnessStatus(kagentHarness, true, "")}),
		fakeTemplate(t, "coding", nil, []string{testOtherHarness}, []any{fakeHarnessStatus(testOtherHarness, true, "")}),
		fakeTemplate(t, "snapshotting", nil, []string{testOtherHarness}, []any{fakeHarnessStatus(testOtherHarness, false, "waiting for the golden snapshot")}),
		fakeTemplate(t, "unadmitted", nil, nil, nil),
	}
	for name, want := range map[string]string{a2aTestAgent: kagentHarness, "coding": testOtherHarness} {
		if got, err := admittingHarness(templates, name); err != nil || got != want {
			t.Errorf("admittingHarness(%s) = %q, %v; want %q", name, got, err, want)
		}
	}
	for name, reason := range map[string]string{
		"snapshotting": "waiting for the golden snapshot",
		"unadmitted":   unadmittedReason,
		"missing":      "not in this user's roster",
	} {
		if _, err := admittingHarness(templates, name); err == nil || !strings.Contains(err.Error(), reason) {
			t.Errorf("admittingHarness(%s) error = %v, want %q", name, err, reason)
		}
	}
}
