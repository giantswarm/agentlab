package lab

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// The lab's per-component patches, rendered into the agent-platform values
// as `components.<name>.postRenderers` (agent-platform-values.yaml.tmpl).
// The meta chart forwards the list verbatim to that component's HelmRelease,
// and the bundled helm-controller runs them as Kustomize post-renderers over
// the component chart's render — the same mechanism the fleet has for chart
// fixes, and the plain replacement for the post-renderer binary earlier
// agentlab versions wrapped around `helm install`. Every patch is a strategic
// merge on one named object, so a chart bump needs no hand-copying:
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
//     forwarded id_token themselves (mcp-kubernetes, model-manager,
//     agent-manager, and the lab's mcp-prometheus through its own
//     HelmRelease): they too must reach the issuer URL,
//     https://localhost:<DexPort>, but hostNetwork is not an option — all
//     four listen on :8080 and would collide on the single kind node. The
//     sidecar (socat) listens on the pod's own loopback :<DexPort> and
//     forwards to the Dex Service, so `localhost` resolves inside the pod
//     exactly as on the host; Dex's certificate carries `localhost`, so TLS
//     verification against the lab CA holds. Lab only (HACKS.md U13).
//
//  5. The dev images (platform.devImages): a kustomize image override to the
//     build on this host plus imagePullPolicy IfNotPresent on its container,
//     so the side-loaded image is used as is (backstage's chart pulls Always).
//
//  6. The selector of the connectivity chart's kagent controller metrics
//     Service (the ServiceMonitor's target): the chart selects
//     `app.kubernetes.io/instance: <its own release name>`, which matched
//     under the standalone umbrella (one release for every subchart) and
//     matches no pod under the meta chart, where kagent is its own release
//     named `kagent`. Until the chart selects kagent's instance label the
//     lab's Prometheus would scrape nothing of kagent (HACKS.md U19).
//
// The values-side shape is Flux's: `postRenderers: [{kustomize: {patches:
// [{target, patch}], images: [{name, newName, newTag}]}}]`.

// dexLocalhostImage is the socat image of the sidecar (side-loaded like every
// other lab image; Docker Hub).
const dexLocalhostImage = "alpine/socat:1.8.1.3"

// dexLocalhostContainer is the sidecar's name; a strategic merge keys
// containers by name, so a re-render replaces it in place.
const dexLocalhostContainer = "dex-localhost"

// dexServiceAddr is the lab Dex behind its ClusterIP Service (dex.yaml.tmpl):
// the same HTTPS endpoint the NodePort publishes on the host.
const dexServiceAddr = "dex.dex.svc.cluster.local:5556"

// The target selector keys and kinds of the patches below.
const (
	kindKey        = "kind"
	kindDeployment = "Deployment"
	kindService    = "Service"
)

// kustomizePatch is one Flux postRenderers kustomize patch: a strategic merge
// (or JSON 6902) patch and the object it targets.
type kustomizePatch struct {
	Target map[string]string `yaml:"target"`
	Patch  literalYAML       `yaml:"patch"`
}

// literalYAML is a multi-line string that marshals in YAML's literal block
// style (`patch: |`), so the rendered values file reads as the manifest it
// carries rather than one escaped line.
type literalYAML string

func (l literalYAML) MarshalYAML() (any, error) {
	return &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.LiteralStyle, Value: string(l)}, nil
}

// kustomizeImage is one Flux postRenderers kustomize image override.
type kustomizeImage struct {
	Name    string `yaml:"name"`
	NewName string `yaml:"newName,omitempty"`
	NewTag  string `yaml:"newTag,omitempty"`
	Digest  string `yaml:"digest,omitempty"`
}

// postRenderer is one entry of a HelmRelease's postRenderers list.
type postRenderer struct {
	Kustomize struct {
		Patches []kustomizePatch `yaml:"patches,omitempty"`
		Images  []kustomizeImage `yaml:"images,omitempty"`
	} `yaml:"kustomize"`
}

// devImageTarget is what the dev-image swap of one component has to know
// about the component chart's render: the image name it replaces and the
// Deployment/container whose pull policy it relaxes.
type devImageTarget struct {
	deployment, container, image string
}

