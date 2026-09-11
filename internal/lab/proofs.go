package lab

// The agent proofs run on kagent API v2, the one line the platform ships:
// agents are Generic chart 1.x HelmReleases whose render is an AgentTemplate
// admitted by the platform Harness and run as a Substrate actor, their turns
// AgentInstances through the edge (agent.go, agenttemplate.go, kagentapi.go).
// The stable and the dev channel run the same proofs.

// The agent-manager tool arguments and report fields the proofs share.
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
