package lab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Masterminds/semver/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

// A proof runs against the components the lab ran when it started. The meta
// chart's OCIRepositories track most components by semver range, and
// source-controller resolves a range again on every interval: a component
// release published mid-proof would roll its HelmRelease under the proof (a
// muster upgrade drops every MCP sign-in). So a proof holds the ranges for its
// length: every OCIRepository in the platform namespace whose ref is a range
// is suspended — it keeps the artifact it resolved, its HelmRelease keeps the
// chart it runs — and resumed when the proof ends. An exact version never
// moves and is left alone (the connectivity chart a chartPath lab re-pushes
// under its tag among them).
//
// The hold is the lab's, not the person's: a source suspended without the
// annotation is someone's deliberate suspend and is never touched. Each proof
// adds its holder (<host>/<pid>) to the annotation and removes it at the end;
// the last one out resumes the source, so two proofs on one lab do not lift
// each other's hold. A holder whose process is gone (a killed proof) is
// pruned by the next hold or release on the same host.
//
// An explicit change of what the lab runs wins over a hold: `agentlab
// platform` (and `up`, and the step proofs that install a stepped chart)
// lifts every hold before it installs (liftRangeHolds), so a
// platform.devImages or platform.chartPath swap, an overlay's version and a
// new chart all apply, and the proof running in this process takes its hold
// again on the components it installed (resumeRangeHold).
//
// A release published while the proof held its range rolls when the range
// resumes. The proof does not return under that roll: its release waits until
// every HelmRelease on a source that moved is Ready at the new chart
// (settleResumed), so the next command meets the lab steady. An interrupt
// resumes without waiting.

const (
	// rangeHoldAnnotation lists the proofs holding a source, comma-separated.
	rangeHoldAnnotation = "agentlab.giantswarm.io/range-hold"
	// rangeHoldFieldManager owns spec.suspend while a hold is on.
	rangeHoldFieldManager = "agentlab-range-hold"
	// helmReleaseDeployed is a Helm release snapshot's status once the
	// upgrade succeeded.
	helmReleaseDeployed = "deployed"
)

// rangeSettleTimeout bounds the wait for the releases a resume rolls: a
// chart's pull and Helm's wait for its workloads. A var for the tests.
var rangeSettleTimeout = 10 * time.Minute

// RangeHold is one proof's hold on the platform's version ranges.
type RangeHold struct {
	holder  string
	sources []string
	// resumed are the sources the last drop resumed, with the artifact
	// revision each held, for settleResumed.
	resumed []heldSource
	stop    func()
}

// heldSource is a source the hold resumed and the artifact revision it kept
// while suspended.
type heldSource struct {
	name     string
	revision string
}

var (
	activeHoldMu sync.Mutex
	// activeHold is the hold of the proof this process runs, for platformUp
	// to take again after an install.
	activeHold *RangeHold
	// rangeHoldHost and processAlive are test seams.
	rangeHoldHost = os.Hostname
	processAlive  = pidAlive
)

// HoldComponentRanges suspends the platform's range-tracking OCIRepositories
// for the proof this process runs, and names them. A lab without Flux's
// sources (no platform) holds nothing. An interrupt resumes them before the
// process ends.
func HoldComponentRanges(ctx context.Context) (*RangeHold, error) {
	host, err := rangeHoldHost()
	if err != nil {
		return nil, fmt.Errorf("naming the range hold: %w", err)
	}
	h := &RangeHold{holder: host + "/" + strconv.Itoa(os.Getpid())}
	if err := h.take(ctx); err != nil {
		return nil, err
	}
	if len(h.sources) > 0 {
		note("Holding the version ranges of %s for the proof (suspended; resumed when it ends)", strings.Join(h.sources, ", "))
	}
	h.stop = releaseOnSignal(h)
	activeHoldMu.Lock()
	activeHold = h
	activeHoldMu.Unlock()
	return h, nil
}

// take adds the holder to every range source, suspending the ones no proof
// held yet.
func (h *RangeHold) take(ctx context.Context) error {
	gvr, sources, err := rangeSources(ctx)
	if err != nil || gvr == nil {
		return err
	}
	h.sources = nil
	for _, src := range sources {
		if !resolvedCurrentSpec(&src) {
			// Suspended now, it would never resolve: the release would wait
			// for a chart forever.
			warn("not holding the version range of OCIRepository %s/%s: it has not resolved its current spec yet", platformNamespace, src.GetName())
			continue
		}
		held, err := updateHold(ctx, *gvr, src.GetName(), func(holders []string, ownSuspend bool) ([]string, bool, bool) {
			if ownSuspend {
				return nil, false, false
			}
			if !slices.Contains(holders, h.holder) {
				holders = append(holders, h.holder)
			}
			return holders, true, true
		})
		if err != nil {
			return err
		}
		if held {
			h.sources = append(h.sources, src.GetName())
		}
	}
	return nil
}

