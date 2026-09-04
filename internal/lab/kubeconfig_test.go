package lab

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// fakeKindKubeconfig is what the stand-in kind emits for `get kubeconfig`:
// the shape of the real output, with the admin client certificate a token
// kubeconfig must not inherit.
const fakeKindKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: Zm9v
    server: https://127.0.0.1:34547
  name: kind-agentlab
contexts:
- context:
    cluster: kind-agentlab
    user: kind-agentlab
  name: kind-agentlab
current-context: kind-agentlab
users:
- name: kind-agentlab
  user:
    client-certificate-data: Zm9v
    client-key-data: Zm9v
`

// installFakeKind puts a `kind` on PATH that answers `get kubeconfig --name
// agentlab` with fakeKindKubeconfig, fails every other cluster the way kind
// does, and logs each invocation to a file; returns that file.
func installFakeKind(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o750); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "kind-calls")
	script := "#!/bin/sh\n" +
		"echo \"kind $*\" >> \"$KIND_CALLS\"\n" +
		"if [ \"$1 $2 $3 $4\" != \"get kubeconfig --name agentlab\" ]; then\n" +
		"  echo \"ERROR: could not locate any control plane nodes for cluster named '$4'\" >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"cat <<'KUBECONFIG'\n" + fakeKindKubeconfig + "KUBECONFIG\n"
	if err := os.WriteFile(filepath.Join(bin, "kind"), []byte(script), 0o700); err != nil { // #nosec G306 -- an executable stand-in under t.TempDir
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KIND_CALLS", calls)
	return calls
}

func resetKindKubeconfigCache(t *testing.T) {
	t.Helper()
	kindKubeconfigCache.name, kindKubeconfigCache.raw = "", nil
	t.Cleanup(func() { kindKubeconfigCache.name, kindKubeconfigCache.raw = "", nil })
}

// TestUseClusterKubeconfig drives the export against a stand-in kind: the
// lab-owned kubeconfig lands under state/ owner-only and byte-identical to
// what kind emitted, the very file the command constructor pins kubectl to;
// one kind call serves both it and the cluster entry the token kubeconfigs
// are built from; and a cluster kind does not know fails by name with kind's
// message instead of leaving kubectl to the shell's kubeconfig.
func TestUseClusterKubeconfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	calls := installFakeKind(t, dir)
	resetKindKubeconfigCache(t)

	cfg := config.Default()
	if err := useClusterKubeconfig(cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(labKubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state/kubeconfig mode %v, want owner-only 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(labKubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != fakeKindKubeconfig {
		t.Errorf("state/kubeconfig is not kind's output:\n%s", raw)
	}
	if got, want := lastEnv(command("kubectl", "get", "pods").Env, "KUBECONFIG"), "KUBECONFIG="+labKubeconfig(); got != want {
		t.Errorf("kubectl runs with %q, want %q", got, want)
	}

	name, cluster, err := kindClusterEntry(cfg.ClusterName)
	if err != nil {
		t.Fatal(err)
	}
	if name != "kind-agentlab" || cluster["server"] != "https://127.0.0.1:34547" {
		t.Errorf("cluster entry = %q %v", name, cluster)
	}
	if log, _ := os.ReadFile(calls); strings.Count(string(log), "\n") != 1 { // #nosec G304 -- the call log this test's stand-in wrote under t.TempDir
		t.Errorf("kind was run %d times for one export + one entry lookup, want 1 (cached):\n%s", strings.Count(string(log), "\n"), log)
	}

	resetKindKubeconfigCache(t)
	cfg.ClusterName = "nope"
	err = useClusterKubeconfig(cfg)
	if err == nil {
		t.Fatal("a cluster kind does not know must fail the export")
	}
	for _, want := range []string{`"nope"`, "agentlab up", "could not locate any control plane nodes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing-cluster error %q lacks %q", err, want)
		}
	}
}
