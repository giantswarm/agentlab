package config

// GitHubToken is where the lab's token for GitHub comes from: never this file.
// Source names where the token lives; `agentlab up` and `platform` read it
// from there into the lab's Secret agentlab-github-token within their own
// process (internal/lab/secretsource.go), so the portal's skill discovery
// and agent-manager's skill resolution call GitHub authenticated after every
// recreate and the value never reaches an argument, a file or the output.
type GitHubToken struct {
	// Source is a reference in the form AIKey.Source takes. Empty means no token: GitHub is called as the
	// skills GitHub App when one is recorded, else unauthenticated.
	Source string `yaml:"source,omitempty"`
}

// Validate refuses a source that is not a reference, as AIKey.Validate does.
func (g GitHubToken) Validate() error {
	if g.Source == "" {
		return nil
	}
	return ValidateSecretSource(g.Source)
}
