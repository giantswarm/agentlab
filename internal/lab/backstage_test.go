package lab

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// backstageOverlayAppConfig renders backstage-catalog.yaml.tmpl and returns
// the parsed app-config overlay (the agentlab-backstage-app-config
// ConfigMap's embedded YAML).
func backstageOverlayAppConfig(t *testing.T, cfg *config.Config) map[string]any {
	t.Helper()
	raw, err := renderTemplate(cfg, "backstage-catalog.yaml.tmpl", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			t.Fatalf("overlay ConfigMap agentlab-backstage-app-config not found in:\n%s", raw)
		} else if err != nil {
			t.Fatalf("rendered backstage-catalog.yaml is not valid YAML: %v\n%s", err, raw)
		}
		meta, _ := doc["metadata"].(map[string]any)
		if doc["kind"] != "ConfigMap" || meta["name"] != "agentlab-backstage-app-config" {
			continue
		}
		data, _ := doc["data"].(map[string]any)
		inner, ok := data["app-config.agentlab.yaml"].(string)
		if !ok {
			t.Fatalf("overlay ConfigMap has no data[app-config.agentlab.yaml]:\n%s", raw)
		}
		var appConfig map[string]any
		if err := yaml.Unmarshal([]byte(inner), &appConfig); err != nil {
			t.Fatalf("overlay app-config is not YAML: %v\n%s", err, inner)
		}
		return appConfig
	}
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

// TestBackstageOverlayBaseURLFollowsGatewayPort (HACKS.md U15): the umbrella's
// app-config has no port in its URLs, so the overlay must carry Backstage's
// public URL on every gatewayPort — and it must be the very URL the Dex
// client's redirectURI is built from, or logins fail with "Unregistered
// redirect_uri".
func TestBackstageOverlayBaseURLFollowsGatewayPort(t *testing.T) {
	for _, tc := range []struct {
		port int
		want string
	}{
		{443, "https://backstage.127.0.0.1.nip.io"},
		{8443, "https://backstage.127.0.0.1.nip.io:8443"},
	} {
		t.Run(fmt.Sprint(tc.port), func(t *testing.T) {
			cfg := config.Default()
			cfg.Platform.GatewayPort = tc.port
			if got := cfg.BackstageBaseURL(); got != tc.want {
				t.Fatalf("BackstageBaseURL() = %q, want %q", got, tc.want)
			}
			appConfig := backstageOverlayAppConfig(t, cfg)
			for _, path := range [][]string{
				{"app", "baseUrl"},
				{"backend", "baseUrl"},
				{"backend", "cors", "origin"},
			} {
				if got := dig(appConfig, path...); got != tc.want {
					t.Errorf("overlay %s = %v, want %q", strings.Join(path, "."), got, tc.want)
				}
			}

			dexRaw, err := renderTemplate(cfg, "dex.yaml.tmpl", nil)
			if err != nil {
				t.Fatalf("render dex: %v", err)
			}
			redirect := "- " + tc.want + "/api/auth/oidc-agent-platform/handler/frame"
			if !strings.Contains(string(dexRaw), redirect) {
				t.Errorf("Dex client redirectURI %q not registered:\n%s", redirect, dexRaw)
			}
		})
	}
}

