package lab

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// The kind node is a privileged container running systemd, and under a
// rootful engine its root is the host's root: a kernel-global sysctl written
// inside the node is written on the host (agentlab#309, HACKS.md U27). Two
// writers in the node do that at every node start — `agentlab up` and every
// host boot, since the node restarts with docker:
//
//   - kubelet's setupKernelTunables sets six keys to the values it wants —
//     among them kernel.panic_on_oops=1 and kernel.panic=10, which turn any
//     kernel oops on the workstation into a reboot. No kubelet setting leaves
//     the host alone: protectKernelDefaults refuses to start on a host whose
//     values differ, which is every workstation.
//   - the node's systemd-sysctl applies the Debian defaults the node image
//     ships in /usr/lib/sysctl.d — kernel.core_pattern=core among them, which
//     disables the host's systemd-coredump.
//
// The node keeps its hands off: kubelet reads each of its keys from a shadow
// file bind-mounted over /proc/sys inside the node (a kubelet drop-in's
// ExecStartPre, before every kubelet start), sees the value it wants and
// writes nothing; an empty directory over /usr/lib/sysctl.d leaves
// systemd-sysctl only kind's own /etc/sysctl.d, whose net.* keys are
// per-network-namespace. Pods mount their own /proc and read the host's
// values. The mounts are part of the node container, so a node created
// before them keeps writing until `agentlab down && agentlab up`.

// kubeletKernelTunables are the sysctls kubelet's setupKernelTunables
// (pkg/kubelet/cm/container_manager_linux.go) writes, with the values it
// wants — what the shadow files read.
var kubeletKernelTunables = map[string]string{
	"vm/overcommit_memory":      "1",
	"vm/panic_on_oom":           "0",
	"kernel/panic":              "10",
	"kernel/panic_on_oops":      "1",
	"kernel/keys/root_maxkeys":  "1000000",
	"kernel/keys/root_maxbytes": "25000000",
}

// watchedHostKernelKeys are the host sysctls `agentlab up` compares before
// and after it creates the node: kubelet's keys plus the kernel-global ones
// the node image's sysctl.d set.
var watchedHostKernelKeys = append(slices.Sorted(maps.Keys(kubeletKernelTunables)),
	"kernel/core_pattern", "kernel/pid_max")

// nodeFilesDir is where the files the node mounts are written: state/node,
// absolute, the way the kind config's extraMounts need it.
func nodeFilesDir() (string, error) {
	return filepath.Abs(filepath.Join(StateDir, "node"))
}

// Where the node sees them.
const (
	kubeletHostKernelDropIn = "/etc/systemd/system/kubelet.service.d/20-agentlab-host-kernel.conf"
	shadowTunablesScript    = "/usr/local/lib/agentlab/shadow-kubelet-tunables.sh"
)

// kubeletHostKernelDropInContent runs the shadow script before every
// kubelet start; "-" keeps a failed mount from keeping kubelet down.
const kubeletHostKernelDropInContent = `# Written by agentlab (HACKS.md U27): kubelet reads its kernel tunables from
# shadow files, so it never writes the host's kernel settings. A failure is
# logged and kubelet starts anyway; agentlab up reports a changed host value.
[Service]
ExecStartPre=-/bin/sh ` + shadowTunablesScript + `
`

// shadowTunablesScriptContent bind-mounts one shadow file per kubelet
// tunable over /proc/sys, idempotently: kubelet restarts (kubeadm restarts it
// during init) find the mounts in place, and a node restart starts over with
// a fresh /run.
func shadowTunablesScriptContent() string {
	var b strings.Builder
	b.WriteString(`# Written by agentlab (HACKS.md U27): kubelet reads the values it wants from
# these shadow files and leaves the host's kernel settings alone.
set -u
while read -r key value; do
	target=/proc/sys/$key
	findmnt --mountpoint "$target" >/dev/null && continue
	shadow=/run/agentlab/kernel-tunables/$key
	mkdir -p "${shadow%/*}" && echo "$value" >"$shadow" && mount --bind "$shadow" "$target" || exit 1
done <<'EOF'
`)
	for _, key := range slices.Sorted(maps.Keys(kubeletKernelTunables)) {
		fmt.Fprintf(&b, "%s %s\n", key, kubeletKernelTunables[key])
	}
	b.WriteString("EOF\n")
	return b.String()
}

// writeNodeFiles writes what the kind config mounts into the node — the
// kubelet drop-in, its script and the empty sysctl.d. They must exist
// before `kind create`: docker creates a missing bind source as a directory.
func writeNodeFiles() error {
	dir, err := nodeFilesDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "sysctl.d"), 0o750); err != nil {
		return err
	}
	for name, content := range map[string]string{
		"kubelet-host-kernel.conf":   kubeletHostKernelDropInContent,
		"shadow-kubelet-tunables.sh": shadowTunablesScriptContent(),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// hostKernelSettings reads the watched sysctls on a Linux host; nil
// elsewhere, where the engine's VM is the node's "host", not this machine.
func hostKernelSettings() map[string]string {
	if runtime.GOOS != "linux" {
		return nil
	}
	settings := map[string]string{}
	for _, key := range watchedHostKernelKeys {
		if raw, err := os.ReadFile(filepath.Join("/proc/sys", key)); err == nil { // #nosec G304 -- fixed /proc/sys keys
			settings[key] = strings.TrimSpace(string(raw))
		}
	}
	return settings
}

// hostKernelChange is one watched sysctl whose value changed.
type hostKernelChange struct {
	key, was, now string // key as sysctl(8) spells it: kernel.panic
}

// hostKernelChanges lists the watched sysctls whose value changed.
func hostKernelChanges(before, after map[string]string) []hostKernelChange {
	var changes []hostKernelChange
	for _, key := range watchedHostKernelKeys {
		was, known := before[key]
		if now, ok := after[key]; known && ok && was != now {
			changes = append(changes, hostKernelChange{strings.ReplaceAll(key, "/", "."), was, now})
		}
	}
	return changes
}

// warnHostKernelChanges warns about every watched host sysctl the node
// creation changed — what the guard above exists to prevent — with the
// command that puts the values back.
func warnHostKernelChanges(before map[string]string) {
	changes := hostKernelChanges(before, hostKernelSettings())
	if len(changes) == 0 {
		return
	}
	var seen, restore []string
	for _, c := range changes {
		seen = append(seen, fmt.Sprintf("%s %s -> %s", c.key, c.was, c.now))
		restore = append(restore, fmt.Sprintf("'%s=%s'", c.key, c.was))
	}
	warn("creating the node changed this host's kernel settings (HACKS.md U27): %s; restore them with: sudo sysctl -w %s",
		strings.Join(seen, ", "), strings.Join(restore, " "))
}

// warnUnguardedNode warns when an existing lab's node was created without
// the guard: it rewrites the host's kernel settings at every start.
func warnUnguardedNode(node string) {
	if runtime.GOOS != "linux" {
		return
	}
	out, err := outputQuiet("docker", "inspect", "-f", "{{range .Mounts}}{{.Destination}}\n{{end}}", node)
	if err != nil || slices.Contains(strings.Fields(out), kubeletHostKernelDropIn) {
		return
	}
	warn("node %s predates the host kernel guard (HACKS.md U27): at every start it sets kernel.panic=10, kernel.panic_on_oops=1 and vm.overcommit_memory=1 on this host; `agentlab down && agentlab up` recreates it with the guard", node)
}
