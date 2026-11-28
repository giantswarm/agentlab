package config

import (
	"strings"
	"testing"
)

// TestBackstageLocalDevPort pins the hot-reload backend's validation and URL:
// off by default, refused on the port kind publishes for the in-cluster pod.
func TestBackstageLocalDevPort(t *testing.T) {
	c := Default()
	if got := c.BackstageLocalDevURL(); got != "" {
		t.Errorf("default BackstageLocalDevURL = %q, want none", got)
	}
	c.Backstage.LocalDevPort = DefaultBackstageLocalDevPort
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate with localDevPort %d: %v", DefaultBackstageLocalDevPort, err)
	}
	if got, want := c.BackstageLocalDevURL(), "http://localhost:7017"; got != want {
		t.Errorf("BackstageLocalDevURL = %q, want %q", got, want)
	}
	// Every port the lab or the hot-reload run itself holds is refused.
	for name, port := range map[string]int{
		"backstage.port":       c.Backstage.Port,
		"the frontend's":       BackstageFrontendPort,
		"dexPort":              c.DexPort,
		"apiServerPort":        c.APIServerPort,
		"platform.musterPort":  c.Platform.MusterPort,
		"platform.gatewayPort": c.Platform.GatewayPort,
		"platform.agentsPort":  c.Platform.AgentsPort,
	} {
		c.Backstage.LocalDevPort = port
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "backstage.localDevPort") {
			t.Errorf("Validate with localDevPort == %s (%d): %v, want a backstage.localDevPort refusal", name, port, err)
		}
	}
}

// TestBackstageLocalDevFrontendURL: the frontend origin of a hot-reload run
// (muster's return allowlist) follows localDevPort being set, and names the
// frontend's port, not the backend's.
func TestBackstageLocalDevFrontendURL(t *testing.T) {
	c := Default()
	if got := c.BackstageLocalDevFrontendURL(); got != "" {
		t.Errorf("default BackstageLocalDevFrontendURL = %q, want none", got)
	}
	c.Backstage.LocalDevPort = 7099
	if got, want := c.BackstageLocalDevFrontendURL(), "http://localhost:3000"; got != want {
		t.Errorf("BackstageLocalDevFrontendURL = %q, want %q", got, want)
	}
}
