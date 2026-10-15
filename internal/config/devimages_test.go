package config

import (
	"strings"
	"testing"
)

// TestDevImagesKnowClusterManagerAndAgentgateway: the dev-image loop swaps
// the cluster-manager build in — and that entry is the lab's switch for the
// component the chart ships off — and the agentgateway proxy and controller
// each under their Deployment's name; the refusal of an unknown target names
// all of them. The proxy travels as registry, repository and tag, so a
// digest ref is refused for it alone.
func TestDevImagesKnowClusterManagerAndAgentgateway(t *testing.T) {
	cfg := Default()
	if cfg.ClusterManagerEnabled() {
		t.Fatal("cluster-manager is off without its dev image")
	}
	cfg.Platform.DevImages = map[string]string{
		DevImageClusterManager:         "cluster-manager:dev",
		DevImageAgentgateway:           "agentgateway:dev",
		DevImageAgentgatewayController: "agentgateway-controller:dev",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cluster-manager, agentgateway and agentgateway-controller must be devImages targets: %v", err)
	}
	if !cfg.ClusterManagerEnabled() {
		t.Fatal("the cluster-manager dev image turns the component on")
	}
	cfg.Platform.Enabled = false
	if cfg.ClusterManagerEnabled() {
		t.Fatal("want disabled with the platform off")
	}
	cfg.Platform.Enabled = true

	cfg.Platform.DevImages = map[string]string{"agentgateway-proxy": "agentgateway:dev"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("an unknown target must be refused")
	}
	for _, want := range []string{DevImageClusterManager, DevImageAgentgateway, DevImageAgentgatewayController} {
		if !strings.Contains(err.Error(), ", "+want+",") && !strings.Contains(err.Error(), ", "+want+")") {
			t.Errorf("the refusal must list %s among the targets, got %v", want, err)
		}
	}

	digest := "agentgateway@sha256:" + strings.Repeat("a", 64)
	cfg.Platform.DevImages = map[string]string{DevImageAgentgateway: digest}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "proxy.image") || !strings.Contains(err.Error(), "name a tag") {
		t.Errorf("a digest ref for the proxy must be refused naming the chart's values, got %v", err)
	}
	cfg.Platform.DevImages = map[string]string{DevImageAgentgatewayController: "agentgateway-controller@sha256:" + strings.Repeat("a", 64)}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the controller is a Deployment target and takes a digest: %v", err)
	}
}

// TestDevImagesKnowWorkspaceManager: the dev-image loop swaps the
// workspace-manager build in while the workspaces are on — the component's
// one switch — and refuses the target while they are off.
func TestDevImagesKnowWorkspaceManager(t *testing.T) {
	cfg := Default()
	cfg.Platform.DevImages = map[string]string{DevImageWorkspaceManager: "workspace-manager:dev"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "platform.workspaces.enabled") {
		t.Fatalf("without the workspaces the target must be refused naming the key, got %v", err)
	}
	cfg.Platform.Workspaces.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("workspace-manager must be a devImages target with the workspaces on: %v", err)
	}
}

// TestDevImagesKnowSubstrateAPI: Substrate's API server is a devImages
// target while the agents, which bring Substrate, are on, and refused
// without them.
func TestDevImagesKnowSubstrateAPI(t *testing.T) {
	cfg := Default()
	cfg.Platform.DevImages = map[string]string{DevImageSubstrateAPI: "ateapi:dev"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ate-api-server must be a devImages target with the agents on: %v", err)
	}
	cfg.Platform.Agents = false
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "platform.agents") {
		t.Errorf("without the agents the target must be refused naming the key, got %v", err)
	}
}

// A coding Harness's image is a devImages target while the lab renders that
// Harness, and a registry target, not a Deployment's.
func TestDevImagesKnowCodingHarnesses(t *testing.T) {
	cfg := Default()
	cfg.Platform.Coding.Enabled = true
	cfg.Platform.Coding.Harnesses = []string{CodingRuntimeClaude, CodingRuntimeCodex}
	cfg.Platform.DevImages = map[string]string{DevImageClaudeHarness: "claude-harness:dev", DevImageCodexHarness: "codex-harness:dev"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the coding Harnesses' images must be devImages targets: %v", err)
	}
	for _, target := range []string{DevImageHarness, DevImageClaudeHarness, DevImageCodexHarness} {
		if !RegistryDevImage(target) {
			t.Errorf("%s is not a registry target", target)
		}
	}
	if RegistryDevImage("muster") {
		t.Error("muster is a registry target")
	}
	cfg.Platform.Coding.Harnesses = nil
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), DevImageCodexHarness) {
		t.Errorf("codex-harness without the codex Harness must be refused naming the key, got %v", err)
	}
}

// platform.coding.harnesses takes the known runtimes once each.
func TestCodingHarnessesValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		harnesses []string
		want      string
	}{
		"not a runtime": {[]string{"omp"}, "unknown runtime"},
		"twice":         {[]string{CodingRuntimeCodex, CodingRuntimeCodex}, "twice"},
		"codex alone":   {[]string{CodingRuntimeCodex}, ""},
		"both":          {[]string{CodingRuntimeClaude, CodingRuntimeCodex}, ""},
		"empty":         {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			cfg.Platform.Coding.Enabled = true
			cfg.Platform.Coding.Harnesses = tc.harnesses
			err := cfg.Validate()
			if tc.want == "" && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
	cfg := Default()
	cfg.Platform.Coding.Enabled = true
	if got := cfg.CodingHarnesses(); len(got) != 1 || got[0] != CodingRuntimeClaude {
		t.Errorf("CodingHarnesses() = %v, want claude by default", got)
	}
}
