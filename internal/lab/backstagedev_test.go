package lab

import (
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestBackstageDevOverlay pins what the local run swaps: its own URLs, and
// every in-cluster address of the chart's app-config for one this machine
// reaches.
func TestBackstageDevOverlay(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.GatewayPort = 8443
	cfg.Backstage.LocalDevPort = 7017
	raw, err := backstageDevOverlay(cfg, "https://127.0.0.1:6443", []byte("ca"), "/lab/state/backstage-dev/users.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		App struct {
			BaseURL string `yaml:"baseUrl"`
		}
		Backend struct {
			BaseURL string `yaml:"baseUrl"`
			Listen  struct {
				Port int
				Host string
			}
			Cors struct{ Origin string }
		}
		Kubernetes struct {
			ClusterLocatorMethods []struct {
				Clusters []struct {
					URL    string `yaml:"url"`
					CAData string `yaml:"caData"`
				} `yaml:"clusters"`
			} `yaml:"clusterLocatorMethods"`
		}
		Muster struct {
			Installations []struct{ URL string }
		}
		AgentPlatform struct {
			Kagent struct {
				Installations map[string]struct {
					APIBaseURL string `yaml:"apiBaseUrl"`
				}
			}
		} `yaml:"agentPlatform"`
	}
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	checks := map[string][2]string{
		"app.baseUrl":         {got.App.BaseURL, "http://localhost:3000"},
		"backend.baseUrl":     {got.Backend.BaseURL, "http://localhost:7017"},
		"backend.cors.origin": {got.Backend.Cors.Origin, "http://localhost:3000"},
		"backend.listen.host": {got.Backend.Listen.Host, "127.0.0.1"},
		"apiserver":           {got.Kubernetes.ClusterLocatorMethods[0].Clusters[0].URL, "https://127.0.0.1:6443"},
		"apiserver caData":    {got.Kubernetes.ClusterLocatorMethods[0].Clusters[0].CAData, "Y2E="},
		"muster url":          {got.Muster.Installations[0].URL, "https://muster.127.0.0.1.nip.io:8443/mcp"},
		"kagent api":          {got.AgentPlatform.Kagent.Installations[platformNamespace].APIBaseURL, "https://agentgateway.127.0.0.1.nip.io:8443"},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
	if got.Backend.Listen.Port != 7017 {
		t.Errorf("backend.listen.port = %d, want 7017", got.Backend.Listen.Port)
	}
}

// TestCheckNoIncludeKeys pins that cluster config stays data on this machine:
// the chart's own `${VAR}` interpolation passes, any $file/$include/$env
// include key — however deep, in any document — and unparseable YAML are
// refused, naming the ConfigMap, the key and the directive.
func TestCheckNoIncludeKeys(t *testing.T) {
	const chartShaped = `app:
  baseUrl: https://backstage.example
auth:
  session:
    secret: ${AUTH_SESSION_SECRET}
  providers:
    oidc-agent-platform:
      development:
        clientSecret: ${AGENT_PLATFORM_OIDC_CLIENT_SECRET}
`
	if err := checkNoIncludeKeys("cm", "app-config.yaml", []byte(chartShaped)); err != nil {
		t.Errorf("chart-shaped config with ${VAR} interpolation refused: %v", err)
	}
	for name, doc := range map[string]string{
		"$file nested deep": "backend:\n  auth:\n    keys:\n      - secret:\n          $file: /etc/passwd\n",
		"$env":              "integrations:\n  github:\n    - token:\n        $env: GITHUB_TOKEN\n",
		"$include":          "catalog:\n  $include: ../somewhere.yaml\n",
		"second document":   "app:\n  baseUrl: https://ok\n---\nproxy:\n  $file: /etc/hosts\n",
	} {
		err := checkNoIncludeKeys("cm", "app-config.yaml", []byte(doc))
		if err == nil {
			t.Errorf("%s: not refused", name)
			continue
		}
		for _, want := range []string{"cm", "app-config.yaml", "$"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: refusal %q does not name %q", name, err, want)
			}
		}
	}
	if err := checkNoIncludeKeys("cm", "broken.yaml", []byte("a: [\n")); err == nil || !strings.Contains(err.Error(), "broken.yaml") {
		t.Errorf("broken YAML: %v, want a refusal naming the key", err)
	}
}

// TestBackstageDevBaseEnv keeps the dev process's environment minimal: what
// node and yarn need passes, everything else stays out of the config's
// ${VAR} interpolation reach.
func TestBackstageDevBaseEnv(t *testing.T) {
	got := backstageDevBaseEnv([]string{
		"PATH=/usr/bin",
		"HOME=/home/u",
		"YARN_CACHE_FOLDER=/tmp/yarn",
		"npm_config_registry=https://registry.example",
		"AWS_SECRET_ACCESS_KEY=never",
		"GITHUB_TOKEN=never",
		"KUBECONFIG=/home/u/.kube/config",
	})
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/u", "YARN_CACHE_FOLDER=/tmp/yarn", "npm_config_registry=https://registry.example"} {
		if !slices.Contains(got, want) {
			t.Errorf("env is missing %s: %v", want, got)
		}
	}
	for _, kv := range got {
		if strings.Contains(kv, "never") || strings.HasPrefix(kv, "KUBECONFIG=") {
			t.Errorf("env leaks %s", kv)
		}
	}
}

// TestBackstageDevTag gives every build its own tag, so kind's containerd
// never keeps running an older build under a reused one.
func TestBackstageDevTag(t *testing.T) {
	now := time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)
	tag := backstageDevTag(t.TempDir(), now)
	if want := "backstage-dev:nogit-20261009140000"; tag != want {
		t.Errorf("tag outside git = %q, want %q", tag, want)
	}
	if err := config.ValidateImageRef(tag); err != nil {
		t.Errorf("tag %q is not a valid devImages ref: %v", tag, err)
	}
	// Only inside a git checkout: a source export of agentlab has no commit
	// to name, and "nogit" is then the right tag.
	if _, err := gitOutputIn(".", "rev-parse", "--short", "HEAD"); err == nil {
		if strings.Contains(backstageDevTag(".", now), "nogit") {
			t.Errorf("tag inside the agentlab checkout names no commit")
		}
	}
}

// TestBackstageImageBuild runs Podman's build unlabelled, where SELinux would
// hide the Dockerfile's bind-mounted bundles from its non-root user.
func TestBackstageImageBuild(t *testing.T) {
	if got := strings.Join(backstageImageBuild("t", false), " "); strings.Contains(got, "label=disable") {
		t.Errorf("docker build carries the podman flag: %s", got)
	}
	if got := strings.Join(backstageImageBuild("t", true), " "); !strings.HasSuffix(got, "--security-opt label=disable") {
		t.Errorf("podman build = %s, want it unlabelled", got)
	}
}
