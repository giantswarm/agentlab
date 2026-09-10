package lab

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/agentlab/internal/config"
)

const platformNamespace = "agent-platform"

// platformRelease is the platform's Helm release name. Also the name of the
// muster installation Backstage surfaces: the chart's app-config registers
// `gs.installations.<release name>` (backstage.installationName defaults to
// it), and the portal proofs pass it as `?installation=`.
const platformRelease = "agent-platform"

// Component names, shared by the log targets, the cert SANs, the deploy
// steps and the dev-image table.
const (
	componentDex           = "dex"
	componentMuster        = "muster"
	componentBackstage     = "backstage"
	componentKagent        = "kagent"
	componentMCPKubernetes = "mcp-kubernetes"
)

// A Kubernetes object's Ready condition, and a condition's status when it
// holds.
const (
	conditionReady = "Ready"
	conditionTrue  = "True"
)

// caCertKey is the data key a CA-bundle Secret carries its PEM under — the
// convention the chart's global.identity.ca, muster's extraCaFile and
// Substrate's actor-id trust anchor share.
const caCertKey = "ca.crt"

// The platform's generated secrets (platformSecretsName): created once and
// then left alone — regenerating the encryption key on every run would
// invalidate every issued token. dex-client-secret must match the
// `agent-platform` staticClient in the Dex config.
const (
	platformSecretsName       = "agent-platform-secrets"
	backstageSessionSecretKey = "backstage-session-secret"
)

// The CoreDNS Deployment the Corefile rewrite rolls, in kube-system.
const (
	kubeSystemNamespace = "kube-system"
	corednsDeployment   = "coredns"
)

// musterMCPServerResource is muster's MCPServer as kubectl's resource
// argument, fully qualified because kagent ships an MCPServer CRD of its own
// (mcpservers.kagent.dev): the bare kind resolves to the wrong API group.
const musterMCPServerResource = "mcpservers.muster.giantswarm.io"

// musterRestartTimeout bounds the rollout of a muster pod the lab replaces.
const musterRestartTimeout = 120 * time.Second

// Leftovers of earlier agentlab versions in the lab's working directory: the
// git-vendored agent-platform-standalone chart and the generated Helm
// post-renderer plugin. Neither has a reader anymore; the platform install
// removes them so a working directory does not carry a dead checkout around.
const (
	legacyVendorDir      = ".vendor"
	legacyHelmPluginsDir = StateDir + "/helm-plugins"
	// Renders of templates that are gone: the old lab's own Flux values and
	// the mcp-prometheus values of its `helm install` (a HelmRelease now).
	legacyFluxValues          = StateDir + "/flux-values.yaml"
	legacyMCPPrometheusValues = StateDir + "/mcp-prometheus-values.yaml"
	// The Flux controllers the old lab installed itself (flux2 chart) for the
	// agent create flow; the chart brings its own engine now and refuses a
	// second Flux. Named here so the refusal and `platform-down` agree.
	legacyFluxNamespace = "flux-system"
	legacyFluxRelease   = "flux"
)

// helmInstallTimeout bounds the upgrade-or-install of the meta chart and its
// wait. The embedded Helm's wait is kstatus over the chart's objects, the
// platform HelmReleases included, so the install returns when every
// component is Ready — a first boot side-loads the images, but the engine
// (source- and helm-controller) still pulls its own on the way.
const helmInstallTimeout = 15 * time.Minute

// PlatformUp installs the Giant Swarm agent platform into the lab cluster and
// wires it to the lab Dex.
//
// The platform is the agent-platform meta chart — the same chart every Giant
// Swarm management cluster runs — in its LAB SHAPE: the chart brings its own
// Flux engine (components.flux.enabled, the flux-engine subchart: Flux
// Operator + one FluxInstance with source- and helm-controller), so `helm
// install` yields a running platform on a cluster with no Flux; and the chart
// does NOT manage itself (gitops.self.enabled: false). Self-management would
// have the bundled helm-controller adopt this release and follow the
// PUBLISHED chart's version range — the lab installs unreleased charts
// (platform.chartPath) and dev images, which that HelmRelease would replace
// with the release it finds in the registry. So Helm keeps owning the
// release: this function is the one writer (an idempotent upgrade-or-install
// through the embedded Helm, no post-renderer, no --force-conflicts), and the
// release is the Helm CLI's too — `helm upgrade` from a shell stays the lab's
// day-2 tool.
func PlatformUp(cfg *config.Config) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	// The standalone entry point skips Up's overlapped preload, so join a
	// synchronous one here: on a live cluster the node filter makes it a
	// no-op, and after a half-failed boot it heals the missing side-loads
	// before the installs start their rollout waits.
	reportPreload(loadLabImages(cfg, pullLabImages(cfg)))
	return platformUp(cfg, "Platform is up.")
}

// platformChart is the chart the embedded Helm installs and renders: the
// pinned release from the registry, or a local chart directory.
type platformChart struct {
	// ref is what Helm takes as the chart: an oci:// URL or a directory.
	ref string
	// version is the version of a registry chart; empty for a directory.
	version string
	// branch is the dev channel's branch when the version is one of its
	// builds (platform.chartBranch); empty on the stable channel.
	branch string
}

