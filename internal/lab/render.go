package lab

import (
	"bytes"
	"crypto/sha1"
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

	"agentlab/internal/config"
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
	CertsDir            string // absolute, for the kind extraMount
	MusterNodePort      int
	BrowserCallbackPort int
	AllGroups           []string
	KubernetesClientSecret,
	BackstageClientSecret,
	MusterClientSecret string
}

func newTmplData(cfg *config.Config) (*tmplData, error) {
	certsDir, err := filepath.Abs("certs")
	if err != nil {
		return nil, err
	}
	return &tmplData{
		Config:                 cfg,
		CertsDir:               certsDir,
		MusterNodePort:         config.MusterNodePort,
		BrowserCallbackPort:    config.BrowserCallbackPort,
		AllGroups:              config.Groups,
		KubernetesClientSecret: config.KubernetesClientSecret,
		BackstageClientSecret:  config.BackstageClientSecret,
		MusterClientSecret:     config.MusterClientSecret,
	}, nil
}

var tmplFuncs = template.FuncMap{
	// userID derives a stable UUID-shaped id from the email, so renders are
	// deterministic and adding a user never renumbers the others.
	"userID": func(email string) string {
		sum := sha1.Sum([]byte("agentlab-user:" + email))
		h := hex.EncodeToString(sum[:16])
		return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
	},
	"join": func(items []string, sep string) string { return strings.Join(items, sep) },
}

// renderTemplate renders one embedded template with the config.
func renderTemplate(cfg *config.Config, name string) ([]byte, error) {
	data, err := newTmplData(cfg)
	if err != nil {
		return nil, err
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
	"kind-config.yaml.tmpl":           {out: "kind-config.yaml"},
	"rbac.yaml.tmpl":                  {out: "rbac.yaml"},
	"agent-platform-values.yaml.tmpl": {out: "agent-platform-values.yaml"},
	"mcp-kubernetes-values.yaml.tmpl": {out: "mcp-kubernetes-values.yaml"},
	"demo-workflow.yaml.tmpl":         {out: "demo-workflow.yaml"},
	"dex.yaml.tmpl":                   {out: "dex.yaml", extraInputs: []string{"certs/tls.crt"}},
	"backstage.yaml.tmpl":             {out: "backstage.yaml", extraInputs: []string{"certs/ca.crt"}},
}

// renderManifest renders one embedded template into state/ per the manifests
// table and returns the content and the written path. When the template
// carries the checksum placeholder, it is replaced with sha256 over the
// *unstamped* render plus the extra input files (certs), so the pod rolls
// exactly when config or certs change and an unchanged re-apply is a pure
// no-op.
func renderManifest(cfg *config.Config, tmplName string) ([]byte, string, error) {
	spec, ok := manifests[tmplName]
	if !ok {
		return nil, "", fmt.Errorf("template %s is not in the manifests table", tmplName)
	}
	content, err := renderTemplate(cfg, tmplName)
	if err != nil {
		return nil, "", err
	}
	if bytes.Contains(content, []byte(checksumPlaceholder)) {
		h := sha256.New()
		h.Write(content)
		for _, path := range spec.extraInputs {
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, "", fmt.Errorf("checksum input %s: %w", path, err)
			}
			h.Write(raw)
		}
		sum := hex.EncodeToString(h.Sum(nil))
		content = bytes.Replace(content, []byte(checksumPlaceholder), []byte(sum), 1)
	}
	if err := os.MkdirAll(StateDir, 0o755); err != nil {
		return nil, "", err
	}
	path := filepath.Join(StateDir, spec.out)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return nil, "", err
	}
	return content, path, nil
}

// RenderAll renders every manifest into state/ for inspection. The stamped
// manifests (dex, backstage) need the certs, so they are generated first if
// missing.
func RenderAll(cfg *config.Config) error {
	if err := GenCerts(false); err != nil {
		return err
	}
	for _, tmpl := range slices.Sorted(maps.Keys(manifests)) {
		if _, _, err := renderManifest(cfg, tmpl); err != nil {
			return err
		}
	}
	fmt.Printf("Rendered %s/\n", StateDir)
	return nil
}