// Release removes the proof's hold; a source no other proof holds resumes,
// source-controller resolves its range again, and Release returns once the
// releases on a source that moved are Ready at its new chart. Safe to call
// twice and on a nil hold.
func (h *RangeHold) Release(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if h.stop != nil {
		h.stop()
		h.stop = nil
	}
	activeHoldMu.Lock()
	if activeHold == h {
		activeHold = nil
	}
	activeHoldMu.Unlock()
	if err := h.drop(ctx); err != nil {
		return err
	}
	resumed := h.resumed
	h.resumed = nil
	return settleResumed(ctx, resumed)
}

// drop removes the holder from the sources it held, and records the ones it
// resumed in h.resumed.
func (h *RangeHold) drop(ctx context.Context) error {
	if len(h.sources) == 0 {
		return nil
	}
	gvr, err := ociRepositoryGVR()
	if err != nil || gvr == nil {
		return err
	}
	var errs []error
	for _, name := range h.sources {
		// The artifact the hold kept: what the source's HelmReleases run.
		var revision string
		if src, err := getObject(ctx, *gvr, platformNamespace, name); err == nil {
			revision, _, _ = unstructured.NestedString(src.Object, "status", "artifact", "revision")
		}
		held, err := updateHold(ctx, *gvr, name, func(holders []string, _ bool) ([]string, bool, bool) {
			holders = slices.DeleteFunc(holders, func(x string) bool { return x == h.holder })
			return holders, len(holders) > 0, true
		})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
		if err == nil && !held {
			h.resumed = append(h.resumed, heldSource{name: name, revision: revision})
		}
	}
	if len(errs) == 0 {
		note("Resumed the version ranges of %s", strings.Join(h.sources, ", "))
	}
	h.sources = nil
	return errors.Join(errs...)
}

// settleResumed waits for what resuming the sources set off: source-controller
// resolves each range again, and on a source whose artifact moved (a release
// published while the proof held it) helm-controller upgrades every
// HelmRelease whose chartRef names it. It returns once each of those is Ready
// at the new chart — Helm's wait has rolled its workloads — so the command
// after the proof never meets the roll. A source that did not move returns at
// once.
func settleResumed(ctx context.Context, resumed []heldSource) error {
	if len(resumed) == 0 {
		return nil
	}
	srcGVR, err := ociRepositoryGVR()
	if err != nil || srcGVR == nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, rangeSettleTimeout)
	defer cancel()
	var errs []error
	for _, r := range resumed {
		revision, err := waitSourceObserved(ctx, *srcGVR, r.name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if revision == "" || revision == r.revision {
			continue
		}
		note("OCIRepository %s moved from %s to %s while the proof held it: waiting for its releases to roll", r.name, orNone(r.revision), revision)
		if err := waitReleasesAt(ctx, r.name, revision); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// waitSourceObserved waits until source-controller has reconciled the
// source's resumed spec, failed or not, and returns its artifact revision.
func waitSourceObserved(ctx context.Context, gvr schema.GroupVersionResource, name string) (string, error) {
	var revision string
	err := wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
		src, err := getObject(ctx, gvr, platformNamespace, name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		revision, _, _ = unstructured.NestedString(src.Object, "status", "artifact", "revision")
		observed, _, _ := unstructured.NestedInt64(src.Object, "status", "observedGeneration")
		var readyObserved int64
		if c := condition(src, conditionReady); c != nil {
			readyObserved, _, _ = unstructured.NestedInt64(c, "observedGeneration")
		}
		return max(observed, readyObserved) >= src.GetGeneration(), nil
	})
	if wait.Interrupted(err) {
		return "", fmt.Errorf("OCIRepository %s/%s did not reconcile its resumed range within %s; check `kubectl -n %s describe ocirepository %s`", platformNamespace, name, rangeSettleTimeout, platformNamespace, name)
	}
	return revision, err
}

// waitReleasesAt waits until every HelmRelease on the source is Ready at the
// artifact revision, and names each as it gets there. A release helm-controller
// gave up on ends the wait with its message.
func waitReleasesAt(ctx context.Context, source, revision string) error {
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return err
	}
	var pending []string
	err = wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
		releases, err := listObjects(ctx, gvr, "", "")
		if err != nil {
			return false, err
		}
		pending = pending[:0]
		for i := range releases {
			hr := &releases[i]
			if !chartRefNames(hr, source) {
				continue
			}
			if suspended, _, _ := unstructured.NestedBool(hr.Object, crSpec, "suspend"); suspended {
				continue
			}
			ready, message := conditionStatus(hr, conditionReady), conditionMessage(hr, conditionReady)
			if ready == condFalseStatus && strings.Contains(message, "retries exhausted") {
				return false, fmt.Errorf("HelmRelease %s/%s failed to roll to %s after the version ranges resumed: %s", hr.GetNamespace(), hr.GetName(), revision, message)
			}
			if !releaseAt(hr, revision) {
				pending = append(pending, fmt.Sprintf("%s/%s (Ready=%s %s)", hr.GetNamespace(), hr.GetName(), orNone(ready), message))
			}
		}
		return len(pending) == 0, nil
	})
	if wait.Interrupted(err) {
		return fmt.Errorf("HelmRelease(s) on OCIRepository %s not Ready at %s within %s after the version ranges resumed: %s; check `kubectl -n %s describe helmrelease`", source, revision, rangeSettleTimeout, strings.Join(pending, ", "), platformNamespace)
	}
	if err == nil {
		note("The releases on OCIRepository %s run %s", source, revision)
	}
	return err
}

