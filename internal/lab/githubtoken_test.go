package lab

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// gitHubTestToken is a sentinel no real token looks like; the assertions
// below hunt for it in everything the render writes to state/.
const gitHubTestToken = "ghp_agentlab_unit_test_sentinel_0000000000"

// renderGitHubTokenSurfaces renders the two files that name the Secret: the
// meta chart's values and the Backstage overlay, raw and parsed.
func renderGitHubTokenSurfaces(t *testing.T, cfg *config.Config) (valuesRaw string, values map[string]any, overlayRaw string, overlay map[string]any) {
	t.Helper()
	out, err := renderTemplate(cfg, platformValuesTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(out, &values); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	raw, err := renderTemplate(cfg, "backstage-catalog.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), values, string(raw), backstageOverlayAppConfig(t, cfg)
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
	valuesRaw, values, overlayRaw, overlay := renderGitHubTokenSurfaces(t, gitHubTokenTestConfig())

	if got := dig(values, "backstage", "backstage", "extraEnvVarsSecrets"); !reflect.DeepEqual(got, []any{gitHubTokenSecret}) {
		t.Errorf("backstage.backstage.extraEnvVarsSecrets = %v, want [%s]", got, gitHubTokenSecret)
	}
	for _, tc := range []struct {
		path []string
		want any
	}{
		{[]string{"agent-manager", "skills", "github", "tokenSecret", "name"}, gitHubTokenSecret},
		{[]string{"agent-manager", "skills", "github", "tokenSecret", "key"}, gitHubTokenSecretKey},
		{[]string{"agentManager", "migration", "githubToken", "secretName"}, gitHubTokenSecret},
		{[]string{"agentManager", "migration", "githubToken", "key"}, gitHubTokenSecretKey},
	} {
		if got := dig(values, tc.path...); got != tc.want {
			t.Errorf("%s = %v, want %v", strings.Join(tc.path, "."), got, tc.want)
		}
	}
	// The lab's other agent-manager values survive next to the new key.
	if got := dig(values, "agent-manager", "muster", "mcpServer", "auth", "requiredAudiences"); got == nil {
		t.Errorf("agent-manager.muster.mcpServer.auth.requiredAudiences went missing:\n%s", valuesRaw)
	}

	github, _ := dig(overlay, "integrations", "github").([]any)
	if len(github) != 1 {
		t.Fatalf("overlay integrations.github = %v, want one github.com entry", dig(overlay, "integrations", "github"))
	}
	entry, _ := github[0].(map[string]any)
	if entry["host"] != "github.com" || entry["token"] != "${"+GitHubTokenEnv+"}" {
		t.Errorf("overlay integrations.github[0] = %v, want host github.com and token ${%s}", entry, GitHubTokenEnv)
	}

	for name, text := range map[string]string{platformValuesTemplate: valuesRaw, "backstage-catalog.yaml.tmpl": overlayRaw} {
		if strings.Contains(text, gitHubTestToken) {
			t.Errorf("%s carries the token itself:\n%s", name, text)
		}
	}
}

// TestGitHubTokenUnsetLeavesTheLabAsItWas: without the variable none of the
// keys render — the consumers call GitHub unauthenticated, as before.
func TestGitHubTokenUnsetLeavesTheLabAsItWas(t *testing.T) {
	t.Setenv(GitHubTokenEnv, "")
	valuesRaw, values, overlayRaw, overlay := renderGitHubTokenSurfaces(t, gitHubTokenTestConfig())
	for _, path := range [][]string{
		{"backstage", "backstage", "extraEnvVarsSecrets"},
		{"agent-manager", "skills"},
		{"agentManager"},
	} {
		if got := dig(values, path...); got != nil {
			t.Errorf("%s = %v without $%s, want nothing", strings.Join(path, "."), got, GitHubTokenEnv)
		}
	}
	if got := dig(overlay, "integrations"); got != nil {
		t.Errorf("overlay integrations = %v without $%s, want nothing", got, GitHubTokenEnv)
	}
	for name, text := range map[string]string{platformValuesTemplate: valuesRaw, "backstage-catalog.yaml.tmpl": overlayRaw} {
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
	cfg.Platform.ChartVersion = "3.23.1"
	if !cfg.LegacyChart() {
		t.Fatal("3.23.1 is not the legacy line?")
	}
	valuesRaw, _, overlayRaw, _ := renderGitHubTokenSurfaces(t, cfg)
	for name, text := range map[string]string{platformValuesTemplate: valuesRaw, "backstage-catalog.yaml.tmpl": overlayRaw} {
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
