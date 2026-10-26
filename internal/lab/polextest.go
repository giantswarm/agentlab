package lab

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The chart's PolicyExceptions, as the fleet's Kyverno answers them.
//
// The lab installs the chart with kyvernoPolicies.enabled: false (the values
// template says why), so none of the chart's PolicyExceptions reach the
// lab's apiserver through the install, and one the fleet's Kyverno refuses —
// `spec.match.any[].subjects: Forbidden: Usage of user info is forbidden`,
// subjects in an exception that background scans apply — would pass every
// other proof and fail an installation's upgrade. Kyverno validates every
// PolicyException on create through its exception webhook
// (kyverno-exception-validating-webhook-cfg, failurePolicy Fail, no side
// effects), the same validation whether or not the engine applies
// exceptions (features.policyExceptions, on in the lab as on the fleet). The
// lab's Kyverno is the fleet's (admission.go), so the proof needs no second
// engine: the chart is rendered offline with the lab's values,
// kyvernoPolicies.enabled: true — the shape every installation with Kyverno
// renders — and, where the agents runtime is on, tenants.organizations
// enabled, the switch that adds the exceptions for the Organizations'
// workers, so the exceptions the fleet renders are judged whether or not
// this lab runs the tenants. The meta chart and each component release it
// carries are rendered (the connectivity chart's exceptions among them), and
// every PolicyException in those renders is created as a server-side dry
// run in its namespace. A refusal fails with the apiserver's message; a dry
// run leaves nothing behind. The exceptions' namespace is the one fixture: a
// dry run into a namespace that does not exist is refused by the apiserver
// before any webhook sees it, so the proof makes sure each exists (the
// fleet's policy-exceptions, there on every installation).

// policyExceptionKind is Kyverno's PolicyException kind.
const policyExceptionKind = "PolicyException"

// PolexTestOptions picks the chart the PolicyException proof renders.
type PolexTestOptions struct {
	// ChartVersion is an exact agent-platform release to render instead of
	// the chart agentlab.yaml installs (platform.chartVersion, chartPath,
	// chartBranch); the lab's values stay the same.
	ChartVersion string
}

// polexVerdict is one rendered PolicyException and the apiserver's answer.
type polexVerdict struct {
	// source names the render it came from: the meta chart or a component
	// release.
	source    string
	namespace string
	name      string
	// refusal is the apiserver's refusal; nil when the dry run was admitted.
	refusal error
}

func (v polexVerdict) String() string {
	if v.refusal == nil {
		return fmt.Sprintf("%s/%s (%s): admitted", v.namespace, v.name, v.source)
	}
	return fmt.Sprintf("%s/%s (%s): refused: %v", v.namespace, v.name, v.source, v.refusal)
}

// PolexTest is the standalone proof: the chart's PolicyExceptions dry-run
// against the lab's Kyverno, one line per exception, then the verdict.
func PolexTest(cfg *config.Config, opts PolexTestOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	chart := platformChartFor(cfg)
	if opts.ChartVersion != "" {
		chart = platformChart{ref: config.ChartRepository, version: opts.ChartVersion, labRegistry: labRegistryAddressFor(cfg)}
	}
	step("Rendering the PolicyExceptions of %s (%s) and dry-running them against the lab's Kyverno %s", chart, polexShape(cfg), kyvernoVersion)
	verdict, err := proveChartPolicyExceptions(cfg, chart)
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Println(verdict)
	return nil
}

