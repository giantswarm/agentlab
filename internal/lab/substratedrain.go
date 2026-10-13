package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The drain case of the Substrate step proof: the roll of a multi-worker
// pool under the step. On an installation the step's WorkerPool and atelet
// DaemonSet roll while the new control plane takes every golden again and
// sessions resume, so a golden take or a resume lands on a worker that is
// draining, has gone away or has no ateom socket yet. A one-node lab rolls
// its workers too fast to meet that race, so the case forces it: from the
// moment the candidate's ate-api-server pod is Ready, every worker pod of the
// pool is deleted every substrateDrainEvery for substrateDrainFor (the way
// the race was reproduced by hand), and the suspended conversation's resume
// is issued while the drain runs — once the release before's ate-api-server
// pods are gone, so that it reaches the candidate (drainResume), which the
// candidate's RPC log must show. The proof then requires every golden of the
// lab in the layout the candidate writes with no terminal error, and the
// conversation resumed intact.

const (
	substrateDrainEvery = 8 * time.Second
	substrateDrainFor   = time.Minute
	// substrateDrainHandoverPoll is how often the resume looks for the
	// release before's ate-api-server pods to be gone, substrateDrainRetry
	// the wait before a resume is issued again.
	substrateDrainHandoverPoll = time.Second
	substrateDrainRetry        = 2 * time.Second
)

// workerDrain is one drain under a step: started by the candidate's
// ate-api-server, its rounds of worker pod deletes, and the resume issued
// during it.
type workerDrain struct {
	apiBefore string
	cancel    context.CancelFunc
	done      chan struct{}
	began     chan struct{}

	mu      sync.Mutex
	apiPod  string
	start   time.Time
	end     time.Time
	rounds  int
	deleted []string
	errs    []string
}

// startWorkerDrain waits in the background for an ate-api-server pod that
// runs another image than apiBefore and is Ready, then drains the workers
// for substrateDrainFor. wait ends it early or after it ran.
func startWorkerDrain(apiBefore string, timeout time.Duration) *workerDrain {
	ctx, cancel := context.WithTimeout(context.Background(), timeout+substrateDrainFor)
	d := &workerDrain{apiBefore: apiBefore, cancel: cancel, done: make(chan struct{}), began: make(chan struct{})}
	go d.run(ctx)
	return d
}

func (d *workerDrain) run(ctx context.Context) {
	defer close(d.done)
	for {
		pod, err := readyAPIPodOff(ctx, d.apiBefore)
		if err != nil && ctx.Err() == nil {
			d.fail("reading ate-api-server's pods: %v", err)
		}
		if pod != "" {
			d.mu.Lock()
			d.apiPod, d.start = pod, time.Now()
			d.mu.Unlock()
			note("drain: ate-api-server %s runs the candidate; deleting every worker pod every %s for %s", pod, substrateDrainEvery, substrateDrainFor)
			close(d.began)
			break
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				d.fail("stopped before it began: the step failed first")
			} else {
				d.fail("no ate-api-server pod of the candidate became Ready: %v", ctx.Err())
			}
			return
		case <-time.After(substrateStepPoll):
		}
	}
	until := time.Now().Add(substrateDrainFor)
	for time.Now().Before(until) {
		d.round(ctx)
		select {
		case <-ctx.Done():
			d.finish()
			return
		case <-time.After(substrateDrainEvery):
		}
	}
	d.finish()
}

// round deletes every worker pod that is not terminating already.
func (d *workerDrain) round(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, kubeReadTimeout)
	defer cancel()
	pods, err := listObjects(rctx, gvrPods, kagentNamespace, substrateWorkerLabel)
	if err != nil {
		d.fail("listing the worker pods: %v", err)
		return
	}
	var names []string
	for _, pod := range drainablePods(pods) {
		if err := deleteObject(rctx, gvrPods, kagentNamespace, pod, 0); err != nil {
			d.fail("%v", err)
			continue
		}
		names = append(names, pod)
	}
	d.mu.Lock()
	d.rounds++
	d.deleted = append(d.deleted, names...)
	round := d.rounds
	d.mu.Unlock()
	note("drain round %d at +%s: deleted %s", round, time.Since(d.start).Round(time.Second), wordedPods(names))
}

