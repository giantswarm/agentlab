package lab

import (
	"fmt"
	"os"
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
		if name != "agentlab" {
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
// byte-identical to what kind emitted, the very file the command constructor
// pins kubectl to; one read serves both it and the cluster entry the token
// kubeconfigs are built from; and a cluster kind does not know fails by name
// with kind's message instead of leaving kubectl to the shell's kubeconfig.
func TestUseClusterKubeconfig(t *testing.T) {
	t.Chdir(t.TempDir())
	calls := stubKindKubeconfig(t)
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