func (c platformChart) String() string {
	switch {
	case c.version == "":
		return "the local chart at " + c.ref
	case c.branch != "":
		return fmt.Sprintf("agent-platform %s (branch %s)", c.version, c.branch)
	default:
		return fmt.Sprintf("agent-platform %s", c.version)
	}
}

// platformChartFor reads the chart source from the config: platform.chartPath
// wins over the pinned release; on the dev channel the version is the
// branch's build ResolveChartVersion recorded.
func platformChartFor(cfg *config.Config) platformChart {
	if cfg.Platform.ChartPath != "" {
		return platformChart{ref: cfg.Platform.ChartPath}
	}
	return platformChart{ref: config.ChartRepository, version: cfg.Platform.ChartVersion, branch: cfg.Platform.ChartBranch}
}

// platformUp installs and verifies the platform, then prints the boot's one
// and only summary under the given header: `up` passes "Lab is up." so the
// user reads a single "what to do next" block once everything is verified,
// the standalone `agentlab platform` entry point passes "Platform is up.".
func platformUp(cfg *config.Config, header string) error {
	// The working-directory leftovers go first: nothing reads them, and the
	// refusal below is exactly the moment a user of an earlier agentlab meets.
	removeLegacyArtifacts()
	if err := refuseOlderLabShape(); err != nil {
		return err
	}
	// The dev channel follows its branch on every run, like Flux would: a
	// newer build is a new revision of the release below. The pick lands in
	// agentlab.yaml so the next command — or a colleague reading the file —
	// sees the exact version this lab runs.
	if changed, err := ResolveChartVersion(cfg); err != nil {
		return err
	} else if changed {
		if err := cfg.Save(); err != nil {
			return err
		}
	}
	chart := platformChartFor(cfg)
	step("Installing %s in the lab shape (bundled Flux engine on, self-management off)", chart)
	ctx := context.Background()

	// The Gateway API CRDs are the chart's documented cluster-level
	// prerequisite; embedded so the boot needs no network for them.
	// Idempotent re-apply; the apply waits for the apiserver to serve the
	// kinds before anything below uses them.
	step("Installing the Gateway API CRDs (standard channel)")
	if _, err := applyManifests(ctx, gatewayAPICRDs); err != nil {
		return err
	}

	step("Creating namespace and secrets")
	if err := ensureNamespace(platformNamespace); err != nil {
		return err
	}
	// The kagent namespace is the chart's: with the bundled engine and kagent
	// on, its pre-install/pre-upgrade hook creates the namespace ahead of the
	// kagent HelmRelease, and the connectivity release adopts it (deleting it
	// with the release on the ordered teardown).
	// muster appends this to its system trust pool so it can talk to the lab's
	// self-signed Dex over TLS (values: muster.muster.extraCaFile); Backstage
	// mounts the same Secret through global.identity.ca (NODE_EXTRA_CA_CERTS).
	if err := ensureSecretFromFiles(platformNamespace, "dex-ca", map[string]string{
		caCertKey: caCertPath,
	}); err != nil {
		return err
	}
	// The edge certificate: an externally provisioned pair when configured
	// (platform.tls in agentlab.yaml — a real cert for a domain the user
	// owns), otherwise the lab-CA wildcard (*.<domain>), which everything
	// that already trusts certs/ca.crt trusts too.
	edgeCert, edgeKey := gatewayCertPath, gatewayKeyPath
	if cfg.Platform.TLS.Set() {
		note("edge certificate: externally provisioned (%s)", cfg.Platform.TLS.CertFile)
		edgeCert, edgeKey = cfg.Platform.TLS.CertFile, cfg.Platform.TLS.KeyFile
	} else if err := ensureGatewayCert(cfg.Platform.Domain); err != nil {
		return err
	}
	if err := ensureTLSSecret(platformNamespace, "agent-platform-tls", edgeCert, edgeKey); err != nil {
		return err
	}
	if err := ensurePlatformSecrets(ctx); err != nil {
		return err
	}

	// Inside pods, *.<domain> must resolve to the edge Gateway (outside, the
	// nip.io wildcard already answers 127.0.0.1) — without this Backstage
	// could never reach https://muster.<domain>/mcp. The pinned edge Service
	// below is the rewrite's target, applied before anything resolves it.
	step("Pointing in-cluster *.%s at the edge (CoreDNS rewrite)", cfg.Platform.Domain)
	if coredns, _, err := renderManifest(cfg, "coredns.yaml.tmpl"); err != nil {
		return err
	} else if _, err := applyRestartingOnChange(ctx, coredns, kubeSystemNamespace, corednsDeployment); err != nil {
		return err
	}
	if nodeport, _, err := renderManifest(cfg, "gateway-nodeport.yaml.tmpl"); err != nil {
		return err
	} else if _, err := applyManifests(ctx, nodeport); err != nil {
		return err
	}

	if cfg.Backstage.Enabled {
		// Catalog entities + the agent create flow's scaffolder Template,
		// mounted into the chart's Backstage; must exist before the pod starts.
		// Backstage reads app-config at startup only, so a changed overlay
		// (e.g. flipping platform.observability toggles mimirEnabled) needs a
		// pod roll on re-runs: started here and absorbed by the install's wait
		// right below; on a fresh install the deployment does not exist yet and
		// the first pod reads the final config.
		if catalog, _, err := renderManifest(cfg, "backstage-catalog.yaml.tmpl"); err != nil {
			return err
		} else if _, err := applyRestartingOnChange(ctx, catalog, platformNamespace, componentBackstage); err != nil {
			return err
		}
	}

	// Before the platform on purpose: the Prometheus Operator's CRDs must be
	// served when the chart renders — its cluster-shape knobs detect
	// monitoring.coreos.com/v1 once, at render time — and the operator must
	// exist before the ServiceMonitors the components render.
	if cfg.Platform.Observability {
		if err := observabilityUp(cfg); err != nil {
			return err
		}
	}
	// Substrate before the platform too: the dev channel's kagent creates
	// its WorkerPool and Harnesses against Substrate's API at startup, and
	// its controller does not come up without them (substrate.go).
	if cfg.SubstrateEnabled() {
		if err := substrateUp(cfg); err != nil {
			return err
		}
	}

	// Managed models: every host model server's endpoint is detected from
	// the kind docker network and proven reachable from inside the cluster
	// BEFORE the install, so a host-side misconfiguration (bind address,
	// firewall) fails here with its fix instead of after helm's wait — for
	// every backend model-manager fronts.
	var backendEndpoints map[string]string
	if cfg.ModelManagerEnabled() {
		endpoints, err := resolveBackendEndpoints(cfg)
		if err != nil {
			return err
		}
		backendEndpoints = endpoints
		for _, b := range cfg.Platform.ModelManager.Backends {
			step("Checking host %s is reachable from pods (%s)", config.BackendServerName(b), endpoints[b])
			if err := preflightHostServer(cfg, b, endpoints[b]); err != nil {
				return err
			}
		}
	}

	_, valuesPath, err := renderManifest(cfg, "agent-platform-values.yaml.tmpl")
	if err != nil {
		return err
	}
	// Read the way `helm -f` reads it, once for the renders and the install:
	// the lab's render first, then platform.valuesFiles in order (the overlays
	// win) — a lab that points a component at another chart source or forwards
	// values the template does not know.
	values, err := helmValuesFiles(append([]string{valuesPath}, cfg.Platform.ValuesFiles...)...)
	if err != nil {
		return err
	}
	// Rendered here, applied after the install (the Workflow CRD ships with
	// muster), so a render error surfaces before the long wait.
	demoWorkflow, _, err := renderManifest(cfg, "demo-workflow.yaml.tmpl")
	if err != nil {
		return err
	}

	// Every platform image goes host cache -> node, never kubelet -> network:
	// the host cache survives `agentlab down`, so even when this very boot
	// fails later, the next one starts from warm images. Derived from the
	// charts as they are about to be installed (the meta chart's own objects,
	// then every component chart at the version its OCIRepository resolves
	// to), so a first boot and version bumps are covered too. Best-effort:
	// anything this misses is pulled in-node under the install's wait
	// timeout, exactly as before.
	step("Side-loading the platform images (the host cache survives `agentlab down`)")
	sideloadPlatformImages(cfg, platformImages(cfg, chart, values))
	// The dev images (platform.devImages) are builds of this host: never
	// pullable, always side-loaded, so their pods find them under
	// imagePullPolicy IfNotPresent — which is why each ref is then verified
	// in the node's own image list before the install (ensureNodeImages): a
	// missing one would surface five minutes later as an ImagePullBackOff,
	// helm-controller's upgrade timeout and a rollback to the chart's image.
	if len(cfg.Platform.DevImages) > 0 {
		refs := devImageRefs(cfg)
		if res := sideloadImages(cfg, hostPullImages(refs)); res.n > 0 {
			note("side-loaded %d dev images (%s)", res.n, res.d)
		}
		if err := ensureNodeImages(cfg, refs); err != nil {
			return err
		}
	}
	// The dex-localhost sidecar the lab patches onto the MCP servers
	// (agent-platform-values.yaml.tmpl); its image is in no chart render.
	if res := sideloadImages(cfg, hostPullImages([]string{dexLocalhostImage})); res.n > 0 {
		note("side-loaded the dex-localhost sidecar image (%s)", res.d)
	}

	step("Installing %s (this waits for every component HelmRelease)", chart)
	// One writer, one operation: the plain idempotent upgrade-or-install
	// through the embedded Helm (helm.go), no post-renderer (the lab's
	// patches are per-component `postRenderers` VALUES the chart forwards to
	// the component HelmReleases — see the values template) and no
	// --force-conflicts (nothing else writes the release's objects: the
	// dev-image loop goes through the values too). Helm 4's wait is kstatus
	// over the chart's objects — the FluxInstance and every platform
	// HelmRelease among them — so the install returns once the components
	// are Ready; waitPlatformReleases below then reads the outcome per
	// release.
	//
	// An unchanged re-run writes no revision: when the release's newest
	// revision is deployed from this very chart version with these very
	// values — the dev channel re-resolved to the build it already runs, an
	// `up` over a live lab — there is nothing to upgrade to, and the wait
	// below still reads every component's health. Whatever changes the
	// values (a dev image, a values file, a toggle in agentlab.yaml) or the
	// version (a newer build, a bumped pin) upgrades as before; a chart
	// directory (platform.chartPath) always does, its content is not
	// versioned. `helm -n agent-platform upgrade` from a shell stays the way
	// to force a revision.
	if rev, err := helmDeployedRevision(platformNamespace, platformRelease, chart.version, values); err != nil {
		return err
	} else if rev > 0 {
		note("chart agent-platform %s already installed with these values — nothing to do (Helm revision %d stays)", chart.version, rev)
	} else if err := helmUpgradeInstall(platformNamespace, platformRelease, chart.ref, chart.version, values, helmInstallTimeout, helmInstallOptions{}); err != nil {
		reportPlatformReleases()
		return err
	}
	if err := waitPlatformReleases(); err != nil {
		return err
	}

	// The lab's own MCP server for the Prometheus tools rides the same engine
	// (a HelmRelease of the mcp-prometheus chart) — after the platform, which
	// brings the engine and the tenant identity the release runs as.
	if cfg.Platform.Observability {
		if err := mcpPrometheusUp(cfg); err != nil {
			return err
		}
	}

	// Every downstream forwards the user's token (auth.forwardToken), so muster
	// connects per session, not at startup: until the first session signs in
	// the CR reads Auth Required, afterwards Connected. Either proves the
	// server answers (a dead server reads Failed); platform-test and
	// models-test then drive the sessions that flip them to Connected.
	step("Waiting for muster to reach the Kubernetes MCP")
	if err := waitMCPServerReachable(cfg.MCPServerName()); err != nil {
		return err
	}
	if cfg.Platform.Observability {
		step("Waiting for muster to reach the Prometheus MCP")
		if err := waitMCPServerReachable(mcpPrometheusRelease); err != nil {
			return err
		}
	}
	if cfg.ModelManagerEnabled() {
		// The model-manager chart renders its own MCPServer CR (with the
		// forward-token auth block); reachable proves the pod serves MCP.
		step("Waiting for muster to reach model-manager")
		if err := waitMCPServerReachable(modelManagerMCPServer); err != nil {
			return err
		}
	}
	if cfg.Platform.Agents {
		// agent-manager ships with the platform whenever kagent is on and
		// registers itself the same way (forward-token auth block).
		step("Waiting for muster to reach agent-manager")
		if err := waitMCPServerReachable(agentManagerMCPServer); err != nil {
			return err
		}
	}

	// The Workflow CRD ships with muster, so this has to land after the
	// install. A muster with no workflows leaves the Backstage muster plugin's
	// main tab empty, which reads as "the plugin is broken" rather than
	// "nothing to show".
	step("Creating the demo workflow")
	if _, err := applyManifests(ctx, demoWorkflow); err != nil {
		return err
	}
	// The per-server OAuth sign-in fixture (oauthfixture.go) — after the
	// install for the same CRD reason.
	if err := ensureOAuthFixture(cfg); err != nil {
		return err
	}
	// The fake-fleet fixture (fleetfixture.go): the family MCPServers with
	// the tool-group label the fleet charts stamp — same CRD reason.
	if err := ensureFleetFixture(cfg); err != nil {
		return err
	}

	// The agents' model key. The default ModelConfig (rendered by the kagent
	// chart from providers.anthropic) references this secret; agent pods
	// mount it at run time, so it can land after the install — which it must,
	// since the chart itself creates the kagent namespace.
	if cfg.Platform.Agents {
		step("Wiring the agents to Anthropic (ModelConfig model: %s)", cfg.AIModel)
		if _, err := ensureAnthropicSecret(kagentNamespace, "kagent-anthropic"); err != nil {
			return err
		}
		// The extra ModelConfigs from platform.extraModels (self-hosted
		// endpoints, OpenRouter, Gemini, ...) — after the install for the same
		// reason as the Secret above: the chart owns the kagent namespace and
		// the ModelConfig CRD. The host model servers' models are
		// model-manager's own ModelConfigs, for every backend it fronts.
		if err := ensureExtraModels(cfg); err != nil {
			return err
		}
		// Both heals below are shaped for the 0.x line's Agent CR. kagent API
		// v2 (Harness + AgentTemplate, kagent.dev/v1alpha3) serves no
		// agents.kagent.dev, composes no runtime image from its release tag
		// (Harnesses pin their images by digest) and has no iconUrl to accept.
		if !agentCRDServed() {
			note("kagent serves no %s (API v2: Harness + AgentTemplate); skipping the Agent-CR heals", agentCRD)
		} else {
			// Agent pods need the golang-adk runtime image at kagent's own tag,
			// which upstream has been observed not to publish (HACKS.md U8).
			step("Ensuring the agents' ADK runtime images are on the node")
			healADKImages(cfg)
			// The 0.9.x Agent CRD rejects the spec.iconUrl the create flow always
			// composes, failing every created agent's HelmRelease (HACKS.md U11).
			step("Ensuring the Agent CRD accepts spec.iconUrl")
			if err := patchAgentCRDIconURL(); err != nil {
				return err
			}
		}
	}

	// The public URL runs client -> agentgateway edge -> muster: reaching it
	// proves the Gateway is programmed, the data-plane pod serves TLS with the
	// lab wildcard cert, and the /-route forwards to muster. Retry rather than
	// probing once: the component releases are Ready, but the controller
	// creates the data-plane pod asynchronously after the Gateway lands, and
	// muster's HTTP listener accepts slightly later.
	step("Waiting for muster through the edge on %s", cfg.MusterBaseURL())
	client, err := labHTTPClient(3 * time.Second)
	if err != nil {
		return err
	}
	reachable := waitFor(60, 3*time.Second, func() bool {
		return httpUp(client, cfg.MusterBaseURL()+"/.well-known/oauth-authorization-server")
	})

	if reachable {
		if err := ensureMusterValidatesTokens(cfg); err != nil {
			return err
		}
	}

	reach := fmt.Sprintf("muster is live on %s (through the agentgateway edge)", cfg.MusterBaseURL())
	if !reachable {
		reach = fmt.Sprintf(`muster is NOT reachable on %s after 3 minutes.
  If 'docker port %s' shows no %d line, this cluster
  predates the edge port mapping and must be recreated (agentlab down && agentlab up).
  Otherwise check the edge (kubectl -n %s get gateway,pods) and 'agentlab logs muster'.
  Direct (edge-bypassing) stopgap: %s`,
			cfg.MusterBaseURL(), cfg.ControlPlaneNode(), cfg.Platform.GatewayPort,
			platformNamespace, cfg.MusterDirectURL())
	}

	backstageHint := "  Backstage is disabled (backstage.enabled in agentlab.yaml)."
	if cfg.Backstage.Enabled {
		// The release is Ready; this proves the route through the edge and
		// Backstage's own listener.
		step("Waiting for Backstage on %s", cfg.BackstageBaseURL())
		if waitFor(60, 3*time.Second, func() bool { return httpUp(client, cfg.BackstageBaseURL()) }) {
			backstageHint = fmt.Sprintf("  Backstage: %s (Sign In -> Dex; users and passwords in %s)",
				cfg.BackstageBaseURL(), config.File)
		} else {
			backstageHint = fmt.Sprintf(`  Backstage is NOT reachable on %s.
  Check 'kubectl -n %s get pods' and 'agentlab logs backstage'.`,
				cfg.BackstageBaseURL(), platformNamespace)
		}
	}

	agentsHint := "  Agents (kagent) are disabled (platform.agents in agentlab.yaml)."
	if cfg.Platform.Agents {
		// The kagent release is Ready, so the NodePort answers as soon as
		// kube-proxy programs it — a short retry suffices.
		step("Waiting for the kagent UI on %s", cfg.KagentUIBaseURL())
		uiUp := waitFor(10, 2*time.Second, func() bool {
			return httpUp(client, cfg.KagentUIBaseURL())
		})
		if uiUp {
			agentsHint = fmt.Sprintf("  Agents (kagent) run with model %s%s; UI: %s",
				cfg.AIModel, extraModelsHint(cfg.Platform.ExtraModels), cfg.KagentUIBaseURL())
		} else {
			agentsHint = fmt.Sprintf(`  Agents (kagent) run with model %s, but the UI is NOT reachable on %s.
  If 'docker port %s' shows no %d line, this cluster
  predates the kagent UI port mapping and must be recreated (agentlab down && agentlab up).
  Stopgap: kubectl -n kagent port-forward svc/kagent-ui %d:8080`,
				cfg.AIModel, cfg.KagentUIBaseURL(), cfg.ControlPlaneNode(),
				cfg.Platform.AgentsPort, cfg.Platform.AgentsPort)
		}
	}
	obsHint := "  Observability is disabled (platform.observability in agentlab.yaml)."
	if cfg.Platform.Observability {
		obsHint = "  Observability: Prometheus scrapes the cluster; muster serves it as x_mcp-prometheus_* tools\n" +
			"  (try asking Claude Code for a pod's CPU or memory)."
	}
	if cfg.SubstrateEnabled() {
		agentsHint += fmt.Sprintf("\n  Substrate %s (kagent's actor runtime) runs in %s: kubectl get workerpools,sandboxconfigs -A", substrateVersion, substrateNamespace)
	}
	fmt.Printf(`
%s
  %s
%s
%s

%s

%s
%s
%s
%s%s`, header, reach, usersBlock(cfg), backstageHint, claudeCodeHint(cfg), agentsHint, modelManagerHint(cfg, backendEndpoints), obsHint, devImagesHint(cfg), tryItBlock(cfg))
	// Everything the platform runs is in the node now — record it so the next
	// boot side-loads instead of pulling.
	snapshotPreloadImages(cfg)
	return nil
}

