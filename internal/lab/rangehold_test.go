package lab

import (
	"context"
	"os"
	"strconv"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// rangeSource is an OCIRepository in the platform namespace: ref its
// spec.ref.semver, resolved whether source-controller has an artifact for the
// current spec, suspend and holders as they are stored.
func rangeSource(name, ref string, resolved, suspend bool, holders string) *unstructured.Unstructured {
	src := &unstructured.Unstructured{Object: map[string]any{}}
	src.SetGroupVersionKind(fluxOCIRepositoryGVK)
	src.SetName(name)
	src.SetNamespace(platformNamespace)
	src.SetGeneration(2)
	_ = unstructured.SetNestedField(src.Object, ref, crSpec, "ref", "semver")
	if suspend {
		_ = unstructured.SetNestedField(src.Object, true, crSpec, "suspend")
	}
	if holders != "" {
		src.SetAnnotations(map[string]string{rangeHoldAnnotation: holders})
	}
	if resolved {
		_ = unstructured.SetNestedField(src.Object, int64(2), "status", "observedGeneration")
		_ = unstructured.SetNestedField(src.Object, "5.42.0@sha256:abc", "status", "artifact", "revision")
	}
	return src
}

// The fixtures' range, and a proof on another host.
const (
	testMajorRange  = ">=5.0.0 <6.0.0"
	otherHostHolder = "otherhost/2"
)

// stubRangeHoldHost names this host for the holders and judges every other
// process on it alive unless dead lists it.
func stubRangeHoldHost(t *testing.T, dead ...int) {
	t.Helper()
	prevHost, prevAlive := rangeHoldHost, processAlive
	rangeHoldHost = func() (string, error) { return "testhost", nil }
	processAlive = func(pid int) bool {
		for _, d := range dead {
			if pid == d {
				return false
			}
		}
		return true
	}
	t.Cleanup(func() { rangeHoldHost, processAlive = prevHost, prevAlive })
}

// heldState reads one source's suspend and holder annotation back.
func heldState(t *testing.T, f *fakeLab, name string) (bool, string) {
	t.Helper()
	src := f.stored(t, fluxOCIRepositoryGVR, platformNamespace, name)
	suspended, _, _ := unstructured.NestedBool(src.Object, crSpec, "suspend")
	return suspended, src.GetAnnotations()[rangeHoldAnnotation]
}

// TestHoldComponentRanges: a proof suspends exactly the sources that track a
// range and have resolved it — never an exact version, someone's own suspend
// or a source still resolving — and its release resumes them.
func TestHoldComponentRanges(t *testing.T) {
	stubRangeHoldHost(t)
	f := newFakeLab(t,
		rangeSource(componentMuster, testMajorRange, true, false, ""),
		rangeSource(config.ConnectivityChartName, "1.2.3", true, false, ""),
		rangeSource(componentKagent, ">=0.9.0", true, true, ""),
		rangeSource(componentBackstage, ">=1.0.0", false, false, ""),
	)
	ctx := context.Background()
	me := "testhost/" + strconv.Itoa(os.Getpid())

	h, err := HoldComponentRanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.sources) != 1 || h.sources[0] != componentMuster {
		t.Errorf("held %v, want [muster]", h.sources)
	}
	if suspended, holders := heldState(t, f, componentMuster); !suspended || holders != me {
		t.Errorf("muster: suspend=%v holders=%q, want suspended and held by %s", suspended, holders, me)
	}
	for _, name := range []string{config.ConnectivityChartName, componentBackstage} {
		if suspended, holders := heldState(t, f, name); suspended || holders != "" {
			t.Errorf("%s: suspend=%v holders=%q, want untouched", name, suspended, holders)
		}
	}
	if suspended, holders := heldState(t, f, componentKagent); !suspended || holders != "" {
		t.Errorf("kagent (its own suspend): suspend=%v holders=%q, want left as it was", suspended, holders)
	}

	if err := h.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if suspended, holders := heldState(t, f, componentMuster); suspended || holders != "" {
		t.Errorf("muster after the release: suspend=%v holders=%q, want resumed", suspended, holders)
	}
	if suspended, _ := heldState(t, f, componentKagent); !suspended {
		t.Error("the release resumed a source the hold never suspended")
	}
	if err := h.Release(ctx); err != nil {
		t.Errorf("a second release: %v", err)
	}
}

// TestRangeHoldSharedAndPruned: the last proof out resumes a source two
// proofs hold, and a holder whose process is gone is pruned.
func TestRangeHoldSharedAndPruned(t *testing.T) {
	stubRangeHoldHost(t, 4242)
	f := newFakeLab(t, rangeSource(componentMuster, testMajorRange, true, true, "testhost/4242"))
	ctx := context.Background()

	a := &RangeHold{holder: "testhost/1"}
	b := &RangeHold{holder: otherHostHolder}
	for _, h := range []*RangeHold{a, b} {
		if err := h.take(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, holders := heldState(t, f, componentMuster); holders != "testhost/1,"+otherHostHolder {
		t.Errorf("holders %q, want the dead one pruned and both proofs listed", holders)
	}
	if err := a.drop(ctx); err != nil {
		t.Fatal(err)
	}
	if suspended, holders := heldState(t, f, componentMuster); !suspended || holders != otherHostHolder {
		t.Errorf("after the first proof: suspend=%v holders=%q, want still held by the second", suspended, holders)
	}
	if err := b.drop(ctx); err != nil {
		t.Fatal(err)
	}
	if suspended, holders := heldState(t, f, componentMuster); suspended || holders != "" {
		t.Errorf("after the last proof: suspend=%v holders=%q, want resumed", suspended, holders)
	}
}

// TestLiftRangeHolds: an install lifts every proof's hold, so what it
// installs applies, and the proof of this process holds again afterwards.
func TestLiftRangeHolds(t *testing.T) {
	stubRangeHoldHost(t)
	f := newFakeLab(t,
		rangeSource(componentMuster, testMajorRange, true, false, ""),
		rangeSource(componentKagent, ">=0.9.0", true, true, ""),
	)
	ctx := context.Background()
	h, err := HoldComponentRanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Release(ctx) })
	other := &RangeHold{holder: otherHostHolder}
	if err := other.take(ctx); err != nil {
		t.Fatal(err)
	}

	held, err := liftRangeHolds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if held != h {
		t.Errorf("lift returned %v, want this process's hold", held)
	}
	if suspended, holders := heldState(t, f, componentMuster); suspended || holders != "" {
		t.Errorf("muster during the install: suspend=%v holders=%q, want every hold lifted", suspended, holders)
	}
	if suspended, _ := heldState(t, f, componentKagent); !suspended {
		t.Error("the lift resumed someone's own suspend")
	}

	if err := resumeRangeHold(ctx, held); err != nil {
		t.Fatal(err)
	}
	if suspended, holders := heldState(t, f, componentMuster); !suspended || holders != h.holder {
		t.Errorf("muster after the install: suspend=%v holders=%q, want held by this proof again", suspended, holders)
	}
}

// TestIsVersionRange: an exact version never moves; anything else may.
func TestIsVersionRange(t *testing.T) {
	for ref, want := range map[string]bool{
		"":             false,
		"1.2.3":        false,
		"v1.2.3":       false,
		"1.2.3-rc.1":   false,
		testMajorRange: true,
		"5.x":          true,
		"~1.2":         true,
		"*":            true,
	} {
		if got := isVersionRange(ref); got != want {
			t.Errorf("isVersionRange(%q) = %v, want %v", ref, got, want)
		}
	}
}
