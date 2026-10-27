package lab

import (
	"context"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// gitHubTestToken is a sentinel no real token looks like; the assertions
// below hunt for it in everything the render writes to state/.
const gitHubTestToken = "ghp_agentlab_unit_test_sentinel_0000000000" // #nosec G101 -- test sentinel, not a credential

// The values blocks the token lands in: the agent-manager chart's own and
// the connectivity chart's wiring for it; the chart version of the 3.x line.
const (
	agentManagerValuesKey = "agent-manager"
	agentManagerWiringKey = "agentManager"
	legacyChartVersion    = "3.23.1"
)

// gitHubTokenSurfaces is what the render writes that could name the Secret
// (or leak the token): the meta chart's values and the Backstage overlay,
// raw and parsed.
type gitHubTokenSurfaces struct {
	valuesRaw, overlayRaw string
	values, overlay       map[string]any
}

// raw is the rendered text by template name, for the token hunt.
func (s gitHubTokenSurfaces) raw() map[string]string {
	return map[string]string{platformValuesTemplate: s.valuesRaw, backstageOverlayTemplate: s.overlayRaw}
}

func renderGitHubTokenSurfaces(t *testing.T, cfg *config.Config) gitHubTokenSurfaces {
	t.Helper()
	out, err := renderTemplate(cfg, platformValuesTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(out, &values); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	overlayRaw, err := renderTemplate(cfg, backstageOverlayTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	return gitHubTokenSurfaces{valuesRaw: string(out), overlayRaw: string(overlayRaw), values: values, overlay: backstageOverlayAppConfig(t, cfg)}
}

func gitHubTokenTestConfig() *config.Config {
	cfg := config.Default()
	cfg.Platform.Enabled, cfg.Platform.Agents, cfg.Backstage.Enabled = true, true, true
	return cfg
}

// TestGitHubTokenWiresBothConsumers: with githubToken.source recorded the
// values name the Secret for the portal (extraEnvVarsSecrets, read by the
// overlay's integrations.github reference), agent-manager
// (skills.github.tokenSecret) and the migrate Job
// (agentManager.migration.githubToken) — the Secret's NAME only: the
// reference appears in nothing the render writes.
func TestGitHubTokenWiresBothConsumers(t *testing.T) {
	cfg := gitHubTokenTestConfig()
	cfg.GitHubToken.Source = gitHubTokenTestSource
	s := renderGitHubTokenSurfaces(t, cfg)

	if got := dig(s.values, componentBackstage, componentBackstage, "extraEnvVarsSecrets"); !reflect.DeepEqual(got, []any{portalAnthropicSecret, gitHubTokenSecret}) {
		t.Errorf("backstage.backstage.extraEnvVarsSecrets = %v, want [%s %s]", got, portalAnthropicSecret, gitHubTokenSecret)
	}
	tokenSecret := []string{agentManagerValuesKey, "skills", "github", "tokenSecret"}
	githubToken := []string{agentManagerWiringKey, "migration", "githubToken"}
	for _, tc := range []struct {
		path []string
		want any
	}{
		{append(tokenSecret, nameKey), gitHubTokenSecret},
		{append(tokenSecret, fieldKey), gitHubTokenSecretKey},
		{append(githubToken, "secretName"), gitHubTokenSecret},
		{append(githubToken, fieldKey), gitHubTokenSecretKey},
	} {
		if got := dig(s.values, tc.path...); got != tc.want {
			t.Errorf("%s = %v, want %v", strings.Join(tc.path, "."), got, tc.want)
		}
	}
	// The lab's other agent-manager values survive next to the new key.
	if got := dig(s.values, agentManagerValuesKey, "muster", "mcpServer", "auth", "requiredAudiences"); got == nil {
		t.Errorf("agent-manager.muster.mcpServer.auth.requiredAudiences went missing:\n%s", s.valuesRaw)
	}

	integrations := dig(s.overlay, "integrations", "github")
	github, _ := integrations.([]any)
	if len(github) != 1 {
		t.Fatalf("overlay integrations.github = %v, want one github.com entry", integrations)
	}
	entry, _ := github[0].(map[string]any)
	if entry["host"] != "github.com" || entry["token"] != "${"+GitHubTokenEnv+"}" {
		t.Errorf("overlay integrations.github[0] = %v, want host github.com and token ${%s}", entry, GitHubTokenEnv)
	}

	for name, text := range s.raw() {
		if strings.Contains(text, gitHubTokenTestSource) {
			t.Errorf("%s carries the source reference:\n%s", name, text)
		}
	}
}

// TestGitHubTokenEnvIsNeverRead: without githubToken.source none of the keys
// render, a $GITHUB_TOKEN in agentlab's environment notwithstanding — the
// consumers call GitHub unauthenticated (or as the App); the portal's
// environment keeps only the Anthropic key.
func TestGitHubTokenEnvIsNeverRead(t *testing.T) {
	t.Setenv(GitHubTokenEnv, gitHubTestToken)
	s := renderGitHubTokenSurfaces(t, gitHubTokenTestConfig())
	if got := dig(s.values, componentBackstage, componentBackstage, "extraEnvVarsSecrets"); !reflect.DeepEqual(got, []any{portalAnthropicSecret}) {
		t.Errorf("backstage.backstage.extraEnvVarsSecrets = %v without githubToken.source, want [%s]", got, portalAnthropicSecret)
	}
	for _, path := range [][]string{
		{agentManagerValuesKey, "skills"},
		{agentManagerWiringKey},
	} {
		if got := dig(s.values, path...); got != nil {
			t.Errorf("%s = %v without githubToken.source, want nothing", strings.Join(path, "."), got)
		}
	}
	if got := dig(s.overlay, "integrations"); got != nil {
		t.Errorf("overlay integrations = %v without githubToken.source, want nothing", got)
	}
	for name, text := range s.raw() {
		if strings.Contains(text, gitHubTokenSecret) || strings.Contains(text, gitHubTestToken) {
			t.Errorf("%s names %s or carries $%s without githubToken.source:\n%s", name, gitHubTokenSecret, GitHubTokenEnv, text)
		}
	}
}

// TestGitHubTokenLegacyChartTakesNoKeys: the 3.x line the rehearsal seeds
// has neither agent-manager's tokenSecret nor the migrate Job, so a seed run
// with the source recorded renders none of the keys.
func TestGitHubTokenLegacyChartTakesNoKeys(t *testing.T) {
	cfg := gitHubTokenTestConfig()
	cfg.GitHubToken.Source = gitHubTokenTestSource
	cfg.Platform.ChartVersion = legacyChartVersion
	if !cfg.LegacyChart() {
		t.Fatalf("%s is not the legacy line?", legacyChartVersion)
	}
	for name, text := range renderGitHubTokenSurfaces(t, cfg).raw() {
		if strings.Contains(text, gitHubTokenSecret) {
			t.Errorf("%s wires the GitHub token on the 3.x line:\n%s", name, text)
		}
	}
}

// TestEnsureGitHubTokenSecrets: without githubToken.source nothing is
// written and the secret tooling is not called, a $GITHUB_TOKEN in agentlab's
// environment notwithstanding; a Secret an earlier run placed stays.
func TestEnsureGitHubTokenSecrets(t *testing.T) {
	newFakeLab(t)
	ctx := context.Background()
	cfg := gitHubTokenTestConfig()
	t.Setenv(GitHubTokenEnv, gitHubTestToken)
	calls := fakeSecretTool(t, func() error { return nil })
	exists := func() bool {
		t.Helper()
		ok, err := objectExists(ctx, gvrSecrets, platformNamespace, gitHubTokenSecret)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	var err error
	warned := stderrOf(t, func() { err = ensureGitHubTokenSecrets(ctx, cfg) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warned, "WARNING") || !strings.Contains(warned, "githubToken.source") || !strings.Contains(warned, "--github-token-source") {
		t.Errorf("without githubToken.source: stderr %q, want a warning naming the setting and its configure flag", warned)
	}
	if exists() || len(*calls) != 0 {
		t.Fatalf("without githubToken.source: Secret written=%v, secret tooling calls %q; want neither", exists(), *calls)
	}

	if err := ensureSecret(platformNamespace, gitHubTokenSecret, corev1.SecretTypeOpaque, map[string][]byte{gitHubTokenSecretKey: []byte("placed earlier")}); err != nil {
		t.Fatal(err)
	}
	if err := ensureGitHubTokenSecrets(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !exists() {
		t.Errorf("a run without githubToken.source removed %s/%s", platformNamespace, gitHubTokenSecret)
	}
}

// A lab whose portal and agents are both off calls GitHub for nothing, so
// a missing githubToken.source is no warning.
func TestEnsureGitHubTokenSecretsQuietWithoutConsumers(t *testing.T) {
	newFakeLab(t)
	cfg := gitHubTokenTestConfig()
	cfg.Platform.Agents, cfg.Backstage.Enabled = false, false
	fakeSecretTool(t, func() error { return nil })
	var err error
	if warned := stderrOf(t, func() { err = ensureGitHubTokenSecrets(t.Context(), cfg) }); warned != "" {
		t.Errorf("stderr %q, want nothing", warned)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// stderrOf is what fn prints on stderr.
func stderrOf(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = stderr }()
	fn()
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestDeploymentSetsEnv: E4/E5 run only when agent-manager's Deployment
// carries a GitHub credential, the lab's token or the skills App, in any
// container; anything else is the anonymous window and skips them.
func TestDeploymentSetsEnv(t *testing.T) {
	const podNameEnv = "K8S_POD_NAME"
	deployment := func(containers ...[]string) *appsv1.Deployment {
		d := &appsv1.Deployment{}
		for _, names := range containers {
			c := corev1.Container{}
			for _, n := range names {
				c.Env = append(c.Env, corev1.EnvVar{Name: n})
			}
			d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, c)
		}
		return d
	}
	for _, tc := range []struct {
		name string
		d    *appsv1.Deployment
		want bool
	}{
		{"the lab's token", deployment([]string{podNameEnv, GitHubTokenEnv}), true},
		{"the skills App", deployment([]string{"AGENT_MANAGER_SKILLS_GITHUB_APP_ID", "AGENT_MANAGER_SKILLS_GITHUB_APP_INSTALLATION_ID"}), true},
		{"in a second container", deployment([]string{podNameEnv}, []string{GitHubTokenEnv}), true},
		{"no credential", deployment([]string{podNameEnv}), false},
		{"no env at all", deployment(nil), false},
		{"no containers", deployment(), false},
	} {
		if got := deploymentSetsEnv(tc.d, agentManagerGitHubEnv); got != tc.want {
			t.Errorf("%s: deploymentSetsEnv = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// gitHubTokenTestSource is a reference of the form githubToken.source takes;
// nothing resolves it, the fake secret tooling stands in.
const gitHubTokenTestSource = "op://lab/github-token/credential" // #nosec G101 -- a reference, not a credential

// TestEnsureGitHubTokenSecretsFromSource: a recorded source is handed to the
// secret tooling for each namespace, agentlab writing nothing itself;
// a placement that fails fails the run, since the values already name the
// Secret.
func TestEnsureGitHubTokenSecretsFromSource(t *testing.T) {
	newFakeLab(t)
	ctx := context.Background()
	cfg := gitHubTokenTestConfig()
	cfg.GitHubToken.Source = gitHubTokenTestSource
	t.Setenv(GitHubTokenEnv, gitHubTestToken)
	if err := ensureNamespace(kagentNamespace); err != nil {
		t.Fatal(err)
	}

	calls := fakeSecretTool(t, func() error { return nil })
	if err := ensureGitHubTokenSecrets(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		secretCopyArgs(gitHubTokenTestSource, secretTarget(cfg, platformNamespace, gitHubTokenSecret, gitHubTokenSecretKey)),
		secretCopyArgs(gitHubTokenTestSource, secretTarget(cfg, kagentNamespace, gitHubTokenSecret, gitHubTokenSecretKey)),
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("secret tooling calls = %q, want %q", *calls, want)
	}
	if exists, err := objectExists(ctx, gvrSecrets, platformNamespace, gitHubTokenSecret); err != nil || exists {
		t.Errorf("agentlab wrote %s/%s itself (exists=%v, err=%v); the source is the tooling's to place", platformNamespace, gitHubTokenSecret, exists, err)
	}

	fakeSecretTool(t, nil)
	err := ensureGitHubTokenSecrets(ctx, cfg)
	if err == nil || !strings.Contains(err.Error(), "githubToken.source") {
		t.Errorf("err = %v, want the failed placement naming githubToken.source", err)
	}
}
