package lab

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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
