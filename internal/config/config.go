// Package config holds the lab configuration: everything the interactive
// form asks for, persisted to agentlab.yaml so re-runs are reproducible.
package config

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/pkg/project"
)

// File is the configuration file written next to the binary's working
// directory by `agentlab configure` and read by every other command.
const File = "agentlab.yaml"

// The lab groups are a fixed vocabulary: RBAC binds the first three to
// cluster-admin / edit-in-demo / view (see the rbac template). The two
// tenant groups the lab binds nothing to: `agentlab tenants-test` binds each
// to agent-platform:user in one organization namespace, so a token's groups
// claim is what gives its person that tenant.
const (
	groupPlatformAdmins = "platform-admins"
	groupDevelopers     = "developers"
	groupViewers        = "viewers"
	GroupTenantA        = "tenant-a"
	GroupTenantB        = "tenant-b"
)

var Groups = []string{groupPlatformAdmins, groupDevelopers, groupViewers, GroupTenantA, GroupTenantB}

// TenantUsers are the people `agentlab configure --tenants` adds when no user
// carries their tenant group: one person per organization of tenants-test,
// in that group alone.
var TenantUsers = []User{
	{Email: "alice@lab.local", Username: "alice", Name: "Lab Tenant A", Password: defaultPassword, Groups: []string{GroupTenantA}},
	{Email: "bob@lab.local", Username: "bob", Name: "Lab Tenant B", Password: defaultPassword, Groups: []string{GroupTenantB}},
}

// EnsureTenantUsers adds each of TenantUsers whose group no user carries
// and whose email is free, and returns the emails it added.
func (c *Config) EnsureTenantUsers() []string {
	var added []string
	for _, u := range TenantUsers {
		if c.FindUserInGroup(u.Groups[0]) != nil || c.FindUser(u.Email) != nil {
			continue
		}
		u.Groups = slices.Clone(u.Groups)
		c.Users = append(c.Users, u)
		added = append(added, u.Email)
	}
	return added
}

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
	// An audience-only client: Giant Swarm apiservers trust its id, and it
	// trusts agent-platform to request its audience. It never runs a flow.
	DexK8sAuthenticatorClientID = "dex-k8s-authenticator"
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

// DefaultClusterName is a new lab's clusterName when no other lab on the
// machine holds it.
const DefaultClusterName = "agentlab"

// DefaultDexPort is the lab Dex NodePort when agentlab.yaml sets none.
const DefaultDexPort = 32000

// DefaultAPIServerPort is the host port of the lab's Kubernetes API server
// when agentlab.yaml sets none.
const DefaultAPIServerPort = 6443

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
const DefaultChartVersion = "4.164.1"

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
// for the Deployments the lab's dev loops build from a checkout — the
// agentgateway component's two Deployments under their own names — plus
// DevImageHarness for the platform Harness's runtime image and one target per
// coding Harness's image (DevImageClaudeHarness, DevImageCodexHarness,
// DevImageOmpHarness).
var DevImageComponents = []string{"muster", "backstage", "kagent", "mcp-kubernetes", "model-manager", "agent-manager", "vm-manager", DevImageKlausGateway, DevImageClusterManager, DevImageWorkspaceManager, DevImageAgentgateway, DevImageAgentgatewayController, DevImageSubstrateAPI, DevImageMCPPrometheus, DevImageHarness, DevImageClaudeHarness, DevImageCodexHarness, DevImageOmpHarness}

// DevImageKlausGateway is the devImages key of the klaus-gateway component
// (platform.klausGateway): the Deployment `klaus-gateway`, container
// `klaus-gateway`. Meaningful only while the component is on.
const DevImageKlausGateway = "klaus-gateway"

// DevImageClusterManager is the devImages key of the cluster-manager
// component: the Deployment `cluster-manager`, container `cluster-manager`.
// The meta chart ships the component off, and the lab has no other switch
// for it: naming its image here turns it on (ClusterManagerEnabled), with
// the lab's own values for it (the `kubernetes` audience its MCP server
// requires of the forwarded token).
const DevImageClusterManager = "cluster-manager"

// DevImageWorkspaceManager is the devImages key of the workspace-manager
// component: the Deployment `workspace-manager`, container
// `workspace-manager`. The component follows platform.workspaces.enabled,
// so the target is meaningful only while the workspaces are on.
const DevImageWorkspaceManager = "workspace-manager"

// DevImageAgentgateway is the devImages key of the agentgateway data plane:
// the Deployment `agentgateway`, container `agentgateway` — the proxy the
// agentgateway controller runs for the lab's Gateway. The controller creates
// that Deployment at run time from the image its chart hands it as registry,
// repository and tag (the chart's proxy.image values), so the lab forwards
// the ref through those values rather than as a post-renderer, and a digest
// ref — which the three fields cannot carry — is refused.
const DevImageAgentgateway = "agentgateway"

// DevImageAgentgatewayController is the devImages key of the agentgateway
// controller: the Deployment `agentgateway-controller`, container
// `controller`, rendered by the agentgateway component's chart.
const DevImageAgentgatewayController = "agentgateway-controller"

// DevImageSubstrateAPI is the devImages key of Agent Substrate's control
// plane API: the Deployment `ate-api-server`, container `ate-api-server`,
// rendered by the `substrate` component's chart — where the ActorTemplate
// reconciler and its golden snapshots live. Meaningful only with the agents,
// which bring Substrate.
const DevImageSubstrateAPI = "ate-api-server"

// DevImageMCPPrometheus is the devImages key of the lab's own mcp-prometheus
// (platform.observability): the Deployment `mcp-prometheus`, container
// `mcp-prometheus`, rendered by the HelmRelease the lab writes outside the
// meta chart (mcp-prometheus.yaml.tmpl), whose postRenderers carry the swap.
const DevImageMCPPrometheus = "mcp-prometheus"

// DevImageHarness is the devImages key of the platform Harness's workload
// image — the Go ADK runtime every agent runs on under kagent API v2. Not a
// Deployment: the connectivity chart renders the image by digest into the
// `Harness` object, and Substrate's atelet pulls it from a registry into its
// own layer cache, so the lab pushes the local build to its registry
// (DevRegistryPort) and forwards the digest through kagent.harness.image.
const DevImageHarness = "harness"

// DevImageClaudeHarness, DevImageCodexHarness and DevImageOmpHarness are the
// devImages keys of a coding Harness's image, `<runtime>-harness`: a build
// of the kagent line's claude-harness, codex-harness or omp-harness, say
// from a pull request. Like the `harness` target it is no Deployment: the
// lab pushes it to its registry and forwards the digest as the image of that
// runtime's kagent.harnesses entry. Meaningful only while the lab renders
// that Harness (CodingHarnessRendered).
const (
	DevImageClaudeHarness = CodingRuntimeClaude + "-harness"
	DevImageCodexHarness  = CodingRuntimeCodex + "-harness"
	DevImageOmpHarness    = CodingRuntimeOmp + "-harness"
)

// codingHarnessVersionPins are the coding runtimes whose CLI version the
// kagent controller compiles into the Harness config with StrictVersion, by
// the constant that pins it on the kagent line: their harness dev image
// travels with a controller dev image from the same build.
var codingHarnessVersionPins = map[string]string{
	CodingRuntimeClaude: "ExpectedClaudeVersion, claudeconfig.PinnedClaudeVersion",
	CodingRuntimeCodex:  "ExpectedCodexVersion, codexconfig.PinnedCodexVersion",
}

// CodingHarnessDevImage is the devImages key of a coding runtime's image.
func CodingHarnessDevImage(runtime string) string { return runtime + "-harness" }

// RegistryDevImage reports whether a devImages target is pushed to the lab
// registry and pinned by digest (a Harness's image) rather than side-loaded
// into the node as a Deployment's.
func RegistryDevImage(component string) bool {
	return component == DevImageHarness || component == DevImageClaudeHarness || component == DevImageCodexHarness || component == DevImageOmpHarness
}

// The coding runtimes the lab renders a Harness of, each named after its
// runtime: claude (Claude Code), codex (OpenAI's Codex CLI) and omp
// (oh-my-pi, a BYO Harness on the kagent line's omp-harness adapter).
const (
	CodingRuntimeClaude = "claude"
	CodingRuntimeCodex  = "codex"
	CodingRuntimeOmp    = "omp"
)

// CodingRuntimes are the runtimes platform.coding.harnesses takes.
var CodingRuntimes = []string{CodingRuntimeClaude, CodingRuntimeCodex, CodingRuntimeOmp}

// CodingChartRuntime is the `runtime` the kagent chart's harnesses[] entry
// of a coding runtime names: the runtime itself where kagent has one, `byo`
// for omp, which runs the omp-harness adapter on an image the entry names.
func CodingChartRuntime(runtime string) string {
	if runtime == CodingRuntimeOmp {
		return "byo"
	}
	return runtime
}

// codingImageRe is a coding Harness's image pinned by digest, what the kagent
// chart's harnesses[] entry and the Harness API take.
var codingImageRe = regexp.MustCompile(`^[^[:space:]@]+@sha256:[a-f0-9]{64}$`)

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

// codingChartFloor is the first agent-platform release that forwards
// kagent.harnesses, the kagent chart's further Harnesses.
var codingChartFloor = semver.MustParse("4.110.0")

// refusalNoticesChartFloor is the first agent-platform release that runs
// klaus-gateway's egress refusal notices (klausGateway.refusals, on by
// default where Slack, kagent and Substrate are) and admits the gateway as
// the kagent controller's session reader. An older release forwards a
// klausGateway.refusals block to a klaus-gateway whose closed schema
// refuses it.
var refusalNoticesChartFloor = semver.MustParse("4.154.0")

// RefusalNoticesChart reports whether the lab's meta chart runs the egress
// refusal notices: a released chart from refusalNoticesChartFloor on, a
// chart directory, a branch build.
func (c *Config) RefusalNoticesChart() bool {
	_, below := c.chartBelow(refusalNoticesChartFloor)
	return !below
}

// familiesChartFloor is the first agent-platform release whose connectivity
// chart registers the bundled mcp-kubernetes as a kubernetes family member
// (mcp-kubernetes.mcpServer.managementCluster, giantswarm/agent-platform#403).
// An older release ignores the key and registers the family-less singleton
// next to the lab's families.
var familiesChartFloor = semver.MustParse("4.93.0")