// ensurePlatformSecrets creates the platform's generated secrets once
// (platformSecretsName) and leaves an existing Secret alone, except for the
// keys a newer lab introduced: a cluster created by an older lab lacks them,
// and Backstage's env would fail to resolve.
func ensurePlatformSecrets(ctx context.Context) error {
	exists, err := objectExists(ctx, gvrSecrets, platformNamespace, platformSecretsName)
	if err != nil {
		return err
	}
	if !exists {
		if err := ensureSecret(platformNamespace, platformSecretsName, corev1.SecretTypeOpaque, map[string][]byte{
			"dex-client-secret":       []byte(config.AgentPlatformClientSecret),
			"registration-token":      []byte(randHex(32)),
			"oauth-encryption-key":    []byte(randBase64(32)),
			"valkey-password":         []byte(randHex(16)),
			backstageSessionSecretKey: []byte(randBase64(32)),
		}); err != nil {
			return err
		}
		note("created %s", platformSecretsName)
		return nil
	}
	note("%s already exists, leaving it alone", platformSecretsName)
	if !secretHasKey(platformNamespace, platformSecretsName, backstageSessionSecretKey) {
		patch := fmt.Sprintf(`{"stringData":{%q:%q}}`, backstageSessionSecretKey, randBase64(32))
		if err := patchObject(ctx, gvrSecrets, platformNamespace, platformSecretsName, types.MergePatchType, []byte(patch)); err != nil {
			return err
		}
		note("added %s to %s", backstageSessionSecretKey, platformSecretsName)
	}
	return nil
}

