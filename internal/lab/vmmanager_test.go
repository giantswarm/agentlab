package lab

import (
	"os"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// testImageDir is the image directory the tests configure.
const testImageDir = "/srv/images"

// TestKVMLine: the discovery report says whether the VM provisioner can run
// as a pod of the node here and what the configuration does with it.
func TestKVMLine(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.VMManager = config.VMManager{Enabled: true, ImageDir: testImageDir}
	d := &Discovery{}
	if got := d.kvmLine(cfg); !strings.Contains(got, "present") || !strings.Contains(got, "a local guest image build from "+testImageDir) {
		t.Fatalf("with the devices and a directory: %q", got)
	}
	cfg.Platform.VMManager.ImageDir = ""
	if got := d.kvmLine(cfg); !strings.Contains(got, "the release's guest image") {
		t.Fatalf("without a directory the report names the release's image: %q", got)
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

// TestVMManagerGuestImageFor: without a local build, and before one is
// pushed (the renders of `agentlab up`), the chart keeps its default (nil);
// a record reads back with its digest, a missing or digest-less one is an
// error for the reader.
func TestVMManagerGuestImageFor(t *testing.T) {
	cfg := config.Default()
	got, err := vmManagerGuestImageFor(cfg)
	if err != nil || got != nil {
		t.Fatalf("no directory: got %+v, %v", got, err)
	}
	cfg.Platform.VMManager.ImageDir = testImageDir
	if got, err := vmManagerGuestImageFor(cfg); err != nil || got != nil {
		t.Fatalf("a directory before the push renders the chart default, got %+v, %v", got, err)
	}
	path := t.TempDir() + "/record.json"
	if err := os.WriteFile(path, []byte(`{"imageDir":"`+testImageDir+`","reference":"localhost:5001/vm-manager-guest-image:dev","digest":"sha256:abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	record, err := readGuestImageRecord(path)
	if err != nil || record.Digest != "sha256:abc" {
		t.Fatalf("record: %+v, %v", record, err)
	}
	if _, err := readGuestImageRecord(t.TempDir() + "/missing.json"); err == nil {
		t.Fatal("a missing record must be an error")
	}
	if err := os.WriteFile(path, []byte(`{"digest":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGuestImageRecord(path); err == nil {
		t.Fatal("a record without a digest must be an error")
	}
}

// TestVMManagerHint: the summary names the image directory and a dev image.
func TestVMManagerHint(t *testing.T) {
	cfg := config.Default()
	if got := vmManagerHint(cfg); !strings.Contains(got, "not wired") {
		t.Fatalf("off: %q", got)
	}
	cfg.Platform.VMManager = config.VMManager{Enabled: true, ImageDir: testImageDir}
	cfg.Platform.DevImages = map[string]string{"vm-manager": "vm-manager:dev"}
	got := vmManagerHint(cfg)
	for _, want := range []string{testImageDir, "vm-manager:dev", "x_vm-manager_*", toolGroupAgentPlatform} {
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

// TestExplainQuoteVerdict: a golden mismatch names what the PCR measures —
// the pod firmware for PCR 0, the guest image for PCR 4, the sysext for PCR
// 13 — and the recipe that records the values again; any other verdict is
// returned as it is.
func TestExplainQuoteVerdict(t *testing.T) {
	host := &vmHostInfo{OVMFCode: "/usr/share/OVMF/OVMF_CODE_4M.fd", Firmware: &vmFirmware{SHA256: "50a48d30e35dd0bbba8e78838313ea5b8c9201408ce7fc50495f2f61bc5f12bf", Package: "ovmf-generic", Version: "2025.11-3ubuntu7.2"}}
	firmware := explainQuoteVerdict("golden mismatch: pcr 0 expected c9894ac4…, got 306be437…", "giantswarm-vm-base_0.1.0", host.firmware())
	for _, want := range []string{"golden mismatch: pcr 0 expected c9894ac4…, got 306be437…", "OVMF of the vm-manager pod image", "ovmf-generic 2025.11-3ubuntu7.2 (/usr/share/OVMF/OVMF_CODE_4M.fd, sha256 50a48d30e35d…)", "re-record golden PCRs", "vm-manager image golden giantswarm-vm-base_0.1.0 --clear", "docs/vm-manager.md"} {
		if !strings.Contains(firmware, want) {
			t.Fatalf("the PCR 0 verdict lacks %q:\n%s", want, firmware)
		}
	}
	if got := explainQuoteVerdict("golden mismatch: pcr 0 expected a, got b", "img", (&vmHostInfo{}).firmware()); !strings.Contains(got, "a build this vm-manager does not report") {
		t.Fatalf("a vm-manager without get_host's firmware is named as such:\n%s", got)
	}
	if got := explainQuoteVerdict("golden mismatch: pcr 4 expected a, got b", "img", ""); !strings.Contains(got, "the guest image changed") {
		t.Fatalf("the PCR 4 verdict names the guest image:\n%s", got)
	}
	if got := explainQuoteVerdict("golden mismatch: pcr 13 expected a, got b", "img", ""); !strings.Contains(got, "Kubernetes sysext") {
		t.Fatalf("the PCR 13 verdict names the sysext:\n%s", got)
	}
	if got := explainQuoteVerdict("nonce mismatch", "img", ""); got != "nonce mismatch" {
		t.Fatalf("another verdict is returned as it is: %q", got)
	}
}
