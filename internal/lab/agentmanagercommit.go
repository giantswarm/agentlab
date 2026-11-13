package lab

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/Masterminds/semver/v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// agent-manager's commit mode in the lab (docs/agents.md "Commit mode"):
// create_agent, update_agent and delete_agent with mode commit open a pull
// request as the person in the repository that owns the agents. The lab
// gives agent-manager the person's GitHub App user grant the way an
// installation does without pinning its MCP registration to an App
// (agent-manager's github.tokenExchange): agent-manager presents the
// person's forwarded ID token to muster's RFC 8693 broker
// (muster.oauth.server.tokenExchangeBroker) as the confidential client
// agent-manager, and muster releases the person's own grant of the
// workspaces' github instance App — the grant the person made once for the
// workspace-manager, filed under that App's issuer — never a token of its
// own. The audience it asks for is the name muster keeps the App under, the
// provider App the MCP registration lists (auth.providerApps). Apply mode,
// every read and every other proof keep the ID token: a person without the
// grant gets grant_required on a commit and nothing else changes. The App
// is installed with contents and pull-request write on the instance's proof
// repositories, the first of which is the commit proofs' target
// (`agentlab backstage-test`, backstagetest_commit.go).
//
// The lab wires it while the platform runs agents, the github instance
// renders, muster keeps that instance's App as the provider App github (one
// provider App per instance, or the workspace-manager's pin with devctl on)
// and the agent-manager the install resolves takes github.tokenExchange
// (agentManagerCommitFloor and newer).

const (
	// agentManagerCommitClientID is muster's confidential broker client
	// agent-manager authenticates as.
	agentManagerCommitClientID = "agent-manager"
	// AgentManagerCommitSecretName is the Secret of the broker client in the
	// platform namespace: client-id and client-secret, read by muster's
	// brokerClients and by agent-manager.
	AgentManagerCommitSecretName = "agent-manager-github-exchange" // #nosec G101 -- Secret NAME, not a credential
	// agentManagerCommitDir keeps the client's secret, generated once, beside
	// the lab's other credentials.
	agentManagerCommitDir = "certs/agent-manager-commit" // #nosec G101 -- a directory, not a credential
	// agentManagerCommitTokenPath is muster's RFC 8693 endpoint.
	agentManagerCommitTokenPath = "/oauth/token" // #nosec G101 -- a path, not a credential
)

// agentManagerCommitFloor is the first agent-manager whose chart takes
// github.tokenExchange.
var agentManagerCommitFloor = semver.MustParse("3.0.0-rc.10")

// agentManagerCommitValues is what the values template renders for commit
// mode: agent-manager's exchange client (muster's public token endpoint,
// which pods reach through the edge, and the lab CA it is served under; the
// client id from its Secret) and muster's broker (the client, its one
// target named after the provider App, the grant's issuer, and the lab Dex
// as the trusted issuer of the subject token).
type agentManagerCommitValues struct {
	TokenEndpoint, ClientID, Audience, Secret, CASecret string
	// Instance and GrantIssuer are the workspaces instance whose App grant
	// muster releases and that App's issuer key.
	Instance, GrantIssuer string
	// SubjectIssuer is the lab Dex; JWKSURL and JWKSHost where muster reads
	// its keys in the cluster.
	SubjectIssuer, JWKSURL, JWKSHost string
	Proof                            bool
}

type managerGitHubClient struct {
	Name, Secret string
}

func (c *agentManagerCommitValues) BrokerClients() []managerGitHubClient {
	clients := []managerGitHubClient{{agentManagerCommitClientID, AgentManagerCommitSecretName}}
	if c.Proof {
		for _, name := range []string{clusterManager, modelManagerMCPServer, componentSkillManager} {
			clients = append(clients, managerGitHubClient{name, name + "-github-exchange"})
		}
	}
	return clients
}

// musterKeepsProviderApp says whether muster keeps a provider App named
// instance, the name agent-manager's grant_required answer and its
// registration's auth.providerApps carry, from which muster builds the
// sign-in link: one provider App per instance (apps), or the pinned github
// App kept as the provider App github beside the pin while devctl is on.
// The pin alone keeps no provider App, and muster answers an App it does
// not keep with an error instead of a link.
func musterKeepsProviderApp(cfg *config.Config, grant *workspaceGrantValues, apps []string, instance string) bool {
	if slices.Contains(apps, instance) {
		return true
	}
	return grant != nil && grant.Provider == instance && instance == config.WorkspaceProviderGitHub && cfg.DevctlEnabled()
}

