package lab

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestDeleteNamespace: a namespace that is not there is success, one that is
// gets deleted and waited for.
func TestDeleteNamespace(t *testing.T) {
	f := newFakeLab(t, &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: observabilityNamespace},
	})
	ctx := context.Background()
	if err := deleteNamespace(ctx, "absent"); err != nil {
		t.Errorf("a missing namespace must be success: %v", err)
	}
	if err := deleteNamespace(ctx, observabilityNamespace); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dyn.Tracker().Get(gvrNamespaces, "", observabilityNamespace); !apierrors.IsNotFound(err) {
		t.Errorf("namespace still stored after the delete: %v", err)
	}
}

// TestMCPPrometheusReleaseExists: the lab's HelmRelease is found in the
// platform namespace and not reported for a cluster without it.
func TestMCPPrometheusReleaseExists(t *testing.T) {
	ctx := context.Background()
	newFakeLab(t)
	if mcpPrometheusReleaseExists(ctx) {
		t.Error("no HelmRelease, yet reported as existing")
	}
	newFakeLab(t, fakeHelmRelease(mcpPrometheusRelease, conditionTrue, "Helm install succeeded"))
	if !mcpPrometheusReleaseExists(ctx) {
		t.Error("the mcp-prometheus HelmRelease is there, yet not found")
	}
}
