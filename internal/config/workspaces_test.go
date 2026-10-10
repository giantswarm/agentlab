package config

import (
	"strings"
	"testing"
)

// The workspaces switch: it needs the agents runtime (Substrate's signers
// certify the driver's endpoint); the NFS driver's node plugin runs on every
// node, so any number of reserved substrate workers is accepted.
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
	cfg.SubstrateNodes = 2
	if err := cfg.Validate(); err != nil {
		t.Errorf("workspaces with two substrate nodes: %v, want accepted (the node plugin runs on every node)", err)
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

// The provider is github or nothing, and counts only with workspaces on.
func TestWorkspacesProvider(t *testing.T) {
	cfg := Default()
	cfg.Platform.Workspaces.Provider = WorkspaceProviderGitHub
	if cfg.WorkspaceGitHubProvider() {
		t.Error("WorkspaceGitHubProvider true with workspaces off")
	}
	cfg.Platform.Workspaces.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("provider github: %v", err)
	}
	if !cfg.WorkspaceGitHubProvider() {
		t.Error("WorkspaceGitHubProvider false with workspaces on and provider github")
	}
	if got, want := cfg.WorkspaceManagerSignInURL(), "https://workspace-manager.127.0.0.1.nip.io/signin"; got != want {
		t.Errorf("sign-in URL %s, want %s (port-free on 443)", got, want)
	}
	cfg.Platform.Workspaces.Provider = "gitlab"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "platform.workspaces.provider") {
		t.Errorf("provider gitlab: err = %v, want refused", err)
	}
}
