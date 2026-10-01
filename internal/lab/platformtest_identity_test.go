package lab

import (
	"reflect"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// The controller identity proof needs the controller's GRPCRoute and the
// families proof a family member, which only the 4.x connectivity chart
// renders: a 3.x lab — a release, or a checkout of the maintenance line —
// skips both with the reason and calls the family-less mcp-kubernetes by its
// own tools; the current line runs both and calls the family's.
func TestLegacyLabProofs(t *testing.T) {
	checkout := func(values string) string {
		return writeChartFiles(t, t.TempDir(), "agent-platform", map[string]string{chartYAML: metaChartYAML, "values.yaml": values})
	}
	for _, tc := range []struct {
		name               string
		version, chartPath string
		skip               bool
	}{
		{"3.x release", legacyChartVersion, "", true},
		{"3.x checkout", "", checkout("components:\n  kagent: {}\n"), true},
		{"the default", config.DefaultChartVersion, "", false},
		{"4.x checkout", "", checkout("components:\n  kagent: {}\n  substrate: {}\n"), false},
	} {
		cfg := config.Default()
		cfg.Platform.ChartVersion, cfg.Platform.ChartPath = tc.version, tc.chartPath
		if got := controllerIdentitySkip(cfg); (got != "") != tc.skip {
			t.Errorf("%s: controllerIdentitySkip() = %q, want a reason: %v", tc.name, got, tc.skip)
		}
		if got := familiesSkip(cfg); (got != "") != tc.skip {
			t.Errorf("%s: familiesSkip() = %q, want a reason: %v", tc.name, got, tc.skip)
		}
		server, tool, args := cfg.ClusterName+"-mcp-kubernetes", "x_kubernetes_list", map[string]any{familyInstanceArg: cfg.ClusterName + "-mcp-kubernetes", "resourceType": "pods"}
		if tc.skip {
			server, tool, args = componentMCPKubernetes, "x_mcp-kubernetes_list", map[string]any{"resourceType": "pods"}
		}
		if got := cfg.MCPServerName(); got != server {
			t.Errorf("%s: MCPServerName() = %q, want %q", tc.name, got, server)
		}
		if got := kubernetesTool(cfg, "list"); got != tool {
			t.Errorf("%s: kubernetesTool(list) = %q, want %q", tc.name, got, tool)
		}
		if got := kubernetesArgs(cfg, map[string]any{"resourceType": "pods"}); !reflect.DeepEqual(got, args) {
			t.Errorf("%s: kubernetesArgs = %v, want %v", tc.name, got, args)
		}
	}
}
