package lab

import (
	"context"
	"fmt"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

// The agent-manager surface of organizations-test: every agent-manager tool
// takes an organization by its name, the name of its Organization, and
// never by its namespace (giantswarm/giantswarm#39503). A member's call
// with their organization's namespace, a call naming the other organization
// and a call with both the organization and a namespace are refused, with
// nothing written; the twin, the member's get_agent of their own agent by
// their organization's name, is answered and names the organization.
const (
	// organizationsTestToolAgent is the name of each refused create_agent;
	// it never exists.
	organizationsTestToolAgent = "agentlab-organizations-tool"
	// organizationsTestMissing names an organization that does not exist:
	// what the answer about the other organization is compared with.
	organizationsTestMissing = "agentlab-organizations-missing"
	// The codes agent-manager's refusals start with.
	codeInvalidRequest = "invalid_request"
	codeNotFound       = "not_found"
	// organizationDecidesNamespace is agent-manager's refusal of namespace
	// together with organization and no cluster (internal/api/place.go).
	organizationDecidesNamespace = "the organization decides the namespace"
)

// organizationsAgentManager is what the agent-manager surface works with:
// the two organizations, their namespaces and their members.
type organizationsAgentManager struct {
	cfg               *config.Config
	names, namespaces [2]string
	users             [2]*config.User
}

// checks are the agent-manager surface's negative tests.
func (m *organizationsAgentManager) checks() []organizationCheck {
	return []organizationCheck{
		{surface: surfaceAgentManager, boundary: "a member's create_agent with their organization's namespace and no organization is refused as an invalid request naming the organization, nothing written",
			run: m.namespaceRefused},
		{surface: surfaceAgentManager, boundary: "a member's create_agent naming the other organization is answered not_found, as for an organization that does not exist, nothing written",
			run: m.otherAsMissing},
		{surface: surfaceAgentManager, boundary: "a create_agent with organization and namespace and no cluster is refused as an invalid request, nothing written",
			run: m.organizationWithNamespaceRefused},
	}
}

// login opens a muster session as the member and checks the twin: their
// get_agent of their own organization's agent by the organization's name is
// answered with that organization and its namespace.
func (m *organizationsAgentManager) login(i int) (*musterSession, error) {
	u := m.users[i]
	token, err := passwordGrant(m.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, u.Email, u.Password, musterLoginScopes)
	if err != nil {
		return nil, err
	}
	s, err := openMusterSession(m.cfg, token, "organizations-test")
	if err != nil {
		return nil, err
	}
	var got struct {
		Name         string `json:"name"`
		Namespace    string `json:"namespace"`
		Organization string `json:"organization"`
	}
	if err := s.callServerJSON(agentManagerToolPrefix+"get_agent", map[string]any{organizationArg: m.names[i], nameKey: organizationsTestAgent}, &got); err != nil {
		return nil, fmt.Errorf("the twin: %s's get_agent %s of organization %s: %w", u.Email, organizationsTestAgent, m.names[i], err)
	}
	if got.Name != organizationsTestAgent || got.Organization != m.names[i] || got.Namespace != m.namespaces[i] {
		return nil, fmt.Errorf("the twin: %s's get_agent %s of organization %s answered agent %q of organization %q in %q, want organization %s in %s",
			u.Email, organizationsTestAgent, m.names[i], got.Name, got.Organization, got.Namespace, m.names[i], m.namespaces[i])
	}
	return s, nil
}

// createArgs are a valid create_agent of the proof's tool agent, without
// where it goes.
func createArgs(place map[string]any) map[string]any {
	args := map[string]any{nameKey: organizationsTestToolAgent, modelConfigKey: organizationModel, toolsetKey: []string{presetNone}}
	for k, v := range place {
		args[k] = v
	}
	return args
}

// refusedAs reads a tool's answer as agent-manager's refusal with code and,
// when want is set, a message holding it, and returns its text.
func refusedAs(env *toolEnvelope, code, want string) (string, error) {
	text := envelopeText(env)
	switch {
	case !env.IsError:
		return "", fmt.Errorf("answered, not refused: %s", excerpt(text, 300))
	case !strings.Contains(text, code+": "):
		return "", fmt.Errorf("refused with %q, not agent-manager's %s", excerpt(text, 300), code)
	case want != "" && !strings.Contains(text, want):
		return "", fmt.Errorf("refused with %q, which does not say %q", excerpt(text, 300), want)
	}
	return text, nil
}

// nothingCreated fails when the proof's tool agent has a HelmRelease in ns.
func nothingCreated(ns string) error {
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	exists, err := objectExists(ctx, gvr, ns, organizationsTestToolAgent)
	switch {
	case err != nil:
		return err
	case exists:
		return fmt.Errorf("HelmRelease %s/%s exists after the refusal", ns, organizationsTestToolAgent)
	}
	return nil
}

// namespaceRefused: alice's create_agent with her organization's namespace
// as namespace and no organization is refused as an invalid request that
// names the organization to pass, and nothing is written.
func (m *organizationsAgentManager) namespaceRefused() (string, error) {
	alice, a := m.users[0], m.namespaces[0]
	s, err := m.login(0)
	if err != nil {
		return "", err
	}
	env, err := s.callToolEnvelope(agentManagerToolPrefix+"create_agent", createArgs(map[string]any{namespaceKey: a}))
	if err != nil {
		return "", err
	}
	text, err := refusedAs(env, codeInvalidRequest, fmt.Sprintf("pass organization %q", m.names[0]))
	if err != nil {
		return "", fmt.Errorf("%s's create_agent with namespace %s: %w", alice.Email, a, err)
	}
	if err := nothingCreated(a); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s's create_agent %s with namespace %s refused (%s), no HelmRelease written; the twin, get_agent %s with organization %s, answered for %s in %s",
		alice.Email, organizationsTestToolAgent, a, excerpt(text, 200), organizationsTestAgent, m.names[0], m.names[0], a), nil
}

