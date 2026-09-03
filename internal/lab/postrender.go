package lab

import (
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
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
//
//  4. A `dex-localhost` sidecar on the MCP servers that validate the user's
//     forwarded id_token themselves (mcp-kubernetes, model-manager, and
//     mcp-prometheus through installOCIChart): they too must reach the issuer
//     URL, https://localhost:<DexPort>, but hostNetwork is not an option —
//     all three listen on :8080 and would collide on the single kind node.
//     The sidecar (socat) listens on the pod's own loopback :<DexPort> and
//     forwards to the Dex Service, so `localhost` resolves inside the pod
//     exactly as on the host; Dex's certificate carries `localhost`, so TLS
//     verification against the lab CA holds. Lab only (HACKS.md U10).
//
// dexPort is the lab Dex NodePort (agentlab.yaml dexPort), the port of the
// issuer URL the sidecar answers on.
func PostRender(in io.Reader, out io.Writer, dexPort int) error {
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
		if kind == "Deployment" && dexLocalhostDeployments[name] {
			patchDexLocalhostSidecar(root, dexPort)
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

// dexLocalhostDeployments are the Deployments that get the dex-localhost
// sidecar: every MCP server that runs mcp-oauth against the lab issuer without
// hostNetwork.
var dexLocalhostDeployments = map[string]bool{
	"mcp-kubernetes":      true,
	modelManagerMCPServer: true,
	mcpPrometheusRelease:  true,
}

// dexLocalhostImage is the socat image of the sidecar (side-loaded like every
// other lab image; Docker Hub).
const dexLocalhostImage = "alpine/socat:1.8.1.3"

// dexLocalhostContainer is the sidecar's name; a re-render replaces it in
// place instead of appending a second one.
const dexLocalhostContainer = "dex-localhost"

// dexServiceAddr is the lab Dex behind its ClusterIP Service (dex.yaml.tmpl):
// the same HTTPS endpoint the NodePort publishes on the host.
const dexServiceAddr = "dex.dex.svc.cluster.local:5556"

// patchDexLocalhostSidecar adds (or replaces) the dex-localhost sidecar on a
// Deployment's pod template. The pod keeps its own network namespace; the
// sidecar just makes 127.0.0.1/::1:<DexPort> answer with Dex.
func patchDexLocalhostSidecar(root *yaml.Node, dexPort int) {
	podSpec := ensureMap(ensureMap(ensureMap(root, "spec"), "template"), "spec")
	containers := mapValue(podSpec, "containers")
	if containers == nil || containers.Kind != yaml.SequenceNode {
		return
	}
	kept := make([]*yaml.Node, 0, len(containers.Content)+1)
	for _, c := range containers.Content {
		if scalarAt(c, nameKey) != dexLocalhostContainer {
			kept = append(kept, c)
		}
	}
	side := &yaml.Node{Kind: yaml.MappingNode, Tag: yamlTagMap}
	setKey(side, nameKey, strNode(dexLocalhostContainer))
	setKey(side, "image", strNode(dexLocalhostImage))
	args := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	// An IPv6 wildcard listener is dual-stack on Linux (bindv6only=0), so both
	// [::1] — which Go dials first for localhost — and 127.0.0.1 answer.
	args.Content = append(args.Content,
		strNode(fmt.Sprintf("TCP6-LISTEN:%d,fork,reuseaddr", dexPort)),
		strNode("TCP:"+dexServiceAddr))
	setKey(side, "args", args)
	resources := &yaml.Node{Kind: yaml.MappingNode, Tag: yamlTagMap}
	requests := &yaml.Node{Kind: yaml.MappingNode, Tag: yamlTagMap}
	setKey(requests, "cpu", strNode("5m"))
	setKey(requests, "memory", strNode("16Mi"))
	setKey(resources, "requests", requests)
	setKey(side, "resources", resources)
	containers.Content = append(kept, side)
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
