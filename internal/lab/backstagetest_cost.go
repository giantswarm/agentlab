package lab

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/agentlab/internal/config"
)

// The reported-cost half of `agentlab backstage-test`: a Claude Code agent
// (the lab's claude Harness) answers one turn as the person, its task carries
// the costUsd Claude Code reported under kagent.dev/a2a/usage, and the
// portal's session-usage route — what the Usage tab renders as "Reported
// cost" — reports that cost for the agent. With --keep-cost-fixture the agent
// and its session stay for the usage tab in a browser; the next run removes
// them before it starts.

// BackstageTestCostAgent is the fixture agent on the claude Harness.
const BackstageTestCostAgent = backstageTestAgent + "-cost"

// The fixture's session title and the bound of the session-usage wait: the
// route caches a person's summary for five minutes (the agent-platform
// backend's DEFAULT_CACHE_TTL_MS), and the chat proof read it earlier in the
// run with the same token.
const (
	portalCostSessionName = "agentlab backstage-test (reported cost)"
	portalUsageCacheTTL   = 5 * time.Minute
	portalUsageWait       = portalUsageCacheTTL + time.Minute
)

// costUsdKey is the field of the usage bag the runtime's reported cost is in.
const costUsdKey = "costUsd"

// portalSessionUsage is the part of GET /session-usage the proof reads.
type portalSessionUsage struct {
	Totals struct {
		Turns   int      `json:"turns"`
		CostUsd *float64 `json:"costUsd"`
	} `json:"totals"`
	ByAgent []struct {
		AgentID *string  `json:"agentId"`
		Turns   int      `json:"turns"`
		CostUsd *float64 `json:"costUsd"`
	} `json:"byAgent"`
}

// agentCost is the reported cost session-usage lists for the agent: the
// byAgent entry whose id names it (kagent's agent_id, which carries the
// agent's name), nil while none carries a cost.
func (u portalSessionUsage) agentCost(name string) (id string, cost *float64) {
	for _, a := range u.ByAgent {
		if a.AgentID != nil && agentIDNames(*a.AgentID, name) && a.CostUsd != nil {
			return *a.AgentID, a.CostUsd
		}
	}
	return "", nil
}

// agentIDNames reports whether kagent's agent id is the agent's: name or
// namespace/name, in the Kubernetes form or kagent's Python identifier form
// (ConvertToPythonIdentifier: "/" as "__NS__", "-" as "_").
func agentIDNames(id, name string) bool {
	id = strings.ReplaceAll(strings.ReplaceAll(id, "__NS__", "/"), "_", "-")
	return id == name || strings.HasSuffix(id, "/"+name)
}

// reportedCost is the costUsd the turn's usage bag carries, wherever the task
// holds it: the task's metadata, its status message's, or an agent message of
// its history (where the portal's usage reducer reads it). ok is false when no
// usage bag carries one.
func reportedCost(task *a2aTask) (float64, bool) {
	bags := []map[string]any{task.Metadata}
	if task.Status.Message != nil {
		bags = append(bags, task.Status.Message.Metadata)
	}
	for _, m := range task.History {
		bags = append(bags, m.Metadata)
	}
	for _, bag := range bags {
		usage, _ := bag[usageMetadataKey].(map[string]any)
		if cost, ok := usage[costUsdKey].(float64); ok {
			return cost, true
		}
	}
	return 0, false
}

// sessionUsage is GET /session-usage: the person's usage summary.
func (ps *portalSession) sessionUsage() (*portalSessionUsage, []byte, error) {
	var out portalSessionUsage
	status, raw, err := ps.kagentJSON(http.MethodGet, kagentSessionUsagePath, nil, &out)
	if err != nil {
		return nil, raw, err
	}
	if status != http.StatusOK {
		return nil, raw, fmt.Errorf("GET %s answered %d: %.300s", kagentSessionUsagePath, status, raw)
	}
	return &out, raw, nil
}

// removeCostFixture removes what a run with --keep-cost-fixture left: the
// person's sessions on the fixture agent, then the agent.
func removeCostFixture(primary *portalSession) {
	if mine, err := primary.listSessions(); err == nil {
		for _, s := range mine {
			if s.Agent.Name != BackstageTestCostAgent {
				continue
			}
			note("cleanup: deleting session %s on %s", s.ID, BackstageTestCostAgent)
			if err := primary.deleteSession(s.ID); err != nil {
				note("cleanup: %v", err)
			}
		}
	} else {
		note("cleanup: listing %s's sessions: %v", primary.user.Email, err)
	}
	if agentExists(BackstageTestCostAgent) {
		note("cleanup: removing %s", BackstageTestCostAgent)
		if err := removeAgent(BackstageTestCostAgent); err != nil {
			note("cleanup: %v", err)
		}
	}
}

