package project

import "testing"

const stamped = "0.17.0"

func TestVersionPrefersLinkerValueAndAddsV(t *testing.T) {
	t.Cleanup(func() { version = "" })
	for in, want := range map[string]string{
		stamped:        "v" + stamped,
		"v" + stamped:  "v" + stamped,
		" 1.2.3-rc.1 ": "v1.2.3-rc.1",
	} {
		version = in
		if got := Version(); got != want {
			t.Errorf("version %q: got %q, want %q", in, got, want)
		}
	}
}

func TestVersionFallsBackToBuildInfoOrDev(t *testing.T) {
	version = ""
	// Under `go test` the main module is the test binary with version
	// "(devel)" or empty, so the fallback chain must end in "dev" — and
	// never in an empty string, which the usage signal could not group by.
	if got := Version(); got == "" {
		t.Fatal("Version() must never be empty")
	}
}

func TestVersionLineCarriesDetailsOnlyWhenKnown(t *testing.T) {
	t.Cleanup(func() { version, gitSHA, buildTimestamp = "", "", "" })
	version, gitSHA, buildTimestamp = stamped, "8b5caa0abcdef0123456789", "2026-09-07T10:00:00Z"
	if got, want := VersionLine(), "v0.17.0 (commit 8b5caa0, built 2026-09-07T10:00:00Z)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	version, gitSHA, buildTimestamp = stamped, "", ""
	if got := VersionLine(); got != "v0.17.0" && got[:8] != "v0.17.0 " {
		t.Errorf("unexpected line %q", got)
	}
}
