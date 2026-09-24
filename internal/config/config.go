// Package config holds the lab configuration: everything the interactive
// form asks for, persisted to agentlab.yaml so re-runs are reproducible.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// File is the configuration file written next to the binary's working
// directory by `agentlab configure` and read by every other command.
const File = "agentlab.yaml"

// The three lab groups are a fixed vocabulary: RBAC binds exactly these to
// cluster-admin / edit-in-demo / view (see the rbac template).
const (
	groupPlatformAdmins = "platform-admins"
	groupDevelopers     = "developers"
	groupViewers        = "viewers"
)

var Groups = []string{groupPlatformAdmins, groupDevelopers, groupViewers}

// defaultPassword is the throwaway password every default lab user starts
// with; like everything else in the lab's identity, it guards nothing real.
const defaultPassword = "password"

// Static OAuth client IDs and secrets, paired per client. The IDs also appear
// in the templates (Dex staticClients and each consumer's own config), which
// must agree with these. The lab's identity is self-contained and throwaway
// by design; nothing here guards anything real.
const (
	KubernetesClientID     = "kubernetes"
	KubernetesClientSecret = "kubernetes-lab-secret" // #nosec G101 -- static throwaway lab credential, by design
	// The ONE platform client, following the chart's global.identity
	// convention: muster and Backstage authenticate with the same Dex client,
	// so a token minted for either carries an audience the other trusts.
	AgentPlatformClientID     = "agent-platform"
	AgentPlatformClientSecret = "agent-platform-lab-secret" // #nosec G101 -- static throwaway lab credential, by design
)

// MusterNodePort is the port muster's aggregator binds on the kind node
// (hostNetwork). It is the muster chart's default listen port, not a lab
// choice: in-node consumers (Backstage) always dial localhost:8090, while the
// host-side port mapping is configurable via Platform.MusterPort.
const MusterNodePort = 8090

// BrowserCallbackPort is the fixed local port for the OAuth callback of
// `agentlab login --browser` (and its hidden `agentlab browser` alias). Fixed because it must be pre-registered in Dex's redirectURIs.
const BrowserCallbackPort = 5555

// KagentUINodePort is the fixed NodePort the kagent-ui Service is pinned to,
// so the kind port mapping (created once, at cluster creation) has a stable
// node-side port to publish. The kagent chart renders `ui.service.type` but no
// `nodePort` field, so the pin is a Kustomize patch in the kagent component's
// `postRenderers` (HACKS.md U9). A real Kubernetes NodePort, hence the 30000-32767 range; the host side
// is configurable via Platform.AgentsPort.
const KagentUINodePort = 30880

// GatewayNodePort is the fixed NodePort publishing the agentgateway edge
// (the chart-owned Gateway's HTTPS :443 listener) on the kind node. The
// data-plane Service is created by the agentgateway controller at run time —
// not part of the Helm release, so neither values nor a postRenderers patch
// can pin its NodePort. The lab renders its own selector-matched NodePort Service
// instead (gateway-nodeport.yaml.tmpl), pinned here so the kind port mapping
// has a stable node-side port. Host side: Platform.GatewayPort.
const GatewayNodePort = 30443

// GatewayPublicNodePort pins the NodePort of the edge Service's second port
// (platform.gatewayPort, rendered off 443 only) so the apiserver never picks
// one that collides with the other fixed NodePorts. Nothing on the host maps
// it.
const GatewayPublicNodePort = 30444

// PinnedNodePorts are the node-side ports the lab fixes itself. They share the
// node's port space with DexPort, whose Service claims the host port as its
// NodePort, so nothing else may take one of these numbers.
var PinnedNodePorts = []int{MusterNodePort, KagentUINodePort, GatewayNodePort, GatewayPublicNodePort}

// DefaultDexPort is the lab Dex NodePort when agentlab.yaml sets none.
const DefaultDexPort = 32000

// DefaultChartVersion is the agent-platform release the lab installs when
// agentlab.yaml pins none — the release this agentlab was verified with: the
// 4.x line (kagent API v2 with Agent Substrate and the platform Postgres
// shipped by the chart) on the upstream lines' own releases (kagent and
// Substrate `>=1.0.0 <1.1.0`, the agentgateway line's `2.0.0`). Bump
// deliberately, with a lab run: the lab never floats. A candidate must pin
// kagent and Substrate on one Substrate release (`helm show values` of the
// chart: components.kagent's range admits only kagent releases whose worker
// image is the major.minor components.substrate installs) — a chart that
// leaves the kagent range open across a Substrate release boots no golden
// actor (agentlab#187).
const DefaultChartVersion = "4.49.0"

// ChartRepository is where the agent-platform chart releases live.
const ChartRepository = "oci://gsoci.azurecr.io/charts/giantswarm/agent-platform"

// ConnectivityChartName is the meta chart's wiring chart, published off the
// same tag as the meta chart and installed at its exact version
// (components.agent-platform-connectivity.releasedWithChart).
const ConnectivityChartName = "agent-platform-connectivity"

// chartFile is the file that makes a directory a chart.
const chartFile = "Chart.yaml"

// ConnectivityChartDir is where a checkout keeps the connectivity chart next
// to the meta chart directory chartPath names: helm/agent-platform's sibling
// helm/agent-platform-connectivity.
func ConnectivityChartDir(chartPath string) string {
	return filepath.Join(filepath.Dir(chartPath), ConnectivityChartName)
}

// DevImageComponents are the targets platform.devImages can swap: the
// agent-platform chart's component names (its `components.<name>` entries)
// for the Deployments the lab's dev loops build from a checkout, plus
// DevImageHarness for the platform Harness's runtime image.
var DevImageComponents = []string{"muster", "backstage", "kagent", "mcp-kubernetes", "model-manager", "agent-manager", "vm-manager", DevImageKlausGateway, DevImageHarness}

// DevImageKlausGateway is the devImages key of the klaus-gateway component
// (platform.klausGateway): the Deployment `klaus-gateway`, container
// `klaus-gateway`. Meaningful only while the component is on.
const DevImageKlausGateway = "klaus-gateway"

// DevImageHarness is the devImages key of the platform Harness's workload
// image — the Go ADK runtime every agent runs on under kagent API v2. Not a
// Deployment: the connectivity chart renders the image by digest into the
// `Harness` object, and Substrate's atelet pulls it from a registry into its
// own layer cache, so the lab pushes the local build to its registry
// (DevRegistryPort) and forwards the digest through kagent.harness.image.
const DevImageHarness = "harness"

// vmManagerChartFloor is the first agent-platform release whose vm-manager
// component (components.vm-manager) comes from gsoci and fetches its guest
// image as an artifact (vm-manager >= 0.20.2, the guestImage values this lab
// sets; giantswarm/agent-platform#452); 4.11 to 4.14 pinned the ghcr.io chart
// with the node-path inputs.
var vmManagerChartFloor = semver.MustParse("4.15.0")

// servingChartFloor is the first agent-platform release the serving switch
// works on: the llm-d control plane alone (kserve-llmisvc-resources renders
// the shared objects itself, no kserve-crd / kserve-resources), the
// kserve-runtime-configs component, the models Gateway and the discovery
// ConfigMap's spec.gateway. An older release refuses the llm-d controller's
// release without kserve-resources.
var servingChartFloor = semver.MustParse("4.44.0")

// DefaultDevRegistryPort is the host port of the lab registry when
// agentlab.yaml sets none: kind's documented local-registry port.
const DefaultDevRegistryPort = 5001

type User struct {
	Email        string   `yaml:"email"`
	Username     string   `yaml:"username"`
	Name         string   `yaml:"name"`
	Password     string   `yaml:"password"`
	PasswordHash string   `yaml:"passwordHash"` // bcrypt of Password, cached so renders stay deterministic
	Groups       []string `yaml:"groups"`
}

func (u User) HasGroup(g string) bool {
	return slices.Contains(u.Groups, g)
}

