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
	"github.com/giantswarm/agentlab/internal/telemetry"
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
func PlatformUp(cfg *config.Config, offers Offers) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	// The standalone entry point skips Up's overlapped preload, so join a
	// synchronous one here: on a live cluster the node filter makes it a
	// no-op, and after a half-failed boot it heals the missing side-loads
	// before the installs start their rollout waits.
	reportPreload(loadLabImages(cfg, pullLabImages(cfg)))
	return platformUp(cfg, "Platform is up.", offers)
}

// platformTopologyFor is the topology of the chart the config installs, read
// off its offline render before the cluster exists (the preflight's input):
// the lab values rendered as the install will render them, the meta chart
// templated with them, the roster read out. A render that fails here (an
// unpublished pin) falls back to the verified line's topology with a note;
// the install reports the chart's error properly, later.
//
// The exceptions are a chart that refuses the lab's values and a registry
// that does not answer through the render's retries: this is the FIRST
// render of a boot, so the verdict is in hand before the certs, the cluster
// and Dex — the five minutes an install would spend before reaching the same
// answer (the install pulls the same chart from the same registry). They are
// returned rather than noted. The caller has resolved the dev channel first
// (Up), so the chart judged is the one the install will use.
func platformTopologyFor(cfg *config.Config) (platformTopology, error) {
	var roster *platformRoster
	if cfg.Platform.Enabled {
		chart := platformChartFor(cfg)
		_, valuesPath, err := renderManifest(cfg, platformValuesTemplate)
		if err == nil {
			var values map[string]any
			if values, err = helmValuesFiles(append([]string{valuesPath}, cfg.Platform.ValuesFiles...)...); err == nil {
				roster, err = renderPlatformRoster(chart, values)
			}
		}
		if err != nil {
			if isSchemaRejection(err) {
				return platformTopology{}, chartRefusesValuesError(chart, err)
			}
			if registryUnreachable(err) {
				return platformTopology{}, chartUnreachableError(chart, err, "agentlab up")
			}
			note("cannot render %s ahead of the boot (%v); budgeting for the 4.x line's topology", chart, excerptEnds(err.Error(), 300))
		}
	}
	return topologyOf(cfg, roster), nil
}

