package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/giantswarm/agentlab/internal/config"
)

// The Cilium stand-ins (platform.networkPolicy): a policy the platform ships
// only as a CiliumNetworkPolicy — the Valkey store's, whose wrapper has no
// kubernetes flavour — is served in kind (the lab provides the CRDs) but
// enforced by nothing, since kind runs kindnetd, not Cilium. For each such
// policy of a release in ciliumStandInReleases the lab applies a plain
// NetworkPolicy translated from the render, which kindnetd enforces: the
// chart's own peers, so a renamed label or a dropped client fails in the lab
// as it would on an installation. The translation carries a policy's
// ingress; what it cannot carry (its egress, an entity other than the node,
// an L7 rule, mutual authentication) is named, never approximated by an
// open rule.

// ciliumStandInReleases are the releases whose CiliumNetworkPolicies the lab
// enforces through a stand-in.
var ciliumStandInReleases = []string{valkeyRelease}

// valkeyRelease is the release of the platform's Valkey (the routing and
// token store muster and klaus-gateway read).
const valkeyRelease = "valkey"

// kindCiliumNetworkPolicy is the kind a stand-in translates.
const kindCiliumNetworkPolicy = "CiliumNetworkPolicy"

// The stand-in's labels: agentlab's, and the policy it stands in for.
const (
	standInLabel       = "agentlab.giantswarm.io/stands-in-for"
	standInLabelPrefix = "ciliumnetworkpolicy."
	standInNameSuffix  = "-cilium-stand-in"
)

// Cilium's label keys for an endpoint's namespace: plain, and with the k8s
// source prefix.
var ciliumNamespaceKeys = []string{"io.kubernetes.pod.namespace", "k8s:io.kubernetes.pod.namespace"}

// ciliumNodeEntities are the entities a NetworkPolicy names by the nodes'
// addresses: the local node (the kubelet's probes) and the others.
var ciliumNodeEntities = []string{ciliumEntityHost, "remote-node"}

// ciliumEntityHost is Cilium's entity for the local node.
const ciliumEntityHost = "host"

// ciliumPolicy is the part of a CiliumNetworkPolicy the stand-in reads.
type ciliumPolicy struct {
	Metadata metav1.ObjectMeta `json:"metadata"`
	Spec     struct {
		EndpointSelector metav1.LabelSelector `json:"endpointSelector"`
		Ingress          []ciliumIngressRule  `json:"ingress"`
		IngressDeny      []json.RawMessage    `json:"ingressDeny"`
		Egress           []json.RawMessage    `json:"egress"`
		EgressDeny       []json.RawMessage    `json:"egressDeny"`
	} `json:"spec"`
}

// ciliumIngressRule is one ingress rule as the stand-in reads it.
type ciliumIngressRule struct {
	FromEndpoints  []metav1.LabelSelector `json:"fromEndpoints"`
	FromEntities   []string               `json:"fromEntities"`
	FromCIDR       []string               `json:"fromCIDR"`
	FromCIDRSet    []ciliumCIDRRule       `json:"fromCIDRSet"`
	ToPorts        []ciliumPortRule       `json:"toPorts"`
	Authentication *struct {
		Mode string `json:"mode"`
	} `json:"authentication"`
}

// ciliumCIDRRule is a fromCIDRSet entry.
type ciliumCIDRRule struct {
	CIDR   string   `json:"cidr"`
	Except []string `json:"except"`
}

// ciliumPortRule is a toPorts entry.
type ciliumPortRule struct {
	Ports []struct {
		Port     string `json:"port"`
		EndPort  int32  `json:"endPort"`
		Protocol string `json:"protocol"`
	} `json:"ports"`
	Rules json.RawMessage `json:"rules"`
}

// ciliumStandIn is one translated policy: the NetworkPolicy, and what of
// the Cilium policy it does not carry.
type ciliumStandIn struct {
	policy  *networkingv1.NetworkPolicy
	unheld  []string
	release string
}

// String names the policy and its stand-in for the boot log.
func (s ciliumStandIn) String() string {
	return fmt.Sprintf("%s/%s (%s)", s.policy.Namespace, s.policy.Name, s.release)
}