// applyRestartingOnChange applies a manifest and, when the apply created or
// changed anything, restarts the Deployment that reads the result at startup
// only — CoreDNS its Corefile, Backstage its app-config overlay — so a re-run
// with a changed render rolls the pod exactly once and an unchanged one
// leaves it alone. A Deployment that does not exist yet is not an error: a
// fresh install's first pod reads the final config. Reports whether it
// restarted.
func applyRestartingOnChange(ctx context.Context, manifest []byte, ns, deployment string) (bool, error) {
	results, err := applyManifests(ctx, manifest)
	if err != nil {
		return false, err
	}
	if !anyChanged(results) {
		return false, nil
	}
	if err := restartDeployment(ctx, ns, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// refuseOlderLabShape stops an install onto a cluster an earlier agentlab
// built: the standalone umbrella under the same release name, or the Flux
// controllers the old lab installed itself in flux-system (the chart's own
// guard refuses a second Flux too, with a message about clusters that run
// Flux — this one names the actual cause). The lab has no in-place migration
// on purpose: the kind cluster is throwaway, and `agentlab down && agentlab
// up` is a clean five-minute slate. The other way out, `agentlab
// platform-down` then `agentlab platform`, keeps the cluster and Dex:
// platform-down removes both of the above (PlatformDown), and the component
// releases replace the umbrella's CRDs (`crds: CreateReplace`).
func refuseOlderLabShape() error {
	const fix = "run `agentlab down && agentlab up` (or `agentlab platform-down`, then `agentlab platform`)"
	if chartName, err := helmReleaseChart(platformNamespace, platformRelease); err == nil && chartName == "agent-platform-standalone" {
		return fmt.Errorf("this cluster runs the agent-platform-standalone umbrella an earlier agentlab installed;\n" +
			"the lab installs the agent-platform meta chart now and has no in-place migration:\n" + fix)
	}
	if legacyFluxInstalled() {
		return fmt.Errorf("this cluster runs the Flux controllers an earlier agentlab installed (release %s in %s);\n"+
			"the agent-platform chart brings its own engine and refuses a second Flux:\n"+fix, legacyFluxRelease, legacyFluxNamespace)
	}
	return nil
}

// legacyFluxInstalled reports whether the Flux controllers an earlier
// agentlab installed itself are on the cluster (release flux in flux-system).
func legacyFluxInstalled() bool {
	return helmReleaseExists(legacyFluxNamespace, legacyFluxRelease)
}

// removeLegacyArtifacts deletes what earlier agentlab versions left in the
// working directory (see legacyVendorDir and the constants next to it).
// Quiet when there is nothing; a failure to remove is a note, not an error —
// nothing reads any of them anymore.
func removeLegacyArtifacts() {
	for _, path := range []string{legacyVendorDir, legacyHelmPluginsDir, legacyFluxValues, legacyMCPPrometheusValues} {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			note("could not remove %s (left by an earlier agentlab; safe to delete by hand): %v", path, err)
			continue
		}
		note("removed %s (left by an earlier agentlab; nothing reads it anymore)", path)
	}
}

// platformReleaseStatus is one platform HelmRelease as helm-controller
// reports it: the Ready condition's status ("True", "False" or "" before the
// first reconcile) and its message.
type platformReleaseStatus struct {
	name, ready, message string
}

// platformReleases lists the HelmReleases in the platform namespace — the
// component releases the meta chart rendered plus the lab's own
// (mcp-prometheus) — with their Ready condition.
func platformReleases() ([]platformReleaseStatus, error) {
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return nil, err
	}
	items, err := listObjects(context.Background(), gvr, platformNamespace, "")
	if err != nil {
		return nil, err
	}
	releases := make([]platformReleaseStatus, 0, len(items))
	for i := range items {
		releases = append(releases, platformReleaseStatus{
			name:    items[i].GetName(),
			ready:   conditionStatus(&items[i], conditionReady),
			message: conditionMessage(&items[i], conditionReady),
		})
	}
	return releases, nil
}

