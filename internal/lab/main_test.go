package lab

import (
	"os"
	"testing"
)

// TestMain fixes the engine to one that is not rootless Podman, so what the
// lab picks from it (workspacesStorageFor) does not follow the machine the
// tests run on; a test of the rootless branch sets it itself. The meta chart
// reads as one that declares no values, so no test reaches a registry; a
// test of a chart's values stubs them (stubPlatformChartValues).
func TestMain(m *testing.M) {
	rootlessPodmanOnLinux = func() bool { return false }
	platformChartValuesProbe = func(platformChart) (platformChartValues, error) { return platformChartValues{}, nil }
	os.Exit(m.Run())
}
