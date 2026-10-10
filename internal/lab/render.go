package lab

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

//go:embed templates/*.tmpl
var templatesFS embed.FS

// StateDir is where rendered manifests land, for inspection and for
// kubectl/helm to consume. Gitignored; regenerated on every command.
const StateDir = "state"

// checksumPlaceholder is what stamped manifests carry before the input
// checksum replaces it (the standard Helm checksum/config pattern, done here
// because plain manifests have no templating hook at apply time).
const checksumPlaceholder = "REPLACED_AT_APPLY"

// tmplData exposes the config plus computed values to the templates.
type tmplData struct {
	*config.Config
	CertsDir              string // absolute, for the kind extraMount
	NodeFilesDir          string // absolute, the node's host-kernel guard (hostkernel.go)
	MusterNodePort        int
	KagentUINodePort      int
	GatewayNodePort       int
	GatewayPublicNodePort int
	BrowserCallbackPort   int
	DomainRegex           string // Platform.Domain with dots escaped, for the CoreDNS rewrite
	AllGroups             []string
	KubernetesClientID,
	KubernetesClientSecret,
	AgentPlatformClientID,
	AgentPlatformClientSecret string
	// ModelManagerEnabled mirrors cfg.ModelManagerEnabled(); Backends is
	// platform.modelManager.backends (the first is model-manager's default
	// backend) and Endpoints the URL model-manager dials for each of them
	// (autodetected from the kind docker network — empty when the cluster
	// does not exist yet, which only a pre-boot `agentlab render` sees;
	// platformUp resolves them strictly).
	ModelManagerEnabled   bool
	ModelManagerBackends  []string
	ModelManagerEndpoints map[string]string
	// LabHostName and LabHostIP are the CoreDNS hosts entry that lets pods
	// reach this machine by name (labHostName, the autodetected endpoints'
	// host); LabHostIP is empty, and the entry left out, while the kind
	// network does not exist yet.
	LabHostName string
	LabHostIP   string
	// LegacyChart mirrors cfg.LegacyChart(): the lab installs a released
	// 3.x meta chart, and agent-platform-values.yaml.tmpl renders the 3.x
	// lab shape (kagent 0.10 with its bundled Postgres, no Substrate, no
	// platform Postgres) instead of the current line's.
	LegacyChart bool
	// FamilylessChart mirrors cfg.FamilylessChart(): the connectivity chart
	// registers the family-less mcp-kubernetes (the 3.x line, an upgrade
	// seed's 4.x line), whose mcp-kubernetes chart refuses mcpServer.
	FamilylessChart bool
	// ExtraModels is what extra-models.yaml.tmpl renders: the
	// platform.extraModels entries.
	ExtraModels []config.ExtraModel
	// ModelConfigAPIVersion is the ModelConfig apiVersion the lab serves
	// (modelConfigGVR), e.g. api.kagent.dev/v1alpha3.
	ModelConfigAPIVersion string
	// The per-server OAuth sign-in fixture (oauthfixture.go): the MCPServer
	// name the proofs sign in to and the protected endpoint it points at.
	OAuthFixtureServer string
	GitHubMCPServer    string
	// GitHubMCPClientSecret is the OAuth client Secret the MCPServer
	// references, platform.github.secret resolved (githubmcp.go).
	GitHubMCPClientSecret config.SecretRef
	// GitHubSignIn is the lab Dex's GitHub connector as agentlab.yaml
	// configures it, nil while the sign-in is off (githubsignin.go);
	// ApplyDex renders what the cluster allows instead: the connector once
	// its client Secret is in place, with the Secret's version.
	GitHubSignIn    *gitHubSignInValues
	OAuthFixtureURL string
	// VMManagerEnabled turns the chart's vm-manager component on
	// (vmmanager.go); VMManagerGuestImage is the chart's guestImage block for
	// a local build pushed into the lab registry, nil for the release's
	// published artifact.
	VMManagerEnabled    bool
	VMManagerGuestImage *vmManagerGuestImage
	// KlausGatewayEnabled turns the chart's klaus-gateway component on
	// (klausgateway.go); KlausGateway carries the names and URLs its
	// `klausGateway:` block needs.
	KlausGatewayEnabled bool
	KlausGateway        klausGatewayValues
	// ServingEnabled mirrors cfg.ServingEnabled(); Serving carries the names
	// the serving blocks render (serving.go): the lab preset and its
	// runtime image, the serving namespace, the models Gateway.
	ServingEnabled bool
	Serving        servingValues
	// WorkspacesEnabled mirrors cfg.WorkspacesEnabled(); WorkspacesChart
	// says the platform chart takes the `workspaces:` block the values
	// template renders (workspaces.go: chartCarriesWorkspaces), and
	// Workspaces carries the names workspaces.yaml.tmpl and that block
	// render.
	WorkspacesEnabled bool
	WorkspacesChart   bool
	Workspaces        workspacesValues
	// WorkspaceProviders are the workspace-manager's provider instances
	// (workspaceprovider.go), none while none is rendered: agentlab.yaml's
	// in a render without a cluster, what the cluster allows in the
	// install's. WorkspaceFake says the lab's GitHub is among them
	// (workspacefake.go): CoreDNS sends its name to its Service, and the
	// workspace-manager trusts the lab CA its leaf is from.
	WorkspaceProviders []workspaceProviderValues
	WorkspaceFake      bool
	// PostRenderers is the lab's per-component `postRenderers` list as
	// indented YAML, keyed by agent-platform component name
	// (postrenderers.go): the hostNetwork, sidecar and nodePort patches plus
	// the dev-image overrides — the template's roster blocks read their
	// component's entry, its trailing range renders the rest
	// (ExtraPostRenderers); MCPPrometheusPostRenderers the same for the
	// lab's own mcp-prometheus HelmRelease. MCPPrometheusChartVersion pins
	// that release's chart (observability.go).
	PostRenderers              map[string]string
	MCPPrometheusPostRenderers string
	MCPPrometheusChartVersion  string
	// HarnessDevImage is the `harness` dev image as the platform Harness pins
	// it — the lab registry's ref by digest (devimages.go), forwarded as
	// kagent.harness.image; empty without the target, or in a render that
	// cannot ask the registry. LocalRegistryEndpoint is the lab registry as
	// Substrate's atelet reaches it on the kind docker network
	// (--localhost-registry-replacement): a fixed name per cluster, rendered
	// whenever the agents are, so the flag is in place before any swap.
	HarnessDevImage       string
	LocalRegistryEndpoint string
	// LabCA is the lab CA's certificate (PEM), rendered as the extra upstream
	// trust of Substrate's egress gateway: it terminates the actors' TLS and
	// dials the models Gateway, whose certificate the lab CA signs, itself.
	// Read only when the agents, and so Substrate, render.
	LabCA string
	// ConnectivityChart is the connectivity component's source when the
	// meta chart comes from a checkout (platform.chartPath, connectivity.go):
	// the lab registry as pods reach it and the version the checkout's
	// chart is pushed as, the meta chart's own. Nil for a registry chart,
	// whose connectivity is published with it.
	ConnectivityChart *localConnectivityChart
	// AteletImageCacheArgs is the lab's atelet image-cache policy
	// (ateletImageCacheArgs, substrate.go), rendered after the registry flag
	// in the same substrate.atelet.extraArgs list.
	AteletImageCacheArgs []string
	// WorkerPoolArchLabel and WorkerPoolArch are the WorkerPool's CPU
	// feature-set pin (substrate.go): the node label, and the architecture
	// this host's only node carries. The chart's pin is the fleet's amd64,
	// and an arm64 lab whose pool keeps it never schedules a worker.
	WorkerPoolArchLabel string
	WorkerPoolArch      string
	// SubstrateNodeKey is the label and taint key of the substrateNodes
	// workers (config.SubstrateNodeKey): the kind config puts it on the
	// nodes, the values pin atelet and the WorkerPool to them.
	SubstrateNodeKey string
	// GitHubToken mirrors gitHubTokenWired(cfg): githubToken.source is
	// recorded or $GITHUB_TOKEN is set on the host (githubtoken.go), so the
	// values name the Secret the lab places it in — the portal's
	// extraEnvVarsSecrets and the overlay's integrations.github,
	// agent-manager's skills.github.tokenSecret, the migrate Job's
	// githubToken. Only ever the Secret's name, never the token.
	GitHubToken bool
	// BackstageAppConfigChecksum is the data checksum of the lab's app-config
	// overlay (appconfig.go): stamped on the overlay ConfigMap, where
	// `agentlab status` reads a hand edit against it, and carried in the
	// overlay's backstage.extraAppConfig entry, whose checksum the chart folds
	// into the pod template, so a changed overlay rolls Backstage through
	// helm-controller. Empty while Backstage is off.
	BackstageAppConfigChecksum string
}

