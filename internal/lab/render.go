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

	"github.com/giantswarm/agentlab/internal/config"
)

//go:embed templates/*.tmpl templates/static/*
var templatesFS embed.FS

// gatewayAPICRDs is the Gateway API standard-channel install (v1.5.0),
// embedded so a boot needs no network to give the cluster the Gateway/
// HTTPRoute/... CRDs — the documented cluster-level prerequisite of the
// agent-platform-standalone chart.
//
//go:embed templates/gateway-api-crds.yaml
var gatewayAPICRDs []byte

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
	CertsDir            string // absolute, for the kind extraMount
	MusterNodePort      int
	KagentUINodePort    int
	GatewayNodePort     int
	BrowserCallbackPort int
	DomainRegex         string // Platform.Domain with dots escaped, for the CoreDNS rewrite
	AllGroups           []string
	KubernetesClientSecret,
	AgentPlatformClientSecret string
	// ModelManagerEnabled mirrors cfg.ModelManagerEnabled(); the backend is
	// the first of platform.modelManager.backends and the endpoint the URL
	// model-manager dials for it (autodetected from the kind docker network
	// — empty when the cluster does not exist yet, which only a pre-boot
	// `agentlab render` sees; platformUp resolves it strictly).
	ModelManagerEnabled  bool
	ModelManagerBackend  string
	ModelManagerEndpoint string
	// ExtraModels is what extra-models.yaml.tmpl renders: the
	// platform.extraModels entries, plus — when the platform run computes
	// them — the tool-calling models of the host backends model-manager does
	// not front (hostmodels.go).
	ExtraModels []config.ExtraModel
	// The per-server OAuth sign-in fixture (oauthfixture.go): the MCPServer
	// name the proofs sign in to and the protected endpoint it points at.
	OAuthFixtureServer string
	OAuthFixtureURL    string
}

func newTmplData(cfg *config.Config) (*tmplData, error) {
	certsDir, err := filepath.Abs("certs")
	if err != nil {
		return nil, err
	}
	endpoint := ""
	if cfg.ModelManagerEnabled() {
		if endpoint, err = resolveBackendEndpoint(cfg, cfg.Platform.ModelManager.Primary()); err != nil {
			note("model-manager endpoint left empty in the render: %v", err)
			endpoint = ""
		}
	}
	return &tmplData{
		Config:                    cfg,
		ModelManagerEnabled:       cfg.ModelManagerEnabled(),
		ModelManagerBackend:       cfg.Platform.ModelManager.Primary(),
		ModelManagerEndpoint:      endpoint,
		ExtraModels:               cfg.Platform.ExtraModels,
		OAuthFixtureServer:        oauthFixtureServer,
		OAuthFixtureURL:           oauthFixtureURL,
		CertsDir:                  certsDir,
		MusterNodePort:            config.MusterNodePort,
		KagentUINodePort:          config.KagentUINodePort,
		GatewayNodePort:           config.GatewayNodePort,
		BrowserCallbackPort:       config.BrowserCallbackPort,
		DomainRegex:               strings.ReplaceAll(cfg.Platform.Domain, ".", `\.`),
		AllGroups:                 config.Groups,
		KubernetesClientSecret:    config.KubernetesClientSecret,
		AgentPlatformClientSecret: config.AgentPlatformClientSecret,
	}, nil
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
	// staticFile inlines an embedded asset from templates/static/ verbatim —
	// content that must bypass template rendering, like the scaffolder
	// Template whose ${{ … }} expressions Go's text/template would try to
	// parse as actions.
	"staticFile": func(name string) (string, error) {
		raw, err := templatesFS.ReadFile("templates/static/" + name)
		return string(raw), err
	},
	// indent prefixes every non-empty line, for nesting staticFile content
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
	extraInputs []string
}{
	"kind-config.yaml.tmpl":                  {out: "kind-config.yaml"},
	"rbac.yaml.tmpl":                         {out: "rbac.yaml"},
	"agent-platform-values.yaml.tmpl":        {out: "agent-platform-values.yaml"},
	"flux-values.yaml.tmpl":                  {out: "flux-values.yaml"},
	"kube-prometheus-stack-values.yaml.tmpl": {out: "kube-prometheus-stack-values.yaml"},
	"mcp-prometheus-values.yaml.tmpl":        {out: "mcp-prometheus-values.yaml"},
	"observability-route.yaml.tmpl":          {out: "observability-route.yaml"},
	"demo-workflow.yaml.tmpl":                {out: "demo-workflow.yaml"},
	"oauth-fixture.yaml.tmpl":                {out: "oauth-fixture.yaml"},
	"extra-models.yaml.tmpl":                 {out: "extra-models.yaml"},
	"coredns.yaml.tmpl":                      {out: "coredns.yaml"},
	"gateway-nodeport.yaml.tmpl":             {out: "gateway-nodeport.yaml"},
	"backstage-catalog.yaml.tmpl":            {out: "backstage-catalog.yaml"},
	"dex.yaml.tmpl":                          {out: "dex.yaml", extraInputs: []string{tlsCertPath}},
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
		for _, path := range spec.extraInputs {
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
	extraModels := cfg.Platform.ExtraModels
	if models, _, err := allExtraModels(cfg, nil); err == nil {
		extraModels = models
	} else {
		note("extra-models.yaml rendered without the host backends' models: %v", err)
	}
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
