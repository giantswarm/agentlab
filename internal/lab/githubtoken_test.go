package lab

import (
	"context"
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

// TestGitHubTokenWiresBothConsumers: with $GITHUB_TOKEN set the values name
// the Secret for the portal (extraEnvVarsSecrets, read by the overlay's
// integrations.github reference), agent-manager (skills.github.tokenSecret)
// and the migrate Job (agentManager.migration.githubToken) — the Secret's
// NAME only: the token itself appears in nothing the render writes.
func TestGitHubTokenWiresBothConsumers(t *testing.T) {
	t.Setenv(GitHubTokenEnv, gitHubTestToken)
	s := renderGitHubTokenSurfaces(t, gitHubTokenTestConfig())

	if got := dig(s.values, componentBackstage, componentBackstage, "extraEnvVarsSecrets"); !reflect.DeepEqual(got, []any{gitHubTokenSecret}) {
		t.Errorf("backstage.backstage.extraEnvVarsSecrets = %v, want [%s]", got, gitHubTokenSecret)
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
		if strings.Contains(text, gitHubTestToken) {
			t.Errorf("%s carries the token itself:\n%s", name, text)
		}
	}
}

// TestGitHubTokenUnsetLeavesTheLabAsItWas: without the variable none of the
// keys render — the consumers call GitHub unauthenticated, as before.
func TestGitHubTokenUnsetLeavesTheLabAsItWas(t *testing.T) {
	t.Setenv(GitHubTokenEnv, "")
	s := renderGitHubTokenSurfaces(t, gitHubTokenTestConfig())
	for _, path := range [][]string{
		{componentBackstage, componentBackstage, "extraEnvVarsSecrets"},
		{agentManagerValuesKey, "skills"},
		{agentManagerWiringKey},
	} {
		if got := dig(s.values, path...); got != nil {
			t.Errorf("%s = %v without $%s, want nothing", strings.Join(path, "."), got, GitHubTokenEnv)
		}
	}
	if got := dig(s.overlay, "integrations"); got != nil {
		t.Errorf("overlay integrations = %v without $%s, want nothing", got, GitHubTokenEnv)
	}
	for name, text := range s.raw() {
		if strings.Contains(text, gitHubTokenSecret) {
			t.Errorf("%s names %s without $%s:\n%s", name, gitHubTokenSecret, GitHubTokenEnv, text)
		}
	}
}

// TestGitHubTokenLegacyChartTakesNoKeys: the 3.x line the rehearsal seeds
// has neither agent-manager's tokenSecret nor the migrate Job, so a seed run
// with the variable exported renders none of the keys.
func TestGitHubTokenLegacyChartTakesNoKeys(t *testing.T) {
	t.Setenv(GitHubTokenEnv, gitHubTestToken)
	cfg := gitHubTokenTestConfig()
	cfg.Platform.ChartVersion = legacyChartVersion
	if !cfg.LegacyChart() {
		t.Fatalf("%s is not the legacy line?", legacyChartVersion)
	}
	for name, text := range renderGitHubTokenSurfaces(t, cfg).raw() {
		if strings.Contains(text, gitHubTokenSecret) || strings.Contains(text, gitHubTestToken) {
			t.Errorf("%s wires the GitHub token on the 3.x line:\n%s", name, text)
		}
	}
}

// TestEnsureGitHubTokenSecrets: nothing without the variable; with it the
// agent-platform Secret, the kagent copy once that namespace exists, an
// update (not "left alone") on a new token, and no deletion when a later run
// lacks the variable.
func TestEnsureGitHubTokenSecrets(t *testing.T) {
	newFakeLab(t)
	ctx := context.Background()
	cfg := gitHubTokenTestConfig()
	tokenIn := func(ns string) string {
		t.Helper()
		got, err := secretDataKey(ctx, ns, gitHubTokenSecret, gitHubTokenSecretKey)
		if err != nil {
			t.Fatalf("%s/%s: %v", ns, gitHubTokenSecret, err)
		}
		return string(got)
	}
	exists := func(ns string) bool {
		t.Helper()
		ok, err := objectExists(ctx, gvrSecrets, ns, gitHubTokenSecret)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	t.Setenv(GitHubTokenEnv, "")
	if err := ensureGitHubTokenSecrets(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if exists(platformNamespace) {
		t.Fatalf("a Secret was written without $%s", GitHubTokenEnv)
	}

	t.Setenv(GitHubTokenEnv, gitHubTestToken)
	if err := ensureGitHubTokenSecrets(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := tokenIn(platformNamespace); got != gitHubTestToken {
		t.Errorf("%s/%s carries %q, want the token from $%s", platformNamespace, gitHubTokenSecret, got, GitHubTokenEnv)
	}
	if exists(kagentNamespace) {
		t.Errorf("the kagent copy landed before the namespace exists (the chart creates it)")
	}

	if err := ensureNamespace(kagentNamespace); err != nil {
		t.Fatal(err)
	}
	if err := ensureGitHubTokenSecrets(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := tokenIn(kagentNamespace); got != gitHubTestToken {
		t.Errorf("%s/%s carries %q after the namespace exists, want the token", kagentNamespace, gitHubTokenSecret, got)
	}

	rotated := gitHubTestToken + "-rotated"
	t.Setenv(GitHubTokenEnv, rotated)
	if err := ensureGitHubTokenSecrets(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{platformNamespace, kagentNamespace} {
		if got := tokenIn(ns); got != rotated {
			t.Errorf("%s/%s carries %q after a re-run with a new token, want the rotation", ns, gitHubTokenSecret, got)
		}
	}

	t.Setenv(GitHubTokenEnv, "")
	if err := ensureGitHubTokenSecrets(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{platformNamespace, kagentNamespace} {
		if !exists(ns) {
			t.Errorf("a run without $%s removed %s/%s", GitHubTokenEnv, ns, gitHubTokenSecret)
		}
	}
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

// TestGitHubTokenSourceWiresBothConsumers: a recorded githubToken.source
// wires the consumers without $GITHUB_TOKEN on the host — the lab started by
// an agent, whose environment never carries the token — and the render
// carries the Secret's name, never the reference's value.
func TestGitHubTokenSourceWiresBothConsumers(t *testing.T) {
	t.Setenv(GitHubTokenEnv, "")
	cfg := gitHubTokenTestConfig()
	cfg.GitHubToken.Source = gitHubTokenTestSource
	s := renderGitHubTokenSurfaces(t, cfg)
	if got := dig(s.values, agentManagerValuesKey, "skills", "github", "tokenSecret", nameKey); got != gitHubTokenSecret {
		t.Errorf("agent-manager.skills.github.tokenSecret.name = %v with githubToken.source, want %s", got, gitHubTokenSecret)
	}
	if got := dig(s.values, componentBackstage, componentBackstage, "extraEnvVarsSecrets"); !reflect.DeepEqual(got, []any{gitHubTokenSecret}) {
		t.Errorf("backstage.backstage.extraEnvVarsSecrets = %v with githubToken.source, want [%s]", got, gitHubTokenSecret)
	}
}

// TestEnsureGitHubTokenSecretsFromSource: a recorded source is handed to the
// secret tooling for each namespace, the source winning over $GITHUB_TOKEN;
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
