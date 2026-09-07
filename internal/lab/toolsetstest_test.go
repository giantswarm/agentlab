package lab

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// The fixture pins Dex as its authorization server with the platform client,
// and ships the Secret that client is read from.
func TestOAuthFixtureRenderPinsDex(t *testing.T) {
	cfg := config.Default()
	out, err := renderTemplate(cfg, "oauth-fixture.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"authorizationServer:",
		"issuer: " + cfg.Issuer(),
		"name: " + oauthFixtureServer + "-client",
		"scopes: openid profile email offline_access",
		"kind: Secret",
		"client-id: " + config.AgentPlatformClientID,
		"client-secret: " + config.AgentPlatformClientSecret,
		"forwardToken: false",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered fixture lacks %q:\n%s", want, s)
		}
	}
}

// Dex lists muster's proxy callback on the platform client, next to the
// server-role callback and Backstage's handler.
func TestDexRenderRegistersProxyCallback(t *testing.T) {
	cfg := config.Default()
	out, err := renderTemplate(cfg, "dex.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		cfg.MusterBaseURL() + oauthProxyCallbackPath,
		cfg.MusterBaseURL() + "/oauth/callback",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered Dex config lacks the redirect URI %q", want)
		}
	}
}

func TestToolNamesInReply(t *testing.T) {
	cases := []struct {
		reply string
		want  []string
	}{
		{"x_mcp-kubernetes_list\nx_mcp-kubernetes_get\nworkflow_lab-cluster-overview", []string{"workflow_lab-cluster-overview", "x_mcp-kubernetes_get", "x_mcp-kubernetes_list"}},
		{"Here are the tools:\n- `x_mcp-kubernetes_list`\n- `core_workflow_list`", []string{"core_workflow_list", "x_mcp-kubernetes_list"}},
		{"x_a_b, x_a_b, x_c_d", []string{"x_a_b", "x_c_d"}},
		{"NO_TOOLS", nil},
		{"I have no tools available.", nil},
	}
	for _, tc := range cases {
		if got := toolNamesInReply(tc.reply); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("toolNamesInReply(%q) = %v, want %v", tc.reply, got, tc.want)
		}
	}
}

// The composed manifest is the composer's shape: OCIRepository first, then
// the HelmRelease with the top-level toolset value.
func TestComposeAgentManifest(t *testing.T) {
	m := composeAgentManifest("probe", "default-model-config", []string{presetReadOnly, "workflow:incident-triage"})
	docs := strings.Split(m, "\n---\n")
	if len(docs) != 2 || !strings.Contains(docs[0], "kind: OCIRepository") || !strings.Contains(docs[1], "kind: HelmRelease") {
		t.Fatalf("wanted OCIRepository then HelmRelease, got:\n%s", m)
	}
	for _, want := range []string{
		"url: oci://gsoci.azurecr.io/charts/giantswarm/agent",
		"semver: x.x.x",
		"name: probe\n  namespace: kagent",
		`toolset: ["preset:read-only", "workflow:incident-triage"]`,
		"modelConfig:\n      name: default-model-config",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
}

func TestSessionIDInChallenge(t *testing.T) {
	state := base64.RawURLEncoding.EncodeToString([]byte(`{"session_id":"ext-0123abcd","user_id":"u","server_name":"lab-oauth-fixture"}`))
	got := sessionIDInChallenge("https://muster.127.0.0.1.nip.io/oauth/proxy/start?state=" + state)
	if got != "ext-0123abcd" {
		t.Errorf("sessionIDInChallenge = %q", got)
	}
	if got := sessionIDInChallenge("https://muster.127.0.0.1.nip.io/oauth/proxy/start"); got != "" {
		t.Errorf("no state must give no session id, got %q", got)
	}
}
