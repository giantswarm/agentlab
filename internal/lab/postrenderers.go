package lab

import (
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
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
//  4. A `dex-localhost` sidecar on every Deployment that carries the
//     platform's OAuth contract: the MCP servers that validate the user's
//     forwarded id_token themselves (mcp-kubernetes, the managers —
//     model-manager, agent-manager, vm-manager, cluster-manager — and the
//     lab's mcp-prometheus through its own HelmRelease) must reach the issuer
//     URL, https://localhost:<DexPort>/dex, but hostNetwork is not an option
//     — all of them listen on :8080 and would collide on the single kind
//     node. The sidecar (socat) listens on the pod's own loopback :<DexPort>
//     and forwards to the Dex Service, so `localhost` resolves inside the pod
//     exactly as on the host; Dex's certificate carries `localhost`, so TLS
//     verification against the lab CA holds. Which Deployments get it is a
//     rule over the component charts' renders, not a list: every Deployment
//     whose containers are told the lab Dex's localhost address
//     (dexLocalhostTargets) — so a component the chart turns on by default or
//     an overlay turns on is covered the day it appears. Lab only (HACKS.md
//     U13).
//
//  5. The dev images (platform.devImages): a kustomize image override to the
//     build on this host plus imagePullPolicy IfNotPresent on its container,
//     so the side-loaded image is used as is (backstage's chart pulls Always).
//     The name the override replaces is the component chart's, read from its
//     render (devimages.go) — the kagent controller's differs between the
//     lines — and an override that would match nothing in the render is an
//     error before the install, because kustomize drops it silently. The
//     `harness` target is not a post-renderer: the platform Harness pins its
//     image by digest through the connectivity values (kagent.harness.image,
//     the values template), served by the lab registry.
//
//  6. The Slack Web API base of the klaus-gateway component
//     (KLAUS_GATEWAY_SLACK_API_BASE, which the chart has no value for): the
//     selector-less Service `agentlab klaus-gateway-test` points at its fake
//     Slack Web API on this host while it runs, since no workspace answers
//     the lab (klausgateway.go). Lab only.
//
// The values-side shape is Flux's: `postRenderers: [{kustomize: {patches:
// [{target, patch}], images: [{name, newName, newTag}]}}]`.

// dexLocalhostImage is the socat image of the sidecar (side-loaded like every
// other lab image): the gsoci mirror of alpine/socat.
const dexLocalhostImage = "gsoci.azurecr.io/giantswarm/socat:1.7.4.4"

// dexLocalhostContainer is the sidecar's name; a strategic merge keys
// containers by name, so a re-render replaces it in place.
const dexLocalhostContainer = "dex-localhost"

// dexLocalhostAnnotation, on a Deployment or its pod template, overrides the
// sidecar rule (dexLocalhostKey) for that Deployment: "false" keeps the
// sidecar off one the rule would select, "true" puts it on one the rule
// would skip — a server that dials the issuer through a name the rule does
// not know. Any other value is an error.
const dexLocalhostAnnotation = "agentlab.giantswarm.io/dex-localhost"

// dexDialNames are the names — of an environment variable, or of an argument
// in its variable spelling (flagVariable: `--dex-issuer-url` is
// DEX_ISSUER_URL) — through which the platform's OAuth resource servers are
// told the issuer they dial for OIDC discovery and the JWKS: the managers'
// `--dex-issuer-url`, the mcp-oauth servers' DEX_ISSUER_URL (mcp-kubernetes,
// mcp-prometheus), oauth2-proxy's OIDC_ISSUER_URL (the kagent UI). A variable
// that carries the address as an identity only — the authorization server a
// resource server names in its RFC 9728 metadata and keys grants by
// (OAUTH_AUTHORIZATION_SERVER), an issuer a token is checked against — is
// never dialed and selects nothing, whatever its value.
var dexDialNames = []string{dexIssuerURLVar, oidcIssuerURLVar}

// dexIssuerURLVar is the mcp-oauth resource servers' issuer variable, and
// the managers' `--dex-issuer-url` in its variable spelling; oidcIssuerURLVar
// is oauth2-proxy's.
const (
	dexIssuerURLVar  = "DEX_ISSUER_URL"
	oidcIssuerURLVar = "OIDC_ISSUER_URL"
)

// dexLocalhostTarget is one Deployment the sidecar rule selected and the key
// it selected it by: the dial argument or variable as the Deployment spells
// it, or the annotation.
type dexLocalhostTarget struct {
	deployment, key string
}

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

// devImageTarget is what the dev-image swap of one Deployment component has
// to know about the component chart's render: the Deployment/container whose
// image it replaces and whose pull policy it relaxes, and the image name the
// chart renders there when the render itself cannot be asked (`agentlab
// render`, a component chart that failed to render) — what kustomize matches
// on (the name without tag). `agentlab platform` resolves the live name from
// the render instead (resolveDevImageNames).
type devImageTarget struct {
	deployment, container, image string
}

// devImageTargets maps the Deployment targets of the DevImageComponents to
// their chart render. The fallback names are the charts' at global.registry
// gsoci.azurecr.io; the kagent controller's is the 3.x wrapper's — the kagent
// line publishes it under its own path,
// gsoci.azurecr.io/giantswarm/kagent/controller, which the render says.
var devImageTargets = map[string]devImageTarget{
	componentMuster:        {componentMuster, componentMuster, "gsoci.azurecr.io/giantswarm/muster"},
	componentBackstage:     {componentBackstage, componentBackstage, "gsoci.azurecr.io/giantswarm/backstage"},
	componentKagent:        {"kagent-controller", "controller", "gsoci.azurecr.io/giantswarm/kagent-controller"},
	componentMCPKubernetes: {componentMCPKubernetes, componentMCPKubernetes, "gsoci.azurecr.io/giantswarm/mcp-kubernetes"},
	modelManagerMCPServer:  {modelManagerMCPServer, modelManagerMCPServer, "gsoci.azurecr.io/giantswarm/model-manager"},
	agentManagerMCPServer:  {agentManagerMCPServer, agentManagerMCPServer, "gsoci.azurecr.io/giantswarm/agent-manager"},
	vmManagerMCPServer:     {vmManagerMCPServer, vmManagerMCPServer, "gsoci.azurecr.io/giantswarm/vm-manager"},
	klausGatewayComponent:  {klausGatewayComponent, klausGatewayComponent, "gsoci.azurecr.io/giantswarm/klaus-gateway"},
}

// defaultDevImageNames is the image name per configured Deployment target as
// the table above has it — the names a render without a cluster uses.
func defaultDevImageNames(cfg *config.Config) map[string]string {
	names := map[string]string{}
	for component := range cfg.Platform.DevImages {
		if target, ok := devImageTargets[component]; ok {
			names[component] = target.image
		}
	}
	return names
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

// dexLocalhostPatch is patch 4 for a Deployment: the forwarder as a native
// sidecar — an init container that restarts always, with a startup probe on
// its listen port — so the kubelet starts the server's own container only once
// the forwarder listens (a server dialing the issuer at start would otherwise
// be refused and restart once). An IPv6 wildcard listener is dual-stack on
// Linux (bindv6only=0), so both [::1] — which Go dials first for localhost —
// and 127.0.0.1 answer.
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
      initContainers:
        - name: %s
          image: %s
          restartPolicy: Always
          args:
            - TCP6-LISTEN:%d,fork,reuseaddr
            - TCP:%s
          startupProbe:
            tcpSocket:
              port: %d
            periodSeconds: 1
            failureThreshold: 30
          resources:
            requests:
              cpu: 5m
              memory: 16Mi
`, deployment, dexLocalhostContainer, dexLocalhostImage, dexPort, dexServiceAddr, dexPort)),
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

// slackAPIPatch is patch 6: the component's Slack Web API is the lab's
// Service in front of the proof's fake.
func slackAPIPatch() kustomizePatch {
	return kustomizePatch{
		Target: map[string]string{kindKey: kindDeployment, nameKey: klausGatewayComponent},
		Patch: literalYAML(fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
spec:
  template:
    spec:
      containers:
        - name: %[1]s
          env:
            - name: %[2]s
              value: %[3]s
`, klausGatewayComponent, slackAPIBaseEnv, klausGatewaySlackAPIBase)),
	}
}

