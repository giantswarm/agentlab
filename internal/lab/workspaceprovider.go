package lab

import (
	"context"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/giantswarm/agentlab/internal/config"
)

// The workspace-manager's provider instances (platform.workspaces.provider),
// both of the chart's `github` kind, each with a Secret the values reference
// and never a value:
//
//   - fake, the default: the lab's own GitHub (githubfake_workspaces.go) as a
//     GitHub Enterprise-shaped instance at https://github.<domain>, run by the
//     lab in a container (workspacefake.go). Its App is the embedded
//     fixture's; the App's key pair and the client secret are the lab's own
//     (certs/github-fake, `agentlab github-fake credentials`), placed as the
//     Secret agent-platform/workspace-fake by the install.
//   - github: a GitHub App of the lab's own on github.com, its App id and
//     client id in agentlab.yaml, its private key and client secret in the
//     Secret agent-platform/workspace-github, which the operator's secret
//     tooling places (`beekeeper secret copy <ref> --to-secret
//     <context>/agent-platform/workspace-github/<key>`, one call per key).
//     agentlab reads which keys it carries and never a value.

// workspaceGitHubKeys are the keys an instance's Secret must carry: the
// App's private key (the mirrors' sync) and the OAuth client's secret (the
// person's sign-in).
var workspaceGitHubKeys = []string{config.WorkspaceGitHubPrivateKeyKey, config.GitHubClientSecretKey}

// workspaceProviderValues is one instance as the values template renders it
// into workspace-manager.providers. URL is the instance's web URL, the git
// host, whose REST API the workspace-manager takes under /api/v3; empty is
// github.com.
type workspaceProviderValues struct {
	Name, URL, AppID, ClientID       string
	Secret, PrivateKeyKey, SecretKey string
}

// workspaceFakeValues is the instance fake: the embedded fixture's App (the
// one `agentlab github-fake --workspaces` serves without --fixture) at the
// lab's GitHub, its Secret the lab's own.
func workspaceFakeValues(cfg *config.Config) (*workspaceProviderValues, error) {
	fixture, err := loadGitHubFixture("")
	if err != nil {
		return nil, err
	}
	return &workspaceProviderValues{
		Name:          config.WorkspaceProviderFake,
		URL:           cfg.GitHubFakeURL(),
		AppID:         strconv.FormatInt(fixture.App.ID, 10),
		ClientID:      fixture.App.ClientID,
		Secret:        config.WorkspaceFakeSecretName,
		PrivateKeyKey: config.WorkspaceGitHubPrivateKeyKey,
		SecretKey:     config.GitHubClientSecretKey,
	}, nil
}

// workspaceGitHubValuesFor is the instance github as agentlab.yaml configures
// it: nil without the provider, or while the App's ids are empty (the
// workspace-manager refuses an instance without them at start-up). A render
// without a cluster takes it as it is; the install renders what
// workspaceGitHubFor finds.
func workspaceGitHubValuesFor(cfg *config.Config) *workspaceProviderValues {
	gh := cfg.Platform.Workspaces.GitHub
	if !cfg.WorkspaceGitHubProvider() || gh.AppID == "" || gh.ClientID == "" {
		return nil
	}
	return &workspaceProviderValues{
		Name:          config.WorkspaceProviderGitHub,
		AppID:         gh.AppID,
		ClientID:      gh.ClientID,
		Secret:        config.WorkspaceGitHubSecretName,
		PrivateKeyKey: config.WorkspaceGitHubPrivateKeyKey,
		SecretKey:     config.GitHubClientSecretKey,
	}
}

// workspaceProvidersFor are the instances a render without a cluster
// carries, in the workspace-manager's order: the fake when configured, the
// github instance when configured with its ids.
func workspaceProvidersFor(cfg *config.Config) ([]workspaceProviderValues, error) {
	return workspaceProviders(cfg, workspaceGitHubValuesFor(cfg))
}

// workspaceProvidersInstall are the instances the install renders: the fake
// when configured, the github instance as workspaceGitHubFor allows it.
func workspaceProvidersInstall(ctx context.Context, cfg *config.Config) ([]workspaceProviderValues, error) {
	github, err := workspaceGitHubFor(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return workspaceProviders(cfg, github)
}

func workspaceProviders(cfg *config.Config, github *workspaceProviderValues) ([]workspaceProviderValues, error) {
	var providers []workspaceProviderValues
	if cfg.WorkspaceFakeProvider() {
		fake, err := workspaceFakeValues(cfg)
		if err != nil {
			return nil, err
		}
		providers = append(providers, *fake)
	}
	if github != nil {
		providers = append(providers, *github)
	}
	return providers, nil
}

// workspaceProviderNames are the instances' names, as list_providers lists
// them.
func workspaceProviderNames(providers []workspaceProviderValues) []string {
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.Name)
	}
	return names
}

// workspaceGitHubFor is the instance github the install renders: the
// configured one once the App's ids are set and its Secret carries both
// keys. Otherwise nil, with a warning that names what is missing, the
// callback URL to register and the commands that place the Secret: the base
// URL, the route and the Dex redirect URI are in place regardless, so the
// instance is all the App's registration still needs.
func workspaceGitHubFor(ctx context.Context, cfg *config.Config) (*workspaceProviderValues, error) {
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
	warn("platform.workspaces.provider names github but %s missing -- the workspace-manager gets no provider instance %s", strings.Join(missing, ", "), config.WorkspaceProviderGitHub)
	warn("  register a GitHub App with callback URL %s, record its App id and client id in agentlab.yaml and place its keys, one per call:", cfg.WorkspaceGitHubCallbackURL())
	for _, key := range workspaceGitHubKeys {
		warn("  beekeeper secret copy <ref to the %s> --to-secret kind-%s/%s/%s/%s", key, cfg.ClusterName, platformNamespace, config.WorkspaceGitHubSecretName, key)
	}
	warn("  then re-run; agentlab never reads the values (docs/workspaces.md)")
	return nil, nil
}