// proveChartPolicyExceptions renders the chart's PolicyExceptions and puts
// each to the lab's apiserver as a server-side dry run. Every exception's
// answer is noted; any refusal fails the proof with all refusals' messages.
// The renders take their time; the dry runs are bounded by kubeReadTimeout
// from the moment they start.
func proveChartPolicyExceptions(cfg *config.Config, chart platformChart) (string, error) {
	exceptions, err := renderChartPolicyExceptions(cfg, chart)
	if err != nil {
		return "", err
	}
	if len(exceptions) == 0 {
		return fmt.Sprintf("PASS: %s renders no PolicyException (%s) — nothing for Kyverno to refuse", chart, polexShape(cfg)), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	verdicts, err := dryRunPolicyExceptions(ctx, exceptions)
	if err != nil {
		return "", err
	}
	var refused []error
	for _, v := range verdicts {
		note("%s", v)
		if v.refusal != nil {
			refused = append(refused, fmt.Errorf("PolicyException %s/%s (%s): %w", v.namespace, v.name, v.source, v.refusal))
		}
	}
	if len(refused) > 0 {
		return "", fmt.Errorf("the fleet's Kyverno %s refuses %d of the %d PolicyExceptions %s renders (%s) — an installation's upgrade fails on them:\n%w", kyvernoVersion, len(refused), len(verdicts), chart, polexShape(cfg), errors.Join(refused...))
	}
	return fmt.Sprintf("PASS: the fleet's Kyverno %s admits all %d PolicyExceptions %s renders (%s) — %s", kyvernoVersion, len(verdicts), chart, polexShape(cfg), polexNames(verdicts)), nil
}

// renderedPolicyException is a PolicyException with the render it came from.
type renderedPolicyException struct {
	source string
	obj    *unstructured.Unstructured
}

// renderChartPolicyExceptions renders the meta chart with the lab's values,
// kyvernoPolicies.enabled: true and, with the agents runtime on, the tenants
// (polexValues), then every component release of that
// render, and returns their PolicyExceptions in render order. A component
// render that fails is judged as `agentlab platform` judges it
// (judgeRenderFailures): an unreachable registry, a missing chart or a chart
// refusing its values stops the proof, the rest is a note.
func renderChartPolicyExceptions(cfg *config.Config, chart platformChart) ([]renderedPolicyException, error) {
	values, err := polexValues(cfg)
	if err != nil {
		return nil, err
	}
	roster, err := renderPlatformRoster(chart, values)
	if err != nil {
		return nil, fmt.Errorf("rendering %s with kyvernoPolicies.enabled: true: %w", chart, err)
	}
	_, _, renders, errs := fluxReleaseImages(roster.releases, polexAPIVersions(cfg))
	if err := judgeRenderFailures(chart, errs); err != nil {
		return nil, err
	}
	// The connectivity chart carries most of the platform's exceptions: a
	// proof without its render proves nothing about them.
	if roster.has(config.ConnectivityChartName) {
		if _, ok := renders[config.ConnectivityChartName]; !ok {
			i := slices.IndexFunc(errs, func(f renderFailure) bool { return f.rel.Name == config.ConnectivityChartName })
			if i >= 0 {
				return nil, fmt.Errorf("the %s release does not render with kyvernoPolicies.enabled: true, so its PolicyExceptions cannot be judged: %w", config.ConnectivityChartName, errs[i].err)
			}
			return nil, fmt.Errorf("the %s release has no render, so its PolicyExceptions cannot be judged", config.ConnectivityChartName)
		}
	}
	exceptions, err := policyExceptionsOf(platformRelease, roster.manifest)
	if err != nil {
		return nil, err
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range renders {
			if !yield(name) {
				return
			}
		}
	})
	for _, name := range names {
		found, err := policyExceptionsOf(name, renders[name])
		if err != nil {
			return nil, err
		}
		exceptions = append(exceptions, found...)
	}
	return exceptions, nil
}

// polexValues are the meta chart's values the proof renders: the lab's,
// with the tenants on where the agents runtime is (the template's
// platform.tenants block: tenants.organizations and what the chart requires
// beside it), and kyvernoPolicies.enabled: true.
func polexValues(cfg *config.Config) (map[string]any, error) {
	values, err := platformValuesWith(cfg, func(t *tmplData) {
		t.TenantsEnabled = t.TenantsEnabled || polexTenants(cfg)
	})
	if err != nil {
		return nil, err
	}
	withKyvernoPolicies(values)
	return values, nil
}