type Platform struct {
	Enabled bool `yaml:"enabled"`
	// The agents runtime (kagent), an optional part of the platform install.
	// On real clusters agent delivery runs through Flux/GitOps, which the lab
	// does not run, so labs that are not exercising agents can skip the
	// runtime entirely. Inert when the platform itself is disabled.
	Agents bool `yaml:"agents"`
	// A minimal observability stack: the Giant Swarm kube-prometheus-stack
	// chart (the observability bundle's own pinned constituent, with the
	// Prometheus server re-enabled — the bundle itself is MC-shaped: Alloy
	// remote-writing to Mimir, no local query endpoint) plus mcp-prometheus
	// registered in muster, so agents can answer PromQL questions
	// (x_mcp-prometheus_<tool>). On by default: asking the platform about the
	// cluster's CPU/memory is part of the demo story. Not in the umbrella's
	// BOM (yet) — the lab pins the two charts itself (observability.go).
	// Inert when the platform itself is disabled.
	Observability bool `yaml:"observability"`
	// The fake fleet (lab/fleetfixture.go): six `Auth Required` MCPServers
	// (kubernetes/capi/prometheus × two fake management clusters) that give
	// the portal's server groups, its fleet coverage and the agent Tools
	// step a federated shape to render on one cluster. Off by default: a
	// default lab lists only the MCP servers of the lab that runs it, and
	// the fixture's rows ask a person to sign in to clusters that do not
	// exist. Switching it off removes the members on the next `agentlab
	// platform`; the proofs assert the fleet shape only while it is on.
	// Inert when the platform itself is disabled.
	FakeFleet bool `yaml:"fakeFleet"`
	// Host-side port for the kagent UI (http://localhost:<port>). The kind
	// mapping onto KagentUINodePort always exists — like the other mappings,
	// it is fixed at cluster creation — so agents can be enabled later.
	AgentsPort int `yaml:"agentsPort"`
	// Host-side port for muster's DIRECT debug access (hostNetwork + kind
	// mapping, bypassing the gateway). The platform's public URLs go through
	// the agentgateway edge on GatewayPort.
	MusterPort int `yaml:"musterPort"`
	// The platform's public domain. Every public hostname derives from it
	// (muster.<domain>, backstage.<domain>, ...). The default, nip.io's
	// loopback wildcard, resolves to 127.0.0.1 from anywhere without host
	// configuration; inside pods a CoreDNS rewrite points the same names at
	// the edge Gateway Service.
	Domain string `yaml:"domain"`
	// Host-side port of the agentgateway edge (HTTPS). 443 keeps the public
	// URLs port-free; any other value suffixes every public URL with it, and
	// the lab's edge Service (gateway-nodeport.yaml.tmpl) then also serves
	// that port in-cluster, so the ported URLs resolve from pods too. Change
	// only if 443 is taken.
	GatewayPort int `yaml:"gatewayPort"`
	// TLS optionally hands the edge an externally provisioned certificate
	// pair (PEM) instead of the minted lab-CA wildcard — for users who own a
	// real domain (wildcard record -> 127.0.0.1) and run their own ACME
	// tooling. Both fields set or both empty. The Dex issuer still serves
	// the lab CA either way; see docs/tls.md.
	TLS PlatformTLS `yaml:"tls"`
	// The agent-platform chart release the lab installs
	// (oci://gsoci.azurecr.io/charts/giantswarm/agent-platform): an exact
	// version, never a range — two runs install the same thing, and the lab
	// never floats onto a release nobody tested it with. DefaultChartVersion
	// is the release this agentlab was verified against.
	ChartVersion string `yaml:"chartVersion"`
	// ChartPath installs the meta chart from a local directory instead of the
	// pinned release — an agent-platform checkout's helm/agent-platform, for
	// chart changes that have no release yet (the lab's chart loop). The
	// checkout's connectivity chart beside it (ConnectivityChartDir) is
	// installed with it: the meta chart pins that component to its own
	// version, which no registry publishes for a checkout, so the lab pushes
	// the sibling into the lab registry and points the meta chart there
	// (lab/connectivity.go). Both directories are read, never written;
	// chartVersion is ignored while set.
	ChartPath string `yaml:"chartPath,omitempty"`
	// ChartBranch selects the DEV CHANNEL: the lab follows the newest dev
	// build of this agent-platform branch — the `X.Y.Z-r<branch-hash>t
	// <YYYYMMDDHHMMSS>h<sha7>` prerelease tags gitsemver 3 publishes for every
	// commit of a branch with branch publishing on (BranchHash; builds made
	// before 2026-09-23 carry the superseded `X.Y.Z-dev.<branch>.<date>.
	// <time>.h<sha>` shape, which is read too) — instead of a release. `configure`,
	// `up` and `platform` resolve it against the chart registry's tags and
	// write the tag they picked into chartVersion, so `render`, the image
	// preload and a re-run install exactly what was resolved, and the boot
	// says which build it runs. Mutually exclusive with chartPath; "" is the
	// stable channel. See docs/platform.md "Dev channel".
	ChartBranch string `yaml:"chartBranch,omitempty"`
	// ChartPinned freezes chartVersion at the recorded dev build while
	// chartBranch is set: `up` and `platform` stop re-resolving, so the lab
	// keeps running the build under test until `agentlab platform --pin=false`
	// (or the key is dropped). Meaningless without chartBranch.
	ChartPinned bool `yaml:"chartPinned,omitempty"`
	// DevImages swaps a component's image for a build of your own (the lab's
	// dev-image loop): target -> image ref (`muster: muster:dev-1a2b`). Keys
	// are the DevImageComponents. For a Deployment target `agentlab platform`
	// side-loads the ref from the host docker cache, resolves the image name
	// it replaces from the component chart's render (the kagent controller is
	// `gsoci.azurecr.io/giantswarm/kagent/controller` on the 4.x line, the
	// path the kagent line publishes under, and
	// `gsoci.azurecr.io/giantswarm/kagent-controller` on 3.x — a target that
	// matches nothing in the render is an error before the install, never a
	// silently dropped override) and renders it into the component's
	// HelmRelease as a kustomize image override (postRenderers) with
	// imagePullPolicy IfNotPresent. For the `harness` target it pushes the
	// build to the lab registry and forwards the digest as
	// kagent.harness.image, so the platform Harness runs it and recompiles
	// every admitted template. Either way the swap is part of the release — a
	// plain `helm upgrade` applies it, and removing the entry restores the
	// chart's image (or digest) on the next run.
	DevImages map[string]string `yaml:"devImages,omitempty"`
	// DevRegistryPort is the host port (127.0.0.1) of the lab registry that
	// serves the `harness` dev image: a `registry` container on the kind
	// docker network, created on demand by `agentlab platform` and removed by
	// `agentlab down`. Unset means DefaultDevRegistryPort.
	DevRegistryPort int `yaml:"devRegistryPort,omitempty"`
	// ValuesFiles are extra Helm values files merged over the lab's rendered
	// values before the meta chart install, in order, with `helm -f`
	// semantics (maps merge, lists replace, the later file wins): a lab that
	// points a component at another chart source
	// (components.<name>.repository / versionRange / insecure) or forwards
	// values the lab template does not know. Paths are absolute or relative
	// to the lab directory; each must exist.
	ValuesFiles []string `yaml:"valuesFiles,omitempty"`
	// Additional kagent ModelConfigs beyond the chart-rendered default
	// (aiModel): self-hosted OpenAI-compatible endpoints (vLLM, Ollama),
	// OpenRouter, Gemini, plain OpenAI. Rendered as lab-labeled ModelConfig
	// CRs by `agentlab platform`; entries removed here are pruned on the next
	// run. Inert unless agents are enabled.
	ExtraModels []ExtraModel `yaml:"extraModels,omitempty"`
	// Managed models: the chart's model-manager component in front of the
	// model servers that run on the lab host (backends.go is the list, with
	// each server's port and name) — pull, load,
	// unload and delete models from the portal (or as x_model-manager_*
	// tools through muster), each pulled model wired into kagent as a
	// keyless ModelConfig automatically. Complements extraModels, which
	// wires endpoints statically and manages nothing. Requires agents.
	// `agentlab configure` fills the backends from what answers on this
	// machine, on every run.
	ModelManager ModelManager `yaml:"modelManager"`
	// The platform's VM provisioner (github.com/giantswarm/vm-manager) as a
	// pod of the kind node: the chart's components.vm-manager, turned on by
	// the lab's values (x_vm-manager_<tool> through muster, tool group
	// agent-platform, forward-token auth against the lab Dex). The node is a
	// privileged container, so the host's /dev/kvm and /dev/vhost-vsock are
	// in it for the pod. The guest image is the artifact the chart's release
	// published, or a local build pushed into the lab registry (imageDir).
	// `agentlab configure` turns it off on a machine without the devices;
	// --vm-manager turns it on. A build of the checkout swaps in through
	// devImages.
	VMManager VMManager `yaml:"vmManager"`
	// Swarmgeist (github.com/giantswarm/klaus-gateway) as the meta chart's
	// in-cluster component (components.klaus-gateway), the way every
	// installation runs it: A2A to the kagent controller over the in-cluster
	// agentgateway target, the Slack adapter on a placeholder credentials
	// Secret (no workspace answers it; the lab points its Web API at
	// klaus-gateway-test's fake), and the OBO link store as a Kubernetes
	// Secret (obo.store: secret) with keys the lab generates once. Off by
	// default; --klaus-gateway turns it on; needs the agents runtime. A build
	// of the checkout swaps in through devImages. `agentlab
	// klaus-gateway-test` proves it next to the host-mode gateway
	// (klausgateway.go).
	KlausGateway KlausGateway `yaml:"klausGateway"`
	// Model serving on llm-d: the serving slice of a Giant Swarm installation
	// on the kind node (internal/lab/serving.go) — the chart's KServe llmisvc
	// controller with its CRDs (components.kserve-llmisvc-crd and
	// -resources), the well-known LLMInferenceServiceConfigs it composes
	// from (components.kserve-runtime-configs), the connectivity chart's
	// serving objects behind components.modelServing (the serving namespace,
	// the published presets and their discovery ConfigMap, the models
	// Gateway with its JWT policy against the lab Dex) and model-manager's
	// kserve backend, which composes a preset into an LLMInferenceService.
	// cert-manager comes with it: the controller's webhook certificate is a
	// cert-manager Certificate. The node has no GPU, so the lab publishes
	// one preset of its own — a small instruct model on the llm-d CPU
	// runtime — and model-manager places it on the node's CPU capacity. Off
	// by default; --serving turns it on; needs the agents runtime, which the
	// served model is wired into. `agentlab serving-test` is the proof.
	Serving Serving `yaml:"serving"`
}

