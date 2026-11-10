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

// TestGitOpsFixtureManifests checks the fixture is one Kustomization of the
// version model-manager reads, suspended in the platform namespace — the
// API is the lab's (providedapis.go), never the fixture's.
func TestGitOpsFixtureManifests(t *testing.T) {
	objs, err := decodeManifests(gitopsFixtureKustomization())
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("%d objects, want the Kustomization alone", len(objs))
	}
	ks := objs[0]
	suspend, _, _ := unstructured.NestedBool(ks.Object, "spec", "suspend")
	if ks.GroupVersionKind() != kustomizationGVR.GroupVersion().WithKind("Kustomization") || ks.GetName() != gitopsFluxFixture ||
		ks.GetNamespace() != platformNamespace || !suspend {
		t.Fatalf("Kustomization %s %s/%s suspend=%v", ks.GroupVersionKind(), ks.GetNamespace(), ks.GetName(), suspend)
	}
}