func (d *workerDrain) finish() {
	d.mu.Lock()
	d.end = time.Now()
	d.mu.Unlock()
	note("drain: ended after %s", d.end.Sub(d.start).Round(time.Second))
}

func (d *workerDrain) fail(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	note("drain: %s", line)
	d.mu.Lock()
	d.errs = append(d.errs, line)
	d.mu.Unlock()
}

// running reports whether the drain has begun and not ended.
func (d *workerDrain) running() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.start.IsZero() && d.end.IsZero()
}

// window is when the drain began and ended (the zero time while it runs).
func (d *workerDrain) window() (start, end time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.start, d.end
}

// wait blocks until the drain has ended (or given up) and returns its
// verdict: an error when it deleted no worker pod.
func (d *workerDrain) wait() error {
	<-d.done
	d.cancel()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.deleted) == 0 {
		return fmt.Errorf("the drain deleted no worker pod (%s): the step was not exercised under a drain", strings.Join(d.errs, "; "))
	}
	return nil
}

// stop ends the drain at once, on a failure before it ran its course.
func (d *workerDrain) stop() {
	d.cancel()
	<-d.done
}

// String words the drain for its PASS line.
func (d *workerDrain) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fmt.Sprintf("%d rounds over %s from ate-api-server %s Ready, %d worker pods deleted (%s)",
		d.rounds, d.end.Sub(d.start).Round(time.Second), d.apiPod, len(d.deleted), wordedPods(d.deleted))
}

// drainablePods are the names of the pods that are not terminating already.
func drainablePods(pods []unstructured.Unstructured) []string {
	var names []string
	for _, pod := range pods {
		if pod.GetDeletionTimestamp() == nil {
			names = append(names, pod.GetName())
		}
	}
	return names
}

func wordedPods(names []string) string {
	if len(names) == 0 {
		return "no pod"
	}
	return strings.Join(names, ", ")
}

// apiPods is every pod of the ate-api-server Deployment.
func apiPods(ctx context.Context) ([]unstructured.Unstructured, error) {
	rctx, cancel := context.WithTimeout(ctx, kubeReadTimeout)
	defer cancel()
	return listObjects(rctx, gvrPods, substrateNamespace, "app="+substrateAPIDeployment)
}

// readyAPIPodOff is the name of a Ready ate-api-server pod that runs another
// image than before (the candidate's), "" while there is none.
func readyAPIPodOff(ctx context.Context, before string) (string, error) {
	pods, err := apiPods(ctx)
	if err != nil {
		return "", err
	}
	return podReadyOff(pods, before), nil
}

// podReadyOff is the first pod that is Ready, not terminating, and whose
// first container runs another image than before.
func podReadyOff(pods []unstructured.Unstructured, before string) string {
	for _, pod := range pods {
		if img := podImage(pod); img == "" || img == before || pod.GetDeletionTimestamp() != nil {
			continue
		}
		conditions, _, _ := unstructured.NestedSlice(pod.Object, crStatus, crConditions)
		if ready, _ := readyConditionOf(conditions); ready {
			return pod.GetName()
		}
	}
	return ""
}

// podImage is the image of a pod's first container, "" without one.
func podImage(pod unstructured.Unstructured) string {
	containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", podContainersKey)
	if len(containers) == 0 {
		return ""
	}
	c, _ := containers[0].(map[string]any)
	img, _ := c[imageKey].(string)
	return img
}

// podsOn is the names of the pods whose first container runs image,
// terminating ones included: a pod of the release before that is still
// there still serves the connections it holds. podsOff is the others.
func podsOn(pods []unstructured.Unstructured, image string) []string {
	return podsWhere(pods, func(img string) bool { return img == image })
}

func podsOff(pods []unstructured.Unstructured, image string) []string {
	return podsWhere(pods, func(img string) bool { return img != image })
}

