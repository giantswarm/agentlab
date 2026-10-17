package lab

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Masterminds/semver/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/agentlab/internal/config"
)

// The workspace-manager's registration pinned to one provider App in muster
// (workspace-manager 0.10.0 and newer): a person grants that App once in
// muster (core_auth_login, grantScope subject), and muster attaches the
// person's App token to every call of the manager's tools, the Dex ID token
// beside it in X-Muster-Id-Token. A person who has not granted it gets
// muster's sign-in link instead of a tool's answer. The instance is
// config.WorkspaceGrantProvider; its OAuth client is the Secret the instance
// already names, which carries the client id under GitHubClientIDKey for
// muster beside the secret.
//
// An earlier workspace-manager takes no pin (its schema refuses the keys)
// and keeps each person's sign-in itself, so the lab pins only a manager
// whose chart, resolved the way Flux resolves the component's versionRange,
// is 0.10.0 or newer.

// workspaceManagerGrantFloor is the first workspace-manager whose
// registration muster pins to a provider App.
var workspaceManagerGrantFloor = semver.MustParse("0.10.0-0")

// workspaceGitLabScopes are the scopes the GitLab OAuth application is
// registered with, which its authorization asks for.
const workspaceGitLabScopes = "api read_user read_repository write_repository"

// The provider endpoints the pin names.
const (
	githubAuthorizePath   = "/login/oauth/authorize"
	githubAccessTokenPath = "/login/oauth/access_token" // #nosec G101 -- a path, not a credential
	githubOAuthIssuer     = "https://github.com/login/oauth"
	gitlabTokenPath       = "/oauth/token" // #nosec G101 -- a path, not a credential
)

// workspaceGrantValues is the pin as the values template renders it into
// workspace-manager.muster.mcpServer.auth: the instance, its App's issuer
// identity (the key muster files the person's grant under), the issuer the
// provider puts in its authorization response ("" for one that puts none),
// its endpoints, the scope ("" for a GitHub App) and the Secret of its OAuth
// client.
type workspaceGrantValues struct {
	Provider, Issuer, ExpectedIssuer     string
	AuthorizationEndpoint, TokenEndpoint string
	Scopes, Secret                       string
}

// workspaceGrantFor is the pin for the rendered instances, nil when the
// manager takes none or the instance is not rendered (a github or gitlab
// instance whose ids or Secret are not in place yet).
func workspaceGrantFor(cfg *config.Config, providers []workspaceProviderValues) *workspaceGrantValues {
	name := cfg.WorkspaceGrantProvider()
	if name == "" || !workspaceManagerTakesGrant(cfg) {
		return nil
	}
	for _, p := range providers {
		if p.Name == name {
			return workspaceGrantOf(p)
		}
	}
	return nil
}

// workspaceGrantOf is the pin to one instance's App.
func workspaceGrantOf(p workspaceProviderValues) *workspaceGrantValues {
	g := &workspaceGrantValues{Provider: p.Name, Secret: p.Secret}
	switch {
	case p.Kind == providerKindGitLab:
		base := strings.TrimSuffix(p.URL, "/")
		g.Issuer, g.AuthorizationEndpoint, g.TokenEndpoint, g.Scopes = base, base+"/oauth/authorize", base+gitlabTokenPath, workspaceGitLabScopes
	case p.URL == "":
		// github.com: the App is known by its client id (agentlab.yaml
		// records no slug), and GitHub names its issuer in the response.
		g.Issuer, g.ExpectedIssuer = "https://github.com/apps/"+p.ClientID, githubOAuthIssuer
		g.AuthorizationEndpoint, g.TokenEndpoint = "https://github.com"+githubAuthorizePath, "https://github.com"+githubAccessTokenPath
	default:
		// The lab's GitHub, which names no issuer in its response.
		base := strings.TrimSuffix(p.URL, "/")
		g.Issuer, g.AuthorizationEndpoint, g.TokenEndpoint = base+"/apps/"+p.ClientID, base+githubAuthorizePath, base+githubAccessTokenPath
	}
	return g
}

// workspaceManagerTakesGrant says whether the workspace-manager the install
// resolves is 0.10.0 or newer; one that cannot be resolved is a note and no
// pin.
func workspaceManagerTakesGrant(cfg *config.Config) bool {
	version, err := workspaceManagerVersion(cfg)
	if err != nil {
		note("the workspace-manager's chart version could not be resolved (%v): its registration is not pinned to a provider App", err)
		return false
	}
	if version == "" {
		return false
	}
	v, err := semver.NewVersion(version)
	return err == nil && !v.LessThan(workspaceManagerGrantFloor)
}

