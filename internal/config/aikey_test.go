package config

import (
	"strings"
	"testing"
)

// TestAIKeySourceIsAReference: aiKey.source takes an op:// field or a SOPS
// path and refuses a value — a key pasted into agentlab.yaml must never pass
// configure; empty is the no-source default and the file carries no aiKey
// then.
func TestAIKeySourceIsAReference(t *testing.T) {
	cfg := Default()
	if cfg.AIKey != (AIKey{}) {
		t.Fatalf("no source by default: %+v", cfg.AIKey)
	}
	for _, ok := range []string{"", "op://Employee/agentlab.anthropic-key/credential", "lab.sops.yaml#stringData.ANTHROPIC_API_KEY", "sops://secrets/lab.sops.yaml#data.key", "/abs/path/to/file.sops.yaml#a.b.c"} {
		cfg.AIKey.Source = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"sk-ant-api03-abcdef", "op://vault/item", "op://vault//field", "op://vault/item/field/extra", "file.sops.yaml", "#path", "file#", "op://vault/it em/field", "a value with spaces"} {
		cfg.AIKey.Source = bad
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "aiKey.source") {
			t.Errorf("%q must be refused naming aiKey.source, got %v", bad, err)
		}
	}
}

// TestGitHubTokenSourceIsAReference: githubToken.source takes the references
// aiKey.source does and refuses a pasted value.
func TestGitHubTokenSourceIsAReference(t *testing.T) {
	cfg := Default()
	cfg.GitHubToken.Source = "op://lab/github-token/credential"
	if err := cfg.Validate(); err != nil {
		t.Errorf("an op:// reference refused: %v", err)
	}
	cfg.GitHubToken.Source = "ghp_pasted_value"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "githubToken.source") {
		t.Errorf("a pasted value must be refused naming githubToken.source, got %v", err)
	}
}

// TestGitHubAppValidate: a complete App with numeric ids and a key reference
// is taken; a partial one, a non-numeric id, a pasted key and an App next to
// githubToken.source are refused naming githubApp.
func TestGitHubAppValidate(t *testing.T) {
	good := GitHubApp{AppID: "123456", InstallationID: "7890", PrivateKeySource: "op://lab/skills-app/private-key"}
	cfg := Default()
	cfg.GitHubApp = good
	if err := cfg.Validate(); err != nil {
		t.Errorf("a complete App refused: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"partial":     func(c *Config) { c.GitHubApp.InstallationID = "" },
		"non-numeric": func(c *Config) { c.GitHubApp.AppID = "skills-app" },
		"pasted key":  func(c *Config) { c.GitHubApp.PrivateKeySource = "pasted value" },
		"with token":  func(c *Config) { c.GitHubToken.Source = "op://lab/github-token/credential" },
	} {
		cfg := Default()
		cfg.GitHubApp = good
		mutate(cfg)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "githubApp") {
			t.Errorf("%s: want a refusal naming githubApp, got %v", name, err)
		}
	}
}

// TestSkillsGitHubAppFromWorkspaces: an installation id alone takes the
// workspaces GitHub App's id and private key source; a githubApp that names
// its own App keeps it, and without an installation id there is no skills
// App and no refusal.
func TestSkillsGitHubAppFromWorkspaces(t *testing.T) {
	workspaces := WorkspaceGitHub{AppID: "5288072", ClientID: "Iv23liclient", PrivateKeySource: "/lab/agents.sops.yaml#stringData.private-key"}

	cfg := Default()
	cfg.Platform.Workspaces.GitHub = workspaces
	if got := cfg.SkillsGitHubApp(); got.Configured() {
		t.Errorf("no installation id: skills App = %+v, want none", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a workspaces App without a skills installation refused: %v", err)
	}

	cfg.GitHubApp.InstallationID = "171259128"
	want := GitHubApp{AppID: workspaces.AppID, InstallationID: "171259128", PrivateKeySource: workspaces.PrivateKeySource}
	if got := cfg.SkillsGitHubApp(); got != want {
		t.Errorf("skills App = %+v, want the workspaces App's %+v", got, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("an installation id on the workspaces App refused: %v", err)
	}

	own := GitHubApp{AppID: "123456", InstallationID: "7890", PrivateKeySource: "op://lab/skills-app/private-key"}
	cfg.GitHubApp = own
	if got := cfg.SkillsGitHubApp(); got != own {
		t.Errorf("skills App = %+v, want its own %+v", got, own)
	}

	cfg = Default()
	cfg.GitHubApp.InstallationID = "171259128"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "platform.workspaces.github") {
		t.Errorf("an installation id without any App: want a refusal naming platform.workspaces.github, got %v", err)
	}
}
