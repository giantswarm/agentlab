package lab

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// aggregated is the value of Kyverno's aggregation labels.
const aggregated = "true"

// The fleet's exclude list, as management-cluster-bases writes it on every
// rule.
var fleetExemptNamespaces = []string{"flux-giantswarm", "giantswarm", "monitoring"}

// TestFluxMultiTenancyBundleIsTheFleets: the embedded bundle is the fleet's
// ClusterPolicy — Enforce, the nine rules by name, every rule excluding the
// fleet's three namespaces, the HelmRelease rules the lab exercises among
// them — next to the aggregated ClusterRole its rules read with.
func TestFluxMultiTenancyBundleIsTheFleets(t *testing.T) {
	objs, err := decodeManifests(fluxMultiTenancy)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("bundle has %d documents, want the ClusterPolicy and the ClusterRole", len(objs))
	}
	policy, role := objs[0], objs[1]
	if policy.GetKind() != "ClusterPolicy" || policy.GetName() != fleetPolicyName {
		t.Fatalf("first document is %s %s, want ClusterPolicy %s", policy.GetKind(), policy.GetName(), fleetPolicyName)
	}
	if action, _, _ := unstructured.NestedString(policy.Object, "spec", "validationFailureAction"); action != "Enforce" {
		t.Errorf("validationFailureAction = %q, want Enforce", action)
	}
	wantRules := []string{
		"serviceAccountNameMustBeSet", "kustomizationSourceRefNamespaceIsSafe", "helmReleaseSourceRefNamespaceIsSafe",
		"targetNamespaceMustBeLocal", "storageNamespaceMustBeLocal", "alertEventSourcesMustBeLocal",
		"receiverResourcesMustBeLocal", "eventsObjectMustBeLocal", "imagePolicyRepositoryMustBeLocal",
	}
	rules, _, _ := unstructured.NestedSlice(policy.Object, "spec", "rules")
	var names []string
	for _, r := range rules {
		rule := r.(map[string]any)
		name, _, _ := unstructured.NestedString(rule, "name")
		names = append(names, name)
		namespaces, _, _ := unstructured.NestedStringSlice(rule, "exclude", "resources", "namespaces")
		if !slices.Equal(namespaces, fleetExemptNamespaces) {
			t.Errorf("rule %s excludes %v, want the fleet's %v", name, namespaces, fleetExemptNamespaces)
		}
	}
	if !slices.Equal(names, wantRules) {
		t.Errorf("rules = %v, want %v", names, wantRules)
	}
	if role.GetKind() != "ClusterRole" || role.GetName() != "kyverno:gs-mcb:flux-multi-tenancy" {
		t.Errorf("second document is %s %s, want the fleet's aggregated ClusterRole", role.GetKind(), role.GetName())
	}
	if v := role.GetLabels()["rbac.kyverno.io/aggregate-to-admission-controller"]; v != aggregated {
		t.Errorf("the ClusterRole does not aggregate to the admission controller (label %q)", v)
	}
	// The matrix's denials name rules of this policy with their messages.
	for _, c := range fleetAdmissionCases() {
		if c.admitted() {
			continue
		}
		if !slices.Contains(names, c.deniedBy) {
			t.Errorf("case %q expects rule %s, which the policy has not", c.name, c.deniedBy)
		}
		if !strings.Contains(string(fluxMultiTenancy), c.message) {
			t.Errorf("case %q expects message %q, which the policy has not", c.name, c.message)
		}
	}
}

// TestLabFleetPolicyExemptsThePlatformNamespace: the lab's one addition — the
// platform namespace on every rule's exclude list, after the fleet's three,
// once — and nothing else touched (the embedded bytes stay the fleet's).
func TestLabFleetPolicyExemptsThePlatformNamespace(t *testing.T) {
	objs, err := labFleetPolicy(platformNamespace, platformNamespace)
	if err != nil {
		t.Fatal(err)
	}
	rules, _, _ := unstructured.NestedSlice(objs[0].Object, "spec", "rules")
	if len(rules) != 9 {
		t.Fatalf("%d rules, want 9", len(rules))
	}
	want := append(slices.Clone(fleetExemptNamespaces), platformNamespace)
	for _, r := range rules {
		rule := r.(map[string]any)
		name, _, _ := unstructured.NestedString(rule, "name")
		namespaces, _, _ := unstructured.NestedStringSlice(rule, "exclude", "resources", "namespaces")
		if !slices.Equal(namespaces, want) {
			t.Errorf("rule %s excludes %v, want %v", name, namespaces, want)
		}
	}
	// The embedded bundle stays the fleet's: decoding it again yields the
	// fleet's three namespaces (TestFluxMultiTenancyBundleIsTheFleets), and
	// the exemption exists only in what labFleetPolicy returns.
	fresh, err := decodeManifests(fluxMultiTenancy)
	if err != nil {
		t.Fatal(err)
	}
	freshRules, _, _ := unstructured.NestedSlice(fresh[0].Object, "spec", "rules")
	namespaces, _, _ := unstructured.NestedStringSlice(freshRules[0].(map[string]any), "exclude", "resources", "namespaces")
	if slices.Contains(namespaces, platformNamespace) {
		t.Errorf("the embedded bundle carries %s itself: the exemption is added in code, the file stays the fleet's", platformNamespace)
	}
}

