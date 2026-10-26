package lab

import (
	"fmt"

	"github.com/giantswarm/agentlab/internal/config"
)

// proveAgentsTestPlugins is the example package on the kagent Harness, the
// row coding-test proves on the coding Harnesses: update_agent binds it with
// its skill selected, the agent boots a new revision and a new Session's
// agent lists that skill; with the selection emptied it boots again and
// lists it no more.
func proveAgentsTestPlugins(cfg *config.Config, session *musterSession, token string) error {
	for _, selection := range [][]string{{pluginExampleSkill}, {}} {
		step("%supdate_agent: plugins [%s @ %.12s selecting %v]; a new revision Ready on %s, a new Session's agent lists %v of the package's skills", agentManagerToolPrefix, pluginExampleURL, pluginExampleCommit, selection, kagentHarness, selection)
		r, err := readAgentReadinessOn(agentsTestAgent, kagentHarness)
		if err != nil {
			return err
		}
		if r.agent == nil {
			return fmt.Errorf("agent %s has no Agent to update", agentsTestAgent)
		}
		updated, err := agentManagerUpdate(session, map[string]any{nameKey: agentsTestAgent, pluginsKey: []agentPlugin{examplePlugin(selection)}})
		if err != nil {
			return err
		}
		note("update_agent changed %v", updated.Changed)
		if !waitPluginSelection(agentsTestAgent, selection) {
			return fmt.Errorf("AgentTemplate %s does not carry %s with %v selected a minute after update_agent", agentsTestAgent, pluginExampleURL, selection)
		}
		if _, err := waitReadyOnNewRevision(agentsTestAgent, kagentHarness, r.agent.Status.LatestSuccessfulRevision, agentsTestReadyTimeout); err != nil {
			return err
		}
		reply, err := firstTurnAs(cfg, agentsTestAgent, token, codingPluginSkillsPrompt(codingRuntime{}, false))
		if err != nil {
			return err
		}
		verdict, err := judgePluginSkills(parseProbeLines(reply)["skills"], selection, []string{pluginExampleSkill})
		if err != nil {
			return fmt.Errorf("on Harness %s: %w; the reply: %q", kagentHarness, err, excerpt(reply, 600))
		}
		note("%s", verdict)
	}
	return nil
}
