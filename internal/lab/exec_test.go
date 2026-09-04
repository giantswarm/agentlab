package lab

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// lastEnv returns the last KEY=value entry for key — the one os/exec hands the
// child when a key repeats.
func lastEnv(env []string, key string) string {
	last := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			last = kv
		}
	}
	return last
}

// TestCommandPinsKubeconfig: kubectl and helm run against the lab-owned
// kubeconfig whatever the shell has — an inherited KUBECONFIG is overridden
// and a missing current-context is irrelevant — while kind and every other
// tool inherit the environment untouched, because kind manages the user's own
// kubeconfig.
func TestCommandPinsKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", "/elsewhere/config")
	if !filepath.IsAbs(labKubeconfig()) || !strings.HasSuffix(labKubeconfig(), filepath.FromSlash(labKubeconfigPath)) {
		t.Fatalf("labKubeconfig() = %q, want an absolute path ending in %q", labKubeconfig(), labKubeconfigPath)
	}
	want := "KUBECONFIG=" + labKubeconfig()
	for _, name := range []string{"kubectl", "helm"} {
		if got := lastEnv(command(name, "version").Env, "KUBECONFIG"); got != want {
			t.Errorf("%s: effective KUBECONFIG %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"kind", "docker", "git"} {
		if got := lastEnv(command(name, "version").Env, "KUBECONFIG"); got != "KUBECONFIG=/elsewhere/config" {
			t.Errorf("%s: effective KUBECONFIG %q, want the inherited one", name, got)
		}
	}
}

// TestOutputQuietErrorCarriesStderr: a failed probe's error says what the
// command said, not just "exit status 1" — the empty stdout of a failed
// kubectl read is otherwise indistinguishable from "no status yet". On
// success stderr stays off both the error and the terminal.
func TestOutputQuietErrorCarriesStderr(t *testing.T) {
	out, err := outputQuiet("sh", "-c", "echo partial; echo 'error: current-context is not set' >&2; exit 1")
	if err == nil {
		t.Fatal("expected the failing command to error")
	}
	if out != "partial\n" {
		t.Errorf("stdout = %q, want the partial output kept", out)
	}
	for _, want := range []string{"sh -c", "exit status 1", "error: current-context is not set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}

	out, err = outputQuiet("sh", "-c", "echo ok; echo noise >&2")
	if err != nil || out != "ok\n" {
		t.Errorf("success: out=%q err=%v, want ok and nil", out, err)
	}
}

// TestNotReached: a wait loop whose last status read failed reports the
// kubectl failure — with kubectl's words — instead of an empty "last status",
// which is exactly how a shell without a current-context made every proof
// read as a CR the controller never touched.
func TestNotReached(t *testing.T) {
	readErr := errors.New(`kubectl -n kagent get modelconfigs.kagent.dev qwen: exit status 1: error: current-context is not set`)
	err := notReached("ModelConfig qwen", "Accepted", "", readErr, "check `kubectl -n kagent describe modelconfigs.kagent.dev qwen`")
	for _, want := range []string{"ModelConfig qwen: kubectl failed:", "current-context is not set", "check `kubectl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("read failure %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "last status") {
		t.Errorf("read failure %q must not pose as a status", err)
	}
	if !errors.Is(err, readErr) {
		t.Errorf("read failure should wrap the kubectl error")
	}

	err = notReached("ModelConfig qwen", "Accepted", "False", nil, "check it")
	if want := `ModelConfig qwen never reached Accepted (last status: "False");` + "\ncheck it"; err.Error() != want {
		t.Errorf("status failure = %q, want %q", err, want)
	}
}
