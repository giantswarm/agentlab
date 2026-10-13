package lab

import (
	"github.com/giantswarm/agentlab/internal/config"
)

// The lab's coding Harness, rendered through the chart's kagent.harnesses
// the way an installation turns coding agents on: Claude Code run headless
// by the kagent line's claude-harness image at the digest the chart
// publishes, with its own snapshot prefix in the lab's in-cluster store.
// claude is what a coding agent runs on (the workspaces proof's Session and
// coding-test's agent).

// claudeRuntime is the runtime of a coding Harness: Claude Code.
const claudeRuntime = "claude"

// codingHarness is one entry of kagent.harnesses.
type codingHarness struct {
	Name, SnapshotLocation string
}

// codingHarnessesFor is what the lab renders into kagent.harnesses: claude
// whenever coding agents or the workspaces rig need it; none otherwise.
func codingHarnessesFor(cfg *config.Config, rig *workspaceAdmissionValues) []codingHarness {
	if !cfg.CodingEnabled() && rig == nil {
		return nil
	}
	return []codingHarness{{Name: claudeHarness, SnapshotLocation: claudeHarnessSnapshotLocation}}
}
