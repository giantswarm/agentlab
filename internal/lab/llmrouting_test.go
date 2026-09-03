package lab

import (
	"fmt"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestPlatformValuesLLMRoutingOff: the toggle is off by default and the
// values must then carry no llmRouting key at all. The released umbrella does
// not declare one, and its values.schema.json rejects an undeclared top-level
// key outright — a stray block would fail every default install.
func TestPlatformValuesLLMRoutingOff(t *testing.T) {
	cfg := config.Default()
	if cfg.Platform.LLMRouting {
		t.Fatalf("platform.llmRouting must default to false (the chart change is unreleased)")
	}
	out := renderPlatformValues(t, cfg)
	if strings.Contains(out, "llmRouting") {
		t.Errorf("llmRouting renders with the toggle off:\n%s", out)
	}
	if strings.Contains(out, "baseUrl: http://agentgateway.") {
		t.Errorf("the ModelConfig cutover renders with the toggle off:\n%s", out)
	}
}

// TestPlatformValuesLLMRoutingOn pins the two halves that must land together:
// the listener the chart renders, and the base URL kagent's default
// ModelConfig dials. One without the other is a broken lab — a listener
// nothing uses, or agents pointed at a port with nothing behind it.
func TestPlatformValuesLLMRoutingOn(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.LLMRouting = true
	out := renderPlatformValues(t, cfg)

	wantURL := fmt.Sprintf("baseUrl: http://agentgateway.%s.svc:%d", platformNamespace, llmListenerPort)
	for _, want := range []string{
		"llmRouting:",
		"enabled: true",
		fmt.Sprintf("port: %d", llmListenerPort),
		wantURL,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered values missing %q:\n%s", want, out)
		}
	}
	// The listener port and the port in the base URL come from one constant;
	// this asserts the rendered pair agrees, which a hand-edited template
	// could break silently (the agents would dial a closed port).
	listener := out[strings.Index(out, "llmRouting:"):]
	if end := strings.Index(listener, "\ncomponents:"); end > 0 {
		listener = listener[:end]
	}
	if !strings.Contains(listener, fmt.Sprintf("port: %d", llmListenerPort)) {
		t.Errorf("the llm listener does not carry port %d:\n%s", llmListenerPort, listener)
	}
}

// TestLLMRoutingNeedsAgents: the cutover points kagent's default ModelConfig
// at the listener, so the toggle is meaningless without the runtime and the
// config refuses it rather than rendering a listener nothing calls.
func TestLLMRoutingNeedsAgents(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.LLMRouting = true
	cfg.Platform.Agents = false
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("llmRouting without agents must not validate")
	}
	if !strings.Contains(err.Error(), "platform.llmRouting needs platform.agents") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestPlatformChartDir: apsPath replaces the vendored checkout as the chart
// root, and both roots use the same repository layout.
func TestPlatformChartDir(t *testing.T) {
	cfg := config.Default()
	if got, want := platformChartDir(cfg), apsDir+"/helm/agent-platform-standalone"; got != want {
		t.Errorf("vendored chart dir = %q, want %q", got, want)
	}
	cfg.Platform.APSPath = "/tmp/aps"
	if got, want := platformChartDir(cfg), "/tmp/aps/helm/agent-platform-standalone"; got != want {
		t.Errorf("local chart dir = %q, want %q", got, want)
	}
}

func renderPlatformValues(t *testing.T, cfg *config.Config) string {
	t.Helper()
	raw, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return string(raw)
}
