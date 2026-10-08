package lab

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestGitHubMCPTemplate pins the MCPServer to an installation's GitHub shape:
// the hosted server, a pinned GitHub authorization server whose client is the
// Secret by name and namespace, the grant filed per person, and no Secret in
// the render.
func TestGitHubMCPTemplate(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.GitHub.Enabled = true
	raw, err := renderTemplate(cfg, "github-mcp.yaml.tmpl", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)
	for _, want := range []string{
		"kind: MCPServer",
		"name: " + gitHubMCPServer,
		"namespace: " + platformNamespace,
		"url: https://api.githubcopilot.com/mcp/",
		"toolPrefix: " + gitHubMCPServer,
		"type: oauth",
		renderedNoForwardToken,
		"issuer: https://github.com/login/oauth",
		"name: " + config.DefaultGitHubSecretName,
		"namespace: " + config.DefaultGitHubSecretNamespace,
		"grantScope: subject",
		renderedManagedByAgentlab,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered MCPServer missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "kind: Secret") {
		t.Errorf("the render must not carry a Secret:\n%s", out)
	}
}

// TestGitHubMCPTemplateNamesTheConfiguredSecret: platform.github.secret
// points muster at another Secret, in another namespace.
func TestGitHubMCPTemplateNamesTheConfiguredSecret(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.GitHub.Enabled = true
	cfg.Platform.GitHub.Secret = config.SecretRef{Name: "lab-github-client", Namespace: "lab-secrets"}
	raw, err := renderTemplate(cfg, "github-mcp.yaml.tmpl", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)
	for _, want := range []string{"name: lab-github-client", "namespace: lab-secrets"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered MCPServer missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, config.DefaultGitHubSecretName) {
		t.Errorf("the default Secret must not be rendered next to the configured one:\n%s", out)
	}
}

// TestMissingSecretKeys: the check names absent and empty keys, in the
// order asked for, and nothing of the values.
func TestMissingSecretKeys(t *testing.T) {
	secret := func(data map[string]any) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{Object: map[string]any{fieldKind: kindSecret}}
		if data != nil {
			obj.Object["data"] = data
		}
		return obj
	}
	idKey, secretKey := config.GitHubClientIDKey, config.GitHubClientSecretKey
	missingID, missingSecret := "key "+idKey, "key "+secretKey
	// base64 of a value, as the apiserver answers it.
	const id, sec = "SXYxLmxhYi1jbGllbnQtaWQ=", "bGFiLWNsaWVudC1zZWNyZXQtdmFsdWU="
	for _, tc := range []struct {
		name string
		data map[string]any
		want []string
	}{
		{"no data", nil, []string{missingID, missingSecret}},
		{"id only", map[string]any{idKey: id}, []string{missingSecret}},
		{"secret only", map[string]any{secretKey: sec}, []string{missingID}},
		{"empty secret", map[string]any{idKey: id, secretKey: ""}, []string{missingSecret}},
		{"both", map[string]any{idKey: id, secretKey: sec}, nil},
	} {
		got := missingSecretKeys(secret(tc.data), gitHubMCPClientKeys...)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: missing %v, want %v", tc.name, got, tc.want)
		}
		for _, m := range got {
			if strings.Contains(m, id) || strings.Contains(m, sec) {
				t.Errorf("%s: the answer carries a value: %q", tc.name, m)
			}
		}
	}
}
