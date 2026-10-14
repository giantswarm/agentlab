package config

import (
	"errors"
	"fmt"
)

// GitHubApp is the read-only skills GitHub App agent-manager resolves skills
// with: installation tokens at the App's rate limit instead of a static
// token or GitHub's anonymous window. The App id and installation id are
// public; the private key is never this file: PrivateKeySource names where
// the operator's secret tooling finds it, and `agentlab up` and `platform`
// hand the reference to that tooling, which writes the key straight into the
// lab's Secret agentlab-github-app (internal/lab/githubapp.go).
type GitHubApp struct {
	// AppID is the App's numeric id.
	AppID string `yaml:"appId,omitempty"`
	// InstallationID is the numeric id of the App's installation whose
	// repositories the skills come from.
	InstallationID string `yaml:"installationId,omitempty"`
	// PrivateKeySource is a reference `beekeeper secret copy` resolves to the
	// App's private key (PEM), in the form AIKey.Source takes.
	PrivateKeySource string `yaml:"privateKeySource,omitempty"`
	// ClientID and ClientSecretSource are the App's OAuth client: the
	// portal's GitHub integration takes an App only with both, so the
	// portal's skill discovery calls GitHub as the App when they are set.
	// The client id is public; ClientSecretSource is a reference in the form
	// PrivateKeySource takes.
	ClientID           string `yaml:"clientId,omitempty"`
	ClientSecretSource string `yaml:"clientSecretSource,omitempty"`
}

// SkillsGitHubApp is the App agent-manager and the portal resolve skills
// with: githubApp,
// its appId and privateKeySource those of the workspaces GitHub App
// (platform.workspaces.github) when githubApp leaves both empty, so a lab
// whose workspaces App is installed records only the installation id; the
// workspaces App's OAuth client, when it records both halves, comes with it
// unless githubApp names one. Zero
// when githubApp records nothing.
func (c *Config) SkillsGitHubApp() GitHubApp {
	app := c.GitHubApp
	if app.Configured() && app.AppID == "" && app.PrivateKeySource == "" {
		ws := c.Platform.Workspaces.GitHub
		app.AppID, app.PrivateKeySource = ws.AppID, ws.PrivateKeySource
		if app.ClientID == "" && app.ClientSecretSource == "" && ws.ClientID != "" && ws.ClientSecretSource != "" {
			app.ClientID, app.ClientSecretSource = ws.ClientID, ws.ClientSecretSource
		}
	}
	return app
}

// HasClient reports whether the App's OAuth client is recorded, which the
// portal's GitHub integration needs besides the App id and key.
func (a GitHubApp) HasClient() bool { return a.ClientID != "" && a.ClientSecretSource != "" }

// Configured reports whether any part of the App is recorded; Validate
// refuses a partial one.
func (a GitHubApp) Configured() bool {
	return a.AppID != "" || a.InstallationID != "" || a.PrivateKeySource != ""
}

// Validate refuses a partial App, an id that is not a number and a private
// key that is not a reference.
func (a GitHubApp) Validate() error {
	if !a.Configured() {
		return nil
	}
	if a.AppID == "" || a.InstallationID == "" || a.PrivateKeySource == "" {
		return errors.New("appId, installationId and privateKeySource are set together or not at all; installationId alone takes appId and privateKeySource from platform.workspaces.github")
	}
	for name, id := range map[string]string{"appId": a.AppID, "installationId": a.InstallationID} {
		if !isDigits(id) {
			return fmt.Errorf("%s %q is not a numeric GitHub id", name, id)
		}
	}
	if err := ValidateSecretSource(a.PrivateKeySource); err != nil {
		return fmt.Errorf("privateKeySource %q: %w", a.PrivateKeySource, err)
	}
	if (a.ClientID == "") != (a.ClientSecretSource == "") {
		return errors.New("clientId and clientSecretSource are set together or not at all")
	}
	if a.ClientSecretSource != "" {
		if err := ValidateSecretSource(a.ClientSecretSource); err != nil {
			return fmt.Errorf("clientSecretSource %q: %w", a.ClientSecretSource, err)
		}
	}
	return nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