// waitPlatformReleases waits for every platform HelmRelease to be Ready and
// names the ones that are not, with helm-controller's own message. Helm's
// wait already covered them (kstatus), so this is normally instant; it is
// the readable report when it was not, and the guard against a release the
// wait could not see (one created after Helm returned).
func waitPlatformReleases() error {
	var pending []platformReleaseStatus
	var readErr error
	ready := waitFor(60, 5*time.Second, func() bool {
		var releases []platformReleaseStatus
		releases, readErr = platformReleases()
		if readErr != nil {
			return false
		}
		pending = pending[:0]
		for _, r := range releases {
			if r.ready != conditionTrue {
				pending = append(pending, r)
			}
			// helm-controller gave up on this one: no point waiting out the
			// clock, the message says why.
			if r.ready == "False" && strings.Contains(r.message, "retries exhausted") {
				readErr = fmt.Errorf("HelmRelease %s failed: %s", r.name, r.message)
				return true
			}
		}
		return len(pending) == 0
	})
	if readErr != nil {
		return fmt.Errorf("%w\ncheck `kubectl -n %s describe helmrelease` and `agentlab logs <component>`", readErr, platformNamespace)
	}
	if !ready {
		var lines []string
		for _, r := range pending {
			lines = append(lines, fmt.Sprintf("  %s: Ready=%s %s", r.name, orNone(r.ready), r.message))
		}
		return fmt.Errorf("platform HelmReleases not Ready after 5 minutes:\n%s\ncheck `kubectl -n %s describe helmrelease <name>` and `kubectl -n %s get pods`",
			strings.Join(lines, "\n"), platformNamespace, platformNamespace)
	}
	return nil
}

