package config

import (
	"os"
	"strings"
	"testing"
)

// loadTestChart is the chart version the load tests write; any exact version
// at or above servingChartFloor (one test turns serving on).
const loadTestChart = "4.44.0"

// A field this release does not know is refused, not dropped: every run
// writes agentlab.yaml back, and a lenient decode by an older release would
// lose a newer release's switch and uninstall what it configured.
func TestLoadRefusesAFieldThisReleaseDoesNotKnow(t *testing.T) {
	t.Chdir(t.TempDir())
	raw := []byte("clusterName: lab\nplatform:\n  chartVersion: " + loadTestChart + "\n  frobnicate:\n    enabled: true\n")
	if err := os.WriteFile(File, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("a config with an unknown field loaded")
	}
	for _, want := range []string{"frobnicate", "does not know", "update agentlab"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	after, err := os.ReadFile(File)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(raw) {
		t.Fatalf("the refused file was rewritten:\n%s", after)
	}
}

// The fields this release knows load as before; an empty file is the defaults.
func TestLoadKnownFieldsAndAnEmptyFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(File, []byte("platform:\n  chartVersion: "+loadTestChart+"\n  serving:\n    enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Platform.Serving.Enabled || cfg.Platform.ChartVersion != loadTestChart {
		t.Fatalf("known fields not loaded: %+v", cfg.Platform)
	}
	if err := os.WriteFile(File, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err != nil {
		t.Fatalf("an empty %s: %v", File, err)
	}
}
