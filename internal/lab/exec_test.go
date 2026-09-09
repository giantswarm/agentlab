package lab

import (
	"errors"
	"strings"
	"testing"
)

// TestOutputQuietErrorCarriesStderr: a failed probe's error says what the
// command said, not just "exit status 1" — the empty stdout of a failed
// docker read is otherwise indistinguishable from "nothing there yet". On
// success stderr stays off both the error and the terminal.
func TestOutputQuietErrorCarriesStderr(t *testing.T) {
	out, err := outputQuiet("sh", "-c", "echo partial; echo 'Error: No such container: agentlab-control-plane' >&2; exit 1")
	if err == nil {
		t.Fatal("expected the failing command to error")
	}
	if out != "partial\n" {
		t.Errorf("stdout = %q, want the partial output kept", out)
	}
	for _, want := range []string{"sh -c", "exit status 1", "No such container: agentlab-control-plane"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}

	out, err = outputQuiet("sh", "-c", "echo ok; echo noise >&2")
	if err != nil || out != "ok\n" {
		t.Errorf("success: out=%q err=%v, want ok and nil", out, err)
	}
}

// TestNotReached: a wait loop whose last status read failed reports the read
// failure — with the apiserver's words — instead of an empty "last status",
// which would read exactly like a CR the controller never touched.
func TestNotReached(t *testing.T) {
	readErr := errors.New(`Get "https://127.0.0.1:34547/apis/kagent.dev/v1alpha2/namespaces/kagent/modelconfigs/qwen": dial tcp 127.0.0.1:34547: connect: connection refused`)
	err := notReached("ModelConfig qwen", "Accepted", "", readErr, "check `kubectl -n kagent describe modelconfigs.kagent.dev qwen`")
	for _, want := range []string{"ModelConfig qwen: the status read failed:", "connection refused", "check `kubectl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("read failure %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "last status") {
		t.Errorf("read failure %q must not pose as a status", err)
	}
	if !errors.Is(err, readErr) {
		t.Errorf("read failure should wrap the apiserver's error")
	}

	err = notReached("ModelConfig qwen", "Accepted", "False", nil, "check it")
	if want := `ModelConfig qwen never reached Accepted (last status: "False");` + "\ncheck it"; err.Error() != want {
		t.Errorf("status failure = %q, want %q", err, want)
	}
}
