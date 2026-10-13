package lab

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// bareHarness is a Harness without limits.
const bareHarness = "bare"

// harnessObject is a Harness of the kagent namespace with the claude spec
// given.
func harnessObject(name string, claude map[string]any) *unstructured.Unstructured {
	h := &unstructured.Unstructured{Object: map[string]any{
		crSpec: map[string]any{claudeRuntime: claude},
	}}
	h.SetGroupVersionKind(gvkHarness)
	h.SetNamespace(kagentNamespace)
	h.SetName(name)
	return h
}

// TestDropHarnessLimits: a Harness carrying spec.claude.limits loses them
// and keeps the rest of its claude spec; one without is left as it is; a
// second run changes nothing.
func TestDropHarnessLimits(t *testing.T) {
	ctx := context.Background()
	f := newFakeLab(t, []runtime.Object{
		harnessObject(claudeHarness, map[string]any{
			"limits": map[string]any{"budgetUSD": "1.00", "maxTurns": int64(10)},
			"model":  "kept",
		}),
		harnessObject(bareHarness, map[string]any{}),
	}...)
	for run := range 2 {
		if err := dropHarnessLimits(ctx); err != nil {
			t.Fatalf("run %d: %v", run+1, err)
		}
		for _, name := range []string{claudeHarness, bareHarness} {
			h := f.stored(t, gvrHarnesses, kagentNamespace, name)
			if limits, found, _ := unstructured.NestedFieldNoCopy(h.Object, harnessLimitsPath...); found {
				t.Errorf("run %d: Harness %s keeps limits %v", run+1, name, limits)
			}
		}
		claude := f.stored(t, gvrHarnesses, kagentNamespace, claudeHarness)
		if model, _, _ := unstructured.NestedString(claude.Object, crSpec, claudeRuntime, "model"); model != "kept" {
			t.Errorf("run %d: spec.claude.model = %q, want the rest of the claude spec kept", run+1, model)
		}
	}
}