// Serving configures model serving on llm-d in the lab.
type Serving struct {
	// On, `agentlab platform` turns on the chart's kserve-llmisvc-crd,
	// kserve-llmisvc-resources and kserve-runtime-configs components and the
	// modelServing switch with the lab's serving values (the values
	// template's `modelServing:` block: the lab preset, the models Gateway),
	// adds the kserve backend to the list model-manager fronts and installs
	// cert-manager before the chart.
	Enabled bool `yaml:"enabled"`
}

// KlausGateway configures the chart's klaus-gateway component in the lab.
type KlausGateway struct {
	// On, `agentlab platform` enables components.klaus-gateway with the
	// lab's values (the values template's `klausGateway:` block) and creates
	// the two Secrets the component reads: the placeholder Slack
	// credentials and the OBO keys.
	Enabled bool `yaml:"enabled"`
}

// VMManager configures the chart's vm-manager component in the lab.
type VMManager struct {
	// On, `agentlab platform` enables components.vm-manager: the pod with
	// the node's KVM devices, OAuth against the lab Dex and the muster
	// registration the chart renders. Refused on a machine without /dev/kvm
	// and /dev/vhost-vsock (`agentlab configure` turns it off there).
	Enabled bool `yaml:"enabled"`
	// A local guest image build the pod boots instead of the artifact the
	// chart's release published — a vm-manager checkout's images/build
	// after `make -C images`: the base image, its UKI, the Kubernetes sysext
	// layers and policy.json. `agentlab platform` pushes it into the lab
	// registry and pins the chart to its digest, so a rebuilt image rolls
	// the pod on the next run. Empty: the release's guest image. An absolute
	// path, or relative to the lab directory.
	ImageDir string `yaml:"imageDir,omitempty"`
}

// Validate checks the vm-manager block: the image directory, when set, is a
// directory that exists.
func (v VMManager) Validate() error {
	if v.ImageDir == "" {
		return nil
	}
	st, err := os.Stat(v.ImageDir)
	switch {
	case err != nil:
		return fmt.Errorf("imageDir: %w (a vm-manager checkout's images/build after `make -C images`)", err)
	case !st.IsDir():
		return fmt.Errorf("imageDir %q: not a directory", v.ImageDir)
	}
	return nil
}

// ApplyDiscovered follows what `agentlab configure` found: a machine without
// the KVM devices cannot run the pod, so the key goes off there with the
// reason; a machine with them keeps what the file says (the pod is a heavier
// piece of the lab than a model server, so it is never turned on by itself);
// pinEnabled (--vm-manager) decides instead.
func (v *VMManager) ApplyDiscovered(kvm bool, pinEnabled *bool) {
	switch {
	case pinEnabled != nil:
		v.Enabled = *pinEnabled
	case !kvm:
		v.Enabled = false
	}
}

// ModelManager configures the umbrella's model-manager component in the lab.
type ModelManager struct {
	// On, `agentlab platform` enables components.model-manager in front of
	// every listed backend, its agentgateway route (JWT-validated: the portal
	// backend forwards the user's Dex token) and the muster registration.
	// `agentlab configure` turns it on whenever a host model server answers
	// (and off when none does), unless --model-manager pins it.
	Enabled bool `yaml:"enabled"`
	// The host model servers, in order; backends.go names them and owns the
	// list, so this comment cannot go stale as servers are added. ONE
	// model-manager fronts all of them at once (model-manager >= 0.17.0,
	// `model-manager.backends` in the chart values); the first entry is its
	// default backend — where a request that names none goes. `agentlab
	// configure` fills the list from what answers on this machine and can be
	// reached from pods (Ollama first); --model-manager-backends pins it.
	// kserve is not a host server: the serving switch (Platform.Serving)
	// adds it to the list the chart's model-manager fronts (ChartBackends).
	Backends []string `yaml:"backends,omitempty"`
	// Per-backend base URL as pods reach it, keyed by backend. Empty
	// autodetects http://<kind docker network gateway>:<default port> at
	// platform time (BackendPort names each server's) — the same
	// address docs/models.md documents for extraModels (`docker network inspect
	// kind`). Set one for a server elsewhere on the LAN: a backend with an
	// endpoint here is kept by `agentlab configure` whether or not a server
	// answers on this machine.
	Endpoints map[string]string `yaml:"endpoints,omitempty"`
	// The one-backend form earlier versions wrote (backend + endpoint):
	// still read, folded into backends/endpoints on load, never written.
	Backend  string `yaml:"backend,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`
}

// Primary is model-manager's default backend — where a request that names
// none goes: the first of the list, or the historical default (an Ollama)
// for an enabled block that names none.
func (m ModelManager) Primary() string {
	if len(m.Backends) == 0 {
		return ModelManagerBackendOllama
	}
	return m.Backends[0]
}

// EndpointFor is the configured endpoint override of a backend, "" to
// autodetect.
func (m ModelManager) EndpointFor(backend string) string {
	return m.Endpoints[backend]
}