// sidecarPostRenderer is the one-patch postRenderers entry of a release the
// lab renders itself (mcp-prometheus.yaml.tmpl).
func sidecarPostRenderer(deployment string, dexPort int) postRenderer {
	var pr postRenderer
	pr.Kustomize.Patches = []kustomizePatch{dexLocalhostPatch(deployment, dexPort)}
	return pr
}

// hostNetworkComponents are the components whose Deployment the lab puts on
// the host network (patch 1+2): they reach the issuer URL as the host does
// and are no sidecar targets.
var hostNetworkComponents = []string{componentMuster, componentBackstage}

// dexLocalhostAddr is the lab Dex as the pods are told it — the host:port of
// the issuer URL (config.Issuer) — the address the sidecar answers on.
func dexLocalhostAddr(cfg *config.Config) string {
	return fmt.Sprintf("%s:%d", localhostName, cfg.DexPort)
}

// dexLocalhostTargets is the rule that picks the sidecar's targets (patch 4)
// off the component renders (keyed by release name, platformImages): every
// Deployment whose containers are told the lab Dex's localhost address
// through a name they dial it by (dexLocalhostKey) has to reach it from
// inside the pod. That is the platform's OAuth contract as a render shows it
// (global.identity forwarded into the pod by the chart), so a manager the
// chart adds is covered the day it is turned on, without a name here. The
// Deployments the lab puts on the host network (hostNetworkComponents) and
// any the chart already runs there reach the address as the host does and
// are left out. Keyed by component, the targets sorted by Deployment name; a
// component without a target is absent. An annotation the rule cannot read
// is the error.
func dexLocalhostTargets(cfg *config.Config, renders map[string]string) (map[string][]dexLocalhostTarget, error) {
	addr := dexLocalhostAddr(cfg)
	targets := map[string][]dexLocalhostTarget{}
	for _, component := range slices.Sorted(maps.Keys(renders)) {
		if slices.Contains(hostNetworkComponents, component) {
			continue
		}
		for _, d := range renderedDeployments(renders[component]) {
			key, ok, err := dexLocalhostKey(addr, d.Spec.Template.Spec.HostNetwork, d.told(), d.Metadata.Annotations, d.Spec.Template.Metadata.Annotations)
			if err != nil {
				return nil, fmt.Errorf("the %s render's Deployment %s: %w", component, d.Metadata.Name, err)
			}
			if ok {
				targets[component] = append(targets[component], dexLocalhostTarget{deployment: d.Metadata.Name, key: key})
			}
		}
		slices.SortFunc(targets[component], func(a, b dexLocalhostTarget) int { return strings.Compare(a.deployment, b.deployment) })
	}
	return targets, nil
}

