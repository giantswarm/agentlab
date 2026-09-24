package config

import (
	"os"
	"path/filepath"
	"strings"
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

// A released 3.x meta chart is the legacy shape; the current line is every
// 4.x release, and the dev channel and a chart directory whatever their
// version says. The channel follows the install's precedence: a chart
// directory over a branch over the pinned release.
func TestLegacyChart(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		version, branch, chartDir string
		major                     uint64
		channel                   string
		legacy                    bool
	}{
		{"3.x release", "3.23.1", "", "", 3, ChartChannelStable, true},
		{"3.x release with a v", "v3.20.0", "", "", 3, ChartChannelStable, true},
		{"4.x release", "4.7.11", "", "", 4, ChartChannelStable, false},
		{"the default", DefaultChartVersion, "", "", 4, ChartChannelStable, false},
		{"4.0 prerelease", "4.0.0-rc.1", "", "", 4, ChartChannelStable, false},
		{"5.x release", "5.0.0", "", "", 5, ChartChannelStable, false},
		{"dev channel resolved to a 3.x-numbered build", "3.24.0-dev.main.2026-09-11.08-12-33.h7f841be", "main", "", 3, ChartChannelDev, false},
		{"chart directory with a 3.x pin left over", "3.23.1", "", "/tmp/agent-platform", 3, ChartChannelPath, false},
		{"chart directory and a branch (Validate refuses the pair; the directory wins)", "4.7.11", "main", "/tmp/agent-platform", 4, ChartChannelPath, false},
		{"unset (rejected by ValidateChartVersion first)", "", "", "", 0, ChartChannelStable, false},
	} {
		cfg := Default()
		cfg.Platform.ChartVersion, cfg.Platform.ChartBranch, cfg.Platform.ChartPath = tc.version, tc.branch, tc.chartDir
		if got := cfg.ChartMajor(); got != tc.major {
			t.Errorf("%s: ChartMajor() = %d, want %d", tc.name, got, tc.major)
		}
		if got := cfg.ChartChannel(); got != tc.channel {
			t.Errorf("%s: ChartChannel() = %q, want %q", tc.name, got, tc.channel)
		}
		if got := cfg.LegacyChart(); got != tc.legacy {
			t.Errorf("%s: LegacyChart() = %v, want %v", tc.name, got, tc.legacy)
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

	// A checkout's helm/ directory: the meta chart and its connectivity
	// chart side by side.
	cfg.Platform.ChartPath = filepath.Join(t.TempDir(), "agent-platform")
	if err := cfg.Validate(); err == nil {
		t.Error("chartPath without a Chart.yaml: want an error")
	}
	writeChart := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, chartFile), []byte("name: "+filepath.Base(dir)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeChart(cfg.Platform.ChartPath)
	// The meta chart alone is refused before anything is created: it
	// installs its connectivity chart at its own version, which the lab can
	// only serve by pushing the checkout's sibling directory.
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), ConnectivityChartName) || !strings.Contains(err.Error(), "--chart-branch") {
		t.Errorf("chartPath without the connectivity chart beside it: want the refusal naming %s and the dev channel, got %v", ConnectivityChartName, err)
	}
	if got, want := ConnectivityChartDir(cfg.Platform.ChartPath), filepath.Join(filepath.Dir(cfg.Platform.ChartPath), ConnectivityChartName); got != want {
		t.Errorf("ConnectivityChartDir = %q, want the sibling %q", got, want)
	}
	writeChart(ConnectivityChartDir(cfg.Platform.ChartPath))
	if err := cfg.Validate(); err != nil {
		t.Errorf("chartPath with both charts: %v", err)
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
	// The harness target is the platform Harness's image: it comes with the
	// agents, and the registry that serves it has a host port of its own.
	cfg.Platform.DevImages = map[string]string{DevImageHarness: "golang-adk:dev-139"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("devImages.harness with agents on: %v", err)
	}
	if cfg.Platform.DevRegistryPort != DefaultDevRegistryPort {
		t.Errorf("default devRegistryPort = %d, want %d", cfg.Platform.DevRegistryPort, DefaultDevRegistryPort)
	}
	cfg.Platform.DevRegistryPort = 0
	cfg.Normalize()
	if cfg.Platform.DevRegistryPort != DefaultDevRegistryPort {
		t.Errorf("Normalize left devRegistryPort %d, want the default", cfg.Platform.DevRegistryPort)
	}
	cfg.Platform.DevRegistryPort = 70000
	if err := cfg.Validate(); err == nil {
		t.Error("devRegistryPort out of range: want an error")
	}
	cfg.Platform.DevRegistryPort = DefaultDevRegistryPort
	cfg.Platform.Agents = false
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "platform.agents") {
		t.Errorf("devImages.harness without agents: want the error naming platform.agents, got %v", err)
	}
	cfg.Platform.Agents = true

	cfg.Platform.ValuesFiles = []string{filepath.Join(t.TempDir(), "missing.yaml")}
	if err := cfg.Validate(); err == nil {
		t.Error("valuesFiles with a missing file: want an error")
	}
	cfg.Platform.ValuesFiles = []string{""}
	if err := cfg.Validate(); err == nil {
		t.Error("valuesFiles with an empty path: want an error")
	}
	overlay := filepath.Join(t.TempDir(), "overlay.yaml")
	if err := os.WriteFile(overlay, []byte("components: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Platform.ValuesFiles = []string{overlay}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valuesFiles with an existing file: %v", err)
	}
	cfg.Platform.ValuesFiles = []string{}
	cfg.Normalize()
	if cfg.Platform.ValuesFiles != nil {
		t.Error("Normalize must drop an empty valuesFiles list")
	}
}