// ciliumStandIns translates every CiliumNetworkPolicy of the stand-in
// releases' renders, sorted by name. A render whose policy leaves the
// namespace to the release lands in namespace.
func ciliumStandIns(renders map[string]string, namespace string, nodeIPs []string) ([]ciliumStandIn, error) {
	var out []ciliumStandIn
	for _, release := range ciliumStandInReleases {
		docs, err := renderedDocs(renders[release], kindCiliumNetworkPolicy)
		if err != nil {
			return nil, fmt.Errorf("the %s render: %w", release, err)
		}
		for _, doc := range docs {
			var cnp ciliumPolicy
			if err := json.Unmarshal(doc, &cnp); err != nil {
				return nil, fmt.Errorf("the %s render's %s: %w", release, kindCiliumNetworkPolicy, err)
			}
			if cnp.Metadata.Namespace == "" {
				cnp.Metadata.Namespace = namespace
			}
			s, err := translateCiliumPolicy(&cnp, nodeIPs)
			if err != nil {
				return nil, fmt.Errorf("the %s render's %s %s: %w", release, kindCiliumNetworkPolicy, cnp.Metadata.Name, err)
			}
			s.release = release
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b ciliumStandIn) int { return strings.Compare(a.policy.Name, b.policy.Name) })
	return out, nil
}

// translateCiliumPolicy is the stand-in of one CiliumNetworkPolicy: an
// Ingress policy on the same pods whose rules admit the same peers on the
// same ports. Cilium resolves an endpoint selector without a namespace
// label to the policy's own namespace, as a NetworkPolicy's podSelector
// peer does. A rule the translation cannot carry whole is left out — the
// stand-in refuses more, never admits more — and named in unheld.
func translateCiliumPolicy(cnp *ciliumPolicy, nodeIPs []string) (ciliumStandIn, error) {
	selector, err := stripCiliumSource(cnp.Spec.EndpointSelector)
	if err != nil {
		return ciliumStandIn{}, fmt.Errorf("endpointSelector: %w", err)
	}
	if _, ns := namespaceOf(selector); ns {
		return ciliumStandIn{}, fmt.Errorf("endpointSelector names a namespace, which a NetworkPolicy's podSelector cannot")
	}
	np := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cnp.Metadata.Name + standInNameSuffix,
			Namespace: cnp.Metadata.Namespace,
			Labels: map[string]string{
				managedByLabel: applyFieldManager,
				standInLabel:   standInLabelPrefix + cnp.Metadata.Name,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: selector,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{},
		},
	}
	s := ciliumStandIn{policy: np}
	if len(cnp.Spec.Egress) > 0 || len(cnp.Spec.EgressDeny) > 0 {
		s.unheld = append(s.unheld, "its egress rules (the stand-in isolates ingress only)")
	}
	if len(cnp.Spec.IngressDeny) > 0 {
		s.unheld = append(s.unheld, "its ingressDeny rules (a NetworkPolicy has no deny)")
	}
	for i, rule := range cnp.Spec.Ingress {
		r, unheld, err := translateIngressRule(rule, nodeIPs)
		if err != nil {
			return ciliumStandIn{}, fmt.Errorf("ingress[%d]: %w", i, err)
		}
		for _, u := range unheld {
			s.unheld = append(s.unheld, fmt.Sprintf("ingress[%d]: %s", i, u))
		}
		if r != nil {
			np.Spec.Ingress = append(np.Spec.Ingress, *r)
		}
	}
	return s, nil
}