// polexAPIVersions are the component renders' .Capabilities: the lab's
// (componentAPIVersions) and the APIs the switches the proof turns on ask
// for, which the lab's apiserver serves — Kyverno's and the
// MutatingAdmissionPolicy the tenants' ModelConfig policy needs.
func polexAPIVersions(cfg *config.Config) []string {
	versions := append(componentAPIVersions(cfg), "kyverno.io/v1")
	if !slices.Contains(versions, mutatingAdmissionPolicyAPIVersion) {
		versions = append(versions, mutatingAdmissionPolicyAPIVersion)
	}
	return versions
}

// polexTenants reports whether the proof renders the tenants: wherever the
// agents runtime of the 4.x line is on, the tenants' requirement.
func polexTenants(cfg *config.Config) bool {
	return cfg.Platform.Enabled && cfg.Platform.Agents && !cfg.LegacyChart()
}

// polexShape words the values the proof renders, for its lines.
func polexShape(cfg *config.Config) string {
	if polexTenants(cfg) {
		return "the lab's values, kyvernoPolicies.enabled: true, tenants.organizations on"
	}
	return "the lab's values, kyvernoPolicies.enabled: true"
}

// withKyvernoPolicies sets kyvernoPolicies.enabled: true on the meta chart's
// values, keeping the block's other keys (the exception namespace, the rule
// map) as the lab or an overlay set them.
func withKyvernoPolicies(values map[string]any) {
	block, _ := values["kyvernoPolicies"].(map[string]any)
	if block == nil {
		block = map[string]any{}
	}
	block[valuesEnabled] = true
	values["kyvernoPolicies"] = block
}

// policyExceptionsOf decodes a render and keeps its PolicyExceptions.
func policyExceptionsOf(source, manifest string) ([]renderedPolicyException, error) {
	objs, err := decodeManifests([]byte(manifest))
	if err != nil {
		return nil, fmt.Errorf("decoding the render of %s: %w", source, err)
	}
	var found []renderedPolicyException
	for _, obj := range objs {
		if obj.GetKind() == policyExceptionKind && obj.GroupVersionKind().Group == "kyverno.io" {
			found = append(found, renderedPolicyException{source: source, obj: obj})
		}
	}
	return found, nil
}

// dryRunPolicyExceptions makes sure each exception's namespace exists, then
// creates every exception as a server-side dry run and records the answer.
// An error is the proof's own (no client, no such kind served); a refusal of
// an exception is its verdict.
func dryRunPolicyExceptions(ctx context.Context, exceptions []renderedPolicyException) ([]polexVerdict, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	var namespaces []string
	for _, e := range exceptions {
		if ns := e.obj.GetNamespace(); ns != "" && !slices.Contains(namespaces, ns) {
			namespaces = append(namespaces, ns)
		}
	}
	for _, ns := range namespaces {
		if err := ensureNamespace(ns); err != nil {
			return nil, fmt.Errorf("the PolicyExceptions' namespace %s: %w", ns, err)
		}
	}
	verdicts := make([]polexVerdict, 0, len(exceptions))
	for _, e := range exceptions {
		gvk := e.obj.GroupVersionKind()
		mapping, err := k.restMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w (is the fleet's Kyverno installed? run `agentlab platform`)", gvk.Kind, e.obj.GetName(), err)
		}
		ns := e.obj.GetNamespace()
		_, err = k.resource(mapping.Resource, ns).Create(ctx, e.obj, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		// Only the apiserver's answer is a verdict; a call that got none is
		// the proof's own failure.
		if _, answered := err.(apierrors.APIStatus); err != nil && !answered {
			return nil, fmt.Errorf("dry-running PolicyException %s/%s: %w", ns, e.obj.GetName(), err)
		}
		verdicts = append(verdicts, polexVerdict{source: e.source, namespace: ns, name: e.obj.GetName(), refusal: err})
	}
	return verdicts, nil
}

// polexNames lists the admitted exceptions for the verdict line.
func polexNames(verdicts []polexVerdict) string {
	names := make([]string, 0, len(verdicts))
	for _, v := range verdicts {
		names = append(names, v.name)
	}
	return strings.Join(names, ", ")
}
