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
