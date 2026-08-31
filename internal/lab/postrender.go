package lab

import (
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentplatform-kind/internal/config"
)

// PostRender is the Helm post-renderer for the agent-platform-standalone
// install (stdin: the full rendered release, stdout: the same with the muster
// and kagent-ui patches). Plain Helm has no values hook for any of these, so
// they live here — the replacement for the Flux postRenderers the old
// agent-platform meta-package forwarded to helm-controller. Wired up through
// a generated postrenderer/v1 plugin whose command is this very binary with
// the `post-render` arg (Helm 4 accepts only plugin-type post-renderers; see
// helmplugin.go).
//
// (The allowPublicClientRegistration ConfigMap edit that used to live here is
// gone: the muster chart renders the key itself since 5.7.2, muster#1118 —
// the value in agent-platform-values.yaml.tmpl is enough now.)
//
//  1. hostNetwork + dnsPolicy on the muster and backstage Deployments: both
//     must resolve the issuer URL to the Dex NodePort from inside the pod so
//     they can share ONE issuer URL with the browser on the host — the same
//     trick the kube-apiserver static pod uses. Not a chart value in either
//     chart. dnsPolicy ClusterFirstWithHostNet keeps cluster DNS, which the
//     CoreDNS rewrite needs to point *.<domain> at the edge Gateway.
//
//  2. maxSurge 0 on the same Deployments: hostNetwork means the pod binds its
//     port on the node, so the default rolling update deadlocks on a
//     single-node cluster — the new pod cannot start until the old one
//     releases the port. Old pod goes down first.
//
//  3. A fixed nodePort on the kagent-ui Service: the values set
//     `ui.service.type: NodePort` (a chart value), but the chart's ui-service
//     template renders no `nodePort` field, so Kubernetes would pick a random
//     one — useless to kind's fixed extraPortMappings. Pinned to
//     config.KagentUINodePort, the containerPort side of the mapping that
//     publishes the UI on the host (HACKS.md U9).
func PostRender(in io.Reader, out io.Writer) error {
	dec := yaml.NewDecoder(in)
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)
	defer func() { _ = enc.Close() }()

	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("parsing rendered release: %w", err)
		}
		root := docRoot(&doc)
		if root == nil || root.Kind != yaml.MappingNode {
			continue // empty document (the `select(. != null)` of the old yq)
		}
		kind := scalarAt(root, "kind")
		name := scalarAt(mapValue(root, "metadata"), nameKey)

		if kind == "Deployment" && (name == componentMuster || name == componentBackstage) {
			patchHostNetworkDeployment(root)
		}
		if kind == "Service" && name == "kagent-ui" {
			patchKagentUIService(root)
		}
		if err := enc.Encode(&doc); err != nil {
			return err
		}
	}
}

func patchHostNetworkDeployment(root *yaml.Node) {
	podSpec := ensureMap(ensureMap(ensureMap(root, "spec"), "template"), "spec")
	setKey(podSpec, "hostNetwork", boolNode(true))
	setKey(podSpec, "dnsPolicy", strNode("ClusterFirstWithHostNet"))

	strategy := &yaml.Node{Kind: yaml.MappingNode, Tag: yamlTagMap}
	setKey(strategy, "type", strNode("RollingUpdate"))
	rolling := &yaml.Node{Kind: yaml.MappingNode, Tag: yamlTagMap}
	setKey(rolling, "maxSurge", intNode(0))
	setKey(rolling, "maxUnavailable", intNode(1))
	setKey(strategy, "rollingUpdate", rolling)
	setKey(ensureMap(root, "spec"), "strategy", strategy)
}

// patchKagentUIService pins the UI port entry to the fixed NodePort the kind
// config maps onto the host. Keyed on the port name so an extra port added by
// a future chart stays untouched.
func patchKagentUIService(root *yaml.Node) {
	ports := mapValue(ensureMap(root, "spec"), "ports")
	if ports == nil || ports.Kind != yaml.SequenceNode {
		return
	}
	for _, p := range ports.Content {
		if scalarAt(p, nameKey) == "ui" {
			setKey(p, "nodePort", intNode(config.KagentUINodePort))
		}
	}
}

// --- yaml.Node plumbing -----------------------------------------------------

// yamlTagMap is the YAML tag for mapping nodes built from scratch.
const yamlTagMap = "!!map"

func docRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		root := doc.Content[0]
		if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
			return nil
		}
		return root
	}
	return doc
}

// mapValue returns the value node for a key in a mapping, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func scalarAt(m *yaml.Node, key string) string {
	v := mapValue(m, key)
	if v == nil {
		return ""
	}
	return v.Value
}

// ensureMap returns the mapping node at key, creating it if missing.
func ensureMap(m *yaml.Node, key string) *yaml.Node {
	if v := mapValue(m, key); v != nil {
		return v
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: yamlTagMap}
	m.Content = append(m.Content, strNode(key), v)
	return v
}

// setKey sets (or replaces) key -> value in a mapping node.
func setKey(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, strNode(key), value)
}

func strNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func boolNode(b bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprintf("%t", b)}
}

func intNode(n int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", n)}
}
