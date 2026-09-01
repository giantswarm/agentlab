package config

import (
	"strings"
	"testing"
)

func TestExtraModelValidate(t *testing.T) {
	valid := ExtraModel{Name: "qwen3-8-27b", Provider: "OpenAI", Model: "qwen3-8-27b",
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
			m.Provider = "Gemini"
			m.BaseURL = ""
			m.APIKeyEnv = "GEMINI_API_KEY"
			return m
		}, ""},
		{"anthropic base url", func(m ExtraModel) ExtraModel {
			m.Provider = "Anthropic"
			return m
		}, ""},
		{"ollama", func(m ExtraModel) ExtraModel {
			m.Provider = "Ollama"
			m.BaseURL = "http://192.168.1.10:11434"
			return m
		}, ""},
		{"bad name", func(m ExtraModel) ExtraModel { m.Name = "Qwen_3"; return m }, "lowercase"},
		{"reserved default name", func(m ExtraModel) ExtraModel { m.Name = "default-model-config"; return m }, "reserved"},
		{"reserved anthropic name", func(m ExtraModel) ExtraModel { m.Name = "anthropic"; return m }, "reserved"},
		{"unknown provider", func(m ExtraModel) ExtraModel { m.Provider = "Bedrock"; return m }, "unknown provider"},
		{"missing model", func(m ExtraModel) ExtraModel { m.Model = ""; return m }, "model is required"},
		{"gemini with base url", func(m ExtraModel) ExtraModel {
			m.Provider = "Gemini"
			return m
		}, "no baseUrl"},
		{"ollama without base url", func(m ExtraModel) ExtraModel {
			m.Provider = "Ollama"
			m.BaseURL = ""
			return m
		}, "requires baseUrl"},
		{"ollama with key env", func(m ExtraModel) ExtraModel {
			m.Provider = "Ollama"
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
	m := ExtraModel{Name: "qwen", Provider: "OpenAI"}
	if m.SecretName() != "kagent-qwen" || m.SecretKey() != "OPENAI_API_KEY" || !m.NeedsSecret() {
		t.Errorf("OpenAI wiring: secret=%s key=%s needs=%v", m.SecretName(), m.SecretKey(), m.NeedsSecret())
	}
	if (ExtraModel{Provider: "Gemini"}).SecretKey() != "GOOGLE_API_KEY" {
		t.Errorf("Gemini must map to GOOGLE_API_KEY (what the ADK runtime reads)")
	}
	if (ExtraModel{Provider: "Ollama"}).NeedsSecret() {
		t.Errorf("Ollama is keyless")
	}
}

func TestConfigValidateExtraModels(t *testing.T) {
	cfg := Default()
	cfg.Platform.ExtraModels = []ExtraModel{
		{Name: "qwen", Provider: "OpenAI", Model: "qwen3-8-27b"},
		{Name: "qwen", Provider: "OpenAI", Model: "qwen3-8-27b"},
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
