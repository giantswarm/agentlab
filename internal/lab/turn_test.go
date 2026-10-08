package lab

import (
	"strings"
	"testing"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// TestRosterLine: a roster entry names the Agent, its template and Harness,
// the display name, and why it cannot start a conversation when it cannot.
func TestRosterLine(t *testing.T) {
	for _, tc := range []struct {
		in   agentListing
		want string
	}{
		{
			in:   agentListing{Namespace: kagentNamespace, Name: "sre", Template: "sre", Harness: platformHarness, DisplayName: "SRE"},
			want: `  kagent/sre  template=sre  harness=kagent  "SRE"`,
		},
		{
			in:   agentListing{Namespace: kagentNamespace, Name: fakeNarrowAgent, Template: fakeNarrowAgent, Harness: platformHarness, Unavailable: "Agent narrow has not compiled a ready revision: booting"},
			want: "  kagent/narrow  template=narrow  harness=kagent  unavailable: Agent narrow has not compiled a ready revision: booting",
		},
		{
			in:   agentListing{Namespace: kagentNamespace, Name: "inline"},
			want: "  kagent/inline",
		},
	} {
		if got := rosterEntry(tc.in); got != tc.want {
			t.Errorf("rosterEntry(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSelectAgent: a turn is created of the Agent pairing the template with
// the named Harness, else of the template's Agent that reports Ready; a
// template without an Agent, an Agent that is not Ready, and a Harness no
// Agent pairs the template with are refused with the reason.
func TestSelectAgent(t *testing.T) {
	templates := []*apiv1alpha1.AgentTemplate{fakeTemplate(t, a2aTestAgent, nil), fakeTemplate(t, fakeCodingAgent, nil)}
	agents := []*apiv1alpha1.Agent{
		fakeAgent(t, a2aTestAgent, a2aTestAgent, kagentHarness, nil, fakeReady(true, "")),
		fakeAgent(t, fakeCodingAgent, fakeCodingAgent, testOtherHarness, nil, fakeReady(true, "")),
		fakeAgent(t, "coding-snapshotting", fakeCodingAgent, "snapshotting", nil, fakeReady(false, "waiting for the golden snapshot")),
		fakeAgent(t, fakeNarrowAgent, fakeNarrowAgent, kagentHarness, nil, fakeReady(false, "booting")),
	}
	for name, want := range map[string]string{a2aTestAgent: a2aTestAgent, fakeCodingAgent: fakeCodingAgent} {
		if got, err := selectAgent(agents, templates, name, ""); err != nil || got.Name != want {
			t.Errorf("selectAgent(%s) = %q, %v; want %q", name, got.Name, err, want)
		}
	}
	if _, err := selectAgent(agents, templates, fakeCodingAgent, "snapshotting"); err == nil || !strings.Contains(err.Error(), "waiting for the golden snapshot") {
		t.Errorf("an Agent pinned by Harness that is not Ready: %v", err)
	}
	if got, err := selectAgent(agents, templates, fakeCodingAgent, testOtherHarness); err != nil || got.Name != fakeCodingAgent {
		t.Errorf("the Agent of the named Harness: %q, %v", got.Name, err)
	}
	for name, reason := range map[string]string{
		fakeNarrowAgent: "booting",
		"nobody":        "not in this user's roster",
	} {
		if _, err := selectAgent(agents, templates, name, ""); err == nil || !strings.Contains(err.Error(), reason) {
			t.Errorf("selectAgent(%s) error = %v, want %q", name, err, reason)
		}
	}
	if _, err := selectAgent(agents, templates, a2aTestAgent, "nowhere"); err == nil || !strings.Contains(err.Error(), "no Agent pairs AgentTemplate "+a2aTestAgent+" with Harness nowhere") {
		t.Errorf("a Harness no Agent pairs the template with: %v", err)
	}
}
