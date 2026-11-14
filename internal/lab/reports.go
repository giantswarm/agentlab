package lab

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/fluxcd/cli-utils/pkg/kstatus/polling/engine"
	"github.com/fluxcd/cli-utils/pkg/kstatus/polling/statusreaders"
	"github.com/fluxcd/cli-utils/pkg/kstatus/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

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
// a policy's verdict from a PolicyReport (verdictFrom) instead of the
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

	// kyvernoGroup is the API group of Kyverno's policies.
	kyvernoGroup = "kyverno.io"
	// kindClusterPolicy and kindPod are the kinds a policy and the reports
	// about Pods name.
	kindClusterPolicy = "ClusterPolicy"
	kindPod           = "Pod"
	// reportScope, reportResults, reportPolicy and reportResult are the
	// PolicyReport fields a verdict reads: the resource the report is about,
	// its results, and each result's policy and outcome.
	reportScope   = "scope"
	reportResults = "results"
	reportPolicy  = "policy"
	reportResult  = "result"

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
	// Kyverno validates each policy on apply, about 5 s apiece on the lab's
	// node, so an unchanged release is left as it is rather than applied
	// again (as the platform chart is).
	if rev, err := helmDeployedRevision(kyvernoNamespace, kyvernoPoliciesRelease, kyvernoPoliciesChartVersion, nil); err != nil {
		return err
	} else if rev > 0 {
		note("chart kyverno-policies %s already installed — nothing to do (Helm revision %d stays)", kyvernoPoliciesChartVersion, rev)
	} else if err := installPodPolicies(cfg); err != nil {
		return err
	}
	names, err := waitPoliciesReady(context.Background(), kyvernoPoliciesSelector)
	if err != nil {
		return err
	}
	note("%d ClusterPolicies Ready: %s", len(names), strings.Join(names, ", "))
	return nil
}

// kyvernoPoliciesOptions installs and upgrades the kyverno-policies release
// with Helm's wait reading each policy's readiness from its Ready condition
// (kyvernoPolicyStatusReader).
var kyvernoPoliciesOptions = helmInstallOptions{
	CreateNamespace: true,
	StatusReaders:   []func(meta.RESTMapper) engine.StatusReader{newKyvernoPolicyStatusReader},
}

// kyvernoPolicyKinds are the Kyverno policies whose readiness is their Ready
// condition.
var kyvernoPolicyKinds = []schema.GroupKind{
	{Group: kyvernoGroup, Kind: kindClusterPolicy},
	{Group: kyvernoGroup, Kind: "Policy"},
}

// kyvernoPolicyStatusReader reads a Kyverno policy's readiness for Helm's
// kstatus wait from its Ready condition, where kstatus' own reader cannot:
// Kyverno writes no observedGeneration, and a policy it has not reconciled
// yet carries no condition, which kstatus reads as Current. Kyverno's first
// status write is then Ready=False "Not Ready", until its resource webhook
// carries the policy. Read by kstatus, a fresh install's policies were all
// Current before Kyverno had judged one, and Helm's waiter, which judges the
// statuses its watch delivers after it ended the wait, failed the install on
// the "Not Ready" that came next (HACKS.md U50). Read here, the wait ends
// only once every policy reads Ready=True, past Kyverno's Not Ready.
type kyvernoPolicyStatusReader struct {
	engine.StatusReader
}

// newKyvernoPolicyStatusReader is the reader on the cluster's REST mapper,
// which it needs to read a policy by its identifier.
func newKyvernoPolicyStatusReader(mapper meta.RESTMapper) engine.StatusReader {
	return kyvernoPolicyStatusReader{statusreaders.NewGenericStatusReader(mapper, kyvernoPolicyStatus)}
}

// Supports names the Kyverno policies; every other kind is kstatus'.
func (kyvernoPolicyStatusReader) Supports(gk schema.GroupKind) bool {
	return slices.Contains(kyvernoPolicyKinds, gk)
}

// kyvernoPolicyStatus is a policy's kstatus from its Ready condition:
// Current once True, InProgress before Kyverno wrote one and while it is
// False — never Failed, which Helm's wait leaves out of its aggregate and
// would end the wait on.
func kyvernoPolicyStatus(u *unstructured.Unstructured) (*status.Result, error) {
	if u.GetDeletionTimestamp() != nil {
		return &status.Result{Status: status.TerminatingStatus, Message: "Resource scheduled for deletion"}, nil
	}
	switch conditionStatus(u, conditionReady) {
	case conditionTrue:
		return &status.Result{Status: status.CurrentStatus, Message: conditionReady}, nil
	case "":
		return &status.Result{Status: status.InProgressStatus, Message: "Kyverno has not reconciled the policy yet"}, nil
	default:
		msg := conditionMessage(u, conditionReady)
		if msg == "" {
			msg = "Not Ready"
		}
		return &status.Result{Status: status.InProgressStatus, Message: msg}, nil
	}
}