// normalize folds the legacy one-backend form into the lists, orders the
// backends canonically and gives an enabled block without backends the
// historical default (an Ollama), so files written by earlier versions read
// exactly as before.
func (m *ModelManager) normalize() {
	if m.Backend != "" {
		if !slices.Contains(m.Backends, m.Backend) {
			m.Backends = append([]string{m.Backend}, m.Backends...)
		}
		if m.Endpoint != "" {
			if m.Endpoints == nil {
				m.Endpoints = map[string]string{}
			}
			if _, set := m.Endpoints[m.Backend]; !set {
				m.Endpoints[m.Backend] = m.Endpoint
			}
		}
	}
	m.Backend, m.Endpoint = "", ""
	if m.Enabled && len(m.Backends) == 0 {
		m.Backends = []string{ModelManagerBackendOllama}
	}
	if len(m.Endpoints) == 0 {
		m.Endpoints = nil
	}
}

// ApplyDiscovered merges what `agentlab configure` found on this machine into
// the block: the backends are the servers that answer plus every backend kept
// by an explicit endpoint (a server elsewhere on the LAN), in canonical order;
// managed models go on when there is at least one and the agents runtime is
// on, and off otherwise. pinBackends (--model-manager-backends) replaces the
// list outright; pinEnabled (--model-manager) decides the flag instead of the
// discovery — a pinned-on block without a backend falls back to the Ollama
// default, so the platform preflight reports the real reachability error.
func (m *ModelManager) ApplyDiscovered(found []string, agents bool, pinEnabled *bool, pinBackends []string) {
	m.normalize()
	var backends []string
	switch {
	case pinBackends != nil:
		backends = slices.Clone(pinBackends)
	default:
		for _, b := range ModelManagerBackends {
			if slices.Contains(found, b) || m.Endpoints[b] != "" {
				backends = append(backends, b)
			}
		}
	}
	m.Backends = backends
	for b := range m.Endpoints {
		if !slices.Contains(m.Backends, b) {
			delete(m.Endpoints, b)
		}
	}
	if len(m.Endpoints) == 0 {
		m.Endpoints = nil
	}
	if pinEnabled != nil {
		m.Enabled = *pinEnabled
	} else {
		m.Enabled = agents && len(m.Backends) > 0
	}
	if m.Enabled && len(m.Backends) == 0 {
		m.Backends = []string{ModelManagerBackendOllama}
	}
}

// ExtraModel is one additional kagent ModelConfig. API keys are NOT config:
// APIKeyEnv only names the host env var read at deploy time — the value lands
// in the Secret kagent-<name>, never in this file or in rendered state/.
type ExtraModel struct {
	// ModelConfig CR name; also names the key Secret kagent-<name>.
	Name string `yaml:"name"`
	// One of ModelProviders (a subset of the kagent CRD's enum: the providers
	// expressible with just a model + base URL).
	Provider string `yaml:"provider"`
	// The provider's model id (what the endpoint serves, e.g. `qwen3-8-27b`
	// for a local vLLM or `deepseek/deepseek-chat` on OpenRouter).
	Model string `yaml:"model"`
	// Endpoint override. Required for Ollama (the in-cluster default would be
	// useless), optional for OpenAI/Anthropic (any compatible endpoint:
	// vLLM `http://host:8000/v1`, OpenRouter `https://openrouter.ai/api/v1`),
	// not applicable to Gemini.
	BaseURL string `yaml:"baseUrl,omitempty"`
	// Host env var holding the API key at deploy time. Empty means a keyless
	// endpoint: the Secret is still created with a placeholder, because the
	// kagent ADK runtime requires the provider's env var to exist.
	APIKeyEnv string `yaml:"apiKeyEnv,omitempty"`
	// Skip TLS verification on the provider connection (ModelConfig spec.tls.
	// disableVerify) — for self-hosted endpoints with self-signed certs.
	InsecureTLS bool `yaml:"insecureTLS,omitempty"`
	// OpenAI provider only: the ModelConfig's openAI.reasoningEffort, sent as
	// reasoning_effort with every call. `none` switches a thinking model's
	// reasoning off on Ollama's /v1 alias.
	ReasoningEffort string `yaml:"reasoningEffort,omitempty"`
	// Ollama provider only: the ModelConfig's ollama.think, sent as the chat
	// request's think field. false switches a thinking model's reasoning off
	// on kagent's native Ollama provider (docs/models.md "Agent proofs without
	// an Anthropic key"); unset leaves Ollama's default, under which a model
	// with the thinking capability thinks.
	Think *bool `yaml:"think,omitempty"`
}

// The provider vocabulary for extra models, spelled exactly as the kagent
// ModelConfig CRD's provider enum spells them.
const (
	ProviderOpenAI    = "OpenAI"
	ProviderAnthropic = "Anthropic"
	ProviderGemini    = "Gemini"
	ProviderOllama    = "Ollama"
)

// ModelProviders maps each provider to the key name inside the Secret. The
// kagent controller injects that key as an env var of the same name into
// agent pods, and the ADK runtime looks up exactly these canonical names —
// so the key name is provider-derived, not configurable. Ollama is keyless
// (empty key = no Secret).
var ModelProviders = map[string]string{
	ProviderOpenAI:    "OPENAI_API_KEY",
	ProviderAnthropic: "ANTHROPIC_API_KEY",
	ProviderGemini:    "GOOGLE_API_KEY",
	ProviderOllama:    "",
}

// ModelProviderNames is ModelProviders' keys in a stable order, for the form
// options and error messages.
var ModelProviderNames = []string{ProviderOpenAI, ProviderAnthropic, ProviderGemini, ProviderOllama}

// ReasoningEfforts is the ModelConfig CRD's enum for openAI.reasoningEffort on
// the kagent line (kagent.dev/v1alpha3).
var ReasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// SecretName is the Kubernetes Secret (in ns kagent) holding this model's key.
func (m ExtraModel) SecretName() string { return "kagent-" + m.Name }

// SecretKey is the key inside the Secret — the provider's canonical env var
// name. Empty for keyless providers (no Secret attached at all).
func (m ExtraModel) SecretKey() string { return ModelProviders[m.Provider] }

// NeedsSecret reports whether this model's ModelConfig references a Secret.
func (m ExtraModel) NeedsSecret() bool { return m.SecretKey() != "" }

// PlatformTLS is an externally provisioned certificate for the edge; see
// Platform.TLS.
type PlatformTLS struct {
	CertFile string `yaml:"certFile,omitempty"`
	KeyFile  string `yaml:"keyFile,omitempty"`
}

// Set reports whether an external edge certificate is configured.
func (t PlatformTLS) Set() bool { return t.CertFile != "" }

type Backstage struct {
	Enabled bool `yaml:"enabled"`
	// Backstage binds this port on the node (hostNetwork) and kind maps the
	// same number onto the host, so the URL is identical on both sides.
	// The image is not configured here: the agent-platform chart's backstage
	// component (its version range, resolved by the chart's Flux) decides the
	// version; platform.devImages swaps in a build of your own.
	Port int `yaml:"port"`
}

type Config struct {
	// Kind cluster name; also prefixes RBAC bindings and names the muster
	// installation surfaced in Backstage.
	ClusterName string `yaml:"clusterName"`
	// Dex NodePort == host port: the issuer https://localhost:<port>/dex must
	// be the same URL from the Mac and from inside the node, so both sides
	// use one number. Must sit in the NodePort range (30000-32767).
	DexPort  int    `yaml:"dexPort"`
	DexImage string `yaml:"dexImage"`

	// The Claude model both AI consumers use: the platform agents' default
	// ModelConfig (kagent) and Backstage's ai-chat. The API key is NOT config:
	// it is read from $ANTHROPIC_API_KEY at deploy time and lands only in
	// Kubernetes Secrets, never in this file or in rendered manifests.
	AIModel string `yaml:"aiModel"`

	Users     []User    `yaml:"users"`
	Platform  Platform  `yaml:"platform"`
	Backstage Backstage `yaml:"backstage"`
}

