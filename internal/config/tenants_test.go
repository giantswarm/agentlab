package config

import (
	"slices"
	"testing"
)

// configure --tenants adds one user per tenant group once: a second run, or
// a lab whose own user already carries the group, adds nobody, and the
// result validates (the groups are in the vocabulary).
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
