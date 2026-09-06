package lab

import (
	"strings"
	"sync"
)

// The lab drives containers through the `docker` CLI, and Podman's
// docker-compatible CLI answers to it too (kind picks its podman provider
// the same way). Docker is the primary path; where podman differs, the code
// branches on dockerIsPodman:
//
//   - `docker save a b c` writes ONE image carrying every name as a tag
//     (podman's archive is single-image unless --multi-image-archive is
//     passed), and kind's `load docker-image` runs exactly that save — so
//     the lab loads one image per call (kindLoadImages, HACKS.md U16).

// dockerIsPodman reports whether the `docker` on PATH is Podman's
// docker-compatible CLI. Cached for the process: the answer cannot change
// under a running command.
var dockerIsPodman = sync.OnceValue(func() bool {
	out, err := outputQuiet("docker", "--version")
	return err == nil && isPodmanVersion(out)
})

// isPodmanVersion reads `docker --version`: "podman version 5.8.4" for the
// compatible CLI, "Docker version 29.7.2, build ..." for docker.
func isPodmanVersion(out string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(out)), "podman")
}
