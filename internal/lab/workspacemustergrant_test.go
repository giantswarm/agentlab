package lab

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// testGrantClientID is an App's client id in the pin's tests.
const testGrantClientID = "Iv1.pinned"

// testGrantAppID is that App's id, the issuer's last segment.
const testGrantAppID = "4711"

// stubWorkspaceManagerTags answers the workspace-manager repository's tag
// listing with tags, the version memo cleared before and after.
func stubWorkspaceManagerTags(t *testing.T, tags ...string) {
	t.Helper()
	clear := func() {
		workspaceManagerVersionMu.Lock()
		workspaceManagerVersionCache = map[workspaceManagerSource]string{}
		workspaceManagerVersionMu.Unlock()
	}
	clear()
	listChartTags = func(url string, _ bool) ([]string, error) {
		if !strings.HasSuffix(url, "/workspace-manager") {
			t.Errorf("listed the tags of %s, want the workspace-manager chart", url)
		}
		return tags, nil
	}
	t.Cleanup(func() { listChartTags = helmChartTagsFrom; clear() })
}

// The lab pins a manager only from 0.10.0 on, resolved the way Flux
// resolves the component's range: the chart's default range resolving to a
// 0.10 release pins, one resolving to 0.9 does not, and a range in
// platform.valuesFiles admitting a release candidate wins over the chart's.
func TestWorkspaceManagerTakesGrant(t *testing.T) {
	source := workspaceManagerSource{repository: "oci://gsoci.azurecr.io/charts/giantswarm", chart: "workspace-manager", versionRange: ">=0.5.0 <1.0.0"}
	stubWorkspacesChart(t, chartWorkspaces{Carries: true, Organizations: true, Manager: source})

	stubWorkspaceManagerTags(t, "0.9.0", "0.10.0-rc.5")
	if workspaceManagerTakesGrant(workspaceLab("")) {
		t.Error("a range resolving to 0.9.0 (the rc admitted by no floor of -0) pins the manager")
	}

	stubWorkspaceManagerTags(t, "0.9.0", "0.10.0")
	if !workspaceManagerTakesGrant(workspaceLab("")) {
		t.Error("a range resolving to 0.10.0 does not pin the manager")
	}

	stubWorkspaceManagerTags(t, "0.9.0", "0.10.0-rc.5")
	overlay := filepath.Join(t.TempDir(), "rc.yaml")
	if err := os.WriteFile(overlay, []byte("components:\n  workspace-manager:\n    versionRange: 0.10.0-rc.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := workspaceLab("")
	cfg.Platform.ValuesFiles = []string{overlay}
	if !workspaceManagerTakesGrant(cfg) {
		t.Error("platform.valuesFiles' range of 0.10.0-rc.5 does not pin the manager")
	}

	floored := source
	floored.versionRange = ">=0.10.0-rc.5 <1.0.0"
	stubWorkspacesChart(t, chartWorkspaces{Carries: true, Organizations: true, Manager: floored})
	stubWorkspaceManagerTags(t)
	listChartTags = func(url string, _ bool) ([]string, error) {
		t.Errorf("a range floored at 0.10.0-rc.5 listed the tags of %s", url)
		return nil, nil
	}
	if !workspaceManagerTakesGrant(workspaceLab("")) {
		t.Error("a range floored at 0.10.0-rc.5 does not pin the manager")
	}

	stubWorkspacesChart(t, chartWorkspaces{Carries: true})
	if workspaceManagerTakesGrant(workspaceLab("")) {
		t.Error("a chart without the component pins a manager")
	}
}

// A range pins without a tag listing only when every alternative's lower
// bound is at or above 0.10.0-0.
func TestRangeFloorAdmitsGrant(t *testing.T) {
	for r, want := range map[string]bool{
		">=0.10.0-rc.5 <1.0.0":    true,
		">=0.10.0":                true,
		"0.10.0-rc.5":             true,
		"=0.11.0":                 true,
		">= 0.10.0, < 1.0.0":      true,
		">=0.5.0 <1.0.0":          false,
		">0.9.0":                  false,
		"<1.0.0":                  false,
		"~0.10.0":                 false,
		">=0.10.0 || >=0.9.0":     false,
		">=0.10.0 || 0.12.0-rc.1": true,
	} {
		if got := rangeFloorAdmitsGrant(r); got != want {
			t.Errorf("rangeFloorAdmitsGrant(%q) = %v, want %v", r, got, want)
		}
	}
}

// The pin per instance kind, its issuer <url>/apps/<App id> as the meta
// chart derives it: the lab's GitHub and its own endpoints with no
// response issuer, github.com with GitHub's, a GitLab with its scopes; the
// OAuth client is always the instance's Secret.
func TestWorkspaceGrantOf(t *testing.T) {
	const lab, gitlab = "https://github.lab.example", "https://gitlab.example.com"
	for _, c := range []struct {
		p    workspaceProviderValues
		want workspaceGrantValues
	}{
		{workspaceProviderValues{Name: config.WorkspaceProviderFake, Kind: providerKindGitHub, URL: lab, AppID: testGrantAppID, ClientID: testGrantClientID, Secret: config.WorkspaceFakeSecretName},
			workspaceGrantValues{Provider: config.WorkspaceProviderFake, Issuer: lab + "/apps/" + testGrantAppID,
				AuthorizationEndpoint: lab + githubAuthorizePath, TokenEndpoint: lab + githubAccessTokenPath, Secret: config.WorkspaceFakeSecretName}},
		{workspaceProviderValues{Name: config.WorkspaceProviderGitHub, Kind: providerKindGitHub, AppID: testGrantAppID, ClientID: testGrantClientID, Secret: config.WorkspaceGitHubSecretName},
			workspaceGrantValues{Provider: config.WorkspaceProviderGitHub, Issuer: "https://github.com/apps/" + testGrantAppID, ExpectedIssuer: githubOAuthIssuer,
				AuthorizationEndpoint: "https://github.com" + githubAuthorizePath, TokenEndpoint: "https://github.com" + githubAccessTokenPath, Secret: config.WorkspaceGitHubSecretName}},
		{workspaceProviderValues{Name: config.WorkspaceProviderGitLab, Kind: providerKindGitLab, URL: gitlab + "/", ClientID: testGrantClientID, Secret: config.WorkspaceGitLabSecretName},
			workspaceGrantValues{Provider: config.WorkspaceProviderGitLab, Issuer: gitlab, AuthorizationEndpoint: gitlab + "/oauth/authorize",
				TokenEndpoint: gitlab + gitlabTokenPath, RevocationEndpoint: gitlab + gitlabRevokePath, Scopes: workspaceGitLabScopes, Secret: config.WorkspaceGitLabSecretName}},
	} {
		if got := *workspaceGrantOf(c.p); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.p.Name, got, c.want)
		}
	}
}

