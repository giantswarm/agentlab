package lab

import (
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestInventoryStatus pins the inventory entry to kustomize-controller's
// shape, the one model-manager matches: namespace_name_group_kind and the
// version.
func TestInventoryStatus(t *testing.T) {
	mc := &unstructured.Unstructured{}
	mc.SetAPIVersion("kagent.dev/v1alpha2")
	mc.SetKind("ModelConfig")
	mc.SetNamespace(kagentNamespace)
	mc.SetName("qwen2-5-0-5b")
	raw, err := inventoryStatus(mc)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Status struct {
			Inventory struct {
				Entries []inventoryEntry `json:"entries"`
			} `json:"inventory"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := inventoryEntry{ID: "kagent_qwen2-5-0-5b_kagent.dev_ModelConfig", Version: "v1alpha2"}
	if e := got.Status.Inventory.Entries; len(e) != 1 || e[0] != want {
		t.Fatalf("status.inventory.entries = %+v, want [%+v]", e, want)
	}
}

// TestGitOpsFixtureManifests checks the fixture's API serves the version
// model-manager reads, with the status subresource the inventory is written
// through, and that the Kustomization is suspended in the platform namespace.
func TestGitOpsFixtureManifests(t *testing.T) {
	objs, err := decodeManifests(append(append(kustomizationCRD(), "---\n"...), gitopsFixtureKustomization()...))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("%d objects, want the CRD and the Kustomization", len(objs))
	}
	crd, ks := objs[0], objs[1]
	if crd.GetName() != kustomizationCRDName || crd.GetLabels()[managedByLabel] != managedByAgentlabValue {
		t.Fatalf("CRD %s labelled %v", crd.GetName(), crd.GetLabels())
	}
	if gvks := crdServedGVKs(crd); len(gvks) != 1 || gvks[0] != kustomizationGVR.GroupVersion().WithKind("Kustomization") {
		t.Fatalf("CRD serves %v", gvks)
	}
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if _, ok, _ := unstructured.NestedMap(versions[0].(map[string]any), "subresources", "status"); !ok {
		t.Fatal("CRD has no status subresource")
	}
	suspend, _, _ := unstructured.NestedBool(ks.Object, "spec", "suspend")
	if ks.GroupVersionKind() != kustomizationGVR.GroupVersion().WithKind("Kustomization") || ks.GetName() != gitopsFluxFixture ||
		ks.GetNamespace() != platformNamespace || !suspend {
		t.Fatalf("Kustomization %s %s/%s suspend=%v", ks.GroupVersionKind(), ks.GetNamespace(), ks.GetName(), suspend)
	}
}