// installPodPolicies installs the kyverno-policies release, once more when
// Kyverno's policy webhook timed out on it. A just-started admission
// controller can take longer than the webhook's 10 s deadline to validate a
// Pod policy (each computes its autogen rules for every Pod controller), and
// the chart's 22 policies queue behind each other; the second attempt, after
// the webhook answered a Pod policy again, upgrades the failed release.
func installPodPolicies(cfg *config.Config) error {
	install := func() error {
		return installOCIChartWith(cfg, kyvernoNamespace, kyvernoPoliciesRelease, kyvernoPoliciesChartRef, kyvernoPoliciesChartVersion, nil, kyvernoInstallTimeout, kyvernoPoliciesOptions)
	}
	err := install()
	if err == nil || !webhookTimedOut(err) {
		return err
	}
	note("Kyverno's policy webhook timed out on the Pod policies; waiting for it and installing them once more")
	if err := waitKyvernoServes(context.Background()); err != nil {
		return err
	}
	return install()
}

// webhookTimedOut reports whether an apply failed because Kyverno's policy
// webhook did not answer in time.
func webhookTimedOut(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "failed calling webhook") && strings.Contains(msg, "context deadline exceeded")
}

// waitPoliciesReady waits until every ClusterPolicy the selector names
// reports Ready, and returns their names. Kyverno sets Ready once its
// resource webhook configuration carries the policy, so a Pod applied after
// this is judged by it. After an install or upgrade Helm's wait has read the
// same condition (kyvernoPolicyStatusReader) and this reads them Ready at
// once; on an unchanged release, which Helm leaves alone, it is the wait.
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
		if conditionStatus(&p, conditionReady) != conditionTrue {
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

// verdictFrom finds the report scoped to the resource among reports and the
// policy's results in it.
func verdictFrom(reports []unstructured.Unstructured, namespace, kind, name, policy string) (reportVerdict, error) {
	v := reportVerdict{namespace: namespace, kind: kind, name: name, policy: policy}
	for _, r := range reports {
		scopeKind, _, _ := unstructured.NestedString(r.Object, reportScope, kindKey)
		scopeName, _, _ := unstructured.NestedString(r.Object, reportScope, nameKey)
		if scopeKind != kind || scopeName != name {
			continue
		}
		results, _, _ := unstructured.NestedSlice(r.Object, reportResults)
		var policies []string
		for _, res := range results {
			m, ok := res.(map[string]any)
			if !ok {
				continue
			}
			p, _ := m[reportPolicy].(string)
			if !slices.Contains(policies, p) {
				policies = append(policies, p)
			}
			if p == policy {
				result, _ := m[reportResult].(string)
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
	pods, err := listObjects(ctx, gvrPods, platformNamespace, "")
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
		if phase, _, _ := unstructured.NestedString(p.Object, crStatus, "phase"); phase != string(corev1.PodRunning) || p.GetDeletionTimestamp() != nil {
			continue
		}
		v, err := verdictFrom(reports, platformNamespace, kindPod, p.GetName(), reportProbePolicy)
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

// kyvernoDeployments are the controllers the lab's Kyverno runs; the
// admission controller serves every webhook.
var kyvernoDeployments = []string{"kyverno-admission-controller", "kyverno-reports-controller"}

// kyvernoServesAttempts and kyvernoServesInterval bound the wait for the
// policy webhook to answer from the rolled-out admission controller.
const (
	kyvernoServesAttempts = 45
	kyvernoServesInterval = 2 * time.Second
)

// waitKyvernoServes waits until an upgrade of the Kyverno release has
// settled: both controllers rolled out with no old replica left, and the
// policy webhook admitting a policy. A values change rolls the admission
// controller; while the old replica terminates, the policy webhook times out
// and Kyverno's webhook controller holds back every update of the resource
// webhook (it sees a rolling update while the Deployment counts more
// replicas than it wants), so a policy applied then is refused, or reads
// Ready long before admission enforces it. A policy applied after this is
// validated at once and enforced as soon as it is Ready.
func waitKyvernoServes(ctx context.Context) error {
	for _, d := range kyvernoDeployments {
		if err := waitDeploymentRolledOut(ctx, kyvernoNamespace, d, kyvernoInstallTimeout); err != nil {
			return err
		}
	}
	gvr, err := gvrFor(clusterPolicyResource)
	if err != nil {
		return err
	}
	probe, err := decodeManifests([]byte(kyvernoServesProbe))
	if err != nil || len(probe) != 1 {
		return fmt.Errorf("the Kyverno readiness probe policy: %d objects, %w", len(probe), err)
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	var last error
	served := waitFor(kyvernoServesAttempts, kyvernoServesInterval, func() bool {
		_, last = k.resource(gvr, "").Create(ctx, probe[0], metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		return last == nil
	})
	if !served {
		return fmt.Errorf("the Kyverno policy webhook does not admit a policy after %s: %w\ncheck `agentlab pods -n %s`", time.Duration(kyvernoServesAttempts)*kyvernoServesInterval, last, kyvernoNamespace)
	}
	note("Kyverno rolled out (%s) and its policy webhook answers", strings.Join(kyvernoDeployments, ", "))
	return nil
}

// kyvernoServesProbe is the policy the readiness check dry-runs: valid, in
// Audit, a Pod policy like the fleet's (so the webhook computes its autogen
// rules, the costly part of validating one), matching nothing the lab
// creates; never stored.
const kyvernoServesProbe = `apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: agentlab-kyverno-serves-probe
spec:
  validationFailureAction: Audit
  background: false
  rules:
    - name: probe
      match:
        any:
          - resources:
              kinds: [Pod]
              names: [agentlab-kyverno-serves-probe]
      validate:
        message: probe
        pattern:
          metadata:
            name: "?*"
`