// A pinned manager's values carry muster.mcpServer.auth naming the instance
// and its authorization server, and no token exchange; an unpinned one keeps
// the exchange and no pin.
func TestWorkspaceGrantValues(t *testing.T) {
	stubWorkspacesProbe(t, true)
	grant := workspaceGrantOf(workspaceProviderValues{Name: config.WorkspaceProviderFake, Kind: providerKindGitHub, URL: "https://github.lab.example", ClientID: testGrantClientID, Secret: config.WorkspaceFakeSecretName})
	values, raw, _, _ := renderWorkspaceTemplates(t, workspaceLab(""), func(d *tmplData) { d.WorkspaceGrant = grant })
	wm, _ := workspaceManagerBlock(t, values)
	muster, _ := wm["muster"].(map[string]any)
	server, _ := muster["mcpServer"].(map[string]any)
	auth, _ := server["auth"].(map[string]any)
	as, _ := auth["authorizationServer"].(map[string]any)
	if auth["provider"] != config.WorkspaceProviderFake || as["issuer"] != grant.Issuer || as["expectedIssuer"] != "" || as["scopes"] != "" ||
		as["authorizationEndpoint"] != grant.AuthorizationEndpoint || as["tokenEndpoint"] != grant.TokenEndpoint || as["grantScope"] != "subject" {
		t.Errorf("workspace-manager.muster.mcpServer.auth = %v", auth)
	}
	if ref, _ := as["clientCredentialsSecretRef"].(map[string]any); ref["name"] != config.WorkspaceFakeSecretName {
		t.Errorf("clientCredentialsSecretRef = %v, want %s", as["clientCredentialsSecretRef"], config.WorkspaceFakeSecretName)
	}
	if _, ok := wm["tokenExchange"]; ok {
		t.Errorf("a pinned manager's values carry a token exchange:\n%s", raw)
	}

	values, _, _, _ = renderWorkspaceTemplates(t, workspaceLab(""), nil)
	wm, _ = workspaceManagerBlock(t, values)
	if _, ok := wm["muster"]; ok {
		t.Errorf("an unpinned manager's values carry a pin: %v", wm["muster"])
	}
	if _, ok := wm["tokenExchange"]; !ok {
		t.Error("an unpinned manager's values lost the token exchange")
	}
}