// TestBackstageOverlayIsLastAppConfig pins the ordering the override relies
// on: Backstage gives later app-config files precedence, so the lab's overlay
// must be listed after the umbrella's file in extraAppConfig — otherwise the
// umbrella's port-free URLs win and U15 silently regresses.
func TestBackstageOverlayIsLastAppConfig(t *testing.T) {
	raw, err := renderTemplate(config.Default(), "agent-platform-values.yaml.tmpl", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var values struct {
		Backstage struct {
			Backstage struct {
				ExtraAppConfig []struct {
					Filename     string `yaml:"filename"`
					ConfigMapRef string `yaml:"configMapRef"`
				} `yaml:"extraAppConfig"`
			} `yaml:"backstage"`
		} `yaml:"backstage"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("values are not YAML: %v\n%s", err, raw)
	}
	files := values.Backstage.Backstage.ExtraAppConfig
	if len(files) < 2 {
		t.Fatalf("want the umbrella's app-config plus the lab overlay in extraAppConfig, got %+v", files)
	}
	last := files[len(files)-1]
	if last.ConfigMapRef != "agentlab-backstage-app-config" || last.Filename != "app-config.agentlab.yaml" {
		t.Errorf("lab overlay must be the LAST extraAppConfig entry (later files win the merge), got %+v", files)
	}
	var umbrella bool
	for _, f := range files[:len(files)-1] {
		umbrella = umbrella || f.ConfigMapRef == "agent-platform-backstage-app-config"
	}
	if !umbrella {
		t.Errorf("umbrella app-config agent-platform-backstage-app-config missing before the overlay: %+v", files)
	}
}

// TestAgentManagerVerdict pins backstage-test's feature detection to the
// lab's shape: with agents on, a muster without agent-manager fails the
// sign-in half (the portal would offer no installation to create agents on);
// with agents off the lab installs none, so its absence is not a failure and
// the per-server Sign in path and every user still run.
func TestAgentManagerVerdict(t *testing.T) {
	withAM := []string{agentManagerMCPServer, oauthFixtureServer, componentMCPKubernetes}
	withoutAM := []string{oauthFixtureServer, componentMCPKubernetes}
	for _, tc := range []struct {
		name    string
		servers []string
		agents  bool
		wantErr bool
	}{
		{"agents on, agent-manager listed", withAM, true, false},
		{"agents on, agent-manager missing", withoutAM, true, true},
		{"agents off, agent-manager missing", withoutAM, false, false},
		{"agents off, agent-manager listed", withAM, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, err := agentManagerVerdict(tc.servers, tc.agents)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), agentManagerMCPServer) {
					t.Fatalf("want an error naming %s, got verdict %q, err %v", agentManagerMCPServer, verdict, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if verdict == "" {
				t.Error("want a verdict line for the proof's output")
			}
		})
	}
}

// TestBackstageOverlayAIChatModel: the overlay names the AI chat's model —
// the lab's aiModel, never Backstage's OpenAI default — and the Anthropic key
// as the reference the portal's environment resolves, never a value.
func TestBackstageOverlayAIChatModel(t *testing.T) {
	cfg := config.Default()
	cfg.AIModel = "claude-opus-4-1"
	overlay := backstageOverlayAppConfig(t, cfg)
	if got := dig(overlay, "aiChat", "model"); got != cfg.AIModel {
		t.Errorf("overlay aiChat.model = %v, want the lab's aiModel %s", got, cfg.AIModel)
	}
	if got := dig(overlay, "aiChat", "anthropic", "apiKey"); got != "${"+AnthropicKeyEnv+"}" {
		t.Errorf("overlay aiChat.anthropic.apiKey = %v, want ${%s}", got, AnthropicKeyEnv)
	}
}

// TestBackstagePortalMode: backstage.portal renders app.portals.agentPlatform
// in the lab's app-config overlay — on for agent-platform (and for a file
// written before the switch), off for classic, which also sends / to the
// classic home page instead of the shell's /agent-platform — and in no mode
// do the values restate backstage.extraEnvVars: Helm replaces that list, so
// any entry of the lab's would drop the chart's NODE_EXTRA_CA_CERTS.
func TestBackstagePortalMode(t *testing.T) {
	for _, tc := range []struct {
		portal       string
		want         bool
		rootRedirect any
	}{
		{portal: config.PortalAgentPlatform, want: true},
		{portal: "", want: true},
		{portal: config.PortalClassic, want: false, rootRedirect: "/"},
	} {
		t.Run(tc.portal, func(t *testing.T) {
			cfg := config.Default()
			cfg.Backstage.Portal = tc.portal
			cfg.Normalize()
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			overlay := backstageOverlayAppConfig(t, cfg)
			if got := dig(overlay, "app", "portals", "agentPlatform"); got != tc.want {
				t.Errorf("app.portals.agentPlatform = %v, want %v", got, tc.want)
			}
			if got := dig(overlay, "app", "rootRedirect"); got != tc.rootRedirect {
				t.Errorf("app.rootRedirect = %v, want %v", got, tc.rootRedirect)
			}
			raw, err := renderTemplate(cfg, platformValuesTemplate, nil)
			if err != nil {
				t.Fatal(err)
			}
			var values map[string]any
			if err := yaml.Unmarshal(raw, &values); err != nil {
				t.Fatal(err)
			}
			if got := dig(values, componentBackstage, componentBackstage, "extraEnvVars"); got != nil {
				t.Errorf("the values restate backstage.backstage.extraEnvVars (%v): Helm replaces the chart's list, NODE_EXTRA_CA_CERTS with it", got)
			}
		})
	}
}

// TestBackstagePortalModeRefusesUnknown: a hand-edited mode is refused with
// the modes there are.
func TestBackstagePortalModeRefusesUnknown(t *testing.T) {
	cfg := config.Default()
	cfg.Backstage.Portal = "shell"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "backstage.portal") {
		t.Fatalf("Validate() = %v, want the backstage.portal refusal", err)
	}
}

// TestBackstageInstallationsPage pins backstage.installationsPage: on, the
// lab's values enable the portal's Installations page and its nav item next
// to Deployments and Clusters, so the portal's installations e2e specs have a
// page to drive; off (the default), the extensions are what they were.
func TestBackstageInstallationsPage(t *testing.T) {
	base := []string{"page:gs/deployments", "nav-item:gs/deployments", "page:gs/clusters", "nav-item:gs/clusters"}
	for _, tc := range []struct {
		name string
		on   bool
		want []string
	}{
		{"off", false, base},
		{"on", true, append(slices.Clone(base), "page:gs/installations", "nav-item:gs/installations")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Backstage.InstallationsPage = tc.on
			raw, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl", nil)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			var values struct {
				Backstage struct {
					EnabledExtensions []string `yaml:"enabledExtensions"`
				} `yaml:"backstage"`
			}
			if err := yaml.Unmarshal(raw, &values); err != nil {
				t.Fatalf("values are not YAML: %v\n%s", err, raw)
			}
			if !slices.Equal(values.Backstage.EnabledExtensions, tc.want) {
				t.Errorf("backstage.enabledExtensions = %v, want %v", values.Backstage.EnabledExtensions, tc.want)
			}
		})
	}
}
