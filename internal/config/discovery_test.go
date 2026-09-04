package config

import (
	"slices"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// Files written before the backends list carried backend/endpoint: they read
// as the one-item lists and never write the old form again.
func TestModelManagerNormalizeFoldsLegacyForm(t *testing.T) {
	mm := ModelManager{Enabled: true, Backend: ModelManagerBackendLemonade, Endpoint: "http://192.168.1.10:13305"}
	mm.normalize()
	if !slices.Equal(mm.Backends, []string{ModelManagerBackendLemonade}) || mm.Endpoints[ModelManagerBackendLemonade] != "http://192.168.1.10:13305" {
		t.Fatalf("legacy form not folded: %+v", mm)
	}
	if mm.Backend != "" || mm.Endpoint != "" {
		t.Fatalf("legacy fields survived normalize: %+v", mm)
	}
	if mm.Primary() != ModelManagerBackendLemonade || len(mm.Secondary()) != 0 {
		t.Fatalf("primary/secondary: %q / %v", mm.Primary(), mm.Secondary())
	}
}

// An enabled block without any backend (an old file with `enabled: true`
// alone) means what it always meant: the host Ollama.
func TestModelManagerNormalizeEnabledDefaultsToOllama(t *testing.T) {
	mm := ModelManager{Enabled: true}
	mm.normalize()
	if !slices.Equal(mm.Backends, []string{ModelManagerBackendOllama}) {
		t.Fatalf("backends = %v, want [ollama]", mm.Backends)
	}
	off := ModelManager{}
	off.normalize()
	if len(off.Backends) != 0 || off.Primary() != ModelManagerBackendOllama {
		t.Fatalf("disabled block: backends=%v primary=%q", off.Backends, off.Primary())
	}
}

func TestApplyDiscovered(t *testing.T) {
	cases := []struct {
		name         string
		start        ModelManager
		found        []string
		agents       bool
		pinEnabled   *bool
		pinBackends  []string
		wantBackends []string
		wantEnabled  bool
	}{
		{"both answer", ModelManager{}, []string{ModelManagerBackendOllama, ModelManagerBackendLemonade}, true, nil, nil, []string{ModelManagerBackendOllama, ModelManagerBackendLemonade}, true},
		{"canonical order whatever the probe order", ModelManager{}, []string{ModelManagerBackendLemonade, ModelManagerBackendOllama}, true, nil, nil, []string{ModelManagerBackendOllama, ModelManagerBackendLemonade}, true},
		{"only lemonade", ModelManager{}, []string{ModelManagerBackendLemonade}, true, nil, nil, []string{ModelManagerBackendLemonade}, true},
		{"none answers turns it off", ModelManager{Enabled: true, Backends: []string{ModelManagerBackendOllama}}, nil, true, nil, nil, nil, false},
		{"a server that vanished drops out", ModelManager{Enabled: true, Backends: []string{ModelManagerBackendOllama, ModelManagerBackendLemonade}}, []string{ModelManagerBackendOllama}, true, nil, nil, []string{ModelManagerBackendOllama}, true},
		{"an explicit endpoint keeps its backend", ModelManager{Enabled: true, Backends: []string{ModelManagerBackendLemonade}, Endpoints: map[string]string{ModelManagerBackendLemonade: "http://lan:13305"}}, nil, true, nil, nil, []string{ModelManagerBackendLemonade}, true},
		{"agents off keeps managed models off", ModelManager{}, []string{ModelManagerBackendOllama}, false, nil, nil, []string{ModelManagerBackendOllama}, false},
		{"pinned off", ModelManager{}, []string{ModelManagerBackendOllama}, true, boolPtr(false), nil, []string{ModelManagerBackendOllama}, false},
		{"pinned on without a server falls back to ollama", ModelManager{}, nil, true, boolPtr(true), nil, []string{ModelManagerBackendOllama}, true},
		{"pinned backends replace the discovery", ModelManager{}, []string{ModelManagerBackendOllama, ModelManagerBackendLemonade}, true, nil, []string{ModelManagerBackendLemonade}, []string{ModelManagerBackendLemonade}, true},
		{"legacy form folds first", ModelManager{Enabled: true, Backend: ModelManagerBackendOllama, Endpoint: "http://lan:11434"}, nil, true, nil, nil, []string{ModelManagerBackendOllama}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mm := tc.start
			mm.ApplyDiscovered(tc.found, tc.agents, tc.pinEnabled, tc.pinBackends)
			if !slices.Equal(mm.Backends, tc.wantBackends) {
				t.Errorf("backends = %v, want %v", mm.Backends, tc.wantBackends)
			}
			if mm.Enabled != tc.wantEnabled {
				t.Errorf("enabled = %v, want %v", mm.Enabled, tc.wantEnabled)
			}
			if mm.Backend != "" || mm.Endpoint != "" {
				t.Errorf("legacy fields set after apply: %+v", mm)
			}
			if err := mm.Validate(tc.agents); err != nil {
				t.Errorf("applied block does not validate: %v", err)
			}
		})
	}
}