// proveReportedCost proves the reported cost end to end as the person: the
// fixture agent on the claude Harness, one turn started through
// start_session, its task carrying a non-zero costUsd, and session-usage
// reporting a cost for the agent. A lab that renders no claude Harness skips
// it with a verdict that says so. keep leaves the agent and its session in
// place for the Usage tab in a browser.
func proveReportedCost(cfg *config.Config, primary *portalSession, modelConfig string, keep bool) ([]string, error) {
	if !cfg.CodingHarnessRendered(config.CodingRuntimeClaude) {
		return []string{fmt.Sprintf("SKIP: the reported cost — the lab renders no Harness %s (platform.coding.enabled or platform.workspaces.enabled turns it on)", claudeHarness)}, nil
	}
	email := primary.user.Email
	removeCostFixture(primary)
	if !keep {
		defer removeCostFixture(primary)
	}

	step("The reported cost: the fixture %s on Harness %s answers one turn as %s", BackstageTestCostAgent, claudeHarness, email)
	ctx, cancel := context.WithTimeout(context.Background(), backstageTestReadyTimeout)
	defer cancel()
	if err := waitHarnessReady(ctx, claudeHarness, backstageTestReadyTimeout); err != nil {
		return nil, err
	}
	spec := agentSpec{
		Name: BackstageTestCostAgent, ModelConfig: modelConfig, Harness: claudeHarness,
		DisplayName:   backstageTestDisplayName + " (reported cost)",
		Description:   "Claude Code fixture of `agentlab backstage-test` whose turn reports its cost; deleted by the next run.",
		SystemMessage: "Answer in as few words as the question allows.",
	}
	if _, _, err := readyAgent(helmReleaseWriter{}, spec, backstageTestReadyTimeout); err != nil {
		return nil, err
	}
	agent := portalAgentRef{Namespace: kagentNamespace, Name: spec.Name}
	instance, taskID, err := primary.startSession(agent, portalCostSessionName, uuid.NewString(), portalPongPrompt)
	if err != nil {
		return nil, err
	}
	turn, err := primary.waitTaskSettled(instance.ID, taskID, portalTurnTimeout)
	if err != nil {
		return nil, err
	}
	if turn.Status.State != taskStateCompleted {
		return nil, fmt.Errorf("the turn on %s (task %s) ended %s: %s", spec.Name, taskID, turn.Status.State, excerpt(taskReplyText(turn), 200))
	}
	cost, ok := reportedCost(turn)
	if !ok {
		return nil, fmt.Errorf("the completed turn on Harness %s (task %s) carries no %s.%s — Claude Code's total_cost_usd did not reach the task", claudeHarness, taskID, usageMetadataKey, costUsdKey)
	}
	if cost <= 0 {
		return nil, fmt.Errorf("the turn on Harness %s (task %s) reports %s %v, wanted a non-zero cost — is ModelConfig %s an Anthropic model Claude Code prices?", claudeHarness, taskID, costUsdKey, cost, modelConfig)
	}
	note("session %s, task %s %s: reply %q, %s %.6f", instance.ID, taskID, turn.Status.State, excerpt(taskReplyText(turn), 80), costUsdKey, cost)
	verdicts := []string{fmt.Sprintf("PASS: one turn on %s (Harness %s) as %s completes and its task reports %s %.6f", spec.Name, claudeHarness, email, costUsdKey, cost)}

	step("The Usage tab's Reported cost: GET %s reports a cost for %s (the route caches a person's summary up to %s)", kagentSessionUsagePath, spec.Name, portalUsageCacheTTL)
	var usage *portalSessionUsage
	var raw []byte
	var agentID string
	var agentCost *float64
	reported := waitFor(int(portalUsageWait/pollInterval), pollInterval, func() bool {
		usage, raw, err = primary.sessionUsage()
		if err != nil {
			return false
		}
		agentID, agentCost = usage.agentCost(spec.Name)
		return agentCost != nil
	})
	if err != nil {
		return verdicts, err
	}
	if !reported {
		return verdicts, fmt.Errorf("GET %s lists no reported cost for %s after %s: %.600s", kagentSessionUsagePath, spec.Name, portalUsageWait, raw)
	}
	if *agentCost < cost || usage.Totals.CostUsd == nil || *usage.Totals.CostUsd < *agentCost {
		return verdicts, fmt.Errorf("GET %s reports %s %v for %s and %v in total, wanted at least the turn's %.6f: %.600s", kagentSessionUsagePath, costUsdKey, *agentCost, agentID, usage.Totals.CostUsd, cost, raw)
	}
	note("%s: %s %.6f for %s, %.6f in total", kagentSessionUsagePath, costUsdKey, *agentCost, agentID, *usage.Totals.CostUsd)
	verdicts = append(verdicts, fmt.Sprintf("PASS: %s reports the Reported cost %.6f for %s (%.6f in total) as %s", kagentSessionUsagePath, *agentCost, agentID, *usage.Totals.CostUsd, email))
	if keep {
		note("kept for the browser: agent %s/%s, session %s (%q) of %s — the Usage tab shows Reported cost", kagentNamespace, spec.Name, instance.ID, portalCostSessionName, email)
	}
	return verdicts, nil
}
