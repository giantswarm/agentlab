package lab

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The fleet's Pod policies and their reports in the lab.
//
// An installation's Kyverno judges every Pod against the fleet's Pod
// Security Standards and best-practice policies (the kyverno-policies app
// security-bundle installs) and writes the verdicts into PolicyReports, which
// policy-reporter and every policy proof read. The lab carries the same pair:
// the kyverno app with its reports controller (templates/kyverno-values.yaml)
// and the kyverno-policies chart security-bundle pins, so `kubectl get
// policyreports -A` lists a result for every platform Pod and a proof asserts
// a policy's verdict from a PolicyReport (policyReportResult) instead of the
// kyverno CLI.
//
// One lab difference: the chart's own default, Audit, where security-bundle
// sets Enforce. The lab installs the platform chart without its
// PolicyExceptions (kyvernoPolicies.enabled: false, admission.go), so an
// enforced Pod Security Standard would refuse the platform Pods an
// installation's exceptions admit; in Audit every Pod runs and its report
// says what the fleet's policy answers.

const (
	// kyvernoPoliciesRelease, kyvernoPoliciesChartRef and
	// kyvernoPoliciesChartVersion pin the fleet's kyverno-policies app as
	// security-bundle 2.4.0 installs it beside the kyverno app.
	kyvernoPoliciesRelease      = "kyverno-policies"
	kyvernoPoliciesChartRef     = "oci://gsoci.azurecr.io/charts/giantswarm/kyverno-policies"
	kyvernoPoliciesChartVersion = "0.27.1"
	// kyvernoPoliciesSelector selects every ClusterPolicy of that release.
	kyvernoPoliciesSelector = "app.kubernetes.io/instance=" + kyvernoPoliciesRelease

	// policyReportResource is the namespaced PolicyReport the reports
	// controller writes, one per resource it judged.
	policyReportResource = "policyreports.wgpolicyk8s.io"

	// reportProbePolicy is the fleet policy the platform proof reads a
	// verdict of from a platform Pod's PolicyReport: a Pod Security
	// Standard (Baseline) every Pod is judged against.
	reportProbePolicy = "disallow-privileged-containers"

	// policyReadyAttempts and policyReadyInterval bound the wait for a
	// policy's Ready condition: Kyverno sets it once the resource webhook
	// carries the policy.
	policyReadyAttempts = 60
	policyReadyInterval = 2 * time.Second
	// policyReportAttempts and policyReportInterval bound the wait for a
	// Pod's report: the reports controller scans every existing resource
	// when a policy arrives, then aggregates the result.
	policyReportAttempts = 60
	policyReportInterval = 5 * time.Second
)

// fleetPodPoliciesUp installs the fleet's Pod policies on the lab's Kyverno
// and waits until every one of them is Ready, so a proof that applies a Pod
// next is judged by all of them.
func fleetPodPoliciesUp(cfg *config.Config) error {
	note("the fleet's Pod policies: chart %s:%s (Audit, the chart's default)", kyvernoPoliciesChartRef, kyvernoPoliciesChartVersion)
	if err := installOCIChart(cfg, kyvernoNamespace, kyvernoPoliciesRelease, kyvernoPoliciesChartRef, kyvernoPoliciesChartVersion, nil, kyvernoInstallTimeout); err != nil {
		return err
	}
	names, err := waitPoliciesReady(context.Background(), kyvernoPoliciesSelector)
	if err != nil {
		return err
	}
	note("%d ClusterPolicies Ready: %s", len(names), strings.Join(names, ", "))
	return nil
}

// waitPoliciesReady waits until every ClusterPolicy the selector names
// reports Ready, and returns their names. Kyverno sets Ready once its
// resource webhook configuration carries the policy, so a Pod applied after
// this is judged by it.
func waitPoliciesReady(ctx context.Context, selector string) ([]string, error) {
	gvr, err := gvrFor(clusterPolicyResource)
	if err != nil {
		return nil, err
	}
	var names, pending []string
	var readErr error
	ready := waitFor(policyReadyAttempts, policyReadyInterval, func() bool {
		var policies []unstructured.Unstructured
		policies, readErr = listObjects(ctx, gvr, "", selector)
		if readErr != nil {
			return false
		}
		names, pending = policyReadiness(policies)
		return len(names) > 0 && len(pending) == 0
	})
	switch {
	case ready:
		return names, nil
	case readErr != nil:
		return nil, fmt.Errorf("reading the ClusterPolicies %s: %w", selector, readErr)
	case len(names) == 0:
		return nil, fmt.Errorf("no ClusterPolicy %s after %s", selector, time.Duration(policyReadyAttempts)*policyReadyInterval)
	default:
		return nil, fmt.Errorf("ClusterPolicies not Ready after %s: %s\ncheck `agentlab pods -n %s`", time.Duration(policyReadyAttempts)*policyReadyInterval, strings.Join(pending, ", "), kyvernoNamespace)
	}
}

// policyReadiness splits policies into all their names and the names of
// those not yet Ready, both sorted.
func policyReadiness(policies []unstructured.Unstructured) (names, pending []string) {
	for _, p := range policies {
		names = append(names, p.GetName())
		if conditionStatus(&p, "Ready") != "True" {
			pending = append(pending, p.GetName())
		}
	}
	slices.Sort(names)
	slices.Sort(pending)
	return names, pending
}