// labCAFile is the CA certificate newTmplData renders as LabCA; the tests
// point it at a fixture.
var labCAFile = caCertPath

func newTmplData(cfg *config.Config) (*tmplData, error) {
	vmManagerGuestImage, err := vmManagerGuestImageFor(cfg)
	if err != nil {
		return nil, err
	}
	connectivity, err := localConnectivityChartFor(cfg)
	if err != nil {
		return nil, err
	}
	certsDir, err := filepath.Abs("certs")
	if err != nil {
		return nil, err
	}
	nodeFiles, err := nodeFilesDir()
	if err != nil {
		return nil, err
	}
	// Only for a lab that names host model servers (the managed ones or
	// extraModels), best effort like the endpoints below: a pre-boot render
	// has no kind network, and `agentlab platform` renders again once it
	// exists.
	var labHostIP string
	if cfg.ModelManagerEnabled() || len(cfg.Platform.ExtraModels) > 0 {
		labHostIP, _ = kindGatewayIPFn(cfg.ControlPlaneNode())
	}
	endpoints := map[string]string{}
	if cfg.ModelManagerEnabled() {
		if endpoints, err = resolveBackendEndpoints(cfg); err != nil {
			note("model-manager endpoints left empty in the render: %v", err)
			endpoints = map[string]string{}
		}
	}
	// The patches a render without the component charts can know: the
	// dev-image overrides with the table's names, no dex-localhost sidecar.
	// `agentlab platform` re-renders with what the component renders say —
	// the names (devimages.go) and the sidecar's targets (postrenderers.go).
	postRenderers, err := componentPostRenderers(cfg, defaultDevImageNames(cfg), nil)
	if err != nil {
		return nil, err
	}
	var mcpPrometheusRenderers []postRenderer
	if dexLocalhostBridged(cfg) {
		mcpPrometheusRenderers = []postRenderer{sidecarPostRenderer(mcpPrometheusRelease, cfg.DexPort)}
	}
	mcpPrometheus, err := yaml.Marshal(mcpPrometheusRenderers)
	if err != nil {
		return nil, err
	}
	var labCA string
	if cfg.Platform.Agents && !cfg.LegacyChart() {
		caFile := labCAFile
		if cfg.Platform.TLS.Set() {
			caFile = trustBundleFile(cfg)
		}
		raw, err := os.ReadFile(caFile) // #nosec G304 -- the lab's own CA bundle
		if err != nil {
			return nil, fmt.Errorf("the lab CA for Substrate's egress trust: %w", err)
		}
		labCA = strings.TrimRight(string(raw), "\n")
	}
	workspaceProviders, err := workspaceProvidersFor(cfg)
	if err != nil {
		return nil, err
	}
	return &tmplData{
		Config:                     cfg,
		LabCA:                      labCA,
		PostRenderers:              postRenderers,
		MCPPrometheusPostRenderers: strings.TrimRight(string(mcpPrometheus), "\n"),
		MCPPrometheusChartVersion:  mcpPrometheusChartVersion,
		LocalRegistryEndpoint:      devRegistryEndpoint(cfg),
		ConnectivityChart:          connectivity,
		AteletImageCacheArgs:       ateletImageCacheArgs,
		WorkerPoolArchLabel:        workerPoolArchLabel,
		WorkerPoolArch:             workerPoolArch(),
		SubstrateNodeKey:           config.SubstrateNodeKey,
		GitHubToken:                gitHubTokenWired(cfg),
		ModelManagerEnabled:        cfg.ModelManagerEnabled(),
		LegacyChart:                cfg.LegacyChart(),
		FamilylessChart:            cfg.FamilylessChart(),
		ModelManagerBackends:       cfg.ChartBackends(),
		ModelManagerEndpoints:      endpoints,
		ExtraModels:                cfg.Platform.ExtraModels,
		OAuthFixtureServer:         oauthFixtureServer,
		GitHubMCPServer:            gitHubMCPServer,
		GitHubMCPClientSecret:      cfg.Platform.GitHub.ClientSecret(),
		GitHubSignIn:               gitHubSignInValuesFor(cfg),
		OAuthFixtureURL:            oauthFixtureURL,
		VMManagerEnabled:           cfg.VMManagerEnabled(),
		VMManagerGuestImage:        vmManagerGuestImage,
		KlausGatewayEnabled:        cfg.KlausGatewayEnabled(),
		KlausGateway:               klausGatewayValuesFor(cfg),
		ServingEnabled:             cfg.ServingEnabled(),
		Serving:                    servingValuesFor(),
		WorkspacesEnabled:          cfg.WorkspacesEnabled(),
		WorkspacesChart:            workspacesChartCarries(cfg),
		Workspaces:                 workspacesValuesFor(cfg),
		WorkspaceProviders:         workspaceProviders,
		WorkspaceFake:              cfg.WorkspaceFakeProvider(),
		CertsDir:                   certsDir,
		NodeFilesDir:               nodeFiles,
		MusterNodePort:             config.MusterNodePort,
		KagentUINodePort:           config.KagentUINodePort,
		GatewayNodePort:            config.GatewayNodePort,
		GatewayPublicNodePort:      config.GatewayPublicNodePort,
		BrowserCallbackPort:        config.BrowserCallbackPort,
		DomainRegex:                strings.ReplaceAll(cfg.Platform.Domain, ".", `\.`),
		LabHostName:                labHostName,
		LabHostIP:                  labHostIP,
		AllGroups:                  config.Groups,
		KubernetesClientID:         config.KubernetesClientID,
		KubernetesClientSecret:     config.KubernetesClientSecret,
		AgentPlatformClientID:      config.AgentPlatformClientID,
		AgentPlatformClientSecret:  config.AgentPlatformClientSecret,
	}, nil
}