// Default returns the canonical lab setup: the agent platform enabled (it is
// what the lab exists to test) with the agentgateway edge and Backstage — the
// real platform topology — three users, and Dex on 32000.
func Default() *Config {
	return &Config{
		ClusterName: "agentlab",
		DexPort:     DefaultDexPort,
		// groups on staticPasswords requires Dex >= v2.45.0 (docs/identity.md);
		// the gsoci mirror of dexidp/dex, digest-identical to upstream's.
		DexImage: "gsoci.azurecr.io/giantswarm/dex:v2.45.1",
		// The agent-platform BOM's own default, and the newest model the
		// pinned Backstage build's thinking-mode handling is known to cover.
		AIModel: "claude-sonnet-4-6",
		Users: []User{
			{Email: "admin@lab.local", Username: "admin", Name: "Lab Admin", Password: defaultPassword,
				Groups: []string{groupPlatformAdmins, groupDevelopers}},
			{Email: "dev@lab.local", Username: "dev", Name: "Lab Developer", Password: defaultPassword,
				Groups: []string{groupDevelopers}},
			{Email: "viewer@lab.local", Username: "viewer", Name: "Lab Viewer", Password: defaultPassword,
				Groups: []string{groupViewers}},
		},
		Platform: Platform{
			Enabled:       true,
			Agents:        true,
			Observability: true,
			AgentsPort:    8081,
			MusterPort:    8090,
			Domain:        "127.0.0.1.nip.io",
			GatewayPort:   443,
			ChartVersion:  DefaultChartVersion,
			// The lab registry behind the `harness` dev image; a container
			// on the kind network, so no node port mapping is involved.
			DevRegistryPort: DefaultDevRegistryPort,
		},
		Backstage: Backstage{
			Enabled: true,
			Port:    7007,
		},
	}
}