// reportVerdict is one policy's verdict on one resource, as a PolicyReport
// carries it.
type reportVerdict struct {
	// namespace, kind and name are the resource the report scopes.
	namespace, kind, name string
	// policy is the ClusterPolicy; results its rules' results (pass, fail,
	// warn, error, skip), one per rule that matched, sorted.
	policy  string
	results []string
}

func (v reportVerdict) String() string {
	return fmt.Sprintf("%s %s/%s: %s %s", v.kind, v.namespace, v.name, v.policy, strings.Join(v.results, ","))
}

// policyReportResult reads the PolicyReport the reports controller wrote for
// a resource and returns the policy's verdict on it — what a proof asserts
// instead of running the kyverno CLI. A resource without a report, or whose
// report carries no result of the policy, is an error naming what is there.
func policyReportResult(ctx context.Context, namespace, kind, name, policy string) (reportVerdict, error) {
	gvr, err := gvrFor(policyReportResource)
	if err != nil {
		return reportVerdict{}, fmt.Errorf("%w (is the lab's Kyverno reports controller installed? run `agentlab platform`)", err)
	}
	reports, err := listObjects(ctx, gvr, namespace, "")
	if err != nil {
		return reportVerdict{}, err
	}
	return verdictFrom(reports, namespace, kind, name, policy)
}

// verdictFrom finds the report scoped to the resource among reports and the
// policy's results in it.
func verdictFrom(reports []unstructured.Unstructured, namespace, kind, name, policy string) (reportVerdict, error) {
	v := reportVerdict{namespace: namespace, kind: kind, name: name, policy: policy}
	for _, r := range reports {
		scopeKind, _, _ := unstructured.NestedString(r.Object, "scope", "kind")
		scopeName, _, _ := unstructured.NestedString(r.Object, "scope", "name")
		if scopeKind != kind || scopeName != name {
			continue
		}
		results, _, _ := unstructured.NestedSlice(r.Object, "results")
		var policies []string
		for _, res := range results {
			m, ok := res.(map[string]any)
			if !ok {
				continue
			}
			p, _ := m["policy"].(string)
			if !slices.Contains(policies, p) {
				policies = append(policies, p)
			}
			if p == policy {
				result, _ := m["result"].(string)
				v.results = append(v.results, result)
			}
		}
		if len(v.results) == 0 {
			slices.Sort(policies)
			return v, fmt.Errorf("the PolicyReport %s/%s of %s %s carries no result of %s (policies: %s)", namespace, r.GetName(), kind, name, policy, strings.Join(policies, ", "))
		}
		slices.Sort(v.results)
		return v, nil
	}
	return v, fmt.Errorf("no PolicyReport in %s scopes %s %s (%d reports there)", namespace, kind, name, len(reports))
}

// provePolicyReports is the platform proof's report stage: every Running Pod
// of the platform namespace carries a PolicyReport with the fleet's
// reportProbePolicy verdict, waited for while the reports controller
// catches up with Pods a roll just created.
func provePolicyReports(ctx context.Context) ([]reportVerdict, error) {
	var verdicts []reportVerdict
	var last error
	ok := waitFor(policyReportAttempts, policyReportInterval, func() bool {
		verdicts, last = platformPodVerdicts(ctx)
		return last == nil
	})
	if !ok {
		return nil, fmt.Errorf("the platform's Pods carry no complete PolicyReports after %s: %w\ncheck `kubectl get policyreports -n %s` and `agentlab pods -n %s`", time.Duration(policyReportAttempts)*policyReportInterval, last, platformNamespace, kyvernoNamespace)
	}
	return verdicts, nil
}

// platformPodVerdicts reads reportProbePolicy's verdict for every Running
// Pod in the platform namespace.
func platformPodVerdicts(ctx context.Context) ([]reportVerdict, error) {
	podGVR, err := gvrFor("pods")
	if err != nil {
		return nil, err
	}
	pods, err := listObjects(ctx, podGVR, platformNamespace, "")
	if err != nil {
		return nil, err
	}
	reportGVR, err := gvrFor(policyReportResource)
	if err != nil {
		return nil, err
	}
	reports, err := listObjects(ctx, reportGVR, platformNamespace, "")
	if err != nil {
		return nil, err
	}
	var verdicts []reportVerdict
	var errs []error
	for _, p := range pods {
		if phase, _, _ := unstructured.NestedString(p.Object, "status", "phase"); phase != "Running" || p.GetDeletionTimestamp() != nil {
			continue
		}
		v, err := verdictFrom(reports, platformNamespace, "Pod", p.GetName(), reportProbePolicy)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		verdicts = append(verdicts, v)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(verdicts) == 0 {
		return nil, fmt.Errorf("no Running Pod in %s", platformNamespace)
	}
	return verdicts, nil
}

// verdictTally counts the verdicts by their results, "pass 12, fail 1".
func verdictTally(verdicts []reportVerdict) string {
	counts := map[string]int{}
	for _, v := range verdicts {
		counts[strings.Join(v.results, ",")]++
	}
	keys := slices.Sorted(maps.Keys(counts))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}