// dexIssuerService is the ClusterIP Service pods reach the issuer under the
// platform domain through (platform.tls; dex.yaml.tmpl, coredns.yaml.tmpl).
const dexIssuerService = "dex-issuer"

// DexIssuerService is dexIssuerService for the templates.
func (t *tmplData) DexIssuerService() string { return dexIssuerService }

// DexJWKSHost and DexJWKSPort are where the agentgateway routes fetch the
// lab Dex's keys over TLS: its Service by the in-cluster name the lab-CA
// leaf carries, or — with an externally provisioned pair, which carries no
// in-cluster name — the issuer's own host and port.
func (t *tmplData) DexJWKSHost() string {
	if t.Platform.TLS.Set() {
		return t.DexHost()
	}
	return dexServiceHost
}

// DexJWKSPort: see DexJWKSHost.
func (t *tmplData) DexJWKSPort() int {
	if t.Platform.TLS.Set() {
		return t.DexPort
	}
	return 5556
}

// ExtraPostRenderers is the part of PostRenderers the values template's
// roster does not render under these settings: the components it names no
// block for — cluster-manager, which only an overlay turns on — and the ones
// whose block it renders only while the lab's own toggle is on (vm-manager,
// klaus-gateway, agent-manager with the agents). The template's trailing
// range renders these as `<component>: {postRenderers: …}` so every patch
// the rule found reaches its release and no component key renders twice.
// Mirrors the template's conditions: a block that is always there (muster,
// mcp-kubernetes, kagent, model-manager, backstage — the last three carry
// their `enabled` from a toggle and render their patches only while it is
// on) is never extra.
func (t *tmplData) ExtraPostRenderers() map[string]string {
	named := map[string]bool{
		componentMuster:           true,
		componentMCPKubernetes:    true,
		componentKagent:           true,
		componentBackstage:        true,
		modelManagerMCPServer:     true,
		agentManagerMCPServer:     t.Platform.Agents,
		vmManagerMCPServer:        t.VMManagerEnabled,
		klausGatewayComponent:     t.KlausGatewayEnabled,
		llmisvcResourcesComponent: t.ServingEnabled,
	}
	extra := map[string]string{}
	for component, postRenderers := range t.PostRenderers {
		if !named[component] {
			extra[component] = postRenderers
		}
	}
	return extra
}

