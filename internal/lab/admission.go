package lab

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agentlab/internal/config"
)

// The fleet's admission in the lab.
//
// Every Giant Swarm management cluster runs Kyverno and enforces the
// ClusterPolicy flux-multi-tenancy (management-cluster-bases,
// bases/flux-app-v2/common) on every Flux object outside flux-giantswarm,
// giantswarm and monitoring: a HelmRelease or Kustomization in an org
// namespace carries .spec.serviceAccountName or
// .spec.kubeConfig.secretRef.name, and leaves its namespace
// (targetNamespace, storageNamespace) only with a kubeconfig. A lab without
// it admits what an installation refuses: cluster-manager's own-cluster
// releases passed every lab proof and were denied on an installation
// (agentlab#192). So `agentlab up` carries the policy — verbatim
// (templates/flux-multi-tenancy.yaml, the ClusterPolicy and the aggregated
// ClusterRole its rules read with), in Enforce, on the fleet's Kyverno charts
// from gsoci (kyverno-crds and the kyverno app, the pair security-bundle
// installs) with its admission and reports controllers
// (templates/kyverno-values.yaml)
// — with one lab addition made here: the platform namespace joins each
// rule's exclude list, standing in for flux-giantswarm. On an installation
// the platform's child HelmReleases live there, exempt; in the lab they live
// in agent-platform and must stay admitted the same way (the lab-rendered
// mcp-prometheus release among them, which targets monitoring from
// agent-platform — legal only for an exempt namespace).
//
// The tenant the policy asks for exists too: templates/org-fixture.yaml is
// the lab's organization namespace shaped like the fleet's org-<org> — the
// ServiceAccount `automation` bound to cluster-admin inside the namespace by
// `write-all-customer-sa`, what rbac-operator gives every org namespace on an
// installation — so a release that names it (cluster-manager's default
// flux.tenantServiceAccount) is admitted AND runs.
//
// Proof, not presence: the boot ends with the matrix below run as server-side
// dry runs — a HelmRelease shaped like cluster-manager 0.5.2's pool release
// denied with the fleet's message, the two fleet shapes admitted, and the
// own-cluster operator's old shape (a tenant, but installing elsewhere)
// denied by targetNamespaceMustBeLocal. platform-test runs the same matrix.
// A dry run passes admission like a real create and leaves nothing behind;
// the policy's own webhook is registered only once the policy exists, so the
// first denial is also the readiness gate.
//
// The fleet's Pod policies follow, in Audit, with the reports controller
// writing their verdicts into PolicyReports (reports.go).
//
// What the lab does not carry: the chart's own Kyverno objects
// (kyvernoPolicies.enabled: false in the values template — PolicyExceptions
// that would excuse platform Pods from Pod Security Standards the lab only
// audits, and mutate policies the lab never ran). The chart's PolicyExceptions are
// judged without installing them: polextest.go dry-runs them against this
// Kyverno. The Flux kinds the policy also names and the lab does not serve
// — Kustomization, Alert, Receiver, ImagePolicy: the platform's engine runs
// source-controller and helm-controller only — are left out of the lab's
// copy at apply time (withoutUnservedKinds), with the rules that match
// nothing else: such a rule matches nothing here either way, and a kind no
// API serves keeps Kyverno refetching discovery. The HelmRelease rules are
// the ones the lab exercises.

