package lab

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

// The fixtures' artifacts: the one a hold keeps (rangeSource's) and the
// release published while the proof held the range.
const (
	heldRevision  = "5.42.0@sha256:abc"
	movedRevision = "5.43.0@sha256:def"
	// readyUnknown is Ready while helm-controller runs the upgrade.
	readyUnknown = "Unknown"
	// conditionMessageKey is a status condition's message field.
	conditionMessageKey = "message"
)

// chartRelease is a HelmRelease on the platform namespace's OCIRepository
// source, deployed at the artifact revision with Ready as given.
func chartRelease(name, source, revision, ready, message string) *unstructured.Unstructured {
	hr := &unstructured.Unstructured{Object: map[string]any{}}
	hr.SetGroupVersionKind(fluxHelmReleaseGVK)
	hr.SetName(name)
	hr.SetNamespace(platformNamespace)
	_ = unstructured.SetNestedStringMap(hr.Object, map[string]string{"kind": kindOCIRepository, nameKey: source, "namespace": platformNamespace}, crSpec, "chartRef")
	setReleaseAt(hr, revision, ready, message)
	return hr
}

// setReleaseAt writes the status helm-controller leaves after deploying the
// artifact revision: the attempted chart and its digest, the newest snapshot,
// Ready.
func setReleaseAt(hr *unstructured.Unstructured, revision, ready, message string) {
	version, digest, _ := strings.Cut(revision, "@")
	chart := version + "+" + strings.TrimPrefix(digest, "sha256:")
	_ = unstructured.SetNestedField(hr.Object, chart, crStatus, "lastAttemptedRevision")
	_ = unstructured.SetNestedField(hr.Object, digest, crStatus, "lastAttemptedRevisionDigest")
	_ = unstructured.SetNestedSlice(hr.Object, []any{map[string]any{"chartVersion": chart, crStatus: helmReleaseDeployed}}, crStatus, "history")
	_ = unstructured.SetNestedSlice(hr.Object, []any{map[string]any{fieldTypeKey: conditionReady, crStatus: ready, conditionMessageKey: message}}, crStatus, "conditions")
}

// moveOnResume plays source-controller: the update that resumes the source
// finds the release published while it was held.
func moveOnResume(f *fakeLab, revision string) {
	f.dyn.PrependReactor("update", "ocirepositories", func(action clienttesting.Action) (bool, runtime.Object, error) {
		src := action.(clienttesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		if suspended, _, _ := unstructured.NestedBool(src.Object, crSpec, "suspend"); !suspended {
			_ = unstructured.SetNestedField(src.Object, revision, crStatus, "artifact", "revision")
		}
		return false, nil, nil
	})
}

// holdMuster takes a proof's hold on the muster fixture source.
func holdMuster(t *testing.T) *RangeHold {
	t.Helper()
	h, err := HoldComponentRanges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.sources) != 1 {
		t.Fatalf("held %v, want [muster]", h.sources)
	}
	return h
}

// TestReleaseWaitsForTheMovedRange: a release published while the proof held
// its range rolls on the resume, and the proof's release returns only once
// the HelmRelease on that source is Ready at the new chart — a release on
// another source that is not Ready does not hold it up.
func TestReleaseWaitsForTheMovedRange(t *testing.T) {
	stubRangeHoldHost(t)
	f := newFakeLab(t,
		rangeSource(componentMuster, testMajorRange, true, false, ""),
		chartRelease(componentMuster, componentMuster, heldRevision, conditionTrue, ""),
		chartRelease(componentBackstage, componentBackstage, heldRevision, condFalseStatus, "upgrading"),
	)
	moveOnResume(f, movedRevision)
	// helm-controller: the second look finds the upgrade done.
	var lists atomic.Int32
	f.dyn.PrependReactor("list", "helmreleases", func(clienttesting.Action) (bool, runtime.Object, error) {
		if lists.Add(1) == 2 {
			hr := f.stored(t, fluxHelmReleaseGVR, platformNamespace, componentMuster)
			setReleaseAt(hr, movedRevision, conditionTrue, "")
			if err := f.dyn.Tracker().Update(fluxHelmReleaseGVR, hr, platformNamespace); err != nil {
				t.Error(err)
			}
		}
		return false, nil, nil
	})
	h := holdMuster(t)

	if err := h.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := lists.Load(); n < 2 {
		t.Errorf("the release returned after %d look(s) at the HelmReleases, want it to wait for the upgrade", n)
	}
	if !releaseAt(f.stored(t, fluxHelmReleaseGVR, platformNamespace, componentMuster), movedRevision) {
		t.Error("the release returned before muster ran the moved chart")
	}
}