// dexLocalhostKey is the sidecar rule for one Deployment, the same over a
// render (dexLocalhostTargets) and a live object (platform-test's
// deploymentDexLocalhostKey): a pod off the host network whose containers
// are told the lab Dex's localhost address through a name they dial it by
// (dexDialNames) is a target, keyed by that argument or variable as the
// Deployment spells it; one told the address under any other name — an
// identity, never dialed — is not. The annotation (dexLocalhostAnnotation)
// on the Deployment or its pod template decides instead when it is there,
// and is the key of a target it selects. told yields the containers'
// arguments and environment variables as name/value pairs (toldArgs).
func dexLocalhostKey(addr string, hostNetwork bool, told iter.Seq2[string, string], annotations ...map[string]string) (string, bool, error) {
	if hostNetwork {
		return "", false, nil
	}
	for _, a := range annotations {
		raw, ok := a[dexLocalhostAnnotation]
		if !ok {
			continue
		}
		on, err := strconv.ParseBool(raw)
		if err != nil {
			return "", false, fmt.Errorf("annotation %s=%q is neither true nor false", dexLocalhostAnnotation, raw)
		}
		if on {
			return dexLocalhostAnnotation + "=" + raw, true, nil
		}
		return "", false, nil
	}
	for name, value := range told {
		if slices.Contains(dexDialNames, flagVariable(name)) && strings.Contains(value, addr) {
			return name, true, nil
		}
	}
	return "", false, nil
}

// flagVariable is an argument's flag name in the spelling of the environment
// variable it stands in for — `--dex-issuer-url` is DEX_ISSUER_URL; a
// variable's name is its own.
func flagVariable(name string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimLeft(name, "-"), "-", "_"))
}

// toldArgs yields a container's arguments as name/value pairs: `--name=value`
// split at its first `=`, a bare `--name` paired with the argument after it.
// It reports whether the yield ran to the end.
func toldArgs(args []string, yield func(string, string) bool) bool {
	for i, arg := range args {
		name, value, ok := strings.Cut(arg, "=")
		if !ok && i+1 < len(args) {
			value = args[i+1]
		}
		if !yield(name, value) {
			return false
		}
	}
	return true
}