const (
	// kyvernoNamespace and kyvernoRelease: the engine's Helm release,
	// kyvernoCRDsRelease the release of its CRDs beside it.
	kyvernoNamespace   = "kyverno"
	kyvernoRelease     = "kyverno"
	kyvernoCRDsRelease = "kyverno-crds"
	// kyvernoChartRef and kyvernoChartVersion pin the fleet's kyverno app
	// as security-bundle installs it on every installation: the chart
	// wraps the upstream Kyverno chart kyvernoVersion (v1.17.2) and
	// policy-reporter. kyvernoCRDsChartRef and kyvernoCRDsChartVersion pin
	// the kyverno-crds app it depends on, since the wrapper installs no
	// CRDs itself.
	kyvernoChartRef         = "oci://gsoci.azurecr.io/charts/giantswarm/kyverno"
	kyvernoChartVersion     = "0.24.2"
	kyvernoVersion          = "3.7.2"
	kyvernoCRDsChartRef     = "oci://gsoci.azurecr.io/charts/giantswarm/kyverno-crds"
	kyvernoCRDsChartVersion = "1.17.0"
	// kyvernoInstallTimeout bounds each chart's upgrade-or-install and its
	// wait (the CRDs; two Deployments).
	kyvernoInstallTimeout = 5 * time.Minute

	// fleetPolicyName is the ClusterPolicy the fleet enforces.
	fleetPolicyName = "flux-multi-tenancy"
	// fleetPolicyDeniedBy is the webhook the fleet's denial names — the
	// Enforce webhook's failure-policy suffix.
	fleetPolicyDeniedBy = `admission webhook "validate.kyverno.svc-fail" denied the request`

	// orgNamespace is the lab's organization namespace, tenantServiceAccount
	// the fleet's tenant identity in every org namespace and
	// tenantRoleBinding the binding that makes it cluster-admin there.
	orgNamespace         = "org-lab"
	tenantServiceAccount = "automation"
	tenantRoleBinding    = "write-all-customer-sa"

	// admissionProbeName names the dry-run HelmReleases and the
	// OCIRepository they reference (never created).
	admissionProbeName = "lab-admission-probe"
)

var (
	//go:embed templates/flux-multi-tenancy.yaml
	fluxMultiTenancy []byte
	//go:embed templates/kyverno-values.yaml
	kyvernoValues []byte
	//go:embed templates/org-fixture.yaml
	orgFixture []byte
)

// clusterPolicyResource is Kyverno's ClusterPolicy as kubectl's resource
// argument.
const clusterPolicyResource = "clusterpolicies.kyverno.io"

// fleetAdmissionUp installs the fleet's admission after the platform: the
// Kyverno engine, the org fixture, the policy with the platform namespace
// exempt, the proof that it enforces, and the fleet's Pod policies. After the platform because the
// policy names Flux kinds, which the platform's bundled engine brings.
func fleetAdmissionUp(cfg *config.Config) error {
	ctx := context.Background()
	step("Installing the fleet's admission: Kyverno %s (chart %s:%s, CRDs %s:%s) with the %s policy in Enforce", kyvernoVersion, kyvernoChartRef, kyvernoChartVersion, kyvernoCRDsChartRef, kyvernoCRDsChartVersion, fleetPolicyName)
	values, err := helmValues(kyvernoValues)
	if err != nil {
		return fmt.Errorf("kyverno-values.yaml: %w", err)
	}
	if err := uninstallUpstreamKyverno(); err != nil {
		return err
	}
	if err := installOCIChart(cfg, kyvernoNamespace, kyvernoCRDsRelease, kyvernoCRDsChartRef, kyvernoCRDsChartVersion, nil, kyvernoInstallTimeout); err != nil {
		return err
	}
	if err := installOCIChart(cfg, kyvernoNamespace, kyvernoRelease, kyvernoChartRef, kyvernoChartVersion, values, kyvernoInstallTimeout); err != nil {
		return err
	}
	if err := waitKyvernoServes(ctx); err != nil {
		return err
	}

	note("the org namespace %s with the tenant ServiceAccount %s (%s -> cluster-admin)", orgNamespace, tenantServiceAccount, tenantRoleBinding)
	if _, err := applyManifests(ctx, orgFixture); err != nil {
		return err
	}

	objs, err := labFleetPolicy(platformNamespace)
	if err != nil {
		return err
	}
	served, err := servedKindNames()
	if err != nil {
		return err
	}
	if dropped := withoutUnservedKinds(objs, served); len(dropped) > 0 {
		note("the %s rules leave out the kinds the lab does not serve: %s", fleetPolicyName, strings.Join(dropped, ", "))
	}
	// waitKyvernoServes proved the policy webhook answers; the retry covers
	// an apiserver that still routes a call to the pod it just replaced.
	var applyErr error
	applied := waitFor(30, 2*time.Second, func() bool {
		applyErr = applyObjects(ctx, objs)
		return applyErr == nil
	})
	if !applied {
		return fmt.Errorf("applying the %s policy: %w", fleetPolicyName, applyErr)
	}
	note("ClusterPolicy %s applied (Enforce; %s exempt next to the fleet's flux-giantswarm, giantswarm, monitoring)", fleetPolicyName, platformNamespace)

	// The readiness gate: the resource webhook exists only once a policy
	// does, so wait for the first real denial, then run the whole matrix.
	step("Verifying the policy enforces: a HelmRelease in %s without a tenant is denied", orgNamespace)
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return err
	}
	denied := fleetAdmissionCases()[0]
	var last error
	enforcing := waitFor(45, 2*time.Second, func() bool {
		last = denied.check(ctx, gvr)
		return last == nil
	})
	if !enforcing {
		return fmt.Errorf("the %s policy does not enforce after 90 s: %w\ncheck `agentlab pods -n %s` and `kubectl get validatingwebhookconfigurations`", fleetPolicyName, last, kyvernoNamespace)
	}
	results, err := proveFleetAdmission(ctx)
	if err != nil {
		return err
	}
	for _, r := range results {
		note("%s", r)
	}
	return fleetPodPoliciesUp(cfg)
}