// chartRefNames reports whether the HelmRelease's chart comes from the
// platform namespace's OCIRepository of that name.
func chartRefNames(hr *unstructured.Unstructured, source string) bool {
	ref, _, _ := unstructured.NestedStringMap(hr.Object, crSpec, "chartRef")
	ns := ref["namespace"]
	if ns == "" {
		ns = hr.GetNamespace()
	}
	return ref["kind"] == kindOCIRepository && ref["name"] == source && ns == platformNamespace
}

// releaseAt reports whether helm-controller has upgraded the HelmRelease to
// the artifact revision (<version>@<digest>) and Helm deployed it: the
// attempted chart is the artifact's, the newest snapshot is that chart
// deployed, and Ready is True.
func releaseAt(hr *unstructured.Unstructured, revision string) bool {
	version, digest, _ := strings.Cut(revision, "@")
	attempted, _, _ := unstructured.NestedString(hr.Object, "status", "lastAttemptedRevision")
	attemptedDigest, _, _ := unstructured.NestedString(hr.Object, "status", "lastAttemptedRevisionDigest")
	if digest != "" && attemptedDigest != digest {
		return false
	}
	if attemptedVersion, _, _ := strings.Cut(attempted, "+"); attemptedVersion != strings.SplitN(version, "+", 2)[0] {
		return false
	}
	history, _, _ := unstructured.NestedSlice(hr.Object, "status", "history")
	if len(history) == 0 {
		return false
	}
	latest, _ := history[0].(map[string]any)
	chartVersion, _, _ := unstructured.NestedString(latest, "chartVersion")
	status, _, _ := unstructured.NestedString(latest, "status")
	return chartVersion == attempted && status == helmReleaseDeployed && conditionStatus(hr, conditionReady) == conditionTrue
}

// liftRangeHolds resumes every source a proof holds, whoever holds it: the
// install that follows decides what runs. Returns the hold of this process's
// proof, for resumeRangeHold to take again.
func liftRangeHolds(ctx context.Context) (*RangeHold, error) {
	gvr, sources, err := rangeSources(ctx)
	if err != nil || gvr == nil {
		return nil, err
	}
	var lifted []string
	for _, src := range sources {
		if len(rangeHolders(&src)) == 0 {
			continue
		}
		if _, err := updateHold(ctx, *gvr, src.GetName(), func([]string, bool) ([]string, bool, bool) {
			return nil, false, true
		}); err != nil {
			return nil, err
		}
		lifted = append(lifted, src.GetName())
	}
	if len(lifted) > 0 {
		note("Lifted the proof's hold on the version ranges of %s: this install decides what runs", strings.Join(lifted, ", "))
	}
	activeHoldMu.Lock()
	defer activeHoldMu.Unlock()
	return activeHold, nil
}

// resumeRangeHold takes the hold of this process's proof again after an
// install, on the versions the install left running.
func resumeRangeHold(ctx context.Context, h *RangeHold) error {
	if h == nil {
		return nil
	}
	if err := h.take(ctx); err != nil {
		return err
	}
	if len(h.sources) > 0 {
		note("Holding the version ranges of %s again for the rest of the proof", strings.Join(h.sources, ", "))
	}
	return nil
}

// releaseOnSignal resumes the hold when the proof is interrupted, then lets
// the signal take its course. The returned func stops the watch.
func releaseOnSignal(h *RangeHold) func() {
	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigs:
			signal.Stop(sigs)
			if err := h.drop(context.Background()); err != nil {
				warn("resuming the version ranges after %s: %v", sig, err)
			}
			resignal(sig)
		case <-done:
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			signal.Stop(sigs)
			close(done)
		})
	}
}

