package lab

import (
	"github.com/giantswarm/agentlab/internal/config"
)

// The agent proofs follow the kagent API the cluster serves. The released
// kagent (the stable channel, kagent 0.x) serves agents.kagent.dev: agents
// are Agent CRs delivered as HelmReleases, driven by the *_v1.go proofs.
// kagent API v2 (the dev channel, kagent main) serves no Agent kind: agents
// are AgentTemplates admitted by a Harness and run as Substrate actors,
// their turns AgentInstances over gRPC-Web through the edge — the proofs of
// agentstest.go, toolsetstest.go, kagentapi.go and agenttemplate.go. One
// binary proves both lines, so a stable lab and a dev-channel lab run the
// same `agentlab agents-test`; docs/platform.md "Dev channel".

// The agent-manager tool arguments and report fields both lines share.
const (
	forceKey         = "force"
	displayNameKey   = "displayName"
	systemMessageKey = "systemMessage"
	toolsetKey       = "toolset"
	// verdictReady is get_agent_status's verdict for an agent that runs.
	verdictReady = "ready"
	// agentsTestDisplayName is the display name of agents-test's throwaway agent.
	agentsTestDisplayName = "agentlab agents-test"
)

// AgentsTest is the headless proof that agent-manager acts as the signed-in
// user, on whichever kagent API the cluster serves (agentsTestV1,
// agentsTestV2).
func AgentsTest(cfg *config.Config, email string) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if kagentLegacy() {
		return agentsTestV1(cfg, email)
	}
	return agentsTestV2(cfg, email)
}

// ToolsetsTest is the headless toolset proof, on whichever kagent API the
// cluster serves (toolsetsTestV1, toolsetsTestV2).
func ToolsetsTest(cfg *config.Config, email string, opts ToolsetsTestOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if kagentLegacy() {
		return toolsetsTestV1(cfg, email, opts)
	}
	return toolsetsTestV2(cfg, email, opts)
}

// kagentLegacy reports whether kagent serves agents.kagent.dev — the released
// line — and says which proofs run. The lab's cluster kubeconfig must be in
// place (useClusterKubeconfig).
func kagentLegacy() bool {
	if agentCRDServed() {
		note("kagent serves %s (the released line): agents as HelmReleases, the 0.x proofs", agentCRD)
		return true
	}
	note("kagent serves no %s: kagent API v2 (%s AgentTemplates on a Harness, Substrate actors), the v2 proofs", agentCRD, agentTemplateAPIVersion)
	return false
}

// agentTurnAs drives one turn on an agent as the user whose Dex id_token is
// given — the way the portal's session chat does — on whichever kagent API
// the cluster serves: A2A through the kagent UI route on the released line
// (agentTurnAsV1), an AgentInstance over gRPC-Web through the edge on kagent
// API v2 (agentTurnAsV2). Shared steps of the proofs call this one.
func agentTurnAs(cfg *config.Config, name, token, prompt string) (string, error) {
	if agentCRDServed() {
		return agentTurnAsV1(cfg, name, token, prompt)
	}
	return agentTurnAsV2(cfg, name, token, prompt)
}