// agentManagerCommitFor is the commit wiring for the rendered instances,
// nil while any precondition is missing.
func agentManagerCommitFor(cfg *config.Config, providers []workspaceProviderValues, grant *workspaceGrantValues, apps []string) *agentManagerCommitValues {
	if !cfg.Platform.Enabled || !cfg.Platform.Agents || cfg.LegacyChart() {
		return nil
	}
	instance := config.WorkspaceProviderGitHub
	var gh *workspaceProviderValues
	for i := range providers {
		if providers[i].Name == instance && providers[i].Kind == providerKindGitHub {
			gh = &providers[i]
		}
	}
	if gh == nil {
		return nil
	}
	if !musterKeepsProviderApp(cfg, grant, apps, instance) {
		return nil
	}
	if !agentManagerTakesCommit(cfg) {
		return nil
	}
	t := &tmplData{Config: cfg}
	jwksHost := t.DexJWKSHost()
	return &agentManagerCommitValues{
		Proof:         cfg.Platform.ManagerGitHubProof,
		TokenEndpoint: cfg.MusterBaseURL() + agentManagerCommitTokenPath,
		ClientID:      agentManagerCommitClientID,
		Audience:      instance,
		Secret:        AgentManagerCommitSecretName,
		CASecret:      labCASecret,
		Instance:      instance,
		GrantIssuer:   workspaceGrantOf(*gh).Issuer,
		SubjectIssuer: cfg.Issuer(),
		JWKSURL:       fmt.Sprintf("https://%s:%d/dex/keys", jwksHost, t.DexJWKSPort()),
		JWKSHost:      jwksHost,
	}
}

// agentManagerTakesCommit says whether the agent-manager the install
// resolves is agentManagerCommitFloor or newer; one that cannot be
// resolved is a note and false.
func agentManagerTakesCommit(cfg *config.Config) bool {
	if cfg.Platform.ManagerGitHubProof {
		return true
	}
	version, err := componentVersion(cfg, agentManagerMCPServer, workspacesChartFor(cfg).AgentManager, agentManagerCommitFloor)
	if err != nil {
		note("agent-manager's chart version could not be resolved (%v): commit mode stays off", err)
		return false
	}
	if version == "" {
		return false
	}
	v, err := semver.NewVersion(version)
	return err == nil && !v.LessThan(agentManagerCommitFloor)
}

// agentManagerCommitUp places the broker client's Secret, its secret
// generated once and kept, ahead of the install that names it.
func agentManagerCommitUp(configuration *agentManagerCommitValues) error {
	for _, client := range configuration.BrokerClients() {
		files, err := ensureManagerGitHubFiles(client.Name)
		if err != nil {
			return err
		}
		if err := ensureSecretFromFiles(platformNamespace, client.Secret, files); err != nil {
			return err
		}
	}
	return nil
}

// ensureManagerGitHubFiles keeps each broker client's credentials in its own directory.
func ensureManagerGitHubFiles(clientID string) (map[string]string, error) {
	directory := filepath.Join("certs", clientID+"-commit")
	if clientID == agentManagerCommitClientID {
		directory = agentManagerCommitDir
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, err
	}
	id := filepath.Join(directory, config.GitHubClientIDKey)
	if err := os.WriteFile(id, []byte(clientID), 0o600); err != nil {
		return nil, err
	}
	secret := filepath.Join(directory, config.GitHubClientSecretKey)
	if _, err := os.Stat(secret); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(secret, []byte(randomHex(32)), 0o600); err != nil {
			return nil, err
		}
		fmt.Printf("Generated %s's broker client secret in %s\n", clientID, directory)
	} else if err != nil {
		return nil, err
	}
	return map[string]string{config.GitHubClientIDKey: id, config.GitHubClientSecretKey: secret}, nil
}

// agentManagerCommitHint is the platform-up summary line of commit mode.
func agentManagerCommitHint(c *agentManagerCommitValues) string {
	if c == nil {
		return "  agent-manager offers no commit mode (it needs platform.workspaces' github instance, muster keeping its App as the provider App github, and agent-manager " + agentManagerCommitFloor.Original() + " or newer)."
	}
	return fmt.Sprintf("  agent-manager commit mode: pull requests as the person, on their grant of the %s instance's App (muster's broker at %s).\n"+
		"  Proof: `agentlab backstage-test` (its commit stage).", c.Instance, c.TokenEndpoint)
}
