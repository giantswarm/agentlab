package lab

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
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
	const a, b, c, aBefore, bBefore = "tmpl-a", "tmpl-b", "tmpl-c", "kagent/a-uid", "kagent/b-uid"
	const perVolume, next = snapshotLayoutPerVolume, snapshotLayoutGVisorNightly20260909
	before := goldenSet{
		golden:  map[string]string{a: aBefore, b: bBefore},
		layout:  map[string]int32{a: perVolume, b: snapshotLayoutUnrecorded},
		failure: map[string]string{},
	}

	// The candidate writes the layout a's golden is in: a's is kept, b's
	// (recorded without a layout) is taken again into it.
	kept := goldenSet{
		golden:    map[string]string{a: aBefore, b: "kagent/b-uid-2", c: "kagent/c-uid"},
		layout:    map[string]int32{a: perVolume, b: perVolume, c: perVolume},
		failure:   map[string]string{},
		templates: map[string]bool{a: true, b: true, c: true},
	}
	if off := goldensOff(before, kept, perVolume); len(off) != 0 {
		t.Errorf("a's golden kept, b's taken again into the candidate's layout: %v, want none", off)
	}
	// The candidate writes a newer layout: every golden is taken again into it.
	retaken := goldenSet{
		golden:    map[string]string{a: "kagent/a-uid-3", b: "kagent/b-uid-3"},
		layout:    map[string]int32{a: next, b: next},
		templates: map[string]bool{a: true, b: true},
	}
	if off := goldensOff(before, retaken, next); len(off) != 0 {
		t.Errorf("every golden taken again into the new layout: %v, want none", off)
	}
	// Not there: a's golden kept in the older layout, b's gone, a's re-take
	// crashed.
	stale := goldenSet{
		golden:    map[string]string{a: aBefore},
		layout:    map[string]int32{a: perVolume},
		failure:   map[string]string{a: "GoldenActorCrashed: resume failed: assigned worker is draining"},
		templates: map[string]bool{a: true, b: true},
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
	// A superseded revision kagent deleted during the run is no longer
	// listed: nothing is left to take again, the others are judged alone.
	deleted := goldenSet{
		golden:    map[string]string{a: "kagent/a-uid-3"},
		layout:    map[string]int32{a: next},
		templates: map[string]bool{a: true},
	}
	if off := goldensOff(before, deleted, next); len(off) != 0 {
		t.Errorf("b deleted during the run, a taken again: %v, want none", off)
	}
	deleted.layout[a] = perVolume
	if off := goldensOff(before, deleted, next); len(off) != 1 || !strings.HasPrefix(off[0], a+" ") {
		t.Errorf("b deleted during the run, a in the old layout: %v, want only a's", off)
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
	after := goldenSet{golden: map[string]string{a: "kagent/a-uid-2", b: bBefore}, layout: map[string]int32{a: 2, b: 2}, templates: map[string]bool{a: true, b: true}}
	if got, want := goldensWorded(before, after), "2 templates (tmpl-a kagent/a-uid -> kagent/a-uid-2 (layout 0 -> 2); tmpl-b keeps kagent/b-uid (layout 2))"; got != want {
		t.Errorf("goldensWorded = %q, want %q", got, want)
	}
	delete(after.templates, b)
	if got, want := goldensWorded(before, after), "2 templates (tmpl-a kagent/a-uid -> kagent/a-uid-2 (layout 0 -> 2); tmpl-b deleted during the run (was kagent/b-uid))"; got != want {
		t.Errorf("goldensWorded with b deleted = %q, want %q", got, want)
	}
	if got := goldensWorded(goldenSet{}, after); !strings.Contains(got, "no template") {
		t.Errorf("goldensWorded without goldens = %q", got)
	}
}

// TestPodsOnImage: the release before's ate-api-server pods are those on its
// image, a terminating one included (it serves the connections it holds
// until it is gone); the candidate's are the others.
func TestPodsOnImage(t *testing.T) {
	const before, candidate = "gsoci.azurecr.io/giantswarm/substrate/ateapi:1.8.1", "gsoci.azurecr.io/giantswarm/substrate/ateapi:1.8.2-rc.6"
	terminating := stepPod("api-old-2", before, true)
	now := metav1.Now()
	terminating.SetDeletionTimestamp(&now)
	pods := []unstructured.Unstructured{stepPod("api-old-1", before, true), terminating, stepPod("api-new-1", candidate, true), stepPod("api-new-2", candidate, false)}
	if got := strings.Join(podsOn(pods, before), ","); got != "api-old-1,api-old-2" {
		t.Errorf("podsOn = %s, want the release before's two, the terminating one included", got)
	}
	if got := strings.Join(podsOff(pods, before), ","); got != "api-new-1,api-new-2" {
		t.Errorf("podsOff = %s, want the candidate's two", got)
	}
	if got := podsOn(nil, before); got != nil {
		t.Errorf("podsOn of no pods = %v", got)
	}
}

// The RPC log lines of ate-api-server as it writes them, the fields the
// proof reads kept: a ResumeActor that restored the actor, one the drain cut,
// one of another actor, a GetWorker, a line prefixed by its stream, and lines
// that are no RPC.
const ateAPILog = `{"time":"2026-10-12T04:00:12.500000000Z","level":"INFO","msg":"ateapi starting","version":"dev"}
{"time":"2026-10-12T04:00:12.500000000Z","level":"INFO","msg":"Handle RPC","method":"/ateapi.Control/ResumeActor","req":{"actor":{"atespace":"kagent","name":"session-s1"},"fencing_token":{"holder":"h","generation":3}},"resp":{"actor":{"metadata":{"atespace":"kagent","name":"session-s1","uid":"u1"},"status":{"state":2}}},"err":null,"elapsed-time":"2.5s","principal":{"ID":"system:serviceaccount:kagent:kagent-controller","Kind":"jwt","Issuer":""}}
[ate-api-server-b/ate-api-server] {"time":"2026-10-12T04:01:30.000000000Z","level":"INFO","msg":"Handle RPC","method":"/ateapi.Control/ResumeActor","req":{"actor":{"atespace":"kagent","name":"session-s1"}},"resp":null,"err":"rpc error: code = Unavailable desc = resume failed: assigned worker is draining","elapsed-time":"1.2s","principal":{"ID":"system:serviceaccount:kagent:kagent-controller","Kind":"jwt","Issuer":""}}
{"time":"2026-10-12T04:00:20.000000000Z","level":"INFO","msg":"Handle RPC","method":"/ateapi.Control/ResumeActor","req":{"actor":{"atespace":"kagent","name":"session-s2"}},"resp":{"actor":{"status":{"state":2}}},"err":null,"elapsed-time":"729.694µs"}
{"time":"2026-10-12T04:00:21.000000000Z","level":"INFO","msg":"Handle RPC","method":"/ateapi.Control/GetWorker","req":{"worker":{"name":"w"}},"resp":null,"err":"rpc error: code = NotFound desc = Worker w not found","elapsed-time":"359.267µs"}
not json at all
`

// TestActorResumes: the ResumeActors of the actor are read off the RPC log
// in the order they arrived (the line's time less its elapsed-time), the
// stream prefix and the lines that are no RPC skipped; those that arrived
// while the drain ran are told apart from the ones outside it, and each is
// worded by its offset from the drain's start and how it ended.
func TestActorResumes(t *testing.T) {
	rpcs := ateRPCs(ateAPILog)
	if len(rpcs) != 4 {
		t.Fatalf("ateRPCs read %d RPCs, want 4:\n%+v", len(rpcs), rpcs)
	}
	start := time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC)
	end := start.Add(64 * time.Second)
	all, during := actorResumes(rpcs, "session-s1", start, end)
	if len(all) != 2 || len(during) != 1 {
		t.Fatalf("actorResumes = %d all, %d during, want 2 and 1", len(all), len(during))
	}
	if got := during[0].began(); !got.Equal(start.Add(10 * time.Second)) {
		t.Errorf("the restore arrived at %s, want +10s (12.5s less 2.5s)", got)
	}
	if raw := all[1].raw; !strings.HasPrefix(raw, `{"time":"2026-10-12T04:01:30`) || strings.Contains(raw, "[ate-api-server-b") {
		t.Errorf("the line is kept as logged, its stream prefix skipped, got %q", raw)
	}
	if got := resumesWorded(during, start); got != "at +10s RUNNING" {
		t.Errorf("resumesWorded(during) = %q", got)
	}
	if got := resumesWorded(all, start); got != "at +10s RUNNING; at +1m29s rpc error: code = Unavailable desc = resume failed: assigned worker is draining" {
		t.Errorf("resumesWorded(all) = %q", got)
	}
	if got := offsetWorded(-3 * time.Second); got != "-3s" {
		t.Errorf("offsetWorded(-3s) = %q", got)
	}
	if _, during := actorResumes(rpcs, "session-s3", start, end); len(during) != 0 {
		t.Errorf("an actor without a resume has %d", len(during))
	}
}

