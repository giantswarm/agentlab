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

	"github.com/Masterminds/semver/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

const (
	// rangeHoldAnnotation lists the proofs holding a source, comma-separated.
	rangeHoldAnnotation = "agentlab.giantswarm.io/range-hold"
	// rangeHoldFieldManager owns spec.suspend while a hold is on.
	rangeHoldFieldManager = "agentlab-range-hold"
)

// RangeHold is one proof's hold on the platform's version ranges.
type RangeHold struct {
	holder  string
	sources []string
	stop    func()
}

var (
	activeHoldMu sync.Mutex
	// activeHold is the hold of the proof this process runs, for platformUp
	// to take again after an install.
	activeHold *RangeHold
	// rangeHoldHost and processAlive are test seams.
	rangeHoldHost = os.Hostname
	processAlive  = func(pid int) bool {
		err := syscall.Kill(pid, 0)
		return err == nil || errors.Is(err, syscall.EPERM)
	}
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
// and source-controller resolves its range again. Safe to call twice and on
// a nil hold.
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
	return h.drop(ctx)
}

// drop removes the holder from the sources it held.
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
		_, err := updateHold(ctx, *gvr, name, func(holders []string, _ bool) ([]string, bool, bool) {
			holders = slices.DeleteFunc(holders, func(x string) bool { return x == h.holder })
			return holders, len(holders) > 0, true
		})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		note("Resumed the version ranges of %s", strings.Join(h.sources, ", "))
	}
	h.sources = nil
	return errors.Join(errs...)
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
			if s, ok := sig.(syscall.Signal); ok {
				_ = syscall.Kill(os.Getpid(), s)
			}
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
