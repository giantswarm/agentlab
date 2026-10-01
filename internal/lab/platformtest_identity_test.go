package lab

import (
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// The controller identity proof needs the controller's GRPCRoute, which only
// the 4.x connectivity chart renders: a 3.x lab — a release, or a checkout of
// the maintenance line — skips it with the reason, the current line runs it.
func TestControllerIdentitySkip(t *testing.T) {
	checkout := func(values string) string {
		return writeChartFiles(t, t.TempDir(), "agent-platform", map[string]string{chartYAML: metaChartYAML, "values.yaml": values})
	}
	for _, tc := range []struct {
		name               string
		version, chartPath string
		skip               bool
	}{
		{"3.x release", "3.23.1", "", true},
		{"3.x checkout", "", checkout("components:\n  kagent: {}\n"), true},
		{"the default", config.DefaultChartVersion, "", false},
		{"4.x checkout", "", checkout("components:\n  kagent: {}\n  substrate: {}\n"), false},
	} {
		cfg := config.Default()
		cfg.Platform.ChartVersion, cfg.Platform.ChartPath = tc.version, tc.chartPath
		if got := controllerIdentitySkip(cfg); (got != "") != tc.skip {
			t.Errorf("%s: controllerIdentitySkip() = %q, want a reason: %v", tc.name, got, tc.skip)
		}
	}
}
