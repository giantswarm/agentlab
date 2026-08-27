package lab

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"dexlab/internal/config"
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
		sum := sha1.Sum([]byte("dexlab-user:" + email))
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

// renderToState renders a template into state/<outName> and returns both the
// content and the path.
func renderToState(cfg *config.Config, tmplName, outName string) ([]byte, string, error) {
	content, err := renderTemplate(cfg, tmplName)
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(StateDir, 0o755); err != nil {
		return nil, "", err
	}
	path := filepath.Join(StateDir, outName)
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
	plain := map[string]string{
		"kind-config.yaml.tmpl":           "kind-config.yaml",
		"rbac.yaml.tmpl":                  "rbac.yaml",
		"agent-platform-values.yaml.tmpl": "agent-platform-values.yaml",
		"mcp-kubernetes-values.yaml.tmpl": "mcp-kubernetes-values.yaml",
		"demo-workflow.yaml.tmpl":         "demo-workflow.yaml",
	}
	for tmpl, out := range plain {
		if _, _, err := renderToState(cfg, tmpl, out); err != nil {
			return err
		}
	}
	if _, _, err := renderStamped(cfg, "dex.yaml.tmpl", "dex.yaml", "certs/tls.crt"); err != nil {
		return err
	}
	if _, _, err := renderStamped(cfg, "backstage.yaml.tmpl", "backstage.yaml", "certs/ca.crt"); err != nil {
		return err
	}
	fmt.Printf("Rendered %s/\n", StateDir)
	return nil
}

// renderStamped renders a template, then replaces the checksum placeholder
// with sha256 over the *unstamped* render plus the extra input files (certs),
// so the pod rolls exactly when config or certs change and an unchanged
// re-apply is a pure no-op.
func renderStamped(cfg *config.Config, tmplName, outName string, extraInputs ...string) ([]byte, string, error) {
	content, err := renderTemplate(cfg, tmplName)
	if err != nil {
		return nil, "", err
	}
	h := sha256.New()
	h.Write(content)
	for _, path := range extraInputs {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("checksum input %s: %w", path, err)
		}
		h.Write(raw)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	stamped := bytes.Replace(content, []byte(checksumPlaceholder), []byte(sum), 1)
	if err := os.MkdirAll(StateDir, 0o755); err != nil {
		return nil, "", err
	}
	path := filepath.Join(StateDir, outName)
	if err := os.WriteFile(path, stamped, 0o644); err != nil {
		return nil, "", err
	}
	return stamped, path, nil
}
