package lab

import (
	"cmp"
	"context"
	"fmt"
	"slices"
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

// workspaceManagerRevocationFloor is the first workspace-manager whose chart
// renders the pin's revocation endpoint; an earlier one's schema refuses the
// key.
var workspaceManagerRevocationFloor = semver.MustParse("0.11.0-rc.6")

// workspaceGitLabScopes are the scopes the GitLab OAuth application is
// registered with, which its authorization asks for.
const workspaceGitLabScopes = "api read_user read_repository write_repository"

// The provider endpoints the pin names.
const (
	githubAuthorizePath   = "/login/oauth/authorize"
	githubAccessTokenPath = "/login/oauth/access_token" // #nosec G101 -- a path, not a credential
	githubOAuthIssuer     = "https://github.com/login/oauth"
	gitlabTokenPath       = "/oauth/token" // #nosec G101 -- a path, not a credential
	gitlabRevokePath      = "/oauth/revoke"
)

// workspaceGrantValues is the pin as the values template renders it into
// workspace-manager.muster.mcpServer.auth: the instance, its App's issuer
// identity (the key muster files the person's grant under), the issuer the
// provider puts in its authorization response ("" for one that puts none),
// its endpoints, the RFC 7009 revocation endpoint muster revokes a person's
// grant at on their disconnect ("" for GitHub, which has none, and for a
// manager whose chart does not render it), the scope ("" for a GitHub App)
// and the Secret of its OAuth client.
type workspaceGrantValues struct {
	Provider, Issuer, ExpectedIssuer     string
	AuthorizationEndpoint, TokenEndpoint string
	RevocationEndpoint                   string
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
			g := workspaceGrantOf(p)
			if g.RevocationEndpoint != "" && !workspaceManagerAtLeast(cfg, workspaceManagerRevocationFloor) {
				g.RevocationEndpoint = ""
			}
			return g
		}
	}
	return nil
}

// workspaceProviderAppsFor are the rendered instances muster keeps a
// provider App of each for, side by side, with the registration pinned to
// none (config.WorkspaceProviderApps): the lab renders no pin, and the meta
// chart derives one App per instance with person sign-in
// (agent-platform.workspaceManager.providerApps) once there are two. Nil
// with a pin, for a manager that takes none, and while fewer than two
// instances render.
func workspaceProviderAppsFor(cfg *config.Config, providers []workspaceProviderValues) []string {
	if !cfg.WorkspaceProviderApps() || len(providers) < 2 || !workspaceManagerTakesGrant(cfg) {
		return nil
	}
	names := make([]string, len(providers))
	for i, p := range providers {
		names[i] = p.Name
	}
	return names
}

// workspaceReleasableFor is the instance muster's credential provider
// releases while workspaces are on and no instance renders (a github or
// gitlab instance whose ids or Secret are not in place yet): the pinned
// instance with the issuer agentlab.yaml gives it, github.com's OAuth
// issuer while the App's id is empty. The meta chart derives the
// releasable from the pin and refuses an empty one with Substrate on;
// without an instance there is no pin, and this keeps the render going
// until the configured instance renders; with no pin
// (config.WorkspaceProviderApps) the first instance's. Nil with workspaces
// off.
func workspaceReleasableFor(cfg *config.Config) *workspaceGrantValues {
	if !cfg.WorkspacesEnabled() {
		return nil
	}
	name := cmp.Or(cfg.WorkspaceGrantProvider(), cfg.WorkspaceProviders()[0])
	if gl := cfg.Platform.Workspaces.GitLab; name == config.WorkspaceProviderGitLab && gl.URL != "" {
		return workspaceGrantOf(workspaceProviderValues{Name: name, Kind: providerKindGitLab, URL: gl.URL})
	}
	issuer := githubOAuthIssuer
	if id := cfg.Platform.Workspaces.GitHub.AppID; id != "" {
		issuer = workspaceGrantOf(workspaceProviderValues{Name: name, Kind: providerKindGitHub, AppID: id}).Issuer
	}
	return &workspaceGrantValues{Provider: name, Issuer: issuer}
}

