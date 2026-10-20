package lab

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	workspaceManagerGroupVersion, _ = schema.ParseGroupVersion("workspace-manager.giantswarm.io/v1alpha1")
	gvkWorkspace                    = workspaceManagerGroupVersion.WithKind("Workspace")
	gvrWorkspaces                   = workspaceManagerGroupVersion.WithResource("workspaces")
)

// workspaceObject is a Workspace in kagent's namespace, created at created,
// with the labels.
func workspaceObject(name string, created time.Time, labels map[string]string) *unstructured.Unstructured {
	u := customObject(gvkWorkspace, kagentNamespace, name, labels)
	u.SetCreationTimestamp(metav1.NewTime(created))
	return u
}

// e2eName is the portal suite's name for a fixture Workspace of a run that
// started at start.
func e2eName(start time.Time, kind string) string {
	return e2eWorkspacePrefix + strconv.FormatInt(start.UnixMilli(), 36) + "-1-" + kind
}

// leftoverSeeds is a used lab: what earlier runs left, what this run and a
// portal run going on made, and a person's own Workspace.
func leftoverSeeds(now time.Time) (seeds []*unstructured.Unstructured, leftovers []string) {
	old, recent := now.Add(-4*time.Hour), now.Add(-20*time.Minute)
	seeds = []*unstructured.Unstructured{
		workspaceObject("agentlab-gateway-ws", old, map[string]string{fixtureLabel: workspaceFixtureValue, workspaceRunLabel: "earlier"}),
		workspaceObject("agentlab-sync-filter", recent, map[string]string{fixtureLabel: workspaceFixtureValue}),
		workspaceObject("agentlab-sync-list", recent, map[string]string{fixtureLabel: workspaceFixtureValue, workspaceRunLabel: labRunID}),
		workspaceObject(e2eName(old, "public"), old, nil),
		workspaceObject(e2eName(old, "refusal"), old, nil),
		workspaceObject(e2eName(recent, "public"), recent, nil),
		workspaceObject("e2e-ws-local-1-public", old, nil),
		workspaceObject("e2e-ws-local-2-public", recent, nil),
		workspaceObject("team-docs", old, nil),
	}
	leftovers = []string{"agentlab-gateway-ws", "agentlab-sync-filter", e2eName(old, "public"), e2eName(old, "refusal"), "e2e-ws-local-1-public"}
	slices.Sort(leftovers)
	return seeds, leftovers
}

// TestWorkspaceLeftovers: the lab's fixtures of another run (or of none
// named) and the portal suite's of a run older than its leftover age are
// leftovers — a run id that is no time falls back to the creation; this
// run's fixtures, a portal run going on and a person's own Workspace stay.
func TestWorkspaceLeftovers(t *testing.T) {
	now := time.Now()
	seeds, want := leftoverSeeds(now)
	items := make([]unstructured.Unstructured, 0, len(seeds))
	for _, s := range seeds {
		items = append(items, *s)
	}
	var got []string
	for _, l := range workspaceLeftovers(items, labRunID, now) {
		if l.why == "" {
			t.Errorf("%s: no reason given", l.name)
		}
		got = append(got, l.name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("leftovers = %v, want %v", got, want)
	}
}

// TestSweepWorkspaceLeftovers: on a used lab the sweep removes exactly the
// leftovers and a second sweep has nothing to do; a lab without the
// workspace-manager's kind has nothing to sweep.
func TestSweepWorkspaceLeftovers(t *testing.T) {
	now := time.Now()
	seeds, leftovers := leftoverSeeds(now)
	objs := make([]runtime.Object, 0, len(seeds))
	var all []string
	for _, s := range seeds {
		objs = append(objs, s)
		all = append(all, s.GetName())
	}
	newFakeLab(t, objs...)
	ctx := context.Background()
	for round := 1; round <= 2; round++ {
		if err := sweepWorkspaceLeftovers(ctx); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		items, err := listObjects(ctx, gvrWorkspaces, kagentNamespace, "")
		if err != nil {
			t.Fatal(err)
		}
		var left []string
		for _, i := range items {
			left = append(left, i.GetName())
		}
		slices.Sort(left)
		want := slices.DeleteFunc(slices.Clone(all), func(n string) bool { return slices.Contains(leftovers, n) })
		slices.Sort(want)
		if !slices.Equal(left, want) {
			t.Errorf("round %d: left %v, want %v", round, left, want)
		}
	}
}

// TestLabelLabWorkspace: a Workspace a proof created carries the lab's
// fixture label and this run's id, so a later run's sweep finds it.
func TestLabelLabWorkspace(t *testing.T) {
	newFakeLab(t, workspaceObject("agentlab-signin-proof", time.Now(), nil))
	ctx := context.Background()
	if err := labelLabWorkspace(ctx, "agentlab-signin-proof"); err != nil {
		t.Fatal(err)
	}
	obj, err := getObject(ctx, gvrWorkspaces, kagentNamespace, "agentlab-signin-proof")
	if err != nil {
		t.Fatal(err)
	}
	if l := obj.GetLabels(); l[fixtureLabel] != workspaceFixtureValue || l[workspaceRunLabel] != labRunID {
		t.Errorf("labels = %v, want %s=%s and %s=%s", l, fixtureLabel, workspaceFixtureValue, workspaceRunLabel, labRunID)
	}
	if left := workspaceLeftovers([]unstructured.Unstructured{*obj}, "a-later-run", time.Now()); len(left) != 1 {
		t.Errorf("a later run sees %v, want the labelled Workspace as a leftover", left)
	}
}

// TestWorkspaceGrantLedger: every grant a run used is dropped once at its
// end, a person recorded twice once and through their newest session, in
// the order first recorded; the ledger is empty afterwards.
func TestWorkspaceGrantLedger(t *testing.T) {
	var l workspaceGrantLedger
	var dropped []string
	l.record(testPortalUser, func() { dropped = append(dropped, "first session") })
	l.record(accessDefaultSecond, func() { dropped = append(dropped, accessDefaultSecond) })
	l.record(testPortalUser, func() { dropped = append(dropped, testPortalUser) })
	if got := l.release(); !slices.Equal(got, []string{testPortalUser, accessDefaultSecond}) {
		t.Errorf("release named %v", got)
	}
	if !slices.Equal(dropped, []string{testPortalUser, accessDefaultSecond}) {
		t.Errorf("dropped %v, want admin's newest and dev's, once each", dropped)
	}
	if got := l.release(); len(got) != 0 || len(dropped) != 2 {
		t.Errorf("a second release dropped %v (%v)", got, dropped)
	}
}
