package lab

import (
	"context"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fluxObject is a HelmRelease or OCIRepository as the cluster holds it,
// owned by the release named in its Helm annotations ("" for none).
func fluxObject(gvk schema.GroupVersionKind, ns, name, ownerRelease string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(ns)
	obj.SetName(name)
	if ownerRelease != "" {
		obj.SetAnnotations(map[string]string{
			helmReleaseNameAnnotation:      ownerRelease,
			helmReleaseNamespaceAnnotation: platformNamespace,
		})
		obj.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/instance": ownerRelease})
	}
	return obj
}

// The deployed revision's Flux objects are read off its manifest and hooks,
// an object without a namespace in the release's; other kinds are ignored.
func TestRenderedFluxObjects(t *testing.T) {
	manifests := `
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: backstage
  namespace: agent-platform
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: backstage
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: backstage
  namespace: agent-platform
---
`
	got, err := renderedFluxObjects(manifests, platformNamespace)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"OCIRepository agent-platform/backstage": true,
		"HelmRelease agent-platform/backstage":   true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rendered = %v, want %v", got, want)
	}
}

// The wait for an orphan's deletion covers the action helm-controller runs
// and the uninstall after it, each by the HelmRelease's own timeout.
func TestOrphanDeleteWait(t *testing.T) {
	for _, c := range []struct {
		name, timeout, uninstall string
		want                     time.Duration
	}{
		{"defaults", "", "", 11 * time.Minute},
		{"spec.timeout", "10m", "", 21 * time.Minute},
		{"uninstall timeout", "10m", "2m", 13 * time.Minute},
		{"unparsable", "soon", "", 11 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			obj := fluxObject(fluxHelmReleaseGVK, platformNamespace, "orphan", platformRelease)
			obj.Object["spec"] = map[string]any{
				"timeout":   c.timeout,
				"uninstall": map[string]any{"timeout": c.uninstall},
			}
			if got := orphanDeleteWait(obj); got != c.want {
				t.Fatalf("orphanDeleteWait = %s, want %s", got, c.want)
			}
		})
	}
}

// A component a failed upgrade added — its HelmRelease and OCIRepository
// owned by the release, rendered by no deployed revision — is removed and
// named; what the revision renders stays, and so do the objects of other
// owners (the lab's own mcp-prometheus, agent-manager's agent sources).
func TestRemoveFluxOrphans(t *testing.T) {
	f := newFakeLab(t, []runtime.Object{
		fluxObject(fluxHelmReleaseGVK, platformNamespace, "backstage", platformRelease),
		fluxObject(fluxOCIRepositoryGVK, platformNamespace, "backstage", platformRelease),
		fluxObject(fluxHelmReleaseGVK, platformNamespace, "orphan", platformRelease),
		fluxObject(fluxOCIRepositoryGVK, platformNamespace, "orphan", platformRelease),
		fluxObject(fluxHelmReleaseGVK, platformNamespace, mcpPrometheusRelease, ""),
		fluxObject(fluxOCIRepositoryGVK, kagentNamespace, agentChartOCIRepository, ""),
	}...)
	rendered := map[string]bool{
		"HelmRelease agent-platform/backstage":   true,
		"OCIRepository agent-platform/backstage": true,
	}
	ctx := context.Background()
	removed, err := removeFluxOrphans(ctx, rendered, platformRelease, platformNamespace)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"HelmRelease agent-platform/orphan", "OCIRepository agent-platform/orphan"}
	if !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed = %v, want %v", removed, want)
	}
	for _, c := range []struct {
		gvr       schema.GroupVersionResource
		ns, name  string
		wantThere bool
	}{
		{fluxHelmReleaseGVR, platformNamespace, "backstage", true},
		{fluxOCIRepositoryGVR, platformNamespace, "backstage", true},
		{fluxHelmReleaseGVR, platformNamespace, mcpPrometheusRelease, true},
		{fluxOCIRepositoryGVR, kagentNamespace, agentChartOCIRepository, true},
		{fluxHelmReleaseGVR, platformNamespace, "orphan", false},
		{fluxOCIRepositoryGVR, platformNamespace, "orphan", false},
	} {
		_, err := f.dyn.Resource(c.gvr).Namespace(c.ns).Get(ctx, c.name, metav1.GetOptions{})
		if there := err == nil; there != c.wantThere {
			t.Errorf("%s %s/%s there = %v, want %v (%v)", c.gvr.Resource, c.ns, c.name, there, c.wantThere, err)
		}
	}

	// A second run over the reconciled set removes nothing.
	removed, err = removeFluxOrphans(ctx, rendered, platformRelease, platformNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("second run removed %v, want nothing", removed)
	}
}
