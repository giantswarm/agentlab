package lab

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLabRESTClientGetterPinsKubeconfig: the embedded clients read the
// lab-owned kubeconfig and nothing else — the shell's KUBECONFIG is ignored,
// the namespace is the operation's, and the REST config carries the lab's
// throughput and identity.
func TestLabRESTClientGetterPinsKubeconfig(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("KUBECONFIG", "/elsewhere/config")
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
  - name: kind-lab
    cluster:
      server: https://127.0.0.1:6443
      insecure-skip-tls-verify: true
users:
  - name: kind-lab
    user:
      token: t
contexts:
  - name: kind-lab
    context:
      cluster: kind-lab
      user: kind-lab
      namespace: from-kubeconfig
current-context: kind-lab
`
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(labKubeconfigPath, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}

	getter := labRESTClientGetter("agent-platform")
	if got := getter.ToRawKubeConfigLoader().ConfigAccess().GetExplicitFile(); got != labKubeconfig() {
		t.Errorf("explicit kubeconfig %q, want %q", got, labKubeconfig())
	}
	ns, _, err := getter.ToRawKubeConfigLoader().Namespace()
	if err != nil || ns != "agent-platform" {
		t.Errorf("namespace %q, %v; want the operation's", ns, err)
	}
	cfg, err := getter.ToRESTConfig()
	if err != nil {
		t.Fatalf("ToRESTConfig: %v", err)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Errorf("host %q, want the lab kubeconfig's server", cfg.Host)
	}
	if cfg.QPS != 50 || cfg.Burst != 100 {
		t.Errorf("QPS/Burst %v/%v, want 50/100", cfg.QPS, cfg.Burst)
	}
	if !strings.HasPrefix(cfg.UserAgent, "agentlab/") {
		t.Errorf("user agent %q, want the lab's", cfg.UserAgent)
	}

	// Without a namespace the kubeconfig's context namespace applies.
	ns, _, err = labRESTClientGetter("").ToRawKubeConfigLoader().Namespace()
	if err != nil || ns != "from-kubeconfig" {
		t.Errorf("namespace %q, %v; want the kubeconfig's", ns, err)
	}

	// A lab that is not up: the file is missing and the error names it.
	if err := os.Remove(labKubeconfigPath); err != nil {
		t.Fatal(err)
	}
	if _, err := labRESTClientGetter("").ToRESTConfig(); err == nil || !strings.Contains(err.Error(), filepath.Base(labKubeconfigPath)) {
		t.Errorf("missing kubeconfig: %v, want an error naming it", err)
	}
}