func podsWhere(pods []unstructured.Unstructured, keep func(image string) bool) []string {
	var names []string
	for _, pod := range pods {
		if keep(podImage(pod)) {
			names = append(names, pod.GetName())
		}
	}
	return names
}

// drainResume is the suspended conversation's resume during the drain. It
// is issued once the release before's ate-api-server pods are gone, so that
// it reaches the candidate: the kagent controller holds one connection to the
// ate-api Service for its lifetime, which the rollout moves only when the pod
// behind it goes, and the candidate's first pod is Ready while the release
// before's still serve — a resume issued then restores under the release
// before, which the candidate's RPC log never shows. It is issued again, as a
// client would, while kagent answers that it could not now (resumeAgain).
// Its outcome is read by the resume step, the candidate's RPC log by the
// restore step.
type drainResume struct {
	done    chan struct{}
	session *apiv1alpha1.Session
	err     error
	// handover is when the release before's pods were gone, issuedAt the
	// first attempt, answered the last answer; attempts counts them.
	handover, issuedAt, answered time.Time
	attempts                     int
}

// resumeDuringDrain issues the resume in the background once the drain has
// begun and the release before's ate-api-server pods are gone; a drain that
// ends with them still there is the error.
func (r *substrateStepRun) resumeDuringDrain(d *workerDrain) *drainResume {
	res := &drainResume{done: make(chan struct{})}
	go func() {
		defer close(res.done)
		select {
		case <-d.began:
		case <-d.done:
			res.err = fmt.Errorf("the drain never began, so the resume was not issued during one")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), substrateDrainFor+sessionReadyTimeout+kagentTurnTimeout)
		defer cancel()
		if res.err = awaitAPIHandover(ctx, d, r.before.api); res.err != nil {
			return
		}
		res.handover = time.Now()
		start, _ := d.window()
		note("drain: no ate-api-server pod of the release before is left at +%s; resuming Session %s during the drain", res.handover.Sub(start).Round(time.Second), r.session)
		for {
			res.attempts++
			if res.issuedAt.IsZero() {
				res.issuedAt = time.Now()
			}
			res.session, res.err = r.api.resumeSession(ctx, r.session)
			res.answered = time.Now()
			if res.err == nil || !resumeAgain(res.err) {
				return
			}
			note("drain: resume attempt %d answered at +%s: %v", res.attempts, res.answered.Sub(start).Round(time.Second), res.err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(substrateDrainRetry):
			}
		}
	}()
	return res
}

// worded is the resume's own account: when it was issued relative to the
// drain's start (and the release before's pods' going), its attempts and
// how it ended.
func (res *drainResume) worded(start time.Time) string {
	if res.issuedAt.IsZero() {
		return "the resume was not issued: " + res.err.Error()
	}
	outcome := "answered " + sessionState(res.session)
	if res.err != nil {
		outcome = "ended: " + res.err.Error()
	}
	return fmt.Sprintf("the resume issued at +%s (the release before's ate-api-server pods gone at +%s), %d attempt(s), %s at +%s",
		res.issuedAt.Sub(start).Round(time.Second), res.handover.Sub(start).Round(time.Second), res.attempts, outcome, res.answered.Sub(start).Round(time.Second))
}

// resumeAgain reports an answer to ResumeSession a client issues the resume
// again on: Unavailable (the controller's connection to ate-api moving with
// the rollout, or a resume Substrate still deferred at the controller's own
// bound) and Aborted (the operation pending from the attempt before, which
// the next one continues). Any other — FailedPrecondition, the runtime lost —
// is final; resumeSession has already waited out a "not yet settled".
func resumeAgain(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.Aborted:
		return true
	}
	return false
}