// TestAdmissionCasesShape: every case is a HelmRelease in the org namespace
// that only the fleet's tenancy rules can tell apart — suspended, chart in
// its own namespace, the shape on top — and the denied cases cover the two
// rules cluster-manager 0.5.2 tripped.
func TestAdmissionCasesShape(t *testing.T) {
	cases := fleetAdmissionCases()
	// The first case gates the boot on the webhook: it must be a denial.
	if cases[0].admitted() {
		t.Fatal("the first case gates the boot on the webhook and must be a denial")
	}
	var denied, admitted int
	for _, c := range cases {
		hr := c.release()
		if hr.GetNamespace() != orgNamespace || hr.GetKind() != "HelmRelease" || hr.GetAPIVersion() != "helm.toolkit.fluxcd.io/v2" {
			t.Errorf("%s: %s/%s %s, want a helm.toolkit.fluxcd.io/v2 HelmRelease in %s", c.name, hr.GetAPIVersion(), hr.GetKind(), hr.GetNamespace(), orgNamespace)
		}
		if suspend, _, _ := unstructured.NestedBool(hr.Object, "spec", "suspend"); !suspend {
			t.Errorf("%s: not suspended — a probe that were created must never reconcile", c.name)
		}
		if ns, found, _ := unstructured.NestedString(hr.Object, "spec", "chartRef", "namespace"); found {
			t.Errorf("%s: chartRef.namespace %q set — the source-namespace rule must not be what decides", c.name, ns)
		}
		if c.admitted() {
			admitted++
		} else {
			denied++
		}
	}
	if denied != 2 || admitted != 2 {
		t.Errorf("%d denied and %d admitted cases, want 2 and 2", denied, admitted)
	}
}

// TestAdmissionCaseJudge: the verdict logic against the apiserver's answers —
// the fleet's denial verbatim passes a denied case, a bare Forbidden or the
// wrong rule does not, admission passes an admitted case and fails a denied
// one.
func TestAdmissionCaseJudge(t *testing.T) {
	cases := fleetAdmissionCases()
	deny, admit := cases[0], cases[1]
	fleetDenial := errors.New(`admission webhook "validate.kyverno.svc-fail" denied the request:

resource HelmRelease/org-lab/lab-admission-probe was blocked due to the following policies

flux-multi-tenancy:
  serviceAccountNameMustBeSet: 'validation error: either .spec.serviceAccountName
    or .spec.kubeConfig.secretRef.name is required. rule serviceAccountNameMustBeSet[0]
    failed at path /spec/serviceAccountName/ rule serviceAccountNameMustBeSet[1] failed
    at path /spec/kubeConfig/'`)
	if err := deny.judge(fleetDenial); err != nil {
		t.Errorf("the fleet's denial must satisfy the denied case: %v", err)
	}
	if err := deny.judge(nil); err == nil || !strings.Contains(err.Error(), "admitted it") {
		t.Errorf("admission must fail the denied case, got %v", err)
	}
	if err := deny.judge(errors.New("helmreleases.helm.toolkit.fluxcd.io is forbidden: User cannot create")); err == nil || !strings.Contains(err.Error(), "not the fleet's way") {
		t.Errorf("a denial without the policy must fail the denied case, got %v", err)
	}
	wrongRule := errors.New(strings.ReplaceAll(fleetDenial.Error(), "serviceAccountNameMustBeSet:", "targetNamespaceMustBeLocal:"))
	if err := deny.judge(wrongRule); err == nil {
		t.Error("a denial by another rule must fail the denied case")
	}
	if err := admit.judge(nil); err != nil {
		t.Errorf("admission must satisfy the admitted case: %v", err)
	}
	if err := admit.judge(fleetDenial); err == nil || !strings.Contains(err.Error(), "wanted admission") {
		t.Errorf("a denial must fail the admitted case, got %v", err)
	}
	if got := deny.String(); !strings.Contains(got, "denied by serviceAccountNameMustBeSet") {
		t.Errorf("String() = %q", got)
	}
	if got := admit.String(); !strings.HasSuffix(got, ": admitted") {
		t.Errorf("String() = %q", got)
	}
}