// workspaceGrantOf is the pin to one instance's App.
func workspaceGrantOf(p workspaceProviderValues) *workspaceGrantValues {
	g := &workspaceGrantValues{Provider: p.Name, Secret: p.Secret}
	switch p.Kind {
	case providerKindGitLab:
		base := strings.TrimSuffix(p.URL, "/")
		g.Issuer, g.AuthorizationEndpoint, g.TokenEndpoint, g.Scopes = base, base+"/oauth/authorize", base+gitlabTokenPath, workspaceGitLabScopes
		g.RevocationEndpoint = base + gitlabRevokePath
	default:
		// github.com: the App is known by its App id (agentlab.yaml
		// records no slug), and GitHub names its issuer in the response.
		g.Issuer, g.ExpectedIssuer = "https://github.com/apps/"+p.AppID, githubOAuthIssuer
		g.AuthorizationEndpoint, g.TokenEndpoint = "https://github.com"+githubAuthorizePath, "https://github.com"+githubAccessTokenPath
	}
	return g
}

// workspaceManagerTakesGrant says whether the workspace-manager the install
// resolves is 0.10.0 or newer; one that cannot be resolved is a note and no
// pin.
func workspaceManagerTakesGrant(cfg *config.Config) bool {
	return workspaceManagerAtLeast(cfg, workspaceManagerGrantFloor)
}

// workspaceManagerAtLeast says whether the workspace-manager the install
// resolves is floor or newer; one that cannot be resolved is a note and
// false.
func workspaceManagerAtLeast(cfg *config.Config, floor *semver.Version) bool {
	version, err := workspaceManagerVersion(cfg, floor)
	if err != nil {
		note("the workspace-manager's chart version could not be resolved (%v): the lab renders nothing that needs %s or newer", err, floor.Original())
		return false
	}
	if version == "" {
		return false
	}
	v, err := semver.NewVersion(version)
	return err == nil && !v.LessThan(floor)
}

// rangeFloorAdmitsGrant is rangeFloorAdmits at the grant floor.
func rangeFloorAdmitsGrant(versionRange string) bool {
	return rangeFloorAdmits(versionRange, workspaceManagerGrantFloor)
}

// rangeFloorAdmits says whether every version the range admits is at or
// above floor: each of its ||-alternatives carries a lower bound (>=, >, = or
// a bare version) of the floor or newer. Such a range decides without
// listing the registry's tags.
func rangeFloorAdmits(versionRange string, floor *semver.Version) bool {
	alternatives := strings.Split(versionRange, "||")
	for _, alt := range alternatives {
		floored := false
		for _, term := range strings.FieldsFunc(alt, func(r rune) bool { return r == ' ' || r == ',' }) {
			bound := strings.TrimLeft(term, ">=v")
			if strings.HasPrefix(term, "<") || strings.HasPrefix(term, "!") || strings.ContainsAny(bound, "~^*xX") {
				continue
			}
			v, err := semver.NewVersion(bound)
			if err == nil && !v.LessThan(floor) {
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
// such component; floor itself for a range whose floor is at or above it,
// without listing tags. Memoized per source.
func workspaceManagerVersion(cfg *config.Config, floor *semver.Version) (string, error) {
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
	if rangeFloorAdmits(src.versionRange, floor) {
		return floor.Original(), nil // no tag below the floor can resolve
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

// ensureWorkspaceGrantClientIDs places the OAuth client id of each instance
// muster signs a person in to (the pinned one, or every App's with no pin)
// in its Secret under GitHubClientIDKey, where muster reads the client. The
// id is public: agentlab writes it and reads no value of the Secret.
func ensureWorkspaceGrantClientIDs(ctx context.Context, instances []string, providers []workspaceProviderValues) error {
	if len(instances) == 0 {
		return nil
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	for _, p := range providers {
		if !slices.Contains(instances, p.Name) {
			continue
		}
		patch := fmt.Sprintf(`{"stringData":{%q:%q}}`, config.GitHubClientIDKey, p.ClientID)
		if _, err := k.clientset.CoreV1().Secrets(platformNamespace).Patch(ctx, p.Secret, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("placing the client id of %s in the Secret %s/%s for muster: %w", p.Name, platformNamespace, p.Secret, err)
		}
	}
	return nil
}