// devImageTargets maps the DevImageComponents to their chart render. Image
// names are the charts' at global.registry gsoci.azurecr.io — what kustomize
// matches on (the name without tag).
var devImageTargets = map[string]devImageTarget{
	componentMuster:        {componentMuster, componentMuster, "gsoci.azurecr.io/giantswarm/muster"},
	componentBackstage:     {componentBackstage, componentBackstage, "gsoci.azurecr.io/giantswarm/backstage"},
	componentKagent:        {"kagent-controller", "controller", "gsoci.azurecr.io/giantswarm/kagent-controller"},
	componentMCPKubernetes: {componentMCPKubernetes, componentMCPKubernetes, "gsoci.azurecr.io/giantswarm/mcp-kubernetes"},
	modelManagerMCPServer:  {modelManagerMCPServer, modelManagerMCPServer, "gsoci.azurecr.io/giantswarm/model-manager"},
	agentManagerMCPServer:  {agentManagerMCPServer, agentManagerMCPServer, "gsoci.azurecr.io/giantswarm/agent-manager"},
}

// hostNetworkPatch is patch 1+2 for a Deployment.
func hostNetworkPatch(deployment string) kustomizePatch {
	return kustomizePatch{
		Target: map[string]string{kindKey: kindDeployment, nameKey: deployment},
		Patch: literalYAML(fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
spec:
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 0
      maxUnavailable: 1
  template:
    spec:
      hostNetwork: true
      dnsPolicy: ClusterFirstWithHostNet
`, deployment)),
	}
}

// dexLocalhostPatch is patch 4 for a Deployment. An IPv6 wildcard listener is
// dual-stack on Linux (bindv6only=0), so both [::1] — which Go dials first for
// localhost — and 127.0.0.1 answer.
func dexLocalhostPatch(deployment string, dexPort int) kustomizePatch {
	return kustomizePatch{
		Target: map[string]string{kindKey: kindDeployment, nameKey: deployment},
		Patch: literalYAML(fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
spec:
  template:
    spec:
      containers:
        - name: %s
          image: %s
          args:
            - TCP6-LISTEN:%d,fork,reuseaddr
            - TCP:%s
          resources:
            requests:
              cpu: 5m
              memory: 16Mi
`, deployment, dexLocalhostContainer, dexLocalhostImage, dexPort, dexServiceAddr)),
	}
}

// kagentUINodePortPatch is patch 3. A Service's ports list merges by BOTH
// `port` and `protocol` (the list's map keys in the Kubernetes schema), so
// the entry restates the chart's port (8080) and protocol (TCP) next to the
// port name: an entry without the protocol matches nothing and kustomize
// drops it silently (seen on the first lab run — the Service came up on a
// random NodePort). A chart that moved the UI port would render a second `ui`
// entry and the apiserver would reject the Service — loud, rather than a
// silently random NodePort.
func kagentUINodePortPatch() kustomizePatch {
	return kustomizePatch{
		Target: map[string]string{kindKey: kindService, nameKey: "kagent-ui"},
		Patch: literalYAML(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: kagent-ui
spec:
  ports:
    - name: ui
      port: 8080
      protocol: TCP
      nodePort: %d
`, config.KagentUINodePort)),
	}
}

// connectivityComponent is the meta chart's component name of the
// agent-platform-connectivity chart, also its release name.
const connectivityComponent = "agent-platform-connectivity"

// kagentMetricsService is the connectivity chart's kagent controller metrics
// Service: `<release name>-kagent-controller-metrics`, in the kagent namespace.
const kagentMetricsService = connectivityComponent + "-kagent-controller-metrics"

// kagentMetricsSelectorPatch is patch 6: a strategic merge on the Service's
// selector map, so only the instance key changes (HACKS.md U19).
func kagentMetricsSelectorPatch() kustomizePatch {
	return kustomizePatch{
		Target: map[string]string{kindKey: kindService, nameKey: kagentMetricsService},
		Patch: literalYAML(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
spec:
  selector:
    app.kubernetes.io/instance: %s
`, kagentMetricsService, componentKagent)),
	}
}