func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}

// The branches the dev-channel tests spell out.
const (
	pocBranch  = "poc/kagent-main"
	mainBranch = "main"
)

// The branch spelling in a dev tag is gitsemver's: lowercase, runs of
// anything outside [a-z0-9] collapsed to one hyphen, hyphens trimmed, a
// numeric name without leading zeros, "unknown" for nothing at all.
func TestSanitizeBranch(t *testing.T) {
	for branch, want := range map[string]string{
		pocBranch:                  "poc-kagent-main",
		mainBranch:                 mainBranch,
		"Feat/Agent_Workspaces.v2": "feat-agent-workspaces-v2",
		"renovate/axios-1.x":       "renovate-axios-1-x",
		"--weird//branch--":        "weird-branch",
		"release/0042":             "release-0042",
		"0042":                     "42",
		"000":                      "0",
		"":                         "unknown",
		"///":                      "unknown",
	} {
		if got := SanitizeBranch(branch); got != want {
			t.Errorf("SanitizeBranch(%q) = %q, want %q", branch, got, want)
		}
	}
}

// The branch fingerprint in a gitsemver 3 dev tag: the CRC32 of the full
// branch name as eight lowercase hex digits, what `gitsemver branch-hash`
// prints (the vectors are gitsemver's own and the lines' branches).
func TestBranchHash(t *testing.T) {
	for branch, want := range map[string]string{
		"my-feature": "7b5b4fa7",
		"renovate/update-all-dependencies-to-latest": "08a93c50",
		mainBranch:                 "bf28cd64",
		"giantswarm":               "588f3d76",
		pocBranch:                  "d384adaf",
		"Feat/Agent_Workspaces.v2": "93268861", // the raw name, never sanitized
	} {
		if got := BranchHash(branch); got != want {
			t.Errorf("BranchHash(%q) = %q, want %q", branch, got, want)
		}
	}
}

// The dev channel: a branch must leave a name to match tags with, and it
// excludes a local chart; the pin is meaningless without a branch.
func TestChartBranchValidation(t *testing.T) {
	for _, ok := range []string{"", pocBranch, mainBranch, "feat/x_1"} {
		if err := ValidateChartBranch(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{" main", "main ", "///", "--"} {
		if err := ValidateChartBranch(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}

	cfg := Default()
	cfg.Platform.ChartBranch = pocBranch
	if err := cfg.Validate(); err != nil {
		t.Errorf("chartBranch alone: %v", err)
	}
	cfg.Platform.ChartPath = filepath.Join(t.TempDir(), "agent-platform")
	for _, dir := range []string{cfg.Platform.ChartPath, ConnectivityChartDir(cfg.Platform.ChartPath)} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, chartFile), []byte("name: "+filepath.Base(dir)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("chartBranch with chartPath: want the mutual-exclusion error, got %v", err)
	}
	cfg.Platform.ChartPath = ""

	cfg.Platform.ChartPinned = true
	cfg.Normalize()
	if !cfg.Platform.ChartPinned {
		t.Error("Normalize must keep the pin while chartBranch is set")
	}
	cfg.Platform.ChartBranch = ""
	cfg.Normalize()
	if cfg.Platform.ChartPinned {
		t.Error("Normalize must drop the pin without a chartBranch")
	}
}
