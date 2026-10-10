package lab

import (
	"context"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/giantswarm/agentlab/internal/config"
)

// The workspace-manager's provider instance github (platform.workspaces
// provider github): a GitHub App of the lab's own, its App id and client id
// in agentlab.yaml, its private key and client secret in the Secret
// agent-platform/workspace-github, which the operator's secret tooling
// places (`beekeeper secret copy <ref> --to-secret
// <context>/agent-platform/workspace-github/<key>`, one call per key). The
// values template references the Secret; agentlab reads which keys it
// carries and never a value.

// workspaceGitHubKeys are the keys the instance's Secret must carry: the
// App's private key (the mirrors' sync) and the OAuth client's secret (the
// person's sign-in).
var workspaceGitHubKeys = []string{config.WorkspaceGitHubPrivateKeyKey, config.GitHubClientSecretKey}

// workspaceGitHubValues is the instance the values template renders into
// workspace-manager.providers.
type workspaceGitHubValues struct {
	Name, AppID, ClientID            string
	Secret, PrivateKeyKey, SecretKey string
}

// workspaceGitHubValuesFor is the instance as agentlab.yaml configures it:
// nil without the provider, or while the App's ids are empty (the
// workspace-manager refuses an instance without them at start-up). A render
// without a cluster takes it as it is; the install renders what
// workspaceGitHubFor finds.
func workspaceGitHubValuesFor(cfg *config.Config) *workspaceGitHubValues {
	gh := cfg.Platform.Workspaces.GitHub
	if !cfg.WorkspaceGitHubProvider() || gh.AppID == "" || gh.ClientID == "" {
		return nil
	}
	return &workspaceGitHubValues{
		Name:          config.WorkspaceProviderGitHub,
		AppID:         gh.AppID,
		ClientID:      gh.ClientID,
		Secret:        config.WorkspaceGitHubSecretName,
		PrivateKeyKey: config.WorkspaceGitHubPrivateKeyKey,
		SecretKey:     config.GitHubClientSecretKey,
	}
}

// workspaceGitHubFor is the instance the install renders: the configured
// one once the App's ids are set and its Secret carries both keys. Otherwise
// nil, with a warning that names what is missing, the callback URL to
// register and the commands that place the Secret: the base URL, the route
// and the Dex redirect URI are in place regardless, so the instance is all
// the App's registration still needs.
func workspaceGitHubFor(ctx context.Context, cfg *config.Config) (*workspaceGitHubValues, error) {
	if !cfg.WorkspaceGitHubProvider() {
		return nil, nil
	}
	var missing []string
	gh := cfg.Platform.Workspaces.GitHub
	if gh.AppID == "" {
		missing = append(missing, "platform.workspaces.github.appId")
	}
	if gh.ClientID == "" {
		missing = append(missing, "platform.workspaces.github.clientId")
	}
	obj, err := getObject(ctx, gvrSecrets, platformNamespace, config.WorkspaceGitHubSecretName)
	switch {
	case apierrors.IsNotFound(err):
		missing = append(missing, "Secret "+platformNamespace+"/"+config.WorkspaceGitHubSecretName)
	case err != nil:
		return nil, err
	default:
		for _, key := range missingSecretKeys(obj, workspaceGitHubKeys...) {
			missing = append(missing, "Secret "+platformNamespace+"/"+config.WorkspaceGitHubSecretName+" "+key)
		}
	}
	note("workspace-manager at %s: the GitHub App's callback URL is %s, the lab Dex's redirect URI %s", cfg.WorkspaceManagerBaseURL(), cfg.WorkspaceGitHubCallbackURL(), cfg.WorkspaceManagerSignInURL())
	if len(missing) == 0 {
		return workspaceGitHubValuesFor(cfg), nil
	}
	warn("platform.workspaces.provider is github but %s missing -- the workspace-manager gets no provider instance %s", strings.Join(missing, ", "), config.WorkspaceProviderGitHub)
	warn("  register a GitHub App with callback URL %s, record its App id and client id in agentlab.yaml and place its keys, one per call:", cfg.WorkspaceGitHubCallbackURL())
	for _, key := range workspaceGitHubKeys {
		warn("  beekeeper secret copy <ref to the %s> --to-secret kind-%s/%s/%s/%s", key, cfg.ClusterName, platformNamespace, config.WorkspaceGitHubSecretName, key)
	}
	warn("  then re-run; agentlab never reads the values (docs/workspaces.md)")
	return nil, nil
}
