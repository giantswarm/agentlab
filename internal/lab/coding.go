package lab

import (
	"github.com/giantswarm/agentlab/internal/config"
)

// The lab's coding Harnesses, rendered through the chart's kagent.harnesses
// the way an installation turns coding agents on: Claude Code run headless
// by the kagent line's claude-harness image at the digest the chart
// publishes, each with its own snapshot prefix in the lab's in-cluster
// store and the platform's limits on every turn. claude is what a coding
// agent runs on (the workspaces proof's Session and coding-test's agent);
// claude-capped is the same runtime on a budget no turn stays under, the
// subject of coding-test's cap stage.

const (
	// claudeRuntime is the runtime of a coding Harness: Claude Code.
	claudeRuntime = "claude"
	// claudeCappedHarness is the coding Harness whose budget every turn
	// reaches.
	claudeCappedHarness = "claude-capped"
	// claudeCappedBudgetUSD is its budget: below the cost of one model call
	// with Claude Code's system prompt.
	claudeCappedBudgetUSD = "0.01"
	// claudeHarnessBudgetUSD and claudeHarnessMaxTurns are claude's limits.
	claudeHarnessBudgetUSD = "1.00"
	claudeHarnessMaxTurns  = 10
	// codingSnapshotPrefix is where a coding Harness keeps its snapshots,
	// beside the platform Harness's.
	codingSnapshotPrefix = "s3://ate-snapshots/"
)

// codingHarness is one entry of kagent.harnesses.
type codingHarness struct {
	Name, SnapshotLocation, BudgetUSD string
	MaxTurns                          int
}

// codingHarnessesFor is what the lab renders into kagent.harnesses: claude
// whenever coding agents or the workspaces rig need it, claude-capped with
// coding agents; none otherwise.
func codingHarnessesFor(cfg *config.Config, rig *workspaceAdmissionValues) []codingHarness {
	var harnesses []codingHarness
	if cfg.CodingEnabled() || rig != nil {
		harnesses = append(harnesses, codingHarness{
			Name: claudeHarness, SnapshotLocation: claudeHarnessSnapshotLocation,
			BudgetUSD: claudeHarnessBudgetUSD, MaxTurns: claudeHarnessMaxTurns,
		})
	}
	if cfg.CodingEnabled() {
		harnesses = append(harnesses, codingHarness{
			Name: claudeCappedHarness, SnapshotLocation: codingSnapshotPrefix + claudeCappedHarness,
			BudgetUSD: claudeCappedBudgetUSD, MaxTurns: claudeHarnessMaxTurns,
		})
	}
	return harnesses
}
