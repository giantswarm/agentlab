package lab

import (
	"os"
	"testing"
)

// TestMain fixes the engine to one that is not rootless Podman, so what the
// lab picks from it (workspacesStorageFor) does not follow the machine the
// tests run on; a test of the rootless branch sets it itself.
func TestMain(m *testing.M) {
	rootlessPodmanOnLinux = func() bool { return false }
	os.Exit(m.Run())
}
