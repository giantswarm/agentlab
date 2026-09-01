package config

import "testing"

// A stale pin in agentlab.yaml is what let an amd64-only Backstage survive a
// pulled multi-arch bump, so drift must be reported and --defaults must clear
// it — without touching the lab's shape.
func TestPinDrift(t *testing.T) {
	cfg := Default()
	if drift := cfg.PinDrift(); len(drift) != 0 {
		t.Fatalf("fresh defaults drift: %+v", drift)
	}

	cfg.Platform.APSRef = "d2dc19f9a705b7b297c2fafcd685675683a4631c"
	drift := cfg.PinDrift()
	if len(drift) != 1 || drift[0].Name != "platform.apsRef" {
		t.Fatalf("want one platform.apsRef drift, got %+v", drift)
	}
	if drift[0].Current != cfg.Platform.APSRef || drift[0].Shipped != Default().Platform.APSRef {
		t.Errorf("drift reports wrong values: %+v", drift[0])
	}

	cfg.DexImage = "ghcr.io/dexidp/dex:v2.44.0"
	if got := len(cfg.PinDrift()); got != 2 {
		t.Errorf("want both pins drifting, got %d", got)
	}
}

func TestAdoptPins(t *testing.T) {
	cfg := Default()
	cfg.Platform.APSRef = "d2dc19f9a705b7b297c2fafcd685675683a4631c"
	cfg.DexImage = "ghcr.io/dexidp/dex:v2.44.0"
	// The lab's shape is the user's, and adoption must not touch it.
	cfg.ClusterName = "mylab"
	cfg.DexPort = 32001
	cfg.Platform.MusterPort = 9090
	cfg.Users = cfg.Users[:1]

	moved := cfg.AdoptPins()
	if len(moved) != 2 {
		t.Fatalf("want 2 pins moved, got %+v", moved)
	}
	if drift := cfg.PinDrift(); len(drift) != 0 {
		t.Errorf("drift survived adoption: %+v", drift)
	}
	if cfg.Platform.APSRef != Default().Platform.APSRef || cfg.DexImage != Default().DexImage {
		t.Errorf("pins not adopted: apsRef=%q dexImage=%q", cfg.Platform.APSRef, cfg.DexImage)
	}
	if cfg.ClusterName != "mylab" || cfg.DexPort != 32001 || cfg.Platform.MusterPort != 9090 || len(cfg.Users) != 1 {
		t.Errorf("adoption clobbered the lab's shape: %+v", cfg)
	}

	// Already-current pins report nothing moved.
	if moved := cfg.AdoptPins(); len(moved) != 0 {
		t.Errorf("want no-op on current pins, got %+v", moved)
	}
}
