package lab

import (
	"context"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agentlab/internal/config"
)

// The OAuth client of the MCPServer `github` (github-mcp.yaml.tmpl): an OAuth
// App, or a GitHub App's client, whose callback URL is muster's proxy
// callback. Real credentials: host environment -> Kubernetes Secret only,
// never agentlab.yaml, state/, a log line or a process's argv.
const (
	GitHubMCPClientIDEnv     = "GITHUB_MCP_CLIENT_ID"     // #nosec G101 -- env var NAME, not a credential
	GitHubMCPClientSecretEnv = "GITHUB_MCP_CLIENT_SECRET" // #nosec G101 -- env var NAME, not a credential
)

const (
	gitHubMCPServer       = "github"
	gitHubMCPClientSecret = "github-oauth-client" // #nosec G101 -- Secret NAME, not a credential
)

// gitHubMCPHealthyStates: Auth Required until a session signs in, Connected
// while one is.
var gitHubMCPHealthyStates = []string{mcpServerStateAuthRequired, mcpServerStateConnected}

// ensureGitHubMCP registers GitHub's hosted MCP server with muster while
// platform.github is on and removes it while it is off. After the umbrella
// install: the MCPServer CRD ships with muster.
//
// Without both variables nothing is applied: a server whose client Secret is
// missing could never complete a sign-in, and a run that merely lacks an
// export leaves an earlier run's Secret and MCPServer as they are.
func ensureGitHubMCP(cfg *config.Config) error {
	ctx := context.Background()
	if !cfg.Platform.GitHub.Enabled {
		return removeGitHubMCP(ctx)
	}
	clientID, clientSecret := os.Getenv(GitHubMCPClientIDEnv), os.Getenv(GitHubMCPClientSecretEnv)
	if clientID == "" || clientSecret == "" {
		note("platform.github is on but $%s / $%s are not both set -- skipping MCPServer %s", GitHubMCPClientIDEnv, GitHubMCPClientSecretEnv, gitHubMCPServer)
		note("  register an OAuth App (or GitHub App) with callback URL %s%s, export both and re-run", cfg.MusterBaseURL(), oauthProxyCallbackPath)
		return nil
	}
	step("Registering GitHub's hosted MCP server with muster (MCPServer %s)", gitHubMCPServer)
	// Applied in-process, so the client secret never appears in a process's
	// argv or in a file; server-side apply creates or rotates it.
	if _, err := applyTyped(ctx, &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: kindSecret},
		ObjectMeta: metav1.ObjectMeta{
			Name: gitHubMCPClientSecret, Namespace: platformNamespace,
			Labels: map[string]string{managedByLabel: managedByAgentlabValue},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"client-id": []byte(clientID), "client-secret": []byte(clientSecret)},
	}); err != nil {
		return err
	}
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

// removeGitHubMCP deletes the MCPServer and its client Secret when agentlab
// created them; an object of that name without the lab's managed-by label is
// someone else's and stays. Idempotent.
func removeGitHubMCP(ctx context.Context) error {
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return err
	}
	if err := deleteIfLabManaged(ctx, gvr, gitHubMCPServer); err != nil {
		return err
	}
	return deleteIfLabManaged(ctx, gvrSecrets, gitHubMCPClientSecret)
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
