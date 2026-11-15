package lab

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

const gitHubAppTestKeySource = "op://lab/skills-app/private-key"

// gitHubAppTestClientID is the workspaces App's public client id in the tests.
const gitHubAppTestClientID = "Iv23liclient"

func gitHubAppTestConfig() *config.Config {
	cfg := gitHubTokenTestConfig()
	cfg.GitHubApp = config.GitHubApp{AppID: "123456", InstallationID: "7890", PrivateKeySource: gitHubAppTestKeySource}
	return cfg
}

// TestGitHubAppWiresAgentManager: a recorded App names the Secret in
// agent-manager's skills.github.app and drops its tokenSecret (the chart
// refuses both), while the portal keeps the token githubToken.source gives it;
// the reference itself appears in nothing the render writes.
func TestGitHubAppWiresAgentManager(t *testing.T) {
	cfg := gitHubAppTestConfig()
	cfg.GitHubToken.Source = gitHubTokenTestSource
	s := renderGitHubTokenSurfaces(t, cfg)
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
	for name, text := range renderGitHubTokenSurfaces(t, gitHubTokenTestConfig()).raw() {
		if strings.Contains(text, gitHubAppSecret) {
			t.Errorf("%s names %s without githubApp:\n%s", name, gitHubAppSecret, text)
		}
	}
}

// TestGitHubAppLegacyChartTakesNoKeys: the 3.x line has no skills App key.
func TestGitHubAppLegacyChartTakesNoKeys(t *testing.T) {
	cfg := gitHubAppTestConfig()
	cfg.Platform.ChartVersion = legacyChartVersion
	for name, text := range renderGitHubTokenSurfaces(t, cfg).raw() {
		if strings.Contains(text, gitHubAppSecret) {
			t.Errorf("%s wires the App on the 3.x line:\n%s", name, text)
		}
	}
}

// testPEM is what the fake vault answers for a private key: a test value.
const testPEM = "-----BEGIN RSA PRIVATE KEY-----\ntest\n-----END RSA PRIVATE KEY-----\n" // #nosec G101 -- a test value, not a credential

// TestEnsureGitHubAppSecret: nothing without an App; with one agentlab
// applies the public ids and places the private key from its source next
// to them; a failed placement fails the run.
func TestEnsureGitHubAppSecret(t *testing.T) {
	newFakeLab(t)
	ctx := context.Background()

	reads := fakeSecretSources(t, map[string]string{gitHubAppTestKeySource: testPEM})
	if err := ensureGitHubAppSecret(ctx, gitHubTokenTestConfig()); err != nil {
		t.Fatal(err)
	}
	if len(*reads) != 0 {
		t.Errorf("sources read without an App: %q", *reads)
	}
	if exists, _ := objectExists(ctx, gvrSecrets, platformNamespace, gitHubAppSecret); exists {
		t.Errorf("%s/%s written without an App", platformNamespace, gitHubAppSecret)
	}

	cfg := gitHubAppTestConfig()
	if err := ensureGitHubAppSecret(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if want := []string{gitHubAppTestKeySource}; !reflect.DeepEqual(*reads, want) {
		t.Errorf("sources read = %q, want %q", *reads, want)
	}
	for key, value := range map[string]string{gitHubAppSecretAppID: "123456", gitHubAppSecretInstallationID: "7890", gitHubAppSecretPrivateKey: testPEM} {
		got, err := secretDataKey(ctx, platformNamespace, gitHubAppSecret, key)
		if err != nil || string(got) != value {
			t.Errorf("%s/%s key %s = %q (err %v), want %q", platformNamespace, gitHubAppSecret, key, got, err, value)
		}
	}

	newFakeLab(t)
	fakeSecretSources(t, nil)
	if err := ensureGitHubAppSecret(ctx, cfg); err == nil || !strings.Contains(err.Error(), "githubApp.privateKeySource") || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("err = %v, want the failed placement naming githubApp.privateKeySource and the reader's answer", err)
	}
}

// TestGitHubAppFromWorkspacesWiresAgentManager: a lab recording the
// workspaces GitHub App and only the skills installation id wires
// agent-manager's skills.github.app, and the Secret carries the workspaces
// App's id while its private key source goes to the secret tooling.
func TestGitHubAppFromWorkspacesWiresAgentManager(t *testing.T) {
	const workspacesKeySource = "/lab/agents.sops.yaml#stringData.private-key"
	cfg := gitHubTokenTestConfig()
	cfg.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: "5288072", ClientID: gitHubAppTestClientID, PrivateKeySource: workspacesKeySource}
	cfg.GitHubApp.InstallationID = "171259128"

	s := renderGitHubTokenSurfaces(t, cfg)
	if got := dig(s.values, agentManagerValuesKey, "skills", "github", "app", "secretName"); got != gitHubAppSecret {
		t.Errorf("agent-manager.skills.github.app.secretName = %v, want %s", got, gitHubAppSecret)
	}
	for name, text := range s.raw() {
		if strings.Contains(text, workspacesKeySource) {
			t.Errorf("%s carries the private key reference:\n%s", name, text)
		}
	}

	newFakeLab(t)
	ctx := context.Background()
	reads := fakeSecretSources(t, map[string]string{workspacesKeySource: testPEM})
	if err := ensureGitHubAppSecret(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if want := []string{workspacesKeySource}; !reflect.DeepEqual(*reads, want) {
		t.Errorf("sources read = %q, want %q", *reads, want)
	}
	for key, value := range map[string]string{gitHubAppSecretAppID: "5288072", gitHubAppSecretInstallationID: "171259128", gitHubAppSecretPrivateKey: testPEM} {
		got, err := secretDataKey(ctx, platformNamespace, gitHubAppSecret, key)
		if err != nil || string(got) != value {
			t.Errorf("%s/%s key %s = %q (err %v), want %q", platformNamespace, gitHubAppSecret, key, got, err, value)
		}
	}
}

