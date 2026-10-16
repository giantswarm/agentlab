package lab

import (
	"fmt"
	"net"

	"github.com/giantswarm/agentlab/internal/config"
)

// The codex Harness's host model (docs/platform.md "The coding agents
// proof"): codex speaks the OpenAI Responses API alone, and a small host
// model (qwen3.5:9b) runs every turn but relays the proof's probe lines
// unreliably. `agentlab configure` wires the strongest of CodexHostModels the
// host's Ollama has downloaded as a platform.extraModels entry on its
// Responses API, which coding-test --harness codex then runs on by default
// (config.CodexModelConfig).

// CodexHostModel is a host model a codex proof passes on, with the memory it
// needs loaded.
type CodexHostModel struct {
	// ID is the model's Ollama tag.
	ID string
	// GiB is what the model needs in memory, its weights and a turn's
	// context: the model-server budget a codex proof on it asks for.
	GiB int
}

// CodexHostModels are the host models a codex proof passes on, preferred
// first: gpt-oss-20b at its MXFP4 weights, qwen3-coder 30b-a3b at Q4_K_M.
var CodexHostModels = []CodexHostModel{
	{ID: "gpt-oss:20b", GiB: 16},
	{ID: "qwen3-coder:30b", GiB: 21},
}

// CodexHostModelName is the ModelConfig, and extraModels entry, configure
// wires a codex host model as.
const CodexHostModelName = "codex-host"

// CodexHostModel is the extraModels entry for the first of CodexHostModels
// the host's Ollama has downloaded and can call tools with: the OpenAI
// provider on Ollama's /v1 and its Responses API, keyless, reached by the
// name pods resolve the host to. ok is false when Ollama is not found, pods
// cannot reach it, or it has none of them.
func (d *Discovery) CodexHostModel() (config.ExtraModel, bool) {
	for _, s := range d.Servers {
		if s.Backend != config.ModelManagerBackendOllama || (s.Probed && s.PodHost == "") {
			continue
		}
		for _, want := range CodexHostModels {
			for _, m := range s.Models {
				if m.ID == want.ID && m.Tools {
					return config.ExtraModel{
						Name:      CodexHostModelName,
						Provider:  config.ProviderOpenAI,
						Model:     m.ID,
						BaseURL:   fmt.Sprintf("http://%s/v1", net.JoinHostPort(podHostName(s.PodHost), fmt.Sprint(s.Port))),
						APIFormat: config.APIFormatResponses,
					}, true
				}
			}
		}
	}
	return config.ExtraModel{}, false
}

// podHostName is the name a ModelConfig reaches a host server by: the
// container runtime's host alias where that is how pods reach it, else
// labHostName for the kind gateway (or a host not probed yet), since kagent
// injects an OpenAI-provider ModelConfig's key only for a DNS hostname.
func podHostName(podHost string) string {
	if podHost == "" || net.ParseIP(podHost) != nil {
		return labHostName
	}
	return podHost
}

// ApplyCodexHostModel adds the discovered codex host model to cfg's
// extraModels while the lab renders the codex Harness and no entry speaks the
// Responses API yet; an entry of the person's own wins. It returns the entry
// added, ok false when none was.
func ApplyCodexHostModel(cfg *config.Config, d *Discovery) (config.ExtraModel, bool) {
	if !cfg.CodingHarnessRendered(config.CodingRuntimeCodex) || cfg.CodexModelConfig() != "" {
		return config.ExtraModel{}, false
	}
	m, ok := d.CodexHostModel()
	if !ok {
		return config.ExtraModel{}, false
	}
	for _, e := range cfg.Platform.ExtraModels {
		if e.Name == m.Name {
			return config.ExtraModel{}, false
		}
	}
	cfg.Platform.ExtraModels = append(cfg.Platform.ExtraModels, m)
	return m, true
}