// Load reads agentlab.yaml. A missing file is reported as os.ErrNotExist so
// callers can decide whether to fall back to the form or to defaults.
func Load() (*Config, error) {
	raw, err := os.ReadFile(File)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	if err := decodeStrict(raw, cfg); err != nil {
		return nil, err
	}
	// Earlier versions wrote the one-backend form (backend/endpoint); read
	// it as the one-item lists so the same lab renders exactly as before.
	cfg.Platform.ModelManager.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	changed, err := cfg.EnsureHashes()
	if err != nil {
		return nil, err
	}
	if changed {
		if err := cfg.write(); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// decodeStrict reads agentlab.yaml into cfg and refuses a field this release
// does not know. Every run writes the file back (Load re-hashes passwords,
// configure and platform record what they found), so a lenient decode would
// drop such a field silently and the next `platform` would uninstall what it
// configured — a newer release's switch, say. The refusal names the field and
// the fix: update agentlab, or remove the field. An empty file is the defaults.
func decodeStrict(raw []byte, cfg *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	err := dec.Decode(cfg)
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return nil
	case strings.Contains(err.Error(), "not found in type"):
		return fmt.Errorf("%s names a field this agentlab release does not know (%w) — the file was written by a newer release, and this run would drop the field on its next write; update agentlab (https://github.com/giantswarm/agentlab/releases) or remove the field", File, err)
	default:
		return fmt.Errorf("parsing %s: %w", File, err)
	}
}

func (c *Config) Save() error {
	if _, err := c.EnsureHashes(); err != nil {
		return err
	}
	return c.write()
}

// write persists the config as-is; Save and Load ensure the hashes first
// (exactly once — bcrypt-comparing every user is not free).
func (c *Config) write() error {
	out, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := []byte("# agentlab lab configuration. Regenerate with `agentlab configure`.\n" +
		"# Passwords are lab-only throwaway credentials; the hash is cached so\n" +
		"# rendered manifests stay byte-identical across runs (no spurious pod rolls).\n")
	return os.WriteFile(File, append(header, out...), 0o600)
}

// EnsureHashes fills in User.PasswordHash wherever it is missing or no longer
// matches the password. The hash is cached in the config file on purpose:
// bcrypt salts are random, so hashing at render time would change the Dex
// manifest (and roll the pod) on every single run.
func (c *Config) EnsureHashes() (changed bool, err error) {
	for i := range c.Users {
		u := &c.Users[i]
		if u.PasswordHash != "" &&
			bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(u.Password)) == nil {
			continue
		}
		h, err := bcrypt.GenerateFromPassword([]byte(u.Password), 10)
		if err != nil {
			return false, fmt.Errorf("hashing password for %s: %w", u.Email, err)
		}
		u.PasswordHash = string(h)
		changed = true
	}
	return changed, nil
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidateClusterName is the one home of the cluster-name rule; the huh form
// uses it directly as an input validator.
func ValidateClusterName(s string) error {
	if !nameRe.MatchString(s) {
		return fmt.Errorf("lowercase alphanumeric and dashes only")
	}
	return nil
}

// ValidateAIModel constrains the DEFAULT model to Anthropic: both consumers
// (the kagent ModelConfig provider and Backstage's ai-chat prefix routing)
// are wired for Anthropic only, keyed by the one $ANTHROPIC_API_KEY secret.
// Other providers go through platform.extraModels instead.
func ValidateAIModel(s string) error {
	if !strings.HasPrefix(s, "claude-") {
		return fmt.Errorf("must be a claude-* model (other providers go in platform.extraModels)")
	}
	return nil
}

// reservedModelNames collide with what the kagent chart / the lab already
// own: the chart-rendered default ModelConfig, and the name whose Secret
// (kagent-anthropic) the default ModelConfig references.
var reservedModelNames = []string{"default-model-config", "anthropic"}

// ValidateModelName is the one home of the extra-model-name rule; the huh
// form uses it directly as an input validator.
func ValidateModelName(s string) error {
	if !nameRe.MatchString(s) {
		return fmt.Errorf("lowercase alphanumeric and dashes only")
	}
	if slices.Contains(reservedModelNames, s) {
		return fmt.Errorf("%q is reserved (the default ModelConfig and its Secret)", s)
	}
	return nil
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateAPIKeyEnv accepts an env var name or empty (keyless endpoint).
func ValidateAPIKeyEnv(s string) error {
	if s != "" && !envNameRe.MatchString(s) {
		return fmt.Errorf("not an environment variable name")
	}
	return nil
}

// Validate checks one extra model entry; string-typed like the port
// validators where the form shares it, entry-level here.
func (m ExtraModel) Validate() error {
	if err := ValidateModelName(m.Name); err != nil {
		return fmt.Errorf("name %q: %w", m.Name, err)
	}
	key, known := ModelProviders[m.Provider]
	if !known {
		return fmt.Errorf("%s: unknown provider %q (one of %s)", m.Name, m.Provider, strings.Join(ModelProviderNames, ", "))
	}
	if m.Model == "" {
		return fmt.Errorf("%s: model is required", m.Name)
	}
	if err := ValidateAPIKeyEnv(m.APIKeyEnv); err != nil {
		return fmt.Errorf("%s: apiKeyEnv %q: %w", m.Name, m.APIKeyEnv, err)
	}
	switch m.Provider {
	case ProviderGemini:
		if m.BaseURL != "" {
			return fmt.Errorf("%s: Gemini takes no baseUrl (the ModelConfig CRD has no endpoint field for it)", m.Name)
		}
	case ProviderOllama:
		if m.BaseURL == "" {
			return fmt.Errorf("%s: Ollama requires baseUrl (the host serving the API, e.g. http://192.168.1.10:11434)", m.Name)
		}
	}
	if key == "" && m.APIKeyEnv != "" {
		return fmt.Errorf("%s: %s is keyless — apiKeyEnv would be silently ignored", m.Name, m.Provider)
	}
	if m.Think != nil && m.Provider != ProviderOllama {
		return fmt.Errorf("%s: think applies to the Ollama provider only (the ModelConfig's ollama.think)", m.Name)
	}
	if m.ReasoningEffort != "" {
		if m.Provider != ProviderOpenAI {
			return fmt.Errorf("%s: reasoningEffort applies to the OpenAI provider only (the ModelConfig's openAI.reasoningEffort)", m.Name)
		}
		if !slices.Contains(ReasoningEfforts, m.ReasoningEffort) {
			return fmt.Errorf("%s: unknown reasoningEffort %q (one of %s)", m.Name, m.ReasoningEffort, strings.Join(ReasoningEfforts, ", "))
		}
	}
	return nil
}

// Normalize applies cross-field implications after an entry point sets the
// enable flags: Backstage's muster plugin is the reason to run it, so it
// implies the platform. Validate stays the backstop for hand-edited files.
func (c *Config) Normalize() {
	if c.Backstage.Enabled {
		c.Platform.Enabled = true
	}
	// A file written by an earlier agentlab (or with the key emptied) pins
	// nothing; the default pin is the release this binary was verified with.
	if c.Platform.ChartVersion == "" {
		c.Platform.ChartVersion = DefaultChartVersion
	}
	if len(c.Platform.DevImages) == 0 {
		c.Platform.DevImages = nil
	}
	if c.Platform.DevRegistryPort == 0 {
		c.Platform.DevRegistryPort = DefaultDevRegistryPort
	}
	if len(c.Platform.ValuesFiles) == 0 {
		c.Platform.ValuesFiles = nil
	}
	// A pin only means something on the dev channel; a stable lab has
	// nothing to freeze.
	if c.Platform.ChartBranch == "" {
		c.Platform.ChartPinned = false
	}
	c.Platform.ModelManager.normalize()
}

// branchSanitizeRe is gitsemver's: every run of characters outside [a-z0-9]
// becomes one hyphen, so "--" never occurs in a sanitized name and stays
// free as gitsemver's truncation marker.
var branchSanitizeRe = regexp.MustCompile(`[^a-z0-9]+`)

// SanitizeBranch spells a git branch the way gitsemver 2 (sanitizeBranchName)
// embedded it in the superseded dev shape `X.Y.Z-dev.<branch>.<date>.<time>.
// h<sha>`, which the registry still holds: lowercased, runs of anything but
// [a-z0-9] collapsed to one hyphen, hyphens trimmed off both ends —
// `poc/kagent-main` is `poc-kagent-main`. A purely numeric result loses its
// leading zeros (a semver numeric identifier forbids them); an empty result
// is gitsemver's "unknown". gitsemver 2 may further have shortened the name
// to fit its 63-character version budget (head`--`tail), which the dev-tag
// filter accounts for. gitsemver 3 embeds BranchHash instead.
func SanitizeBranch(branch string) string {
	b := strings.Trim(branchSanitizeRe.ReplaceAllString(strings.ToLower(branch), "-"), "-")
	if b == "" {
		return "unknown"
	}
	if allDigits(b) {
		if b = strings.TrimLeft(b, "0"); b == "" {
			b = "0"
		}
	}
	return b
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// BranchHash is gitsemver 3's branch fingerprint, the `r<hash>` field of a
// dev version: the CRC-32/ISO-HDLC checksum (Go's crc32.ChecksumIEEE) of the
// full, unsanitized branch name as eight lowercase hex digits — what
// `gitsemver branch-hash <branch>` prints. `main` is bf28cd64, `giantswarm`
// 588f3d76.
func BranchHash(branch string) string {
	return fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(branch)))
}

// ValidateChartBranch accepts a branch name that leaves something to match
// a dev tag with; "" is the stable channel and always fine.
func ValidateChartBranch(s string) error {
	if s == "" {
		return nil
	}
	if strings.TrimSpace(s) != s {
		return fmt.Errorf("must not have surrounding whitespace")
	}
	if SanitizeBranch(s) == "unknown" {
		return fmt.Errorf("must contain a letter or digit (a git branch name has one; the superseded dev tags carry the branch as a lowercase [a-z0-9-] name)")
	}
	return nil
}

// exactVersionRe is a plain semver version (an optional leading v tolerated):
// what platform.chartVersion must be — a range would let the lab float.
var exactVersionRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// ValidateChartVersion accepts an exact chart version only.
func ValidateChartVersion(s string) error {
	if s == "" {
		return fmt.Errorf("required: the agent-platform release to install (an exact version, e.g. %s)", DefaultChartVersion)
	}
	if !exactVersionRe.MatchString(s) {
		return fmt.Errorf("must be an exact version (e.g. %s), not a range — the lab never floats", DefaultChartVersion)
	}
	return nil
}

// imageRefRe is a container image reference with an explicit tag or digest:
// [registry/][path/]name:tag or @sha256:…. A bare name would resolve to
// `latest` and hide which build the lab runs.
var imageRefRe = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)*(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|@sha256:[a-f0-9]{64})$`)

// ValidateImageRef accepts an image reference that names its tag or digest.
func ValidateImageRef(s string) error {
	if !imageRefRe.MatchString(s) {
		return fmt.Errorf("must be an image reference with a tag or digest, e.g. muster:dev-1a2b3c")
	}
	return nil
}

func (c *Config) Validate() error {
	if err := ValidateClusterName(c.ClusterName); err != nil {
		return fmt.Errorf("cluster name %q: %w", c.ClusterName, err)
	}
	if err := ValidateNodePort(strconv.Itoa(c.DexPort)); err != nil {
		return fmt.Errorf("dexPort: %w", err)
	}
	if slices.Contains(PinnedNodePorts, c.DexPort) {
		return fmt.Errorf("dexPort: %d is a NodePort the lab already pins, and the apiserver rejects the second Service that claims it", c.DexPort)
	}
	if err := ValidateAIModel(c.AIModel); err != nil {
		return fmt.Errorf("aiModel %q: %w", c.AIModel, err)
	}
	if len(c.Users) == 0 {
		return fmt.Errorf("at least one user is required")
	}
	if c.AdminUser() == nil {
		return fmt.Errorf("at least one user must be in the platform-admins group (the up verification and muster RBAC depend on it)")
	}
	seen := map[string]bool{}
	for _, u := range c.Users {
		if u.Email == "" || u.Username == "" || u.Password == "" {
			return fmt.Errorf("user %q: email, username and password are all required", u.Email)
		}
		if seen[u.Email] {
			return fmt.Errorf("duplicate user email %q", u.Email)
		}
		seen[u.Email] = true
		for _, g := range u.Groups {
			if !slices.Contains(Groups, g) {
				return fmt.Errorf("user %s: unknown group %q (RBAC only binds %v)", u.Email, g, Groups)
			}
		}
	}
	if c.Backstage.Enabled && !c.Platform.Enabled {
		return fmt.Errorf("backstage requires the agent platform (the muster plugin is the reason to run it)")
	}
	if err := ValidatePort(strconv.Itoa(c.Platform.MusterPort)); err != nil {
		return fmt.Errorf("platform.musterPort: %w", err)
	}
	if c.Platform.Domain == "" {
		return fmt.Errorf("platform.domain is required (public hostnames derive from it)")
	}
	if err := ValidatePort(strconv.Itoa(c.Platform.GatewayPort)); err != nil {
		return fmt.Errorf("platform.gatewayPort: %w", err)
	}
	if (c.Platform.TLS.CertFile == "") != (c.Platform.TLS.KeyFile == "") {
		return fmt.Errorf("platform.tls: certFile and keyFile must be set together")
	}
	if c.Platform.TLS.Set() {
		for _, p := range []string{c.Platform.TLS.CertFile, c.Platform.TLS.KeyFile} {
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("platform.tls: %w", err)
			}
		}
	}
	if err := ValidatePort(strconv.Itoa(c.Platform.AgentsPort)); err != nil {
		return fmt.Errorf("platform.agentsPort: %w", err)
	}
	if err := ValidateChartVersion(c.Platform.ChartVersion); err != nil {
		return fmt.Errorf("platform.chartVersion %q: %w", c.Platform.ChartVersion, err)
	}
	if c.Platform.ChartPath != "" {
		if _, err := os.Stat(filepath.Join(c.Platform.ChartPath, chartFile)); err != nil {
			return fmt.Errorf("platform.chartPath: %w (an agent-platform checkout's helm/agent-platform directory)", err)
		}
		if _, err := os.Stat(filepath.Join(ConnectivityChartDir(c.Platform.ChartPath), chartFile)); err != nil {
			return fmt.Errorf("platform.chartPath: no connectivity chart beside it (%w): the meta chart installs its wiring chart %s at its own version, which no registry publishes for a checkout, so the lab pushes the checkout's copy from that sibling directory into the lab registry — point chartPath at a checkout's helm/agent-platform, or follow a branch's dev builds instead (`agentlab configure --chart-path \"\" --chart-branch <branch>`)", err, ConnectivityChartName)
		}
	}
	if err := ValidateChartBranch(c.Platform.ChartBranch); err != nil {
		return fmt.Errorf("platform.chartBranch %q: %w", c.Platform.ChartBranch, err)
	}
	if c.Platform.ChartBranch != "" && c.Platform.ChartPath != "" {
		return fmt.Errorf("platform.chartBranch and platform.chartPath are mutually exclusive: a local chart has no dev builds to follow (`configure --chart-path \"\"` clears the path)")
	}
	for component, ref := range c.Platform.DevImages {
		if !slices.Contains(DevImageComponents, component) {
			return fmt.Errorf("platform.devImages: unknown target %q (one of %s)", component, strings.Join(DevImageComponents, ", "))
		}
		if err := ValidateImageRef(ref); err != nil {
			return fmt.Errorf("platform.devImages.%s %q: %w", component, ref, err)
		}
	}
	if _, ok := c.Platform.DevImages[DevImageHarness]; ok && !c.Platform.Agents {
		return fmt.Errorf("platform.devImages.%s: the platform Harness comes with the agents (platform.agents: true)", DevImageHarness)
	}
	if _, ok := c.Platform.DevImages[DevImageKlausGateway]; ok && !c.Platform.KlausGateway.Enabled {
		return fmt.Errorf("platform.devImages.%s: the component is off (platform.klausGateway.enabled: true, or `agentlab configure --klaus-gateway`) — the override would swap the image of nothing", DevImageKlausGateway)
	}
	// Swarmgeist is a client of the kagent controller: without the agents
	// runtime the component has nothing to talk to.
	if c.Platform.KlausGateway.Enabled && c.Platform.Enabled && !c.Platform.Agents {
		return fmt.Errorf("platform.klausGateway requires platform.agents (klaus-gateway runs its conversations on the kagent controller)")
	}
	if c.Platform.Serving.Enabled && c.Platform.Enabled && !c.Platform.Agents {
		return fmt.Errorf("platform.serving requires platform.agents (model-manager wires a served model into kagent as a ModelConfig)")
	}
	// The serving switch turns on the llm-d components alone; a pinned
	// release before servingChartFloor would refuse the llm-d controller's
	// release without kserve-resources and fail the install out of sight.
	if c.Platform.Serving.Enabled && c.Platform.Enabled && c.Platform.ChartPath == "" && c.Platform.ChartBranch == "" {
		if v, err := semver.NewVersion(c.Platform.ChartVersion); err == nil && v.LessThan(servingChartFloor) {
			return fmt.Errorf("platform.serving needs agent-platform %s or newer (the llm-d control plane alone, components.kserve-runtime-configs and modelServing.modelsGateway); platform.chartVersion is %s", servingChartFloor, c.Platform.ChartVersion)
		}
	}
	// The vm-manager component exists from agent-platform 4.11.0; a pinned
	// release before it would take components.vm-manager as an unknown key
	// and fail the install out of sight. A local checkout or a branch build
	// carries its own answer.
	if c.Platform.VMManager.Enabled && c.Platform.Enabled && c.Platform.ChartPath == "" && c.Platform.ChartBranch == "" {
		if v, err := semver.NewVersion(c.Platform.ChartVersion); err == nil && v.LessThan(vmManagerChartFloor) {
			return fmt.Errorf("platform.vmManager needs agent-platform %s or newer (components.vm-manager); platform.chartVersion is %s", vmManagerChartFloor, c.Platform.ChartVersion)
		}
	}
	if c.Platform.DevRegistryPort != 0 {
		if err := ValidatePort(strconv.Itoa(c.Platform.DevRegistryPort)); err != nil {
			return fmt.Errorf("platform.devRegistryPort: %w", err)
		}
	}
	for _, path := range c.Platform.ValuesFiles {
		if path == "" {
			return fmt.Errorf("platform.valuesFiles: an empty path")
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("platform.valuesFiles: %w", err)
		}
	}
	seenModels := map[string]bool{}
	for _, m := range c.Platform.ExtraModels {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("platform.extraModels: %w", err)
		}
		if seenModels[m.Name] {
			return fmt.Errorf("platform.extraModels: duplicate name %q", m.Name)
		}
		seenModels[m.Name] = true
	}
	if err := c.Platform.ModelManager.Validate(c.Platform.Agents); err != nil {
		return fmt.Errorf("platform.modelManager: %w", err)
	}
	if err := c.Platform.VMManager.Validate(); err != nil {
		return fmt.Errorf("platform.vmManager: %w", err)
	}
	if err := ValidatePort(strconv.Itoa(c.Backstage.Port)); err != nil {
		return fmt.Errorf("backstage.port: %w", err)
	}
	return nil
}