// TestResumeAgain: a client issues the resume again on Unavailable and
// Aborted, wrapped or not; a FailedPrecondition, the runtime lost, and an
// answer that is no status are final.
func TestResumeAgain(t *testing.T) {
	for _, tc := range []struct {
		err   error
		again bool
	}{
		{status.Error(codes.Unavailable, "Failed to resume Session"), true},
		{fmt.Errorf("ResumeSession s1: %w", status.Error(codes.Unavailable, "Failed to resume Session")), true},
		{status.Error(codes.Aborted, "lifecycle operation is pending"), true},
		{status.Error(codes.FailedPrecondition, "session runtime is lost"), false},
		{status.Error(codes.NotFound, "Session not found"), false},
		{errors.New("session s1 is still SUSPENDED after 1m30s"), false},
		{nil, false},
	} {
		if got := resumeAgain(tc.err); got != tc.again {
			t.Errorf("resumeAgain(%v) = %v, want %v", tc.err, got, tc.again)
		}
	}
}

// TestDrainResumeWorded: the resume's account names when it was issued and
// the release before's pods were gone, its attempts and how it ended; one
// never issued says why.
func TestDrainResumeWorded(t *testing.T) {
	start := time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC)
	res := &drainResume{handover: start.Add(9 * time.Second), issuedAt: start.Add(9 * time.Second), answered: start.Add(31 * time.Second), attempts: 2,
		session: &apiv1alpha1.Session{State: apiv1alpha1.RuntimeState_RUNTIME_STATE_READY}}
	if got, want := res.worded(start), "the resume issued at +9s (the release before's ate-api-server pods gone at +9s), 2 attempt(s), answered READY at +31s"; got != want {
		t.Errorf("worded = %q, want %q", got, want)
	}
	res.err = errors.New("ResumeSession s1: rpc error: code = FailedPrecondition desc = session runtime is lost")
	if got := res.worded(start); !strings.Contains(got, "ended: ResumeSession s1") {
		t.Errorf("a resume that ended in an error must say so, got %q", got)
	}
	never := &drainResume{err: errors.New("the drain ended with ate-api-server api-old still there")}
	if got := never.worded(start); !strings.HasPrefix(got, "the resume was not issued: the drain ended") {
		t.Errorf("a resume never issued = %q", got)
	}
}