// kyvernoAppHome is the home of the fleet's kyverno app chart; the upstream
// chart, also named kyverno, carries kyverno.io.
const kyvernoAppHome = "https://github.com/giantswarm/kyverno-app"

// uninstallUpstreamKyverno takes away the release an earlier agentlab
// installed from the upstream Kyverno chart. That release owns the CRDs,
// which the kyverno app does not render: upgrading it in place would delete
// them, and with them the policy, once the kyverno-crds release had taken
// them over. The uninstall drops CRDs, policy and engine together; the step
// installs all three again right after.
func uninstallUpstreamKyverno() error {
	meta, err := helmReleaseMeta(kyvernoNamespace, kyvernoRelease)
	if err != nil || !installedFromUpstreamKyverno(meta) {
		return err
	}
	note("release %s in %s runs the upstream chart %s %s: uninstalling it for the fleet's charts", kyvernoRelease, kyvernoNamespace, meta.Name, meta.Version)
	return helmUninstall(kyvernoNamespace, kyvernoRelease, true, kyvernoInstallTimeout)
}

// installedFromUpstreamKyverno reports whether a release's chart is the
// upstream Kyverno chart rather than the fleet's kyverno app.
func installedFromUpstreamKyverno(meta *chartv2.Metadata) bool {
	return meta != nil && meta.Name == "kyverno" && meta.Home != kyvernoAppHome
}

// labFleetPolicy decodes the fleet's bundle and adds the platform namespace
// (and any other given) to every rule's exclude list — the one lab
// difference, made here so the embedded file stays the fleet's.
func labFleetPolicy(exemptNamespaces ...string) ([]*unstructured.Unstructured, error) {
	objs, err := decodeManifests(fluxMultiTenancy)
	if err != nil {
		return nil, fmt.Errorf("flux-multi-tenancy.yaml: %w", err)
	}
	for _, obj := range objs {
		if obj.GetKind() != kindClusterPolicy {
			continue
		}
		rules, found, err := unstructured.NestedSlice(obj.Object, "spec", "rules")
		if err != nil || !found {
			return nil, fmt.Errorf("ClusterPolicy %s has no spec.rules", obj.GetName())
		}
		for i, r := range rules {
			rule, ok := r.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("ClusterPolicy %s: rule %d is not a map", obj.GetName(), i)
			}
			namespaces, _, _ := unstructured.NestedStringSlice(rule, "exclude", "resources", "namespaces")
			for _, ns := range exemptNamespaces {
				if !slices.Contains(namespaces, ns) {
					namespaces = append(namespaces, ns)
				}
			}
			if err := unstructured.SetNestedStringSlice(rule, namespaces, "exclude", "resources", "namespaces"); err != nil {
				return nil, err
			}
			rules[i] = rule
		}
		if err := unstructured.SetNestedSlice(obj.Object, rules, "spec", "rules"); err != nil {
			return nil, err
		}
	}
	return objs, nil
}