// reportPlatformReleases notes the not-Ready platform HelmReleases after a
// failed install, so a timed-out wait reads as the component that held it up
// rather than a bare "timed out waiting".
func reportPlatformReleases() {
	releases, err := platformReleases()
	if err != nil {
		return
	}
	for _, r := range releases {
		if r.ready != conditionTrue {
			note("HelmRelease %s: Ready=%s %s", r.name, orNone(r.ready), r.message)
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none yet)"
	}
	return s
}

// devImagesHint lists the dev images in the boot summary, so a lab running a
// build of yours says so where you look first.
func devImagesHint(cfg *config.Config) string {
	if len(cfg.Platform.DevImages) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n  Dev images (platform.devImages; remove the entry and re-run `agentlab platform` to restore the chart's):\n")
	for _, name := range slices.Sorted(maps.Keys(cfg.Platform.DevImages)) {
		fmt.Fprintf(&b, "    %-16s %s\n", name, cfg.Platform.DevImages[name])
	}
	return b.String()
}

// waitMCPServerConnected polls one muster MCPServer CR (in the platform
// namespace) until muster reports the downstream connection up.
// waitMCPServerReachable waits for a server muster authenticates to per
// session (auth.forwardToken): Auth Required means the server answered
// muster's probe with an OAuth challenge and waits for the first session,
// Connected that a session already signed in. Every downstream the lab
// aggregates is such a server now, so this replaced the plain Connected wait.
func waitMCPServerReachable(name string) error {
	return waitMCPServerState(name, "Connected", mcpServerStateAuthRequired)
}

// waitMCPServerState polls the MCPServer CR's status.state until it reads one
// of want.
func waitMCPServerState(name string, want ...string) error {
	var state string
	var readErr error
	reached := waitFor(40, 3*time.Second, func() bool {
		state, readErr = mcpServerState(name)
		return readErr == nil && slices.Contains(want, state)
	})
	if !reached {
		return notReached("MCPServer "+name, strings.Join(want, " or "), state, readErr,
			fmt.Sprintf("check `agentlab logs muster`, `kubectl -n %s describe mcpservers.muster.giantswarm.io %s`\n"+
				"and the server's rollout: `kubectl -n %s get deploy,pods`", platformNamespace, name, platformNamespace))
	}
	note("MCPServer %s: %s", name, state)
	return nil
}

// mcpServerState reads one muster MCPServer's status.state in the platform
// namespace (musterMCPServerResource): "" before muster's first reconcile,
// the apiserver's error for a CR — or a CRD — that is not there.
func mcpServerState(name string) (string, error) {
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return "", err
	}
	obj, err := getObject(context.Background(), gvr, platformNamespace, name)
	if err != nil {
		return "", err
	}
	state, _, _ := unstructured.NestedString(obj.Object, "status", "state")
	return state, nil
}