var tmplFuncs = template.FuncMap{
	// userID derives a stable UUID-shaped id from the email, so renders are
	// deterministic and adding a user never renumbers the others.
	"userID": func(email string) string {
		sum := sha256.Sum256([]byte("agentlab-user:" + email))
		h := hex.EncodeToString(sum[:16])
		return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
	},
	"join": func(items []string, sep string) string { return strings.Join(items, sep) },
	// indent prefixes every non-empty line, for nesting rendered content
	// into a YAML block scalar.
	"indent": func(n int, s string) string {
		pad := strings.Repeat(" ", n)
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			if l != "" {
				lines[i] = pad + l
			}
		}
		return strings.Join(lines, "\n")
	},
}

// platformValuesTemplate renders the meta chart's lab values (the lab
// shape, platform.go); backstageOverlayTemplate the lab's Backstage catalog
// and app-config overlay.
const (
	platformValuesTemplate   = "agent-platform-values.yaml.tmpl"
	backstageOverlayTemplate = "backstage-catalog.yaml.tmpl"
)

// renderTemplate renders one embedded template with the config; mutate, when
// given, adjusts the template data first (the platform run hands
// extra-models.yaml.tmpl the statically wired host models this way).
func renderTemplate(cfg *config.Config, name string, mutate func(*tmplData)) ([]byte, error) {
	data, err := newTmplData(cfg)
	if err != nil {
		return nil, err
	}
	if mutate != nil {
		mutate(data)
	}
	if cfg.Backstage.Enabled && (name == platformValuesTemplate || name == backstageOverlayTemplate) {
		if data.BackstageAppConfigChecksum, err = overlayDataChecksum(data); err != nil {
			return nil, err
		}
	}
	return executeTemplate(name, data)
}

