// Package machineid reads the identifier the operating system keeps for the
// computer it runs on: `kern.uuid` on macOS, /etc/machine-id on Linux, the
// registry's MachineGuid on Windows. Each is a UUID the OS assigns once and
// keeps across reboots, networks and hardware changes.
//
// The value is an input to agentlab's usage-signal user identifier and is
// hashed before it goes anywhere (internal/telemetry, docs/telemetry.md
// "Usage data"); nothing here sends or stores it.
package machineid

import (
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
)

// read is the per-OS source, one implementation per machineid_<goos>.go.
// Tests swap it.
var read = readMachineID

// ID returns this computer's identifier in one canonical spelling, or false
// when the OS has none to give: a container without /etc/machine-id, a
// Windows without the registry value, an OS with no source at all. Callers
// fall back rather than fail.
func ID() (string, bool) {
	raw, err := read()
	if err != nil {
		return "", false
	}
	return canonical(raw)
}

// canonical turns every spelling the three sources use — dashed, 32 bare hex
// digits, braced, newline-terminated — into one, and rejects what is not an
// identifier. The all-zeros UUID and systemd's literal "uninitialized" (a
// real /etc/machine-id content before the first save) would otherwise make
// every machine in that state the same machine; a non-empty check misses both.
func canonical(raw string) (string, bool) {
	u, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil || u == uuid.Nil {
		return "", false
	}
	return u.String(), true
}

// firstFile returns the contents of the first path holding an identifier.
// Missing, empty and unusable all fall through alike: systemd writes the
// literal "uninitialized" into /etc/machine-id before the first save, and
// treating that as an answer would leave the later paths unread in the one
// case they exist for. It lives here rather than in machineid_linux.go so
// its branches run on every platform the tests do.
func firstFile(paths ...string) (string, error) {
	for _, p := range paths {
		raw, err := os.ReadFile(p) // #nosec G304 -- fixed OS paths, varied only by tests
		if err != nil {
			continue
		}
		if _, ok := canonical(string(raw)); ok {
			return strings.TrimSpace(string(raw)), nil
		}
	}
	return "", fmt.Errorf("none of %s holds a machine identifier", strings.Join(paths, ", "))
}
