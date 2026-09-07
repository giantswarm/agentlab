package lab

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// Fixture vocabulary, hoisted so the linter's constant check stays quiet.
const (
	ollama          = config.ModelManagerBackendOllama
	lemonade        = config.ModelManagerBackendLemonade
	lmstudio        = config.ModelManagerBackendLMStudio
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
	fieldSize       = "size"
	labelVision     = "vision"
	// LM Studio's inventory fields.
	fieldModels       = "models"
	fieldKey          = "key"
	fieldType         = "type"
	fieldSizeBytes    = "size_bytes"
	fieldCapabilities = "capabilities"
	fieldToolUse      = "trained_for_tool_use"
	typeLLM           = "llm"
	typeEmbedding     = "embedding"
	modelGranite      = "ibm/granite-4-micro"
	modelQwen317b     = "qwen/qwen3-1.7b"
	modelNomicEmbed   = "text-embedding-nomic-embed-text-v1.5"
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
		sizes := map[string]int64{modelQwen35: 6_000_000_000, modelGemma270m: 300_000_000, modelSmollm: 270_000_000}
		var models []map[string]any
		for _, name := range []string{modelQwen35, modelGemma270m, modelSmollm} {
			models = append(models, map[string]any{"name": name, fieldSize: sizes[name]})
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
			{"id": modelQwen3FLM, fieldDownloaded: true, fieldLabels: []string{lemonadeToolsLabel, labelChat}, "recipe": "flm", fieldSize: 3.1},
			{"id": modelGemma4bFLM, fieldDownloaded: true, fieldLabels: []string{labelVision, labelChat}, "recipe": "flm", fieldSize: 4.5},
			{"id": modelMoEFLM, fieldDownloaded: true, fieldLabels: []string{labelVision, "reasoning", lemonadeToolsLabel, labelChat}, fieldSize: 24.3},
			{"id": "not-yet", fieldDownloaded: false, fieldLabels: []string{lemonadeToolsLabel}, fieldSize: 1},
		}})
	})
	return httptest.NewServer(mux)
}

// lmStudioMux answers /api/v1/models like an LM Studio and — the trait the
// whole detection design rests on — HTTP 200 with an error document for every
// other path, Ollama's /api/version, /api/tags and /api/show among them.
func lmStudioMux(models []map[string]any) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{fieldModels: models})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "Unexpected endpoint or method. (" + r.Method + " " + r.URL.Path + ")",
		})
	})
	return mux
}

// fakeLMStudio has a library: two text models (one trained for tool use) and
// an embedding model, which carries no capability object at all.
func fakeLMStudio(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(lmStudioMux([]map[string]any{
		{fieldKey: modelGranite, fieldType: typeLLM, fieldSizeBytes: 2_100_000_000,
			fieldCapabilities: map[string]any{fieldToolUse: true, labelVision: false}},
		{fieldKey: modelQwen317b, fieldType: typeLLM, fieldSizeBytes: 1_000_000_000,
			fieldCapabilities: map[string]any{fieldToolUse: false}},
		{fieldKey: modelNomicEmbed, fieldType: typeEmbedding, fieldSizeBytes: 84_106_624},
	}))
}

// fakeLMStudioEmpty is a fresh install: still an LM Studio, nothing pulled.
func fakeLMStudioEmpty(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(lmStudioMux([]map[string]any{}))
}

