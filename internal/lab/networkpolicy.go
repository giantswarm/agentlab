package lab

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// The network policies' Dex peer (platform.networkPolicy, post-renderer
// patch 11): every platform pod that validates people's tokens dials the
// lab Dex inside the cluster — through the dex-localhost sidecar while the
// issuer is spelled localhost, through cluster DNS under platform.tls — on a
// private address no policy of an installation names, since an
// installation reaches its issuer through the world rule. So every egress
// policy of the component renders that selects such a pod gains one rule:
// the dex namespace on Dex's port. Which policies is a rule over the renders
// (dexEgressPolicies), as the sidecar's targets are: a component the chart
// adds with a policy of its own is covered the day it appears.

// dexNamespace and dexPodPort are the lab Dex as a policy names it: its
// namespace and the pod port behind both its Services (dex.yaml.tmpl).
const (
	dexNamespace = "dex"
	dexPodPort   = 5556
)

// dexEgressPolicy is one egress policy patch 11 appends the Dex rule to.
type dexEgressPolicy struct {
	name, namespace string
	// hasEgress reports whether the render lists egress rules to append to;
	// a policy without any (deny all) gets the list.
	hasEgress bool
}

// renderedPod is a Deployment's pod as a policy selects it.
type renderedPod struct {
	namespace string
	labels    labels.Set
}

// dexEgressPolicies picks patch 11's targets off the component renders
// (keyed by release name): every NetworkPolicy that isolates the egress of
// a Deployment dexDialers names, keyed by the component whose render holds
// the policy and sorted by name. A policy matches a pod in its own
// namespace, or any pod when either render leaves the namespace to the
// release; a policy whose selector cannot be read is the error.
func dexEgressPolicies(renders map[string]string, dialers map[string][]dexLocalhostTarget) (map[string][]dexEgressPolicy, error) {
	var pods []renderedPod
	for _, component := range slices.Sorted(maps.Keys(dialers)) {
		names := map[string]bool{}
		for _, d := range dialers[component] {
			names[d.deployment] = true
		}
		docs, err := renderedDocs(renders[component], kindDeployment)
		if err != nil {
			return nil, fmt.Errorf("the %s render: %w", component, err)
		}
		for _, doc := range docs {
			var d struct {
				Metadata metav1.ObjectMeta `json:"metadata"`
				Spec     struct {
					Template struct {
						Metadata metav1.ObjectMeta `json:"metadata"`
					} `json:"template"`
				} `json:"spec"`
			}
			if err := json.Unmarshal(doc, &d); err != nil {
				return nil, fmt.Errorf("the %s render's Deployment: %w", component, err)
			}
			if names[d.Metadata.Name] {
				pods = append(pods, renderedPod{namespace: d.Metadata.Namespace, labels: d.Spec.Template.Metadata.Labels})
			}
		}
	}
	policies := map[string][]dexEgressPolicy{}
	if len(pods) == 0 {
		return policies, nil
	}
	for _, component := range slices.Sorted(maps.Keys(renders)) {
		docs, err := renderedDocs(renders[component], kindNetworkPolicy)
		if err != nil {
			return nil, fmt.Errorf("the %s render: %w", component, err)
		}
		for _, doc := range docs {
			var np networkingv1.NetworkPolicy
			if err := json.Unmarshal(doc, &np); err != nil {
				return nil, fmt.Errorf("the %s render's NetworkPolicy: %w", component, err)
			}
			if !isolatesEgress(&np) {
				continue
			}
			selector, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
			if err != nil {
				return nil, fmt.Errorf("the %s render's NetworkPolicy %s: %w", component, np.Name, err)
			}
			if slices.ContainsFunc(pods, func(p renderedPod) bool {
				return (np.Namespace == "" || p.namespace == "" || np.Namespace == p.namespace) && selector.Matches(p.labels)
			}) {
				policies[component] = append(policies[component], dexEgressPolicy{name: np.Name, namespace: np.Namespace, hasEgress: len(np.Spec.Egress) > 0})
			}
		}
		slices.SortFunc(policies[component], func(a, b dexEgressPolicy) int { return strings.Compare(a.name, b.name) })
	}
	return policies, nil
}

// isolatesEgress reports whether a policy restricts its pods' egress: it
// names the Egress type, or names none and carries egress rules (the API's
// default for policyTypes).
func isolatesEgress(np *networkingv1.NetworkPolicy) bool {
	if len(np.Spec.PolicyTypes) == 0 {
		return len(np.Spec.Egress) > 0
	}
	return slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)
}

// renderedDocs is every document of a multi-document render whose kind is
// kind, as JSON for the API types. A render that is not YAML ends the list
// where it breaks, as renderedObjects does.
func renderedDocs(rendered, kind string) ([][]byte, error) {
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	var out [][]byte
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			continue
		}
		if err != nil {
			return out, nil
		}
		if doc["kind"] != kind {
			continue
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
}

// dexEgressPatch is patch 11 for one policy: the Dex rule appended to its
// egress list, or the list of that one rule where it has none.
func dexEgressPatch(p dexEgressPolicy) kustomizePatch {
	rule := fmt.Sprintf(`to:
  - namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: %s
ports:
  - port: %d
    protocol: TCP
`, dexNamespace, dexPodPort)
	rule = strings.TrimSuffix(rule, "\n")
	path, value := "/spec/egress/-", indent(rule, "    ")
	if !p.hasEgress {
		path, value = "/spec/egress", indent("- "+strings.ReplaceAll(rule, "\n", "\n  "), "    ")
	}
	target := map[string]string{kindKey: kindNetworkPolicy, nameKey: p.name}
	if p.namespace != "" {
		target[namespaceKey] = p.namespace
	}
	return kustomizePatch{
		Target: target,
		Patch:  literalYAML(fmt.Sprintf("- op: add\n  path: %s\n  value:\n%s\n", path, value)),
	}
}

// labWorldExcludedCIDRs are the policies' worldExcludedCIDRs in the lab
// (agent-platform-values.yaml.tmpl): the chart's private ranges with
// 10.0.0.0/8 narrowed to the cluster's own pod and Service subnets (kind's
// defaults, which kind-config.yaml.tmpl leaves). The resolver of a lab
// machine may answer a public name with a private address — a registry's
// private endpoint, a VPN's — so gsoci.azurecr.io, agent-manager's chart
// registry, can resolve into 10.0.0.0/8, and the chart's world rule, which
// an installation's cilium flavour writes by name, would refuse it. The pod
// and Service subnets stay excluded, so a pod reaches a platform pod only
// through a rule that names it, as on an installation.
var labWorldExcludedCIDRs = []string{
	"10.244.0.0/16",
	"10.96.0.0/16",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
}

// WorldExcludedCIDRs is labWorldExcludedCIDRs for the values template.
func (t *tmplData) WorldExcludedCIDRs() []string { return labWorldExcludedCIDRs }
