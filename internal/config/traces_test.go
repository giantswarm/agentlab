package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTracesConfiguration(t *testing.T) {
	cfg := Default()
	if cfg.TracesEnabled() {
		t.Fatal("traces must be off by default")
	}
	cfg.Platform.Traces = true
	if !cfg.TracesEnabled() {
		t.Fatal("enabled traces not reported")
	}
	cfg.Platform.GatewayPort = 8443
	if got := cfg.TracesBaseURL(); got != "https://traces.127.0.0.1.nip.io:8443" {
		t.Fatalf("trace UI URL %q", got)
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip := Default()
	if err := decodeStrict(raw, roundTrip); err != nil {
		t.Fatal(err)
	}
	if !roundTrip.Platform.Traces {
		t.Fatal("traces lost in config round trip")
	}
	cfg.Platform.Enabled = false
	if cfg.TracesEnabled() {
		t.Fatal("traces must be inert with platform disabled")
	}
	cfg = Default()
	cfg.Platform.ChartVersion = "3.23.1"
	cfg.Platform.Traces = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "platform.traces") {
		t.Fatalf("legacy chart accepted traces: %v", err)
	}
}
