package lab

import (
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// Lines the rendered MCPServer manifests carry, as the template tests assert them.
const (
	renderedNoForwardToken    = "forwardToken: false"
	renderedManagedByAgentlab = "app.kubernetes.io/managed-by: agentlab"
	renderedOAuthAuth         = "type: oauth"
)

// TestOAuthFixtureTemplate pins the fixture CR to what the proofs look for:
// the name the Go side asserts on, muster's own protected endpoint as the
// target, an oauth auth block without SSO (forwardToken off, no token
// exchange — either would make core_auth_login refuse the manual login), and
// its purpose spelled out on the object.
func TestOAuthFixtureTemplate(t *testing.T) {
	raw, err := renderTemplate(config.Default(), "oauth-fixture.yaml.tmpl", nil)
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
		renderedOAuthAuth,
		renderedNoForwardToken,
		"autoStart: true",
		"agentlab.giantswarm.io/purpose:",
		renderedManagedByAgentlab,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered fixture missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tokenExchange") {
		t.Errorf("the fixture must not be an SSO server (token exchange):\n%s", out)
	}
	if got := strings.Count(out, "kind: MCPServer"); got != 2 {
		t.Errorf("want the fixture and its subject-scoped twin, got %d MCPServers:\n%s", got, out)
	}
}

// TestOAuthSubjectFixtureTemplate: the twin files its grants under the
// person, under an issuer identity of its own (scope is per identity, so the
// session-scoped fixture keeps its scope), with the identifier Dex sends as
// the expected RFC 9207 iss; the fixture itself stays session-scoped.
func TestOAuthSubjectFixtureTemplate(t *testing.T) {
	cfg := config.Default()
	raw, err := renderTemplate(cfg, "oauth-fixture.yaml.tmpl", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := strings.Split(string(raw), "\n---\n")
	var fixture, twin string
	for _, doc := range docs {
		switch {
		case strings.Contains(doc, "name: "+oauthSubjectFixtureServer+"\n"):
			twin = doc
		case strings.Contains(doc, "kind: MCPServer") && strings.Contains(doc, "name: "+oauthFixtureServer+"\n"):
			fixture = doc
		}
	}
	if twin == "" || fixture == "" {
		t.Fatalf("want both %s and %s rendered:\n%s", oauthFixtureServer, oauthSubjectFixtureServer, raw)
	}
	for _, want := range []string{
		"url: " + oauthFixtureURL,
		renderedOAuthAuth,
		renderedNoForwardToken,
		"grantScope: subject",
		"issuer: " + cfg.MusterBaseURL() + "/" + oauthSubjectFixtureServer,
		"expectedIssuer: " + cfg.Issuer(),
		"name: " + oauthFixtureServer + "-client",
		renderedManagedByAgentlab,
	} {
		if !strings.Contains(twin, want) {
			t.Errorf("rendered twin missing %q:\n%s", want, twin)
		}
	}
	if strings.Contains(fixture, "grantScope") {
		t.Errorf("%s must stay session-scoped:\n%s", oauthFixtureServer, fixture)
	}
}

// TestPlatformValuesEnableOAuthClient: the umbrella leaves muster's OAuth
// client role (oauth.mcpClient) off; the lab's values turn it on with the
// public URL every sign-in challenge must carry.
func TestPlatformValuesEnableOAuthClient(t *testing.T) {
	cfg := config.Default()
	raw, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl", nil)
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

// A per-session server muster reached reads Awaiting Session from muster
// 5.28.0 on; the platform's wait must take it as reachable, next to the
// Auth Required an older muster reports and Connected.
func TestMCPServerReachableStatesTakeAwaitingSession(t *testing.T) {
	for _, s := range []string{"Awaiting Session", mcpServerStateAuthRequired, mcpServerStateConnected} {
		if !slices.Contains(mcpServerReachableStates, s) {
			t.Errorf("%q is not a reachable state: %v", s, mcpServerReachableStates)
		}
	}
	if slices.Contains(mcpServerReachableStates, "Failed") {
		t.Errorf("Failed must not count as reachable: %v", mcpServerReachableStates)
	}
}