// ociRepositoryGVR is the OCIRepository resource, nil where the cluster
// serves none (no platform, no Flux).
func ociRepositoryGVR() (*schema.GroupVersionResource, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	gvr, err := k.gvrFor(fluxOCIRepositoryResource)
	if err != nil {
		return nil, nil //nolint:nilerr // no Flux sources, nothing to hold
	}
	return &gvr, nil
}

// rangeSources are the platform namespace's OCIRepositories whose ref is a
// semver range rather than an exact version.
func rangeSources(ctx context.Context) (*schema.GroupVersionResource, []unstructured.Unstructured, error) {
	gvr, err := ociRepositoryGVR()
	if err != nil || gvr == nil {
		return nil, nil, err
	}
	all, err := listObjects(ctx, *gvr, platformNamespace, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	sources := slices.DeleteFunc(all, func(src unstructured.Unstructured) bool {
		ref, _, _ := unstructured.NestedString(src.Object, "spec", "ref", "semver")
		return !isVersionRange(ref)
	})
	return gvr, sources, nil
}

// isVersionRange reports whether an OCIRepository's spec.ref.semver can
// resolve to more than one version.
func isVersionRange(ref string) bool {
	if ref == "" {
		return false
	}
	_, err := semver.StrictNewVersion(strings.TrimPrefix(strings.TrimSpace(ref), "v"))
	return err != nil
}

// resolvedCurrentSpec reports whether source-controller has an artifact for
// the source's current spec. A source the hold itself suspended counts: its
// artifact is the one the hold keeps.
func resolvedCurrentSpec(src *unstructured.Unstructured) bool {
	revision, _, _ := unstructured.NestedString(src.Object, "status", "artifact", "revision")
	if revision == "" {
		return false
	}
	observed, _, _ := unstructured.NestedInt64(src.Object, "status", "observedGeneration")
	return observed == src.GetGeneration() || len(rangeHolders(src)) > 0
}

// rangeHolders are the holders the source's annotation lists.
func rangeHolders(src *unstructured.Unstructured) []string {
	var holders []string
	for _, h := range strings.Split(src.GetAnnotations()[rangeHoldAnnotation], ",") {
		if h = strings.TrimSpace(h); h != "" {
			holders = append(holders, h)
		}
	}
	return holders
}

// pruneDeadHolders drops the holders on this host whose process is gone.
func pruneDeadHolders(holders []string) []string {
	host, err := rangeHoldHost()
	if err != nil {
		return holders
	}
	return slices.DeleteFunc(holders, func(holder string) bool {
		h, p, ok := strings.Cut(holder, "/")
		if !ok || h != host {
			return false
		}
		pid, err := strconv.Atoi(p)
		return err == nil && pid != os.Getpid() && !processAlive(pid)
	})
}

// updateHold rewrites one source's holders and suspend under optimistic
// concurrency. change gets the live holders (dead ones pruned) and whether the
// source carries someone's own suspend (suspended, and no hold was ever
// recorded on it), and returns the new holders, the new suspend and
// whether to write at all. Reports whether the source is held afterwards.
func updateHold(ctx context.Context, gvr schema.GroupVersionResource, name string, change func(holders []string, ownSuspend bool) ([]string, bool, bool)) (bool, error) {
	k, err := labKube()
	if err != nil {
		return false, err
	}
	client := k.resource(gvr, platformNamespace)
	var held bool
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		src, err := client.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		suspended, _, _ := unstructured.NestedBool(src.Object, "spec", "suspend")
		recorded := rangeHolders(src)
		holders, suspend, write := change(pruneDeadHolders(recorded), suspended && len(recorded) == 0)
		held = write && len(holders) > 0
		if !write {
			return nil
		}
		annotations := src.GetAnnotations()
		if len(holders) > 0 {
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[rangeHoldAnnotation] = strings.Join(holders, ",")
		} else {
			delete(annotations, rangeHoldAnnotation)
		}
		src.SetAnnotations(annotations)
		if suspend {
			if err := unstructured.SetNestedField(src.Object, true, "spec", "suspend"); err != nil {
				return err
			}
		} else {
			unstructured.RemoveNestedField(src.Object, "spec", "suspend")
		}
		_, err = client.Update(ctx, src, metav1.UpdateOptions{FieldManager: rangeHoldFieldManager})
		return err
	})
	if err != nil {
		return false, fmt.Errorf("holding the version range of %s: %w", describe(gvr, platformNamespace, name), err)
	}
	return held, nil
}
