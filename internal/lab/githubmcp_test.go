package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestGitHubMCPTemplate pins the MCPServer to an installation's GitHub shape:
// the hosted server, a pinned GitHub authorization server whose client is the
// Secret by name, the grant filed per person, and no credential in the render
// even with the client variables set.
func TestGitHubMCPTemplate(t *testing.T) {
	t.Setenv(GitHubMCPClientIDEnv, "Iv1.lab-client-id")
	t.Setenv(GitHubMCPClientSecretEnv, "lab-client-secret-value")
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
		"forwardToken: false",
		"issuer: https://github.com/login/oauth",
		"name: " + gitHubMCPClientSecret,
		"grantScope: subject",
		"app.kubernetes.io/managed-by: agentlab",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered MCPServer missing %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"Iv1.lab-client-id", "lab-client-secret-value", "kind: Secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("the render must not carry the client credentials (%q):\n%s", secret, out)
		}
	}
}
