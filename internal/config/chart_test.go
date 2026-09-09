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

// The dev channel: a branch must leave a name to match tags with, and it
// excludes a local chart; the pin is meaningless without a branch; Substrate
// follows the channel unless pinned.
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
	if !cfg.SubstrateEnabled() {
		t.Error("chartBranch with agents on must imply Substrate")
	}
	cfg.Platform.Agents = false
	if cfg.SubstrateEnabled() {
		t.Error("chartBranch without agents must not imply Substrate")
	}
	cfg.Platform.Agents = true
	off := false
	cfg.Platform.Substrate.Enabled = &off
	if cfg.SubstrateEnabled() {
		t.Error("an explicit substrate.enabled: false must win over the channel")
	}
	cfg.Platform.Substrate.Enabled = nil

	cfg.Platform.ChartPath = t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg.Platform.ChartPath, "Chart.yaml"), []byte("name: agent-platform\n"), 0o600); err != nil {
		t.Fatal(err)
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
	if cfg.SubstrateEnabled() {
		t.Error("the stable channel must not imply Substrate")
	}
	on := true
	cfg.Platform.Substrate.Enabled = &on
	if !cfg.SubstrateEnabled() {
		t.Error("an explicit substrate.enabled: true must install it on the stable channel too")
	}
	cfg.Platform.Enabled = false
	if cfg.SubstrateEnabled() {
		t.Error("Substrate is inert without the platform")
	}
}