// servedKindNames is every kind the apiserver serves, by its bare name as a
// Kyverno rule names it.
func servedKindNames() (map[string]bool, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	return discoverKindNames(k.clientset.Discovery().ServerPreferredResources)
}

// discoverKindNames refuses partial discovery: a failed API group is not
// evidence that its kinds are absent. Otherwise withoutUnservedKinds could
// remove enforcement rules for a kind the lab actually serves.
func discoverKindNames(discover func() ([]*metav1.APIResourceList, error)) (map[string]bool, error) {
	lists, err := discover()
	if err != nil {
		return nil, fmt.Errorf("discovering the served kinds: %w", err)
	}
	served := map[string]bool{}
	for _, l := range lists {
		for _, r := range l.APIResources {
			served[r.Kind] = true
		}
	}
	return served, nil
}

// withoutUnservedKinds takes the kinds the lab does not serve out of every
// ClusterPolicy rule's match, and a rule left matching nothing out of the
// policy, and returns the kinds it took out, sorted. Kyverno resolves a
// policy's kinds through discovery and, for a kind no API serves, throws its
// whole discovery cache away and fetches it again — on every reconcile of
// the policy, about once a second. With the fleet's Flux kinds the lab's
// engine does not run (Alert, Receiver, ImagePolicy) that keeps every
// Kyverno controller refetching discovery: validating one policy takes
// seconds, a policy install meets the webhook's 10 s deadline, and the
// resource webhook falls minutes behind a policy. A rule of an unserved
// kind matches nothing in the lab either way.
func withoutUnservedKinds(objs []*unstructured.Unstructured, served map[string]bool) []string {
	dropped := map[string]bool{}
	for _, obj := range objs {
		if obj.GetKind() != kindClusterPolicy {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(obj.Object, "spec", "rules")
		kept := make([]any, 0, len(rules))
		for _, r := range rules {
			rule, ok := r.(map[string]any)
			if !ok {
				kept = append(kept, r)
				continue
			}
			kinds, found, _ := unstructured.NestedStringSlice(rule, "match", "resources", "kinds")
			if !found {
				kept = append(kept, rule)
				continue
			}
			var servedOnes []string
			for _, kind := range kinds {
				if served[kind] {
					servedOnes = append(servedOnes, kind)
				} else {
					dropped[kind] = true
				}
			}
			if len(servedOnes) == 0 {
				continue
			}
			_ = unstructured.SetNestedStringSlice(rule, servedOnes, "match", "resources", "kinds")
			kept = append(kept, rule)
		}
		_ = unstructured.SetNestedSlice(obj.Object, kept, "spec", "rules")
	}
	return slices.Sorted(maps.Keys(dropped))
}

// applyObjects server-side applies decoded objects in order.
func applyObjects(ctx context.Context, objs []*unstructured.Unstructured) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	for _, obj := range objs {
		if _, err := k.apply(ctx, obj); err != nil {
			return err
		}
	}
	return nil
}

// admissionCase is one HelmRelease shape in the org namespace and what the
// fleet's policy must answer for it.
type admissionCase struct {
	// name is the case in the boot log and the proof's verdict.
	name string
	// shape is the release's spec beyond the chart reference and interval.
	shape map[string]any
	// deniedBy names the rule that must refuse the shape; empty means the
	// shape must be admitted.
	deniedBy string
	// message is the rule's validation message, expected verbatim in a denial.
	message string
}

// admitted reports whether the case expects admission.
func (c admissionCase) admitted() bool { return c.deniedBy == "" }

func (c admissionCase) String() string {
	if c.admitted() {
		return c.name + ": admitted"
	}
	return c.name + ": denied by " + c.deniedBy
}