// told yields every argument (toldArgs) and environment variable of the
// workload's containers as name/value pairs — what the sidecar rule reads.
func (w renderedWorkload) told() iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		for _, c := range w.Spec.Template.Spec.Containers {
			if !toldArgs(c.Args, yield) {
				return
			}
			for _, env := range c.Env {
				if !yield(env.Name, env.Value) {
					return
				}
			}
		}
	}
}

// componentPostRenderers renders the lab's postRenderers list per component
// as indented YAML, keyed by the agent-platform component name, for the
// values template. Components without a patch are absent. imageNames is the
// chart image name each configured Deployment target replaces (resolved from
// the render, or defaultDevImageNames); a configured target absent from it
// renders no override. sidecars is the sidecar's targets per component
// (dexLocalhostTargets) — nil in a render without the component charts
// (`agentlab render`, the values the roster is rendered from), which patches
// no sidecar: `agentlab platform` renders the values once more with them.
func componentPostRenderers(cfg *config.Config, imageNames map[string]string, sidecars map[string][]dexLocalhostTarget) (map[string]string, error) {
	patches := map[string][]kustomizePatch{
		componentKagent: {kagentUINodePortPatch()},
	}
	for _, component := range hostNetworkComponents {
		patches[component] = []kustomizePatch{hostNetworkPatch(component)}
	}
	if cfg.KlausGatewayEnabled() {
		patches[klausGatewayComponent] = append(patches[klausGatewayComponent], slackAPIPatch())
	}
	for _, component := range slices.Sorted(maps.Keys(sidecars)) {
		for _, target := range sidecars[component] {
			patches[component] = append(patches[component], dexLocalhostPatch(target.deployment, cfg.DexPort))
		}
	}
	images := map[string][]kustomizeImage{}
	for _, component := range slices.Sorted(maps.Keys(cfg.Platform.DevImages)) {
		target, ok := devImageTargets[component]
		name := imageNames[component]
		if !ok || name == "" {
			continue
		}
		images[component] = append(images[component], devImageOverride(name, cfg.Platform.DevImages[component]))
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
// (name) with a dev ref: `muster:dev-1a2b` becomes newName localhost/muster +
// newTag dev-1a2b — the name the build is side-loaded under (labImageRef), so
// the pod's ref and the node's copy agree byte for byte.
func devImageOverride(name, ref string) kustomizeImage {
	full := labImageRef(ref)
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

// devImageRefs lists the dev images to side-load into the node under the
// names their pods use (labImageRef): the Deployment targets. The `harness`
// image is not among them — nothing on the node's containerd pulls it
// (pushDevImage).
func devImageRefs(cfg *config.Config) []string {
	var refs []string
	for component, ref := range cfg.Platform.DevImages {
		if component == config.DevImageHarness {
			continue
		}
		refs = append(refs, labImageRef(ref))
	}
	slices.Sort(refs)
	return refs
}

// harnessDevImage is the configured `harness` dev image ref, "" when none.
func harnessDevImage(cfg *config.Config) string {
	return cfg.Platform.DevImages[config.DevImageHarness]
}

// localhostName is the host docker, containerd and atelet treat as a local
// registry without TLS: the lab registry's spelling on the host and in the
// Harness ref, and one of the lab Dex's names (certs.go).
const localhostName = "localhost"

// labImageRef is the name a dev image is side-loaded under and its pod names:
// a ref that names a registry (a dot, a port, or localhost in its first
// component) as it is; a build of this host — `muster:dev-1a2b`,
// `giantswarm/muster:dev` — under localhost/, the namespace podman gives
// local builds and the one no registry answers for. A pod whose side-load
// went missing then fails on `localhost`, where nothing listens, instead of
// asking Docker Hub for a repository of that name, which the registry docker
// implies for a bare ref would have it do. The lab tags the build under that
// name before the side-load (tagLocalDevImages).
func labImageRef(ref string) string {
	first, _, hasPath := strings.Cut(ref, "/")
	if hasPath && (strings.ContainsAny(first, ".:") || first == localhostName) {
		return ref
	}
	return localhostName + "/" + ref
}