// CheckFamiliesChartFloor refuses a released 4.x chart before
// familiesChartFloor, which would register the family-less mcp-kubernetes
// next to the lab's families. Checked where the chart is installed, not on
// load, so `agentlab configure --chart-version` can move an older pin. The
// 3.x line (a migration rehearsal's seed, a maintenance-line fix), a branch
// build, a chart directory and an upgrade proof's seed (platform.upgradeSeed)
// are not checked.
func (c *Config) CheckFamiliesChartFloor() error {
	if c.Platform.ChartPath != "" || c.Platform.ChartBranch != "" || c.LegacyChart() || c.Platform.UpgradeSeed {
		return nil
	}
	if c.belowFamiliesFloor() {
		return fmt.Errorf("agentlab needs agent-platform %s or newer (the bundled mcp-kubernetes as a kubernetes family member); platform.chartVersion is %s — `agentlab configure --defaults --chart-version %s`, or `--upgrade-seed` to seed an upgrade proof on this line (docs/platform.md \"Upgrade proofs from an older line\")", familiesChartFloor, c.Platform.ChartVersion, DefaultChartVersion)
	}
	return nil
}

// belowFamiliesFloor reports whether the lab pins a released 4.x chart
// before familiesChartFloor. A chart directory and the dev channel are the
// current line.
func (c *Config) belowFamiliesFloor() bool {
	if c.Platform.ChartPath != "" || c.Platform.ChartBranch != "" {
		return false
	}
	v, err := semver.NewVersion(c.Platform.ChartVersion)
	return err == nil && v.LessThan(familiesChartFloor)
}

// FamilylessChart reports whether the lab's connectivity chart registers the
// bundled mcp-kubernetes as the family-less MCPServer `mcp-kubernetes`
// instead of a kubernetes family member: the 3.x line, and a released 4.x
// chart before familiesChartFloor (an upgrade proof's seed). The boot and the
// proofs address that server, and platform-test skips the families proof.
func (c *Config) FamilylessChart() bool {
	return c.LegacyChart() || c.belowFamiliesFloor()
}

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
	// (x_prometheus_<tool> with management_cluster set to the cluster name). On by default: asking the platform about the
	// cluster's CPU/memory is part of the demo story. Not in the umbrella's
	// BOM (yet) — the lab pins the two charts itself (observability.go).
	// Inert when the platform itself is disabled.
	Observability bool `yaml:"observability"`
	// Alertmanager adds the kube-prometheus-stack Alertmanager to the
	// observability stack, a Prometheus alerting rule that always fires
	// (AgentlabWatchdog) and ALERTMANAGER_URL on mcp-prometheus, so its alert
	// tools (x_prometheus_get_alertmanager_alerts) have alerts to read. Off by
	// default; --alertmanager turns it on; needs platform.observability. See
	// docs/observability.md "Alertmanager".
	Alertmanager bool `yaml:"alertmanager,omitempty"`
	// MCPPrometheusChartVersion overrides the mcp-prometheus chart release
	// the lab installs with the observability stack (an exact version, a
	// release candidate included), for proving a chart change the image swap
	// (devImages.mcp-prometheus) cannot carry. Empty keeps the release this
	// agentlab pins (internal/lab/observability.go); needs
	// platform.observability.
	MCPPrometheusChartVersion string `yaml:"mcpPrometheusChartVersion,omitempty"`
	// Retired: the fake fleet. The lab registers its own cluster as the
	// infrastructure families instead (MCPServerName). Read so a file an
	// earlier release wrote still loads, never written back.
	RetiredFakeFleet *bool `yaml:"fakeFleet,omitempty"`
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
	// GatewayAddress is the loopback address the edge is published on, empty
	// for 127.0.0.1. A second lab on the machine takes 127.0.0.<n> with the
	// domain lab<n>.127.0.0.<n>.nip.io (`configure` picks both), so it serves
	// on 443 too and its URLs carry no port: Linux routes all of 127.0.0.0/8
	// to loopback. Fixed at node creation like the ports.
	GatewayAddress string `yaml:"gatewayAddress,omitempty"`
	// TLS optionally hands the edge an externally provisioned certificate
	// pair (PEM) instead of the minted lab-CA wildcard — for users who own a
	// real domain (wildcard record -> 127.0.0.1) and run their own ACME
	// tooling. Both fields set or both empty. With a pair the Dex issuer
	// moves under the domain too (Issuer: dex.<domain>) and serves the same
	// pair, which must therefore cover that name; see docs/tls.md.
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
	// UpgradeSeed marks the lab as an upgrade proof's seed: a released 4.x
	// chart before the lab's floor (familiesChartFloor) installs in the shape
	// that line takes (FamilylessChart), so a lab can start on an older line
	// and be upgraded in place to the current one (`agentlab configure
	// --chart-version`, then `agentlab platform`). No effect on a chart at
	// or above the floor. See docs/platform.md "Upgrade proofs from an older
	// line".
	UpgradeSeed bool `yaml:"upgradeSeed,omitempty"`
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
	// MusterPodNetwork runs muster the way an installation does: on the pod
	// network at two replicas behind the edge, a PodDisruptionBudget of
	// minAvailable 1 — the shape muster's availability work is proven in.
	// The issuer stays reachable from its pods as from every other platform
	// pod: the dex-localhost sidecar while it is spelled localhost, cluster
	// DNS under platform.tls. Off (the default), muster runs on the host
	// network at one replica, which muster's direct debug port (musterPort)
	// needs; with it on that port answers nothing. --muster-pod-network turns
	// it on. See docs/platform.md "muster on the pod network".
	MusterPodNetwork bool `yaml:"musterPodNetwork,omitempty"`
	// GitHub registers GitHub's hosted MCP server with muster, signed in to
	// as the person through an OAuth client (internal/lab/githubmcp.go).
	GitHub GitHub `yaml:"github"`
	// GitHubSignIn adds a GitHub connector to the lab Dex, so a person signs
	// in to the lab as their GitHub self next to the local users
	// (internal/lab/githubsignin.go). Its App is a sign-in App and nothing
	// else: the github MCP server above has a client of its own.
	GitHubSignIn GitHubSignIn `yaml:"githubSignIn"`
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
	// Workspace storage: the read-write-many StorageClass a workspace's
	// volume is claimed from (internal/lab/workspaces.go) — an in-cluster
	// NFS server, the NFS CSI driver with an mTLS proxy in front of its
	// controller that admits ate-api-server's pod identity alone, and the
	// StorageClass the chart's workspaces values name. Off by default;
	// --workspaces turns it on; needs the agents runtime: the chart's
	// workspaces live in kagent's namespace, so Validate refuses them without
	// it. `agentlab workspaces-test --storage-only` is the proof.
	Workspaces Workspaces `yaml:"workspaces"`
	// Coding agents: the chart's kagent.harnesses renders the lab's coding
	// Harness (internal/lab/coding.go) claude, Claude Code run headless. Off
	// by default; --coding turns it on; needs the agents runtime. `agentlab
	// coding-test` is the proof.
	Coding Coding `yaml:"coding"`
	// The Organizations' namespaces as tenants: the chart's
	// tenants.organizations.enabled, which turns on agent-manager's tenant
	// reconciler and admits the agent workers of every namespace labelled
	// giantswarm.io/organization. Off by default; --tenants turns it on;
	// needs the agents runtime. `agentlab tenants-test` is the proof.
	Tenants Tenants `yaml:"tenants"`
	// Retired: the platform-manager fixture. Read so a file an earlier
	// release wrote still loads, never written back.
	RetiredPlatformManager any `yaml:"platformManager,omitempty"`
}

// Coding configures the lab's coding Harnesses.
type Coding struct {
	// On, `agentlab platform` renders the coding Harnesses through the
	// chart's kagent.harnesses.
	Enabled bool `yaml:"enabled"`
	// Harnesses are the coding Harnesses rendered, by runtime (one of
	// CodingRuntimes), each named after it; empty is claude alone. codex
	// renders on the chart's codex-harness image, which needs a kagent line
	// whose chart takes `runtime: codex`; omp renders a BYO entry on the
	// image Images names, which needs a kagent line whose chart takes
	// `runtime: byo`.
	Harnesses []string `yaml:"harnesses,omitempty"`
	// Images are the images of the coding Harnesses the chart has no image
	// of its own for (omp: the kagent line's omp-harness adapter, the digest
	// a release's chart stamps as runtimeImages.ompHarness), by runtime,
	// pinned by digest. A dev image (platform.devImages `<runtime>-harness`)
	// wins over an entry here.
	Images map[string]string `yaml:"images,omitempty"`
}

// Tenants configures the Organizations' namespaces as tenants.
type Tenants struct {
	// On, `agentlab platform` renders tenants.organizations.enabled: true.
	Enabled bool `yaml:"enabled"`
}

// Workspaces configures the workspace storage in the lab.
type Workspaces struct {
	// On, `agentlab platform` installs the NFS server, the NFS CSI driver
	// behind its mTLS proxy and the StorageClass before the chart, and
	// renders the chart's `workspaces:` block when the chart carries the
	// key.
	Enabled bool `yaml:"enabled"`
	// Provider names the workspace-manager's provider instances:
	// WorkspaceProviderGitHub (a GitHub App of the lab's own on github.com;
	// the default while empty), WorkspaceProviderGitLab (a GitLab the lab
	// reaches, its OAuth application and group access token), or both,
	// comma-separated ("github,gitlab"). With workspaces on, the
	// workspace-manager's public base URL carries the gateway port
	// (WorkspaceManagerBaseURL) and the chart routes its host.
	Provider string `yaml:"provider,omitempty"`
	// GrantProvider is the instance whose App muster pins the
	// workspace-manager's registration to (WorkspaceGrantProvider): a person
	// grants that App once in muster, and muster attaches the person's token
	// for it to every call of the manager's tools. Empty is the first
	// instance of Provider, in the order WorkspaceProviders gives them.
	// WorkspaceGrantProviderApps pins nothing: muster keeps one provider
	// App per instance, side by side (WorkspaceProviderApps), and a person
	// grants each once (core_auth_login app=<instance>); it takes two or
	// more instances.
	GrantProvider string `yaml:"grantProvider,omitempty"`
	// GitHub is the App behind the provider instance github.
	GitHub WorkspaceGitHub `yaml:"github,omitempty"`
	// GitLab is the GitLab behind the provider instance gitlab.
	GitLab WorkspaceGitLab `yaml:"gitlab,omitempty"`
	// SessionsCleanupAfter is how long a Session's directory outlives its
	// last turn, a Go duration the workspace-manager takes as its
	// sessions.cleanupAfter; empty keeps the chart's 30 days. A short window
	// is a proof's: `workspaces-test --part sessions` waits it out once.
	SessionsCleanupAfter string `yaml:"sessionsCleanupAfter,omitempty"`
	// SyncRefreshAfter is the refresh threshold, a Go duration the
	// workspace-manager takes as its sync.refreshAfter: a Session started on
	// a workspace whose last sync is older requests a sync; empty keeps the
	// manager's 1h. A short threshold is a proof's: `workspaces-test --part
	// refresh` lets a workspace pass it.
	SyncRefreshAfter string `yaml:"syncRefreshAfter,omitempty"`
	// CSICompanion runs the connectivity chart's CSI companion
	// (workspaces.substrate.csiCompanion) as Substrate's driver in place of
	// the lab's own registration: its efs preset with the lab's NFS plugin
	// image and arguments, the lab's NFS driver serving the volumes under
	// the preset's driver name. The lab path for proving a companion chart
	// change (docs/workspaces.md); needs a chart that carries the key.
	CSICompanion bool `yaml:"csiCompanion,omitempty"`
}

