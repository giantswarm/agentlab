package lab

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// gitHubAppSecret is the Secret of the skills GitHub App in agent-platform/,
// in the shape agent-manager's chart reads (skills.github.app.secretName):
// the public ids, which agentlab applies, and the private key, placed from
// githubApp.privateKeySource (secretsource.go). Server-side apply owns only
// the id keys, so the placed key survives every later apply.
const (
	gitHubAppSecret               = "agentlab-github-app" // #nosec G101 -- Secret NAME, not a credential
	gitHubAppSecretAppID          = "app-id"
	gitHubAppSecretInstallationID = "installation-id"
	gitHubAppSecretPrivateKey     = "private-key"
)

// portalGitHubAppSecret is the skills GitHub App as the portal reads it: its
// keys named after the env vars Backstage's envFrom yields
// (backstage.backstage.extraEnvVarsSecrets), which the overlay's
// integrations.github[].apps entry references. agentlab applies the public
// ids and places the private key and the client secret from their sources.
const (
	portalGitHubAppSecret          = "agentlab-portal-github-app" // #nosec G101 -- Secret NAME, not a credential
	portalGitHubAppIDKey           = "GITHUB_APP_ID"
	portalGitHubAppClientIDKey     = "GITHUB_APP_CLIENT_ID"
	portalGitHubAppPrivateKeyKey   = "GITHUB_APP_PRIVATE_KEY"   // #nosec G101 -- env var NAME, not a credential
	portalGitHubAppClientSecretKey = "GITHUB_APP_CLIENT_SECRET" // #nosec G101 -- env var NAME, not a credential
)

// gitHubAppWired is the render's answer: the App is recorded and the chart
// line takes the key (the 4.x agent-manager chart's skills.github.app).
func gitHubAppWired(cfg *config.Config) bool {
	return cfg.SkillsGitHubApp().Configured() && !cfg.LegacyChart()
}

// portalGitHubAppWired: the App is wired, it carries its OAuth client, and
// the portal runs, so the portal's skill discovery calls GitHub as the App.
func portalGitHubAppWired(cfg *config.Config) bool {
	return gitHubAppWired(cfg) && cfg.SkillsGitHubApp().HasClient() && cfg.Backstage.Enabled
}

// ensureGitHubAppSecret is the deploy-time half, run before the install: the
// values name the Secret whenever the App is recorded, so a placement that
// fails fails the run and a lab never deploys agent-manager without it.
// Without an App nothing is written.
func ensureGitHubAppSecret(_ context.Context, cfg *config.Config) error {
	if !gitHubAppWired(cfg) {
		return nil
	}
	app := cfg.SkillsGitHubApp()
	if err := ensureSecret(platformNamespace, gitHubAppSecret, corev1.SecretTypeOpaque, map[string][]byte{
		gitHubAppSecretAppID:          []byte(app.AppID),
		gitHubAppSecretInstallationID: []byte(app.InstallationID),
	}); err != nil {
		return err
	}
	if _, err := ensureSourcedKey(cfg, sourcedSecretKey{ns: platformNamespace, name: gitHubAppSecret, key: gitHubAppSecretPrivateKey,
		field: "githubApp.privateKeySource", source: app.PrivateKeySource, consumer: "agent-manager"}); err != nil {
		return err
	}
	note("secret %s/%s: App %s, installation %s, private key from %s — agent-manager resolves skills as the App", platformNamespace, gitHubAppSecret, app.AppID, app.InstallationID, app.PrivateKeySource)
	return ensurePortalGitHubAppSecret(cfg)
}

// ensurePortalGitHubAppSecret writes the portal's copy of the App when the
// render wires it: the public ids applied, the private key and the client
// secret placed from their sources.
func ensurePortalGitHubAppSecret(cfg *config.Config) error {
	if !portalGitHubAppWired(cfg) {
		return nil
	}
	app := cfg.SkillsGitHubApp()
	if err := ensureSecret(platformNamespace, portalGitHubAppSecret, corev1.SecretTypeOpaque, map[string][]byte{
		portalGitHubAppIDKey:       []byte(app.AppID),
		portalGitHubAppClientIDKey: []byte(app.ClientID),
	}); err != nil {
		return err
	}
	for _, v := range []struct{ field, source, key string }{
		{"githubApp.privateKeySource", app.PrivateKeySource, portalGitHubAppPrivateKeyKey},
		{"githubApp.clientSecretSource", app.ClientSecretSource, portalGitHubAppClientSecretKey},
	} {
		if _, err := ensureSourcedKey(cfg, sourcedSecretKey{ns: platformNamespace, name: portalGitHubAppSecret, key: v.key,
			field: v.field, source: v.source, consumer: "the portal"}); err != nil {
			return err
		}
	}
	note("the portal's skill discovery calls GitHub as App %s (client %s)", app.AppID, app.ClientID)
	return nil
}
