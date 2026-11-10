package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// The gitops_owned fixture (docs/models.md "Dry runs, GitOps-owned objects
// and commit mode"): the Flux Kustomization agentlab-gitops-fixture in the
// platform namespace whose status.inventory lists the object the proof
// labels with its provenance. model-manager holds a Kustomization's labels
// to its inventory — an object whose Kustomization is gone, or no longer
// lists it, is applied by nothing and written live — so the labels alone
// make nothing GitOps-owned.
//
// The lab serves Flux's Kustomization API (its CRD is one of the provided
// APIs, providedapis.go) and runs no kustomize-controller, so the fixture
// writes the inventory itself, as kustomize-controller records it after an
// apply. The Kustomization is suspended, so a controller that does run
// leaves the inventory as written.

// kustomizationGVR is Flux's Kustomization, the version model-manager reads.
var kustomizationGVR = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}

// kustomizationCRDName is the Kustomization CustomResourceDefinition's name.
var kustomizationCRDName = kustomizationGVR.Resource + "." + kustomizationGVR.Group

// gitopsFixtureWait bounds the fixture's removal.
const gitopsFixtureWait = time.Minute

// requireKustomizationAPI fails, naming the fix, when the apiserver serves
// no Kustomization API: a lab whose platform was installed before the lab
// provided it.
func requireKustomizationAPI(ctx context.Context) error {
	if _, err := getObject(ctx, gvrCRDs, "", kustomizationCRDName); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("the apiserver serves no Flux Kustomization API (%s) — the lab provides it; run `agentlab platform` to apply it", kustomizationCRDName)
		}
		return err
	}
	return nil
}

// gitopsFixtureKustomization is the fixture's Kustomization: valid against
// Flux's own schema, suspended, nothing pruned.
func gitopsFixtureKustomization() []byte {
	return fmt.Appendf(nil, `apiVersion: %s
kind: Kustomization
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
spec:
  interval: 1h
  path: ./%s
  prune: false
  suspend: true
  sourceRef:
    kind: GitRepository
    name: %s
`, kustomizationGVR.GroupVersion(), gitopsFluxFixture, platformNamespace, managedByLabel, managedByAgentlabValue, gitopsPath, gitopsFluxFixture)
}

// inventoryEntry is one entry of a Kustomization's status.inventory as
// kustomize-controller records it: id is namespace_name_group_kind (the group
// empty for the core API), v the version applied.
type inventoryEntry struct {
	ID      string `json:"id"`
	Version string `json:"v"`
}

func inventoryEntryOf(obj *unstructured.Unstructured) inventoryEntry {
	gvk := obj.GroupVersionKind()
	return inventoryEntry{
		ID:      strings.Join([]string{obj.GetNamespace(), obj.GetName(), gvk.Group, gvk.Kind}, "_"),
		Version: gvk.Version,
	}
}

// inventoryStatus is the status patch that records objs as the
// Kustomization's inventory.
func inventoryStatus(objs ...*unstructured.Unstructured) ([]byte, error) {
	entries := make([]inventoryEntry, 0, len(objs))
	for _, obj := range objs {
		entries = append(entries, inventoryEntryOf(obj))
	}
	return json.Marshal(map[string]any{crStatus: map[string]any{"inventory": map[string]any{"entries": entries}}})
}

// applyGitOpsFixture applies the Kustomization agentlab-gitops-fixture with
// obj in its inventory and returns its removal.
func applyGitOpsFixture(obj *unstructured.Unstructured) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*kubeReadTimeout)
	defer cancel()
	if err := requireKustomizationAPI(ctx); err != nil {
		return nil, err
	}
	remove := func() {
		ctx, cancel := context.WithTimeout(context.Background(), gitopsFixtureWait)
		defer cancel()
		if err := deleteObject(ctx, kustomizationGVR, platformNamespace, gitopsFluxFixture, gitopsFixtureWait); err != nil {
			note("cleanup: deleting the Kustomization %s/%s: %v", platformNamespace, gitopsFluxFixture, err)
		}
	}
	if _, err := applyManifests(ctx, gitopsFixtureKustomization()); err != nil {
		remove()
		return nil, err
	}
	status, err := inventoryStatus(obj)
	if err != nil {
		remove()
		return nil, err
	}
	k, err := labKube()
	if err != nil {
		remove()
		return nil, err
	}
	if _, err := k.resource(kustomizationGVR, platformNamespace).Patch(ctx, gitopsFluxFixture, types.MergePatchType, status,
		metav1.PatchOptions{FieldManager: applyFieldManager}, crStatus); err != nil {
		remove()
		return nil, fmt.Errorf("recording the inventory of %s: %w", describe(kustomizationGVR, platformNamespace, gitopsFluxFixture), err)
	}
	note("Kustomization %s/%s, status.inventory [%s]", platformNamespace, gitopsFluxFixture, inventoryEntryOf(obj).ID)
	return remove, nil
}
