package lab

import (
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
		"metadata": map[string]any{"name": name},
	}}
	if status != "" {
		obj.Object["status"] = map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": status},
		}}
	}
	return obj
}

// TestPolicyReadiness: a policy counts as Ready only with the condition
// True; one without a status yet is pending.
func TestPolicyReadiness(t *testing.T) {
	names, pending := policyReadiness([]unstructured.Unstructured{
		policyWithReady("restrict-seccomp", "True"),
		policyWithReady("disallow-host-path", "False"),
		policyWithReady("disallow-capabilities", ""),
	})
	if want := []string{"disallow-capabilities", "disallow-host-path", "restrict-seccomp"}; !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if want := []string{"disallow-capabilities", "disallow-host-path"}; !slices.Equal(pending, want) {
		t.Errorf("pending = %v, want %v", pending, want)
	}
}

func policyReport(name, kind, scope string, results ...[2]string) unstructured.Unstructured {
	var rs []any
	for _, r := range results {
		rs = append(rs, map[string]any{"policy": r[0], "rule": r[0] + "-rule", "result": r[1]})
	}
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wgpolicyk8s.io/v1alpha2",
		"kind":       "PolicyReport",
		"metadata":   map[string]any{"name": name, "namespace": "agent-platform"},
		"scope":      map[string]any{"kind": kind, "name": scope, "namespace": "agent-platform"},
		"results":    rs,
	}}
}

// TestVerdictFrom: the report scoped to the resource yields the policy's
// results; a resource without a report, or a report without the policy, is
// an error naming what is there.
func TestVerdictFrom(t *testing.T) {
	reports := []unstructured.Unstructured{
		policyReport("u1", "Deployment", "muster", [2]string{reportProbePolicy, "pass"}),
		policyReport("u2", "Pod", "muster-abc", [2]string{"restrict-seccomp", "fail"}, [2]string{reportProbePolicy, "pass"}, [2]string{reportProbePolicy, "pass"}),
		policyReport("u3", "Pod", "kagent-xyz", [2]string{"restrict-seccomp", "pass"}),
	}

	v, err := verdictFrom(reports, "agent-platform", "Pod", "muster-abc", reportProbePolicy)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(v.results, []string{"pass", "pass"}) {
		t.Errorf("results = %v, want [pass pass]", v.results)
	}
	if got, want := v.String(), "Pod agent-platform/muster-abc: "+reportProbePolicy+" pass,pass"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	if _, err := verdictFrom(reports, "agent-platform", "Pod", "kagent-xyz", reportProbePolicy); err == nil || !strings.Contains(err.Error(), "policies: restrict-seccomp") {
		t.Errorf("report without the policy: err = %v, want one naming the policies there", err)
	}
	if _, err := verdictFrom(reports, "agent-platform", "Pod", "gone", reportProbePolicy); err == nil || !strings.Contains(err.Error(), "no PolicyReport") {
		t.Errorf("no report: err = %v, want no PolicyReport", err)
	}
}

// TestVerdictTally counts verdicts by result.
func TestVerdictTally(t *testing.T) {
	got := verdictTally([]reportVerdict{
		{results: []string{"pass"}}, {results: []string{"fail"}}, {results: []string{"pass"}},
	})
	if want := "fail 1, pass 2"; got != want {
		t.Errorf("verdictTally = %q, want %q", got, want)
	}
}
