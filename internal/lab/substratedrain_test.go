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

// TestGoldensRetaken: every template that had a golden before the step must
// hold another one after it, and no template may record a terminal error —
// the GoldenActorCrashed a drain left on every template of a Substrate
// without the retry.
func TestGoldensRetaken(t *testing.T) {
	const a, aBefore = "tmpl-a", "kagent/a-uid"
	before := goldenSet{golden: map[string]string{a: aBefore, "tmpl-b": "kagent/b-uid"}, failure: map[string]string{}}
	retaken := goldenSet{golden: map[string]string{a: "kagent/a-uid-2", "tmpl-b": "kagent/b-uid-2", "tmpl-c": "kagent/c-uid"}, failure: map[string]string{}}
	if off := goldensRetaken(before, retaken); len(off) != 0 {
		t.Errorf("every golden re-taken: %v, want none", off)
	}
	crashed := goldenSet{
		golden:  map[string]string{a: aBefore},
		failure: map[string]string{a: "GoldenActorCrashed: resume failed: assigned worker is draining"},
	}
	off := goldensRetaken(before, crashed)
	want := []string{
		"tmpl-a still holds kagent/a-uid, taken before the step",
		"tmpl-b has no golden (was kagent/b-uid)",
		`tmpl-a records the terminal error "GoldenActorCrashed: resume failed: assigned worker is draining"`,
	}
	if strings.Join(off, "|") != strings.Join(want, "|") {
		t.Errorf("goldensRetaken = %q, want %q", off, want)
	}
}

// TestGoldensWorded: the PASS line names each template's golden before and
// after the step.
func TestGoldensWorded(t *testing.T) {
	before := goldenSet{golden: map[string]string{"tmpl-a": "kagent/a-uid"}}
	after := goldenSet{golden: map[string]string{"tmpl-a": "kagent/a-uid-2"}}
	if got, want := goldensWorded(before, after), "1 templates (tmpl-a kagent/a-uid -> kagent/a-uid-2)"; got != want {
		t.Errorf("goldensWorded = %q, want %q", got, want)
	}
	if got := goldensWorded(goldenSet{}, after); !strings.Contains(got, "no template") {
		t.Errorf("goldensWorded without goldens = %q", got)
	}
}