// otherAsMissing: alice's create_agent naming bob's organization is
// answered as for an organization that does not exist, not_found, and
// nothing is written in bob's namespace.
func (m *organizationsAgentManager) otherAsMissing() (string, error) {
	alice, b := m.users[0], m.namespaces[1]
	s, err := m.login(0)
	if err != nil {
		return "", err
	}
	tool := agentManagerToolPrefix + "create_agent"
	answer, err := answeredAsMissing(s, tool, tool, createArgs(map[string]any{organizationArg: m.names[1]}), createArgs(map[string]any{organizationArg: organizationsTestMissing}), m.names[1], organizationsTestMissing)
	if err != nil {
		return "", fmt.Errorf("%s's %w", alice.Email, err)
	}
	if !strings.Contains(answer, codeNotFound+": ") {
		return "", fmt.Errorf("%s's create_agent in organization %s was refused with %q, not agent-manager's %s", alice.Email, m.names[1], answer, codeNotFound)
	}
	if err := nothingCreated(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s's create_agent %s in organization %s answered as for organization %s, which does not exist (%s), no HelmRelease written in %s",
		alice.Email, organizationsTestToolAgent, m.names[1], organizationsTestMissing, answer, b), nil
}

// organizationWithNamespaceRefused: alice's create_agent with her
// organization and its namespace, without cluster, is refused as an
// invalid request, and nothing is written.
func (m *organizationsAgentManager) organizationWithNamespaceRefused() (string, error) {
	alice, a := m.users[0], m.namespaces[0]
	s, err := m.login(0)
	if err != nil {
		return "", err
	}
	env, err := s.callToolEnvelope(agentManagerToolPrefix+"create_agent", createArgs(map[string]any{organizationArg: m.names[0], namespaceKey: a}))
	if err != nil {
		return "", err
	}
	text, err := refusedAs(env, codeInvalidRequest, organizationDecidesNamespace)
	if err != nil {
		return "", fmt.Errorf("%s's create_agent with organization %s and namespace %s: %w", alice.Email, m.names[0], a, err)
	}
	if err := nothingCreated(a); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s's create_agent %s with organization %s and namespace %s refused (%s), no HelmRelease written", alice.Email, organizationsTestToolAgent, m.names[0], a, excerpt(text, 200)), nil
}
