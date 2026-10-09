package config

import (
	"strings"
	"testing"
)

// The workspaces switch: it needs the agents runtime (Substrate's signers
// certify the driver's endpoint), and the CSI hostpath driver serves one
// node, so at most one reserved substrate worker.
func TestWorkspacesSwitch(t *testing.T) {
	cfg := Default()
	if cfg.WorkspacesEnabled() {
		t.Fatal("workspaces on by default, want off")
	}
	cfg.Platform.Workspaces.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("workspaces on with agents: %v", err)
	}
	if !cfg.WorkspacesEnabled() {
		t.Fatal("WorkspacesEnabled false with the platform, the agents and the key on")
	}
	cfg.SubstrateNodes = 1
	if err := cfg.Validate(); err != nil {
		t.Errorf("workspaces with one substrate node: %v, want accepted (the driver is pinned to it)", err)
	}
	cfg.SubstrateNodes = 2
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "substrateNodes 0 or 1") {
		t.Errorf("workspaces with two substrate nodes: err = %v, want the one-node limit", err)
	}

	cfg = Default()
	cfg.Platform.Workspaces.Enabled = true
	cfg.Platform.Agents = false
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "platform.workspaces requires platform.agents") {
		t.Errorf("workspaces without agents: err = %v, want the agents requirement", err)
	}
	if cfg.WorkspacesEnabled() {
		t.Error("WorkspacesEnabled true without the agents runtime")
	}

	cfg = Default()
	cfg.Platform.Workspaces.Enabled = true
	cfg.Platform.Enabled = false
	cfg.Backstage.Enabled = false
	if err := cfg.Validate(); err != nil {
		t.Errorf("workspaces with the platform off: %v, want ignored", err)
	}
	if cfg.WorkspacesEnabled() {
		t.Error("WorkspacesEnabled true with the platform off")
	}
}
