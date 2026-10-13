package lab

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// harnessLimitsPath is where a claude Harness carried its turn limits, a
// field of the kagent line's own that the line has dropped.
var harnessLimitsPath = []string{crSpec, claudeRuntime, "limits"}

// dropHarnessLimits removes spec.claude.limits from every Harness of the lab
// before the platform chart is applied. A lab's Harnesses rendered with
// limits by an earlier agentlab keep them owned by the chart's field
// manager, and while the installed kagent CRD still declares the field, the
// chart's server-side apply of `claude: {}` without them is refused as a
// null (giantswarm/kagent-upstream#501). A merge patch removing the field
// first leaves the apply nothing to remove, so the old CRD accepts it. A
// Harness without limits, and a fresh lab without the CRD, is a no-op.
//
// Remove once the lab's kagent floor is at or past the release candidate
// that closes giantswarm/kagent-upstream#501 (its CRD without limits).
func dropHarnessLimits(ctx context.Context) error {
	gvr, err := gvrFor(harnessResource)
	if err != nil {
		return nil // the CRD is not served yet: a first install
	}
	harnesses, err := listObjects(ctx, gvr, "", "")
	if err != nil {
		return err
	}
	for _, h := range harnesses {
		if _, found, _ := unstructured.NestedFieldNoCopy(h.Object, harnessLimitsPath...); !found {
			continue
		}
		if err := patchObject(ctx, gvr, h.GetNamespace(), h.GetName(), types.MergePatchType, []byte(`{"spec":{"claude":{"limits":null}}}`)); err != nil {
			return err
		}
		note("removed spec.claude.limits from Harness %s/%s (the kagent line dropped the field)", h.GetNamespace(), h.GetName())
	}
	return nil
}
