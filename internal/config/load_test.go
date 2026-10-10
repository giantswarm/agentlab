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

// The fields this release knows load as before; a file without clusterName,
// an empty one included, is refused: no lab is the default.
func TestLoadKnownFieldsAndANamelessFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(File, []byte("clusterName: lab\nplatform:\n  chartVersion: "+loadTestChart+"\n  serving:\n    enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Platform.Serving.Enabled || cfg.Platform.ChartVersion != loadTestChart {
		t.Fatalf("known fields not loaded: %+v", cfg.Platform)
	}
	for _, raw := range []string{"", "platform:\n  chartVersion: " + loadTestChart + "\n"} {
		if err := os.WriteFile(File, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "clusterName") {
			t.Errorf("%q: want a refusal naming clusterName, got %v", raw, err)
		}
	}
}

// The retired platform.fakeFleet key, which every file an earlier release
// wrote carries, still loads and is not written back.
func TestLoadDropsTheRetiredFakeFleet(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(File, []byte("clusterName: lab\nplatform:\n  chartVersion: "+loadTestChart+"\n  fakeFleet: true\n"), 0o600); err != nil {
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
	if err := os.WriteFile(File, []byte("clusterName: lab\nplatform:\n  chartVersion: 4.92.0\n"), 0o600); err != nil {
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

// An upgrade seed installs a released 4.x chart below the floor in the
// family-less shape of its line; the refusal without it names the switch.
// Upgraded to the current line, the same file is the family shape again.
func TestUpgradeSeed(t *testing.T) {
	cfg := Default()
	cfg.Platform.ChartVersion = "4.66.5"
	if err := cfg.CheckFamiliesChartFloor(); err == nil || !strings.Contains(err.Error(), "--upgrade-seed") {
		t.Errorf("without the switch: want the switch named, got %v", err)
	}
	cfg.Platform.UpgradeSeed = true
	if err := cfg.CheckFamiliesChartFloor(); err != nil {
		t.Errorf("an upgrade seed: %v", err)
	}
	if !cfg.FamilylessChart() || cfg.MCPServerName() != "mcp-kubernetes" {
		t.Errorf("an upgrade seed on 4.66.5: want the family-less mcp-kubernetes, got familyless=%v %s", cfg.FamilylessChart(), cfg.MCPServerName())
	}
	cfg.Platform.ChartVersion = DefaultChartVersion
	if cfg.FamilylessChart() || cfg.MCPServerName() != cfg.ClusterName+"-mcp-kubernetes" {
		t.Errorf("upgraded to %s: want the family member, got familyless=%v %s", DefaultChartVersion, cfg.FamilylessChart(), cfg.MCPServerName())
	}
	cfg.Platform.ChartVersion = legacyChart
	if !cfg.FamilylessChart() {
		t.Error("the 3.x line is family-less")
	}
}

// A lab whose values overlay is gone still loads: `down` and `configure` need
// no overlay, and the missing file must not read as a missing agentlab.yaml
// (os.ErrNotExist), which sent `down` to "no agentlab.yaml found" and would
// let `configure` start over from the defaults.
func TestLoadWithAMissingValuesFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(File, []byte("clusterName: lab\nplatform:\n  chartVersion: "+loadTestChart+"\n  valuesFiles:\n    - gone/overlay.yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("a missing values file failed Load: %v", err)
	}
	if err := cfg.CheckValuesFiles(); err == nil || !strings.Contains(err.Error(), "platform.valuesFiles[0]: gone/overlay.yaml: no such file") {
		t.Fatalf("CheckValuesFiles: want the entry named, got %v", err)
	}
}
