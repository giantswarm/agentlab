package lab

import (
	"fmt"
	"net"

	"github.com/giantswarm/agentlab/internal/config"
)

// The codex Harness's host model (docs/platform.md "The coding agents
// proof"): codex speaks the OpenAI Responses API alone. `agentlab configure`
// wires the first of CodexHostModels the host's Ollama has downloaded as a
// platform.extraModels entry on its Responses API, which coding-test
// --harness codex then runs on by default (config.CodexModelConfig). The
// proof judges what the model relays of the probe; a hosted Responses API
// model relays it verbatim, the host models listed here reach the sandbox
// stages but not yet every stage.

// CodexHostModel is a host model for a codex proof, with the memory it needs
// loaded.
type CodexHostModel struct {
	// ID is the model's Ollama tag.
	ID string
	// GiB is what the model needs in memory, its weights and a turn's
	// context: the model-server budget a codex proof on it asks for.
	GiB int
}

// CodexHostModels are the host models for a codex proof, preferred first:
// qwen3-coder 30b-a3b at Q4_K_M reaches the sandbox turn, credentials and
// DNS stages; gpt-oss-20b at its MXFP4 weights answers some Responses API
// calls with a server error on Ollama.
var CodexHostModels = []CodexHostModel{
	{ID: "qwen3-coder:30b", GiB: 21},
	{ID: "gpt-oss:20b", GiB: 16},
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
