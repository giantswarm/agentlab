package lab

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestDexTemplateRegistersThePortalClient: the portal's own client carries
// only the portal's callbacks, and every client whose audience the portal
// requests (agent-platform, kubernetes, dex-k8s-authenticator) lists it under
// trustedPeers.
func TestDexTemplateRegistersThePortalClient(t *testing.T) {
	raw, err := renderTemplate(config.Default(), "dex.yaml.tmpl", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)
	start := strings.Index(out, "- id: "+config.PortalClientID)
	if start < 0 {
		t.Fatalf("rendered Dex has no client %s:\n%s", config.PortalClientID, out)
	}
	client := out[start:]
	if end := strings.Index(client, "\n      - id: "); end >= 0 {
		client = client[:end]
	}
	if !strings.Contains(client, "secret: "+config.PortalClientSecret) {
		t.Errorf("the portal client carries no secret %s:\n%s", config.PortalClientSecret, client)
	}
	for _, line := range strings.Split(client, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- http") && !strings.HasSuffix(line, "/api/auth/oidc-agent-platform/handler/frame") {
			t.Errorf("the portal client admits a redirect URI that is not the portal's: %s", line)
		}
	}
	if got := strings.Count(out, "trustedPeers:\n          - agent-platform\n          - "+config.PortalClientID); got != 2 {
		t.Errorf("kubernetes and dex-k8s-authenticator must both trust agent-platform and %s, got %d:\n%s", config.PortalClientID, got, out)
	}
	if !strings.Contains(out, "/oauth/proxy/callback\n        # The portal requests this client's audience") ||
		!strings.Contains(out, "trustedPeers:\n          - "+config.PortalClientID+"\n      # The portal's own client") {
		t.Errorf("agent-platform must trust %s for its audience:\n%s", config.PortalClientID, out)
	}
}

// TestPlatformValuesAdmitHotReloadPortalReturn: while a hot-reload run is
// configured (backstage.localDevPort) muster's own return allowlist carries
// the frontend's /sessions, so the approval page can return the browser to
// `agentlab portal dev`; otherwise the lab sets no list of its own.
func TestPlatformValuesAdmitHotReloadPortalReturn(t *testing.T) {
	allowlist := func(cfg *config.Config) any {
		raw, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl", nil)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(raw, &values); err != nil {
			t.Fatalf("%v\n%s", err, raw)
		}
		var cur any = values
		for _, key := range strings.Split("muster.muster.oauth.mcpClient.postLoginRedirectAllowlist", ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[key]
		}
		return cur
	}
	cfg := config.Default()
	if got := allowlist(cfg); got != nil {
		t.Errorf("no hot-reload run: postLoginRedirectAllowlist = %v, want unset", got)
	}
	cfg.Backstage.LocalDevPort = config.DefaultBackstageLocalDevPort
	got, want := allowlist(cfg), []any{"http://localhost:3000/sessions"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hot-reload run: postLoginRedirectAllowlist = %v, want %v", got, want)
	}
}
