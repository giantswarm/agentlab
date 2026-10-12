package lab

import (
	"io"
	"os"
	"slices"
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

// TestEndTurnRevokesTheShareFirst: a shared turn's cleanup revokes the share
// while its Session still exists, closes the shared client, then deletes the
// Session.
func TestEndTurnRevokesTheShareFirst(t *testing.T) {
	f := readyFake(t)
	f.shares["share-1"] = fakeSessionID
	share := servedShare(t, f, "share-1")

	out := stdoutOf(t, func() {
		if err := endTurn(share.owner, share, fakeSessionID, true); err != nil {
			t.Fatal(err)
		}
	})

	if want := []string{"RevokeSessionShare", "close", "DeleteSession"}; !slices.Equal(f.order, want) {
		t.Errorf("cleanup order = %v, want %v", f.order, want)
	}
	if want := "share revoked: share-1\ndeleted: " + fakeSessionID + "\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

// TestShareRevokeAfterDeletionIsDone: a revoke the controller answers
// NotFound, its Session gone, is withdrawn already and says nothing.
func TestShareRevokeAfterDeletionIsDone(t *testing.T) {
	f := readyFake(t)
	f.shares["share-1"] = fakeSessionID
	share := servedShare(t, f, "share-1")
	if err := share.owner.deleteSession(t.Context(), fakeSessionID); err != nil {
		t.Fatal(err)
	}

	out := stdoutOf(t, share.end)

	if want := []string{"DeleteSession", "RevokeSessionShare", "close"}; !slices.Equal(f.order, want) {
		t.Errorf("calls = %v, want %v", f.order, want)
	}
	if out != "" {
		t.Errorf("a NotFound revoke printed %q, want nothing", out)
	}
}

// servedShare is a share of the fake's session: the owner and the shared
// client on the fake, the shared client's close recorded in its call order.
func servedShare(t *testing.T, f *fakeKagent, id string) *sessionShare {
	t.Helper()
	share := &sessionShare{owner: f.serve(t, fakeToken), client: f.serve(t, "shared-jwt"), id: id}
	share.client.closeConn = func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.order = append(f.order, "close")
		return nil
	}
	return share
}

// stdoutOf is what fn prints on stdout.
func stdoutOf(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = stdout }()
	fn()
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
