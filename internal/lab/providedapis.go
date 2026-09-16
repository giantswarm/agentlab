package lab

import (
	"context"
	_ "embed"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The cluster-level APIs the lab provides itself: CustomResourceDefinitions
// an installation gets from something the kind cluster does not run, applied
// by `up` before the platform installs so the charts that expect the kinds
// render. Each bundle is verbatim upstream, embedded so a boot needs no
// network for it: CustomResourceDefinitions, plus the admission policies
// upstream ships beside them (the Gateway API's safe-upgrades policy).
//
//   - The Gateway API standard channel (v1.5.0): the agent-platform chart's
//     documented cluster-level prerequisite — on a management cluster the
//     shared Envoy Gateway brings it.
//   - The Cilium policy CRDs (cilium.io/v2, from the Cilium release the fleet
//     runs): every Giant Swarm cluster has Cilium, so component charts render
//     their CiliumNetworkPolicy objects unconditionally (gpu-operator-app);
//     kind has no Cilium, and without the API the HelmRelease fails on "no
//     matches for kind". With the CRDs the objects apply and validate against
//     the schema an installation enforces — and enforce nothing here, nothing
//     reads them. A lab-only difference: docs/platform.md "Lab-specific
//     deviations from a real management cluster".
var (
	//go:embed templates/gateway-api-crds.yaml
	gatewayAPICRDs []byte
	//go:embed templates/cilium-crds.yaml
	ciliumCRDs []byte
)

// providedAPI is one bundle: what the boot's step line says it installs, and
// the manifests.
type providedAPI struct {
	what      string
	manifests []byte
}

// providedAPIs is the table `up` applies, in order.
var providedAPIs = []providedAPI{
	{what: "the Gateway API CRDs (standard channel)", manifests: gatewayAPICRDs},
	{what: "the Cilium policy CRDs (cilium.io/v2 — served, enforced by nothing)", manifests: ciliumCRDs},
}

// installProvidedAPIs applies every bundle. An idempotent re-apply, and
// applyManifests returns only once the apiserver serves a bundle's kinds, so
// everything installed afterwards can use them.
func installProvidedAPIs(ctx context.Context) error {
	for _, api := range providedAPIs {
		step("Installing %s", api.what)
		if _, err := applyManifests(ctx, api.manifests); err != nil {
			return err
		}
	}
	return nil
}

// servedKinds is one group/version the bundles define and its kinds — one
// line of what `kubectl api-resources` must list once `up` ran.
type servedKinds struct {
	gv    schema.GroupVersion
	kinds []string
}

func (s servedKinds) String() string {
	return s.gv.String() + ": " + strings.Join(s.kinds, ", ")
}

// providedKinds reads the bundles' CustomResourceDefinitions into their
// served group/versions and kinds, in bundle order; the other documents
// (admission policies) define no kind and are skipped.
func providedKinds() ([]servedKinds, error) {
	var out []servedKinds
	for _, api := range providedAPIs {
		objs, err := decodeManifests(api.manifests)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", api.what, err)
		}
		for _, obj := range objs {
			if obj.GroupVersionKind().GroupKind() != gvkCRD.GroupKind() {
				continue
			}
			for _, gvk := range crdServedGVKs(obj) {
				i := slices.IndexFunc(out, func(s servedKinds) bool { return s.gv == gvk.GroupVersion() })
				if i < 0 {
					out = append(out, servedKinds{gv: gvk.GroupVersion()})
					i = len(out) - 1
				}
				out[i].kinds = append(out[i].kinds, gvk.Kind)
			}
		}
	}
	return out, nil
}

// crdServedGVKs lists the group/version/kind a CustomResourceDefinition
// serves: one per version that is not `served: false`.
func crdServedGVKs(crd *unstructured.Unstructured) []schema.GroupVersionKind {
	group, _, _ := unstructured.NestedString(crd.Object, "spec", "group")
	kind, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "kind")
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	var out []schema.GroupVersionKind
	for _, v := range versions {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if on, found, _ := unstructured.NestedBool(m, "served"); found && !on {
			continue
		}
		if name, _, _ := unstructured.NestedString(m, "name"); name != "" {
			out = append(out, schema.GroupVersionKind{Group: group, Version: name, Kind: kind})
		}
	}
	return out
}

// proveProvidedAPIs asks the apiserver's discovery — what `kubectl
// api-resources` reads — for every kind the bundles define and returns what
// is served, per group/version. A kind the apiserver does not serve fails,
// naming it and the fix.
func proveProvidedAPIs() ([]servedKinds, error) {
	want, err := providedKinds()
	if err != nil {
		return nil, err
	}
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, s := range want {
		list, err := k.clientset.Discovery().ServerResourcesForGroupVersion(s.gv.String())
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("discovering %s: %w", s.gv, err)
		}
		for _, kind := range s.kinds {
			if list == nil || !slices.ContainsFunc(list.APIResources, func(r metav1.APIResource) bool { return r.Kind == kind }) {
				missing = append(missing, s.gv.String()+" "+kind)
			}
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the apiserver does not serve %s — the lab provides these APIs itself; `agentlab up` applies them", strings.Join(missing, ", "))
	}
	return want, nil
}
