package lab

import (
	"fmt"
	"slices"
	"strings"
)

// `agentlab coding-test --step <name>` runs one stage of the coding agents
// proof and the stages it builds on, nothing else: the egress verdict needs
// the sandbox turn, which needs the agent; the usage needs the resume. The
// Harness stage always runs, and on the mockllm the model stage closes every
// run, a step's too, judging the model calls that run made; it is no step of
// its own. A stage the step leaves out is listed in the summary as skipped,
// naming the step, so a step's run never reads as the whole proof's pass.

// The stages other stages build on, by name.
const (
	codingStageHarnesses = "harnesses"
	codingStageCreate    = "create"
	codingStageSandbox   = "sandbox turn"
	codingStageEgress    = "egress"
	codingStageResume    = "suspend and resume"
	codingStageUsage     = "usage"
)

// codingStage is one stage of the coding agents proof: its name in the
// summary and on --step, and the stages whose state it reads.
type codingStage struct {
	name string
	// claude marks a stage that runs on Claude Code alone.
	claude bool
	// closes marks the model stage, which closes every run on the mockllm
	// and is no step of its own.
	closes bool
	// needs are the stages it builds on.
	needs []string
}

// codingStages are the proof's stages in the order the proof runs them.
var codingStages = []codingStage{
	{name: codingStageHarnesses},
	{name: "write-time checks"},
	{name: "plugins accepted"},
	{name: "claude-format refused on codex"},
	{name: "claude-format refused on kagent"},
	{name: "claude-format refused on omp"},
	{name: codingStageCreate},
	{name: codingStageSandbox, needs: []string{codingStageCreate}},
	{name: "uid split", claude: true, needs: []string{codingStageSandbox}},
	{name: "mcp binding", claude: true, needs: []string{codingStageSandbox}},
	{name: "write tool refused", claude: true, needs: []string{codingStageSandbox}},
	{name: "credentials", needs: []string{codingStageSandbox}},
	{name: "dns canary", needs: []string{codingStageSandbox}},
	{name: codingStageEgress, needs: []string{codingStageSandbox}},
	{name: "plugin skills", needs: []string{codingStageCreate}},
	{name: "plugin hook", claude: true, needs: []string{"plugin skills"}},
	{name: "harness change refused", claude: true, needs: []string{codingStageCreate}},
	{name: "harness change to kagent refused", claude: true, needs: []string{codingStageCreate, "claude-format refused on kagent"}},
	{name: "harness change to omp refused", claude: true, needs: []string{codingStageCreate, "claude-format refused on omp"}},
	{name: "mcp call", needs: []string{codingStageCreate}},
	{name: "turn gap", needs: []string{codingStageCreate}},
	{name: "instruction fixture", claude: true, needs: []string{codingStageCreate}},
	{name: codingStageResume, needs: []string{codingStageCreate}},
	{name: codingStageUsage, needs: []string{codingStageResume}},
	{name: "share refused", needs: []string{codingStageCreate}},
	{name: "empty selection", needs: []string{codingStageCreate}},
	{name: "model", closes: true},
}

// codingStepNames are the names --step takes on a run of rt: every stage of
// the proof but the model's on Claude Code, the rest elsewhere.
func codingStepNames(rt codingRuntime) []string {
	var names []string
	for _, s := range codingStages {
		if s.closes || s.claude && !rt.claude {
			continue
		}
		names = append(names, s.name)
	}
	return names
}

// CodingStepHelp words the stages --step takes, for the flag's help.
func CodingStepHelp() string {
	names := make([]string, 0, len(codingStages))
	for _, s := range codingStages {
		if s.closes {
			continue
		}
		name := s.name
		if s.claude {
			name += " (claude)"
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// stepKey is the form a step is matched in: case and dashes against spaces
// do not matter, so `--step suspend-and-resume` names "suspend and resume".
func stepKey(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(name, "-", " "))), " ")
}

// codingStepPlan is the set of stages a --step run of rt runs: the step and,
// transitively, the stages it builds on, with the Harness stage. An empty
// step is the whole proof (nil). A step the runtime has no stage of is
// refused, naming the ones it has.
func codingStepPlan(step string, rt codingRuntime) (map[string]bool, error) {
	if strings.TrimSpace(step) == "" {
		return nil, nil
	}
	names := codingStepNames(rt)
	i := slices.IndexFunc(names, func(name string) bool { return stepKey(name) == stepKey(step) })
	if i < 0 {
		for _, s := range codingStages {
			if s.claude && stepKey(s.name) == stepKey(step) {
				return nil, fmt.Errorf("--step %q runs on the claude Harness alone (--harness %s); on %s --step takes: %s", step, rt.name, rt.name, strings.Join(names, ", "))
			}
		}
		return nil, fmt.Errorf("no stage %q of coding-test on %s: --step takes %s", step, rt.name, strings.Join(names, ", "))
	}
	plan := map[string]bool{codingStageHarnesses: true}
	var add func(name string)
	add = func(name string) {
		if plan[name] {
			return
		}
		plan[name] = true
		for _, s := range codingStages {
			if s.name == name {
				for _, need := range s.needs {
					add(need)
				}
			}
		}
	}
	add(names[i])
	return plan, nil
}

// runs says whether the stage named runs in this run; one a --step leaves
// out is recorded as skipped, with the step that leaves it out. Called once
// per stage, at the place the stage would begin.
func (p *codingProof) runs(name string) bool {
	if p.plan == nil || p.plan[name] {
		return true
	}
	p.stages.omit(fmt.Sprintf("--step %s", p.step), name)
	return false
}

// planWords names the stages a --step run runs, in the proof's order.
func (p *codingProof) planWords() string {
	var names []string
	for _, s := range codingStages {
		if p.plan[s.name] {
			names = append(names, s.name)
		}
	}
	return strings.Join(names, ", ")
}
