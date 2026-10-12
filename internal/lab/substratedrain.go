package lab

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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
// is issued while the drain runs. The proof then requires every golden of
// the lab taken again on the candidate with no terminal error, and the
// conversation resumed intact.

const (
	substrateDrainEvery = 8 * time.Second
	substrateDrainFor   = time.Minute
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

// readyAPIPodOff is the name of a Ready ate-api-server pod that runs another
// image than before (the candidate's), "" while there is none.
func readyAPIPodOff(ctx context.Context, before string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, kubeReadTimeout)
	defer cancel()
	pods, err := listObjects(rctx, gvrPods, substrateNamespace, "app="+substrateAPIDeployment)
	if err != nil {
		return "", err
	}
	return podReadyOff(pods, before), nil
}

// podReadyOff is the first pod that is Ready, not terminating, and whose
// first container runs another image than before.
func podReadyOff(pods []unstructured.Unstructured, before string) string {
	for _, pod := range pods {
		containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", podContainersKey)
		if len(containers) == 0 || pod.GetDeletionTimestamp() != nil {
			continue
		}
		c, _ := containers[0].(map[string]any)
		if img, _ := c[imageKey].(string); img == before {
			continue
		}
		conditions, _, _ := unstructured.NestedSlice(pod.Object, crStatus, crConditions)
		if ready, _ := readyConditionOf(conditions); ready {
			return pod.GetName()
		}
	}
	return ""
}

// drainResume is the suspended conversation's resume, issued once the drain
// has begun; its outcome is read by the resume step.
type drainResume struct {
	done     chan struct{}
	session  *apiv1alpha1.Session
	err      error
	issuedAt time.Time
	inDrain  bool
}

// resumeDuringDrain issues the resume as soon as the drain begins (or gives
// up), in the background.
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
		ctx, cancel := context.WithTimeout(context.Background(), sessionReadyTimeout+kagentTurnTimeout)
		defer cancel()
		res.issuedAt = time.Now()
		res.inDrain = d.running()
		note("drain: resuming Session %s during the drain", r.session)
		res.session, res.err = r.api.resumeSession(ctx, r.session)
	}()
	return res
}

// goldenSet is the golden of every ActorTemplate of the lab in ate-api, by
// template, and the terminal error a template records.
type goldenSet struct {
	golden, failure map[string]string
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
	set := goldenSet{golden: map[string]string{}, failure: map[string]string{}}
	for _, t := range templates {
		name := t.GetMetadata().GetName()
		status := t.GetStatus().GetGoldenSnapshotStatus()
		if tag := status.GetGoldenTag(); tag.GetName() != "" {
			set.golden[name] = tag.GetAtespace() + "/" + tag.GetName()
		}
		if msg := status.GetErrorMessage(); msg != "" {
			set.failure[name] = msg
		}
	}
	return set, err
}

// goldensRetaken words what keeps the goldens after a step from passing:
// every template that had a golden before must hold another one, and no
// template may record a terminal error. Empty when they pass.
func goldensRetaken(before, after goldenSet) []string {
	var off []string
	for _, name := range slices.Sorted(maps.Keys(before.golden)) {
		now, was := after.golden[name], before.golden[name]
		switch now {
		case "":
			off = append(off, fmt.Sprintf("%s has no golden (was %s)", name, was))
		case was:
			off = append(off, fmt.Sprintf("%s still holds %s, taken before the step", name, now))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(after.failure)) {
		off = append(off, fmt.Sprintf("%s records the terminal error %q", name, after.failure[name]))
	}
	return off
}

// awaitGoldensRetaken polls ate-api until every golden of before is taken
// again with no terminal error, or the timeout passes.
func awaitGoldensRetaken(before goldenSet, timeout time.Duration) (goldenSet, error) {
	var after goldenSet
	var off []string
	var readErr error
	done := waitFor(int(timeout/substrateStepPoll), substrateStepPoll, func() bool {
		after, readErr = readGoldens()
		off = goldensRetaken(before, after)
		return readErr == nil && len(off) == 0
	})
	if done {
		return after, nil
	}
	if readErr != nil {
		off = append(off, readErr.Error())
	}
	return after, fmt.Errorf("after the drain, not every golden was taken again within %s: %s", timeout, strings.Join(off, "; "))
}

// goldensWorded words the goldens taken again: each template's golden before
// and after the step.
func goldensWorded(before, after goldenSet) string {
	names := slices.Sorted(maps.Keys(before.golden))
	if len(names) == 0 {
		return "no template had a golden before the step"
	}
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, fmt.Sprintf("%s %s -> %s", name, before.golden[name], after.golden[name]))
	}
	return fmt.Sprintf("%d templates (%s)", len(names), strings.Join(pairs, "; "))
}
