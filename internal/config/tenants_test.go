package config

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// configure --organizations adds one member per organization group once: a
// second run, or a lab whose own user already carries the group, adds
// nobody, and the result validates (the groups are in the vocabulary).
func TestEnsureTenantUsers(t *testing.T) {
	cfg := Default()
	if added := cfg.EnsureTenantUsers(); !slices.Equal(added, []string{"alice@lab.local", "bob@lab.local"}) {
		t.Fatalf("first run added %v", added)
	}
	if added := cfg.EnsureTenantUsers(); len(added) != 0 {
		t.Errorf("second run added %v", added)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	own := Default()
	own.Users[1].Groups = append(own.Users[1].Groups, GroupTenantA)
	if added := own.EnsureTenantUsers(); !slices.Equal(added, []string{"bob@lab.local"}) {
		t.Errorf("with dev in %s: added %v, want bob@lab.local only", GroupTenantA, added)
	}
}

// Organizations are on by default and take effect only with the agents
// runtime on a chart that carries agentPlatformOrganizations; below the
// floor the reason names the floor and the chart.
func TestOrganizationsEnabled(t *testing.T) {
	cfg := Default()
	cfg.Platform.ChartVersion = "4.175.0-rc.9"
	if !cfg.OrganizationsEnabled() || cfg.OrganizationsInactive() != "" {
		t.Errorf("default on 4.175.0-rc.9: enabled=%v inactive=%q", cfg.OrganizationsEnabled(), cfg.OrganizationsInactive())
	}
	cfg.Platform.ChartVersion = "4.174.0"
	if cfg.OrganizationsEnabled() {
		t.Error("on 4.174.0, before agentPlatformOrganizations: enabled")
	}
	if why := cfg.OrganizationsInactive(); !strings.Contains(why, OrganizationsChartFloor.String()) || !strings.Contains(why, "4.174.0") {
		t.Errorf("the reason below the floor: %q", why)
	}
	cfg.Platform.ChartVersion = "4.175.0-rc.9"
	cfg.Platform.Agents = false
	if cfg.OrganizationsEnabled() || !strings.Contains(cfg.OrganizationsInactive(), "platform.agents") {
		t.Errorf("without agents: enabled=%v inactive=%q", cfg.OrganizationsEnabled(), cfg.OrganizationsInactive())
	}
	cfg.Platform.Agents = true
	cfg.Platform.Organizations.Enabled = false
	if cfg.OrganizationsEnabled() || cfg.OrganizationsInactive() != "" {
		t.Errorf("off: enabled=%v inactive=%q", cfg.OrganizationsEnabled(), cfg.OrganizationsInactive())
	}
}

// A file an earlier release wrote with the retired tenants block still
// loads, and a save does not write the block back.
func TestRetiredTenantsLoads(t *testing.T) {
	t.Chdir(t.TempDir())
	raw := "clusterName: lab\nplatform:\n  enabled: true\n  agents: true\n  chartVersion: " + loadTestChart + "\n  tenants:\n    enabled: true\n"
	if err := os.WriteFile(File, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("the retired block refused: %v", err)
	}
	if !cfg.Platform.Organizations.Enabled {
		t.Error("organizations off on a file that names none")
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if after := readFile(t); strings.Contains(after, "tenants:") {
		t.Errorf("the retired block written back:\n%s", after)
	}
}
