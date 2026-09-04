package config

import (
	"strings"
	"testing"
)

func TestExtraModelValidate(t *testing.T) {
	valid := ExtraModel{Name: "qwen3-8-27b", Provider: ProviderOpenAI, Model: "qwen3-8-27b",
		BaseURL: "http://192.168.1.10:8000/v1"}

	cases := []struct {
		name    string
		mutate  func(m ExtraModel) ExtraModel
		wantErr string // substring; empty = valid
	}{
		{"openai with base url", func(m ExtraModel) ExtraModel { return m }, ""},
		{"openai without base url", func(m ExtraModel) ExtraModel { m.BaseURL = ""; return m }, ""},
		{"openrouter with key env", func(m ExtraModel) ExtraModel {
			m.BaseURL = "https://openrouter.ai/api/v1"
			m.APIKeyEnv = "OPENROUTER_API_KEY"
			return m
		}, ""},
		{"gemini", func(m ExtraModel) ExtraModel {
			m.Provider = ProviderGemini
			m.BaseURL = ""
			m.APIKeyEnv = "GEMINI_API_KEY"
			return m
		}, ""},
		{"anthropic base url", func(m ExtraModel) ExtraModel {
			m.Provider = ProviderAnthropic
			return m
		}, ""},
		{ModelManagerBackendOllama, func(m ExtraModel) ExtraModel {
			m.Provider = ProviderOllama
			m.BaseURL = "http://192.168.1.10:11434"
			return m
		}, ""},
		{"bad name", func(m ExtraModel) ExtraModel { m.Name = "Qwen_3"; return m }, "lowercase"},
		{"reserved default name", func(m ExtraModel) ExtraModel { m.Name = "default-model-config"; return m }, "reserved"},
		{"reserved anthropic name", func(m ExtraModel) ExtraModel { m.Name = "anthropic"; return m }, "reserved"},
		{"unknown provider", func(m ExtraModel) ExtraModel { m.Provider = "Bedrock"; return m }, "unknown provider"},
		{"missing model", func(m ExtraModel) ExtraModel { m.Model = ""; return m }, "model is required"},
		{"gemini with base url", func(m ExtraModel) ExtraModel {
			m.Provider = ProviderGemini
			return m
		}, "no baseUrl"},
		{"ollama without base url", func(m ExtraModel) ExtraModel {
			m.Provider = ProviderOllama
			m.BaseURL = ""
			return m
		}, "requires baseUrl"},
		{"ollama with key env", func(m ExtraModel) ExtraModel {
			m.Provider = ProviderOllama
			m.BaseURL = "http://h:11434"
			m.APIKeyEnv = "X"
			return m
		}, "keyless"},
		{"bad key env", func(m ExtraModel) ExtraModel { m.APIKeyEnv = "not a var"; return m }, "environment variable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mutate(valid).Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestExtraModelSecretWiring(t *testing.T) {
	m := ExtraModel{Name: "qwen", Provider: ProviderOpenAI}
	if m.SecretName() != "kagent-qwen" || m.SecretKey() != "OPENAI_API_KEY" || !m.NeedsSecret() {
		t.Errorf("OpenAI wiring: secret=%s key=%s needs=%v", m.SecretName(), m.SecretKey(), m.NeedsSecret())
	}
	if (ExtraModel{Provider: ProviderGemini}).SecretKey() != "GOOGLE_API_KEY" {
		t.Errorf("Gemini must map to GOOGLE_API_KEY (what the ADK runtime reads)")
	}
	if (ExtraModel{Provider: ProviderOllama}).NeedsSecret() {
		t.Errorf("Ollama is keyless")
	}
}

func TestConfigValidateExtraModels(t *testing.T) {
	cfg := Default()
	cfg.Platform.ExtraModels = []ExtraModel{
		{Name: "dup-model", Provider: ProviderOpenAI, Model: "some-model"},
		{Name: "dup-model", Provider: ProviderOpenAI, Model: "other-model"},
	}
	if _, err := cfg.EnsureHashes(); err != nil {
		t.Fatal(err)
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Fatalf("duplicate models not rejected: %v", err)
	}
	cfg.Platform.ExtraModels = cfg.Platform.ExtraModels[:1]
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid extra model rejected: %v", err)
	}
}

func TestModelManagerValidate(t *testing.T) {
	cases := []struct {
		name    string
		mm      ModelManager
		agents  bool
		wantErr string
	}{
		{"off", ModelManager{}, false, ""},
		{"on with agents", ModelManager{Enabled: true, Backend: ModelManagerBackendOllama}, true, ""},
		{"on, endpoint override", ModelManager{Enabled: true, Endpoint: "http://192.168.1.10:11434"}, true, ""},
		{"on without agents", ModelManager{Enabled: true}, false, "requires platform.agents"},
		{"kserve", ModelManager{Enabled: true, Backend: "kserve"}, true, "supports ollama, lemonade"},
		{"lemonade legacy form", ModelManager{Enabled: true, Backend: ModelManagerBackendLemonade, Endpoint: "http://172.21.0.1:13305"}, true, ""},
		{"both backends", ModelManager{Enabled: true, Backends: []string{ModelManagerBackendOllama, ModelManagerBackendLemonade}}, true, ""},
		{"duplicate backend", ModelManager{Enabled: true, Backends: []string{ModelManagerBackendOllama, ModelManagerBackendOllama}}, true, "listed twice"},
		{"endpoint for a backend not listed", ModelManager{Enabled: true, Backends: []string{ModelManagerBackendOllama}, Endpoints: map[string]string{ModelManagerBackendLemonade: "http://h:13305"}}, true, "not in backends"},
		{"bad per-backend endpoint", ModelManager{Enabled: true, Backends: []string{ModelManagerBackendLemonade}, Endpoints: map[string]string{ModelManagerBackendLemonade: "h:13305"}}, true, "http(s) URL"},
		{"bad endpoint", ModelManager{Enabled: true, Endpoint: "172.21.0.1:11434"}, true, "http(s) URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mm.Validate(tc.agents)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestModelManagerEnabledNeedsPlatformAndAgents(t *testing.T) {
	cfg := Default()
	cfg.Platform.ModelManager.Enabled = true
	if !cfg.ModelManagerEnabled() {
		t.Fatalf("enabled with the default platform+agents should report on")
	}
	cfg.Platform.Agents = false
	if cfg.ModelManagerEnabled() {
		t.Fatalf("agents off must switch model-manager off")
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "platform.modelManager") {
		t.Fatalf("validate should reject model-manager without agents, got %v", err)
	}
	if cfg.ModelManagerBaseURL() != "https://agentgateway.127.0.0.1.nip.io/model-manager" {
		t.Fatalf("unexpected model-manager base URL %q", cfg.ModelManagerBaseURL())
	}
}
