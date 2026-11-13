package config

import "fmt"

func (c *Config) validateManagerGitHubProof() error {
	if !c.Platform.ManagerGitHubProof {
		return nil
	}
	if !c.Platform.Enabled || !c.Platform.Agents || c.LegacyChart() {
		return fmt.Errorf("platform.managerGitHubProof requires the platform and agents on the current chart line")
	}
	github := c.Platform.Workspaces.GitHub
	if !c.WorkspaceGitHubProvider() || github.AppID == "" || github.ClientID == "" {
		return fmt.Errorf("platform.managerGitHubProof requires platform.workspaces.github appId and clientId")
	}
	if !c.WorkspaceProviderApps() && !c.DevctlEnabled() {
		return fmt.Errorf("platform.managerGitHubProof requires a github provider App: enable platform.devctl or workspaces grantProvider providerApps")
	}
	if len(github.ProofRepositories) != 2 {
		return fmt.Errorf("platform.managerGitHubProof requires the workspace GitHub App's two proofRepositories")
	}
	app := c.SkillsGitHubApp()
	if !app.Configured() || app.AppID != github.AppID {
		return fmt.Errorf("platform.managerGitHubProof requires githubApp to use the workspace GitHub App and its proof-repository installation")
	}
	return app.Validate()
}