// translateIngressRule is one ingress rule's NetworkPolicy rule, nil when
// nothing of its peers can be carried (the rule is then left out whole).
func translateIngressRule(rule ciliumIngressRule, nodeIPs []string) (*networkingv1.NetworkPolicyIngressRule, []string, error) {
	var unheld []string
	r := &networkingv1.NetworkPolicyIngressRule{}
	for _, sel := range rule.FromEndpoints {
		peer, err := endpointPeer(sel)
		if err != nil {
			return nil, nil, fmt.Errorf("fromEndpoints: %w", err)
		}
		r.From = append(r.From, peer)
	}
	for _, entity := range rule.FromEntities {
		if !slices.Contains(ciliumNodeEntities, entity) {
			unheld = append(unheld, fmt.Sprintf("the %q entity (no NetworkPolicy peer stands for it)", entity))
			continue
		}
		if len(nodeIPs) == 0 {
			return nil, nil, fmt.Errorf("the %q entity needs the nodes' addresses, and none was given", entity)
		}
		for _, ip := range nodeIPs {
			peer := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: ip + "/32"}}
			if !slices.ContainsFunc(r.From, func(p networkingv1.NetworkPolicyPeer) bool {
				return p.IPBlock != nil && p.IPBlock.CIDR == peer.IPBlock.CIDR
			}) {
				r.From = append(r.From, peer)
			}
		}
	}
	for _, cidr := range rule.FromCIDR {
		r.From = append(r.From, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	for _, set := range rule.FromCIDRSet {
		if set.CIDR == "" {
			unheld = append(unheld, "a fromCIDRSet entry without a cidr (a CIDR group)")
			continue
		}
		r.From = append(r.From, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: set.CIDR, Except: set.Except}})
	}
	if rule.Authentication != nil && rule.Authentication.Mode != "" && rule.Authentication.Mode != "disabled" {
		unheld = append(unheld, fmt.Sprintf("mutual authentication %q (admitted without it)", rule.Authentication.Mode))
	}
	for _, pr := range rule.ToPorts {
		if len(pr.Rules) > 0 && string(pr.Rules) != "null" && string(pr.Rules) != "{}" {
			unheld = append(unheld, "an L7 rule (the port admitted whole)")
		}
		for _, p := range pr.Ports {
			port, err := policyPort(p.Port, p.EndPort, p.Protocol)
			if err != nil {
				return nil, nil, fmt.Errorf("toPorts: %w", err)
			}
			if port != nil {
				r.Ports = append(r.Ports, *port)
			}
		}
	}
	if len(r.From) == 0 {
		// A rule with no peer left would admit every source in a
		// NetworkPolicy: leave it out.
		return nil, unheld, nil
	}
	return r, unheld, nil
}

// endpointPeer is a fromEndpoints selector as a NetworkPolicy peer: its
// namespace label becomes the namespace selector, the rest the pod
// selector.
func endpointPeer(sel metav1.LabelSelector) (networkingv1.NetworkPolicyPeer, error) {
	sel, err := stripCiliumSource(sel)
	if err != nil {
		return networkingv1.NetworkPolicyPeer{}, err
	}
	ns, ok := namespaceOf(sel)
	for _, key := range ciliumNamespaceKeys {
		delete(sel.MatchLabels, strings.TrimPrefix(key, "k8s:"))
	}
	if len(sel.MatchLabels) == 0 {
		sel.MatchLabels = nil
	}
	peer := networkingv1.NetworkPolicyPeer{PodSelector: &sel}
	if ok {
		peer.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: ns}}
	}
	return peer, nil
}

// stripCiliumSource drops the k8s: source prefix of a selector's keys; a
// key of another source (reserved:, cidr:) cannot be translated.
func stripCiliumSource(sel metav1.LabelSelector) (metav1.LabelSelector, error) {
	out := metav1.LabelSelector{}
	strip := func(key string) (string, error) {
		if source, rest, ok := strings.Cut(key, ":"); ok && !strings.Contains(source, "/") && !strings.Contains(source, ".") {
			if source != "k8s" {
				return "", fmt.Errorf("label key %q of the Cilium source %q", key, source)
			}
			return rest, nil
		}
		return key, nil
	}
	for _, k := range slices.Sorted(maps.Keys(sel.MatchLabels)) {
		key, err := strip(k)
		if err != nil {
			return out, err
		}
		if out.MatchLabels == nil {
			out.MatchLabels = map[string]string{}
		}
		out.MatchLabels[key] = sel.MatchLabels[k]
	}
	for _, e := range sel.MatchExpressions {
		key, err := strip(e.Key)
		if err != nil {
			return out, err
		}
		if slices.Contains(ciliumNamespaceKeys, e.Key) || slices.Contains(ciliumNamespaceKeys, key) {
			return out, fmt.Errorf("a matchExpression on the namespace label")
		}
		e.Key = key
		out.MatchExpressions = append(out.MatchExpressions, e)
	}
	return out, nil
}

// namespaceOf is the namespace a selector's namespace label names.
func namespaceOf(sel metav1.LabelSelector) (string, bool) {
	for _, key := range ciliumNamespaceKeys {
		if ns, ok := sel.MatchLabels[strings.TrimPrefix(key, "k8s:")]; ok {
			return ns, true
		}
	}
	return "", false
}

