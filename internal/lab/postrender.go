package lab

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"

	"agentlab/internal/config"
)

// PostRender is the Helm post-renderer for the agent-platform-standalone
// install (stdin: the full rendered release, stdout: the same with three
// muster patches). Plain Helm has no values hook for any of these, so they
// live here — the replacement for the Flux postRenderers the old
// agent-platform meta-package forwarded to helm-controller. Wired up as
// `helm --post-renderer <agentlab> --post-renderer-args post-render`.
//
//  1. hostNetwork + dnsPolicy on the muster Deployment: muster must resolve
//     the issuer URL to the Dex NodePort from inside the pod so it can share
//     ONE issuer URL with the browser on the host. Not a muster chart value.
//
//  2. maxSurge 0: hostNetwork means the pod binds the muster port on the node,
//     so the default rolling update deadlocks on a single-node cluster — the
//     new pod cannot start until the old one releases the port. Old pod goes
//     down first.
//
//  3. allowPublicClientRegistration: WORKAROUND (muster chart <= 5.5.6). The
//     chart exposes the key in values.yaml, values.schema.json and its README,
//     but templates/configmap.yaml never renders it, so the value is silently
//     dropped and Dynamic Client Registration stays gated. Claude Code
//     registers as a PUBLIC client on a random loopback port, so neither
//     `registrationToken` (it cannot send one),
//     `trustedPublicRegistrationRedirectURIs` (the port is random) nor
//     `trustedPublicRegistrationSchemes` (http/https are stripped by
//     mcp-oauth's config validation on purpose) can gate it open. The key is
//     edited into the rendered config surgically — everything else in the
//     ConfigMap stays chart-rendered, so muster bumps via the chart need no
//     hand-copying here. Delete this patch once the chart renders the key.
//
//  4. Drop the muster HTTPRoute. The chart hard-fails on empty
//     ingress.parentRefs in ALL modes (it assumes a public Gateway even for
//     muster-direct), so the values carry a placeholder parentRef and the
//     rendered route is stripped here: this lab has no Gateway, no Gateway API
//     CRDs, and reaches muster through hostNetwork + the kind port mapping.
//
//  5. A fixed nodePort on the kagent-ui Service: the values set
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

		if kind == "HTTPRoute" && name == componentMuster {
			continue
		}
		if kind == "Deployment" && name == componentMuster {
			patchMusterDeployment(root)
		}
		if kind == "ConfigMap" && name == "muster-config" {
			if err := patchMusterConfig(root); err != nil {
				return err
			}
		}
		if kind == "Service" && name == "kagent-ui" {
			patchKagentUIService(root)
		}
		if err := enc.Encode(&doc); err != nil {
			return err
		}
	}
}

func patchMusterDeployment(root *yaml.Node) {
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

func patchMusterConfig(root *yaml.Node) error {
	data := mapValue(root, "data")
	if data == nil {
		return fmt.Errorf("muster-config ConfigMap has no data")
	}
	cfgNode := mapValue(data, "config.yaml")
	if cfgNode == nil {
		return fmt.Errorf("muster-config ConfigMap has no config.yaml")
	}
	// Decode the nested config as a node tree too, so key order and the rest
	// of the chart-rendered content survive byte-for-byte in structure.
	var inner yaml.Node
	if err := yaml.Unmarshal([]byte(cfgNode.Value), &inner); err != nil {
		return fmt.Errorf("parsing muster config.yaml: %w", err)
	}
	innerRoot := docRoot(&inner)
	if innerRoot == nil {
		return fmt.Errorf("muster config.yaml is empty")
	}
	server := ensureMap(ensureMap(ensureMap(innerRoot, "aggregator"), "oauth"), "server")
	setKey(server, "allowPublicClientRegistration", boolNode(true))

	var buf bytes.Buffer
	ienc := yaml.NewEncoder(&buf)
	ienc.SetIndent(2)
	if err := ienc.Encode(&inner); err != nil {
		return err
	}
	_ = ienc.Close()

	cfgNode.SetString(buf.String())
	cfgNode.Style = yaml.LiteralStyle
	return nil
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
