package lab

import (
	"slices"

	"github.com/giantswarm/agentlab/internal/config"
)

// The lab's coding Harnesses, rendered through the chart's kagent.harnesses
// the way an installation turns coding agents on: one per runtime of
// platform.coding.harnesses, named after it, run headless by the kagent
// line's image of that runtime (claude-harness, codex-harness) at the digest
// the chart publishes — or a dev build of it the lab registry serves
// (platform.devImages `<runtime>-harness`) — with its own snapshot prefix in
// the lab's in-cluster store. claude is what the workspaces proof's Session
// runs on; coding-test proves any of them.

// claudeRuntime is the runtime of the claude Harness: Claude Code.
const claudeRuntime = config.CodingRuntimeClaude

// codingHarness is one entry of kagent.harnesses.
type codingHarness struct {
	Name, Runtime, SnapshotLocation string
	// Image is the dev build the entry runs, the lab registry's ref by
	// digest; empty runs the chart's image of the runtime.
	Image string
	// PropagateToken turns on the claude runtime's caller forwarder
	// (KAGENT_PROPAGATE_TOKEN): muster called as the person who sent the turn.
	PropagateToken bool
}

// codingHarnessesFor is what the lab renders into kagent.harnesses: the
// configured coding runtimes while coding agents are on, claude whenever the
// workspaces rig needs it; none otherwise. images are the coding Harnesses'
// dev images by runtime (devImages.coding), nil in a render without them.
func codingHarnessesFor(cfg *config.Config, rig *workspaceAdmissionValues, images map[string]string) []codingHarness {
	var runtimes []string
	if cfg.CodingEnabled() {
		runtimes = cfg.CodingHarnesses()
	}
	if rig != nil && !slices.Contains(runtimes, claudeRuntime) {
		runtimes = append([]string{claudeRuntime}, runtimes...)
	}
	out := make([]codingHarness, 0, len(runtimes))
	for _, runtime := range runtimes {
		out = append(out, codingHarness{
			Name:             runtime,
			Runtime:          runtime,
			SnapshotLocation: codingSnapshotPrefix + runtime,
			Image:            images[runtime],
			PropagateToken:   runtime == claudeRuntime,
		})
	}
	return out
}

// codingSnapshotPrefix is where a coding Harness's snapshots go in the lab's
// in-cluster store, followed by its name.
const codingSnapshotPrefix = "s3://ate-snapshots/"
