package lab

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

// checkHostPortsFree refuses a `kind create` that would fail on a host port
// another process already holds. kind binds every port mapping of the node
// at creation and reports a clash as a bare docker "address already in use",
// naming neither the lab's setting nor the holder; this names both, one line
// per port, before minutes of image pulls and a half-created node.
func checkHostPortsFree(cfg *config.Config) error {
	var own map[int]bool
	if port := devRegistryOwnPort(cfg); port != 0 {
		own = map[int]bool{port: true}
	}
	conflicts := cfg.PortConflicts(own)
	if len(conflicts) == 0 {
		return nil
	}
	lines := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		holder := "another process"
		if name := portHolder(c.Port); name != "" {
			holder = name
		}
		lines = append(lines, fmt.Sprintf("127.0.0.1:%d (%s, %s) of the lab %q is held by %s", c.Port, c.Field, c.What, cfg.ClusterName, holder))
	}
	return fmt.Errorf("%s: stop it, or run `agentlab configure` to move the lab to free ports, then `agentlab up`", strings.Join(lines, "; "))
}

// portHolder names the process holding a TCP socket on the local port — its
// short name and PID, never its command line, which can carry secrets — or
// "" when it is not visible: off Linux, or a process of another user (a
// root-owned docker-proxy) whose descriptors this one cannot read.
func portHolder(port int) string {
	if runtime.GOOS != goosLinux {
		return ""
	}
	inodes := socketInodes(port)
	if len(inodes) == 0 {
		return ""
	}
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	for _, fd := range fds {
		link, err := os.Readlink(fd)
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		if !inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] {
			continue
		}
		pid := filepath.Base(filepath.Dir(filepath.Dir(fd)))
		comm, err := os.ReadFile(filepath.Join("/proc", pid, "comm")) // #nosec G304 -- a /proc entry, the PID from the glob
		if err != nil {
			continue
		}
		return fmt.Sprintf("%s (pid %s)", strings.TrimSpace(string(comm)), pid)
	}
	return ""
}

// socketInodes returns the inodes of the TCP sockets bound to the local port,
// from the kernel's tables for both address families.
func socketInodes(port int) map[string]bool {
	inodes := map[string]bool{}
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		// A table that cannot be read (no IPv6) only means fewer candidates.
		found, _ := socketInodesIn(table, port)
		for _, inode := range found {
			inodes[inode] = true
		}
	}
	return inodes
}

// socketInodesIn reads one /proc/net/tcp-format table: after the header, the
// local address is field 1 (hex address:hex port) and the inode field 9.
func socketInodesIn(table string, port int) ([]string, error) {
	f, err := os.Open(table) // #nosec G304 -- the fixed /proc/net tables
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var inodes []string
	s := bufio.NewScanner(f)
	s.Scan() // header
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 10 {
			continue
		}
		_, hexPort, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		if p, err := strconv.ParseUint(hexPort, 16, 16); err != nil || int(p) != port {
			continue
		}
		if fields[9] != "0" {
			inodes = append(inodes, fields[9])
		}
	}
	return inodes, s.Err()
}