// TestGitHubAppWiresThePortal: the workspaces App with its OAuth client
// gives the portal the App too — its Secret in extraEnvVarsSecrets, the
// overlay's integrations.github apps entry referencing the env vars — and
// the placement applies the public ids and hands the private key and the
// client secret to the secret tooling. Without the client the portal stays
// as it was.
func TestGitHubAppWiresThePortal(t *testing.T) {
	const keySource, clientSecretSource = "/lab/agents.sops.yaml#stringData.private-key", "/lab/agents.sops.yaml#stringData.client-secret" // #nosec G101 -- references, not credentials
	cfg := gitHubTokenTestConfig()
	cfg.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: "5288072", ClientID: gitHubAppTestClientID, PrivateKeySource: keySource, ClientSecretSource: clientSecretSource}
	cfg.GitHubApp.InstallationID = "171259128"

	s := renderGitHubTokenSurfaces(t, cfg)
	if got := dig(s.values, componentBackstage, componentBackstage, "extraEnvVarsSecrets"); !reflect.DeepEqual(got, []any{portalAnthropicSecret, portalGitHubAppSecret}) {
		t.Errorf("backstage.backstage.extraEnvVarsSecrets = %v, want [%s %s]", got, portalAnthropicSecret, portalGitHubAppSecret)
	}
	github, _ := dig(s.overlay, "integrations", "github").([]any)
	if len(github) != 1 {
		t.Fatalf("overlay integrations.github = %v, want one github.com entry", github)
	}
	entry, _ := github[0].(map[string]any)
	apps, _ := entry["apps"].([]any)
	want := map[string]any{
		"appId":        "${" + portalGitHubAppIDKey + "}",
		"clientId":     "${" + portalGitHubAppClientIDKey + "}",
		"clientSecret": "${" + portalGitHubAppClientSecretKey + "}",
		"privateKey":   "${" + portalGitHubAppPrivateKeyKey + "}",
		"publicAccess": true,
	}
	if entry["host"] != "github.com" || entry["token"] != nil || len(apps) != 1 || !reflect.DeepEqual(apps[0], want) {
		t.Errorf("overlay integrations.github[0] = %v, want host github.com, no token and the App %v", entry, want)
	}
	for name, text := range s.raw() {
		for _, ref := range []string{keySource, clientSecretSource} {
			if strings.Contains(text, ref) {
				t.Errorf("%s carries the reference %s:\n%s", name, ref, text)
			}
		}
	}

	newFakeLab(t)
	ctx := context.Background()
	const testClientSecret = "client-secret-value" // #nosec G101 -- a test value, not a credential
	reads := fakeSecretSources(t, map[string]string{keySource: testPEM, clientSecretSource: testClientSecret})
	if err := ensureGitHubAppSecret(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if want := []string{keySource, keySource, clientSecretSource}; !reflect.DeepEqual(*reads, want) {
		t.Errorf("sources read = %q, want %q", *reads, want)
	}
	for key, value := range map[string]string{portalGitHubAppIDKey: "5288072", portalGitHubAppClientIDKey: gitHubAppTestClientID, portalGitHubAppPrivateKeyKey: testPEM, portalGitHubAppClientSecretKey: testClientSecret} {
		got, err := secretDataKey(ctx, platformNamespace, portalGitHubAppSecret, key)
		if err != nil || string(got) != value {
			t.Errorf("%s/%s key %s = %q (err %v), want %q", platformNamespace, portalGitHubAppSecret, key, got, err, value)
		}
	}

	cfg.Platform.Workspaces.GitHub.ClientSecretSource = ""
	cfg.Platform.Workspaces.GitHub.ClientID = ""
	for name, text := range renderGitHubTokenSurfaces(t, cfg).raw() {
		if strings.Contains(text, portalGitHubAppSecret) || strings.Contains(text, portalGitHubAppIDKey) {
			t.Errorf("%s wires the portal's App without its OAuth client:\n%s", name, text)
		}
	}
}

// TestPortalGitHubAuthenticated: the portal counts as authenticated exactly
// when its Deployment takes the token's or the App's Secret through envFrom.
func TestPortalGitHubAuthenticated(t *testing.T) {
	for _, tc := range []struct {
		secret string
		want   bool
	}{{"", false}, {portalAnthropicSecret, false}, {gitHubTokenSecret, true}, {portalGitHubAppSecret, true}} {
		d := backstageDeployment()
		if tc.secret != "" {
			d.Spec.Template.Spec.Containers = []corev1.Container{{Name: componentBackstage, EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: tc.secret}}}}}}
		}
		newFakeLab(t, d)
		got, err := portalGitHubAuthenticated()
		if err != nil || got != tc.want {
			t.Errorf("envFrom %q: authenticated = %v (err %v), want %v", tc.secret, got, err, tc.want)
		}
	}
}