// policyPort is one Cilium port as a NetworkPolicy port; "0" or an empty
// port is every port of the protocol (nil port), ANY every protocol.
func policyPort(port string, endPort int32, protocol string) (*networkingv1.NetworkPolicyPort, error) {
	out := &networkingv1.NetworkPolicyPort{}
	switch strings.ToUpper(protocol) {
	case "", "ANY":
		// Cilium's ANY is every protocol; a NetworkPolicy port names one,
		// so the stand-in admits TCP (refusing more, never admitting more).
		tcp := corev1.ProtocolTCP
		out.Protocol = &tcp
	case "TCP", "UDP", "SCTP":
		p := corev1.Protocol(strings.ToUpper(protocol))
		out.Protocol = &p
	default:
		return nil, fmt.Errorf("protocol %q", protocol)
	}
	if port != "" && port != "0" {
		if n, err := strconv.ParseInt(port, 10, 32); err == nil {
			v := intstr.FromInt32(int32(n))
			out.Port = &v
		} else {
			v := intstr.FromString(port)
			out.Port = &v
		}
	}
	if endPort > 0 {
		out.EndPort = &endPort
	}
	return out, nil
}

// ensureCiliumStandIns is the stand-ins of platform.networkPolicy, from the
// releases' renders: applied with the switch on, removed with it off.
func ensureCiliumStandIns(ctx context.Context, cfg *config.Config, renders map[string]string) error {
	if !cfg.Platform.NetworkPolicy {
		return applyCiliumStandIns(ctx, nil)
	}
	step("Standing in for the platform's Cilium-only policies with NetworkPolicies kindnetd enforces")
	nodeIPs, err := nodeAddresses(ctx)
	if err != nil {
		return err
	}
	standIns, err := ciliumStandIns(renders, platformNamespace, nodeIPs)
	if err != nil {
		return err
	}
	if len(standIns) == 0 {
		return fmt.Errorf("platform.networkPolicy: the %s render holds no %s — the lab's values turn valkey.ciliumNetworkPolicy on, so the chart dropped it and the store would run unguarded", strings.Join(ciliumStandInReleases, ", "), kindCiliumNetworkPolicy)
	}
	return applyCiliumStandIns(ctx, standIns)
}

// applyCiliumStandIns applies the stand-ins and removes every earlier one
// the list no longer has (a policy the chart dropped, the switch turned
// off: an empty list removes them all).
func applyCiliumStandIns(ctx context.Context, standIns []ciliumStandIn) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, s := range standIns {
		res, err := applyTyped(ctx, s.policy)
		if err != nil {
			return fmt.Errorf("the stand-in %s: %w", s, err)
		}
		keep[s.policy.Namespace+"/"+s.policy.Name] = true
		note("%s: a NetworkPolicy standing in for the %s CiliumNetworkPolicy kindnetd does not enforce (%s)", res, strings.TrimSuffix(s.policy.Name, standInNameSuffix), s.release)
		for _, u := range s.unheld {
			note("  not carried: %s", u)
		}
	}
	list, err := k.clientset.NetworkingV1().NetworkPolicies("").List(ctx, metav1.ListOptions{LabelSelector: standInLabel})
	if err != nil {
		return fmt.Errorf("listing the lab's Cilium stand-ins: %w", err)
	}
	for _, np := range list.Items {
		if keep[np.Namespace+"/"+np.Name] {
			continue
		}
		if err := k.clientset.NetworkingV1().NetworkPolicies(np.Namespace).Delete(ctx, np.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("removing the stand-in %s/%s: %w", np.Namespace, np.Name, err)
		}
		note("networkpolicy.networking.k8s.io/%s removed (no CiliumNetworkPolicy it stands in for)", np.Name)
	}
	return nil
}

// nodeAddresses is every lab node's InternalIP, sorted: the host entity's
// addresses (the kubelet's probes leave with them).
func nodeAddresses(ctx context.Context) ([]string, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	nodes, err := k.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the nodes: %w", err)
	}
	var ips []string
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP && !strings.Contains(a.Address, ":") {
				ips = append(ips, a.Address)
			}
		}
	}
	slices.Sort(ips)
	return slices.Compact(ips), nil
}
