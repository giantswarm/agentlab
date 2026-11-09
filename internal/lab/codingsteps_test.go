package lab

import (
	"bytes"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// A step runs itself, what it builds on, and the Harness stage.
func TestCodingStepPlan(t *testing.T) {
	claude := codingRuntimes[config.CodingRuntimeClaude]
	for step, want := range map[string][]string{
		codingStageUsage:                   {codingStageHarnesses, codingStageCreate, codingStageResume, codingStageUsage},
		"suspend-and-resume":               {codingStageHarnesses, codingStageCreate, codingStageResume},
		"EGRESS":                           {codingStageHarnesses, codingStageCreate, codingStageSandbox, codingStageEgress},
		"plugin hook":                      {codingStageHarnesses, codingStageCreate, "plugin skills", "plugin hook"},
		"harness change to kagent refused": {codingStageHarnesses, "claude-format refused on kagent", codingStageCreate, "harness change to kagent refused"},
		"write-time checks":                {codingStageHarnesses, "write-time checks"},
		"claude-format-refused-on-codex":   {codingStageHarnesses, "claude-format refused on codex"},
		codingStageHarnesses:               {codingStageHarnesses},
	} {
		plan, err := codingStepPlan(step, claude)
		if err != nil {
			t.Fatalf("--step %q: %v", step, err)
		}
		got := slices.Sorted(maps.Keys(plan))
		if !slices.Equal(got, slices.Sorted(slices.Values(want))) {
			t.Errorf("--step %q runs %v, want %v", step, got, want)
		}
	}
	if plan, err := codingStepPlan("", claude); plan != nil || err != nil {
		t.Errorf("no --step: plan %v, err %v; want the whole proof", plan, err)
	}
}

// Every need names a stage, and comes before the stage that needs it.
func TestCodingStagesNeedsOrdered(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range codingStages {
		for _, need := range s.needs {
			if !seen[need] {
				t.Errorf("stage %q needs %q, which is not a stage before it", s.name, need)
			}
		}
		seen[s.name] = true
	}
}

// A Claude Code stage on another Harness, an unknown stage and the model
// stage are refused, naming what --step takes.
func TestCodingStepRefused(t *testing.T) {
	codex := codingRuntimes[config.CodingRuntimeCodex]
	for step, want := range map[string]string{
		"uid split":    "runs on the claude Harness alone",
		"no-such":      `no stage "no-such" of coding-test on codex`,
		modelField:     `no stage "model"`,
		"instructions": "--step takes harnesses, write-time checks",
	} {
		_, err := codingStepPlan(step, codex)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("--step %q on codex: %v, want an error with %q", step, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "uid split,") {
			t.Errorf("--step %q on codex lists a claude stage: %v", step, err)
		}
	}
}

// The stages a step leaves out are in the summary as skipped, naming the
// step, in the proof's order.
func TestCodingStepOmitsStages(t *testing.T) {
	plan, err := codingStepPlan(codingStageUsage, codingRuntimes[config.CodingRuntimeOmp])
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	stages := &proofStages{now: time.Now, out: &out}
	p := &codingProof{stages: stages, step: codingStageUsage, plan: plan}
	var ran []string
	for _, name := range codingStepNames(codingRuntimes[config.CodingRuntimeOmp]) {
		if p.runs(name) {
			ran = append(ran, name)
			stages.begin(name, "%s", name)
		}
	}
	if err := stages.close(nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{codingStageHarnesses, codingStageCreate, codingStageResume, codingStageUsage}; !slices.Equal(ran, want) {
		t.Errorf("ran %v, want %v", ran, want)
	}
	summary := out.String()
	for _, name := range []string{codingStageEgress, "share refused"} {
		if !slices.ContainsFunc(strings.Split(summary, "\n"), func(line string) bool {
			f := strings.Fields(line)
			return len(f) > 2 && f[0] == "SKIP" && strings.HasPrefix(strings.Join(f[2:], " "), name+" (--step usage)")
		}) {
			t.Errorf("summary lacks %s skipped by --step usage:\n%s", name, summary)
		}
	}
	if got := p.planWords(); got != "harnesses, create, suspend and resume, usage" {
		t.Errorf("planWords = %q", got)
	}
}
