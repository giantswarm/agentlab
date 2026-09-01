package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestExtraModelsTemplate renders the extra-models template across every
// provider shape and asserts the provider-specific spec blocks: the OpenAI
// baseUrl section, the Ollama host mapping, the secret reference wiring, and
// the tls escape hatch.
func TestExtraModelsTemplate(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.ExtraModels = []config.ExtraModel{
		{Name: "qwen3-8-27b", Provider: "OpenAI", Model: "qwen3-8-27b",
			BaseURL: "https://qwen.example.com/v1", InsecureTLS: true},
		{Name: "openrouter-deepseek", Provider: "OpenAI", Model: "deepseek/deepseek-chat",
			BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"},
		{Name: "gemini-flash", Provider: "Gemini", Model: "gemini-2.5-flash", APIKeyEnv: "GEMINI_API_KEY"},
		{Name: "local-llama", Provider: "Ollama", Model: "llama3.3", BaseURL: "http://192.168.1.10:11434"},
		{Name: "claude-proxy", Provider: "Anthropic", Model: "claude-haiku-4-5", BaseURL: "https://proxy.example.com"},
	}
	raw, err := renderTemplate(cfg, "extra-models.yaml.tmpl")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)

	for _, want := range []string{
		// the self-hosted vLLM: baseUrl under openAI, placeholder-backed secret, tls off
		"name: qwen3-8-27b",
		"apiKeySecret: kagent-qwen3-8-27b",
		"apiKeySecretKey: OPENAI_API_KEY",
		"baseUrl: https://qwen.example.com/v1",
		"disableVerify: true",
		// gemini: GOOGLE_API_KEY (the ADK's canonical name), no endpoint block
		"apiKeySecretKey: GOOGLE_API_KEY",
		// ollama: host, not baseUrl
		"host: http://192.168.1.10:11434",
		// anthropic override endpoint
		"apiKeySecretKey: ANTHROPIC_API_KEY",
		"baseUrl: https://proxy.example.com",
		"app.kubernetes.io/managed-by: agentlab",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered extra-models missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "apiKeySecret: kagent-local-llama") {
		t.Errorf("Ollama must not reference a secret:\n%s", out)
	}
	if strings.Count(out, "kind: ModelConfig") != len(cfg.Platform.ExtraModels) {
		t.Errorf("want %d ModelConfigs:\n%s", len(cfg.Platform.ExtraModels), out)
	}

	// No extras -> comments only, nothing to apply (the lifecycle skips
	// kubectl apply on an empty list).
	cfg.Platform.ExtraModels = nil
	raw, err = renderTemplate(cfg, "extra-models.yaml.tmpl")
	if err != nil {
		t.Fatalf("render empty: %v", err)
	}
	if strings.Contains(string(raw), "kind:") {
		t.Errorf("empty extraModels rendered objects:\n%s", raw)
	}
}
