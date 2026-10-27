package lab

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
)

// TestSubstrateDown: the agents switched off in place leave the lab with
// what a lab created without them has — no lab-applied CSIDriverConfig, no
// substrate HelmRelease, no ate-system or podcertificate-controller
// namespace — and the platform's own releases untouched.
func TestSubstrateDown(t *testing.T) {
	f := newFakeLab(t,
		namespaceObject(substrateNamespace),
		namespaceObject(podCertControllerNamespace),
		fluxObject(fluxHelmReleaseGVK, platformNamespace, substrateRelease, platformRelease),
		fluxObject(fluxHelmReleaseGVK, platformNamespace, componentMuster, platformRelease),
		csiDriverConfigObject(managedByAgentlabValue),
	)
	f.mapper.Add(gvkCSIDriverConfig, meta.RESTScopeRoot)
	ctx := context.Background()
	if err := substrateDown(ctx); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{substrateNamespace, podCertControllerNamespace} {
		if exists, _ := objectExists(ctx, gvrNamespaces, "", ns); exists {
			t.Errorf("namespace %s must be gone", ns)
		}
	}
	if exists, _ := objectExists(ctx, fluxHelmReleaseGVR, platformNamespace, substrateRelease); exists {
		t.Errorf("HelmRelease %s must be gone", substrateRelease)
	}
	if clusterObjectExists(ctx, csiDriverConfigResource, workspacesCSIDriver) {
		t.Errorf("the lab's CSIDriverConfig %s must be gone", workspacesCSIDriver)
	}
	if exists, _ := objectExists(ctx, fluxHelmReleaseGVR, platformNamespace, componentMuster); !exists {
		t.Error("the platform's own HelmReleases must be left alone")
	}
	// A second run finds nothing to do.
	if err := substrateDown(ctx); err != nil {
		t.Fatal(err)
	}
}

// A CSIDriverConfig while Substrate's control plane is not on the cluster
// registers nothing: status words it as a leftover, never as registered.
func TestWorkspacesStatusWithoutSubstrate(t *testing.T) {
	status := (&WorkspacesStatus{Controller: conditionReady, CSIDriverConfig: registrantChart, NoSubstrate: true}).String()
	if strings.Contains(status, "registered with Substrate") {
		t.Errorf("status %q says registered with Substrate while Substrate is off", status)
	}
	if !strings.Contains(status, "left over, Agent Substrate not running") {
		t.Errorf("status %q does not name the leftover CSIDriverConfig", status)
	}

	newFakeLab(t)
	if substrateControlPlaneOn(context.Background()) {
		t.Error("substrateControlPlaneOn on a cluster without ate-api-server")
	}
}
