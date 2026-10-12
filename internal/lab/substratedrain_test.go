package lab

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestPodReadyOff: the drain begins with the first ate-api-server pod that
// runs the candidate's image and is Ready; the pod of the release before, a
// candidate pod not Ready yet and a terminating one do not begin it.
func TestPodReadyOff(t *testing.T) {
	const before, candidate = "gsoci.azurecr.io/giantswarm/substrate/ateapi:1.7.0", "gsoci.azurecr.io/giantswarm/substrate/ateapi:1.8.2-rc.4"
	terminating := stepPod("api-old-candidate", candidate, true)
	now := metav1.Now()
	terminating.SetDeletionTimestamp(&now)
	waiting := []unstructured.Unstructured{stepPod("api-before", before, true), stepPod("api-starting", candidate, false), terminating}
	if got := podReadyOff(waiting, before); got != "" {
		t.Errorf("podReadyOff before the candidate is Ready = %q, want none", got)
	}
	if got := podReadyOff(append(waiting, stepPod("api-new", candidate, true)), before); got != "api-new" {
		t.Errorf("podReadyOff = %q, want api-new", got)
	}
}

// TestDrainablePods: a round deletes the worker pods that are not
// terminating already.
func TestDrainablePods(t *testing.T) {
	gone := stepPod("w2", "w", true)
	now := metav1.Now()
	gone.SetDeletionTimestamp(&now)
	got := drainablePods([]unstructured.Unstructured{stepPod("w1", "w", true), gone, stepPod("w3", "w", false)})
	if strings.Join(got, ",") != "w1,w3" {
		t.Errorf("drainablePods = %v, want w1 and w3", got)
	}
}

// TestGoldensOff: after the step every template that had a golden before
// must hold one in the layout the candidate writes — the one from before the
// step when it was in that layout already (a patch step, 1.8.1 to 1.8.2,
// keeps the goldens), a new one otherwise (a layout change takes them
// again) — and no template may record a terminal error, the
// GoldenActorCrashed a drain left on every template of a Substrate without
// the retry.
func TestGoldensOff(t *testing.T) {
	const a, b, aBefore, bBefore = "tmpl-a", "tmpl-b", "kagent/a-uid", "kagent/b-uid"
	const perVolume, next = snapshotLayoutPerVolume, snapshotLayoutPerVolume + 1
	before := goldenSet{
		golden:  map[string]string{a: aBefore, b: bBefore},
		layout:  map[string]int32{a: perVolume, b: snapshotLayoutUnrecorded},
		failure: map[string]string{},
	}

	// The candidate writes the layout a's golden is in: a's is kept, b's
	// (recorded without a layout) is taken again into it.
	kept := goldenSet{
		golden:  map[string]string{a: aBefore, b: "kagent/b-uid-2", "tmpl-c": "kagent/c-uid"},
		layout:  map[string]int32{a: perVolume, b: perVolume, "tmpl-c": perVolume},
		failure: map[string]string{},
	}
	if off := goldensOff(before, kept, perVolume); len(off) != 0 {
		t.Errorf("a's golden kept, b's taken again into the candidate's layout: %v, want none", off)
	}
	// The candidate writes a newer layout: every golden is taken again into it.
	retaken := goldenSet{
		golden: map[string]string{a: "kagent/a-uid-3", b: "kagent/b-uid-3"},
		layout: map[string]int32{a: next, b: next},
	}
	if off := goldensOff(before, retaken, next); len(off) != 0 {
		t.Errorf("every golden taken again into the new layout: %v, want none", off)
	}
	// Not there: a's golden kept in the older layout, b's gone, a's re-take
	// crashed.
	stale := goldenSet{
		golden:  map[string]string{a: aBefore},
		layout:  map[string]int32{a: perVolume},
		failure: map[string]string{a: "GoldenActorCrashed: resume failed: assigned worker is draining"},
	}
	off := goldensOff(before, stale, next)
	want := []string{
		"tmpl-a still holds kagent/a-uid, taken before the step in layout 2, not the layout 3 the candidate writes",
		"tmpl-b has no golden (was kagent/b-uid)",
		`tmpl-a records the terminal error "GoldenActorCrashed: resume failed: assigned worker is draining"`,
	}
	if strings.Join(off, "|") != strings.Join(want, "|") {
		t.Errorf("goldensOff = %q, want %q", off, want)
	}
	// A golden taken again into a layout the proof does not expect of the
	// candidate is named as such: currentSnapshotLayout is behind Substrate.
	if got := goldenOff(aBefore, "kagent/a-uid-3", next, perVolume); got != "was taken again as kagent/a-uid-3 in layout 3, not the layout 2 the candidate writes" {
		t.Errorf("goldenOff of an unexpected layout = %q", got)
	}
}

// TestGoldensWorded: the PASS line names each template's golden kept in its
// layout, or the one before and the one taken again with their layouts.
func TestGoldensWorded(t *testing.T) {
	const a, b, aBefore, bBefore = "tmpl-a", "tmpl-b", "kagent/a-uid", "kagent/b-uid"
	before := goldenSet{golden: map[string]string{a: aBefore, b: bBefore}, layout: map[string]int32{b: 2}}
	after := goldenSet{golden: map[string]string{a: "kagent/a-uid-2", b: bBefore}, layout: map[string]int32{a: 2, b: 2}}
	if got, want := goldensWorded(before, after), "2 templates (tmpl-a kagent/a-uid -> kagent/a-uid-2 (layout 0 -> 2); tmpl-b keeps kagent/b-uid (layout 2))"; got != want {
		t.Errorf("goldensWorded = %q, want %q", got, want)
	}
	if got := goldensWorded(goldenSet{}, after); !strings.Contains(got, "no template") {
		t.Errorf("goldensWorded without goldens = %q", got)
	}
}