var httpURLRe = regexp.MustCompile(`^https?://[^/]+`)

// Validate checks the model-manager block; agents reports whether the kagent
// runtime is on (model-manager wires ModelConfigs into it).
func (m ModelManager) Validate(agents bool) error {
	backends := m.Backends
	if m.Backend != "" && !slices.Contains(backends, m.Backend) {
		backends = append([]string{m.Backend}, backends...) // the legacy form before normalize
	}
	for i, b := range backends {
		if !slices.Contains(ModelManagerBackends, b) {
			if b == ModelManagerBackendKServe {
				return fmt.Errorf("backend %q is not a host server: platform.serving adds it to the backends model-manager fronts", b)
			}
			return fmt.Errorf("backend %q: the lab supports %s (host model servers)", b, strings.Join(ModelManagerBackends, ", "))
		}
		if slices.Contains(backends[:i], b) {
			return fmt.Errorf("backends: %q listed twice", b)
		}
	}
	if m.Endpoint != "" && !httpURLRe.MatchString(m.Endpoint) {
		return fmt.Errorf("endpoint %q: must be an http(s) URL, e.g. http://172.21.0.1:%d", m.Endpoint, BackendPort(m.Primary()))
	}
	for _, b := range slices.Sorted(maps.Keys(m.Endpoints)) {
		ep := m.Endpoints[b]
		if !slices.Contains(backends, b) {
			return fmt.Errorf("endpoints.%s: %q is not in backends %v", b, b, backends)
		}
		if !httpURLRe.MatchString(ep) {
			return fmt.Errorf("endpoints.%s %q: must be an http(s) URL, e.g. http://172.21.0.1:%d", b, ep, BackendPort(b))
		}
	}
	if m.Enabled && !agents {
		return fmt.Errorf("requires platform.agents (model-manager wires pulled models into kagent ModelConfigs)")
	}
	return nil
}

// ModelManagerEnabled reports whether the platform installs model-manager.
func (c *Config) ModelManagerEnabled() bool {
	return c.Platform.Enabled && c.Platform.Agents && (c.Platform.ModelManager.Enabled || c.Platform.Serving.Enabled)
}

// ServingEnabled reports whether the platform serves models on llm-d: the
// platform with its agents runtime, and the key on.
func (c *Config) ServingEnabled() bool {
	return c.Platform.Enabled && c.Platform.Agents && c.Platform.Serving.Enabled
}

