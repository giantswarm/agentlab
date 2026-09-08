package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The chart pin is an exact version: a range would let the lab float onto a
// release nobody ran it with.
func TestValidateChartVersion(t *testing.T) {
	for _, ok := range []string{DefaultChartVersion, "3.20.0", "v3.20.0", "4.0.0-rc.1", "3.20.0+build.7"} {
		if err := ValidateChartVersion(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "3.x", ">=3.20.0 <4.0.0", "3.20", "latest", "^3.20.0", "3.20.0 "} {
		if err := ValidateChartVersion(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

// A dev image names its tag or digest; a bare name would resolve to latest
// and hide which build the lab runs.
func TestValidateImageRef(t *testing.T) {
	for _, ok := range []string{"muster:dev-1a2b", "backstage-dev:tools-84e58101", "giantswarm/muster:dev", "localhost:5000/muster:dev",
		"gsoci.azurecr.io/giantswarm/kagent-controller:0.10.0", "muster@sha256:" + repeat("0", 64)} {
		if err := ValidateImageRef(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "muster", "muster:", "Muster:dev", "muster:dev tag", "muster@sha256:abc"} {
		if err := ValidateImageRef(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

// The chart source in the config: the default pin fills in when a file (an
// older agentlab's, or an emptied key) pins nothing; a chart path must be a
// chart directory; dev images name known components with tagged refs.
const musterComponent = "muster"

func TestChartSourceValidation(t *testing.T) {
	cfg := Default()
	if cfg.Platform.ChartVersion != DefaultChartVersion {
		t.Fatalf("default chartVersion = %q, want %q", cfg.Platform.ChartVersion, DefaultChartVersion)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}

	cfg.Platform.ChartVersion = ""
	cfg.Normalize()
	if cfg.Platform.ChartVersion != DefaultChartVersion {
		t.Errorf("Normalize left chartVersion %q, want the default", cfg.Platform.ChartVersion)
	}

	cfg.Platform.ChartPath = t.TempDir()
	if err := cfg.Validate(); err == nil {
		t.Error("chartPath without a Chart.yaml: want an error")
	}
	if err := os.WriteFile(filepath.Join(cfg.Platform.ChartPath, "Chart.yaml"), []byte("name: agent-platform\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("chartPath with a Chart.yaml: %v", err)
	}

	cfg.Platform.DevImages = map[string]string{"dex": "dex:dev"}
	if err := cfg.Validate(); err == nil {
		t.Error("devImages with an unknown component: want an error")
	}
	cfg.Platform.DevImages = map[string]string{musterComponent: musterComponent}
	if err := cfg.Validate(); err == nil {
		t.Error("devImages with an untagged ref: want an error")
	}
	cfg.Platform.DevImages = map[string]string{musterComponent: "muster:dev-1a2b", "backstage": "backstage-dev:x", "kagent": "kagent-controller:dev"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("devImages for muster, backstage, kagent: %v", err)
	}
}

func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}
