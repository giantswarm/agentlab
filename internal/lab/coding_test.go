package lab

import (
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// codingLab is a lab with coding agents on and the workspaces off.
func codingLab() *config.Config {
	cfg := config.Default()
	cfg.Platform.Coding.Enabled = true
	return cfg
}

// With coding agents on, kagent.harnesses carries claude and claude-capped:
// the claude runtime, a snapshot prefix of their own, the caller's token
// propagated and the platform's limits.
func TestCodingHarnessValues(t *testing.T) {
	values, _, _, _ := renderWorkspaceTemplates(t, codingLab(), nil)
	kagent, _ := values["kagent"].(map[string]any)
	harnesses, _ := kagent["harnesses"].([]any)
	if len(harnesses) != 2 {
		t.Fatalf("kagent.harnesses = %v, want claude and %s", harnesses, claudeCappedHarness)
	}
	platformHarness, _ := kagent["harness"].(map[string]any)
	for i, want := range []struct {
		name, snapshot, budget string
	}{
		{claudeHarness, claudeHarnessSnapshotLocation, claudeHarnessBudgetUSD},
		{claudeCappedHarness, codingSnapshotPrefix + claudeCappedHarness, claudeCappedBudgetUSD},
	} {
		h, _ := harnesses[i].(map[string]any)
		if h["name"] != want.name || h["runtime"] != claudeRuntime || h["snapshotLocation"] != want.snapshot {
			t.Errorf("kagent.harnesses[%d] = %v, want %s on the claude runtime at %s", i, h, want.name, want.snapshot)
		}
		if h["snapshotLocation"] == platformHarness["snapshotLocation"] {
			t.Errorf("%s shares the platform Harness's snapshot location %v", want.name, h["snapshotLocation"])
		}
		limits, _ := h["limits"].(map[string]any)
		if limits["budgetUSD"] != want.budget || limits["maxTurns"] != claudeHarnessMaxTurns {
			t.Errorf("%s limits = %v, want budgetUSD %q (a string, as the CRD takes it) and maxTurns %d", want.name, limits, want.budget, claudeHarnessMaxTurns)
		}
		env, _ := h["env"].([]any)
		if len(env) != 1 || env[0].(map[string]any)["name"] != "KAGENT_PROPAGATE_TOKEN" || env[0].(map[string]any)["value"] != strconv.FormatBool(true) {
			t.Errorf("%s env = %v, want KAGENT_PROPAGATE_TOKEN=true", want.name, env)
		}
	}
}

// Coding agents and the workspaces together render claude once.
func TestCodingHarnessesWithWorkspaces(t *testing.T) {
	stubWorkspacesProbe(t, true)
	cfg := workspaceLab("")
	cfg.Platform.Coding.Enabled = true
	got := codingHarnessesFor(cfg, workspaceAdmissionValuesFor(cfg, workspacesChartCarries(cfg)))
	if len(got) != 2 || got[0].Name != claudeHarness || got[1].Name != claudeCappedHarness {
		t.Fatalf("harnesses = %+v, want claude and %s", got, claudeCappedHarness)
	}
}

// With coding agents off, or without the agents runtime, no coding Harness
// renders.
func TestCodingHarnessesInert(t *testing.T) {
	noAgents := codingLab()
	noAgents.Platform.Agents = false
	for name, cfg := range map[string]*config.Config{"coding off": config.Default(), "no agents runtime": noAgents} {
		t.Run(name, func(t *testing.T) {
			if got := codingHarnessesFor(cfg, nil); len(got) != 0 {
				t.Fatalf("harnesses = %+v, want none", got)
			}
			_, raw, _, _ := renderWorkspaceTemplates(t, cfg, nil)
			if strings.Contains(raw, "harnesses:") {
				t.Errorf("the values carry harnesses:\n%s", excerptAround(raw, "harnesses:"))
			}
		})
	}
}