// WorkspaceGitLab is the GitLab behind the workspace-manager's provider
// instance gitlab: its URL and the OAuth application's client id are public
// and live here; the group access token the sync mirrors with, the OAuth
// application's secret and the proof person's GitLab password are the
// Secret WorkspaceGitLabSecretName in the platform namespace (keys
// WorkspaceGitLabTokenKey, GitHubClientSecretKey and
// WorkspaceGitLabPasswordKey), which the operator's secret tooling places
// and agentlab only references. The instance renders once the URL, the
// client id and the first two keys are in place; the person is what
// `workspaces-test --part gitlab` signs in to GitLab as.
type WorkspaceGitLab struct {
	URL      string `yaml:"url,omitempty"`
	ClientID string `yaml:"clientId,omitempty"`
	// Group is the GitLab group the proof's workspace mirrors (a top-level
	// group the token reads); Project one of its projects the person pushes
	// to.
	Group   string `yaml:"group,omitempty"`
	Project string `yaml:"project,omitempty"`
	// Person is the GitLab username the proof signs in to GitLab as,
	// connecting the lab's admin; its password is the Secret's
	// WorkspaceGitLabPasswordKey.
	Person string `yaml:"person,omitempty"`
	// TokenSource, ClientSecretSource and PasswordSource are references
	// `beekeeper secret copy` resolves, in the form AIKey.Source takes. Set,
	// every up and platform places them into the Secret; empty, the Secret
	// is placed by hand.
	TokenSource        string `yaml:"tokenSource,omitempty"`
	ClientSecretSource string `yaml:"clientSecretSource,omitempty"`
	PasswordSource     string `yaml:"passwordSource,omitempty"`
	// ProofRepositories are the two projects, <group>/<project>, the
	// workspaces sync and sessions parts mirror on this instance
	// (ValidateProofRepositories); the person pushes to the first.
	ProofRepositories []string `yaml:"proofRepositories,omitempty"`
}

// Validate checks the URL and the sources.
func (g WorkspaceGitLab) Validate() error {
	if g.URL != "" {
		u, err := url.Parse(g.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") {
			return fmt.Errorf("platform.workspaces.gitlab.url %q: want https://<host>[:<port>]", g.URL)
		}
	}
	for _, s := range []struct{ name, source string }{
		{"tokenSource", g.TokenSource},
		{"clientSecretSource", g.ClientSecretSource},
		{"passwordSource", g.PasswordSource},
	} {
		if s.source == "" {
			continue
		}
		if err := ValidateSecretSource(s.source); err != nil {
			return fmt.Errorf("platform.workspaces.gitlab.%s %q: %w", s.name, s.source, err)
		}
	}
	return ValidateProofRepositories("platform.workspaces.gitlab.proofRepositories", g.ProofRepositories)
}

// WorkspaceGitHub is the GitHub App of the workspace-manager's
// provider instance github: an App of its own, apart from the lab Dex's
// sign-in App and muster's github server client. Its App id and client id
// are public and live here, empty until the App is registered; its private
// key and client secret are the Secret WorkspaceGitHubSecretName in the
// platform namespace (keys WorkspaceGitHubPrivateKeyKey and
// GitHubClientSecretKey), which the operator's secret tooling places and
// agentlab only references: it reads which keys the Secret carries, never
// a value.
type WorkspaceGitHub struct {
	AppID    string `yaml:"appId,omitempty"`
	ClientID string `yaml:"clientId,omitempty"`
	// PrivateKeySource and ClientSecretSource are references `beekeeper
	// secret copy` resolves to the App's private key and client secret, in
	// the form AIKey.Source takes. Set, every up and platform places them
	// into the Secret, so a recreated lab carries the instance without a
	// manual step; empty, the Secret is placed by hand.
	PrivateKeySource   string `yaml:"privateKeySource,omitempty"`
	ClientSecretSource string `yaml:"clientSecretSource,omitempty"`
	// ProofRepositories are the two repositories, <owner>/<name>, the
	// workspaces sync and sessions parts mirror on this instance through
	// the App (ValidateProofRepositories): the person pushes and
	// force-pushes to a scratch branch of the first with their own git
	// credentials; the second stays as it is.
	ProofRepositories []string `yaml:"proofRepositories,omitempty"`
}

// Validate checks the two sources: each a secret reference when set.
func (g WorkspaceGitHub) Validate() error {
	for _, s := range []struct{ name, source string }{
		{"privateKeySource", g.PrivateKeySource},
		{"clientSecretSource", g.ClientSecretSource},
	} {
		if s.source == "" {
			continue
		}
		if err := ValidateSecretSource(s.source); err != nil {
			return fmt.Errorf("platform.workspaces.github.%s %q: %w", s.name, s.source, err)
		}
	}
	return ValidateProofRepositories("platform.workspaces.github.proofRepositories", g.ProofRepositories)
}

// ValidateProofRepositories checks an instance's proof repositories: none,
// or two distinct <owner>/<name> paths, key naming the field.
func ValidateProofRepositories(key string, repos []string) error {
	if len(repos) == 0 {
		return nil
	}
	if len(repos) != 2 {
		return fmt.Errorf("%s: want two repositories, <owner>/<name>: the one the proof pushes to, then one it leaves as it is (got %d)", key, len(repos))
	}
	for _, r := range repos {
		owner, name, ok := strings.Cut(r, "/")
		if !ok || owner == "" || name == "" || strings.ContainsAny(r, " \t\n:@") || strings.HasSuffix(r, ".git") {
			return fmt.Errorf("%s: %q is no <owner>/<name>", key, r)
		}
	}
	if strings.EqualFold(repos[0], repos[1]) {
		return fmt.Errorf("%s: %s twice, want two repositories", key, repos[0])
	}
	return nil
}

// WorkspaceOrganization is the lab's one workspace-manager Organization, the
// owner of every workspace a lab user creates (the namespace org-<name> is
// the fleet-shaped org namespace the lab also carries).
// WorkspaceOrganizationGroups are the lab Dex groups whose users are its
// members: the platform admins and the developers. The viewers stay outside,
// so the workspace tools refuse them as not a member.
const WorkspaceOrganization = "lab"

var WorkspaceOrganizationGroups = []string{groupPlatformAdmins, groupDevelopers}

// The workspace providers, and the Secrets of their Apps: what the
// operator's secret tooling placed, which the values only ever reference.
const (
	WorkspaceProviderGitHub      = "github"
	WorkspaceProviderGitLab      = "gitlab"
	WorkspaceGitHubSecretName    = "workspace-github" // #nosec G101 -- Secret NAME, not a credential
	WorkspaceGitHubPrivateKeyKey = "private-key"      // #nosec G101 -- Secret KEY name, not a credential
	WorkspaceGitLabSecretName    = "workspace-gitlab" // #nosec G101 -- Secret NAME, not a credential
	WorkspaceGitLabTokenKey      = "token"            // #nosec G101 -- Secret KEY name, not a credential
	WorkspaceGitLabPasswordKey   = "person-password"  // #nosec G101 -- Secret KEY name, not a credential
)

// WorkspaceGrantProviderApps is the grantProvider that pins no instance:
// one muster provider App per instance, side by side.
const WorkspaceGrantProviderApps = "apps"

// workspaceProviderNames are the instances platform.workspaces.provider may
// name, in the order the workspace-manager gets them.
var workspaceProviderNames = []string{WorkspaceProviderGitHub, WorkspaceProviderGitLab}

// parseWorkspaceProviders reads platform.workspaces.provider: github alone
// for "", else the comma-separated set of known instances, each at most
// once, in workspaceProviderNames order.
func parseWorkspaceProviders(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return []string{WorkspaceProviderGitHub}, nil
	}
	var names []string
	for raw := range strings.SplitSeq(value, ",") {
		name := strings.TrimSpace(raw)
		if !slices.Contains(workspaceProviderNames, name) {
			return nil, fmt.Errorf("platform.workspaces.provider %q: want %s, several comma-separated; %q is none of them (`agentlab configure --defaults --workspaces-provider <instances>` rewrites it)", value, strings.Join(workspaceProviderNames, ", "), name)
		}
		if slices.Contains(names, name) {
			return nil, fmt.Errorf("platform.workspaces.provider %q names %s twice", value, name)
		}
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int {
		return slices.Index(workspaceProviderNames, a) - slices.Index(workspaceProviderNames, b)
	})
	return names, nil
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

// GitHub configures the MCPServer `github` the lab registers with muster:
// GitHub's hosted MCP server behind a pinned GitHub authorization server, the
// shape an installation declares. The OAuth client (an OAuth App, or a
// GitHub App's client) is a Kubernetes Secret in the lab the operator's
// secret tooling places; agentlab reads its key names only, never a value.
type GitHub struct {
	// On, `agentlab platform` registers the server; off, it removes it.
	Enabled bool `yaml:"enabled"`
	// Secret is the OAuth client's Secret, keys GitHubClientIDKey and
	// GitHubClientSecretKey. Empty fields mean DefaultGitHubSecretName in
	// DefaultGitHubSecretNamespace (the MCPServer's own namespace).
	Secret SecretRef `yaml:"secret,omitempty"`
}

// AIKey is where the Anthropic key of the default ModelConfig comes from:
// never this file. Source names where the operator's secret tooling finds
// it; `agentlab up` and `platform` hand the reference to that tooling, which
// writes the value from there straight into the lab's Secret in its own
// process (internal/lab/anthropic.go), so the lab carries the key after
// every recreate without a manual step and agentlab never reads a value.
type AIKey struct {
	// Source is a reference `beekeeper secret copy` resolves: a field of the
	// shared vault (op://<vault>/<item>/<field>) or one value of a SOPS file
	// (<file>#<dotted.path>, sops:// in front optional). Empty means the key
	// comes from $ANTHROPIC_API_KEY at deploy time, else a placeholder.
	Source string `yaml:"source,omitempty"`
}

// Validate refuses a source that is not a reference: a value pasted into
// the file (a key, or anything that is neither an op:// field nor a SOPS
// path) must never reach agentlab.yaml.
func (k AIKey) Validate() error {
	if k.Source == "" {
		return nil
	}
	return ValidateSecretSource(k.Source)
}

