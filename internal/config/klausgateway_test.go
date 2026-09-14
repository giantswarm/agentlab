package config

import (
	"strings"
	"testing"
)

// TestKlausGatewayEnabledNeedsTheAgents: the component is a client of the
// kagent controller, so it rides with the agents and the platform.
func TestKlausGatewayEnabledNeedsTheAgents(t *testing.T) {
	cfg := Default()
	if cfg.KlausGatewayEnabled() {
		t.Fatal("off by default")
	}
	cfg.Platform.KlausGateway.Enabled = true
	if !cfg.KlausGatewayEnabled() {
		t.Fatal("want enabled with the platform and the agents on")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a valid lab: %v", err)
	}
	cfg.Platform.Agents = false
	if cfg.KlausGatewayEnabled() {
		t.Fatal("agents off must disable it")
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "platform.agents") {
		t.Fatalf("want the error naming platform.agents, got %v", err)
	}
	cfg.Platform.Agents = true
	cfg.Platform.Enabled = false
	if cfg.KlausGatewayEnabled() {
		t.Fatal("want disabled with the platform off")
	}
}

// TestDevImagesKnowKlausGateway: the dev-image loop swaps the gateway build
// in while the component is on, and refuses the target while it is off.
func TestDevImagesKnowKlausGateway(t *testing.T) {
	cfg := Default()
	cfg.Platform.DevImages = map[string]string{DevImageKlausGateway: "klaus-gateway:dev"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "platform.klausGateway.enabled") {
		t.Fatalf("without the component the target must be refused naming the key, got %v", err)
	}
	cfg.Platform.KlausGateway.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("klaus-gateway must be a devImages target with the component on: %v", err)
	}
}
