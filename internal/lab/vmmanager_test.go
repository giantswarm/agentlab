package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestKVMLine: the discovery report says whether the VM provisioner can run
// as a pod of the node here and what the configuration does with it.
func TestKVMLine(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.VMManager = config.VMManager{Enabled: true, ImageDir: "/srv/images"}
	d := &Discovery{}
	if got := d.kvmLine(cfg); !strings.Contains(got, "present") || !strings.Contains(got, "on, images from /srv/images") {
		t.Fatalf("with the devices and a directory: %q", got)
	}
	cfg.Platform.VMManager.ImageDir = ""
	if got := d.kvmLine(cfg); !strings.Contains(got, "no image directory") {
		t.Fatalf("without a directory the report must say so: %q", got)
	}
	cfg.Platform.VMManager.Enabled = false
	if got := d.kvmLine(cfg); !strings.Contains(got, "--vm-manager") {
		t.Fatalf("off: the report names the flag that turns it on: %q", got)
	}
	d.KVMMissing = []string{"/dev/vhost-vsock"}
	if got := d.kvmLine(cfg); !strings.Contains(got, "no /dev/vhost-vsock") || d.KVMReady() {
		t.Fatalf("a missing device is named and KVMReady is false: %q", got)
	}
}

// TestKVMFixHint: every missing device comes with its own fix.
func TestKVMFixHint(t *testing.T) {
	got := kvmFixHint([]string{"/dev/kvm", "/dev/vhost-vsock"})
	for _, want := range []string{"kvm_intel", "modprobe vhost_vsock"} {
		if !strings.Contains(got, want) {
			t.Fatalf("hint lacks %q: %q", want, got)
		}
	}
}

// TestVMManagerImageMount: the chart's images.hostPath is the node path the
// image directory is mounted at, or nothing without a directory.
func TestVMManagerImageMount(t *testing.T) {
	cfg := config.Default()
	if got := vmManagerImageMountFor(cfg); got != "" {
		t.Fatalf("no directory, got %q", got)
	}
	cfg.Platform.VMManager.ImageDir = "/home/x/vm-manager/images/build"
	if got := vmManagerImageMountFor(cfg); got != vmManagerImageMount {
		t.Fatalf("got %q, want %q", got, vmManagerImageMount)
	}
}

// TestVMManagerHint: the summary names the image directory and a dev image.
func TestVMManagerHint(t *testing.T) {
	cfg := config.Default()
	if got := vmManagerHint(cfg); !strings.Contains(got, "not wired") {
		t.Fatalf("off: %q", got)
	}
	cfg.Platform.VMManager = config.VMManager{Enabled: true, ImageDir: "/srv/images"}
	cfg.Platform.DevImages = map[string]string{"vm-manager": "vm-manager:dev"}
	got := vmManagerHint(cfg)
	for _, want := range []string{"/srv/images", "vm-manager:dev", "x_vm-manager_*", toolGroupAgentPlatform} {
		if !strings.Contains(got, want) {
			t.Fatalf("hint lacks %q: %q", want, got)
		}
	}
}

func TestVMImageHasGolden(t *testing.T) {
	cases := []struct {
		name   string
		policy string
		want   bool
	}{
		{"fresh build", `{"pcr11":{"enter-initrd":"aa"},"pcr13":{}}`, false},
		{"golden recorded", `{"pcr11":{},"golden":{"sha256":{"0":"aa","7":"bb"}}}`, true},
		{"golden empty", `{"golden":{}}`, false},
		{"no policy", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img := vmImage{ID: "giantswarm-vm-base", Version: "0.1.0"}
			if tc.policy != "" {
				img.Policy = []byte(tc.policy)
			}
			if got := img.hasGolden(); got != tc.want {
				t.Fatalf("hasGolden = %v, want %v", got, tc.want)
			}
			if img.ref() != "giantswarm-vm-base_0.1.0" {
				t.Fatalf("ref %q", img.ref())
			}
		})
	}
}

// TestStripANSI: the console tail reads as text — colours, cursor moves and
// the terminal's title/prompt marks removed, the words kept.
func TestStripANSI(t *testing.T) {
	in := "[\x1b[0;32m  OK  \x1b[0m] Reached target \x1b[0;1;39mMulti-User System\x1b[0m.\x1b]3008;start=abc;user=root\x07 done \x1b[!p\x1b[?7h"
	if got, want := stripANSI(in), "[  OK  ] Reached target Multi-User System. done "; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
