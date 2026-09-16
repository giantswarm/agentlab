package lab

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

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
// ClusterRole its rules read with), in Enforce, on a Kyverno of the fleet's
// version reduced to its admission controller (templates/kyverno-values.yaml)
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
// What the lab does not carry: the fleet's Pod Security Standards policies
// and the chart's own Kyverno objects (kyvernoPolicies.enabled: false in the
// values template — PolicyExceptions to policies that are not here and
// mutate policies the lab never ran). The Flux kinds the policy also names —
// Kustomization, Alert, Receiver, ImagePolicy — are not served in the lab:
// the platform's engine runs source-controller and helm-controller only, so
// those rules match nothing here and the HelmRelease rules are the ones the
// lab exercises. Kyverno accepts the policy with a warning on apply
// ("unable to convert GVK to GVR for kinds Kustomization", the first kind
// it cannot resolve) — a warning, not a refusal, and the boot log shows it.

const (
	// kyvernoNamespace and kyvernoRelease: the engine's Helm release.
	kyvernoNamespace = "kyverno"
	kyvernoRelease   = "kyverno"
	// kyvernoChartRef and kyvernoChartVersion pin the upstream chart at the
	// subchart version the fleet's wrapper (giantswarm/kyverno 0.24.2) wraps:
	// 3.7.2 is Kyverno v1.17.2, the release every installation runs.
	kyvernoChartRef     = "oci://ghcr.io/kyverno/charts/kyverno"
	kyvernoChartVersion = "3.7.2"
	// kyvernoInstallTimeout bounds the chart's upgrade-or-install and its
	// wait (one Deployment and its CRDs).
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
// exempt, and the proof that it enforces. After the platform because the
// policy names Flux kinds, which the platform's bundled engine brings.
func fleetAdmissionUp(cfg *config.Config) error {
	ctx := context.Background()
	step("Installing the fleet's admission: Kyverno %s (chart %s) with the %s policy in Enforce", kyvernoChartVersion, kyvernoChartRef, fleetPolicyName)
	values, err := helmValues(kyvernoValues)
	if err != nil {
		return fmt.Errorf("kyverno-values.yaml: %w", err)
	}
	if err := installOCIChart(cfg, kyvernoNamespace, kyvernoRelease, kyvernoChartRef, kyvernoChartVersion, values, kyvernoInstallTimeout); err != nil {
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
	// Kyverno validates policies through its own webhook, registered as the
	// controller starts; the Deployment is Ready a moment before it answers.
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
		return fmt.Errorf("the %s policy does not enforce after 90 s: %w\ncheck `kubectl -n %s get pods` and `kubectl get validatingwebhookconfigurations`", fleetPolicyName, last, kyvernoNamespace)
	}
	results, err := proveFleetAdmission(ctx)
	if err != nil {
		return err
	}
	for _, r := range results {
		note("%s", r)
	}
	return nil
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
		if obj.GetKind() != "ClusterPolicy" {
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
		"kind":       "HelmRelease",
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
