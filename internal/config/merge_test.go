package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// changedLines is what a write changed in agentlab.yaml: the lines of before
// missing from after and of after missing from before, in order.
func changedLines(before, after string) (removed, added []string) {
	b, a := strings.Split(before, "\n"), strings.Split(after, "\n")
	inA := map[string]int{}
	for _, l := range a {
		inA[l]++
	}
	inB := map[string]int{}
	for _, l := range b {
		inB[l]++
	}
	for _, l := range b {
		if inA[l] == 0 {
			removed = append(removed, l)
		} else {
			inA[l]--
		}
	}
	for _, l := range a {
		if inB[l] == 0 {
			added = append(added, l)
		} else {
			inB[l]--
		}
	}
	return removed, added
}

// chartCheckout lays out an agent-platform checkout's two charts under dir
// and returns its helm/agent-platform, the path chartPath takes.
func chartCheckout(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "helm", "agent-platform")
	for _, d := range []string{path, ConnectivityChartDir(path)} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, chartFile), []byte("name: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func readFile(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(File)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// `configure --defaults --chart-path <dir>` on an existing agentlab.yaml
// changes the chart path line and nothing else: non-default tenants and
// portal values, the lab's own comments and its key order stay as they are.
func TestSaveChangesOnlyTheChartPath(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := Default()
	cfg.ClusterName = "lab"
	cfg.Platform.ChartVersion = loadTestChart
	oldPath, newPath := chartCheckout(t, "old"), chartCheckout(t, "new")
	cfg.Platform.ChartPath = oldPath
	cfg.Platform.Tenants.Enabled = true
	cfg.Backstage.Portal = PortalClassic
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	// The lab's own edit: a comment.
	raw := strings.Replace(readFile(t), "clusterName: lab\n", "clusterName: lab # the lease's lab\n", 1)
	if err := os.WriteFile(File, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, path, removed, added string }{
		{"changed", newPath, "    chartPath: " + oldPath, "    chartPath: " + newPath},
		{"cleared", "", "    chartPath: " + newPath, ""},
		{"set", oldPath, "", "    chartPath: " + oldPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := readFile(t)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Platform.ChartPath = tc.path
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
			after := readFile(t)
			removed, added := changedLines(before, after)
			if strings.Join(removed, "\n") != tc.removed || strings.Join(added, "\n") != tc.added {
				t.Fatalf("want -%q +%q, got -%q +%q in:\n%s", tc.removed, tc.added, removed, added, after)
			}
			// The chart path sits where a fresh file has it.
			if tc.path != "" && !strings.Contains(after, "    chartVersion: "+loadTestChart+"\n    chartPath: "+tc.path+"\n") {
				t.Fatalf("chartPath not after chartVersion in:\n%s", after)
			}
			got, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if !got.Platform.Tenants.Enabled || got.Backstage.Portal != PortalClassic || got.Platform.ChartPath != tc.path {
				t.Fatalf("values lost: tenants=%v portal=%q chartPath=%q", got.Platform.Tenants.Enabled, got.Backstage.Portal, got.Platform.ChartPath)
			}
		})
	}
}

// A key the file lacks (a field a newer release added) is added with its
// default; every line the file has stays, its indentation included.
func TestSaveAddsAbsentKeysOnly(t *testing.T) {
	t.Chdir(t.TempDir())
	// Indented by two, the backstage block first.
	raw := "# my lab\nclusterName: lab # mine\nbackstage:\n  enabled: true\n  portal: classic\nplatform:\n  enabled: true\n  agents: true\n  chartVersion: " + loadTestChart + "\n  tenants:\n    enabled: true\n"
	if err := os.WriteFile(File, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	after := readFile(t)
	if removed, _ := changedLines(raw, after); len(removed) != 0 {
		t.Fatalf("lines of the lab's file changed: %q in:\n%s", removed, after)
	}
	for _, want := range []string{"  enabled: true\n  port: 7007\n  portal: classic\n", "  tenants:\n    enabled: true\n", "dexPort: 32000\n"} {
		if !strings.Contains(after, want) {
			t.Errorf("want %q in:\n%s", want, after)
		}
	}
	if strings.Count(after, headerFirstLine) != 1 {
		t.Errorf("the header is not written exactly once:\n%s", after)
	}
	// A second write changes nothing.
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if again := readFile(t); again != after {
		t.Fatalf("a second write changed the file:\n%s\nto:\n%s", after, again)
	}
}

// Usage data on saves no telemetry key, so a release from before the key,
// which refuses keys it does not know, still writes the lab; off saves
// telemetry: false, and on again removes it.
func TestSaveWritesTelemetryOnlyWhenOff(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := Default()
	cfg.Platform.ChartVersion = loadTestChart
	for _, on := range []bool{true, false, true} {
		cfg.SetTelemetry(on)
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(readFile(t), "telemetry:"); got == on {
			t.Fatalf("on=%v: telemetry key present=%v in:\n%s", on, got, readFile(t))
		}
		got, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if got.TelemetryOn() != on {
			t.Fatalf("on=%v: read back %v", on, got.TelemetryOn())
		}
	}
}