// TestReleaseUnmovedRangeReturnsAtOnce: a range that resolved nothing new
// rolls nothing, and the release does not look at the HelmReleases.
func TestReleaseUnmovedRangeReturnsAtOnce(t *testing.T) {
	stubRangeHoldHost(t)
	f := newFakeLab(t,
		rangeSource(componentMuster, testMajorRange, true, false, ""),
		chartRelease(componentMuster, componentMuster, heldRevision, condFalseStatus, "not this proof's business"),
	)
	var lists atomic.Int32
	f.dyn.PrependReactor("list", "helmreleases", func(clienttesting.Action) (bool, runtime.Object, error) {
		lists.Add(1)
		return false, nil, nil
	})
	h := holdMuster(t)

	start := time.Now()
	if err := h.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lists.Load() != 0 || time.Since(start) > pollInterval {
		t.Errorf("an unmoved range: %d look(s) at the HelmReleases in %s, want none and no wait", lists.Load(), time.Since(start))
	}
}

// TestReleaseMovedRangeFailures: a HelmRelease helm-controller gave up on
// ends the wait with its message; one still rolling at the deadline is named.
func TestReleaseMovedRangeFailures(t *testing.T) {
	for _, tc := range []struct {
		name, ready, message, want string
	}{
		{"retries exhausted", condFalseStatus, "upgrade retries exhausted", "failed to roll to " + movedRevision + " after the version ranges resumed: upgrade retries exhausted"},
		{"deadline", readyUnknown, "Running 'upgrade' action", "not Ready at " + movedRevision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubRangeHoldHost(t)
			prev := rangeSettleTimeout
			rangeSettleTimeout = 100 * time.Millisecond
			t.Cleanup(func() { rangeSettleTimeout = prev })
			f := newFakeLab(t,
				rangeSource(componentMuster, testMajorRange, true, false, ""),
				chartRelease(componentMuster, componentMuster, heldRevision, tc.ready, tc.message),
			)
			moveOnResume(f, movedRevision)
			h := holdMuster(t)

			err := h.Release(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), componentMuster) {
				t.Errorf("Release() = %v, want an error naming muster and %q", err, tc.want)
			}
			if suspended, holders := heldState(t, f, componentMuster); suspended || holders != "" {
				t.Errorf("muster: suspend=%v holders=%q, want resumed whatever the wait found", suspended, holders)
			}
		})
	}
}

// TestReleaseAt: a release runs the artifact only once helm-controller
// attempted that chart, Helm deployed it and Ready is True.
func TestReleaseAt(t *testing.T) {
	at := chartRelease("r", "s", movedRevision, conditionTrue, "")
	if !releaseAt(at, movedRevision) {
		t.Error("a release deployed at the revision and Ready reads not at it")
	}
	if releaseAt(at, heldRevision) {
		t.Error("a release reads at a revision it does not run")
	}
	rolling := chartRelease("r", "s", movedRevision, readyUnknown, "Running 'upgrade' action")
	if releaseAt(rolling, movedRevision) {
		t.Error("a release still upgrading reads at the revision")
	}
	stale := chartRelease("r", "s", heldRevision, conditionTrue, "")
	_ = unstructured.SetNestedField(stale.Object, "sha256:def", crStatus, "lastAttemptedRevisionDigest")
	_ = unstructured.SetNestedField(stale.Object, "5.43.0+def", crStatus, "lastAttemptedRevision")
	if releaseAt(stale, movedRevision) {
		t.Error("a release whose newest snapshot is the old chart reads at the new revision")
	}
}
