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

// With coding agents on, kagent.harnesses carries claude: the claude
// runtime, a snapshot location of its own, the caller's token propagated and
// no limits.
func TestCodingHarnessValues(t *testing.T) {
	values, raw, _, _ := renderWorkspaceTemplates(t, codingLab(), nil)
	kagent, _ := values["kagent"].(map[string]any)
	harnesses, _ := kagent["harnesses"].([]any)
	if len(harnesses) != 1 {
		t.Fatalf("kagent.harnesses = %v, want claude alone", harnesses)
	}
	platformHarness, _ := kagent["harness"].(map[string]any)
	h, _ := harnesses[0].(map[string]any)
	if h["name"] != claudeHarness || h["runtime"] != claudeRuntime || h["snapshotLocation"] != claudeHarnessSnapshotLocation {
		t.Errorf("kagent.harnesses[0] = %v, want %s on the claude runtime at %s", h, claudeHarness, claudeHarnessSnapshotLocation)
	}
	if h["snapshotLocation"] == platformHarness["snapshotLocation"] {
		t.Errorf("%s shares the platform Harness's snapshot location %v", claudeHarness, h["snapshotLocation"])
	}
	if _, ok := h["limits"]; ok {
		t.Errorf("%s carries limits %v, a field the kagent line dropped", claudeHarness, h["limits"])
	}
	for _, key := range []string{"budgetUSD", "maxTurns"} {
		if strings.Contains(raw, key) {
			t.Errorf("the values carry %s:\n%s", key, excerptAround(raw, key))
		}
	}
	env, _ := h["env"].([]any)
	if len(env) != 1 || env[0].(map[string]any)["name"] != "KAGENT_PROPAGATE_TOKEN" || env[0].(map[string]any)["value"] != strconv.FormatBool(true) {
		t.Errorf("%s env = %v, want KAGENT_PROPAGATE_TOKEN=true", claudeHarness, env)
	}
}

// Coding agents and the workspaces together render claude once.
func TestCodingHarnessesWithWorkspaces(t *testing.T) {
	stubWorkspacesProbe(t, true)
	cfg := workspaceLab("")
	cfg.Platform.Coding.Enabled = true
	got := codingHarnessesFor(cfg, workspaceAdmissionValuesFor(cfg, workspacesChartCarries(cfg)), nil)
	if len(got) != 1 || got[0].Name != claudeHarness {
		t.Fatalf("harnesses = %+v, want claude alone", got)
	}
}

// With coding agents off, or without the agents runtime, no coding Harness
// renders.
func TestCodingHarnessesInert(t *testing.T) {
	noAgents := codingLab()
	noAgents.Platform.Agents = false
	for name, cfg := range map[string]*config.Config{"coding off": config.Default(), "no agents runtime": noAgents} {
		t.Run(name, func(t *testing.T) {
			if got := codingHarnessesFor(cfg, nil, nil); len(got) != 0 {
				t.Fatalf("harnesses = %+v, want none", got)
			}
			_, raw, _, _ := renderWorkspaceTemplates(t, cfg, nil)
			if strings.Contains(raw, "harnesses:") {
				t.Errorf("the values carry harnesses:\n%s", excerptAround(raw, "harnesses:"))
			}
		})
	}
}

// platform.coding.harnesses [claude, codex] renders both, each on its
// runtime and its own snapshot prefix; codex without the caller forwarder's
// KAGENT_PROPAGATE_TOKEN.
func TestCodingHarnessesCodex(t *testing.T) {
	cfg := codingLab()
	cfg.Platform.Coding.Harnesses = []string{config.CodingRuntimeClaude, config.CodingRuntimeCodex}
	values, _, _, _ := renderWorkspaceTemplates(t, cfg, nil)
	kagent, _ := values["kagent"].(map[string]any)
	harnesses, _ := kagent["harnesses"].([]any)
	if len(harnesses) != 2 {
		t.Fatalf("kagent.harnesses = %v, want claude and codex", harnesses)
	}
	codex, _ := harnesses[1].(map[string]any)
	if codex["name"] != "codex" || codex["runtime"] != "codex" || codex["snapshotLocation"] != codingSnapshotPrefix+"codex" {
		t.Errorf("kagent.harnesses[1] = %v, want codex on the codex runtime at %scodex", codex, codingSnapshotPrefix)
	}
	if _, ok := codex["env"]; ok {
		t.Errorf("codex carries env %v, wanted none (no caller forwarder)", codex["env"])
	}
	if _, ok := codex["image"]; ok {
		t.Errorf("codex carries image %v without a dev image, wanted the chart's", codex["image"])
	}
}

// A coding Harness's dev image is its entry's image; the others keep the
// chart's.
func TestCodingHarnessesDevImage(t *testing.T) {
	cfg := codingLab()
	cfg.Platform.Coding.Harnesses = []string{config.CodingRuntimeClaude, config.CodingRuntimeCodex}
	pinned := "localhost:5001/kagent-dev/kagent/codex-harness@sha256:" + strings.Repeat("a", 64)
	got := codingHarnessesFor(cfg, nil, map[string]string{config.CodingRuntimeCodex: pinned})
	if len(got) != 2 || got[0].Image != "" || got[1].Image != pinned {
		t.Fatalf("harnesses = %+v, want claude on the chart's image and codex on %s", got, pinned)
	}
}