// ChartBackends is the list the chart's model-manager fronts, in order (the
// first is its default backend): the host model servers of
// platform.modelManager while it is on, then the platform's own serving
// (ModelManagerBackendKServe) while platform.serving is. Empty when neither
// is on.
func (c *Config) ChartBackends() []string {
	var backends []string
	if c.Platform.ModelManager.Enabled {
		backends = append(backends, c.Platform.ModelManager.Backends...)
	}
	if c.Platform.Serving.Enabled {
		backends = append(backends, ModelManagerBackendKServe)
	}
	return backends
}

// VMManagerEnabled reports whether the platform registers the host
// vm-manager with muster. The platform is all it needs: vm-manager is an MCP
// server of muster's, not a consumer of the agents runtime.
func (c *Config) VMManagerEnabled() bool {
	return c.Platform.Enabled && c.Platform.VMManager.Enabled
}

// KlausGatewayEnabled reports whether the platform runs the klaus-gateway
// component: the platform with its agents runtime, and the key on.
func (c *Config) KlausGatewayEnabled() bool {
	return c.Platform.Enabled && c.Platform.Agents && c.Platform.KlausGateway.Enabled
}

// The chart channels: where the meta chart the lab installs comes from.
const (
	// ChartChannelStable is a released chart, pinned by platform.chartVersion.
	ChartChannelStable = "stable"
	// ChartChannelDev is the dev channel: the newest dev build of
	// platform.chartBranch (or the one platform.chartPinned froze).
	ChartChannelDev = "dev"
	// ChartChannelPath is a chart directory, platform.chartPath.
	ChartChannelPath = "path"
)

// ChartChannel names where the meta chart comes from: a chart directory wins
// over a branch, a branch over the pinned release (the same precedence the
// install applies).
func (c *Config) ChartChannel() string {
	switch {
	case c.Platform.ChartPath != "":
		return ChartChannelPath
	case c.Platform.ChartBranch != "":
		return ChartChannelDev
	default:
		return ChartChannelStable
	}
}

// ChartMajor is the major version of the meta chart release platform.
// chartVersion pins (a leading v tolerated); 0 when it is not an exact
// version — ValidateChartVersion rejects that before anything renders.
func (c *Config) ChartMajor() uint64 {
	return MajorOf(c.Platform.ChartVersion)
}

// MajorOf is the major of an exact chart version (a leading v tolerated), 0
// when the string is not one.
func MajorOf(version string) uint64 {
	v, err := semver.NewVersion(version)
	if err != nil {
		return 0
	}
	return v.Major()
}

// LegacyChart reports whether the lab installs a released meta chart of the
// 3.x line: an exact platform.chartVersion below 4.0.0 on the stable
// channel. The 3.x line's values are a different shape (kagent 0.10 with
// its bundled Postgres, no Agent Substrate, no platform Postgres, a closed
// root schema that refuses the 4.x keys), so the lab values render in that
// shape for it; a dev channel (platform.chartBranch) or a chart directory
// (platform.chartPath) is always the current line, whatever version its
// Chart.yaml or resolved tag carries.
func (c *Config) LegacyChart() bool {
	if c.ChartChannel() != ChartChannelStable {
		return false
	}
	major := c.ChartMajor()
	return major > 0 && major < 4
}

// AdminUser returns the first user in platform-admins: the identity the up
// verification loop and the smoke tests default to.
func (c *Config) AdminUser() *User {
	for i := range c.Users {
		if c.Users[i].HasGroup(groupPlatformAdmins) {
			return &c.Users[i]
		}
	}
	return nil
}

func (c *Config) FindUser(email string) *User {
	for i := range c.Users {
		if c.Users[i].Email == email {
			return &c.Users[i]
		}
	}
	return nil
}

// FindUserInGroup returns the first user carrying group, or nil.
func (c *Config) FindUserInGroup(group string) *User {
	for i := range c.Users {
		for _, g := range c.Users[i].Groups {
			if g == group {
				return &c.Users[i]
			}
		}
	}
	return nil
}

func (c *Config) Issuer() string {
	return fmt.Sprintf("https://localhost:%d/dex", c.DexPort)
}

// ControlPlaneNode is the docker container name kind gives the (only) node.
func (c *Config) ControlPlaneNode() string { return c.ClusterName + "-control-plane" }

// MCPServerName is the MCPServer CR name the umbrella chart registers for its
// bundled mcp-kubernetes (templates/mcp-kubernetes/mcpserver.yaml): a fixed
// name, independent of the cluster. Muster prefixes the server's tools with
// it: x_mcp-kubernetes_<tool>.
func (c *Config) MCPServerName() string { return "mcp-kubernetes" }

// gatewayURL builds the public URL of a platform hostname: through the
// agentgateway edge, port-free when the edge sits on 443. The chart's
// Backstage app-config renders port-free URLs regardless; the lab's overlay
// restates them from BackstageBaseURL (HACKS.md U15).
func (c *Config) gatewayURL(prefix string) string {
	if c.Platform.GatewayPort == 443 {
		return fmt.Sprintf("https://%s.%s", prefix, c.Platform.Domain)
	}
	return fmt.Sprintf("https://%s.%s:%d", prefix, c.Platform.Domain, c.Platform.GatewayPort)
}

// MusterBaseURL is muster's public URL: client -> agentgateway edge -> muster,
// the real platform topology. What Claude Code dials and what muster's OAuth
// server advertises.
func (c *Config) MusterBaseURL() string { return c.gatewayURL("muster") }

// MusterDirectURL bypasses the edge: the hostNetwork port mapping straight to
// muster, kept for debugging the lab's own plumbing.
func (c *Config) MusterDirectURL() string {
	return fmt.Sprintf("http://localhost:%d", c.Platform.MusterPort)
}

// BackstageBaseURL is Backstage's public URL through the agentgateway edge.
func (c *Config) BackstageBaseURL() string { return c.gatewayURL("backstage") }

// ObservabilityBaseURL is the lab Prometheus's public query API through the
// edge, at the /prometheus prefix Backstage's Mimir integration expects
// (observability-route.yaml.tmpl). Backstage itself always dials it in-cluster
// on 443 (the CoreDNS rewrite), so only host-side callers see GatewayPort.
func (c *Config) ObservabilityBaseURL() string {
	return c.gatewayURL("observability") + "/prometheus"
}

// AgentgatewayBaseURL is the agentgateway hostname through the edge: the
// path-prefixed platform APIs (the kagent controller at /kagent,
// model-manager at /model-manager) live here, same as the chart's Backstage
// app-config derives them.
func (c *Config) AgentgatewayBaseURL() string { return c.gatewayURL("agentgateway") }

// ModelManagerBaseURL is the model-manager API through the edge — the
// umbrella's components.model-manager.route (pathPrefix /model-manager,
// stripped before the service, so its REST API sits at <base>/api/v1). The
// route's JWT policy wants a Dex token on every call.
func (c *Config) ModelManagerBaseURL() string { return c.AgentgatewayBaseURL() + "/model-manager" }

// BackstageDirectURL bypasses the edge (hostNetwork port mapping), kept for
// debugging.
func (c *Config) BackstageDirectURL() string {
	return fmt.Sprintf("http://localhost:%d", c.Backstage.Port)
}

func (c *Config) KagentUIBaseURL() string {
	return fmt.Sprintf("http://localhost:%d", c.Platform.AgentsPort)
}

// ValidateNodePort checks the Kubernetes NodePort range; the Dex port must be
// a NodePort because the Service exposes it as one. String-typed (like
// ValidatePort) so the huh form inputs can use it directly.
func ValidateNodePort(s string) error {
	return validatePort(s, 30000, 32767, "must be in the NodePort range 30000-32767")
}

func ValidatePort(s string) error {
	return validatePort(s, 1, 65535, "must be a port between 1 and 65535")
}

func validatePort(s string, lo, hi int, rangeMsg string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("not a number")
	}
	if n < lo || n > hi {
		return fmt.Errorf("%s", rangeMsg)
	}
	return nil
}
