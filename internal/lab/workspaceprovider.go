package lab

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/giantswarm/agentlab/internal/config"
)

// The workspace-manager's provider instances (platform.workspaces.provider),
// each with a Secret the values reference and never a value:
//
//   - github, the default: a GitHub App of the lab's own on github.com, its App id and
//     client id in agentlab.yaml, its private key and client secret in the
//     Secret agent-platform/workspace-github, which the operator's secret
//     tooling places (`beekeeper secret copy <ref> --to-secret
//     <context>/agent-platform/workspace-github/<key>`, one call per key):
//     every up and platform from platform.workspaces.github's
//     privateKeySource and clientSecretSource when agentlab.yaml records
//     them (ensureWorkspaceSecrets), by hand otherwise. agentlab reads
//     which keys it carries and never a value.
//   - gitlab: a GitLab the lab reaches (a self-managed test instance), of the
//     chart's `gitlab` kind: its URL and the OAuth application's client id
//     in agentlab.yaml, the group access token, the application's secret and
//     the proof person's password in the Secret agent-platform/workspace-gitlab,
//     placed the same way from platform.workspaces.gitlab's sources.

// workspaceGitHubKeys are the keys an instance's Secret must carry: the
// App's private key (the mirrors' sync) and the OAuth client's secret (the
// person's sign-in).
var workspaceGitHubKeys = []string{config.WorkspaceGitHubPrivateKeyKey, config.GitHubClientSecretKey}

// workspaceGitLabKeys are the keys the gitlab instance's Secret must carry
// for the instance to render: the sync's group access token and the OAuth
// application's secret. The person's password is the proof's alone.
var workspaceGitLabKeys = []string{config.WorkspaceGitLabTokenKey, config.GitHubClientSecretKey}

// The chart's provider kinds the values template renders.
const (
	providerKindGitHub = "github"
	providerKindGitLab = "gitlab"
)

// workspaceProviderValues is one instance as the values template renders it
// into workspace-manager.providers. URL is the instance's web URL, the git
// host: on a github instance the workspace-manager takes its REST API under
// /api/v3 and empty is github.com; on a gitlab instance under /api/v4.
// APIURL, when set, is the REST API's base URL instead. AppID and
// PrivateKeyKey are a github instance's App, TokenKey a gitlab instance's
// group access token.
type workspaceProviderValues struct {
	Name, Kind, URL, APIURL, AppID, ClientID   string
	Secret, PrivateKeyKey, SecretKey, TokenKey string
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
		Kind:          providerKindGitHub,
		AppID:         gh.AppID,
		ClientID:      gh.ClientID,
		Secret:        config.WorkspaceGitHubSecretName,
		PrivateKeyKey: config.WorkspaceGitHubPrivateKeyKey,
		SecretKey:     config.GitHubClientSecretKey,
	}
}

// workspaceGitLabValuesFor is the instance gitlab as agentlab.yaml
// configures it: nil without the provider, or while its URL or client id is
// empty. A render without a cluster takes it as it is; the install renders
// what workspaceGitLabFor finds.
func workspaceGitLabValuesFor(cfg *config.Config) *workspaceProviderValues {
	gl := cfg.Platform.Workspaces.GitLab
	if !cfg.WorkspaceGitLabProvider() || gl.URL == "" || gl.ClientID == "" {
		return nil
	}
	return &workspaceProviderValues{
		Name:      config.WorkspaceProviderGitLab,
		Kind:      providerKindGitLab,
		URL:       strings.TrimSuffix(gl.URL, "/"),
		ClientID:  gl.ClientID,
		Secret:    config.WorkspaceGitLabSecretName,
		SecretKey: config.GitHubClientSecretKey,
		TokenKey:  config.WorkspaceGitLabTokenKey,
	}
}

// workspaceProvidersFor are the instances a render without a cluster
// carries, in the workspace-manager's order: the github and gitlab
// instances when configured with their ids.
func workspaceProvidersFor(cfg *config.Config) []workspaceProviderValues {
	return workspaceProviders(workspaceGitHubValuesFor(cfg), workspaceGitLabValuesFor(cfg))
}

