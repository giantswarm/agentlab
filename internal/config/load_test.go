package config

import (
	"os"
	"path/filepath"
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

// The refusal of a field this release does not know names the agentlab that
// wrote the file, as its header records it, and the command that installs it.
func TestLoadRefusalNamesTheWritingRelease(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		want         []string
	}{
		{"a release", "# agentlab lab configuration.\n# Written by agentlab v9.1.0.\n", []string{"written by agentlab v9.1.0", "`agentlab self-update` installs it"}},
		{"a release candidate", "# Written by agentlab v9.1.0-rc.2.\n", []string{"written by agentlab v9.1.0-rc.2", "`agentlab self-update --pre` installs it"}},
		{"no header", "", []string{"written by a newer release", "`agentlab self-update`"}},
		{"a line after the header", "clusterName: lab\n# Written by agentlab v9.1.0.\n", []string{"written by a newer release"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			raw := tc.header + "clusterName: lab\nplatform:\n  chartVersion: " + loadTestChart + "\n  frobnicate: true\n"
			if tc.name == "a line after the header" {
				raw = tc.header + "platform:\n  frobnicate: true\n"
			}
			if err := os.WriteFile(File, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load()
			if err == nil {
				t.Fatal("a config with an unknown field loaded")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
		})
	}
}

// Every write records the agentlab that wrote the file, as a comment an older
// release reads past.
func TestSaveRecordsTheWritingRelease(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := Default()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(File)
	if err != nil {
		t.Fatal(err)
	}
	if got := writtenBy(raw); got == "" {
		t.Fatalf("no writing release in:\n%s", raw)
	}
	if _, err := Load(); err != nil {
		t.Fatalf("the written file does not load: %v", err)
	}
}

// A file an earlier agentlab wrote in a shape this one refuses (workspaces
// without the agents) fails Load naming the requirement, and loads for
// configure, which validates the repaired result.
func TestLoadForConfigureTakesARefusedShape(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(File, []byte("clusterName: lab\nplatform:\n  chartVersion: "+loadTestChart+"\n  agents: false\n  workspaces:\n    enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "platform.workspaces requires platform.agents") {
		t.Fatalf("Load: want the agents requirement, got %v", err)
	}
	cfg, err := LoadForConfigure()
	if err != nil {
		t.Fatalf("LoadForConfigure: %v", err)
	}
	cfg.Platform.Agents = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("the repaired configuration: %v", err)
	}
}

// A command that never writes agentlab.yaml reads a file a newer agentlab
// wrote: the unknown field is read past, the known ones load, the note names
// the field's writer, and the file stays byte for byte. Peek reads it too.
func TestLoadReadOnlyReadsAFileANewerReleaseWrote(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	raw := []byte(writtenByPrefix + "v9.0.0.\nclusterName: newer\nplatform:\n  chartVersion: " + loadTestChart + "\n  gatewayAddress: 127.0.0.2\n  serving:\n    enabled: true\n")
	if err := os.WriteFile(File, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, note, err := LoadReadOnly()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClusterName != "newer" || !cfg.Platform.Serving.Enabled {
		t.Errorf("known fields not loaded: %+v", cfg.Platform)
	}
	for _, want := range []string{"does not know", "v9.0.0", "self-update"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q lacks %q", note, want)
		}
	}
	if peeked, err := Peek(dir); err != nil || peeked.ClusterName != "newer" {
		t.Errorf("Peek = %+v, %v", peeked, err)
	}
	if after, _ := os.ReadFile(File); string(after) != string(raw) {
		t.Fatalf("a read-only load rewrote the file:\n%s", after)
	}
	if _, note, err := LoadReadOnly(); err != nil || note == "" {
		t.Errorf("a second read = %q, %v", note, err)
	}
}

// A file this release reads in full carries no note.
func TestLoadReadOnlyKnownFields(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(File, []byte("clusterName: known\nplatform:\n  chartVersion: "+loadTestChart+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, note, err := LoadReadOnly(); err != nil || note != "" {
		t.Fatalf("LoadReadOnly = %q, %v", note, err)
	}
}

// Before a lab leaves its directory, its relative paths become absolute
// against it; absolute ones and the rest of the file stay.
func TestRebasePaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, File)
	raw := "clusterName: rebased\nplatform:\n  chartVersion: " + loadTestChart + "\n  valuesFiles:\n    - overlay.yaml\n    - /abs/other.yaml\n  vmManager:\n    enabled: false\n    imageDir: images/build\n"
	if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "images", "build"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := RebasePaths(file, dir); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Platform.ValuesFiles; len(got) != 2 || got[0] != filepath.Join(dir, "overlay.yaml") || got[1] != "/abs/other.yaml" {
		t.Errorf("valuesFiles = %v", got)
	}
	if got := cfg.Platform.VMManager.ImageDir; got != filepath.Join(dir, "images/build") {
		t.Errorf("imageDir = %s", got)
	}
	before, _ := os.ReadFile(filepath.Clean(file))
	if err := RebasePaths(file, "/elsewhere"); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(filepath.Clean(file)); string(after) != string(before) {
		t.Errorf("a file without relative paths was rewritten")
	}
}

// A file a newer release wrote is rebased only when it has no relative path:
// a write would drop the field this release does not know.
func TestRebasePathsLeavesANewerFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, File)
	for raw, refused := range map[string]bool{
		"clusterName: newer\nplatform:\n  gatewayAddress: 127.0.0.2\n":                               false,
		"clusterName: newer\nplatform:\n  gatewayAddress: 127.0.0.2\n  valuesFiles:\n    - o.yaml\n": true,
	} {
		if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := RebasePaths(file, dir); (err != nil) != refused {
			t.Errorf("RebasePaths(%q) = %v, want refused %v", raw, err, refused)
		}
		if after, _ := os.ReadFile(filepath.Clean(file)); string(after) != raw {
			t.Errorf("the newer file was rewritten:\n%s", after)
		}
	}
}