// ValidateSecretSource is the one home of the secret-reference rule; the huh
// form uses it directly as an input validator.
func ValidateSecretSource(s string) error {
	if strings.ContainsAny(s, " \t\n") {
		return fmt.Errorf("a reference carries no whitespace")
	}
	if rest, ok := strings.CutPrefix(s, "op://"); ok {
		if parts := strings.Split(rest, "/"); len(parts) != 3 || slices.Contains(parts, "") {
			return fmt.Errorf("an op:// reference is op://<vault>/<item>/<field>")
		}
		return nil
	}
	file, path, ok := strings.Cut(strings.TrimPrefix(s, "sops://"), "#")
	if !ok || file == "" || path == "" {
		return fmt.Errorf("a reference, never a value: op://<vault>/<item>/<field> or <file>#<dotted.path>")
	}
	return nil
}

// SecretRef names a Kubernetes Secret.
type SecretRef struct {
	Name      string `yaml:"name,omitempty"`
	Namespace string `yaml:"namespace,omitempty"`
}

// The OAuth client Secret platform.github reads by default, and the keys
// muster's clientCredentialsSecretRef reads it by (its own defaults).
const (
	DefaultGitHubSecretName      = "github-oauth-client" // #nosec G101 -- Secret NAME, not a credential
	DefaultGitHubSecretNamespace = "agent-platform"
	GitHubClientIDKey            = "client-id"
	GitHubClientSecretKey        = "client-secret" // #nosec G101 -- Secret KEY name, not a credential
)

// ClientSecret is the OAuth client Secret the lab looks for and the
// MCPServer references: platform.github.secret, the defaults filling what
// it leaves empty.
func (g GitHub) ClientSecret() SecretRef {
	ref := g.Secret
	if ref.Name == "" {
		ref.Name = DefaultGitHubSecretName
	}
	if ref.Namespace == "" {
		ref.Namespace = DefaultGitHubSecretNamespace
	}
	return ref
}

// GitHubSignIn configures the `github` connector of the lab Dex: GitHub as a
// second identity source next to the static users, through a GitHub App (or
// OAuth App) whose callback is the lab Dex's own callback URL and whose
// permissions are what a sign-in reads (the person's profile, email and
// organization membership; no repository permission). The client id is
// public and lives here; the client secret is a Kubernetes Secret in the
// dex namespace the operator's secret tooling places, which the Dex pod
// reads as an environment variable — agentlab reads the Secret's key names
// only, never a value, and renders no literal.
type GitHubSignIn struct {
	// On, `agentlab up`, `platform` and `reload` render the connector into
	// the Dex config once the client Secret is in place; off, the lab's Dex
	// knows its local users only.
	Enabled bool `yaml:"enabled"`
	// ClientID is the App's client id, as GitHub shows it on the App's
	// settings page. Required while on.
	ClientID string `yaml:"clientId,omitempty"`
	// Secret names the Secret in GitHubSignInSecretNamespace that carries the
	// client secret under GitHubClientSecretKey. Empty means
	// DefaultGitHubSignInSecretName.
	Secret string `yaml:"secret,omitempty"`
	// Orgs restricts the sign-in to members of these GitHub organizations
	// and puts the person's teams in them on the token as groups
	// (`<org>:<team-slug>`). Empty: any GitHub account signs in, with no
	// groups.
	Orgs []string `yaml:"orgs,omitempty"`
}

// The client Secret platform.githubSignIn reads by default. It lives in the
// Dex namespace: the Dex pod reads it as an environment variable, which a
// pod can only do from a Secret of its own namespace.
const (
	DefaultGitHubSignInSecretName = "github-signin-client" // #nosec G101 -- Secret NAME, not a credential
	GitHubSignInSecretNamespace   = "dex"
	// GitHubSignInSecretEnv is the environment variable the Dex pod reads
	// the client secret as; the rendered connector references it
	// (`clientSecret: $GITHUB_SIGNIN_CLIENT_SECRET`), and Dex expands it.
	GitHubSignInSecretEnv = "GITHUB_SIGNIN_CLIENT_SECRET" // #nosec G101 -- env var NAME, not a credential
)

// ClientSecret is the client Secret the lab looks for and the Dex pod
// references: platform.githubSignIn.secret in the Dex namespace, the default
// name filling an empty one.
func (g GitHubSignIn) ClientSecret() SecretRef {
	name := g.Secret
	if name == "" {
		name = DefaultGitHubSignInSecretName
	}
	return SecretRef{Name: name, Namespace: GitHubSignInSecretNamespace}
}

// GitHubSignInEnabled reports whether the lab Dex gets the GitHub connector.
// Dex is part of every lab, so nothing else gates it.
func (c *Config) GitHubSignInEnabled() bool { return c.Platform.GitHubSignIn.Enabled }

// GitHubSignInCallbackURL is the connector's redirect URI, the lab Dex's own
// callback — the URL registered on the App. A loopback URL works there
// because GitHub only redirects the browser to it; the App's callback list
// (ten entries) is how one App serves several labs.
func (c *Config) GitHubSignInCallbackURL() string { return c.Issuer() + "/callback" }

// gitHubOrgRe is a GitHub organization login: alphanumerics and single
// hyphens, no leading or trailing hyphen.
var gitHubOrgRe = regexp.MustCompile(`^[A-Za-z0-9]+(-[A-Za-z0-9]+)*$`)