func TestDetectHostServer(t *testing.T) {
	ollamaSrv := fakeOllama(t)
	defer ollamaSrv.Close()
	lemonadeSrv := fakeLemonade(t)
	defer lemonadeSrv.Close()

	if v, ok := detectHostServer(config.ModelManagerBackendOllama, ollamaSrv.URL); !ok || v != ollamaVersion {
		t.Errorf("ollama: %q %v", v, ok)
	}
	if v, ok := detectHostServer(config.ModelManagerBackendLemonade, lemonadeSrv.URL); !ok || v != lemonadeVersion {
		t.Errorf("lemonade: %q %v", v, ok)
	}
	// The wrong server on a port is not a hit: Ollama's path on Lemonade 404s.
	if _, ok := detectHostServer(config.ModelManagerBackendOllama, lemonadeSrv.URL); ok {
		t.Errorf("Lemonade detected as Ollama")
	}
	if _, ok := detectHostServer(config.ModelManagerBackendOllama, "http://127.0.0.1:1"); ok {
		t.Errorf("a closed port detected as a server")
	}

	lmstudioSrv := fakeLMStudio(t)
	defer lmstudioSrv.Close()
	empty := fakeLMStudioEmpty(t)
	defer empty.Close()

	if v, ok := detectHostServer(lmstudio, lmstudioSrv.URL); !ok || v != lmStudioIdentAPIv1 {
		t.Errorf("lmstudio: %q %v", v, ok)
	}
	// A fresh install with nothing downloaded is still an LM Studio.
	if v, ok := detectHostServer(lmstudio, empty.URL); !ok || v != lmStudioIdentAPIv1 {
		t.Errorf("empty lmstudio: %q %v", v, ok)
	}
	// Lemonade serves the very same /api/v1/models, with a `data` envelope
	// instead of `models` — the shape is what tells them apart.
	if _, ok := detectHostServer(lmstudio, lemonadeSrv.URL); ok {
		t.Errorf("Lemonade detected as LM Studio")
	}
	if _, ok := detectHostServer(lmstudio, ollamaSrv.URL); ok {
		t.Errorf("Ollama detected as LM Studio")
	}
	// The reason none of this may key off a status code: LM Studio answers
	// 200 for Ollama's and Lemonade's probe paths alike.
	for _, backend := range []string{config.ModelManagerBackendOllama, config.ModelManagerBackendLemonade} {
		if _, ok := detectHostServer(backend, lmstudioSrv.URL); ok {
			t.Errorf("LM Studio detected as %s — a 200 on an unknown path was read as evidence", backend)
		}
	}
}

// Every backend the lab accepts must be fully described, or it would be
// half-added: detected but not readable, or readable but never detected.
func TestEveryBackendHasAProbeAndAReader(t *testing.T) {
	for _, b := range config.ModelManagerBackends {
		spec, known := backendSpec(b)
		if !known {
			t.Errorf("backend %q has no table entry", b)
			continue
		}
		if spec.probe.path == "" || spec.probe.marker == "" || spec.probe.detail == nil || spec.probe.ident == nil {
			t.Errorf("backend %q has an incomplete probe: %+v", b, spec.probe)
		}
		if spec.models == nil {
			t.Errorf("backend %q has no inventory reader", b)
		}
		if spec.bindFix == "" || spec.bindHint == "" {
			t.Errorf("backend %q has no host-side bind fix", b)
		}
		if spec.proofModel == "" || spec.provider == "" || spec.providerNote == "" {
			t.Errorf("backend %q has no models-test expectations", b)
		}
	}
}

