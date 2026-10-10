package config

import (
	"slices"
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

// The provider instances: the fake alone by default, github, or both in
// the workspace-manager's order however they are written; anything else
// refused; and they count only with workspaces on.
func TestWorkspacesProvider(t *testing.T) {
	cfg := Default()
	cfg.Platform.Workspaces.Provider = WorkspaceProviderGitHub
	if cfg.WorkspaceGitHubProvider() || cfg.WorkspaceFakeProvider() || cfg.WorkspaceProviders() != nil {
		t.Error("a provider counts with workspaces off")
	}
	cfg.Platform.Workspaces.Enabled = true
	for _, tc := range []struct {
		provider string
		want     []string
	}{
		{"", []string{WorkspaceProviderFake}},
		{"fake", []string{WorkspaceProviderFake}},
		{"github", []string{WorkspaceProviderGitHub}},
		{"fake,github", []string{WorkspaceProviderFake, WorkspaceProviderGitHub}},
		{" github , fake ", []string{WorkspaceProviderFake, WorkspaceProviderGitHub}},
	} {
		cfg.Platform.Workspaces.Provider = tc.provider
		if err := cfg.Validate(); err != nil {
			t.Fatalf("provider %q: %v", tc.provider, err)
		}
		if got := cfg.WorkspaceProviders(); !slices.Equal(got, tc.want) {
			t.Errorf("provider %q: instances %v, want %v", tc.provider, got, tc.want)
		}
		if cfg.WorkspaceFakeProvider() != slices.Contains(tc.want, WorkspaceProviderFake) || cfg.WorkspaceGitHubProvider() != slices.Contains(tc.want, WorkspaceProviderGitHub) {
			t.Errorf("provider %q: fake %v, github %v, want %v", tc.provider, cfg.WorkspaceFakeProvider(), cfg.WorkspaceGitHubProvider(), tc.want)
		}
	}
	for _, provider := range []string{"gitlab", "fake,gitlab", "fake,fake", "github,"} {
		cfg.Platform.Workspaces.Provider = provider
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "platform.workspaces.provider") {
			t.Errorf("provider %q: err = %v, want refused", provider, err)
		}
		if cfg.WorkspaceProviders() != nil {
			t.Errorf("provider %q: instances %v, want none for a refused value", provider, cfg.WorkspaceProviders())
		}
	}
	cfg.Platform.Workspaces.Provider = ""
	if got, want := cfg.WorkspaceManagerSignInURL(), "https://workspace-manager.127.0.0.1.nip.io/signin"; got != want {
		t.Errorf("sign-in URL %s, want %s (port-free on 443)", got, want)
	}
	if got, want := cfg.WorkspaceCallbackURL(WorkspaceProviderFake), "https://workspace-manager.127.0.0.1.nip.io/callback/fake"; got != want {
		t.Errorf("the fake's callback URL %s, want %s", got, want)
	}
	if got, want := cfg.GitHubFakeURL(), "https://github.127.0.0.1.nip.io"; got != want {
		t.Errorf("the fake's URL %s, want %s", got, want)
	}
	cfg.Platform.GatewayPort = 8443
	if got, want := cfg.GitHubFakeURL(), "https://github.127.0.0.1.nip.io"; got != want {
		t.Errorf("the fake's URL %s, want %s: pods reach it on 443 whatever the gateway port", got, want)
	}
}