// chartRefusesValuesError words a meta chart that will not accept the values
// the lab renders for it. Printed whole: the schema path is the last line of
// Helm's message and the only part worth reading; the advice is the knob
// that selects the chart in this lab's mode (platformChart.remedy).
func chartRefusesValuesError(chart platformChart, err error) error {
	return fmt.Errorf("%s does not accept the values the lab renders for it, so the install would fail:\n\n%s\n\n%s",
		chart, indent(strings.TrimSpace(err.Error()), "  "), chart.remedy())
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

// remedy is what to change when this chart, or a component it pins, refuses
// the values: the knob that selects the chart in this lab's mode. Never
// chartVersion where it is ignored (platform.chartPath) or overwritten on the
// next run (an unpinned platform.chartBranch) — that would be advice nothing
// can act on.
func (c platformChart) remedy() string {
	switch {
	case c.version == "":
		return fmt.Sprintf("Fix the chart at %s, or the values the lab renders for it (platform.valuesFiles).", c.ref)
	case c.branch != "":
		return fmt.Sprintf("Pick another build of branch %s, or pin one: `agentlab configure --chart-branch \"\"`, then `--chart-version <full dev tag>`; or leave the dev channel for a release that accepts these values.", c.branch)
	default:
		return "Set platform.chartVersion in agentlab.yaml to a release that accepts these values."
	}
}

// installedVersion is the version of the chart this run installs: the pinned
// or resolved tag of a registry chart; for a chart directory what its
// Chart.yaml says — empty when that cannot be read, which the install reports
// properly, later.
func (c platformChart) installedVersion() string {
	if c.version != "" {
		return c.version
	}
	return chartDirVersion(c.ref)
}

// connectivityDir is the checkout's connectivity chart when this chart is a
// directory (platform.chartPath): the sibling the lab pushes into the lab
// registry and renders the connectivity release from (connectivity.go).
// Empty for a registry chart, whose connectivity is published with it.
func (c platformChart) connectivityDir() string {
	if c.version != "" {
		return ""
	}
	return config.ConnectivityChartDir(c.ref)
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
func platformUp(cfg *config.Config, header string, offers Offers) error {
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
	ctx := context.Background()
	// The platform signal, now that the chart is resolved: the meta chart
	// line this lab installs, once per run (docs/telemetry.md). Ahead of the
	// install, so a run that fails to install still says which line it was
	// about to — the way the command signal counts failed runs.
	telemetry.Platform(ctx, cfg, chart.installedVersion())
	step("Installing %s in the lab shape (bundled Flux engine on, self-management off)", chart)

	// The cluster-level APIs the lab provides itself (providedapis.go: the
	// Gateway API, the Cilium policy CRDs) — before anything that renders
	// their kinds.
	if err := installProvidedAPIs(ctx); err != nil {
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
	// The GitHub token (githubtoken.go) — before the install: the portal's
	// envFrom and agent-manager's env reference the Secret without
	// `optional`, and the values just rendered name it whenever $GITHUB_TOKEN
	// is set.
	if err := ensureGitHubTokenSecrets(ctx, cfg); err != nil {
		return err
	}
	// The klaus-gateway component's Secrets (klausgateway.go) — before the
	// install too: the chart mounts obo.existingSecret and reads
	// slack.secretName without `optional`.
	if cfg.KlausGatewayEnabled() {
		if err := ensureKlausGatewaySecrets(ctx); err != nil {
			return err
		}
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
	// Model serving (serving.go): cert-manager before the platform too —
	// the llm-d controller's webhook certificate is a cert-manager
	// Certificate and its webhook configurations take their CA from the
	// cainjector, so the component's HelmRelease fails on the missing kinds
	// without it.
	if cfg.ServingEnabled() {
		if err := certManagerUp(cfg); err != nil {
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

	// The VM provisioner as a pod of the node (vmmanager.go): the node's KVM
	// devices checked before the install would leave a pod that cannot boot
	// anything; a local guest image build pushed into the lab registry so the
	// values below can pin it; and the registration an earlier agentlab
	// created for a host vm-manager removed, since the chart now renders one
	// of the same name.
	if cfg.VMManagerEnabled() {
		step("Checking the node can run vm-manager (%s)", strings.Join(kvmDevices, ", "))
		if err := preflightVMManager(cfg); err != nil {
			return err
		}
		if cfg.Platform.VMManager.ImageDir != "" {
			step("Pushing the guest image of %s into the lab registry", cfg.Platform.VMManager.ImageDir)
		}
		if err := pushVMManagerGuestImage(context.Background(), cfg); err != nil {
			return err
		}
	}
	if err := removeLegacyVMManagerRegistration(context.Background()); err != nil {
		return err
	}
	// A chart directory's connectivity chart (connectivity.go): the
	// checkout's copy, into the lab registry at the meta chart's version,
	// ahead of the renders that point the release at it. The digest is what
	// the release is waited for after the install.
	var connectivityDigest string
	if local, err := localConnectivityChartFor(cfg); err != nil {
		return err
	} else if local != nil {
		step("Pushing the connectivity chart of %s into the lab registry as %s (the meta chart installs it at its own version)", local.Dir, local.Version)
		if connectivityDigest, err = pushConnectivityChart(cfg, local); err != nil {
			return err
		}
	}

	// The WorkerPool's CPU feature-set pin names the node its workers land on,
	// which only the cluster can say: the binary's GOARCH — what a render
	// without a cluster falls back to — is cross-built amd64 by the devctl
	// Makefile even on an arm64 host. Resolved once, for both renders below.
	workerArch := clusterWorkerPoolArch(ctx)
	pinWorkerArch := func(t *tmplData) { t.WorkerPoolArch = workerArch }

	_, valuesPath, err := renderManifestWith(cfg, platformValuesTemplate, pinWorkerArch)
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

	// The chart as it is about to be installed, rendered once: what it ships
	// decides what the lab checks and preloads. A render that fails is a
	// note here — the install below reports the same error in Helm's words.
	roster, err := renderPlatformRoster(chart, values)
	if err != nil {
		// A chart that refuses these values refuses them on the cluster too:
		// the install would carry them to helm-controller and fail there, so
		// it is not started. A registry that did not answer through the
		// render's retries stops it too: without the roster there are no
		// component renders, and without those none of the lab's patches.
		// Every other render failure stays a note — the install reports it
		// in Helm's own words if it is real.
		if isSchemaRejection(err) {
			return chartRefusesValuesError(chart, err)
		}
		if registryUnreachable(err) {
			return chartUnreachableError(chart, err, "agentlab platform")
		}
		note("cannot render %s (%v); the node pulls the platform images itself", chart, excerptEnds(err.Error(), 300))
	}
	// Agent Substrate comes with the chart (the substrate component follows
	// kagent on the 4.x line): its pods need the PodCertificateRequest API
	// the lab's kind config turns on, and a cluster created before the gates
	// cannot be fixed in place — refused here, before the install's wait
	// would sit on a Substrate that never comes up.
	if roster.shipsSubstrate() {
		if err := preflightPodCertificateAPI(ctx); err != nil {
			return err
		}
	}

	// Every platform image goes host cache -> node, never kubelet -> network:
	// the host cache survives `agentlab down`, so even when this very boot
	// fails later, the next one starts from warm images. Derived from the
	// charts as they are about to be installed (the meta chart's own objects,
	// then every component chart at the version its OCIRepository resolves
	// to — the kagent line's controller and UI, the Go ADK Harness image by
	// digest, Substrate's control plane and its gVisor worker, the CNPG
	// operator and the Postgres operand, the hook Jobs' kubectl and openssl),
	// so a first boot and version bumps are covered too. Best-effort for the
	// images: anything this misses is pulled in-node under the install's
	// wait timeout. Not for the verdict: a chart that refuses the values its
	// HelmRelease carries stops the boot here, before anything is side-loaded
	// for an install that would not start.
	step("Side-loading the platform images (the host cache survives `agentlab down`)")
	images, renders, err := platformImages(cfg, roster)
	if err != nil {
		return err
	}
	// The WorkerPool as the chart is about to create it: its CPU feature-set
	// pin against the node that would run the workers. Read off the render
	// rather than off the lab's values, so the chart's own default is covered
	// too — the pool's pods are ate-controller's, so nothing in the install's
	// wait would ever report them Pending. Before the side-load, like the
	// sibling preflights: a cluster that cannot run the workers is refused
	// without pulling the platform's images for it first.
	if err := preflightWorkerPoolArch(ctx, renders); err != nil {
		return err
	}
	sideloadPlatformImages(cfg, images)
	// What only the renders could say goes into the values now, rendered
	// once more: the dex-localhost sidecar's targets (dexLocalhostTargets —
	// every Deployment the component charts tell the lab Dex's localhost
	// address, the chart's default-on managers and an overlay's included)
	// and, with dev images (platform.devImages, devimages.go), the Deployment
	// targets side-loaded and their chart image names read off the renders,
	// the `harness` image pushed to the lab registry and pinned by digest.
	// Then the changed pair goes to the charts once more: the meta chart and
	// every component whose spec.values moved (recheckReleases), so a schema
	// that refuses a patch or a dev image is refused here and not after the
	// install's wait. The Harness's state before the install is what the
	// recompile report afterwards compares against.
	sidecars, err := dexLocalhostTargets(cfg, renders)
	if err != nil {
		return err
	}
	noteDexLocalhostTargets(roster, renders, sidecars)
	var dev *devImages
	var harnessBefore harnessState
	imageNames := defaultDevImageNames(cfg)
	if len(cfg.Platform.DevImages) > 0 {
		if dev, err = prepareDevImages(cfg, renders); err != nil {
			return err
		}
		imageNames = dev.names
	}
	postRenderers, err := componentPostRenderers(cfg, imageNames, sidecars)
	if err != nil {
		return err
	}
	if _, valuesPath, err = renderManifestWith(cfg, platformValuesTemplate, func(t *tmplData) {
		pinWorkerArch(t)
		t.PostRenderers = postRenderers
		if dev != nil {
			t.HarnessDevImage = dev.harness
		}
	}); err != nil {
		return err
	}
	if values, err = helmValuesFiles(append([]string{valuesPath}, cfg.Platform.ValuesFiles...)...); err != nil {
		return err
	}
	if dev != nil {
		if err := dev.checkValues(cfg, values); err != nil {
			return err
		}
	}
	if err := recheckReleases(cfg, chart, roster, values); err != nil {
		return err
	}
	if dev != nil && dev.harness != "" {
		if harnessBefore, err = readHarnessState(ctx); err != nil {
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
	// The connectivity release of a chart directory runs the chart just
	// pushed before anything reads the platform: a re-push under the meta
	// chart's version changed the content behind the tag, which the install
	// above did not touch and Flux would fetch on its interval.
	if connectivityDigest != "" {
		if err := waitConnectivityCatchUp(ctx, connectivityDigest); err != nil {
			return err
		}
	}
	// The two halves of Agent Substrate on one release (proveSubstrateLine):
	// a chart whose kagent range admits a worker image from another Substrate
	// release than its atelet installs green and boots no golden actor —
	// refused here, with both images and the fix, not five minutes into the
	// first agents proof.
	var substrate []substrateImages
	if roster.shipsSubstrate() {
		if substrate, err = proveSubstrateLine(ctx, substrateSkewRemedy(cfg)); err != nil {
			return err
		}
	}
	if dev != nil && dev.harness != "" {
		if err := reportHarnessDevImage(ctx, dev, harnessBefore); err != nil {
			return err
		}
	}

	// The fleet's admission (admission.go): Kyverno with the
	// flux-multi-tenancy policy in Enforce, the platform namespace exempt
	// like flux-giantswarm on an installation — after the platform, whose
	// engine brings the Flux kinds the policy names, and before the lab's own
	// HelmRelease below, which is then admitted under it.
	if err := fleetAdmissionUp(cfg); err != nil {
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
	// connects per session, not at startup: until the first session uses it
	// the CR reads Awaiting Session (Auth Required before muster 5.28.0),
	// afterwards Connected. Each proves the server answers (a dead server
	// reads Failed); platform-test and models-test then drive the sessions
	// that flip them to Connected.
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
	// Every server the rule gave the sidecar to registers itself with muster
	// under its own name (the manager charts render their MCPServer CR with
	// the forward-token auth block; the Kubernetes MCP's was waited for
	// above): reachable proves the pod serves MCP through the bridge.
	if err := waitSidecarMCPServers(ctx, cfg, sidecars); err != nil {
		return err
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
		// The migrate Job's copy of the GitHub token (githubtoken.go) — the
		// chart created the kagent namespace by now; a no-op when the
		// pre-install pass already found the namespace, or without the token.
		if err := ensureGitHubTokenSecret(ctx, kagentNamespace); err != nil {
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
	// Whether the portal answered decides the open question at the end of the
	// summary (prompt.go): an unreachable portal is not worth a browser tab.
	portalUp := false
	if cfg.Backstage.Enabled {
		// The release is Ready; this proves the route through the edge and
		// Backstage's own listener.
		step("Waiting for Backstage on %s", cfg.BackstageBaseURL())
		portalUp = waitFor(60, 3*time.Second, func() bool { return httpUp(client, cfg.BackstageBaseURL()) })
		if portalUp {
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
	if roster.shipsSubstrate() {
		version, _ := helmReleaseVersion(substrateNamespace, substrateRelease)
		agentsHint += fmt.Sprintf("\n  Agent Substrate %s (the actors' runtime, from the chart) runs in %s: kubectl get workerpools,sandboxconfigs -A", orNone(version), substrateNamespace)
		for _, s := range substrate {
			agentsHint += "\n  " + s.String()
		}
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
%s
%s
%s
%s%s`, header, reach, usersBlock(cfg), backstageHint, claudeCodeHint(cfg), agentsHint, modelManagerHint(cfg, backendEndpoints), servingHint(cfg), vmManagerHint(cfg), klausGatewayHint(cfg), obsHint, devImagesHint(cfg, dev), tryItBlock(cfg))
	// Everything the platform runs is in the node now — record it so the next
	// boot side-loads instead of pulling.
	snapshotPreloadImages()
	// The two steps the summary above only describes: on a terminal, ask
	// instead of telling (prompt.go). Optional to the last: whatever the
	// person answers, the platform is up and this returns nil.
	offerTrustAndOpen(cfg, offers, portalUp)
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
			if r.ready == condFalseStatus && strings.Contains(r.message, "retries exhausted") {
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
// build of yours says so where you look first: the harness target with the
// registry ref the platform Harness pins.
func devImagesHint(cfg *config.Config, dev *devImages) string {
	if len(cfg.Platform.DevImages) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n  Dev images (platform.devImages; remove the entry and re-run `agentlab platform` to restore the chart's):\n")
	for _, name := range slices.Sorted(maps.Keys(cfg.Platform.DevImages)) {
		ref := cfg.Platform.DevImages[name]
		if name == config.DevImageHarness && dev != nil && dev.harness != "" {
			ref += "  (Harness " + platformHarness + " pins " + dev.harness + ")"
		}
		fmt.Fprintf(&b, "    %-16s %s\n", name, ref)
	}
	return b.String()
}

// noteDexLocalhostTargets reports the sidecar rule's outcome in the boot
// log: which Deployments get the bridge and by which key — or, with no
// component render to read, that none does and what that means. The
// roster's releases whose render was skipped (judgeRenderFailures) are
// named too: the rule never read their Deployments, so a server among them
// runs without the bridge.
func noteDexLocalhostTargets(roster *platformRoster, renders map[string]string, sidecars map[string][]dexLocalhostTarget) {
	unread := ""
	if skipped := roster.unrendered(renders); len(skipped) > 0 {
		unread = fmt.Sprintf("; not read, their renders were skipped: %s", strings.Join(skipped, ", "))
	}
	if len(sidecars) == 0 {
		if roster == nil {
			note("no component render to read, so no %s sidecar is patched: a server that validates the forwarded token cannot reach the lab Dex", dexLocalhostContainer)
		} else {
			note("no component render tells a pod the lab Dex address through a name it dials it by (%s): no %s sidecar to patch%s", strings.Join(dexDialNames, ", "), dexLocalhostContainer, unread)
		}
		return
	}
	var names []string
	for _, component := range slices.Sorted(maps.Keys(sidecars)) {
		for _, target := range sidecars[component] {
			name := target.deployment
			if name != component {
				name = component + "/" + name
			}
			names = append(names, name+" ("+target.key+")")
		}
	}
	note("%s sidecar on the %d Deployments told the lab Dex address: %s%s", dexLocalhostContainer, len(names), strings.Join(names, ", "), unread)
}

// waitSidecarMCPServers waits for muster to reach every server the sidecar
// rule selected (dexLocalhostTargets) that registers itself with muster under
// its Deployment's name — the manager charts render their MCPServer CR that
// way; the Kubernetes MCP's CR is the connectivity chart's and waited for by
// the caller. A target without a CR of its name is noted, not waited for.
func waitSidecarMCPServers(ctx context.Context, cfg *config.Config, sidecars map[string][]dexLocalhostTarget) error {
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return err
	}
	for _, component := range slices.Sorted(maps.Keys(sidecars)) {
		for _, target := range sidecars[component] {
			name := target.deployment
			if name == cfg.MCPServerName() {
				continue
			}
			registered, err := objectExists(ctx, gvr, platformNamespace, name)
			if err != nil {
				return err
			}
			if !registered {
				note("Deployment %s carries the %s sidecar but registers no MCPServer of its name — nothing to wait for", name, dexLocalhostContainer)
				continue
			}
			step("Waiting for muster to reach %s", name)
			if err := waitMCPServerReachable(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// waitMCPServerConnected polls one muster MCPServer CR (in the platform
// namespace) until muster reports the downstream connection up.
// waitMCPServerReachable waits for a server muster authenticates to per
// session (auth.forwardToken): Awaiting Session means muster's probe reached
// it and it waits for the first session (Auth Required on a muster before
// 5.28.0, which spelled it that way), Connected that a session already uses
// it. Every downstream the lab aggregates is such a server now, so this
// replaced the plain Connected wait.
func waitMCPServerReachable(name string) error {
	return waitMCPServerState(name, mcpServerReachableStates...)
}

// mcpServerReachableStates are the CR states of a per-session server that
// answers: Failed is the state that means trouble.
var mcpServerReachableStates = []string{mcpServerStateConnected, mcpServerStateAwaitingSession, mcpServerStateAuthRequired}

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
