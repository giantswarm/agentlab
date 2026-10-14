package lab

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

const gitHubAppTestKeySource = "op://lab/skills-app/private-key"

func gitHubAppTestConfig() *config.Config {
	cfg := gitHubTokenTestConfig()
	cfg.GitHubApp = config.GitHubApp{AppID: "123456", InstallationID: "7890", PrivateKeySource: gitHubAppTestKeySource}
	return cfg
}

// TestGitHubAppWiresAgentManager: a recorded App names the Secret in
// agent-manager's skills.github.app and drops its tokenSecret (the chart
// refuses both), while the portal keeps the token $GITHUB_TOKEN gives it;
// the reference itself appears in nothing the render writes.
func TestGitHubAppWiresAgentManager(t *testing.T) {
	t.Setenv(GitHubTokenEnv, gitHubTestToken)
	s := renderGitHubTokenSurfaces(t, gitHubAppTestConfig())
	if got := dig(s.values, agentManagerValuesKey, "skills", "github", "app", "secretName"); got != gitHubAppSecret {
		t.Errorf("agent-manager.skills.github.app.secretName = %v, want %s", got, gitHubAppSecret)
	}
	if got := dig(s.values, agentManagerValuesKey, "skills", "github", "tokenSecret"); got != nil {
		t.Errorf("agent-manager.skills.github.tokenSecret = %v next to the App, want nothing", got)
	}
	if got := dig(s.values, componentBackstage, componentBackstage, "extraEnvVarsSecrets"); !reflect.DeepEqual(got, []any{portalAnthropicSecret, gitHubTokenSecret}) {
		t.Errorf("backstage.backstage.extraEnvVarsSecrets = %v, want the portal's key and token [%s %s]", got, portalAnthropicSecret, gitHubTokenSecret)
	}
	for name, text := range s.raw() {
		if strings.Contains(text, gitHubAppTestKeySource) {
			t.Errorf("%s carries the private key reference:\n%s", name, text)
		}
	}
}

// TestGitHubAppUnsetLeavesTheLabAsItWas: without an App nothing names its
// Secret.
func TestGitHubAppUnsetLeavesTheLabAsItWas(t *testing.T) {
	t.Setenv(GitHubTokenEnv, "")
	for name, text := range renderGitHubTokenSurfaces(t, gitHubTokenTestConfig()).raw() {
		if strings.Contains(text, gitHubAppSecret) {
			t.Errorf("%s names %s without githubApp:\n%s", name, gitHubAppSecret, text)
		}
	}
}

// TestGitHubAppLegacyChartTakesNoKeys: the 3.x line has no skills App key.
func TestGitHubAppLegacyChartTakesNoKeys(t *testing.T) {
	t.Setenv(GitHubTokenEnv, "")
	cfg := gitHubAppTestConfig()
	cfg.Platform.ChartVersion = legacyChartVersion
	for name, text := range renderGitHubTokenSurfaces(t, cfg).raw() {
		if strings.Contains(text, gitHubAppSecret) {
			t.Errorf("%s wires the App on the 3.x line:\n%s", name, text)
		}
	}
}

// TestEnsureGitHubAppSecret: nothing without an App; with one agentlab
// applies the public ids and hands the private key to the secret tooling,
// never writing it itself; a failed placement fails the run.
func TestEnsureGitHubAppSecret(t *testing.T) {
	newFakeLab(t)
	ctx := context.Background()

	calls := fakeSecretTool(t, func() error { return nil })
	if err := ensureGitHubAppSecret(ctx, gitHubTokenTestConfig()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Errorf("secret tooling called without an App: %q", *calls)
	}
	if exists, _ := objectExists(ctx, gvrSecrets, platformNamespace, gitHubAppSecret); exists {
		t.Errorf("%s/%s written without an App", platformNamespace, gitHubAppSecret)
	}

	cfg := gitHubAppTestConfig()
	if err := ensureGitHubAppSecret(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	want := [][]string{secretCopyArgs(gitHubAppTestKeySource, secretTarget(cfg, platformNamespace, gitHubAppSecret, gitHubAppSecretPrivateKey))}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("secret tooling calls = %q, want %q", *calls, want)
	}
	for key, value := range map[string]string{gitHubAppSecretAppID: "123456", gitHubAppSecretInstallationID: "7890"} {
		got, err := secretDataKey(ctx, platformNamespace, gitHubAppSecret, key)
		if err != nil || string(got) != value {
			t.Errorf("%s/%s key %s = %q (err %v), want %q", platformNamespace, gitHubAppSecret, key, got, err, value)
		}
	}
	if secretHasKey(platformNamespace, gitHubAppSecret, gitHubAppSecretPrivateKey) {
		t.Errorf("agentlab wrote %s itself; the key is the tooling's to place", gitHubAppSecretPrivateKey)
	}

	fakeSecretTool(t, nil)
	if err := ensureGitHubAppSecret(ctx, cfg); err == nil || !strings.Contains(err.Error(), "githubApp.privateKeySource") {
		t.Errorf("err = %v, want the failed placement naming githubApp.privateKeySource", err)
	}
}