// claudeCodeHint is the "point Claude Code at it" block of the platform-up
// output, adapted to whether the lab CA is in the system trust store
// (`agentlab trust` — probed only, never installed from here) and to the
// host's Node: NODE_USE_SYSTEM_CA replaces NODE_EXTRA_CA_CERTS on >= 22.15.
func claudeCodeHint(cfg *config.Config) string {
	var b strings.Builder
	if SystemTrusted() {
		b.WriteString("  Point Claude Code at it (browser login through Dex; the lab CA is trusted):\n")
		if nodeSupportsSystemCA() {
			b.WriteString("    export NODE_USE_SYSTEM_CA=1\n")
		} else {
			b.WriteString("    export NODE_EXTRA_CA_CERTS=" + absCAPath() + "   # Node >= 22.15 takes NODE_USE_SYSTEM_CA=1 instead\n")
		}
	} else {
		b.WriteString("  The lab CA is not in the system trust store: browsers warn on every lab\n")
		b.WriteString("  hostname and Node refuses the edge. One command fixes both (a sudo prompt,\n")
		b.WriteString("  reverted by `agentlab untrust`):\n")
		b.WriteString("    agentlab trust\n")
		b.WriteString("  Then point Claude Code at it (browser login through Dex):\n")
		if nodeSupportsSystemCA() {
			b.WriteString("    export NODE_USE_SYSTEM_CA=1   # without trusting: NODE_EXTRA_CA_CERTS=" + absCAPath() + "\n")
		} else {
			b.WriteString("    export NODE_EXTRA_CA_CERTS=" + absCAPath() + "\n")
		}
	}
	b.WriteString("    claude mcp add --transport http muster " + cfg.MusterBaseURL() + "/mcp\n")
	b.WriteString("    # then in Claude Code: /mcp -> authenticate")
	return b.String()
}

