package lab

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/agentlab/internal/config"
)

// GitHubTokenEnv is the env var name the consumers read the GitHub token
// under: the key of the Secret below. agentlab itself never reads a variable
// of this name — the token reaches the lab only through githubToken.source,
// placed by the secret tooling, so its value never passes through agentlab
// or the session that runs it.
const GitHubTokenEnv = "GITHUB_TOKEN" // #nosec G101 -- env var NAME, not a credential

// gitHubTokenSecret is the Secret the consumers read, in agent-platform/:
// one key, named after the env var so Backstage's envFrom yields
// $GITHUB_TOKEN as it is: the portal (backstage.backstage.extraEnvVarsSecrets,
// read by the overlay's integrations.github[].token: ${GITHUB_TOKEN}) and
// agent-manager (agent-manager.skills.github.tokenSecret). The connectivity
// chart's agent-manager migrate run reads no GitHub token, so the kagent
// namespace holds no copy.
//
// The values (agent-platform-values.yaml.tmpl, backstage-catalog.yaml.tmpl)
// name the Secret only when a token is configured at render time, so a
// render and the Secret always agree on one configuration.
const (
	gitHubTokenSecret    = "agentlab-github-token" // #nosec G101 -- Secret NAME, not a credential
	gitHubTokenSecretKey = GitHubTokenEnv
)

// gitHubTokenSet reports whether the lab has a GitHub token to place — the
// one input the render and the Secret creation decide on: the reference
// githubToken.source records.
func gitHubTokenSet(cfg *config.Config) bool { return cfg.GitHubToken.Source != "" }

// gitHubTokenWired is the render's answer: a token is set and the chart
// line takes the key (the 4.x agent-manager chart's skills.github.tokenSecret;
// the 3.x line the rehearsal seeds has none, and its portal has no discovery
// to authenticate).
func gitHubTokenWired(cfg *config.Config) bool { return gitHubTokenSet(cfg) && !cfg.LegacyChart() }

// ensureGitHubTokenSecrets is the deploy-time half, run before the install:
// the portal's envFrom and agent-manager's env reference the Secret without
// `optional`, so it must exist before the pods do. A kagent/ copy an earlier
// agentlab placed is removed: nothing in that namespace reads it.
//
// Without a token nothing is written: the values rendered from the same
// configuration reference no Secret, so the consumers call GitHub
// unauthenticated, and a Secret an earlier run created stays, unreferenced,
// until `agentlab down`: a run that merely lacks an export never destroys a
// credential. A lab whose portal or agents would call GitHub that way is
// warned, the setting named.
func ensureGitHubTokenSecrets(ctx context.Context, cfg *config.Config) error {
	if err := removeKagentGitHubTokenSecret(ctx); err != nil {
		return err
	}
	if !gitHubTokenSet(cfg) {
		exists, err := objectExists(ctx, gvrSecrets, platformNamespace, gitHubTokenSecret)
		if err == nil && exists {
			note("no githubToken.source — secret %s/%s (key %s) from an earlier run stays, but nothing references it", platformNamespace, gitHubTokenSecret, gitHubTokenSecretKey)
		}
		if (cfg.Platform.Agents || cfg.Backstage.Enabled) && !gitHubAppWired(cfg) {
			warn("%s records no githubToken.source and no skills GitHub App: skill discovery and resolution call GitHub unauthenticated, on the 60 requests an hour this machine's address shares, and agents-test skips refreshSkills; %s (`agentlab configure --github-token-source <ref>` records the token)", config.File, gitHubCredentialHint)
		}
		return nil
	}
	source := cfg.GitHubToken.Source
	answer, err := runSecretTool(secretCopyArgs(source, secretTarget(cfg, platformNamespace, gitHubTokenSecret, gitHubTokenSecretKey))...)
	if err != nil {
		return fmt.Errorf("secret %s/%s key %s from githubToken.source %s: %w\n  the portal and agent-manager reference it; when %s answers, re-run `agentlab platform`",
			platformNamespace, gitHubTokenSecret, gitHubTokenSecretKey, source, err, secretTool)
	}
	note("secret %s/%s key %s placed from %s by %s (%s); agentlab never read the value — skill discovery and resolution call GitHub authenticated", platformNamespace, gitHubTokenSecret, gitHubTokenSecretKey, source, secretTool, strings.TrimSpace(answer))
	return nil
}

// removeKagentGitHubTokenSecret deletes kagent/gitHubTokenSecret when an
// earlier agentlab placed it there; a lab without it changes nothing.
func removeKagentGitHubTokenSecret(ctx context.Context) error {
	exists, err := objectExists(ctx, gvrSecrets, kagentNamespace, gitHubTokenSecret)
	if err != nil || !exists {
		return err
	}
	if err := deleteObject(ctx, gvrSecrets, kagentNamespace, gitHubTokenSecret, 0); err != nil {
		return err
	}
	note("secret %s/%s removed: nothing in %s reads a GitHub token", kagentNamespace, gitHubTokenSecret, kagentNamespace)
	return nil
}

// agentManagerGitHubEnv are the env vars of agent-manager's Deployment that
// carry its GitHub credential for skill resolution: the static token the lab
// wires (skills.github.tokenSecret) or the skills GitHub App's id.
var agentManagerGitHubEnv = []string{GitHubTokenEnv, "AGENT_MANAGER_SKILLS_GITHUB_APP_ID"}

// agentManagerGitHubAuthenticated reports whether the lab's agent-manager
// calls GitHub with a credential — read off its Deployment, since the render
// followed the environment of the `agentlab platform` run, not this one's.
func agentManagerGitHubAuthenticated() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	obj, err := getObject(ctx, gvrDeployments, platformNamespace, agentManagerMCPServer)
	if err != nil {
		return false, err
	}
	var d appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &d); err != nil {
		return false, fmt.Errorf("reading %s: %w", describe(gvrDeployments, platformNamespace, agentManagerMCPServer), err)
	}
	return deploymentSetsEnv(&d, agentManagerGitHubEnv), nil
}

// portalGitHubAuthenticated reports whether the portal's discovery calls
// GitHub with a credential: its Deployment takes the token's or the App's
// Secret through envFrom (the values are never read).
func portalGitHubAuthenticated() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	obj, err := getObject(ctx, gvrDeployments, platformNamespace, componentBackstage)
	if err != nil {
		return false, err
	}
	var d appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &d); err != nil {
		return false, fmt.Errorf("reading %s: %w", describe(gvrDeployments, platformNamespace, componentBackstage), err)
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		for _, e := range c.EnvFrom {
			if e.SecretRef != nil && (e.SecretRef.Name == gitHubTokenSecret || e.SecretRef.Name == portalGitHubAppSecret) {
				return true, nil
			}
		}
	}
	return false, nil
}

// deploymentSetsEnv reports whether any container of the Deployment sets one
// of the env vars by name (the value, a Secret reference, is never read).
func deploymentSetsEnv(d *appsv1.Deployment, names []string) bool {
	for _, c := range d.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if slices.Contains(names, e.Name) {
				return true
			}
		}
	}
	return false
}
