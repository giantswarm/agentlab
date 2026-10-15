package lab

import (
	"slices"

	"github.com/giantswarm/agentlab/internal/config"
)

// The lab's coding Harnesses, rendered through the chart's kagent.harnesses
// the way an installation turns coding agents on: one per runtime of
// platform.coding.harnesses, named after it, run headless by the kagent
// line's image of that runtime (claude-harness, codex-harness) at the digest
// the chart publishes, omp on the omp-harness adapter the entry names
// (platform.coding.images, a BYO entry) — or a dev build of it the lab
// registry serves (platform.devImages `<runtime>-harness`) — with its own
// snapshot prefix in the lab's in-cluster store. claude is what the
// workspaces proof's Session runs on; coding-test proves any of them.

// claudeRuntime is the runtime of the claude Harness: Claude Code.
const claudeRuntime = config.CodingRuntimeClaude

// codingHarness is one entry of kagent.harnesses.
type codingHarness struct {
	// Name is the coding runtime; Runtime what the chart's entry names
	// (config.CodingChartRuntime: byo for omp).
	Name, Runtime, SnapshotLocation string
	// Image is the dev build the entry runs, the lab registry's ref by
	// digest, else the image platform.coding.images names for the runtime;
	// empty runs the chart's image of the runtime.
	Image string
	// Command is the image's command, which the Harness API requires on a
	// BYO Harness: the omp-harness adapter's binary on omp.
	Command []string
	// DisplayName is what the Dev Portal titles the Harness by (the chart
	// renders it as the ui.giantswarm.io/display-name annotation), required
	// on a BYO entry since its image alone does not say what runs.
	DisplayName string
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
		image := images[runtime]
		if image == "" {
			image = cfg.CodingHarnessImage(runtime)
		}
		out = append(out, codingHarness{
			Name:             runtime,
			Runtime:          config.CodingChartRuntime(runtime),
			SnapshotLocation: codingSnapshotPrefix + runtime,
			Image:            image,
			Command:          codingHarnessCommand(runtime),
			DisplayName:      codingHarnessDisplayName(runtime),
			PropagateToken:   runtime == claudeRuntime,
		})
	}
	return out
}

// codingSnapshotPrefix is where a coding Harness's snapshots go in the lab's
// in-cluster store, followed by its name.
const codingSnapshotPrefix = "s3://ate-snapshots/"

// ompHarnessCommand is the omp-harness adapter's binary in the kagent line's
// image (go/harness/omp/Dockerfile), the command a BYO Harness must name.
var ompHarnessCommand = []string{"/usr/local/bin/kagent-omp"}

// codingHarnessCommand is the command a coding runtime's entry names: the
// adapter's binary on omp, none where the runtime's image is the chart's.
func codingHarnessCommand(runtime string) []string {
	if runtime == config.CodingRuntimeOmp {
		return ompHarnessCommand
	}
	return nil
}

// ompDisplayName is what the Dev Portal titles the omp Harness by.
const ompDisplayName = "oh-my-pi"

// codingHarnessDisplayName is the display name a coding runtime's entry
// carries: omp's, which a BYO entry must name; none for the runtimes the
// portal labels by their spec.
func codingHarnessDisplayName(runtime string) string {
	if runtime == config.CodingRuntimeOmp {
		return ompDisplayName
	}
	return ""
}
