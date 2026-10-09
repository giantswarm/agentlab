package config

// GitHubToken is where the lab's GitHub token comes from: never this file.
// Source names where the operator's secret tooling finds it; `agentlab up`
// and `platform` hand the reference to that tooling, which writes the value
// straight into the lab's Secret agentlab-github-token in its own process
// (internal/lab/githubtoken.go), so the portal's skill discovery and
// agent-manager's skill resolution call GitHub authenticated after every
// recreate and agentlab never reads a value.
type GitHubToken struct {
	// Source is a reference `beekeeper secret copy` resolves, in the form
	// AIKey.Source takes. Empty means the token comes from $GITHUB_TOKEN at
	// deploy time, else GitHub is called unauthenticated.
	Source string `yaml:"source,omitempty"`
}

// Validate refuses a source that is not a reference, as AIKey.Validate does.
func (g GitHubToken) Validate() error {
	if g.Source == "" {
		return nil
	}
	return ValidateSecretSource(g.Source)
}
