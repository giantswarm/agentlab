package config

import (
	"os"
	"strings"
	"testing"
)

// loadTestChart is the chart version the load tests write; any exact version
// at or above servingChartFloor (one test turns serving on) and
// familiesChartFloor.
const loadTestChart = "4.93.0"

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

// The retired platform.fakeFleet key, which every file an earlier release
// wrote carries, still loads and is not written back.
func TestLoadDropsTheRetiredFakeFleet(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(File, []byte("platform:\n  chartVersion: "+loadTestChart+"\n  fakeFleet: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(File)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "fakeFleet") {
		t.Errorf("the retired key was written back:\n%s", after)
	}
}

// A released 4.x chart before familiesChartFloor is refused at the install
// (its connectivity chart would register the family-less mcp-kubernetes),
// naming the command that moves the pin; such a file still loads, so that
// command works. The 3.x line and a branch build are not refused.
func TestCheckFamiliesChartFloor(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(File, []byte("platform:\n  chartVersion: 4.92.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("a file pinned below the floor must load: %v", err)
	}
	if err := cfg.CheckFamiliesChartFloor(); err == nil || !strings.Contains(err.Error(), familiesChartFloor.String()) || !strings.Contains(err.Error(), "--chart-version") {
		t.Errorf("4.92.0: want the floor and the fix named, got %v", err)
	}
	cfg.Platform.ChartVersion = legacyChart
	if err := cfg.CheckFamiliesChartFloor(); err != nil {
		t.Errorf("the 3.x line: %v", err)
	}
	cfg.Platform.ChartVersion = "4.92.0"
	cfg.Platform.ChartBranch = mainBranch
	if err := cfg.CheckFamiliesChartFloor(); err != nil {
		t.Errorf("a branch build: %v", err)
	}
	cfg.Platform.ChartBranch = ""
	cfg.Platform.ChartVersion = DefaultChartVersion
	if err := cfg.CheckFamiliesChartFloor(); err != nil {
		t.Errorf("the default: %v", err)
	}
}
