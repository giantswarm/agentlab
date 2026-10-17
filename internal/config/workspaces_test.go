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
		{"gitlab", []string{WorkspaceProviderGitLab}},
		{"gitlab,fake,github", []string{WorkspaceProviderFake, WorkspaceProviderGitHub, WorkspaceProviderGitLab}},
		{"fake-bearer,fake", []string{WorkspaceProviderFake, WorkspaceProviderFakeBearer}},
	} {
		cfg.Platform.Workspaces.Provider = tc.provider
		if err := cfg.Validate(); err != nil {
			t.Fatalf("provider %q: %v", tc.provider, err)
		}
		if got := cfg.WorkspaceProviders(); !slices.Equal(got, tc.want) {
			t.Errorf("provider %q: instances %v, want %v", tc.provider, got, tc.want)
		}
		if cfg.WorkspaceFakeProvider() != slices.Contains(tc.want, WorkspaceProviderFake) || cfg.WorkspaceGitHubProvider() != slices.Contains(tc.want, WorkspaceProviderGitHub) ||
			cfg.WorkspaceGitLabProvider() != slices.Contains(tc.want, WorkspaceProviderGitLab) {
			t.Errorf("provider %q: fake %v, github %v, gitlab %v, want %v", tc.provider, cfg.WorkspaceFakeProvider(), cfg.WorkspaceGitHubProvider(), cfg.WorkspaceGitLabProvider(), tc.want)
		}
		if cfg.WorkspaceFakeBearerProvider() != slices.Contains(tc.want, WorkspaceProviderFakeBearer) {
			t.Errorf("provider %q: fake-bearer %v, want %v", tc.provider, cfg.WorkspaceFakeBearerProvider(), tc.want)
		}
	}
	for _, provider := range []string{"gitea", "fake,gitea", "fake,fake", "github,", "fake-bearer", "github,fake-bearer"} {
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

// The gitlab instance's settings: an https URL of a host, sources that are
// references, never values.
func TestWorkspaceGitLabValidate(t *testing.T) {
	ok := WorkspaceGitLab{ // #nosec G101 -- references and ids, no credential
		URL: "https://gitlab.example.com", ClientID: "abc", Group: "workspaces", Project: "proof", Person: "someone",
		TokenSource: "secrets.sops.yaml#stringData.token", ClientSecretSource: "op://Lab/gitlab/client-secret", PasswordSource: "p.sops.yaml#stringData.password",
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a complete instance: %v", err)
	}
	for name, mutate := range map[string]func(*WorkspaceGitLab){
		"an http URL":            func(g *WorkspaceGitLab) { g.URL = "http://gitlab.example.com" },
		"a URL with a path":      func(g *WorkspaceGitLab) { g.URL = "https://gitlab.example.com/gitlab" },
		"a URL without a host":   func(g *WorkspaceGitLab) { g.URL = "https://" },
		"a token value":          func(g *WorkspaceGitLab) { g.TokenSource = "glpat-abcdef" },
		"a client secret value":  func(g *WorkspaceGitLab) { g.ClientSecretSource = "gloas-abc" },
		"a password with spaces": func(g *WorkspaceGitLab) { g.PasswordSource = "my password" },
	} {
		g := ok
		mutate(&g)
		if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "platform.workspaces.gitlab.") {
			t.Errorf("%s: err = %v, want refused naming the key", name, err)
		}
	}
	cfg := Default()
	cfg.Platform.Workspaces.Enabled = true
	cfg.Platform.Workspaces.Provider = "fake,gitlab"
	cfg.Platform.Workspaces.GitLab = ok
	cfg.Platform.Workspaces.GitLab.URL = "ftp://gitlab.example.com"
	if err := cfg.Validate(); err == nil {
		t.Error("the configuration validates a gitlab instance with a bad URL")
	}
}

// The sessions' cleanup window: empty keeps the chart's, a positive Go
// duration is taken, anything else refused.
func TestWorkspacesSessionsCleanupAfter(t *testing.T) {
	for value, ok := range map[string]bool{"": true, "10m": true, "720h": true, "0s": false, "-1m": false, "ten minutes": false} {
		cfg := Default()
		cfg.Platform.Workspaces.Enabled = true
		cfg.Platform.Workspaces.SessionsCleanupAfter = value
		err := cfg.Validate()
		if ok && err != nil {
			t.Errorf("%q: %v", value, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "sessionsCleanupAfter")) {
			t.Errorf("%q: err = %v, want refused", value, err)
		}
	}
}

// The refresh threshold: empty keeps the manager's, a positive Go duration
// is taken, anything else refused.
func TestWorkspacesSyncRefreshAfter(t *testing.T) {
	for value, ok := range map[string]bool{"": true, "1m": true, "1h": true, "0s": false, "-1m": false, "a minute": false} {
		cfg := Default()
		cfg.Platform.Workspaces.Enabled = true
		cfg.Platform.Workspaces.SyncRefreshAfter = value
		err := cfg.Validate()
		if ok && err != nil {
			t.Errorf("%q: %v", value, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "syncRefreshAfter")) {
			t.Errorf("%q: err = %v, want refused", value, err)
		}
	}
}

// The instance muster pins the workspace-manager to: the first instance
// while grantProvider is empty, the named one otherwise; a name of no
// configured instance is refused.
func TestWorkspaceGrantProvider(t *testing.T) {
	for _, c := range []struct{ provider, grant, want string }{
		{"", "", WorkspaceProviderFake},
		{WorkspaceProviderFake + "," + WorkspaceProviderGitHub, "", WorkspaceProviderFake},
		{WorkspaceProviderGitHub + "," + WorkspaceProviderGitLab, "", WorkspaceProviderGitHub},
		{WorkspaceProviderFake + "," + WorkspaceProviderGitLab, WorkspaceProviderGitLab, WorkspaceProviderGitLab},
	} {
		cfg := Default()
		cfg.Platform.Workspaces.Enabled = true
		cfg.Platform.Workspaces.Provider, cfg.Platform.Workspaces.GrantProvider = c.provider, c.grant
		if err := cfg.Validate(); err != nil {
			t.Errorf("%q/%q: %v", c.provider, c.grant, err)
		}
		if got := cfg.WorkspaceGrantProvider(); got != c.want {
			t.Errorf("%q/%q: WorkspaceGrantProvider() = %q, want %q", c.provider, c.grant, got, c.want)
		}
	}
	cfg := Default()
	cfg.Platform.Workspaces.Enabled = true
	cfg.Platform.Workspaces.Provider, cfg.Platform.Workspaces.GrantProvider = WorkspaceProviderFake, WorkspaceProviderGitLab
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "grantProvider") {
		t.Errorf("a grantProvider of no configured instance: err = %v, want refused", err)
	}
	if got := Default().WorkspaceGrantProvider(); got != "" {
		t.Errorf("workspaces off: WorkspaceGrantProvider() = %q, want none", got)
	}
}
