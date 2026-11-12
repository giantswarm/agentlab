package lab

import (
	"reflect"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// commitLab is a lab with agents and the github instance, its App's ids in
// agentlab.yaml, and the instance as rendered and pinned.
func commitLab(t *testing.T) (*config.Config, []workspaceProviderValues, *workspaceGrantValues) {
	t.Helper()
	cfg := workspaceGitHubLab()
	cfg.Platform.Enabled = true
	cfg.Platform.Agents = true
	cfg.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: testGrantAppID, ClientID: testGrantClientID}
	gh := workspaceProviderValues{Name: config.WorkspaceProviderGitHub, Kind: providerKindGitHub, AppID: testGrantAppID, ClientID: testGrantClientID, Secret: config.WorkspaceGitHubSecretName}
	return cfg, []workspaceProviderValues{gh}, workspaceGrantOf(gh)
}

// stubAgentManagerTags answers the agent-manager repository's tag listing
// with tags, the chart's components.agent-manager source the default range.
func stubAgentManagerTags(t *testing.T, tags ...string) {
	t.Helper()
	stubWorkspacesChart(t, chartWorkspaces{Carries: true, Organizations: true,
		AgentManager: componentSource{repository: testWorkspaceManagerRepository, versionRange: ">=2.0.0 <4.0.0"}})
	clear := func() {
		componentVersionMu.Lock()
		componentVersionCache = map[componentVersionKey]string{}
		componentVersionMu.Unlock()
	}
	clear()
	listChartTags = func(url string, _ bool) ([]string, error) {
		if !strings.HasSuffix(url, "/"+agentManagerMCPServer) {
			t.Errorf("listed the tags of %s, want the agent-manager chart", url)
		}
		return tags, nil
	}
	t.Cleanup(func() { listChartTags = helmChartTagsFrom; clear() })
}

// Commit mode renders only with every precondition: agents, the github
// instance rendered, muster keeping its App (the pin or a provider App), and
// an agent-manager that takes github.tokenExchange.
func TestAgentManagerCommitFor(t *testing.T) {
	cfg, providers, grant := commitLab(t)

	stubAgentManagerTags(t, "2.22.0", "3.0.0-rc.4")
	if c := agentManagerCommitFor(cfg, providers, grant, nil); c != nil {
		t.Errorf("an agent-manager resolving to 2.22.0 is wired for commit mode: %+v", c)
	}

	stubAgentManagerTags(t, "2.22.0", "3.0.0")
	c := agentManagerCommitFor(cfg, providers, grant, nil)
	if c == nil {
		t.Fatal("an agent-manager resolving to 3.0.0 with the pinned github instance offers no commit mode")
	}
	if c.Audience != config.WorkspaceProviderGitHub {
		t.Errorf("the exchange asks for %s, want the provider App muster keeps the grant under, %s", c.Audience, config.WorkspaceProviderGitHub)
	}
	if c.GrantIssuer != grant.Issuer || c.Instance != config.WorkspaceProviderGitHub {
		t.Errorf("the broker releases %s of %s, want the pinned App's issuer %s", c.GrantIssuer, c.Instance, grant.Issuer)
	}
	if c.TokenEndpoint != cfg.MusterBaseURL()+"/oauth/token" || !strings.HasPrefix(c.TokenEndpoint, "https://") {
		t.Errorf("the exchange endpoint is %s, want muster's public token endpoint over HTTPS", c.TokenEndpoint)
	}
	if c.SubjectIssuer != cfg.Issuer() || c.CASecret != labCASecret || c.Secret != AgentManagerCommitSecretName {
		t.Errorf("unexpected wiring: %+v", c)
	}

	if agentManagerCommitFor(cfg, providers, nil, []string{config.WorkspaceProviderGitHub}) == nil {
		t.Error("muster keeping the github App as a provider App (no pin) offers no commit mode")
	}
	if agentManagerCommitFor(cfg, providers, nil, nil) != nil {
		t.Error("commit mode without muster keeping the github App: no grant could be released")
	}
	gitlab := &workspaceGrantValues{Provider: config.WorkspaceProviderGitLab}
	if agentManagerCommitFor(cfg, providers, gitlab, nil) != nil {
		t.Error("commit mode while muster pins the gitlab instance")
	}
	if agentManagerCommitFor(cfg, nil, grant, nil) != nil {
		t.Error("commit mode without the github instance rendered")
	}
	cfg.Platform.Agents = false
	if agentManagerCommitFor(cfg, providers, grant, nil) != nil {
		t.Error("commit mode without agents")
	}
}