// Validate checks the sign-in block: a client id while on, a Secret name
// the apiserver accepts, organization logins GitHub accepts.
func (g GitHubSignIn) Validate() error {
	if g.Enabled && strings.TrimSpace(g.ClientID) == "" {
		return fmt.Errorf("clientId is required while enabled (the App's client id, from its settings page)")
	}
	if g.ClientID != strings.TrimSpace(g.ClientID) || strings.ContainsAny(g.ClientID, " \t\n") {
		return fmt.Errorf("clientId %q: must not contain whitespace", g.ClientID)
	}
	if err := ValidateClusterName(g.ClientSecret().Name); err != nil {
		return fmt.Errorf("secret %q: %w", g.Secret, err)
	}
	for _, org := range g.Orgs {
		if !gitHubOrgRe.MatchString(org) {
			return fmt.Errorf("orgs: %q is not a GitHub organization login", org)
		}
	}
	return nil
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
	// A first `agentlab configure` turns it on when a host model server
	// answers (and leaves it off when none does); from then on the recorded
	// choice stands — a lab configured without managed models stays without
	// them however many servers answer — until --model-manager pins it or
	// the form changes it. Off whenever the agents runtime is off.
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
// by an explicit endpoint (a server elsewhere on the LAN), in canonical order.
// The flag follows the discovery on a first configure only (first: no
// agentlab.yaml yet): managed models go on when there is at least one backend
// and the agents runtime is on. A later configure keeps the recorded choice,
// so `configure --defaults` never turns on what the lab was configured
// without — except that the agents runtime off turns them off, since
// model-manager wires models into it. pinBackends (--model-manager-backends)
// replaces the list outright; pinEnabled (--model-manager) decides the flag
// over both — a block on without a backend falls back to the Ollama default,
// so the platform preflight reports the real reachability error.
func (m *ModelManager) ApplyDiscovered(found []string, agents, first bool, pinEnabled *bool, pinBackends []string) {
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
	switch {
	case pinEnabled != nil:
		m.Enabled = *pinEnabled
	case first:
		m.Enabled = agents && len(m.Backends) > 0
	default:
		m.Enabled = m.Enabled && agents
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
	// OpenAI provider only: the ModelConfig's openAI.apiFormat, the HTTP API
	// the model is called through — the key agent-platform-connectivity's
	// kagent.modelConfigs[] entries carry. `responses` for a model a codex
	// agent uses (the codex runtime speaks the Responses API only);
	// `chatCompletions` is the CRD's default.
	APIFormat string `yaml:"apiFormat,omitempty"`
	// OpenAI provider only: the ModelConfig's openAI.credentialType.
	// `chatgptOAuth` runs the model on a ChatGPT subscription login instead
	// of an API key: the Secret kagent-<name> holds ChatGPTLoginKeys of a
	// `codex login --device-auth`, placed by the operator's secret tooling,
	// never by agentlab. It serves codex only, so it needs apiFormat
	// responses.
	CredentialType string `yaml:"credentialType,omitempty"`
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
// the kagent line (api.kagent.dev/v1alpha3).
var ReasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// APIFormats is the ModelConfig CRD's enum for openAI.apiFormat.
var APIFormats = []string{"chatCompletions", APIFormatResponses}

// APIFormatResponses is openAI.apiFormat for the Responses API.
const APIFormatResponses = "responses"

// CredentialTypes is the ModelConfig CRD's enum for openAI.credentialType.
var CredentialTypes = []string{"apiKey", CredentialTypeChatGPT}

// CredentialTypeChatGPT is openAI.credentialType for a ChatGPT subscription
// login.
const CredentialTypeChatGPT = "chatgptOAuth" // #nosec G101 -- the CRD's enum value, no credential

// ChatGPTLoginLabel opts a login Secret in to kagent's refresher, which
// rotates its tokens in place; kagent refuses a login Secret without it.
const ChatGPTLoginLabel = "kagent.dev/chatgpt-login"

// ChatGPTLoginLabelValue is ChatGPTLoginLabel's opt-in value.
const ChatGPTLoginLabelValue = "true"

// ChatGPTLoginKeys are the keys of a ChatGPT login's Secret.
var ChatGPTLoginKeys = []string{"access_token", "refresh_token", "account_id"}

// IsChatGPTLoginSecret reports whether a Secret's labels opt it in to
// kagent's login refresher.
func IsChatGPTLoginSecret(labels map[string]string) bool {
	return labels[ChatGPTLoginLabel] == ChatGPTLoginLabelValue
}

// ChatGPTLogin reports whether the model runs on a ChatGPT login.
func (m ExtraModel) ChatGPTLogin() bool { return m.CredentialType == CredentialTypeChatGPT }

// SecretName is the Kubernetes Secret (in ns kagent) holding this model's key.
func (m ExtraModel) SecretName() string { return "kagent-" + m.Name }

// SecretKey is the key inside the Secret — the provider's canonical env var
// name. Empty for keyless providers (no Secret attached at all) and for a
// ChatGPT login, whose Secret holds ChatGPTLoginKeys.
func (m ExtraModel) SecretKey() string {
	if m.ChatGPTLogin() {
		return ""
	}
	return ModelProviders[m.Provider]
}

// NeedsSecret reports whether this model's ModelConfig references a Secret.
func (m ExtraModel) NeedsSecret() bool { return m.ChatGPTLogin() || m.SecretKey() != "" }

// PlatformTLS is an externally provisioned certificate for the edge; see
// Platform.TLS.
type PlatformTLS struct {
	CertFile string `yaml:"certFile,omitempty"`
	KeyFile  string `yaml:"keyFile,omitempty"`
}

// Set reports whether an external edge certificate is configured.
func (t PlatformTLS) Set() bool { return t.CertFile != "" }

// covers checks that the pair's leaf (the first certificate of CertFile)
// is valid for host: Dex serves the same pair under dex.<domain>.
func (t PlatformTLS) covers(host string) error {
	raw, err := os.ReadFile(t.CertFile)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("%s holds no PEM certificate", t.CertFile)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("%s: %w", t.CertFile, err)
	}
	if err := leaf.VerifyHostname(host); err != nil {
		return fmt.Errorf("%s does not cover %s, the Dex issuer's host under platform.domain (SAN: %s): mint the pair for *.<domain> and the apex", t.CertFile, host, strings.Join(leaf.DNSNames, ", "))
	}
	return nil
}

type Backstage struct {
	Enabled bool `yaml:"enabled"`
	// Backstage binds this port on the node (hostNetwork) and kind maps the
	// same number onto the host, so the URL is identical on both sides.
	// The image is not configured here: the agent-platform chart's backstage
	// component (its version range, resolved by the chart's Flux) decides the
	// version; platform.devImages swaps in a build of your own.
	Port int `yaml:"port"`
	// Portal is the portal mode the lab's app-config overlay renders:
	// PortalAgentPlatform (the Agent Platform shell, app.portals.agentPlatform
	// on) or PortalClassic (the classic Platform Portal alone). The overlay
	// is an app-config file Backstage deep-merges last, so the switch never
	// restates a chart list such as backstage.extraEnvVars.
	Portal string `yaml:"portal"`
	// InstallationsPage enables the portal's Installations page and its
	// sidebar entry (page:gs/installations, nav-item:gs/installations), which
	// the agent-platform chart turns off and the Backstage app ships off: the
	// portal's installations e2e specs drive it (AGENTLAB_INSTALLATIONS_PAGE=1).
	InstallationsPage bool `yaml:"installationsPage,omitempty"`
}

// The Backstage portal modes (backstage.portal).
const (
	PortalAgentPlatform = "agent-platform"
	PortalClassic       = "classic"
)

// PortalModes lists the portal modes, the default first.
var PortalModes = []string{PortalAgentPlatform, PortalClassic}

// AgentPlatformPortal reports whether the portal serves the Agent Platform
// shell (app.portals.agentPlatform).
func (b Backstage) AgentPlatformPortal() bool { return b.Portal != PortalClassic }

type Config struct {
	// Kind cluster name; also prefixes RBAC bindings and names the muster
	// installation surfaced in Backstage.
	ClusterName string `yaml:"clusterName"`
	// Dex NodePort == host port: the issuer (Issuer) must
	// be the same URL from the Mac and from inside the node, so both sides
	// use one number. Must sit in the NodePort range (30000-32767).
	DexPort  int    `yaml:"dexPort"`
	DexImage string `yaml:"dexImage"`
	// APIServerPort is the host port (on 127.0.0.1) of the cluster's
	// Kubernetes API server. Fixed, so a recreated lab keeps its address:
	// an agent sandbox's allow list names it as 127.0.0.1:<port>, and kind
	// would otherwise pick a random one at every `kind create`.
	APIServerPort int `yaml:"apiServerPort"`

	// SubstrateNodes adds that many kind worker nodes reserved for Agent
	// Substrate: labelled and tainted SubstrateNodeKey, they carry the atelet
	// DaemonSet and the WorkerPool's workers and nothing else, so stopping one
	// removes exactly what a spot interruption removes — the node-loss proofs
	// of paused actors' snapshots. 0, the default, is the single-node lab.
	// Fixed at `kind create`, like every node of the cluster.
	SubstrateNodes int `yaml:"substrateNodes,omitempty"`

	// The Claude model both AI consumers use: the platform agents' default
	// ModelConfig (kagent) and Backstage's ai-chat. The API key is NOT config:
	// it lands only in Kubernetes Secrets, never in this file or in rendered
	// manifests — from the source AIKey records, else $ANTHROPIC_API_KEY at
	// deploy time, else a placeholder.
	AIModel string `yaml:"aiModel"`
	// AIKey is where the key of the default ModelConfig comes from.
	AIKey AIKey `yaml:"aiKey,omitempty"`
	// GitHubToken is where the token of the lab's calls to GitHub comes from.
	GitHubToken GitHubToken `yaml:"githubToken,omitempty"`
	// GitHubApp is the skills GitHub App agent-manager resolves skills with.
	GitHubApp GitHubApp `yaml:"githubApp,omitempty"`

	Users     []User    `yaml:"users"`
	Platform  Platform  `yaml:"platform"`
	Backstage Backstage `yaml:"backstage"`
}

// Default returns the canonical lab setup: the agent platform enabled (it is
// what the lab exists to test) with the agentgateway edge and Backstage — the
// real platform topology — three users, and Dex on 32000.
func Default() *Config {
	return &Config{
		ClusterName:   DefaultClusterName,
		DexPort:       DefaultDexPort,
		APIServerPort: DefaultAPIServerPort,
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
			Portal:  PortalAgentPlatform,
		},
	}
}

// Load reads agentlab.yaml. A missing file is reported as os.ErrNotExist so
// callers can decide whether to fall back to the form or to defaults.
func Load() (*Config, error) {
	return load(true)
}

// LoadForConfigure reads agentlab.yaml like Load without validating it:
// configure validates the result after its flags apply, so a file an earlier
// agentlab wrote in a shape this one refuses is repaired by the configure the
// refusal names.
func LoadForConfigure() (*Config, error) {
	return load(false)
}

func load(validate bool) (*Config, error) {
	raw, err := os.ReadFile(File)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	if err := decodeStrict(raw, cfg); err != nil {
		return nil, err
	}
	cfg.Platform.RetiredFakeFleet = nil
	cfg.Platform.RetiredPlatformManager = nil
	// Earlier versions wrote the one-backend form (backend/endpoint); read
	// it as the one-item lists so the same lab renders exactly as before.
	cfg.Platform.ModelManager.normalize()
	if validate {
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", File, err)
		}
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

// CheckValuesFiles refuses a platform.valuesFiles entry that cannot be read.
// It is not part of Validate: only the platform install merges the overlays,
// so `down`, `status` and `configure` work on a lab whose overlay is gone. The
// error names the entry and never wraps fs.ErrNotExist, which Load's callers
// read as "no agentlab.yaml".
func (c *Config) CheckValuesFiles() error {
	for i, path := range c.Platform.ValuesFiles {
		if _, err := os.Stat(path); err != nil {
			reason := err.Error()
			if pathErr := (*fs.PathError)(nil); errors.As(err, &pathErr) {
				reason = pathErr.Err.Error()
			}
			return fmt.Errorf("platform.valuesFiles[%d]: %s: %s", i, path, reason)
		}
	}
	return nil
}

// Peek reads the agentlab.yaml of the lab in dir without validating,
// re-hashing or writing anything — `agentlab list`'s look at a lab that is
// not the one the command runs against. It writes nothing, so it reads past
// a field this release does not know (decodeTolerant).
func Peek(dir string) (*Config, error) {
	raw, err := os.ReadFile(filepath.Join(dir, File)) // #nosec G304 -- a registered lab directory
	if err != nil {
		return nil, err
	}
	cfg := Default()
	if _, err := decodeTolerant(raw, cfg); err != nil {
		return nil, err
	}
	cfg.Normalize()
	return cfg, nil
}

// LoadReadOnly reads agentlab.yaml for a command that never writes it
// (`trust`, `status`, `open`, …): like Load, except that a field this release
// does not know — the file was written by a newer agentlab — is read past and
// returned as the note to show, not refused. The strict refusal exists so a
// write never drops such a field; a command that writes nothing cannot, and
// the person can still trust, inspect and open a lab a newer build wrote.
// Nothing is re-hashed or written.
func LoadReadOnly() (cfg *Config, note string, err error) {
	raw, err := os.ReadFile(File)
	if err != nil {
		return nil, "", err
	}
	cfg = Default()
	note, err = decodeTolerant(raw, cfg)
	if err != nil {
		return nil, "", err
	}
	cfg.Platform.RetiredFakeFleet = nil
	cfg.Platform.RetiredPlatformManager = nil
	cfg.Platform.ModelManager.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, "", fmt.Errorf("%s: %w", File, err)
	}
	return cfg, note, nil
}

// decodeTolerant is decodeStrict for a reader that writes nothing: a field
// this release does not know is skipped, and note says which, who wrote the
// file and how to get that release. Any other error is decodeStrict's.
func decodeTolerant(raw []byte, cfg *Config) (note string, err error) {
	strict := decodeStrict(raw, cfg)
	if strict == nil || !strings.Contains(strict.Error(), "not found in type") {
		return "", strict
	}
	*cfg = *Default()
	cfg.ClusterName = ""
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return "", fmt.Errorf("parsing %s: %w", File, err)
	}
	return fmt.Sprintf("%s names fields this agentlab release (%s) does not know — %s; read without them, nothing is written. %s.",
		File, project.Version(), writerOf(raw), capitalize(updateHint(writtenBy(raw)))), nil
}

// capitalize upper-cases the first letter of s, for a hint that starts a
// sentence.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Exists reports whether a lab's agentlab.yaml is at path (a link to one
// included).
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// RebasePaths makes every relative path in the agentlab.yaml at file —
// platform.chartPath, platform.valuesFiles, platform.tls's certFile and
// keyFile, platform.vmManager.imageDir — absolute against dir, the lab
// directory they are relative to, and writes the file when any changed: the
// step before a lab moves out of dir. A file with relative paths and a field
// this release does not know is refused and left as it is: the write would
// drop the field.
func RebasePaths(file, dir string) error {
	raw, err := os.ReadFile(file) // #nosec G304 -- a lab's agentlab.yaml
	if err != nil {
		return err
	}
	cfg := Default()
	note, err := decodeTolerant(raw, cfg)
	if err != nil {
		return err
	}
	changed := false
	abs := func(p *string) {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
			changed = true
		}
	}
	abs(&cfg.Platform.ChartPath)
	for i := range cfg.Platform.ValuesFiles {
		abs(&cfg.Platform.ValuesFiles[i])
	}
	abs(&cfg.Platform.TLS.CertFile)
	abs(&cfg.Platform.TLS.KeyFile)
	abs(&cfg.Platform.VMManager.ImageDir)
	if !changed {
		return nil
	}
	if note != "" {
		// A write would drop the fields this release does not know.
		return fmt.Errorf("%s names fields this agentlab release does not know, so it is not rewritten", File)
	}
	return cfg.writeTo(file)
}