// workspaceProvidersInstall are the instances the install renders: the
// github and gitlab instances as workspaceGitHubFor and workspaceGitLabFor
// allow them.
func workspaceProvidersInstall(ctx context.Context, cfg *config.Config) ([]workspaceProviderValues, error) {
	github, err := workspaceGitHubFor(ctx, cfg)
	if err != nil {
		return nil, err
	}
	gitlab, err := workspaceGitLabFor(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return workspaceProviders(github, gitlab), nil
}

func workspaceProviders(configured ...*workspaceProviderValues) []workspaceProviderValues {
	var providers []workspaceProviderValues
	for _, p := range configured {
		if p != nil {
			providers = append(providers, *p)
		}
	}
	return providers
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

// sourcedKey is one key of an instance's Secret and where agentlab.yaml
// says its value is (field names the setting).
type sourcedKey struct{ key, field, source string }

// ensureWorkspaceSecrets is the deploy-time placement of the github and
// gitlab instances' Secrets, run before workspaceGitHubFor and
// workspaceGitLabFor read them: each key whose source agentlab.yaml records
// is placed by the secret tooling, and a placement that fails fails the run,
// so a lab whose configuration says where an instance's keys are never comes
// up without it. A key without a source is left as it is, for a placement by
// hand.
func ensureWorkspaceSecrets(cfg *config.Config) error {
	if cfg.WorkspaceGitHubProvider() {
		gh := cfg.Platform.Workspaces.GitHub
		if err := placeWorkspaceSecret(cfg, config.WorkspaceProviderGitHub, config.WorkspaceGitHubSecretName, []sourcedKey{
			{config.WorkspaceGitHubPrivateKeyKey, "privateKeySource", gh.PrivateKeySource},
			{config.GitHubClientSecretKey, "clientSecretSource", gh.ClientSecretSource},
		}); err != nil {
			return err
		}
	}
	if cfg.WorkspaceGitLabProvider() {
		gl := cfg.Platform.Workspaces.GitLab
		if err := placeWorkspaceSecret(cfg, config.WorkspaceProviderGitLab, config.WorkspaceGitLabSecretName, []sourcedKey{
			{config.WorkspaceGitLabTokenKey, "tokenSource", gl.TokenSource},
			{config.GitHubClientSecretKey, "clientSecretSource", gl.ClientSecretSource},
			{config.WorkspaceGitLabPasswordKey, "passwordSource", gl.PasswordSource},
		}); err != nil {
			return err
		}
	}
	return nil
}

func placeWorkspaceSecret(cfg *config.Config, instance, secret string, keys []sourcedKey) error {
	for _, k := range keys {
		if k.source == "" {
			continue
		}
		answer, err := runSecretTool(secretCopyArgs(k.source, secretTarget(cfg, platformNamespace, secret, k.key))...)
		if err != nil {
			return fmt.Errorf("secret %s/%s key %s from platform.workspaces.%s.%s %s: %w\n  the workspace-manager's provider instance %s references it; when %s answers, re-run `agentlab platform`",
				platformNamespace, secret, k.key, instance, k.field, k.source, err, instance, secretTool)
		}
		note("secret %s/%s key %s placed from %s by %s (%s); agentlab never read the value", platformNamespace, secret, k.key, k.source, secretTool, strings.TrimSpace(answer))
	}
	return nil
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
	callback := WorkspaceAppCallback(cfg)
	note("workspace-manager at %s: the GitHub App's callback URL is %s", cfg.WorkspaceManagerBaseURL(), callback)
	if len(missing) == 0 {
		return workspaceGitHubValuesFor(cfg), nil
	}
	warn("platform.workspaces.provider names github but %s missing -- the workspace-manager gets no provider instance %s", strings.Join(missing, ", "), config.WorkspaceProviderGitHub)
	warn("  register a GitHub App with callback URL %s, record its App id and client id in agentlab.yaml and where its keys are (platform.workspaces.github.privateKeySource and clientSecretSource: every up and platform places them), or place them by hand, one per call:", callback)
	for _, key := range workspaceGitHubKeys {
		warn("  %s secret copy <ref to the %s> --to-secret %s", secretTool, key, secretTarget(cfg, platformNamespace, config.WorkspaceGitHubSecretName, key))
	}
	warn("  then re-run; agentlab never reads the values (docs/workspaces.md)")
	return nil, nil
}

// workspaceGitLabFor is the instance gitlab the install renders: the
// configured one once its URL and client id are set and its Secret carries
// the token and the client secret. Otherwise nil, with a warning that names
// what is missing, the redirect URI to register on the OAuth application
// and the commands that place the Secret.
func workspaceGitLabFor(ctx context.Context, cfg *config.Config) (*workspaceProviderValues, error) {
	if !cfg.WorkspaceGitLabProvider() {
		return nil, nil
	}
	var missing []string
	gl := cfg.Platform.Workspaces.GitLab
	if gl.URL == "" {
		missing = append(missing, "platform.workspaces.gitlab.url")
	}
	if gl.ClientID == "" {
		missing = append(missing, "platform.workspaces.gitlab.clientId")
	}
	obj, err := getObject(ctx, gvrSecrets, platformNamespace, config.WorkspaceGitLabSecretName)
	switch {
	case apierrors.IsNotFound(err):
		missing = append(missing, "Secret "+platformNamespace+"/"+config.WorkspaceGitLabSecretName)
	case err != nil:
		return nil, err
	default:
		for _, key := range missingSecretKeys(obj, workspaceGitLabKeys...) {
			missing = append(missing, "Secret "+platformNamespace+"/"+config.WorkspaceGitLabSecretName+" "+key)
		}
	}
	callback := WorkspaceAppCallback(cfg)
	note("workspace-manager at %s: the GitLab OAuth application's redirect URI is %s", cfg.WorkspaceManagerBaseURL(), callback)
	if len(missing) == 0 {
		return workspaceGitLabValuesFor(cfg), nil
	}
	warn("platform.workspaces.provider names gitlab but %s missing -- the workspace-manager gets no provider instance %s", strings.Join(missing, ", "), config.WorkspaceProviderGitLab)
	warn("  register an OAuth application on the GitLab with redirect URI %s and the scopes api read_user read_repository write_repository, record the GitLab's url and the client id in agentlab.yaml and where the keys are (platform.workspaces.gitlab.tokenSource, clientSecretSource, passwordSource: every up and platform places them), or place them by hand, one per call:", callback)
	for _, key := range append(slices.Clone(workspaceGitLabKeys), config.WorkspaceGitLabPasswordKey) {
		warn("  %s secret copy <ref to the %s> --to-secret %s", secretTool, key, secretTarget(cfg, platformNamespace, config.WorkspaceGitLabSecretName, key))
	}
	warn("  then re-run; agentlab never reads the values (docs/workspaces.md)")
	return nil, nil
}

// WorkspaceAppCallback is the callback URL every instance's App or OAuth
// application registers: muster's proxy callback, where the person grants it
// (workspacemustergrant.go).
func WorkspaceAppCallback(cfg *config.Config) string {
	return cfg.MusterBaseURL() + oauthProxyCallbackPath
}
