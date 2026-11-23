package lab

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// codexHarness is the coding Harness whose runtime needs a Responses API
// model of its own.
const codexHarness = "codex"

// workspacePartHarnesses are the Harnesses a part's Sessions run on: names,
// comma separated, else defaults. modelConfigs is empty, one ModelConfig for
// every Harness but claude and kagent, or harness=ModelConfig pairs, comma
// separated. claude, kagent and omp default to the kagent namespace's
// default ModelConfig; codex needs one named.
func workspacePartHarnesses(names, modelConfigs string, defaults []string) ([]workspacesSessionHarness, error) {
	selected := defaults
	if names != "" {
		selected = strings.Split(names, ",")
	}
	shared, perHarness, err := parsePartModelConfigs(modelConfigs)
	if err != nil {
		return nil, err
	}
	var harnesses []workspacesSessionHarness
	for _, name := range selected {
		name = strings.TrimSpace(name)
		if name == "" || slices.ContainsFunc(harnesses, func(h workspacesSessionHarness) bool { return h.name == name }) {
			return nil, fmt.Errorf("--harness %q names an empty or repeated Harness", names)
		}
		modelConfig, ok := perHarness[name]
		if !ok && name != claudeHarness && name != kagentHarness {
			modelConfig = shared
		}
		delete(perHarness, name)
		if name == codexHarness && modelConfig == "" {
			return nil, fmt.Errorf("--harness %s needs --model-config %s=<ModelConfig>: codex runs on a Responses API model, not the default", codexHarness, codexHarness)
		}
		harnesses = append(harnesses, workspacePartHarness(name, modelConfig))
	}
	if len(perHarness) > 0 {
		return nil, fmt.Errorf("--model-config names Harnesses the part does not run: %s", strings.Join(slices.Sorted(maps.Keys(perHarness)), ", "))
	}
	return harnesses, nil
}

func parsePartModelConfigs(value string) (string, map[string]string, error) {
	perHarness := map[string]string{}
	if value == "" {
		return "", perHarness, nil
	}
	if !strings.Contains(value, "=") {
		return value, perHarness, nil
	}
	for pair := range strings.SplitSeq(value, ",") {
		harness, modelConfig, ok := strings.Cut(pair, "=")
		if !ok || harness == "" || modelConfig == "" {
			return "", nil, fmt.Errorf("--model-config %q: want <ModelConfig> or <harness>=<ModelConfig>,…", value)
		}
		perHarness[harness] = modelConfig
	}
	return "", perHarness, nil
}

// workspacePartHarness is the Session Harness of one name; the kagent
// Harness keeps the sessions part's Agent.
func workspacePartHarness(name, modelConfig string) workspacesSessionHarness {
	if name == kagentHarness {
		return workspacesSessionHarness{name: kagentHarness, modelConfig: modelConfig, agent: sessionsKagentAgent, shellTool: bashToolName}
	}
	return workspacesSessionHarnessFor(name, modelConfig)
}

// harnessObject is the Harness object a Session Harness's Agent runs on.
func (h workspacesSessionHarness) harnessObject(rig *workspaceAdmissionValues) string {
	if h.name == claudeHarness {
		return rig.ClaudeHarness
	}
	return h.name
}

// readyPartAgents applies one Agent per Harness, on its ModelConfig or the
// kagent namespace's default, and waits for each to be Ready.
func readyPartAgents(ctx context.Context, cfg *config.Config, s *musterSession, rig *workspaceAdmissionValues, harnesses []workspacesSessionHarness, timeout time.Duration) error {
	defaultModelConfig := ""
	for _, h := range harnesses {
		if h.name != claudeHarness && h.name != kagentHarness && !cfg.CodingHarnessRendered(h.name) {
			return fmt.Errorf("the lab renders no Harness %s: add it to platform.coding.harnesses in %s, then `agentlab platform`", h.name, config.File)
		}
		harness := h.harnessObject(rig)
		if err := waitHarnessReady(ctx, harness, timeout); err != nil {
			return err
		}
		modelConfig := h.modelConfig
		if modelConfig == "" {
			if defaultModelConfig == "" {
				var err error
				if defaultModelConfig, err = agentManagerModelConfig(s); err != nil {
					return err
				}
			}
			modelConfig = defaultModelConfig
		}
		if agentExists(h.agent) {
			if err := removeAgent(h.agent); err != nil {
				return err
			}
		}
		if _, err := applyManifests(ctx, []byte(workspacesAgentManifest(h.agent, harness, modelConfig, fmt.Sprintf(workspacesHarnessSystemPrompt, h.shellTool)))); err != nil {
			return fmt.Errorf("applying the Agent %s: %w", h.agent, err)
		}
		boot, err := waitAgentReadyOn(h.agent, harness, timeout)
		if err != nil {
			return err
		}
		if !boot.ready {
			return fmt.Errorf("the Agent %s is not Ready on Harness %s after %s: %s", h.agent, harness, boot.elapsed.Round(time.Second), boot.reason)
		}
		note("the Agent %s Ready on Harness %s (ModelConfig %s) in %s", h.agent, harness, modelConfig, boot.elapsed.Round(time.Second))
	}
	return nil
}

// removePartAgents removes the Agents readyPartAgents applied.
func removePartAgents(harnesses []workspacesSessionHarness) []error {
	var errs []error
	for _, h := range harnesses {
		if agentExists(h.agent) {
			errs = append(errs, removeAgent(h.agent))
		}
	}
	return errs
}
