package config

import (
	"strings"
	"testing"
)

func TestVMManagerValidate(t *testing.T) {
	cases := []struct {
		name    string
		vmm     VMManager
		wantErr string
	}{
		{"off", VMManager{}, ""},
		{"on, default port", VMManager{Enabled: true}, ""},
		{"on, explicit port", VMManager{Enabled: true, Port: 8123}, ""},
		{"endpoint override", VMManager{Enabled: true, Endpoint: "http://host.docker.internal:8100"}, ""},
		{"bad port", VMManager{Enabled: true, Port: 70000}, "port"},
		{"bad endpoint", VMManager{Enabled: true, Endpoint: "172.21.0.1:8100"}, "must be an http(s) URL"},
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

func TestVMManagerListenPortDefaults(t *testing.T) {
	var vmm VMManager
	if vmm.ListenPort() != DefaultVMManagerPort {
		t.Fatalf("ListenPort = %d, want %d", vmm.ListenPort(), DefaultVMManagerPort)
	}
	vmm.normalize()
	if vmm.Port != DefaultVMManagerPort {
		t.Fatalf("normalize left port %d", vmm.Port)
	}
	if Default().Platform.VMManager.Port != DefaultVMManagerPort {
		t.Fatalf("Default() writes no port")
	}
}

func TestVMManagerApplyDiscovered(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name  string
		vmm   VMManager
		found bool
		pin   *bool
		want  bool
	}{
		{"found turns it on", VMManager{}, true, nil, true},
		{"gone turns it off", VMManager{Enabled: true}, false, nil, false},
		{"an endpoint keeps it on", VMManager{Enabled: true, Endpoint: "http://10.0.0.5:8100"}, false, nil, true},
		{"pinned on without a server", VMManager{}, false, &on, true},
		{"pinned off despite a server", VMManager{Enabled: true}, true, &off, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.vmm.ApplyDiscovered(tc.found, tc.pin)
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
