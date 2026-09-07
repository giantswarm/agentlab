package lab

import (
	"os"
	"strconv"
	"strings"
	"sync"
)

// The lab drives containers through the `docker` CLI, and Podman's
// docker-compatible CLI answers to it too (kind picks its podman provider
// the same way). Docker is the primary path; where podman differs, the code
// branches on dockerIsPodman. The differences:
//
//   - `docker save a b c` writes ONE image carrying every name as a tag
//     (podman's archive is single-image unless --multi-image-archive is
//     passed), and kind's `load docker-image` runs exactly that save — so
//     the lab loads one image per call (kindLoadImages, HACKS.md U16);
//   - pods reach the host at host.containers.internal, not the bridge
//     gateway, and the host cannot dial that address itself (kindGatewayIP,
//     hostServerAnswers);
//   - local builds are spelled `localhost/<name>` and carry a digest like any
//     pull, so the name is what marks them local (parseImageProvenance);
//   - rootless podman publishes ports from the user's own network namespace
//     and cannot bind the privileged range, so the gateway's default 443 has
//     to move (MinPublishablePort, config.ChooseFreePorts).

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

// defaultUnprivilegedPortStart is the kernel's own default for
// net.ipv4.ip_unprivileged_port_start.
const defaultUnprivilegedPortStart = 1024

// MinPublishablePort is the lowest host port the container engine can publish.
// Docker's daemon binds as root, so every port is available to it. Rootless
// podman publishes from the invoking user's network namespace, where the
// kernel refuses everything below net.ipv4.ip_unprivileged_port_start — the
// lab's default gateway port 443 among them.
func MinPublishablePort() int {
	if !dockerIsPodman() || os.Geteuid() == 0 {
		return 1
	}
	return unprivilegedPortStart(os.ReadFile)
}

// unprivilegedPortStart reads net.ipv4.ip_unprivileged_port_start. An
// unreadable or unparsable sysctl means the kernel default holds.
func unprivilegedPortStart(readFile func(string) ([]byte, error)) int {
	raw, err := readFile("/proc/sys/net/ipv4/ip_unprivileged_port_start")
	if err != nil {
		return defaultUnprivilegedPortStart
	}
	start, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || start < 1 || start > 65535 {
		return defaultUnprivilegedPortStart
	}
	return start
}
