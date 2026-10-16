package lab

import (
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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
	if _, ok := codex["command"]; ok {
		t.Errorf("codex carries command %v, wanted none (the chart's image runs its entrypoint)", codex["command"])
	}
	if _, ok := codex["displayName"]; ok {
		t.Errorf("codex carries displayName %v, wanted none (the portal labels the codex runtime itself)", codex["displayName"])
	}
	if _, ok := codex["env"]; ok {
		t.Errorf("codex carries env %v, wanted none (no caller forwarder)", codex["env"])
	}
	if _, ok := codex["image"]; ok {
		t.Errorf("codex carries image %v without a dev image, wanted the chart's", codex["image"])
	}
}

// platform.coding.harnesses [omp] renders a BYO entry on the image
// platform.coding.images names, named omp, on its own snapshot prefix and
// without the caller forwarder's variable.
func TestCodingHarnessesOmp(t *testing.T) {
	cfg := codingLab()
	cfg.Platform.Coding.Harnesses = []string{config.CodingRuntimeOmp}
	image := "gsoci.azurecr.io/giantswarm/kagent/omp-harness@sha256:" + strings.Repeat("b", 64)
	cfg.Platform.Coding.Images = map[string]string{config.CodingRuntimeOmp: image}
	values, _, _, _ := renderWorkspaceTemplates(t, cfg, nil)
	kagent, _ := values["kagent"].(map[string]any)
	harnesses, _ := kagent["harnesses"].([]any)
	if len(harnesses) != 1 {
		t.Fatalf("kagent.harnesses = %v, want omp alone", harnesses)
	}
	omp, _ := harnesses[0].(map[string]any)
	if omp["name"] != "omp" || omp["runtime"] != "byo" || omp["image"] != image || omp["snapshotLocation"] != codingSnapshotPrefix+"omp" {
		t.Errorf("kagent.harnesses[0] = %v, want omp as a byo entry on %s at %somp", omp, image, codingSnapshotPrefix)
	}
	if command, _ := omp["command"].([]any); len(command) != 1 || command[0] != ompHarnessCommand[0] {
		t.Errorf("omp command = %v, want %v (the Harness API requires workload.command on a BYO Harness)", omp["command"], ompHarnessCommand)
	}
	if omp["displayName"] != ompDisplayName {
		t.Errorf("omp displayName = %v, want %q (a BYO entry names what runs)", omp["displayName"], ompDisplayName)
	}
	if _, ok := omp["env"]; ok {
		t.Errorf("omp carries env %v, wanted none (no caller forwarder)", omp["env"])
	}
	dev := "localhost:5001/kagent-dev/kagent/omp-harness@sha256:" + strings.Repeat("c", 64)
	if got := codingHarnessesFor(cfg, nil, map[string]string{config.CodingRuntimeOmp: dev}); len(got) != 1 || got[0].Image != dev {
		t.Errorf("harnesses = %+v, want omp on the dev image %s over platform.coding.images", got, dev)
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

// The secret-backed credential stage is skipped, naming the headers, on a
// binding with no header from a Secret (a platform line that ships no
// Secret-backed credential); it passes when every Secret key a header names
// is there and fails naming the one that is missing.
func TestJudgeSecretBacked(t *testing.T) {
	const line = "agent-platform 4.140.0"
	plain := agentServer(codingTestAgent, presetReadOnly)
	if skip, err := judgeSecretBacked(plain, func(string, string, string) bool { t.Fatal("a Secret read on a binding with none"); return false }, line); err != nil || !strings.Contains(skip, "["+toolsetHeader+"]") || !strings.Contains(skip, line) {
		t.Errorf("no Secret-backed header: skip %q, err %v; want a skip naming [%s] and %s", skip, err, toolsetHeader, line)
	}

	backed := agentServer(codingTestAgent, "")
	_ = unstructured.SetNestedSlice(backed.Object, []any{
		map[string]any{nameKey: toolsetHeader, fieldValue: presetReadOnly},
		map[string]any{nameKey: "Authorization", "valueFrom": map[string]any{fieldTypeKey: kindSecret, nameKey: "agentlab-coding-mcp", fieldKey: "token"}},
	}, "spec", "headersFrom")
	present := func(ns, name, key string) bool {
		return ns == kagentNamespace && name == "agentlab-coding-mcp" && key == "token"
	}
	if skip, err := judgeSecretBacked(backed, present, line); err != nil || skip != "" {
		t.Errorf("Secret key there: skip %q, err %v; want a pass", skip, err)
	}
	missing := func(string, string, string) bool { return false }
	if skip, err := judgeSecretBacked(backed, missing, line); err == nil || skip != "" || !strings.Contains(err.Error(), "agentlab-coding-mcp key token, which is missing") {
		t.Errorf("Secret key missing: skip %q, err %v; want a failure naming the Secret key", skip, err)
	}
}