// pullPolicyPatch is patch 5's second half: the container of a dev image
// takes the side-loaded copy as is.
func pullPolicyPatch(target devImageTarget) kustomizePatch {
	return kustomizePatch{
		Target: map[string]string{kindKey: kindDeployment, nameKey: target.deployment},
		Patch: literalYAML(fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
spec:
  template:
    spec:
      containers:
        - name: %s
          imagePullPolicy: IfNotPresent
`, target.deployment, target.container)),
	}
}

// sidecarPostRenderer is the one-patch postRenderers entry of a release the
// lab renders itself (mcp-prometheus.yaml.tmpl).
func sidecarPostRenderer(deployment string, dexPort int) postRenderer {
	var pr postRenderer
	pr.Kustomize.Patches = []kustomizePatch{dexLocalhostPatch(deployment, dexPort)}
	return pr
}

// componentPostRenderers renders the lab's postRenderers list per component
// as indented YAML, keyed by the agent-platform component name, for the
// values template. Components without a patch are absent.
func componentPostRenderers(cfg *config.Config) (map[string]string, error) {
	patches := map[string][]kustomizePatch{
		componentMuster:        {hostNetworkPatch(componentMuster)},
		componentBackstage:     {hostNetworkPatch(componentBackstage)},
		componentKagent:        {kagentUINodePortPatch()},
		componentMCPKubernetes: {dexLocalhostPatch(componentMCPKubernetes, cfg.DexPort)},
	}
	if cfg.ModelManagerEnabled() {
		patches[modelManagerMCPServer] = []kustomizePatch{dexLocalhostPatch(modelManagerMCPServer, cfg.DexPort)}
	}
	if cfg.Platform.Agents {
		patches[agentManagerMCPServer] = []kustomizePatch{dexLocalhostPatch(agentManagerMCPServer, cfg.DexPort)}
	}
	// The metrics Service renders only with kagent AND the monitors on; a
	// kustomize target that matches nothing is a no-op, but the values stay
	// minimal when the object is not there.
	if cfg.Platform.Agents && cfg.Platform.Observability {
		patches[connectivityComponent] = []kustomizePatch{kagentMetricsSelectorPatch()}
	}
	images := map[string][]kustomizeImage{}
	for _, component := range slices.Sorted(maps.Keys(cfg.Platform.DevImages)) {
		target := devImageTargets[component]
		images[component] = append(images[component], devImageOverride(target.image, cfg.Platform.DevImages[component]))
		patches[component] = append(patches[component], pullPolicyPatch(target))
	}
	out := map[string]string{}
	for component, list := range patches {
		var pr postRenderer
		pr.Kustomize.Patches = list
		pr.Kustomize.Images = images[component]
		raw, err := yaml.Marshal([]postRenderer{pr})
		if err != nil {
			return nil, fmt.Errorf("rendering the %s postRenderers: %w", component, err)
		}
		out[component] = strings.TrimRight(string(raw), "\n")
	}
	return out, nil
}

// devImageOverride is the kustomize image entry replacing the chart's image
// (name) with a dev ref: `muster:dev-1a2b` becomes newName
// docker.io/library/muster + newTag dev-1a2b — the fully qualified spelling
// kind's containerd lists the side-loaded image under, so the pod's ref and
// the node's copy agree byte for byte.
func devImageOverride(name, ref string) kustomizeImage {
	full := fullImageRef(ref)
	img := kustomizeImage{Name: name}
	if repo, digest, ok := strings.Cut(full, "@"); ok {
		img.NewName, img.Digest = repo, digest
		return img
	}
	// The tag is what follows the last colon after the last slash (a
	// registry port sits before the first slash).
	slash := strings.LastIndex(full, "/")
	if colon := strings.LastIndex(full, ":"); colon > slash {
		img.NewName, img.NewTag = full[:colon], full[colon+1:]
		return img
	}
	img.NewName = full
	return img
}

// devImageRefs lists the dev images to side-load, fully qualified.
func devImageRefs(cfg *config.Config) []string {
	var refs []string
	for _, ref := range cfg.Platform.DevImages {
		refs = append(refs, fullImageRef(ref))
	}
	slices.Sort(refs)
	return refs
}

// fullImageRef spells a ref the way containerd (and `crictl images`) does:
// docker's implicit registry and library namespace made explicit, so
// `muster:dev` is `docker.io/library/muster:dev` and `giantswarm/muster:dev`
// is `docker.io/giantswarm/muster:dev`. A ref whose first component names a
// registry (a dot, a port, or localhost) is left alone.
func fullImageRef(ref string) string {
	first, _, hasPath := strings.Cut(ref, "/")
	if !hasPath {
		return "docker.io/library/" + ref
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return ref
	}
	return "docker.io/" + ref
}
