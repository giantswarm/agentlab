package config

import (
	"net"
	"testing"
)

// takenSet builds a probe stub that reports exactly the given ports occupied.
func takenSet(ports ...int) func(int) bool {
	set := map[int]bool{}
	for _, p := range ports {
		set[p] = true
	}
	return func(p int) bool { return set[p] }
}

func TestChooseFreePortsNothingTaken(t *testing.T) {
	cfg := Default()
	want := Default()
	if changes := cfg.chooseFreePorts(takenSet()); len(changes) != 0 {
		t.Fatalf("expected no changes, got %v", changes)
	}
	got := []int{cfg.DexPort, cfg.Platform.MusterPort, cfg.Platform.AgentsPort, cfg.Platform.GatewayPort, cfg.Backstage.Port}
	exp := []int{want.DexPort, want.Platform.MusterPort, want.Platform.AgentsPort, want.Platform.GatewayPort, want.Backstage.Port}
	for i := range got {
		if got[i] != exp[i] {
			t.Fatalf("port mutated without conflicts: got %v, want %v", got, exp)
		}
	}
}

func TestChooseFreePortsMovesEveryTakenPort(t *testing.T) {
	cfg := Default()
	changes := cfg.chooseFreePorts(takenSet(
		cfg.DexPort, cfg.Platform.MusterPort, cfg.Platform.AgentsPort,
		cfg.Platform.GatewayPort, cfg.Backstage.Port,
	))
	if len(changes) != 5 {
		t.Fatalf("expected 5 changes, got %d: %v", len(changes), changes)
	}
	if cfg.DexPort != 32001 {
		t.Errorf("dexPort = %d, want 32001", cfg.DexPort)
	}
	if cfg.Platform.MusterPort != 8091 {
		t.Errorf("musterPort = %d, want 8091", cfg.Platform.MusterPort)
	}
	if cfg.Platform.AgentsPort != 8082 {
		t.Errorf("agentsPort = %d, want 8082", cfg.Platform.AgentsPort)
	}
	if cfg.Platform.GatewayPort != 8443 {
		t.Errorf("gatewayPort = %d, want 8443", cfg.Platform.GatewayPort)
	}
	if cfg.Backstage.Port != 7008 {
		t.Errorf("backstage.port = %d, want 7008", cfg.Backstage.Port)
	}
	for _, ch := range changes {
		if ch.Field == "" || ch.What == "" || ch.From == ch.To {
			t.Errorf("malformed change: %+v", ch)
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("moved config no longer validates: %v", err)
	}
}

// The kagent UI scan must hop over the muster default (8090): it is another
// lab port even when muster itself did not need to move.
func TestChooseFreePortsSkipsOtherLabPorts(t *testing.T) {
	cfg := Default()
	occupied := []int{cfg.Platform.AgentsPort} // 8081
	for p := 8082; p <= 8089; p++ {
		occupied = append(occupied, p)
	}
	cfg.chooseFreePorts(takenSet(occupied...))
	if cfg.Platform.AgentsPort != 8091 {
		t.Fatalf("agentsPort = %d, want 8091 (8090 is muster's)", cfg.Platform.AgentsPort)
	}
}

func TestChooseFreePortsGatewayPrefers8443(t *testing.T) {
	cfg := Default()
	changes := cfg.chooseFreePorts(takenSet(443, 8443))
	if cfg.Platform.GatewayPort != 8444 {
		t.Fatalf("gatewayPort = %d, want 8444", cfg.Platform.GatewayPort)
	}
	if len(changes) != 1 || changes[0].Note == "" {
		t.Fatalf("expected one gateway change carrying the non-443 note, got %v", changes)
	}
}

func TestChooseFreePortsDexWrapsWithinNodePortRange(t *testing.T) {
	cfg := Default()
	var occupied []int
	for p := cfg.DexPort; p <= 32767; p++ {
		occupied = append(occupied, p)
	}
	cfg.chooseFreePorts(takenSet(occupied...))
	if cfg.DexPort != 30000 {
		t.Fatalf("dexPort = %d, want 30000 (wrap to the bottom of the NodePort range)", cfg.DexPort)
	}
}

// Two ports moving in the same run must not land on the same replacement.
func TestChooseFreePortsReplacementsDoNotCollide(t *testing.T) {
	cfg := Default()
	cfg.Platform.MusterPort = 9000
	cfg.Platform.AgentsPort = 9001
	cfg.chooseFreePorts(takenSet(9000, 9001))
	if cfg.Platform.MusterPort == cfg.Platform.AgentsPort {
		t.Fatalf("muster and agents both landed on %d", cfg.Platform.MusterPort)
	}
	if cfg.Platform.MusterPort != 9002 || cfg.Platform.AgentsPort != 9003 {
		t.Fatalf("musterPort = %d, agentsPort = %d; want 9002 and 9003",
			cfg.Platform.MusterPort, cfg.Platform.AgentsPort)
	}
}

// When a range holds no free port at all, the original stays put (and `up`
// reports the real bind error) instead of looping or panicking.
func TestChooseFreePortsExhaustedRangeKeepsPort(t *testing.T) {
	cfg := Default()
	cfg.chooseFreePorts(func(int) bool { return true })
	if cfg.DexPort != 32000 {
		t.Fatalf("dexPort = %d, want the untouched 32000", cfg.DexPort)
	}
}

func TestPortTakenProbe(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if !portTaken(port) {
		t.Errorf("port %d has a listener but probes free", port)
	}
	_ = l.Close()
	if portTaken(port) {
		t.Errorf("port %d is closed but probes taken", port)
	}
}
