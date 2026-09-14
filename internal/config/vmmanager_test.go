package config

import (
	"strings"
	"testing"
)

func TestVMManagerValidate(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		vmm     VMManager
		wantErr string
	}{
		{"off", VMManager{}, ""},
		{"on, no images", VMManager{Enabled: true}, ""},
		{"on, a directory", VMManager{Enabled: true, ImageDir: dir}, ""},
		{"missing directory", VMManager{Enabled: true, ImageDir: dir + "/missing"}, "imageDir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.vmm.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestVMManagerApplyDiscovered: the key is refused without KVM, kept with it
// (never turned on by itself: the pod is a heavier piece than a model
// server), and pinned by the flag either way.
func TestVMManagerApplyDiscovered(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		vmm  VMManager
		kvm  bool
		pin  *bool
		want bool
	}{
		{"KVM keeps an off key off", VMManager{}, true, nil, false},
		{"KVM keeps an on key on", VMManager{Enabled: true}, true, nil, true},
		{"no KVM turns it off", VMManager{Enabled: true}, false, nil, false},
		{"pinned on", VMManager{}, true, &on, true},
		{"pinned off despite KVM", VMManager{Enabled: true}, true, &off, false},
		{"pinned on without KVM (the preflight refuses it later)", VMManager{}, false, &on, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.vmm.ApplyDiscovered(tc.kvm, tc.pin)
			if tc.vmm.Enabled != tc.want {
				t.Fatalf("Enabled = %v, want %v", tc.vmm.Enabled, tc.want)
			}
		})
	}
}

func TestVMManagerEnabledNeedsThePlatform(t *testing.T) {
	cfg := Default()
	cfg.Platform.VMManager.Enabled = true
	if !cfg.VMManagerEnabled() {
		t.Fatal("want enabled with the platform on")
	}
	cfg.Platform.Agents = false
	if !cfg.VMManagerEnabled() {
		t.Fatal("vm-manager is muster's server, not the agents runtime's — agents off must not disable it")
	}
	cfg.Platform.Enabled = false
	if cfg.VMManagerEnabled() {
		t.Fatal("want disabled with the platform off")
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "backstage") {
		// Default() enables Backstage, which requires the platform: the
		// error is Backstage's, not vm-manager's.
		t.Fatalf("expected the backstage/platform error, got %v", err)
	}
}

// TestDevImagesKnowVMManager: the dev-image loop swaps the vm-manager build in.
func TestDevImagesKnowVMManager(t *testing.T) {
	cfg := Default()
	cfg.Platform.DevImages = map[string]string{"vm-manager": "vm-manager:dev"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("vm-manager must be a devImages target: %v", err)
	}
}
