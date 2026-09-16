package machineid

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// withReader swaps the per-OS source for one the test controls.
func withReader(t *testing.T, fn func() (string, error)) {
	t.Helper()
	prev := read
	read = fn
	t.Cleanup(func() { read = prev })
}

// TestIDIsStableAndCanonical runs against whichever reader was compiled in,
// so it proves kern.uuid on a Mac, /etc/machine-id on the Linux CI runner and
// the registry on a Windows machine — the per-OS files are one call each and
// this is what exercises them.
func TestIDIsStableAndCanonical(t *testing.T) {
	first, ok := ID()
	second, okAgain := ID()
	if ok != okAgain || first != second {
		t.Fatalf("ID() is not stable: (%q, %v) then (%q, %v)", first, ok, second, okAgain)
	}
	if !ok {
		if first != "" {
			t.Errorf("ID() = %q with ok=false, want the empty string", first)
		}
		t.Skip("this machine exposes no identifier; the shape assertions need one")
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Errorf("ID() = %q, which does not parse as a UUID: %v", first, err)
	}
	if first != strings.ToLower(first) {
		t.Errorf("ID() = %q, want the canonical lower-case spelling", first)
	}
}

func TestCanonicalAcceptsEveryOSFormAndRejectsJunk(t *testing.T) {
	const (
		bare = "12a0d395dbb93050b357f0f9f3185660"
		want = "12a0d395-dbb9-3050-b357-f0f9f3185660"
	)
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"macOS and Windows spell it dashed and upper-case", "12A0D395-DBB9-3050-B357-F0F9F3185660", want},
		{"systemd spells it as 32 bare hex digits", bare, want},
		{"some registry values carry braces", "{12A0D395-DBB9-3050-B357-F0F9F3185660}", want},
		{"a file read keeps the trailing newline", bare + "\n", want},
		{"and sysctl output may carry spaces", "  12A0D395-DBB9-3050-B357-F0F9F3185660  ", want},
		{"nothing at all", "", ""},
		{"only whitespace", "   \n", ""},
		{"systemd's first-boot marker is not an identifier", "uninitialized", ""},
		{"neither is the all-zeros UUID", "00000000-0000-0000-0000-000000000000", ""},
		{"a truncated value", "12a0d395dbb93050", ""},
		{"something else entirely", "not-a-uuid", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := canonical(tc.raw)
			if got != tc.want || ok != (tc.want != "") {
				t.Errorf("canonical(%q) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.want != "")
			}
		})
	}
}

func TestIDFallsBackWhenTheSourceFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func() (string, error)
	}{
		{"the source errors", func() (string, error) { return "", errors.New("no such file") }},
		{"the source returns nothing", func() (string, error) { return "", nil }},
		{"the source returns something unusable", func() (string, error) { return "uninitialized", nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withReader(t, tc.fn)
			if got, ok := ID(); got != "" || ok {
				t.Errorf("ID() = (%q, %v), want (\"\", false)", got, ok)
			}
		})
	}
}

// TestFirstFileSkipsMissingAndEmpty covers the Linux fallback chain from any
// platform: firstFile is untagged precisely so this runs on a Mac too.
func TestFirstFileSkipsMissingAndEmpty(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		return p
	}
	const stored = "12a0d395dbb93050b357f0f9f3185660"
	missing := filepath.Join(dir, "absent")
	empty := write("empty", "")
	blank := write("blank", "  \n")
	uninitialized := write("uninitialized", "uninitialized\n")
	zeros := write("zeros", "00000000-0000-0000-0000-000000000000\n")
	filled := write("filled", stored+"\n")

	for _, tc := range []struct {
		name  string
		paths []string
		want  string
	}{
		{"the first path wins when it holds something", []string{filled, missing}, stored},
		{"a missing first path falls through", []string{missing, filled}, stored},
		{"an empty one falls through", []string{empty, filled}, stored},
		{"so does one holding only whitespace", []string{blank, filled}, stored},
		// The case /var/lib/dbus/machine-id exists for: systemd writes
		// "uninitialized" before the first save, and a first path that holds
		// it must not stop the search.
		{"systemd's first-boot marker falls through to the next path", []string{uninitialized, filled}, stored},
		{"so does an all-zeros identifier", []string{zeros, filled}, stored},
		{"a first path holding junk does not win", []string{uninitialized, zeros, blank, filled}, stored},
		{"nothing readable is an error", []string{missing, empty}, ""},
		{"nothing usable is an error too", []string{uninitialized, zeros}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := firstFile(tc.paths...)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("firstFile(%v) = %q, want an error", tc.paths, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("firstFile(%v): %v", tc.paths, err)
			}
			if got != tc.want {
				t.Errorf("firstFile(%v) = %q, want %q", tc.paths, got, tc.want)
			}
		})
	}
}
