package lab

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// gitHubAppSecret is the Secret of the skills GitHub App in agent-platform/,
// in the shape agent-manager's chart reads (skills.github.app.secretName):
// the public ids, which agentlab applies, and the private key, which the
// secret tooling places from githubApp.privateKeySource so agentlab never
// reads it. Server-side apply owns only the id keys, so the key the tooling
// patched survives every later apply.
const (
	gitHubAppSecret               = "agentlab-github-app" // #nosec G101 -- Secret NAME, not a credential
	gitHubAppSecretAppID          = "app-id"
	gitHubAppSecretInstallationID = "installation-id"
	gitHubAppSecretPrivateKey     = "private-key"
)

// gitHubAppWired is the render's answer: the App is recorded and the chart
// line takes the key (the 4.x agent-manager chart's skills.github.app).
func gitHubAppWired(cfg *config.Config) bool {
	return cfg.GitHubApp.Configured() && !cfg.LegacyChart()
}

// ensureGitHubAppSecret is the deploy-time half, run before the install: the
// values name the Secret whenever the App is recorded, so a placement that
// fails fails the run and a lab never deploys agent-manager without it.
// Without an App nothing is written.
func ensureGitHubAppSecret(_ context.Context, cfg *config.Config) error {
	if !gitHubAppWired(cfg) {
		return nil
	}
	app := cfg.GitHubApp
	if err := ensureSecret(platformNamespace, gitHubAppSecret, corev1.SecretTypeOpaque, map[string][]byte{
		gitHubAppSecretAppID:          []byte(app.AppID),
		gitHubAppSecretInstallationID: []byte(app.InstallationID),
	}); err != nil {
		return err
	}
	answer, err := runSecretTool(secretCopyArgs(app.PrivateKeySource, secretTarget(cfg, platformNamespace, gitHubAppSecret, gitHubAppSecretPrivateKey))...)
	if err != nil {
		return fmt.Errorf("secret %s/%s key %s from githubApp.privateKeySource %s: %w\n  agent-manager references it; when %s answers, re-run `agentlab platform`",
			platformNamespace, gitHubAppSecret, gitHubAppSecretPrivateKey, app.PrivateKeySource, err, secretTool)
	}
	note("secret %s/%s: App %s, installation %s, private key placed from %s by %s (%s); agentlab never read it — agent-manager resolves skills as the App", platformNamespace, gitHubAppSecret, app.AppID, app.InstallationID, app.PrivateKeySource, secretTool, strings.TrimSpace(answer))
	return nil
}
