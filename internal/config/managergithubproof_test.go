package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestManagerGitHubProofPrerequisites(t *testing.T) {
	configured := func() *Config {
		configuration := Default()
		configuration.Platform.ManagerGitHubProof = true
		configuration.Platform.Enabled = true
		configuration.Platform.Agents = true
		configuration.Platform.Workspaces.Enabled = true
		configuration.Platform.Workspaces.Provider = WorkspaceProviderGitHub
		configuration.Platform.Devctl.Enabled = true
		configuration.Platform.Workspaces.GitHub = WorkspaceGitHub{AppID: "123", ClientID: "client", PrivateKeySource: "op://lab/github/private-key", ProofRepositories: []string{"example/proof", "example/private-skills"}}
		configuration.GitHubApp.InstallationID = "456"
		return configuration
	}
	require.NoError(t, configured().validateManagerGitHubProof())
	for name, change := range map[string]func(*Config){
		"platform off":          func(c *Config) { c.Platform.Enabled = false },
		"agents off":            func(c *Config) { c.Platform.Agents = false },
		"no app":                func(c *Config) { c.Platform.Workspaces.GitHub.AppID = "" },
		"no client":             func(c *Config) { c.Platform.Workspaces.GitHub.ClientID = "" },
		"no provider App":       func(c *Config) { c.Platform.Devctl.Enabled = false },
		"no installation":       func(c *Config) { c.GitHubApp.InstallationID = "" },
		"no proof repositories": func(c *Config) { c.Platform.Workspaces.GitHub.ProofRepositories = nil },
		"different App": func(c *Config) {
			c.GitHubApp = GitHubApp{AppID: "789", InstallationID: "456", PrivateKeySource: "op://lab/other/private-key"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			configuration := configured()
			change(configuration)
			require.Error(t, configuration.validateManagerGitHubProof())
			configuration.Platform.ManagerGitHubProof = false
			require.NoError(t, configuration.validateManagerGitHubProof())
		})
	}
}
