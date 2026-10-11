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
// The lab's Flux engine runs no kustomize-controller and serves no
// Kustomization API, so the fixture brings the API for its own length — a
// CustomResourceDefinition of kustomize.toolkit.fluxcd.io/v1 that keeps
// every field, labelled agentlab's and removed again afterwards — and writes
// the inventory itself, as kustomize-controller records it after an apply.
// A lab that serves the real API keeps it; the Kustomization is suspended,
// so a controller there leaves the inventory as written.

// kustomizationGVR is Flux's Kustomization, the version model-manager reads.
var kustomizationGVR = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}

// kustomizationCRDName is the Kustomization CustomResourceDefinition's name.
var kustomizationCRDName = kustomizationGVR.Resource + "." + kustomizationGVR.Group

// gitopsFixtureWait bounds the fixture's removal.
const gitopsFixtureWait = time.Minute

// kustomizationCRD is the fixture's Kustomization API: served and stored at
// v1 with a status subresource, every field kept.
func kustomizationCRD() []byte {
	return fmt.Appendf(nil, `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: %s
  labels:
    %s: %s
spec:
  group: %s
  scope: Namespaced
  names:
    kind: Kustomization
    listKind: KustomizationList
    plural: kustomizations
    singular: kustomization
  versions:
    - name: %s
      served: true
      storage: true
      subresources:
        status: {}
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
`, kustomizationCRDName, managedByLabel, managedByAgentlabValue, kustomizationGVR.Group, kustomizationGVR.Version)
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
// obj in its inventory — and the Kustomization API first, where the lab does
// not serve it — and returns its removal: the Kustomization, then the API
// if the fixture brought it.
func applyGitOpsFixture(obj *unstructured.Unstructured) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), crdEstablishTimeout+kubeReadTimeout)
	defer cancel()
	crd, err := getObject(ctx, gvrCRDs, "", kustomizationCRDName)
	ownAPI := apierrors.IsNotFound(err) || (err == nil && crd.GetLabels()[managedByLabel] == managedByAgentlabValue)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	remove := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*gitopsFixtureWait)
		defer cancel()
		if err := deleteObject(ctx, kustomizationGVR, platformNamespace, gitopsFluxFixture, gitopsFixtureWait); err != nil {
			note("cleanup: deleting the Kustomization %s/%s: %v", platformNamespace, gitopsFluxFixture, err)
		}
		if ownAPI {
			if err := deleteObject(ctx, gvrCRDs, "", kustomizationCRDName, gitopsFixtureWait); err != nil {
				note("cleanup: deleting the CustomResourceDefinition %s: %v", kustomizationCRDName, err)
			}
		}
	}
	manifests := gitopsFixtureKustomization()
	if ownAPI {
		manifests = append(append(kustomizationCRD(), "---\n"...), manifests...)
	}
	if _, err := applyManifests(ctx, manifests); err != nil {
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
	api := "the lab's"
	if ownAPI {
		api = "the fixture's own"
	}
	note("Kustomization %s/%s (%s Kustomization API), status.inventory [%s]", platformNamespace, gitopsFluxFixture, api, inventoryEntryOf(obj).ID)
	return remove, nil
}
