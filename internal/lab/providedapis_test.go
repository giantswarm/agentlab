package lab

import (
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
)

// admissionGroup is the API group of the ValidatingAdmissionPolicy (and its
// Binding) the Gateway API install carries beside its CRDs.
const admissionGroup = "admissionregistration.k8s.io"

var (
	ciliumV2       = schema.GroupVersion{Group: "cilium.io", Version: "v2"}
	gatewayAPIV1   = schema.GroupVersion{Group: "gateway.networking.k8s.io", Version: "v1"}
	ciliumPolicies = []string{"CiliumNetworkPolicy", "CiliumClusterwideNetworkPolicy"}
)

func servedFor(t *testing.T, served []servedKinds, gv schema.GroupVersion) servedKinds {
	t.Helper()
	i := slices.IndexFunc(served, func(s servedKinds) bool { return s.gv == gv })
	if i < 0 {
		t.Fatalf("no bundle defines %s; got %v", gv, served)
	}
	return served[i]
}

// TestProvidedAPIsAreCRDBundles: every bundle decodes into
// CustomResourceDefinitions and, at most, the admission policies upstream
// ships beside them; the Cilium bundle defines
// exactly the two policy kinds under cilium.io/v2 — the namespaced
// CiliumNetworkPolicy the component charts render and the cluster-wide one —
// and the Gateway API bundle the kinds the edge is built from.
func TestProvidedAPIsAreCRDBundles(t *testing.T) {
	for _, api := range providedAPIs {
		objs, err := decodeManifests(api.manifests)
		if err != nil {
			t.Fatalf("%s: %v", api.what, err)
		}
		if len(objs) == 0 {
			t.Fatalf("%s: an empty bundle", api.what)
		}
		crds := 0
		for _, obj := range objs {
			switch {
			case obj.GroupVersionKind() == gvkCRD:
				crds++
			case obj.GroupVersionKind().Group == admissionGroup:
			default:
				t.Errorf("%s: %s %s is neither a CustomResourceDefinition nor an admission policy", api.what, obj.GetKind(), obj.GetName())
			}
		}
		if crds == 0 {
			t.Errorf("%s: no CustomResourceDefinition", api.what)
		}
	}

	served, err := providedKinds()
	if err != nil {
		t.Fatal(err)
	}
	cilium := servedFor(t, served, ciliumV2)
	if !slices.Equal(cilium.kinds, ciliumPolicies) {
		t.Errorf("cilium.io/v2 kinds = %v, want %v", cilium.kinds, ciliumPolicies)
	}
	gateway := servedFor(t, served, gatewayAPIV1)
	for _, kind := range []string{"Gateway", "GatewayClass", "HTTPRoute"} {
		if !slices.Contains(gateway.kinds, kind) {
			t.Errorf("gateway.networking.k8s.io/v1 lacks %s: %v", kind, gateway.kinds)
		}
	}

	// The scopes the Cilium kinds carry on an installation.
	objs, err := decodeManifests(ciliumCRDs)
	if err != nil {
		t.Fatal(err)
	}
	scopes := map[string]string{}
	for _, obj := range objs {
		kind, _, _ := unstructured.NestedString(obj.Object, "spec", "names", "kind")
		scope, _, _ := unstructured.NestedString(obj.Object, "spec", "scope")
		scopes[kind] = scope
	}
	if scopes["CiliumNetworkPolicy"] != "Namespaced" || scopes["CiliumClusterwideNetworkPolicy"] != "Cluster" {
		t.Errorf("Cilium policy scopes = %v", scopes)
	}
}

// TestProveProvidedAPIs: the check reads discovery the way `kubectl
// api-resources` does — every kind served passes and names each
// group/version; a group the apiserver does not serve fails naming its kinds.
func TestProveProvidedAPIs(t *testing.T) {
	f := newFakeLab(t)
	disc, ok := f.cs.Discovery().(*fakediscovery.FakeDiscovery)
	if !ok {
		t.Fatalf("the fake clientset's discovery is %T", f.cs.Discovery())
	}
	want, err := providedKinds()
	if err != nil {
		t.Fatal(err)
	}
	var resources []*metav1.APIResourceList
	for _, s := range want {
		if s.gv == ciliumV2 {
			continue
		}
		resources = append(resources, apiResourceList(s))
	}
	disc.Resources = resources

	_, err = proveProvidedAPIs()
	if err == nil {
		t.Fatal("cilium.io/v2 unserved: no error")
	}
	for _, kind := range ciliumPolicies {
		if !strings.Contains(err.Error(), ciliumV2.String()+" "+kind) {
			t.Errorf("error does not name %s %s: %v", ciliumV2, kind, err)
		}
	}
	if strings.Contains(err.Error(), gatewayAPIV1.String()) {
		t.Errorf("error names the served Gateway API: %v", err)
	}

	disc.Resources = append(resources, apiResourceList(servedFor(t, want, ciliumV2)))
	served, err := proveProvidedAPIs()
	if err != nil {
		t.Fatal(err)
	}
	if len(served) != len(want) {
		t.Errorf("served %d group/versions, want %d", len(served), len(want))
	}
	if got := servedFor(t, served, ciliumV2).String(); got != "cilium.io/v2: CiliumNetworkPolicy, CiliumClusterwideNetworkPolicy" {
		t.Errorf("cilium line = %q", got)
	}
}

// apiResourceList is what discovery answers for a group/version that serves
// the given kinds (a status subresource per kind, as the apiserver lists it).
func apiResourceList(s servedKinds) *metav1.APIResourceList {
	list := &metav1.APIResourceList{GroupVersion: s.gv.String()}
	for _, kind := range s.kinds {
		name := strings.ToLower(kind) + "s"
		list.APIResources = append(list.APIResources,
			metav1.APIResource{Name: name, Kind: kind, Namespaced: true},
			metav1.APIResource{Name: name + "/status", Kind: kind, Namespaced: true},
		)
	}
	return list
}