// An endpoint override for a backend the discovery dropped goes with it (it
// would fail validation otherwise), while the kept one stays.
func TestApplyDiscoveredPrunesEndpointsOfDroppedBackends(t *testing.T) {
	mm := ModelManager{Enabled: true, Backends: []string{ModelManagerBackendOllama, ModelManagerBackendLemonade},
		Endpoints: map[string]string{ModelManagerBackendOllama: "http://lan:11434"}}
	mm.ApplyDiscovered(nil, true, nil, []string{ModelManagerBackendLemonade})
	if _, kept := mm.Endpoints[ModelManagerBackendOllama]; kept {
		t.Fatalf("endpoint of the dropped backend survived: %v", mm.Endpoints)
	}
	if err := mm.Validate(true); err != nil {
		t.Fatal(err)
	}
}

func TestConfigLoadsLegacyModelManagerForm(t *testing.T) {
	cfg := Default()
	cfg.Platform.ModelManager = ModelManager{Enabled: true, Backend: ModelManagerBackendOllama}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("legacy form rejected before normalize: %v", err)
	}
	cfg.Normalize()
	if !slices.Equal(cfg.Platform.ModelManager.Backends, []string{ModelManagerBackendOllama}) {
		t.Fatalf("Normalize did not fold the legacy form: %+v", cfg.Platform.ModelManager)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The lab's own published ports are never conflicts; anything else that
// listens on a lab port is.
func TestPortConflictsIgnoreTheClustersOwnPorts(t *testing.T) {
	cfg := Default()
	cfg.Platform.MusterPort = 8092
	ours := map[int]bool{cfg.DexPort: true, cfg.Backstage.Port: true, 8092: true, cfg.Platform.AgentsPort: true, cfg.Platform.GatewayPort: true}
	// Every lab port "listens" (the kind node holds them) plus a foreign
	// process on the kagent UI port — the only conflict to report.
	taken := func(p int) bool { return ours[p] || p == cfg.Platform.AgentsPort }
	conflicts := cfg.portConflicts(func(p int) bool { return !ours[p] && taken(p) })
	if len(conflicts) != 0 {
		t.Fatalf("the cluster's own ports reported as conflicts: %v", conflicts)
	}
	// Now the cluster does not publish the kagent UI port (an older node)
	// and a foreign process holds it.
	delete(ours, cfg.Platform.AgentsPort)
	conflicts = cfg.portConflicts(func(p int) bool { return !ours[p] && taken(p) })
	if len(conflicts) != 1 || conflicts[0].Field != "platform.agentsPort" || conflicts[0].Port != cfg.Platform.AgentsPort {
		t.Fatalf("conflicts = %v, want exactly platform.agentsPort", conflicts)
	}
	if !strings.Contains(conflicts[0].String(), "held by another process") {
		t.Fatalf("unexpected wording: %s", conflicts[0])
	}
}

// With no cluster, ChooseFreePorts moves an existing configuration's ports
// off foreign listeners like a fresh one's — and leaves the free ones alone.
func TestChooseFreePortsOnExistingConfigWithoutCluster(t *testing.T) {
	cfg := Default()
	cfg.Platform.MusterPort = 8092 // moved by an earlier run; still free
	changes := cfg.chooseFreePorts(takenSet(cfg.Backstage.Port))
	if len(changes) != 1 || changes[0].Field != "backstage.port" {
		t.Fatalf("changes = %v, want exactly backstage.port", changes)
	}
	if cfg.Platform.MusterPort != 8092 {
		t.Fatalf("a free non-default port was renumbered: %d", cfg.Platform.MusterPort)
	}
}