// ensureMusterValidatesTokens proves the auth path end to end (Dex password
// grant -> Bearer on /mcp) and heals the one known way it silently breaks:
// muster can come up with a TLS trust pool that is missing the extra CA
// (HACKS.md U6), rejecting every token with invalid_token until the pod is
// replaced — while everything else (rollout, MCPServer Connected, the OAuth
// metadata endpoint) looks healthy. One bounce, then re-probe.
//
// Only called once muster's OAuth endpoint answers 2xx, which implies OIDC
// discovery already succeeded — so a rejected token here is the trust flake,
// not a still-starting OAuth server.
func ensureMusterValidatesTokens(cfg *config.Config) error {
	step("Verifying muster accepts Dex tokens")
	var lastErr error
	if waitFor(3, 2*time.Second, func() bool {
		lastErr = musterTokenProbe(cfg)
		return lastErr == nil
	}) {
		note("muster validated a fresh Dex token")
		return nil
	}
	note("muster rejects Dex tokens (%v)", lastErr)
	note("known muster startup flake (HACKS.md U6) — replacing the muster pod once")
	ctx := context.Background()
	if err := restartDeployment(ctx, platformNamespace, componentMuster); err != nil {
		return err
	}
	if err := waitDeploymentRolledOut(ctx, platformNamespace, componentMuster, musterRestartTimeout); err != nil {
		return err
	}
	// The fresh pod redoes OIDC discovery (fast: Dex and valkey are up) and
	// its OAuth mux 503s meanwhile, so poll the probe rather than the log.
	if !waitFor(20, 3*time.Second, func() bool {
		lastErr = musterTokenProbe(cfg)
		return lastErr == nil
	}) {
		return fmt.Errorf("muster still rejects Dex tokens after a restart: %w", lastErr)
	}
	note("muster validated a fresh Dex token after the restart")
	return nil
}

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

func randBase64(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// ensureNodeImages checks that every ref is in the node's image list as the
// kubelet sees it (crictl), side-loads what is missing once more, and fails
// naming the ref when it still is not there — before the install, so a dev
// image the node never got is a one-line error with its fix rather than a
// five-minute helm-controller timeout and a rollback. A side-loaded tag has
// been seen to miss the node's image list on a first load when the image's
// ID was already there under another tag.
func ensureNodeImages(cfg *config.Config, refs []string) error {
	have, err := nodeImageTags(cfg.ControlPlaneNode())
	if err != nil {
		return fmt.Errorf("listing the node's images: %w", err)
	}
	missing := missingImages(have, refs)
	if len(missing) == 0 {
		return nil
	}
	note("%d dev images are not on the node yet (%s); side-loading them again", len(missing), strings.Join(missing, ", "))
	if _, err := kindLoadImages(cfg, missing); err != nil {
		note("side-loading again failed: %v", err)
	}
	if have, err = nodeImageTags(cfg.ControlPlaneNode()); err != nil {
		return fmt.Errorf("listing the node's images: %w", err)
	}
	if still := missingImages(have, refs); len(still) > 0 {
		return fmt.Errorf("dev images not on the node %s after side-loading: %s\n"+
			"check `docker image inspect <ref>` on the host, load it by hand\n"+
			"(`docker save --platform linux/<arch> <ref> | docker exec -i %s ctr --namespace=k8s.io images import --all-platforms -`),\n"+
			"then re-run `agentlab platform`", cfg.ControlPlaneNode(), strings.Join(still, ", "), cfg.ControlPlaneNode())
	}
	note("all %d dev images are on the node", len(refs))
	return nil
}

// missingImages is the subset of want that have does not list.
func missingImages(have, want []string) []string {
	var missing []string
	for _, ref := range want {
		if !slices.Contains(have, ref) {
			missing = append(missing, ref)
		}
	}
	return missing
}
