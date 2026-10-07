package lab

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// fakeKindKubeconfig is what the stand-in for kind's kubeconfig read answers:
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

// stubKindKubeconfig stands in for the embedded kind's kubeconfig read
// (kindKubeconfigRaw): it answers cluster "agentlab" with fakeKindKubeconfig,
// fails every other cluster with kind's own words, and counts its calls.
func stubKindKubeconfig(t *testing.T) *int {
	t.Helper()
	calls := 0
	prev := kindKubeconfigRaw
	kindKubeconfigRaw = func(name string) ([]byte, error) {
		calls++
		if name != managedByAgentlabValue {
			return nil, fmt.Errorf("kind: reading the kubeconfig of cluster %s: could not locate any control plane nodes for cluster named '%s'", name, name)
		}
		return []byte(fakeKindKubeconfig), nil
	}
	t.Cleanup(func() { kindKubeconfigRaw = prev })
	return &calls
}

func resetKindKubeconfigCache(t *testing.T) {
	t.Helper()
	kindKubeconfigCache.name, kindKubeconfigCache.raw = "", nil
	t.Cleanup(func() { kindKubeconfigCache.name, kindKubeconfigCache.raw = "", nil })
}

// TestUseClusterKubeconfig drives the export against a stand-in for kind's
// kubeconfig read: the lab-owned kubeconfig lands under state/ owner-only and
// byte-identical to what kind emitted, the very file the embedded clients are
// built from whatever the shell's KUBECONFIG says (a bundle built before the
// export is dropped); one read serves both it and the cluster entry the token
// kubeconfigs are built from; and a cluster kind does not know fails by name
// with kind's message instead of leaving the clients to the shell's kubeconfig.
func TestUseClusterKubeconfig(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("KUBECONFIG", "/elsewhere/config")
	calls := stubKindKubeconfig(t)
	resetKindKubeconfigCache(t)

	cfg := config.Default()
	labKubeCache = &kubeClients{} // a bundle built before the export
	t.Cleanup(resetLabKube)
	if err := useClusterKubeconfig(cfg); err != nil {
		t.Fatal(err)
	}
	if labKubeCache != nil {
		t.Error("the export must drop the client bundle built from the previous kubeconfig")
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
	loader := labRESTClientGetter("").ToRawKubeConfigLoader()
	if got := loader.ConfigAccess().GetExplicitFile(); got != labKubeconfig() {
		t.Errorf("the clients read %q, want the exported %q", got, labKubeconfig())
	}
	if kc, err := loader.RawConfig(); err != nil || kc.CurrentContext != "kind-agentlab" {
		t.Errorf("the clients see current-context %q (%v), want kind's from the exported file", kc.CurrentContext, err)
	}

	name, cluster, err := kindClusterEntry(cfg.ClusterName)
	if err != nil {
		t.Fatal(err)
	}
	if name != "kind-agentlab" || cluster["server"] != fakeKindServer {
		t.Errorf("cluster entry = %q %v", name, cluster)
	}
	if *calls != 1 {
		t.Errorf("kind was asked %d times for one export + one entry lookup, want 1 (cached)", *calls)
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

// TestUseClusterKubeconfigRefreshesTheLabsCopies is the lease after `down`
// and `up`: a copy of the lab's admin kubeconfig taken before the cluster was
// recreated (a lab lease holds one) carries the previous cluster's CA, and
// the export of the new cluster rewrites it through the shell's KUBECONFIG —
// no manual step, and owner-only as before. Everything else KUBECONFIG names
// stays as it is: a copy that already is the new cluster's (additions
// included — the sandbox's proxy-url), a kubeconfig that is not this lab's,
// ~/.kube/config even as a stale copy, and a path that is gone.
func TestUseClusterKubeconfigRefreshesTheLabsCopies(t *testing.T) {
	t.Chdir(t.TempDir())
	stubKindKubeconfig(t)
	resetKindKubeconfigCache(t)
	t.Cleanup(resetLabKube)

	// Zm9v is the new cluster's CA and certificates (fakeKindKubeconfig),
	// b2xk the previous cluster's.
	previous := strings.ReplaceAll(fakeKindKubeconfig, "Zm9v", "b2xk")
	proxied := strings.Replace(fakeKindKubeconfig, "    server:", "    proxy-url: socks5://127.0.0.1:1080\n    server:", 1)
	foreign := strings.ReplaceAll(fakeKindKubeconfig, "kind-agentlab", "prod")
	home := t.TempDir()
	t.Setenv("HOME", home)
	lease := filepath.Join(t.TempDir(), "lease", "kubeconfig")
	files := map[string]string{
		lease: previous,
		filepath.Join(t.TempDir(), "sandbox", "kubeconfig"): proxied,
		filepath.Join(t.TempDir(), "prod.yaml"):             foreign,
		filepath.Join(home, ".kube", "config"):              previous,
	}
	var paths []string
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	gone := filepath.Join(t.TempDir(), "released", "kubeconfig")
	paths = append(paths, gone)
	slices.Sort(paths)
	t.Setenv("KUBECONFIG", strings.Join(paths, string(os.PathListSeparator)))

	if err := useClusterKubeconfig(config.Default()); err != nil {
		t.Fatal(err)
	}
	for path, before := range files {
		raw, err := os.ReadFile(path) // #nosec G304 -- the test's own temporary files
		if err != nil {
			t.Fatal(err)
		}
		want := before
		if before == previous && !strings.HasPrefix(path, home) {
			want = fakeKindKubeconfig
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("the refreshed copy %s has mode %v, want owner-only 0600", path, info.Mode().Perm())
			}
		}
		if string(raw) != want {
			t.Errorf("%s after the export:\n%s\nwant:\n%s", path, raw, want)
		}
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("a released lease's path must not come back: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(lease)); len(entries) != 1 {
		t.Errorf("the refresh left %d entries in the lease, want the copy alone (no .tmp)", len(entries))
	}
}
