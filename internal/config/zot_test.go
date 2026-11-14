package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Zot release tags the tests pick among.
const (
	zotV2121 = "v2.1.21"
	zotV2122 = "v2.1.22"
	zotV2123 = "v2.1.23"
	zotV2124 = "v2.1.24"
	zotV300  = "v3.0.0"
)

// TestPickZotRelease: the zot the lab follows is the highest release of
// major 2 at or above the floor; prereleases, test builds, other shapes,
// releases below the floor and another major are skipped, whatever the order.
func TestPickZotRelease(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []string
		want string
	}{
		{
			name: "gsoci's tags as they are",
			tags: []string{"v2.0.2", "v2.0.2-rc1", "v2.1.0", "v2.1.10", "v2.1.11-test", zotV2121, zotV2122, zotV2123, "v2.1.9"},
			want: zotV2123,
		},
		{
			name: "a newer release, a v3 and a prerelease of the next one",
			tags: []string{zotV300, zotV2124, zotV2123, "v2.2.0-rc1", zotV2121},
			want: zotV2124,
		},
		{
			name: "shapes semver alone would read",
			tags: []string{"v2.9", "2.5.0", "v2.4.0+build", "edge", zotV2123},
			want: zotV2123,
		},
		{
			name: "nothing at or above the floor",
			tags: []string{"v2.0.2-rc1", "v2.1.11-test", zotV2121, zotV2122, zotV300},
		},
		{name: "no tags"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PickZotRelease(tc.tags)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("PickZotRelease(%v) = %q, %v; want %q", tc.tags, got, ok, tc.want)
			}
		})
	}
}

// TestCheckZotVersion: zotVersion is empty or a v2 release tag; the floor is
// checked by configure and the lab registry, not by loading the file.
func TestCheckZotVersion(t *testing.T) {
	for _, tc := range []struct {
		tag             string
		valid, atFloor  bool
		wantFloorRefuse bool
	}{
		{tag: "", valid: true, atFloor: true},
		{tag: zotV2123, valid: true, atFloor: true},
		{tag: "v2.1.30", valid: true, atFloor: true},
		{tag: zotV2121, valid: true, wantFloorRefuse: true},
		{tag: "2.1.23"},
		{tag: "v2.1.23-rc1"},
		{tag: zotV300},
		{tag: "v2.1"},
		{tag: "latest"},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			if err := ValidateZotVersion(tc.tag); (err == nil) != tc.valid {
				t.Errorf("ValidateZotVersion(%q) = %v, want valid %v", tc.tag, err, tc.valid)
			}
			err := CheckZotVersion(tc.tag)
			if (err == nil) != tc.atFloor {
				t.Errorf("CheckZotVersion(%q) = %v, want accepted %v", tc.tag, err, tc.atFloor)
			}
			if tc.wantFloorRefuse && (err == nil || !strings.Contains(err.Error(), "v"+ZotFloor.String())) {
				t.Errorf("CheckZotVersion(%q) = %v, want a refusal naming the floor v%s", tc.tag, err, ZotFloor)
			}
		})
	}
}

// TestValidateRefusesABadZotVersion: a zotVersion of another shape fails
// the file's validation, naming the key.
func TestValidateRefusesABadZotVersion(t *testing.T) {
	cfg := Default()
	cfg.ZotVersion = "v2.1.23-rc1"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "zotVersion") {
		t.Fatalf("Validate() = %v, want a refusal naming zotVersion", err)
	}
	cfg.ZotVersion = zotV2121 // below the floor, but loadable: configure mends it
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() with a release below the floor = %v, want nil", err)
	}
}

// TestRegistryMirrorIsOnWithoutAKey: the mirror is on by default and a lab
// that keeps it on carries no key older releases would refuse; only off is
// written.
func TestRegistryMirrorIsOnWithoutAKey(t *testing.T) {
	cfg := Default()
	if !cfg.RegistryMirrorOn() {
		t.Fatal("RegistryMirrorOn() = false for the default lab, want true")
	}
	for _, tc := range []struct {
		on      bool
		wantKey bool
	}{{on: true}, {on: false, wantKey: true}} {
		cfg.SetRegistryMirror(tc.on)
		if cfg.RegistryMirrorOn() != tc.on {
			t.Errorf("SetRegistryMirror(%v): RegistryMirrorOn() = %v", tc.on, cfg.RegistryMirrorOn())
		}
		raw, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(raw), "registryMirror:"); got != tc.wantKey {
			t.Errorf("SetRegistryMirror(%v): the file carries registryMirror: %v, want %v", tc.on, got, tc.wantKey)
		}
	}
}
