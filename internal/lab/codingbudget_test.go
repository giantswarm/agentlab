package lab

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A ModelConfig's model runs on a host model server when model-manager wired
// it (its backend label) or its endpoint names the lab host; a hosted
// provider's does not, nor a self-hosted endpoint elsewhere.
func TestHostModelServer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]any
		spec   map[string]any
		want   string
		host   bool
	}{
		{
			name:   "model-manager backend",
			labels: map[string]any{modelManagerBackendLabel: "ollama"},
			spec:   map[string]any{"provider": "Ollama", "model": "qwen3.5:9b", "ollama": map[string]any{"host": "http://host.agentlab.internal:11434"}},
			want:   "ollama", host: true,
		},
		{
			name: "extraModels entry on the host's Ollama, Responses API",
			spec: map[string]any{"provider": "OpenAI", "model": "qwen3.5:9b", "openAI": map[string]any{"baseUrl": "http://" + labHostName + ":11434/v1", "apiFormat": "responses"}},
			want: "http://" + labHostName + ":11434/v1", host: true,
		},
		{
			name: "hosted Anthropic",
			spec: map[string]any{"provider": "Anthropic", "model": "claude-sonnet-5-5", "anthropic": map[string]any{}},
		},
		{
			name: "self-hosted endpoint off the lab host",
			spec: map[string]any{"provider": "OpenAI", "model": "gpt-oss", "openAI": map[string]any{"baseUrl": "https://models.example.org/v1"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := map[string]any{"metadata": map[string]any{"name": "mc"}, "spec": tc.spec}
			if tc.labels != nil {
				obj["metadata"].(map[string]any)["labels"] = tc.labels
			}
			got, host := hostModelServer(&unstructured.Unstructured{Object: obj})
			if host != tc.host || got != tc.want {
				t.Errorf("hostModelServer = %q, %v; want %q, %v", got, host, tc.want, tc.host)
			}
		})
	}
}

// A hand-set --turn-timeout wins over the model's budget without reading the
// ModelConfig.
func TestTurnBudgetFlag(t *testing.T) {
	p := &codingProof{turnTimeout: hostedModelTurnTimeout / 2}
	p.setTurnBudget("unread")
	if p.turnTimeout != hostedModelTurnTimeout/2 || p.turnBudget != "--turn-timeout" {
		t.Errorf("turn budget %s (%s), want %s (--turn-timeout)", p.turnTimeout, p.turnBudget, hostedModelTurnTimeout/2)
	}
}

// A small model wraps a relayed probe line in markdown: the values read the
// same as a verbatim relay's.
func TestCodingProbeMarkdownRelay(t *testing.T) {
	p := parseCodingProbe("**AGENTLAB_PROBE env-tokens=0**\n`AGENTLAB_PROBE data-tokens=0`\nAGENTLAB_PROBE egress-origin=https://*.githubusercontent.com\n")
	if p["env-tokens"] != "0" || p["data-tokens"] != "0" || p["egress-origin"] != "https://*.githubusercontent.com" {
		t.Errorf("parsed %v, want env-tokens=0 data-tokens=0 and the origin's wildcard kept", p)
	}
}
