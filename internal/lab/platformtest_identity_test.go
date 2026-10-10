package lab

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// The controller identity proof needs the controller's GRPCRoute and the
// families proof a family member, which only the 4.x connectivity chart
// renders: a 3.x lab — a release, or a checkout of the maintenance line —
// skips both with the reason and calls the family-less mcp-kubernetes by its
// own tools; an upgrade seed's 4.x line before the families has the
// controller's route but no family member; the current line runs both and
// calls the family's.
func TestLegacyLabProofs(t *testing.T) {
	checkout := func(values string) string {
		return writeChartFiles(t, t.TempDir(), "agent-platform", map[string]string{chartYAML: metaChartYAML, "values.yaml": values})
	}
	for _, tc := range []struct {
		name               string
		version, chartPath string
		skip, familyless   bool
	}{
		{"3.x release", legacyChartVersion, "", true, true},
		{"3.x checkout", "", checkout("components:\n  kagent: {}\n"), true, true},
		{"4.x upgrade seed", upgradeSeedVersion, "", false, true},
		{"the default", config.DefaultChartVersion, "", false, false},
		{"4.x checkout", "", checkout("components:\n  kagent: {}\n  substrate: {}\n"), false, false},
	} {
		cfg := config.Default()
		cfg.Platform.ChartVersion, cfg.Platform.ChartPath = tc.version, tc.chartPath
		if got := controllerIdentitySkip(cfg); (got != "") != tc.skip {
			t.Errorf("%s: controllerIdentitySkip() = %q, want a reason: %v", tc.name, got, tc.skip)
		}
		if got := familiesSkip(cfg); (got != "") != tc.familyless {
			t.Errorf("%s: familiesSkip() = %q, want a reason: %v", tc.name, got, tc.familyless)
		}
		member := cfg.ClusterName + "-mcp-kubernetes"
		server, tool, args := member, "x_"+familyKubernetes+"_list", map[string]any{familyInstanceArg: member, resourceTypeKey: resourceNamespaces}
		if tc.familyless {
			server, tool, args = componentMCPKubernetes, "x_"+componentMCPKubernetes+"_list", map[string]any{resourceTypeKey: resourceNamespaces}
		}
		if got := cfg.MCPServerName(); got != server {
			t.Errorf("%s: MCPServerName() = %q, want %q", tc.name, got, server)
		}
		if got := kubernetesTool(cfg, "list"); got != tool {
			t.Errorf("%s: kubernetesTool(list) = %q, want %q", tc.name, got, tool)
		}
		if got := kubernetesArgs(cfg, map[string]any{resourceTypeKey: resourceNamespaces}); !reflect.DeepEqual(got, args) {
			t.Errorf("%s: kubernetesArgs = %v, want %v", tc.name, got, args)
		}
	}
}

// TestIdentityProofUsers: the identity proofs act as one platform-admins and
// one viewers user; a configuration without either has no proof to run and
// fails it with the need, never a silent pass.
func TestIdentityProofUsers(t *testing.T) {
	cfg := config.Default()
	admin, viewer, err := identityProofUsers(cfg)
	if err != nil || admin == nil || viewer == nil {
		t.Fatalf("identityProofUsers(default) = %v, %v, %v; want the two users", admin, viewer, err)
	}
	if admin.Email == viewer.Email {
		t.Errorf("one user for both roles: %s", admin.Email)
	}
	for _, missing := range []string{"platform-admins", "viewers"} {
		without := config.Default()
		users := without.Users[:0]
		for _, u := range without.Users {
			if !slices.Contains(u.Groups, missing) {
				users = append(users, u)
			}
		}
		without.Users = users
		_, _, err := identityProofUsers(without)
		if err == nil || !strings.Contains(err.Error(), "needs one platform-admins and one viewers user") {
			t.Errorf("without a %s user: identityProofUsers() = %v, want the need named", missing, err)
		}
	}
}
