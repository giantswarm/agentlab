package lab

import (
	"context"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agentlab/internal/config"
)

// The MCPServer `github` (github-mcp.yaml.tmpl) reads its OAuth client — an
// OAuth App, or a GitHub App's client, whose callback URL is muster's proxy
// callback — from the Secret platform.github.secret names. The operator's
// secret tooling places it (`beekeeper secret copy <ref> --to-secret
// <context>/<namespace>/<name>/<key>`, one call per key); agentlab reads
// which keys it carries and never a value: nothing of the client reaches
// agentlab.yaml, state/, a log line, a process's argv or the environment of
// whoever runs the lab.
const gitHubMCPServer = "github"

// gitHubMCPClientKeys are the keys the Secret must carry, muster's
// clientCredentialsSecretRef defaults.
var gitHubMCPClientKeys = []string{config.GitHubClientIDKey, config.GitHubClientSecretKey}

// gitHubMCPHealthyStates: Auth Required until a session signs in, Connected
// while one is.
var gitHubMCPHealthyStates = []string{mcpServerStateAuthRequired, mcpServerStateConnected}

// ensureGitHubMCP registers GitHub's hosted MCP server with muster while
// platform.github is on and removes it while it is off. After the umbrella
// install: the MCPServer CRD ships with muster.
//
// Without the Secret, or with a key missing, nothing is applied: a server
// whose client is incomplete could never complete a sign-in. The run says
// which Secret and keys it looked for and how to place them, and goes on.
func ensureGitHubMCP(cfg *config.Config) error {
	ctx := context.Background()
	if !cfg.Platform.GitHub.Enabled {
		return removeGitHubMCP(ctx)
	}
	ref := cfg.Platform.GitHub.ClientSecret()
	missing, err := gitHubMCPClientMissing(ctx, ref.Namespace, ref.Name)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		warn("platform.github is on but Secret %s/%s lacks %s -- skipping MCPServer %s", ref.Namespace, ref.Name, strings.Join(missing, " and "), gitHubMCPServer)
		warn("  register an OAuth App (or GitHub App) with callback URL %s%s and place its client, one key per call:", cfg.MusterBaseURL(), oauthProxyCallbackPath)
		for _, key := range gitHubMCPClientKeys {
			warn("  beekeeper secret copy <ref to the %s> --to-secret kind-%s/%s/%s/%s", key, cfg.ClusterName, ref.Namespace, ref.Name, key)
		}
		warn("  then re-run; agentlab never reads the values")
		return nil
	}
	step("Registering GitHub's hosted MCP server with muster (MCPServer %s, client Secret %s/%s)", gitHubMCPServer, ref.Namespace, ref.Name)
	rendered, _, err := renderManifest(cfg, "github-mcp.yaml.tmpl")
	if err != nil {
		return err
	}
	if _, err := applyManifests(ctx, rendered); err != nil {
		return err
	}
	note("sign in once per person: core_auth_login %s through muster, or the portal's Sign in; the OAuth client's callback URL is %s%s", gitHubMCPServer, cfg.MusterBaseURL(), oauthProxyCallbackPath)
	return waitMCPServerState(gitHubMCPServer, gitHubMCPHealthyStates...)
}

// gitHubMCPClientMissing reads the client Secret and answers what it lacks:
// the Secret itself, or the keys without a value. The values stay in the
// apiserver's answer; nothing of them is kept, compared or printed.
func gitHubMCPClientMissing(ctx context.Context, ns, name string) ([]string, error) {
	obj, err := getObject(ctx, gvrSecrets, ns, name)
	if apierrors.IsNotFound(err) {
		return []string{"the Secret itself"}, nil
	}
	if err != nil {
		return nil, err
	}
	return missingSecretKeys(obj, gitHubMCPClientKeys...), nil
}

// missingSecretKeys lists the keys of a Secret that are absent or empty, by
// name, in the order asked for.
func missingSecretKeys(secret *unstructured.Unstructured, keys ...string) []string {
	data, _, _ := unstructured.NestedMap(secret.Object, "data")
	var missing []string
	for _, key := range keys {
		if value, ok := data[key].(string); !ok || value == "" {
			missing = append(missing, "key "+key)
		}
	}
	return missing
}

// removeGitHubMCP deletes the MCPServer when agentlab created it; an object
// of that name without the lab's managed-by label is someone else's and
// stays. The client Secret is the operator's and is never touched.
// Idempotent.
func removeGitHubMCP(ctx context.Context) error {
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return err
	}
	return deleteIfLabManaged(ctx, gvr, gitHubMCPServer)
}

func deleteIfLabManaged(ctx context.Context, gvr schema.GroupVersionResource, name string) error {
	obj, err := getObject(ctx, gvr, platformNamespace, name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if obj.GetLabels()[managedByLabel] != managedByAgentlabValue {
		return nil
	}
	note("deleting %s %s/%s (platform.github is off)", gvr.Resource, platformNamespace, name)
	return deleteObject(ctx, gvr, platformNamespace, name, 0)
}