// decodeStrict reads agentlab.yaml into cfg and refuses a field this release
// does not know. Every run writes the file back (Load re-hashes passwords,
// configure and platform record what they found), so a lenient decode would
// drop such a field silently and the next `platform` would uninstall what it
// configured — a newer release's switch, say. The refusal names the field, the
// agentlab that wrote the file (its header records it) and the fix: the
// self-update that installs that release, or removing the field. Every other
// field defaults; clusterName never does.
func decodeStrict(raw []byte, cfg *Config) error {
	// The file names its lab: Default's clusterName is the first lab's, and
	// a file without one would silently act on that lab's cluster.
	cfg.ClusterName = ""
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	err := dec.Decode(cfg)
	switch {
	case err == nil, errors.Is(err, io.EOF):
		if cfg.ClusterName == "" {
			return fmt.Errorf("%s names no clusterName, the lab it belongs to: add `clusterName: <the lab's name>` (`agentlab list` shows the labs on this machine)", File)
		}
		return nil
	case strings.Contains(err.Error(), "not found in type"):
		return fmt.Errorf("%s names a field this agentlab release (%s) does not know (%w) — %s, and this run would drop the field on its next write; %s, or remove the field", File, project.Version(), err, writerOf(raw), updateHint(writtenBy(raw)))
	default:
		return fmt.Errorf("parsing %s: %w", File, err)
	}
}

// writtenByPrefix starts the header line that records which agentlab wrote
// the file. A comment, not a field: a release from before the line reads the
// file as it always did, and one that does not know a field still finds it.
const writtenByPrefix = "# Written by agentlab "

// writtenBy is the agentlab version the header of raw names, "" when the file
// has no such line (written by hand, or by a release from before it).
func writtenBy(raw []byte) string {
	for line := range strings.Lines(string(raw)) {
		if !strings.HasPrefix(line, "#") {
			// The header ends at the first line that is no comment.
			return ""
		}
		if v, ok := strings.CutPrefix(line, writtenByPrefix); ok {
			return strings.TrimSuffix(strings.TrimSpace(v), ".")
		}
	}
	return ""
}

// writerOf says who wrote raw, for the refusal of a field this release does
// not know.
func writerOf(raw []byte) string {
	if v := writtenBy(raw); v != "" {
		return "the file was written by agentlab " + v
	}
	return "the file was written by a newer release"
}

// updateHint is how to get the release which wrote the file: self-update
// installs the newest release, and a release candidate only comes with --pre,
// which follows them. A development build was installed from no release.
func updateHint(version string) string {
	v, err := semver.NewVersion(version)
	switch {
	case version != "" && err != nil:
		return "install a build of the same checkout"
	case err == nil && v.Prerelease() != "":
		return "`agentlab self-update --pre` installs it or a later release (" + version + " is a release candidate, which only --pre follows)"
	case err == nil:
		return "`agentlab self-update` installs it or a later release"
	default:
		return "update agentlab with `agentlab self-update` (" + project.Source + "/releases)"
	}
}

func (c *Config) Save() error {
	if _, err := c.EnsureHashes(); err != nil {
		return err
	}
	return c.write()
}

// write persists the config as-is; Save and Load ensure the hashes first
// (exactly once — bcrypt-comparing every user is not free). An existing
// agentlab.yaml is updated in place (mergeInto): only the values that changed
// are rewritten, so a one-flag `configure` changes that one line.
func (c *Config) write() error {
	return c.writeTo(File)
}