// executeTemplate renders one embedded template with the data as it is.
func executeTemplate(name string, data *tmplData) ([]byte, error) {
	t, err := template.New(name).Funcs(tmplFuncs).Option("missingkey=error").
		ParseFS(templatesFS, "templates/"+name)
	if err != nil {
		return nil, fmt.Errorf("parsing template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("rendering %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// manifests is the one inventory of embedded templates: the state/ file each
// renders to, plus the extra checksum inputs for the ones that stamp an input
// checksum into their pod template. RenderAll and the lifecycle steps consume
// the same table, so `agentlab render` writes exactly the bytes the lifecycle
// applies.
var manifests = map[string]struct {
	out         string
	extraInputs func(*config.Config) []string
}{
	"kind-config.yaml.tmpl":                  {out: "kind-config.yaml"},
	"rbac.yaml.tmpl":                         {out: "rbac.yaml"},
	platformValuesTemplate:                   {out: "agent-platform-values.yaml"},
	"kube-prometheus-stack-values.yaml.tmpl": {out: "kube-prometheus-stack-values.yaml"},
	certManagerValuesTemplate:                {out: "cert-manager-values.yaml"},
	mcpPrometheusTemplate:                    {out: "mcp-prometheus.yaml"},
	"observability-route.yaml.tmpl":          {out: "observability-route.yaml"},
	"demo-workflow.yaml.tmpl":                {out: "demo-workflow.yaml"},
	"oauth-fixture.yaml.tmpl":                {out: "oauth-fixture.yaml"},
	"github-mcp.yaml.tmpl":                   {out: "github-mcp.yaml"},
	"extra-models.yaml.tmpl":                 {out: "extra-models.yaml"},
	"coredns.yaml.tmpl":                      {out: "coredns.yaml"},
	"gateway-nodeport.yaml.tmpl":             {out: "gateway-nodeport.yaml"},
	"backstage-catalog.yaml.tmpl":            {out: "backstage-catalog.yaml"},
	"dex.yaml.tmpl":                          {out: "dex.yaml", extraInputs: dexCertInputs},
	workspacesTemplate:                       {out: "workspaces.yaml"},
}

// dexCertInputs is the certificate Dex serves (dexServingPair), so a
// rotated pair — a re-mint, or an externally provisioned one renewed — rolls
// the pod.
func dexCertInputs(cfg *config.Config) []string {
	cert, _ := dexServingPair(cfg)
	return []string{cert}
}

// renderManifest renders one embedded template into state/ per the manifests
// table and returns the content and the written path. When the template
// carries the checksum placeholder, it is replaced with sha256 over the
// *unstamped* render plus the extra input files (certs), so the pod rolls
// exactly when config or certs change and an unchanged re-apply is a pure
// no-op.
func renderManifest(cfg *config.Config, tmplName string) ([]byte, string, error) {
	return renderManifestWith(cfg, tmplName, nil)
}

// renderManifestWith is renderManifest with a template-data mutator.
func renderManifestWith(cfg *config.Config, tmplName string, mutate func(*tmplData)) ([]byte, string, error) {
	spec, ok := manifests[tmplName]
	if !ok {
		return nil, "", fmt.Errorf("template %s is not in the manifests table", tmplName)
	}
	content, err := renderTemplate(cfg, tmplName, mutate)
	if err != nil {
		return nil, "", err
	}
	if bytes.Contains(content, []byte(checksumPlaceholder)) {
		h := sha256.New()
		h.Write(content)
		var inputs []string
		if spec.extraInputs != nil {
			inputs = spec.extraInputs(cfg)
		}
		for _, path := range inputs {
			raw, err := os.ReadFile(path) // #nosec G304 -- lab-owned cert paths from the manifests table
			if err != nil {
				return nil, "", fmt.Errorf("checksum input %s: %w", path, err)
			}
			h.Write(raw)
		}
		sum := hex.EncodeToString(h.Sum(nil))
		content = bytes.Replace(content, []byte(checksumPlaceholder), []byte(sum), 1)
	}
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return nil, "", err
	}
	path := filepath.Join(StateDir, spec.out)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return nil, "", err
	}
	return content, path, nil
}

// RenderAll renders every manifest into state/ for inspection. The stamped
// manifest (dex) needs the certs, so they are generated first if missing.
// The extra models include the statically wired host models when the host
// servers answer (best effort: the platform run is where a failure counts).
func RenderAll(cfg *config.Config) error {
	if err := GenCerts(cfg.Platform.Domain, false); err != nil {
		return err
	}
	if err := writeIssuerFiles(cfg); err != nil {
		return err
	}
	if err := writeNodeFiles(); err != nil {
		return err
	}
	extraModels := cfg.Platform.ExtraModels
	for _, tmpl := range slices.Sorted(maps.Keys(manifests)) {
		var mutate func(*tmplData)
		if tmpl == extraModelsTemplate {
			mutate = func(d *tmplData) { d.ExtraModels = extraModels }
		}
		if _, _, err := renderManifestWith(cfg, tmpl, mutate); err != nil {
			return err
		}
	}
	fmt.Printf("Rendered %s/\n", StateDir)
	return nil
}