// A GitLab pin names GitLab's revocation endpoint only for a manager whose
// chart renders it (0.11.0-rc.6 and newer, resolved like the grant floor);
// an earlier chart's schema refuses the key, so its pin goes without.
func TestWorkspaceGrantForRevocationEndpoint(t *testing.T) {
	source := workspaceManagerSource{repository: "oci://gsoci.azurecr.io/charts/giantswarm", chart: "workspace-manager", versionRange: ">=0.5.0 <1.0.0"}
	stubWorkspacesChart(t, chartWorkspaces{Carries: true, Organizations: true, Manager: source})
	const gitlab = "https://gitlab.example.com"
	providers := []workspaceProviderValues{{Name: config.WorkspaceProviderGitLab, Kind: providerKindGitLab, URL: gitlab, ClientID: testGrantClientID, Secret: config.WorkspaceGitLabSecretName}}
	cfg := workspaceLab(config.WorkspaceProviderGitLab)

	stubWorkspaceManagerTags(t, "0.10.2", "0.11.0-rc.5")
	if g := workspaceGrantFor(cfg, providers); g == nil || g.RevocationEndpoint != "" {
		t.Errorf("a range resolving to 0.10.2: pin %+v, want one without a revocation endpoint", g)
	}

	stubWorkspaceManagerTags(t, "0.10.2", "0.11.0")
	if g := workspaceGrantFor(cfg, providers); g == nil || g.RevocationEndpoint != gitlab+gitlabRevokePath {
		t.Errorf("a range resolving to 0.11.0: pin %+v, want the revocation endpoint %s", g, gitlab+gitlabRevokePath)
	}

	floored := source
	floored.versionRange = ">=0.11.0-rc.6 <1.0.0"
	stubWorkspacesChart(t, chartWorkspaces{Carries: true, Organizations: true, Manager: floored})
	stubWorkspaceManagerTags(t)
	listChartTags = func(url string, _ bool) ([]string, error) {
		t.Errorf("a range floored at 0.11.0-rc.6 listed the tags of %s", url)
		return nil, nil
	}
	if g := workspaceGrantFor(cfg, providers); g == nil || g.RevocationEndpoint != gitlab+gitlabRevokePath {
		t.Errorf("a range floored at 0.11.0-rc.6: pin %+v, want the revocation endpoint", g)
	}
}

// The values carry the pin's revocation endpoint when it has one, and no key
// when it has none.
func TestWorkspaceGrantValuesRevocationEndpoint(t *testing.T) {
	stubWorkspacesProbe(t, true)
	for _, endpoint := range []string{"https://gitlab.example.com/oauth/revoke", ""} {
		grant := &workspaceGrantValues{Provider: config.WorkspaceProviderFake, Issuer: "https://gitlab.example.com", AuthorizationEndpoint: "https://gitlab.example.com/oauth/authorize",
			TokenEndpoint: "https://gitlab.example.com/oauth/token", RevocationEndpoint: endpoint, Secret: config.WorkspaceFakeSecretName}
		values, raw, _, _ := renderWorkspaceTemplates(t, workspaceLab(""), func(d *tmplData) { d.WorkspaceGrant = grant })
		wm, _ := workspaceManagerBlock(t, values)
		muster, _ := wm["muster"].(map[string]any)
		server, _ := muster["mcpServer"].(map[string]any)
		auth, _ := server["auth"].(map[string]any)
		as, _ := auth["authorizationServer"].(map[string]any)
		got, ok := as["revocationEndpoint"]
		if endpoint == "" && ok || endpoint != "" && got != endpoint {
			t.Errorf("revocationEndpoint %q: rendered %v (present %t)\n%s", endpoint, got, ok, raw)
		}
	}
}
