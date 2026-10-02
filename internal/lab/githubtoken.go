package lab

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/agentlab/internal/config"
)

// GitHubTokenEnv is where the lab reads a GitHub token from at deploy time
// (`agentlab up`/`platform`) and before the proofs that resolve skills. Like
// the Anthropic key (anthropic.go) it is a real credential: it travels host
// environment -> Kubernetes Secret only and never enters agentlab.yaml, the
// rendered state/ files, a log line or a process's argv. The same variable
// lifts the anonymous rate limit of the self-update check (internal/update).
const GitHubTokenEnv = "GITHUB_TOKEN" // #nosec G101 -- env var NAME, not a credential

// gitHubTokenSecret is the Secret the consumers read — one key, named after
// the env var so Backstage's envFrom yields $GITHUB_TOKEN as it is:
//   - agent-platform/: the portal (backstage.backstage.extraEnvVarsSecrets,
//     read by the overlay's integrations.github[].token: ${GITHUB_TOKEN})
//     and agent-manager (agent-manager.skills.github.tokenSecret);
//   - kagent/: the connectivity chart's agent-manager migrate Job
//     (agentManager.migration.githubToken).
//
// The values (agent-platform-values.yaml.tmpl, backstage-catalog.yaml.tmpl)
// name the Secret only when the variable is set at render time, so a render
// and the Secret always agree on one environment.
const (
	gitHubTokenSecret    = "agentlab-github-token" // #nosec G101 -- Secret NAME, not a credential
	gitHubTokenSecretKey = GitHubTokenEnv
)

// gitHubTokenSet reports whether the host environment carries a GitHub token
// — the one input the render and the Secret creation decide on.
func gitHubTokenSet() bool { return os.Getenv(GitHubTokenEnv) != "" }

// gitHubTokenWired is the render's answer: the token is set and the chart
// line takes the keys (the 4.x agent-manager chart's skills.github.tokenSecret
// and the connectivity chart's agentManager.migration.githubToken; the 3.x
// line the rehearsal seeds has neither, and its portal has no discovery to
// authenticate).
func gitHubTokenWired(cfg *config.Config) bool { return gitHubTokenSet() && !cfg.LegacyChart() }

// ensureGitHubTokenSecrets is the deploy-time half, run before the install:
// agent-platform/ always (the portal's envFrom and agent-manager's env
// reference the Secret without `optional`, so it must exist before the pods
// do), kagent/ when that namespace already exists — a re-run, or the
// rehearsal's upgrade of a seeded lab; a first install gets it after the
// chart created the namespace (platform.go, next to kagent-anthropic).
//
// Without the variable nothing is written: the values rendered from the same
// environment reference no Secret, so the consumers call GitHub
// unauthenticated — the lab's behaviour before the token — and a Secret an
// earlier run created stays, unreferenced, until `agentlab down`: a run that
// merely lacks an export never destroys a credential. The proofs' window
// print (githubwindow.go) is where the difference shows.
func ensureGitHubTokenSecrets(ctx context.Context, cfg *config.Config) error {
	if !gitHubTokenSet() {
		if exists, err := objectExists(ctx, gvrSecrets, platformNamespace, gitHubTokenSecret); err == nil && exists {
			note("$%s is not set — secret %s/%s from an earlier run stays, but nothing references it: GitHub is called unauthenticated (60 requests an hour, shared by this machine)", GitHubTokenEnv, platformNamespace, gitHubTokenSecret)
		}
		return nil
	}
	if err := ensureGitHubTokenSecret(ctx, platformNamespace); err != nil {
		return err
	}
	if !cfg.Platform.Agents {
		return nil
	}
	exists, err := objectExists(ctx, gvrNamespaces, "", kagentNamespace)
	if err != nil || !exists {
		return err
	}
	return ensureGitHubTokenSecret(ctx, kagentNamespace)
}

// ensureGitHubTokenSecret creates or updates ns/gitHubTokenSecret from the
// host environment — an update, unlike the Anthropic Secret, so a re-run with
// a new token rotates it; a no-op without the variable.
func ensureGitHubTokenSecret(_ context.Context, ns string) error {
	token := os.Getenv(GitHubTokenEnv)
	if token == "" {
		return nil
	}
	// Applied in-process, so the token never appears in a process's argv or
	// in a file; server-side apply creates or updates.
	if err := ensureSecret(ns, gitHubTokenSecret, corev1.SecretTypeOpaque, map[string][]byte{gitHubTokenSecretKey: []byte(token)}); err != nil {
		return err
	}
	note("secret %s/%s from $%s — skill discovery and resolution call GitHub authenticated", ns, gitHubTokenSecret, GitHubTokenEnv)
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
