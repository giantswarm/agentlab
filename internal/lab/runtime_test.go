package lab

import "testing"

// isPodmanVersion tells podman's `docker --version` line from docker's.
func TestIsPodmanVersion(t *testing.T) {
	if !isPodmanVersion("podman version 5.8.4\n") || isPodmanVersion("Docker version 29.7.2, build abc") {
		t.Fatal("isPodmanVersion misreads `docker --version`")
	}
}
