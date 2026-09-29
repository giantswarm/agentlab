package lab

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	"github.com/giantswarm/agentlab/internal/config"
)

// The node mounts the host-kernel guard read-only from state/node: kubelet's
// drop-in, the shadow script it runs and the empty sysctl.d over the node
// image's Debian defaults.
func TestKindConfigGuardsHostKernel(t *testing.T) {
	out, err := renderTemplate(config.Default(), "kind-config.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	var kindCfg v1alpha4.Cluster
	if err := yaml.Unmarshal(out, &kindCfg); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	dir, err := nodeFilesDir()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		kubeletHostKernelDropIn: filepath.Join(dir, "kubelet-host-kernel.conf"),
		shadowTunablesScript:    filepath.Join(dir, "shadow-kubelet-tunables.sh"),
		"/usr/lib/sysctl.d":     filepath.Join(dir, "sysctl.d"),
	}
	got := map[string]string{}
	for _, m := range kindCfg.Nodes[0].ExtraMounts {
		if _, ok := want[m.ContainerPath]; ok {
			if !m.Readonly {
				t.Errorf("mount %s is writable, want readOnly", m.ContainerPath)
			}
			got[m.ContainerPath] = m.HostPath
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("host-kernel mounts = %v, want %v", got, want)
	}
}

// The drop-in runs the script the node mounts, and the script shadows every
// kubelet tunable with kubelet's own value — valid sh, since systemd only
// logs its failure.
func TestShadowTunablesScript(t *testing.T) {
	if !strings.Contains(kubeletHostKernelDropInContent, "ExecStartPre=-/bin/sh "+shadowTunablesScript+"\n") {
		t.Errorf("drop-in does not run %s:\n%s", shadowTunablesScript, kubeletHostKernelDropInContent)
	}
	script := shadowTunablesScriptContent()
	for key, value := range kubeletKernelTunables {
		if !strings.Contains(script, "\n"+key+" "+value+"\n") {
			t.Errorf("script does not shadow %s with %s:\n%s", key, value, script)
		}
	}
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v\n%s", err, out)
	}
}

// The files exist before `kind create` — docker would create a missing bind
// source as a directory — and sysctl.d is empty.
func TestWriteNodeFiles(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := writeNodeFiles(); err != nil {
		t.Fatal(err)
	}
	dir, err := nodeFilesDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kubelet-host-kernel.conf", "shadow-kubelet-tunables.sh"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || !info.Mode().IsRegular() {
			t.Errorf("%s: %v, want a regular file", name, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "sysctl.d"))
	if err != nil || len(entries) != 0 {
		t.Errorf("sysctl.d: %v, %d entries, want an empty directory", err, len(entries))
	}
	if err := writeNodeFiles(); err != nil {
		t.Errorf("second write: %v", err)
	}
}

// Only a watched key read both times with a different value is a change, in
// sysctl(8)'s spelling.
func TestHostKernelChanges(t *testing.T) {
	before := map[string]string{
		"kernel/panic":         "0",
		"kernel/panic_on_oops": "0",
		"kernel/core_pattern":  "|/usr/lib/systemd/systemd-coredump %P",
		"vm/overcommit_memory": "0",
	}
	after := map[string]string{
		"kernel/panic":         "10",
		"kernel/panic_on_oops": "0",
		"kernel/core_pattern":  "core",
		"kernel/pid_max":       "4194304",
	}
	want := []hostKernelChange{
		{"kernel.panic", "0", "10"},
		{"kernel.core_pattern", "|/usr/lib/systemd/systemd-coredump %P", "core"},
	}
	if got := hostKernelChanges(before, after); !reflect.DeepEqual(got, want) {
		t.Errorf("hostKernelChanges = %v, want %v", got, want)
	}
}
