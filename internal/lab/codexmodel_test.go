package lab

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

func codexLab(extra ...config.ExtraModel) *config.Config {
	cfg := config.Default()
	cfg.Platform.Coding.Enabled = true
	cfg.Platform.Coding.Harnesses = []string{config.CodingRuntimeClaude, config.CodingRuntimeCodex}
	cfg.Platform.ExtraModels = extra
	return cfg
}

func ollamaWith(podHost string, probed bool, models ...HostModel) *Discovery {
	return &Discovery{KindGateway: gatewayIP, Servers: []HostServer{
		{Backend: ollama, Ident: ollamaVersion, Port: 11434, Probed: probed, PodHost: podHost, Models: models},
	}}
}

// The strongest codex host model Ollama has wins, on its Responses API by
// the name kagent injects a key for: the gateway's IP becomes the lab's host
// name, a runtime's host alias stays.
func TestCodexHostModel(t *testing.T) {
	small := HostModel{ID: modelQwen35, Tools: true}
	coder := HostModel{ID: modelQwen3Coder30b, Tools: true}
	oss := HostModel{ID: modelGptOss20b, Tools: true}
	for name, tc := range map[string]struct {
		disc      *Discovery
		model, at string
	}{
		"gateway":       {disc: ollamaWith(gatewayIP, true, small, oss, coder), model: modelQwen3Coder30b, at: "http://" + labHostName + ":11434/v1"},
		"not probed":    {disc: ollamaWith("", false, oss), model: modelGptOss20b, at: "http://" + labHostName + ":11434/v1"},
		"alias":         {disc: ollamaWith(hostDockerInternal, true, oss), model: modelGptOss20b, at: "http://" + hostDockerInternal + ":11434/v1"},
		"small only":    {disc: ollamaWith(gatewayIP, true, small)},
		"no tools":      {disc: ollamaWith(gatewayIP, true, HostModel{ID: modelGptOss20b})},
		"unreachable":   {disc: ollamaWith("", true, oss)},
		"no model host": {disc: &Discovery{}},
	} {
		t.Run(name, func(t *testing.T) {
			m, ok := tc.disc.CodexHostModel()
			if ok != (tc.model != "") {
				t.Fatalf("found %v (%+v), want %q", ok, m, tc.model)
			}
			if !ok {
				return
			}
			if m.Model != tc.model || m.BaseURL != tc.at || m.Name != CodexHostModelName {
				t.Errorf("entry %+v, want %s at %s", m, tc.model, tc.at)
			}
			if m.Provider != config.ProviderOpenAI || m.APIFormat != config.APIFormatResponses || m.APIKeyEnv != "" {
				t.Errorf("entry %+v, want the keyless OpenAI provider on the Responses API", m)
			}
			if err := m.Validate(); err != nil {
				t.Errorf("the entry does not validate: %v", err)
			}
		})
	}
}

// configure adds the entry only when asked (--codex-host-model), for a lab
// that renders codex and has no Responses API entry of its own, and
// coding-test's codex default follows it.
func TestApplyCodexHostModel(t *testing.T) {
	disc := ollamaWith(gatewayIP, true, HostModel{ID: modelGptOss20b, Tools: true})

	cfg := codexLab()
	if _, ok := ApplyCodexHostModel(cfg, disc, true); !ok {
		t.Fatal("a codex lab with gpt-oss:20b on Ollama gets the codex host model")
	}
	if got := cfg.CodexModelConfig(); got != CodexHostModelName {
		t.Errorf("CodexModelConfig = %q, want %s", got, CodexHostModelName)
	}
	if _, ok := ApplyCodexHostModel(cfg, disc, true); ok || len(cfg.Platform.ExtraModels) != 1 {
		t.Errorf("a second configure must not add it again: %+v", cfg.Platform.ExtraModels)
	}

	own := config.ExtraModel{Name: "mine", Provider: config.ProviderOpenAI, Model: "gpt-5", APIFormat: config.APIFormatResponses}
	cfg = codexLab(own)
	if _, ok := ApplyCodexHostModel(cfg, disc, true); ok || cfg.CodexModelConfig() != "mine" {
		t.Errorf("the person's own Responses API entry wins: %+v", cfg.Platform.ExtraModels)
	}

	cfg = config.Default()
	if _, ok := ApplyCodexHostModel(cfg, disc, true); ok {
		t.Error("a lab without the codex Harness gets no codex host model")
	}
}

// `configure --defaults --coding --coding-harnesses claude,codex` without
// --codex-host-model writes no host model, whichever codex host models the
// host's Ollama has: each is over a proof's memory budget.
func TestApplyCodexHostModelUnasked(t *testing.T) {
	var all []HostModel
	for _, m := range CodexHostModels {
		all = append(all, HostModel{ID: m.ID, Tools: true})
	}
	cfg := codexLab()
	if m, ok := ApplyCodexHostModel(cfg, ollamaWith(gatewayIP, true, all...), false); ok {
		t.Fatalf("configure added %+v unasked", m)
	}
	if len(cfg.Platform.ExtraModels) != 0 || cfg.CodexModelConfig() != "" {
		t.Errorf("the generated config carries a host model: %+v", cfg.Platform.ExtraModels)
	}
}

// The codex host models, by tag.
var (
	modelQwen3Coder30b = CodexHostModels[0].ID
	modelGptOss20b     = CodexHostModels[1].ID
)

// The codex host model table in docs/platform.md states the memory each
// CodexHostModels entry asks of the model server.
func TestCodexHostModelsDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/platform.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range CodexHostModels {
		row := regexp.MustCompile("(?m)^\\| `" + regexp.QuoteMeta(m.ID) + "` [^|]*\\| (\\d+) GiB \\|").FindSubmatch(doc)
		if row == nil {
			t.Errorf("docs/platform.md has no host model row with a GiB figure for %s", m.ID)
			continue
		}
		if got := string(row[1]); got != strconv.Itoa(m.GiB) {
			t.Errorf("docs/platform.md gives %s %s GiB, codexmodel.go %d GiB", m.ID, got, m.GiB)
		}
	}
}