func TestUnknownBackendIsNotRead(t *testing.T) {
	if _, err := hostServerModels("kserve", "http://127.0.0.1:1"); err == nil {
		t.Error("hostServerModels read an unknown backend instead of failing")
	}
	if _, ok := detectHostServer("kserve", "http://127.0.0.1:1"); ok {
		t.Error("detectHostServer claimed an unknown backend")
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
	want := []HostModel{{modelQwen35, true, 6_000_000_000}, {modelGemma270m, false, 300_000_000}, {modelSmollm, true, 270_000_000}}
	if !slices.Equal(got, want) {
		t.Errorf("ollama models = %v, want %v", got, want)
	}

	got, err = hostServerModels(config.ModelManagerBackendLemonade, lemonade.URL)
	if err != nil {
		t.Fatal(err)
	}
	want = []HostModel{{modelQwen3FLM, true, 3_100_000_000}, {modelGemma4bFLM, false, 4_500_000_000}, {modelMoEFLM, true, 24_300_000_000}}
	if !slices.Equal(got, want) {
		t.Errorf("lemonade models = %v, want %v (downloaded only, decimal GB -> bytes)", got, want)
	}

	lmstudioSrv := fakeLMStudio(t)
	defer lmstudioSrv.Close()
	got, err = hostServerModels(lmstudio, lmstudioSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// The embedding model is not an agent model; size_bytes needs no
	// conversion, unlike Lemonade's decimal GB.
	want = []HostModel{{modelGranite, true, 2_100_000_000}, {modelQwen317b, false, 1_000_000_000}}
	if !slices.Equal(got, want) {
		t.Errorf("lmstudio models = %v, want %v (LLMs only, size_bytes as is)", got, want)
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

func TestDiscoveryBackendsAndHint(t *testing.T) {
	d := &Discovery{Servers: []HostServer{
		{Backend: ollama, Ident: ollamaVersion, Port: 11434},
		{Backend: lemonade, Ident: lemonadeVersion, Port: 13305},
		{Backend: lmstudio, Ident: lmStudioIdentAPIv1, Port: 1234},
	}}
	if !slices.Equal(d.Backends(), []string{ollama, lemonade, lmstudio}) {
		t.Fatalf("backends = %v", d.Backends())
	}
	// LM Studio reports no version, so the hint carries its API generation.
	if d.ModelServersHint() != "Ollama 0.33.2 (:11434), Lemonade Server 11.9.0 (:13305), LM Studio api v1 (:1234)" {
		t.Fatalf("hint = %q", d.ModelServersHint())
	}
	if (&Discovery{}).ModelServersHint() != "" {
		t.Fatalf("empty discovery should hint nothing")
	}
}

// The node-side dial answers about the SERVER only when it actually ran: a
// refused connection (bash exits 1) and a hang (`timeout` exits 124) are
// verdicts, every other exit is the probe itself failing and must not be
// reported as an unreachable server.
func TestHostServerAnswersUnderPodman(t *testing.T) {
	for name, tc := range map[string]struct {
		exit      int
		want      bool
		wantProbe bool // the probe failed, so there is no verdict
	}{
		"server answers":     {0, true, false},
		"connection refused": {1, false, false},
		"dial hangs":         {124, false, false},
		"no bash in node":    {127, false, true},
		"exec not permitted": {126, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			installFakeTool(t, dir, "docker", "exit "+strconv.Itoa(tc.exit))
			withPodman(t, true)
			got, err := hostServerAnswers("agentlab-control-plane", "169.254.1.2:11434")
			if (err != nil) != tc.wantProbe {
				t.Fatalf("err = %v, want probe failure %v", err, tc.wantProbe)
			}
			if !tc.wantProbe && got != tc.want {
				t.Errorf("answers = %v, want %v", got, tc.want)
			}
		})
	}
}

// The tools line separates what was found on PATH — docker, or its absence —
// from what the binary carries, so a report never lists an embedded library
// as missing and a reader sees at a glance that docker is all there is to
// install.
func TestReportToolsLine(t *testing.T) {
	out := (&Discovery{Tools: toolVersions("29.7.2")}).Report(config.Default())
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "tools") {
			line = l
		}
	}
	wants := []string{"tools             docker 29.7.2 — embedded: kind v", " (kindest/node:v", ", helm", ", client-go"}
	if buildInfoHasDeps() {
		wants = append(wants, ", helm v", ", client-go v")
	}
	for _, want := range wants {
		if !strings.Contains(line, want) {
			t.Errorf("tools line %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "MISSING") || strings.Contains(line, "kubectl") {
		t.Errorf("tools line %q names something missing, or kubectl", line)
	}

	out = (&Discovery{Tools: toolVersions("")}).Report(config.Default())
	if !strings.Contains(out, "tools             docker MISSING — embedded: kind v") {
		t.Errorf("a missing docker is reported as such, the embedded tools still listed:\n%s", out)
	}
}

// A probe that could not run leaves OnGateway unset, and the report says so
// instead of blaming the server's bind address.
func TestReportSaysWhenTheProbeCouldNotRun(t *testing.T) {
	d := &Discovery{
		KindGateway: "169.254.1.2",
		Servers: []HostServer{{
			Backend:  config.ModelManagerBackendOllama,
			Ident:    "0.20.2",
			Port:     config.OllamaPort,
			ReachErr: errors.New("no bash in the node"),
		}},
	}
	out := d.Report(config.Default())
	if !strings.Contains(out, "cannot tell whether pods reach it on 169.254.1.2") {
		t.Errorf("report does not name the failed probe:\n%s", out)
	}
	if strings.Contains(out, "OLLAMA_HOST=0.0.0.0") {
		t.Errorf("report blames the server's bind address for a probe failure:\n%s", out)
	}
}