// fleetAdmissionCases is the matrix — the first case the denial that gates
// the boot on the webhook being live.
func fleetAdmissionCases() []admissionCase {
	return []admissionCase{
		{
			name:     "no serviceAccountName, no kubeConfig (cluster-manager 0.5.2's pool release)",
			shape:    map[string]any{},
			deniedBy: "serviceAccountNameMustBeSet",
			message:  "either .spec.serviceAccountName or .spec.kubeConfig.secretRef.name is required",
		},
		{
			name:  "serviceAccountName: " + tenantServiceAccount + " (the fleet's <cluster>-*-bundle shape)",
			shape: map[string]any{"serviceAccountName": tenantServiceAccount},
		},
		{
			name: "kubeConfig.secretRef into kube-system (the fleet's <cluster>-* into-the-cluster shape)",
			shape: map[string]any{
				"kubeConfig":      map[string]any{"secretRef": map[string]any{nameKey: "lab-kubeconfig"}},
				"targetNamespace": kubeSystemNamespace,
			},
		},
		{
			name: "serviceAccountName: " + tenantServiceAccount + " with targetNamespace kube-system and no kubeConfig",
			shape: map[string]any{
				"serviceAccountName": tenantServiceAccount,
				"targetNamespace":    kubeSystemNamespace,
			},
			deniedBy: "targetNamespaceMustBeLocal",
			message:  "spec.targetNamespace must be the same as metadata.namespace unless kubeConfig.secretRef.name is set",
		},
	}
}

// release builds the case's HelmRelease: a suspended release of a chart
// reference in its own namespace (the source-namespace rules pass), never
// reconciled even if it were created.
func (c admissionCase) release() *unstructured.Unstructured {
	spec := map[string]any{
		"interval": "10m",
		"suspend":  true,
		"chartRef": map[string]any{kindKey: "OCIRepository", nameKey: admissionProbeName},
	}
	for k, v := range c.shape {
		spec[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2",
		"kind":       kindHelmRelease,
		"metadata": map[string]any{
			nameKey:      admissionProbeName,
			namespaceKey: orgNamespace,
			"labels":     map[string]any{managedByLabel: managedByAgentlabValue},
		},
		"spec": spec,
	}}
}

// check runs the case as a server-side dry run and returns nil when the
// policy answered as the fleet would.
func (c admissionCase) check(ctx context.Context, gvr schema.GroupVersionResource) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	_, err = k.resource(gvr, orgNamespace).Create(ctx, c.release(), metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	return c.judge(err)
}

// judge compares the apiserver's answer with the expectation: an admitted
// shape must pass, a denied one must fail with the fleet's denial naming the
// policy, the rule and its message.
func (c admissionCase) judge(err error) error {
	if c.admitted() {
		if err != nil {
			return fmt.Errorf("%s: wanted admission, got: %w", c.name, err)
		}
		return nil
	}
	if err == nil {
		return fmt.Errorf("%s: wanted the %s policy's denial by %s, the apiserver admitted it", c.name, fleetPolicyName, c.deniedBy)
	}
	// The apiserver renders the denial as folded YAML: the message wraps
	// across lines, so compare with whitespace collapsed.
	got := strings.Join(strings.Fields(err.Error()), " ")
	for _, want := range []string{fleetPolicyDeniedBy, fleetPolicyName + ":", c.deniedBy + ":", c.message} {
		if !strings.Contains(got, strings.Join(strings.Fields(want), " ")) {
			return fmt.Errorf("%s: denied, but not the fleet's way — %q missing from: %w", c.name, want, err)
		}
	}
	return nil
}

// proveFleetAdmission runs the matrix and returns one line per case; the
// first case that answers differently fails with the evidence.
func proveFleetAdmission(ctx context.Context) ([]admissionCase, error) {
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return nil, err
	}
	policy, err := gvrFor(clusterPolicyResource)
	if err != nil {
		return nil, fmt.Errorf("%s: %w (is the fleet's Kyverno installed? run `agentlab platform`)", clusterPolicyResource, err)
	}
	obj, err := getObject(ctx, policy, "", fleetPolicyName)
	if err != nil {
		return nil, fmt.Errorf("ClusterPolicy %s: %w", fleetPolicyName, err)
	}
	if action, _, _ := unstructured.NestedString(obj.Object, "spec", "validationFailureAction"); action != "Enforce" {
		return nil, fmt.Errorf("ClusterPolicy %s: validationFailureAction is %q, the fleet's is Enforce", fleetPolicyName, action)
	}
	cases := fleetAdmissionCases()
	var errs []error
	for _, c := range cases {
		if err := c.check(ctx, gvr); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cases, nil
}
