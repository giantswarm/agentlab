package config

import (
	"slices"
	"strings"
	"testing"
)

// The serving switch: it needs the agents runtime and a chart release that
// carries the serving components, it adds the kserve backend to the list
// model-manager fronts — after the host servers, or alone — and kserve is no
// host server a lab may list by hand.
func TestServingSwitch(t *testing.T) {
	cfg := Default()
	cfg.Platform.Serving.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("serving on with agents on the default pin %s: %v", DefaultChartVersion, err)
	}
	if !cfg.ServingEnabled() || !cfg.ModelManagerEnabled() {
		t.Fatalf("ServingEnabled=%v ModelManagerEnabled=%v, want both on (the switch runs model-manager's kserve backend)", cfg.ServingEnabled(), cfg.ModelManagerEnabled())
	}
	if got := cfg.ChartBackends(); !slices.Equal(got, []string{ModelManagerBackendKServe}) {
		t.Errorf("ChartBackends with no host server = %v, want [kserve]", got)
	}
	cfg.Platform.ModelManager = ModelManager{Enabled: true, Backends: []string{ModelManagerBackendOllama, ModelManagerBackendLemonade}}
	if got := cfg.ChartBackends(); !slices.Equal(got, []string{ModelManagerBackendOllama, ModelManagerBackendLemonade, ModelManagerBackendKServe}) {
		t.Errorf("ChartBackends with host servers = %v, want the host servers first, kserve last", got)
	}
	cfg.Platform.Serving.Enabled = false
	if got := cfg.ChartBackends(); !slices.Equal(got, []string{ModelManagerBackendOllama, ModelManagerBackendLemonade}) {
		t.Errorf("ChartBackends with serving off = %v, want the host servers alone", got)
	}
	cfg.Platform.ModelManager.Enabled = false
	if got := cfg.ChartBackends(); got != nil {
		t.Errorf("ChartBackends with neither = %v, want none", got)
	}

	cfg = Default()
	cfg.Platform.Serving.Enabled = true
	cfg.Platform.Agents = false
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "platform.serving requires platform.agents") {
		t.Errorf("serving without agents: err = %v, want the agents requirement", err)
	}

	cfg = Default()
	cfg.Platform.Serving.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("serving on the default pin %s: %v, want the floor cleared", DefaultChartVersion, err)
	}
	cfg.Platform.ChartVersion = "4.43.0"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "needs agent-platform 4.44.0 or newer") {
		t.Errorf("serving on a pin below the floor: err = %v, want the chart floor", err)
	}
	cfg.Platform.ChartBranch = mainBranch
	if err := cfg.Validate(); err != nil {
		t.Errorf("serving on a branch build: %v, want the floor waived", err)
	}

	mm := ModelManager{Enabled: true, Backends: []string{ModelManagerBackendKServe}}
	if err := mm.Validate(true); err == nil || !strings.Contains(err.Error(), "platform.serving adds it") {
		t.Errorf("kserve listed by hand: err = %v, want the pointer to platform.serving", err)
	}
	if slices.Contains(ModelManagerBackends, ModelManagerBackendKServe) {
		t.Error("kserve is no host server and must not be in ModelManagerBackends")
	}
}
