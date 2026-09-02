package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestOAuthFixtureTemplate pins the fixture CR to what the proofs look for:
// the name the Go side asserts on, muster's own protected endpoint as the
// target, an oauth auth block without SSO (forwardToken off, no token
// exchange — either would make core_auth_login refuse the manual login), and
// its purpose spelled out on the object.
func TestOAuthFixtureTemplate(t *testing.T) {
	raw, err := renderTemplate(config.Default(), "oauth-fixture.yaml.tmpl")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)
	for _, want := range []string{
		"kind: MCPServer",
		"name: " + oauthFixtureServer,
		"namespace: " + platformNamespace,
		"url: " + oauthFixtureURL,
		"type: streamable-http",
		"type: oauth",
		"forwardToken: false",
		"autoStart: true",
		"agentlab.giantswarm.io/purpose:",
		"app.kubernetes.io/managed-by: agentlab",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered fixture missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tokenExchange") {
		t.Errorf("the fixture must not be an SSO server (token exchange):\n%s", out)
	}
	if got := strings.Count(out, "kind: MCPServer"); got != 1 {
		t.Errorf("want exactly one MCPServer, got %d:\n%s", got, out)
	}
}

// TestPlatformValuesEnableOAuthClient: the umbrella leaves muster's OAuth
// client role (oauth.mcpClient) off; the lab's values turn it on with the
// public URL every sign-in challenge must carry.
func TestPlatformValuesEnableOAuthClient(t *testing.T) {
	cfg := config.Default()
	raw, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)
	if got := strings.Count(out, "mcpClient:"); got != 1 {
		t.Fatalf("want exactly one mcpClient block, got %d:\n%s", got, out)
	}
	block := out[strings.Index(out, "mcpClient:"):]
	if end := strings.Index(block, "\n      server:"); end > 0 {
		block = block[:end]
	}
	for _, want := range []string{
		"enabled: true",
		`publicUrl: "` + cfg.MusterBaseURL() + `"`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("mcpClient block missing %q:\n%s", want, block)
		}
	}
}

func TestIsAuthRequiredState(t *testing.T) {
	for _, s := range []string{"Auth Required", "auth_required", "AUTH REQUIRED"} {
		if !isAuthRequiredState(s) {
			t.Errorf("%q should read as auth required", s)
		}
	}
	for _, s := range []string{"Connected", "Failed", "", "reauth_required"} {
		if isAuthRequiredState(s) {
			t.Errorf("%q must not read as auth required", s)
		}
	}
}
