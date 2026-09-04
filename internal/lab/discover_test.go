package lab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// Fixture vocabulary, hoisted so the linter's constant check stays quiet.
const (
	ollama          = config.ModelManagerBackendOllama
	lemonade        = config.ModelManagerBackendLemonade
	capCompletion   = "completion"
	labelChat       = "chat"
	ollamaVersion   = "0.33.2"
	lemonadeVersion = "11.9.0"
	modelQwen35     = "qwen3.5:9b"
	modelGemma270m  = "gemma3:270m"
	modelSmollm     = "smollm2:135m"
	modelQwen3FLM   = "qwen3-it-4b-FLM"
	modelGemma4bFLM = "gemma3-4b-FLM"
	modelMoEFLM     = "Qwen3.6-MoE-35B-A3B-FLM"
	modelQwenVLFLM  = "qwen3vl-it-4b-FLM"
	fieldData       = "data"
	fieldOwnedBy    = "owned_by"
	fieldDownloaded = "downloaded"
	fieldLabels     = "labels"
	labelVision     = "vision"
)

// fakeOllama answers /api/version, /api/tags and /api/show like an Ollama.
func fakeOllama(t *testing.T) *httptest.Server {
	t.Helper()
	caps := map[string][]string{
		modelQwen35:    {capCompletion, labelVision, "tools", "thinking"},
		modelGemma270m: {capCompletion},
		modelSmollm:    {capCompletion, "tools"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": ollamaVersion})
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		var models []map[string]string
		for _, name := range []string{modelQwen35, modelGemma270m, modelSmollm} {
			models = append(models, map[string]string{"name": name})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": caps[req.Model]})
	})
	return httptest.NewServer(mux)
}

// fakeLemonade answers /api/v1/health and /api/v1/models like a Lemonade
// Server (downloaded models only, with labels).
func fakeLemonade(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"all_models_loaded": []any{}, "status": "ok", "version": lemonadeVersion})
	})
	mux.HandleFunc("/api/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", fieldData: []map[string]any{
			{"id": modelQwen3FLM, fieldDownloaded: true, fieldLabels: []string{lemonadeToolsLabel, labelChat}, "recipe": "flm"},
			{"id": modelGemma4bFLM, fieldDownloaded: true, fieldLabels: []string{labelVision, labelChat}, "recipe": "flm"},
			{"id": modelMoEFLM, fieldDownloaded: true, fieldLabels: []string{labelVision, "reasoning", lemonadeToolsLabel, labelChat}},
			{"id": "not-yet", fieldDownloaded: false, fieldLabels: []string{lemonadeToolsLabel}},
		}})
	})
	return httptest.NewServer(mux)
}

func TestDetectHostServer(t *testing.T) {
	ollama := fakeOllama(t)
	defer ollama.Close()
	lemonade := fakeLemonade(t)
	defer lemonade.Close()

	if v, ok := detectHostServer(config.ModelManagerBackendOllama, ollama.URL); !ok || v != ollamaVersion {
		t.Errorf("ollama: %q %v", v, ok)
	}
	if v, ok := detectHostServer(config.ModelManagerBackendLemonade, lemonade.URL); !ok || v != lemonadeVersion {
		t.Errorf("lemonade: %q %v", v, ok)
	}
	// The wrong server on a port is not a hit: Ollama's path on Lemonade 404s.
	if _, ok := detectHostServer(config.ModelManagerBackendOllama, lemonade.URL); ok {
		t.Errorf("Lemonade detected as Ollama")
	}
	if _, ok := detectHostServer(config.ModelManagerBackendOllama, "http://127.0.0.1:1"); ok {
		t.Errorf("a closed port detected as a server")
	}
}

func TestHostServerModels(t *testing.T) {
	ollama := fakeOllama(t)
	defer ollama.Close()
	lemonade := fakeLemonade(t)
	defer lemonade.Close()

	got, err := hostServerModels(config.ModelManagerBackendOllama, ollama.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := []HostModel{{modelQwen35, true}, {modelGemma270m, false}, {modelSmollm, true}}
	if !slices.Equal(got, want) {
		t.Errorf("ollama models = %v, want %v", got, want)
	}

	got, err = hostServerModels(config.ModelManagerBackendLemonade, lemonade.URL)
	if err != nil {
		t.Fatal(err)
	}
	want = []HostModel{{modelQwen3FLM, true}, {modelGemma4bFLM, false}, {modelMoEFLM, true}}
	if !slices.Equal(got, want) {
		t.Errorf("lemonade models = %v, want %v (downloaded only)", got, want)
	}
}

func TestDetectFLM(t *testing.T) {
	flm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{fieldData: []map[string]any{
			{"id": "qwen3-it:4b", fieldOwnedBy: "FastFlowLM"}, {"id": "gemma3:4b", fieldOwnedBy: "FastFlowLM"},
		}})
	}))
	defer flm.Close()
	got := detectFLM(flm.URL, 52625)
	if got == nil || got.Models != 2 || got.Port != 52625 {
		t.Fatalf("detectFLM = %+v", got)
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{fieldData: []map[string]any{{"id": "x", fieldOwnedBy: "vllm"}}}) // a vLLM, not FLM
	}))
	defer other.Close()
	if detectFLM(other.URL, 8000) != nil {
		t.Fatalf("a non-FLM OpenAI-compatible server detected as FLM")
	}
}