// rangeFloorAdmitsGrant says whether every version the range admits is at or
// above the grant floor: each of its ||-alternatives carries a lower bound
// (>=, >, = or a bare version) of the floor or newer. Such a range pins
// without listing the registry's tags.
func rangeFloorAdmitsGrant(versionRange string) bool {
	alternatives := strings.Split(versionRange, "||")
	for _, alt := range alternatives {
		floored := false
		for _, term := range strings.FieldsFunc(alt, func(r rune) bool { return r == ' ' || r == ',' }) {
			bound := strings.TrimLeft(term, ">=v")
			if strings.HasPrefix(term, "<") || strings.HasPrefix(term, "!") || strings.ContainsAny(bound, "~^*xX") {
				continue
			}
			v, err := semver.NewVersion(bound)
			if err == nil && !v.LessThan(workspaceManagerGrantFloor) {
				floored = true
			}
		}
		if !floored {
			return false
		}
	}
	return len(alternatives) > 0
}

// workspaceManagerSource is components.workspace-manager as the install
// reads it: the chart's defaults, platform.valuesFiles over them.
type workspaceManagerSource struct {
	repository, chart, versionRange, filter string
	insecure                                bool
}

// workspaceManagerVersion resolves the workspace-manager's chart version the
// way source-controller resolves the component's OCIRepository: the highest
// tag the range (and the semverFilter) admits; "" for a chart that names no
// such component; the grant floor itself for a range whose floor is at or
// above it, without listing tags. Memoized per source.
func workspaceManagerVersion(cfg *config.Config) (string, error) {
	src := workspacesChartFor(cfg).Manager
	if len(cfg.Platform.ValuesFiles) > 0 {
		overlay, err := helmValuesFiles(cfg.Platform.ValuesFiles...)
		if err != nil {
			return "", err
		}
		src = src.over(overlay)
	}
	if src.repository == "" || src.versionRange == "" {
		return "", nil // a chart without the component: no manager to pin
	}
	if rangeFloorAdmitsGrant(src.versionRange) {
		return workspaceManagerGrantFloor.Original(), nil // no tag below the floor can resolve
	}
	workspaceManagerVersionMu.Lock()
	defer workspaceManagerVersionMu.Unlock()
	if v, ok := workspaceManagerVersionCache[src]; ok {
		return v, nil
	}
	tags, err := listChartTags(strings.TrimSuffix(src.repository, "/")+"/"+cmp.Or(src.chart, workspaceManagerMCPServer), src.insecure)
	if err != nil {
		return "", err
	}
	v, err := pickFilteredTag(tags, src.versionRange, cmp.Or(src.filter, ".*"))
	if err != nil {
		return "", err
	}
	workspaceManagerVersionCache[src] = v
	return v, nil
}

var (
	workspaceManagerVersionMu    sync.Mutex
	workspaceManagerVersionCache = map[workspaceManagerSource]string{}
)

// workspaceManagerSourceOf reads components.workspace-manager from values.
func workspaceManagerSourceOf(values map[string]any) workspaceManagerSource {
	return workspaceManagerSource{}.over(values)
}

// over is the source with what values set of components.workspace-manager.
func (s workspaceManagerSource) over(values map[string]any) workspaceManagerSource {
	components, _ := values[valuesComponents].(map[string]any)
	wm, _ := components[workspaceManagerMCPServer].(map[string]any)
	s.repository = cmp.Or(stringValue(wm, "repository"), s.repository)
	s.chart = cmp.Or(stringValue(wm, "chart"), s.chart)
	s.versionRange = cmp.Or(stringValue(wm, valuesVersionRange), s.versionRange)
	s.filter = cmp.Or(stringValue(wm, valuesSemverFilter), s.filter)
	if insecure, ok := wm["insecure"].(bool); ok {
		s.insecure = insecure
	}
	return s
}

// ensureWorkspaceGrantClientID places the pinned instance's OAuth client id
// in its Secret under GitHubClientIDKey, where muster reads the client; the
// fake's Secret carries it from its creation (githubFakeUp). The id is
// public: agentlab writes it and reads no value of the Secret.
func ensureWorkspaceGrantClientID(ctx context.Context, grant *workspaceGrantValues, providers []workspaceProviderValues) error {
	if grant == nil || grant.Secret == config.WorkspaceFakeSecretName {
		return nil
	}
	for _, p := range providers {
		if p.Name != grant.Provider {
			continue
		}
		k, err := labKube()
		if err != nil {
			return err
		}
		patch := fmt.Sprintf(`{"stringData":{%q:%q}}`, config.GitHubClientIDKey, p.ClientID)
		if _, err := k.clientset.CoreV1().Secrets(platformNamespace).Patch(ctx, grant.Secret, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("placing the client id of %s in the Secret %s/%s for muster: %w", p.Name, platformNamespace, grant.Secret, err)
		}
	}
	return nil
}