// writeTo is write to the agentlab.yaml at path.
func (c *Config) writeTo(path string) error {
	old, err := os.ReadFile(path) // #nosec G304 -- a lab's agentlab.yaml
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	out, err := mergeInto(old, c)
	if err != nil {
		return err
	}
	header := []byte(headerFirstLine + "\n" +
		writtenByPrefix + project.Version() + ".\n" +
		headerPasswordsLine + "\n" +
		headerManifestsLine + "\n")
	return os.WriteFile(path, append(header, out...), 0o600) // #nosec G703 -- a lab's agentlab.yaml
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
	if m.APIFormat != "" {
		if m.Provider != ProviderOpenAI {
			return fmt.Errorf("%s: apiFormat applies to the OpenAI provider only (the ModelConfig's openAI.apiFormat)", m.Name)
		}
		if !slices.Contains(APIFormats, m.APIFormat) {
			return fmt.Errorf("%s: unknown apiFormat %q (one of %s)", m.Name, m.APIFormat, strings.Join(APIFormats, ", "))
		}
	}
	if m.CredentialType != "" {
		if m.Provider != ProviderOpenAI {
			return fmt.Errorf("%s: credentialType applies to the OpenAI provider only (the ModelConfig's openAI.credentialType)", m.Name)
		}
		if !slices.Contains(CredentialTypes, m.CredentialType) {
			return fmt.Errorf("%s: unknown credentialType %q (one of %s)", m.Name, m.CredentialType, strings.Join(CredentialTypes, ", "))
		}
	}
	if m.ChatGPTLogin() {
		switch {
		case m.APIFormat != APIFormatResponses:
			return fmt.Errorf("%s: a ChatGPT login serves codex only, which needs apiFormat %s", m.Name, APIFormatResponses)
		case m.APIKeyEnv != "":
			return fmt.Errorf("%s: a ChatGPT login takes no apiKeyEnv (its Secret %s is placed by the operator's secret tooling)", m.Name, m.SecretName())
		case m.BaseURL != "":
			return fmt.Errorf("%s: a ChatGPT login takes no baseUrl (kagent calls the ChatGPT Codex backend)", m.Name)
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
	// A file written before the switch existed: the chart's own mode.
	if c.Backstage.Portal == "" {
		c.Backstage.Portal = PortalAgentPlatform
	}
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
	if c.APIServerPort < 1 || c.APIServerPort > 65535 {
		return fmt.Errorf("apiServerPort: %d is not a TCP port", c.APIServerPort)
	}
	if err := ValidateAIModel(c.AIModel); err != nil {
		return fmt.Errorf("aiModel %q: %w", c.AIModel, err)
	}
	if err := c.AIKey.Validate(); err != nil {
		return fmt.Errorf("aiKey.source %q: %w", c.AIKey.Source, err)
	}
	if err := c.GitHubToken.Validate(); err != nil {
		return fmt.Errorf("githubToken.source %q: %w", c.GitHubToken.Source, err)
	}
	if err := c.SkillsGitHubApp().Validate(); err != nil {
		return fmt.Errorf("githubApp: %w", err)
	}
	if c.GitHubApp.Configured() && c.GitHubToken.Source != "" {
		return errors.New("githubApp and githubToken.source both set: agent-manager takes one GitHub credential, its chart refuses both — clear one (`agentlab configure --github-token-source \"\"`)")
	}
	if c.SubstrateNodes < 0 || c.SubstrateNodes > MaxSubstrateNodes {
		return fmt.Errorf("substrateNodes: %d, want 0 (the single-node lab) to %d", c.SubstrateNodes, MaxSubstrateNodes)
	}
	if c.SubstrateNodes > 0 && (!c.Platform.Agents || c.LegacyChart()) {
		return fmt.Errorf("substrateNodes: %d needs Agent Substrate, which only the agents of the 4.x line run (platform.agents on, no 3.x chart)", c.SubstrateNodes)
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
	if a := c.Platform.GatewayAddress; a != "" {
		if ip := net.ParseIP(a); ip == nil || ip.To4() == nil || !ip.IsLoopback() {
			return fmt.Errorf("platform.gatewayAddress %q: want an IPv4 loopback address (127.0.0.0/8)", a)
		}
	}
	if (c.Platform.TLS.CertFile == "") != (c.Platform.TLS.KeyFile == "") {
		return fmt.Errorf("platform.tls: certFile and keyFile must be set together")
	}
	ghSecret := c.Platform.GitHub.ClientSecret()
	if err := ValidateClusterName(ghSecret.Name); err != nil {
		return fmt.Errorf("platform.github.secret.name %q: %w", ghSecret.Name, err)
	}
	if err := ValidateClusterName(ghSecret.Namespace); err != nil {
		return fmt.Errorf("platform.github.secret.namespace %q: %w", ghSecret.Namespace, err)
	}
	if err := c.Platform.GitHubSignIn.Validate(); err != nil {
		return fmt.Errorf("platform.githubSignIn: %w", err)
	}
	if c.Platform.TLS.Set() {
		for _, p := range []string{c.Platform.TLS.CertFile, c.Platform.TLS.KeyFile} {
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("platform.tls: %w", err)
			}
		}
		if err := c.Platform.TLS.covers(c.DexHost()); err != nil {
			return fmt.Errorf("platform.tls: %w", err)
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
	if _, ok := c.Platform.DevImages[DevImageSubstrateAPI]; ok && !c.Platform.Agents {
		return fmt.Errorf("platform.devImages.%s: Agent Substrate comes with the agents (platform.agents: true)", DevImageSubstrateAPI)
	}
	if _, ok := c.Platform.DevImages[DevImageMCPPrometheus]; ok && !c.Platform.Observability {
		return fmt.Errorf("platform.devImages.%s: mcp-prometheus comes with the observability stack (platform.observability: true) — the override would swap the image of nothing", DevImageMCPPrometheus)
	}
	if v := c.Platform.MCPPrometheusChartVersion; v != "" {
		if !c.Platform.Observability {
			return fmt.Errorf("platform.mcpPrometheusChartVersion: mcp-prometheus comes with the observability stack (platform.observability: true) — the override would pin the chart of nothing")
		}
		if !exactVersionRe.MatchString(v) {
			return fmt.Errorf("platform.mcpPrometheusChartVersion %q: must be an exact version (e.g. 0.8.1-rc.1), not a range — the lab never floats", v)
		}
	}
	if c.Platform.Alertmanager && !c.Platform.Observability {
		return fmt.Errorf("platform.alertmanager requires platform.observability (the Alertmanager is part of the lab's kube-prometheus-stack)")
	}
	if _, ok := c.Platform.DevImages[DevImageKlausGateway]; ok && !c.Platform.KlausGateway.Enabled {
		return fmt.Errorf("platform.devImages.%s: the component is off (platform.klausGateway.enabled: true, or `agentlab configure --klaus-gateway`) — the override would swap the image of nothing", DevImageKlausGateway)
	}
	if _, ok := c.Platform.DevImages[DevImageWorkspaceManager]; ok && !c.Platform.Workspaces.Enabled {
		return fmt.Errorf("platform.devImages.%s: the component is off (platform.workspaces.enabled: true, or `agentlab configure --workspaces`) — the override would swap the image of nothing", DevImageWorkspaceManager)
	}
	// The agentgateway controller runs its proxies from registry, repository
	// and tag: a digest has no field to travel in.
	if ref, ok := c.Platform.DevImages[DevImageAgentgateway]; ok && strings.Contains(ref, "@") {
		return fmt.Errorf("platform.devImages.%s %q: the agentgateway controller names its proxies' image by registry, repository and tag (the chart's proxy.image values), so a digest ref cannot be forwarded — name a tag", DevImageAgentgateway, ref)
	}
	// Swarmgeist is a client of the kagent controller: without the agents
	// runtime the component has nothing to talk to.
	if c.Platform.KlausGateway.Enabled && c.Platform.Enabled && !c.Platform.Agents {
		return fmt.Errorf("platform.klausGateway requires platform.agents (klaus-gateway runs its conversations on the kagent controller)")
	}
	if c.Platform.Coding.Enabled && c.Platform.Enabled && !c.Platform.Agents {
		return fmt.Errorf("platform.coding requires platform.agents (a coding Harness is a kagent Harness)")
	}
	for i, runtime := range c.Platform.Coding.Harnesses {
		if !slices.Contains(CodingRuntimes, runtime) {
			return fmt.Errorf("platform.coding.harnesses: unknown runtime %q (one of %s)", runtime, strings.Join(CodingRuntimes, ", "))
		}
		if slices.Contains(c.Platform.Coding.Harnesses[:i], runtime) {
			return fmt.Errorf("platform.coding.harnesses: %q twice", runtime)
		}
	}
	for runtime, image := range c.Platform.Coding.Images {
		if !slices.Contains(CodingRuntimes, runtime) {
			return fmt.Errorf("platform.coding.images: unknown runtime %q (one of %s)", runtime, strings.Join(CodingRuntimes, ", "))
		}
		if !codingImageRe.MatchString(image) {
			return fmt.Errorf("platform.coding.images.%s: %q is not an image pinned by digest (<repository>@sha256:<64 hex digits>)", runtime, image)
		}
	}
	for _, runtime := range CodingRuntimes {
		target := CodingHarnessDevImage(runtime)
		if _, ok := c.Platform.DevImages[target]; ok && !c.CodingHarnessRendered(runtime) {
			return fmt.Errorf("platform.devImages.%s: the lab renders no Harness %s (platform.coding.enabled with %s in platform.coding.harnesses) — the override would swap the image of nothing", target, runtime, runtime)
		}
	}
	// The kagent controller compiles the claude and codex Harnesses' pinned
	// CLI version into their config with StrictVersion: a harness built from
	// another commit of the kagent line than the lab's controller refuses to
	// start on the CLI version mismatch, and the golden boot loops.
	for _, runtime := range CodingRuntimes {
		pin, pinned := codingHarnessVersionPins[runtime]
		target := CodingHarnessDevImage(runtime)
		if _, ok := c.Platform.DevImages[target]; ok && pinned && c.Platform.DevImages["kagent"] == "" {
			return fmt.Errorf("platform.devImages.%s needs platform.devImages.kagent from the same build of the kagent line: the controller compiles its pinned CLI version (%s) into the Harness with StrictVersion, so a harness whose CLI differs from the lab controller's pin refuses to start on the version mismatch — build the controller from the same giantswarm/kagent-upstream checkout and name it under platform.devImages.kagent", target, pin)
		}
	}
	if c.CodingHarnessRendered(CodingRuntimeOmp) && c.CodingHarnessImage(CodingRuntimeOmp) == "" && c.Platform.DevImages[DevImageOmpHarness] == "" {
		return fmt.Errorf("platform.coding.harnesses names %s, whose Harness runs an image the chart has none of its own for: set platform.coding.images.%s (the kagent line's omp-harness by digest, runtimeImages.ompHarness.digest of the release's chart) or platform.devImages.%s", CodingRuntimeOmp, CodingRuntimeOmp, DevImageOmpHarness)
	}
	// kagent.harnesses reaches the kagent chart from codingChartFloor on; an
	// older release drops the key and renders no coding Harness.
	if c.Platform.Coding.Enabled && c.Platform.Enabled {
		if chart, below := c.chartBelow(codingChartFloor); below {
			return fmt.Errorf("platform.coding needs agent-platform %s or newer (kagent.harnesses forwarded to the kagent chart); %s — `agentlab configure --coding=false`", codingChartFloor, chart)
		}
	}
	if c.Platform.Tenants.Enabled && c.Platform.Enabled && !c.Platform.Agents {
		return fmt.Errorf("platform.tenants requires platform.agents (the tenant reconciler is agent-manager's, and a tenant's agents run on kagent)")
	}
	if c.Platform.Serving.Enabled && c.Platform.Enabled && !c.Platform.Agents {
		return fmt.Errorf("platform.serving requires platform.agents (model-manager wires a served model into kagent as a ModelConfig)")
	}
	// The serving switch turns on the llm-d components alone; a pinned
	// release before servingChartFloor would refuse the llm-d controller's
	// release without kserve-resources and fail the install out of sight.
	if c.Platform.Serving.Enabled && c.Platform.Enabled {
		if chart, below := c.chartBelow(servingChartFloor); below {
			return fmt.Errorf("platform.serving needs agent-platform %s or newer (the llm-d control plane alone, components.kserve-runtime-configs and modelServing.modelsGateway); %s — `agentlab configure --serving=false`", servingChartFloor, chart)
		}
	}
	// Workspaces need the agents runtime: the chart's workspaces live in
	// kagent's namespace, where every Session runs and mounts its workspace,
	// and the workspace-manager applies its sync ServiceAccount and Roles
	// there. The chart refuses the shape at render time; the lab refuses it
	// before anything installs.
	if c.Platform.Workspaces.Enabled && c.Platform.Enabled && !c.Platform.Agents {
		return fmt.Errorf("platform.workspaces requires platform.agents (the workspaces live in kagent's namespace, where every Session runs and mounts its workspace) — `agentlab configure --agents`, or `--workspaces=false`")
	}
	if names, err := parseWorkspaceProviders(c.Platform.Workspaces.Provider); err != nil {
		return err
	} else if grant := c.Platform.Workspaces.GrantProvider; grant == WorkspaceGrantProviderApps && len(names) < 2 {
		return fmt.Errorf("platform.workspaces.grantProvider %q keeps one muster provider App per instance, side by side, and takes two or more instances of platform.workspaces.provider (%s): `agentlab configure --workspaces-provider %s,%s`", grant, strings.Join(names, ", "), WorkspaceProviderGitHub, WorkspaceProviderGitLab)
	} else if grant != "" && grant != WorkspaceGrantProviderApps && !slices.Contains(names, grant) {
		return fmt.Errorf("platform.workspaces.grantProvider %q names no instance of platform.workspaces.provider (%s), nor %q (one muster provider App per instance)", grant, strings.Join(names, ", "), WorkspaceGrantProviderApps)
	}
	if after := c.Platform.Workspaces.SessionsCleanupAfter; after != "" {
		if d, err := time.ParseDuration(after); err != nil || d <= 0 {
			return fmt.Errorf("platform.workspaces.sessionsCleanupAfter %q is no positive Go duration (such as 3m, or 720h for the chart's 30 days)", after)
		}
	}
	if after := c.Platform.Workspaces.SyncRefreshAfter; after != "" {
		if d, err := time.ParseDuration(after); err != nil || d <= 0 {
			return fmt.Errorf("platform.workspaces.syncRefreshAfter %q is no positive Go duration (such as 1m, or 1h for the manager's default)", after)
		}
	}
	if err := c.Platform.Workspaces.GitHub.Validate(); err != nil {
		return err
	}
	if err := c.Platform.Workspaces.GitLab.Validate(); err != nil {
		return err
	}
	// The vm-manager component exists from agent-platform 4.11.0; a pinned
	// release before it would take components.vm-manager as an unknown key
	// and fail the install out of sight. A local checkout or a branch build
	// carries its own answer.
	if c.Platform.VMManager.Enabled && c.Platform.Enabled {
		if chart, below := c.chartBelow(vmManagerChartFloor); below {
			return fmt.Errorf("platform.vmManager needs agent-platform %s or newer (components.vm-manager); %s — `agentlab configure --vm-manager=false`", vmManagerChartFloor, chart)
		}
	}
	if c.Platform.DevRegistryPort != 0 {
		if err := ValidatePort(strconv.Itoa(c.Platform.DevRegistryPort)); err != nil {
			return fmt.Errorf("platform.devRegistryPort: %w", err)
		}
	}
	for i, path := range c.Platform.ValuesFiles {
		if path == "" {
			return fmt.Errorf("platform.valuesFiles[%d]: an empty path", i)
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
	if c.Backstage.Portal != "" && !slices.Contains(PortalModes, c.Backstage.Portal) {
		return fmt.Errorf("backstage.portal: %q is no portal mode (%s)", c.Backstage.Portal, strings.Join(PortalModes, ", "))
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

// TenantsEnabled reports whether the platform treats the Organizations'
// namespaces as tenants: the platform with its agents runtime on the current
// line, and the key on.
func (c *Config) TenantsEnabled() bool {
	return c.Platform.Enabled && c.Platform.Agents && c.Platform.Tenants.Enabled && !c.LegacyChart()
}

// AlertmanagerEnabled reports whether the lab runs the Alertmanager: the
// platform with its observability stack, and the key on.
func (c *Config) AlertmanagerEnabled() bool {
	return c.Platform.Enabled && c.Platform.Observability && c.Platform.Alertmanager
}

// KlausGatewayEnabled reports whether the platform runs the klaus-gateway
// component: the platform with its agents runtime, and the key on.
func (c *Config) KlausGatewayEnabled() bool {
	return c.Platform.Enabled && c.Platform.Agents && c.Platform.KlausGateway.Enabled
}

// MusterReplicas is the number of muster pods the lab runs: two on the pod
// network (Platform.MusterPodNetwork), else one — a host-network pod binds
// its port on the single kind node, so a second one would never start.
func (c *Config) MusterReplicas() int {
	if c.Platform.MusterPodNetwork {
		return 2
	}
	return 1
}

// ClusterManagerEnabled reports whether the lab turns the chart's
// cluster-manager component on: the platform, with a dev image named for it
// (DevImageClusterManager) — the lab's one switch for a component the meta
// chart ships off. An overlay that enables it is the chart's business, not
// this lab's.
func (c *Config) ClusterManagerEnabled() bool {
	_, ok := c.Platform.DevImages[DevImageClusterManager]
	return c.Platform.Enabled && ok
}

// CodingEnabled reports whether the lab renders its coding Harnesses: the
// platform with its agents runtime, and the key on.
func (c *Config) CodingEnabled() bool {
	return c.Platform.Enabled && c.Platform.Agents && c.Platform.Coding.Enabled
}

// CodingHarnesses are the coding runtimes the lab renders a Harness of while
// coding agents are on: platform.coding.harnesses, claude when it is empty.
func (c *Config) CodingHarnesses() []string {
	if len(c.Platform.Coding.Harnesses) == 0 {
		return []string{CodingRuntimeClaude}
	}
	return c.Platform.Coding.Harnesses
}

// CodingHarnessImage is the image platform.coding.images names for a coding
// runtime, "" where the chart's own image of the runtime applies.
func (c *Config) CodingHarnessImage(runtime string) string {
	return c.Platform.Coding.Images[runtime]
}

// CodingHarnessRendered reports whether the lab renders the coding Harness of
// the runtime: one of CodingHarnesses with coding agents on; claude also for
// the workspaces proof's Session, whenever the workspaces are on.
func (c *Config) CodingHarnessRendered(runtime string) bool {
	if c.CodingEnabled() && slices.Contains(c.CodingHarnesses(), runtime) {
		return true
	}
	return runtime == CodingRuntimeClaude && c.WorkspacesEnabled()
}

// CodexModelConfig is the lab's model for a codex agent: the first
// platform.extraModels entry that speaks the Responses API (the only one the
// codex runtime speaks), "" when none does.
func (c *Config) CodexModelConfig() string {
	for _, m := range c.Platform.ExtraModels {
		if m.Provider == ProviderOpenAI && m.APIFormat == APIFormatResponses {
			return m.Name
		}
	}
	return ""
}

// WorkspacesEnabled reports whether the lab installs the workspace storage
// and the chart its workspace-manager: the platform and the key on (Validate
// requires the agents beside them, and Agent Substrate comes with those).
func (c *Config) WorkspacesEnabled() bool {
	return c.Platform.Enabled && c.Platform.Workspaces.Enabled
}

// WorkspacesCSICompanion reports whether the chart's CSI companion is
// Substrate's workspace driver: workspaces (which come with the agents, and
// Substrate with them) and the key on.
func (c *Config) WorkspacesCSICompanion() bool {
	return c.WorkspacesEnabled() && c.Platform.Workspaces.CSICompanion
}

// WorkspaceProviders names the workspace-manager's provider instances, in
// the order it gets them: what platform.workspaces.provider names, github
// alone while it is empty; none with workspaces off. A value Validate
// refused yields none too.
func (c *Config) WorkspaceProviders() []string {
	if !c.WorkspacesEnabled() {
		return nil
	}
	names, err := parseWorkspaceProviders(c.Platform.Workspaces.Provider)
	if err != nil {
		return nil
	}
	return names
}

// WorkspaceGrantProvider is the instance muster pins the workspace-manager's
// registration to: platform.workspaces.grantProvider, or the first instance;
// "" with workspaces off and with WorkspaceProviderApps, which pins none.
func (c *Config) WorkspaceGrantProvider() string {
	names := c.WorkspaceProviders()
	if len(names) == 0 || c.WorkspaceProviderApps() {
		return ""
	}
	if grant := c.Platform.Workspaces.GrantProvider; slices.Contains(names, grant) {
		return grant
	}
	return names[0]
}

// WorkspaceProviderApps reports whether muster keeps one provider App per
// instance, side by side, with the workspace-manager's registration pinned
// to none (platform.workspaces.grantProvider: WorkspaceGrantProviderApps):
// the meta chart derives the Apps, and a person grants each once
// (core_auth_login app=<instance>).
func (c *Config) WorkspaceProviderApps() bool {
	return c.Platform.Workspaces.GrantProvider == WorkspaceGrantProviderApps && len(c.WorkspaceProviders()) > 1
}

// WorkspaceGitHubProvider reports whether the workspace-manager gets the
// provider instance github: workspaces on with platform.workspaces.provider
// naming it.
func (c *Config) WorkspaceGitHubProvider() bool {
	return slices.Contains(c.WorkspaceProviders(), WorkspaceProviderGitHub)
}

// WorkspaceGitLabProvider reports whether the workspace-manager gets the
// provider instance gitlab: workspaces on with platform.workspaces.provider
// naming it.
func (c *Config) WorkspaceGitLabProvider() bool {
	return slices.Contains(c.WorkspaceProviders(), WorkspaceProviderGitLab)
}

// WorkspaceManagerBaseURL is the workspace-manager's public base URL through
// the edge, where it serves its MCP endpoint and the grants' key set.
func (c *Config) WorkspaceManagerBaseURL() string { return c.gatewayURL("workspace-manager") }

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

// LegacyChart reports whether the lab installs a meta chart of the 3.x line.
// The 3.x line's values are a different shape (kagent 0.10 with its bundled
// Postgres, no Agent Substrate, no platform Postgres, a closed root schema
// that refuses the 4.x keys), so the lab values render in that shape for it.
// The chart the lab installs decides, not the channel: a release or a dev
// build (platform.chartBranch, the resolved build in chartVersion) by its
// major below 4, a chart directory (platform.chartPath) by its roster
// (legacyChartDir) — a branch of the 3.x maintenance line is as legacy as
// its releases.
func (c *Config) LegacyChart() bool {
	if c.ChartChannel() == ChartChannelPath {
		return legacyChartDir(c.Platform.ChartPath)
	}
	major := c.ChartMajor()
	return major > 0 && major < 4
}

// chartBelow reports whether the chart the lab installs predates floor, a
// 4.x release, and names that chart for the refusal: a release below it, or
// the 3.x line however it is installed (a dev build of a 3.x-line branch, a
// chart directory with the 3.x roster). A dev build or a chart directory of
// the current line is not checked.
func (c *Config) chartBelow(floor *semver.Version) (string, bool) {
	switch {
	case c.LegacyChart() && c.Platform.ChartPath != "":
		return "platform.chartPath " + c.Platform.ChartPath + " is the 3.x line", true
	case c.LegacyChart():
		return "platform.chartVersion is " + c.Platform.ChartVersion, true
	case c.Platform.ChartPath != "" || c.Platform.ChartBranch != "":
		return "", false
	}
	if v, err := semver.NewVersion(c.Platform.ChartVersion); err == nil && v.LessThan(floor) {
		return "platform.chartVersion is " + c.Platform.ChartVersion, true
	}
	return "", false
}

// valuesFile is a chart's default values.
const valuesFile = "values.yaml"

// legacyChartDir reports whether the chart directory carries the 3.x
// roster: a components block without components.substrate, the component
// the 4.x line added. A checkout's Chart.yaml version is the placeholder the
// release replaces on both lines, so the roster is what tells them apart; a
// directory without a readable components block is the current line.
func legacyChartDir(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, valuesFile)) // #nosec G304 -- the chart directory agentlab.yaml names
	if err != nil {
		return false
	}
	var values struct {
		Components map[string]yaml.Node `yaml:"components"`
	}
	if yaml.Unmarshal(data, &values) != nil || values.Components == nil {
		return false
	}
	_, substrate := values.Components["substrate"]
	return !substrate
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

// Issuer is the Dex issuer URL, one URL on both sides of the kind boundary:
// https://localhost:<DexPort>/dex, or with an externally provisioned pair
// (platform.tls) https://dex.<domain>:<DexPort>/dex, served with that pair.
func (c *Config) Issuer() string {
	return fmt.Sprintf("https://%s:%d/dex", c.DexHost(), c.DexPort)
}

// DexHost is the issuer's host: localhost, or dex.<domain> with an
// externally provisioned pair. A name either way, never an IP literal:
// muster refuses an issuer whose host is one.
func (c *Config) DexHost() string {
	if c.Platform.TLS.Set() {
		return "dex." + c.Platform.Domain
	}
	return "localhost"
}

// ControlPlaneNode is the docker container name kind gives the (only) node.
func (c *Config) ControlPlaneNode() string { return c.ClusterName + "-control-plane" }

// SubstrateNodeKey is the label and the NoSchedule taint (value "true") of
// the substrateNodes workers.
const SubstrateNodeKey = "agentlab.giantswarm.io/substrate-worker"

// MaxSubstrateNodes bounds substrateNodes: every node is a container running
// its own kubelet and containerd on the host's memory, and a node-loss proof
// needs one node to lose and one to land on.
const MaxSubstrateNodes = 3

// SubstrateNodeNames are the container (and Node) names kind gives the
// substrateNodes workers: <cluster>-worker, <cluster>-worker2, …
func (c *Config) SubstrateNodeNames() []string {
	names := make([]string, c.SubstrateNodes)
	for i := range names {
		names[i] = c.ClusterName + "-worker"
		if i > 0 {
			names[i] += strconv.Itoa(i + 1)
		}
	}
	return names
}

// MCPServerName is the MCPServer CR the connectivity chart registers for the
// bundled mcp-kubernetes (mcp-kubernetes.mcpServer.managementCluster): a
// member of muster's kubernetes family, named the way agent-platform-mcps
// names every management cluster's. Muster exposes the family's tools as
// x_kubernetes_<tool>, the argument management_cluster selecting the lab.
// The 3.x line's and an upgrade seed's connectivity chart (FamilylessChart)
// registers the family-less mcp-kubernetes instead, its tools
// x_mcp-kubernetes_<tool>.
func (c *Config) MCPServerName() string {
	if c.FamilylessChart() {
		return "mcp-kubernetes"
	}
	return c.ClusterName + "-mcp-kubernetes"
}

// PrometheusMCPServerName is the lab's mcp-prometheus as a member of the
// prometheus family (the agent-platform-mcps entry's default name):
// x_prometheus_<tool> with management_cluster set to the lab.
func (c *Config) PrometheusMCPServerName() string { return c.ClusterName + "-mcp-prometheus" }

// DefaultGatewayAddress is where the edge is published without
// platform.gatewayAddress.
const DefaultGatewayAddress = "127.0.0.1"

// GatewayHostAddress is the loopback address the edge is published on, the
// address every public hostname of the lab resolves to on the host.
func (c *Config) GatewayHostAddress() string {
	if c.Platform.GatewayAddress != "" {
		return c.Platform.GatewayAddress
	}
	return DefaultGatewayAddress
}

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

// GatewayURL is the public URL of the platform hostname <prefix>.<domain>
// through the edge, from this host (a proof's own fixture behind the edge).
func (c *Config) GatewayURL(prefix string) string { return c.gatewayURL(prefix) }

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
// path-prefixed platform APIs (the kagent controller at /kagent) live here,
// same as the chart's Backstage app-config derives them.
func (c *Config) AgentgatewayBaseURL() string { return c.gatewayURL("agentgateway") }

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
