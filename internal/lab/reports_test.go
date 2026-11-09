package lab

import (
	"errors"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// seccompPolicy, resultPass and resultFail name a fleet policy and the
// results a PolicyReport records.
const (
	seccompPolicy = "restrict-seccomp"
	resultPass    = "pass"
	resultFail    = "fail"
)

// TestKyvernoValuesRunTheReports: the lab's Kyverno writes reports as an
// installation's does — the reports controller on, admission reports,
// aggregation, policy reports and the background scan enabled.
func TestKyvernoValuesRunTheReports(t *testing.T) {
	values, err := helmValues(kyvernoValues)
	if err != nil {
		t.Fatal(err)
	}
	if on, _, _ := unstructured.NestedBool(values, "kyverno", "reportsController", "enabled"); !on {
		t.Error("kyverno.reportsController.enabled is not true")
	}
	for _, feature := range []string{"admissionReports", "aggregateReports", "policyReports", "backgroundScan"} {
		if on, _, _ := unstructured.NestedBool(values, "kyverno", "features", feature, "enabled"); !on {
			t.Errorf("kyverno.features.%s.enabled is not true", feature)
		}
	}
	if pc, _, _ := unstructured.NestedString(values, "kyverno", "reportsController", "priorityClassName"); pc != "" {
		t.Errorf("kyverno.reportsController.priorityClassName = %q, the lab has no installation PriorityClass", pc)
	}
}

func policyWithReady(name, status string) unstructured.Unstructured {
	obj := unstructured.Unstructured{Object: map[string]any{
		crMetadata: map[string]any{nameKey: name},
	}}
	if status != "" {
		obj.Object[crStatus] = map[string]any{crConditions: []any{
			map[string]any{fieldType: conditionReady, crStatus: status},
		}}
	}
	return obj
}

// TestPolicyReadiness: a policy counts as Ready only with the condition
// True; one without a status yet is pending.
func TestPolicyReadiness(t *testing.T) {
	names, pending := policyReadiness([]unstructured.Unstructured{
		policyWithReady(seccompPolicy, conditionTrue),
		policyWithReady("disallow-host-path", "False"),
		policyWithReady("disallow-capabilities", ""),
	})
	if want := []string{"disallow-capabilities", "disallow-host-path", seccompPolicy}; !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if want := []string{"disallow-capabilities", "disallow-host-path"}; !slices.Equal(pending, want) {
		t.Errorf("pending = %v, want %v", pending, want)
	}
}

func policyReport(name, kind, scope string, results ...[2]string) unstructured.Unstructured {
	var rs []any
	for _, r := range results {
		rs = append(rs, map[string]any{reportPolicy: r[0], "rule": r[0] + "-rule", reportResult: r[1]})
	}
	return unstructured.Unstructured{Object: map[string]any{
		fieldAPIVersion: "wgpolicyk8s.io/v1alpha2",
		kindKey:         "PolicyReport",
		crMetadata:      map[string]any{nameKey: name, namespaceKey: platformNamespace},
		reportScope:     map[string]any{kindKey: kind, nameKey: scope, namespaceKey: platformNamespace},
		reportResults:   rs,
	}}
}

// TestVerdictFrom: the report scoped to the resource yields the policy's
// results; a resource without a report, or a report without the policy, is
// an error naming what is there.
func TestVerdictFrom(t *testing.T) {
	reports := []unstructured.Unstructured{
		policyReport("u1", "Deployment", "muster", [2]string{reportProbePolicy, resultPass}),
		policyReport("u2", kindPod, "muster-abc", [2]string{seccompPolicy, resultFail}, [2]string{reportProbePolicy, resultPass}, [2]string{reportProbePolicy, resultPass}),
		policyReport("u3", kindPod, "kagent-xyz", [2]string{seccompPolicy, resultPass}),
	}

	v, err := verdictFrom(reports, platformNamespace, kindPod, "muster-abc", reportProbePolicy)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(v.results, []string{resultPass, resultPass}) {
		t.Errorf("results = %v, want [pass pass]", v.results)
	}
	if got, want := v.String(), "Pod agent-platform/muster-abc: "+reportProbePolicy+" pass,pass"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	if _, err := verdictFrom(reports, platformNamespace, kindPod, "kagent-xyz", reportProbePolicy); err == nil || !strings.Contains(err.Error(), "policies: restrict-seccomp") {
		t.Errorf("report without the policy: err = %v, want one naming the policies there", err)
	}
	if _, err := verdictFrom(reports, platformNamespace, kindPod, "gone", reportProbePolicy); err == nil || !strings.Contains(err.Error(), "no PolicyReport") {
		t.Errorf("no report: err = %v, want no PolicyReport", err)
	}
}

// TestVerdictTally counts verdicts by result.
func TestVerdictTally(t *testing.T) {
	got := verdictTally([]reportVerdict{
		{results: []string{resultPass}}, {results: []string{resultFail}}, {results: []string{resultPass}},
	})
	if want := "fail 1, pass 2"; got != want {
		t.Errorf("verdictTally = %q, want %q", got, want)
	}
}

// TestWebhookTimedOut: only a policy webhook's deadline counts as the
// timeout the Pod policies' install retries on.
func TestWebhookTimedOut(t *testing.T) {
	timedOut := errors.New(`INSTALL FAILED: server-side apply failed for object /disallow-capabilities kyverno.io/v1, Kind=ClusterPolicy: Internal error occurred: failed calling webhook "validate-policy.kyverno.svc": failed to call webhook: Post "https://kyverno-svc.kyverno.svc:443/policyvalidate?timeout=10s": context deadline exceeded`)
	if !webhookTimedOut(timedOut) {
		t.Error("the policy webhook's deadline is not recognised")
	}
	for _, err := range []error{
		errors.New("INSTALL FAILED: admission webhook \"validate-policy.kyverno.svc\" denied the request: invalid pattern"),
		errors.New("pulling oci://gsoci.azurecr.io/charts/giantswarm/kyverno-policies: context deadline exceeded"),
	} {
		if webhookTimedOut(err) {
			t.Errorf("%q counted as the webhook's timeout", err)
		}
	}
}

// TestWithoutUnservedKinds: the lab's copy of the fleet's policy keeps the
// served kinds of every rule, drops the unserved ones, and drops a rule left
// matching nothing.
func TestWithoutUnservedKinds(t *testing.T) {
	objs, err := labFleetPolicy(platformNamespace)
	if err != nil {
		t.Fatal(err)
	}
	served := map[string]bool{kindHelmRelease: true, "Kustomization": true}
	dropped := withoutUnservedKinds(objs, served)
	if want := []string{"Alert", "ImagePolicy", "Receiver"}; !slices.Equal(dropped, want) {
		t.Errorf("dropped = %v, want %v", dropped, want)
	}
	rules, _, _ := unstructured.NestedSlice(objs[0].Object, "spec", "rules")
	var names []string
	for _, r := range rules {
		rule := r.(map[string]any)
		name, _, _ := unstructured.NestedString(rule, "name")
		names = append(names, name)
		kinds, _, _ := unstructured.NestedStringSlice(rule, "match", "resources", "kinds")
		for _, k := range kinds {
			if !served[k] {
				t.Errorf("rule %s still matches the unserved kind %s", name, k)
			}
		}
	}
	if want := []string{"serviceAccountNameMustBeSet", "kustomizationSourceRefNamespaceIsSafe", "helmReleaseSourceRefNamespaceIsSafe", "targetNamespaceMustBeLocal", "storageNamespaceMustBeLocal"}; !slices.Equal(names, want) {
		t.Errorf("rules = %v, want %v", names, want)
	}

	// Without the Kustomization API, the rule of Kustomization alone goes
	// and the shared rules keep HelmRelease.
	objs, _ = labFleetPolicy(platformNamespace)
	withoutUnservedKinds(objs, map[string]bool{kindHelmRelease: true})
	rules, _, _ = unstructured.NestedSlice(objs[0].Object, "spec", "rules")
	first, _, _ := unstructured.NestedStringSlice(rules[0].(map[string]any), "match", "resources", "kinds")
	if len(rules) != 4 || !slices.Equal(first, []string{kindHelmRelease}) {
		t.Errorf("HelmRelease only: %d rules, the first matching %v; want 4, [HelmRelease]", len(rules), first)
	}
}

// Partial discovery must never become a list that strips enforcement rules.
func TestDiscoverKindNames(t *testing.T) {
	resources := []*metav1.APIResourceList{{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{{Name: "pods", Kind: kindPod}},
	}}
	discoveryErr := errors.New("kustomize.toolkit.fluxcd.io/v1 discovery unavailable")
	for _, readErr := range []error{nil, discoveryErr} {
		kinds, err := discoverKindNames(func() ([]*metav1.APIResourceList, error) {
			return resources, readErr
		})
		if readErr != nil {
			if !errors.Is(err, discoveryErr) || kinds != nil {
				t.Fatalf("partial discovery = %v, %v; want no kinds and the discovery error", kinds, err)
			}
		} else if err != nil || !kinds[kindPod] || len(kinds) != 1 {
			t.Fatalf("complete discovery = %v, %v; want Pod only", kinds, err)
		}
	}
}