// awaitAPIHandover waits until no ate-api-server pod runs the release
// before's image, terminating ones included, looking every
// substrateDrainHandoverPoll while the drain runs: from then on every call
// reaches the candidate. A drain that ends with one still there is the
// error, naming it.
func awaitAPIHandover(ctx context.Context, d *workerDrain, before string) error {
	for {
		pods, err := apiPods(ctx)
		if err != nil {
			return fmt.Errorf("reading ate-api-server's pods for the handover: %w", err)
		}
		left := podsOn(pods, before)
		if len(left) == 0 {
			return nil
		}
		if !d.running() {
			return fmt.Errorf("the drain ended with ate-api-server %s of the release before still there, so no resume could reach the candidate during it: the kagent controller keeps its connection to the ate-api Service until the pod behind it goes", strings.Join(left, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(substrateDrainHandoverPoll):
		}
	}
}

// ateRPC is one RPC as ate-api-server logs it ("Handle RPC"), the fields the
// proof reads: the line is written when the handling ends, at time, after
// elapsed-time.
type ateRPC struct {
	Time    time.Time   `json:"time"`
	Method  string      `json:"method"`
	Req     ateRPCActor `json:"req"`
	Resp    ateRPCActor `json:"resp"`
	Err     string      `json:"err"`
	Elapsed string      `json:"elapsed-time"`
	// raw is the line as logged, the evidence a run's output quotes.
	raw string
}

// ateRPCActor is the actor a request names and a response carries.
type ateRPCActor struct {
	Actor struct {
		Name   string `json:"name"`
		Status struct {
			State int32 `json:"state"`
		} `json:"status"`
	} `json:"actor"`
}

// began is when the RPC arrived.
func (rpc ateRPC) began() time.Time {
	elapsed, err := time.ParseDuration(rpc.Elapsed)
	if err != nil {
		return rpc.Time
	}
	return rpc.Time.Add(-elapsed)
}

// outcome words how the RPC ended: the error, or the actor's state.
func (rpc ateRPC) outcome() string {
	if rpc.Err != "" {
		return rpc.Err
	}
	return strings.TrimPrefix(ateapi.ActorState(rpc.Resp.Actor.Status.State).String(), "ACTOR_STATE_")
}

// ateRPCs is every "Handle RPC" line of an ate-api-server log, a stream's
// prefix before the JSON skipped; other lines are left out.
func ateRPCs(logs string) []ateRPC {
	var rpcs []ateRPC
	for _, line := range strings.Split(logs, "\n") {
		i := strings.IndexByte(line, '{')
		if i < 0 {
			continue
		}
		var rpc ateRPC
		if err := json.Unmarshal([]byte(line[i:]), &rpc); err != nil || rpc.Method == "" {
			continue
		}
		rpc.raw = line[i:]
		rpcs = append(rpcs, rpc)
	}
	return rpcs
}

// actorResumes is every ResumeActor of the actor among the RPCs, in the
// order they arrived, and those of them that arrived from from to to.
func actorResumes(rpcs []ateRPC, actor string, from, to time.Time) (all, during []ateRPC) {
	for _, rpc := range rpcs {
		if rpc.Method != ateapi.Control_ResumeActor_FullMethodName || rpc.Req.Actor.Name != actor {
			continue
		}
		all = append(all, rpc)
	}
	slices.SortFunc(all, func(a, b ateRPC) int { return a.began().Compare(b.began()) })
	for _, rpc := range all {
		if at := rpc.began(); !at.Before(from) && !at.After(to) {
			during = append(during, rpc)
		}
	}
	return all, during
}

// resumesWorded words ResumeActors by when they arrived relative to start
// and how each ended: "at +3s Unavailable …; at +12s RUNNING".
func resumesWorded(rpcs []ateRPC, start time.Time) string {
	parts := make([]string, 0, len(rpcs))
	for _, rpc := range rpcs {
		parts = append(parts, fmt.Sprintf("at %s %s", offsetWorded(rpc.began().Sub(start)), excerpt(rpc.outcome(), 200)))
	}
	return strings.Join(parts, "; ")
}

// offsetWorded is a duration from a start, signed: +12s, -3s.
func offsetWorded(d time.Duration) string {
	d = d.Round(time.Second)
	if d < 0 {
		return d.String()
	}
	return "+" + d.String()
}

// goldenSet is the golden of every ActorTemplate of the lab in ate-api, by
// template: its tag, the snapshot layout it records (currentSnapshotLayout),
// and the terminal error a template records.
type goldenSet struct {
	golden, failure map[string]string
	layout          map[string]int32
}

// readGoldens reads every ActorTemplate of the kagent atespace in ate-api.
func readGoldens() (goldenSet, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	api, err := dialAteAPI(ctx)
	if err != nil {
		return goldenSet{}, err
	}
	defer api.Close()
	templates, err := ateTemplates(ctx, api, func(string) bool { return true })
	set := goldenSet{golden: map[string]string{}, failure: map[string]string{}, layout: map[string]int32{}}
	for _, t := range templates {
		name := t.GetMetadata().GetName()
		status := t.GetStatus().GetGoldenSnapshotStatus()
		if tag := status.GetGoldenTag(); tag.GetName() != "" {
			set.golden[name] = tag.GetAtespace() + "/" + tag.GetName()
			set.layout[name] = status.GetSnapshotLayout()
		}
		if msg := status.GetErrorMessage(); msg != "" {
			set.failure[name] = msg
		}
	}
	return set, err
}

// goldensOff words what keeps the goldens after a step from passing: every
// template that had a golden before must hold one in the layout the
// candidate writes (goldenOff), and no template may record a terminal error.
// Empty when they pass.
func goldensOff(before, after goldenSet, want int32) []string {
	var off []string
	for _, name := range slices.Sorted(maps.Keys(before.golden)) {
		if o := goldenOff(before.golden[name], after.golden[name], after.layout[name], want); o != "" {
			off = append(off, name+" "+o)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(after.failure)) {
		off = append(off, fmt.Sprintf("%s records the terminal error %q", name, after.failure[name]))
	}
	return off
}

// goldenOff words what keeps one template's golden after a step from
// passing, "" when it passes: the golden must be in the layout the candidate
// writes — the one from before the step when it was in that layout already
// (a step that keeps the layout keeps its goldens), a new one otherwise
// (Substrate takes a golden in an older layout again).
func goldenOff(was, now string, layout, want int32) string {
	switch {
	case now == "":
		return fmt.Sprintf("has no golden (was %s)", was)
	case layout == want:
		return ""
	case now == was:
		return fmt.Sprintf("still holds %s, taken before the step in layout %d, not the layout %d the candidate writes", now, layout, want)
	default:
		return fmt.Sprintf("was taken again as %s in layout %d, not the layout %d the candidate writes", now, layout, want)
	}
}

// awaitGoldensCurrent polls ate-api until every golden of before is in the
// layout the candidate writes with no terminal error, or the timeout passes.
func awaitGoldensCurrent(before goldenSet, want int32, timeout time.Duration) (goldenSet, error) {
	var after goldenSet
	var off []string
	var readErr error
	done := waitFor(int(timeout/substrateStepPoll), substrateStepPoll, func() bool {
		after, readErr = readGoldens()
		off = goldensOff(before, after, want)
		return readErr == nil && len(off) == 0
	})
	if done {
		return after, nil
	}
	if readErr != nil {
		off = append(off, readErr.Error())
	}
	return after, fmt.Errorf("after the drain, not every golden is in the snapshot layout the candidate writes within %s: %s", timeout, strings.Join(off, "; "))
}

// goldensWorded words the goldens after the step: each template's golden
// kept in its layout, or the one before and the one taken again.
func goldensWorded(before, after goldenSet) string {
	names := slices.Sorted(maps.Keys(before.golden))
	if len(names) == 0 {
		return "no template had a golden before the step"
	}
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		was, now := before.golden[name], after.golden[name]
		if now == was {
			pairs = append(pairs, fmt.Sprintf("%s keeps %s (layout %d)", name, now, after.layout[name]))
			continue
		}
		pairs = append(pairs, fmt.Sprintf("%s %s -> %s (layout %d -> %d)", name, was, now, before.layout[name], after.layout[name]))
	}
	return fmt.Sprintf("%d templates (%s)", len(names), strings.Join(pairs, "; "))
}