// The values: agent-manager's github.tokenExchange against muster's public
// token endpoint under the lab CA, the client id from its Secret, and
// muster's broker releasing the pinned App's grant to that client alone
// under the provider App's name, the lab Dex its subject tokens' issuer.
// Without commit mode neither block renders.
func TestPlatformValuesAgentManagerCommit(t *testing.T) {
	stubWorkspacesProbe(t, true)
	cfg, providers, grant := commitLab(t)
	commit := &agentManagerCommitValues{
		TokenEndpoint: cfg.MusterBaseURL() + agentManagerCommitTokenPath, ClientID: agentManagerCommitClientID,
		Audience: config.WorkspaceProviderGitHub, Secret: AgentManagerCommitSecretName, CASecret: labCASecret,
		Instance: config.WorkspaceProviderGitHub, GrantIssuer: grant.Issuer,
		SubjectIssuer: cfg.Issuer(), JWKSURL: "https://dex.dex.svc.cluster.local:5556/dex/keys", JWKSHost: "dex.dex.svc.cluster.local",
	}
	render := func(c *agentManagerCommitValues) map[string]any {
		values, _, _, _ := renderWorkspaceTemplates(t, cfg, func(d *tmplData) {
			d.WorkspaceProviders = providers
			d.WorkspaceGrant = grant
			d.AgentManagerCommit = c
		})
		return values
	}

	values := render(commit)
	if got := mapAt(values, "agent-manager", "github")["enabled"]; got != true {
		t.Errorf("agent-manager.github.enabled = %v, want true", got)
	}
	exchange := mapAt(values, "agent-manager", "github", "tokenExchange")
	if exchange == nil {
		t.Fatal("no agent-manager.github.tokenExchange rendered")
	}
	want := map[string]any{"tokenEndpoint": commit.TokenEndpoint, "audience": config.WorkspaceProviderGitHub, "existingSecret": AgentManagerCommitSecretName, "caSecretName": labCASecret}
	if !reflect.DeepEqual(exchange, want) {
		t.Errorf("agent-manager.github.tokenExchange = %v, want %v", exchange, want)
	}
	if pin := mapAt(values, "agent-manager", "github", "authorizationServer"); pin != nil {
		t.Errorf("agent-manager's registration pinned to an App (authorizationServer: %v): every tool would need the grant", pin)
	}
	if mapAt(values, "agent-manager", "githubTokenExchange") != nil {
		t.Error("agent-manager.githubTokenExchange rendered: the chart refuses it")
	}
	server := mapAt(values, "muster", "muster", "oauth", "server")
	broker, _ := server["tokenExchangeBroker"].(map[string]any)
	if broker == nil {
		t.Fatalf("no muster tokenExchangeBroker rendered: %v", server)
	}
	if got := mapAt(broker, "targets", config.WorkspaceProviderGitHub)["grantIssuer"]; got != grant.Issuer {
		t.Errorf("the broker target releases %v, want the pinned App's grant (%s)", got, grant.Issuer)
	}
	if got := mapAt(broker, "clientAudiences")[agentManagerCommitClientID]; len(asSlice(got)) != 1 || asSlice(got)[0] != config.WorkspaceProviderGitHub {
		t.Errorf("the client may request %v, want only %s", got, config.WorkspaceProviderGitHub)
	}
	if got := mapAt(broker, "brokerClients", agentManagerCommitClientID, "clientCredentialsSecretRef")["name"]; got != AgentManagerCommitSecretName {
		t.Errorf("the broker client is seeded from %v, want %s", got, AgentManagerCommitSecretName)
	}
	issuers := asSlice(server["trustedIssuers"])
	if len(issuers) != 1 || issuers[0].(map[string]any)["issuer"] != cfg.Issuer() {
		t.Errorf("trustedIssuers = %v, want the lab Dex alone", issuers)
	}

	values = render(nil)
	if mapAt(values, "agent-manager", "github") != nil {
		t.Error("agent-manager.github rendered without commit mode")
	}
	server = mapAt(values, "muster", "muster", "oauth", "server")
	if server["tokenExchangeBroker"] != nil || server["trustedIssuers"] != nil {
		t.Errorf("muster's broker rendered without commit mode: %v", server)
	}
}

// mapAt walks nested maps by key, nil when a key is missing.
func mapAt(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, _ := m[k].(map[string]any)
		if next == nil {
			return nil
		}
		m = next
	}
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}