func TestHostModelConfigName(t *testing.T) {
	cases := map[[2]string]string{
		{lemonade, modelQwen3FLM}: "lemonade-qwen3-it-4b-flm",
		{lemonade, modelMoEFLM}:   "lemonade-qwen3-6-moe-35b-a3b-flm",
		{ollama, modelQwen35}:     "ollama-qwen3-5-9b",
		{ollama, "aqualaguna/gemma-3-27b-it-abliterated-GGUF:q4_k_m"}: "ollama-aqualaguna-gemma-3-27b-it-abliterated-gguf-q4-k-m",
	}
	for in, want := range cases {
		got := hostModelConfigName(in[0], in[1])
		if got != want {
			t.Errorf("hostModelConfigName(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
		if err := config.ValidateModelName(got); err != nil {
			t.Errorf("%q is not a valid model name: %v", got, err)
		}
	}
}

// The secondary backends' tool-calling models become ModelConfig entries in
// model-manager's shape for that backend; models the user already wired by
// hand are left to that entry; the primary backend contributes nothing (it is
// model-manager's).
func TestHostBackendModelConfigs(t *testing.T) {
	inventory := map[string][]HostModel{
		ollama:   {{modelQwen35, true}},
		lemonade: {{modelQwen3FLM, true}, {modelGemma4bFLM, false}, {modelQwenVLFLM, true}},
	}
	orig := hostModelsFn
	hostModelsFn = func(backend, _ string) ([]HostModel, error) { return inventory[backend], nil }
	defer func() { hostModelsFn = orig }()

	cfg := config.Default()
	cfg.Platform.ModelManager = config.ModelManager{Enabled: true, Backends: []string{ollama, lemonade}}
	cfg.Platform.ExtraModels = []config.ExtraModel{
		// Timo's hand-wired entry for the same model on the same host:port, on /v1 instead of /api/v1.
		{Name: "lemonade-npu", Provider: config.ProviderOpenAI, Model: modelQwen3FLM, BaseURL: "http://172.21.0.1:13305/v1"},
	}
	endpoints := map[string]string{ollama: "http://172.21.0.1:11434", lemonade: "http://172.21.0.1:13305"}
	got, err := hostBackendModelConfigs(cfg, endpoints)
	if err != nil {
		t.Fatal(err)
	}
	want := []config.ExtraModel{
		{Name: "lemonade-qwen3vl-it-4b-flm", Provider: config.ProviderOpenAI, Model: modelQwenVLFLM, BaseURL: "http://172.21.0.1:13305/api/v1"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("wired = %+v, want %+v", got, want)
	}
	for _, m := range got {
		if err := m.Validate(); err != nil {
			t.Errorf("%s: %v", m.Name, err)
		}
	}

	// Ollama as the further backend: the native keyless provider on the endpoint itself.
	cfg.Platform.ModelManager.Backends = []string{lemonade, ollama}
	cfg.Platform.ExtraModels = nil
	got, err = hostBackendModelConfigs(cfg, endpoints)
	if err != nil {
		t.Fatal(err)
	}
	want = []config.ExtraModel{{Name: "ollama-qwen3-5-9b", Provider: config.ProviderOllama, Model: modelQwen35, BaseURL: "http://172.21.0.1:11434"}}
	if !slices.Equal(got, want) {
		t.Fatalf("wired = %+v, want %+v", got, want)
	}

	// Managed models off: nothing is wired, whatever the list says.
	cfg.Platform.ModelManager.Enabled = false
	if got, _ := hostBackendModelConfigs(cfg, endpoints); len(got) != 0 {
		t.Fatalf("wired with managed models off: %+v", got)
	}
}

func TestDiscoveryBackendsAndHint(t *testing.T) {
	d := &Discovery{Servers: []HostServer{
		{Backend: ollama, Version: ollamaVersion, Port: 11434},
		{Backend: lemonade, Version: lemonadeVersion, Port: 13305},
	}}
	if !slices.Equal(d.Backends(), []string{ollama, lemonade}) {
		t.Fatalf("backends = %v", d.Backends())
	}
	if d.ModelServersHint() != "Ollama 0.33.2 (:11434), Lemonade Server 11.9.0 (:13305)" {
		t.Fatalf("hint = %q", d.ModelServersHint())
	}
	if (&Discovery{}).ModelServersHint() != "" {
		t.Fatalf("empty discovery should hint nothing")
	}
}
