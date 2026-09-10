package lab

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// stepClock anchors the elapsed stamp on step lines, so boot logs show where
// the time goes without external timing.
var stepClock = time.Now()

// step prints a top-level progress line, matching the ==> style the shell
// scripts used, stamped with the elapsed time since the process started.
func step(format string, a ...any) {
	e := time.Since(stepClock).Round(time.Second)
	fmt.Printf("==> [%02d:%02d] "+format+"\n",
		append([]any{int(e.Minutes()), int(e.Seconds()) % 60}, a...)...)
}

func note(format string, a ...any) {
	fmt.Printf("    "+format+"\n", a...)
}

// warn prints a loud, indented warning on stderr — for a check that found
// something the boot goes on despite (the runtime's memory below what the
// lab really uses), as opposed to a note, which is informational.
func warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "    WARNING: "+format+"\n", a...)
}

// dockerBin is the one CLI the lab shells out to — docker, or Podman's
// docker-compatible CLI (runtime.go) — for the node container, the image
// pulls and saves, and the probes run inside the node. It is the lab's only
// subprocess: kind (kind.go) and Helm (helm.go) are embedded, and every call
// to the apiserver goes through the embedded Kubernetes client (kube.go),
// bound to the lab-owned kubeconfig by labRESTClientGetter (restclient.go).
// The container engine is therefore what a machine needs installed, and the
// one tool Preflight (discover.go) asks for.
const dockerBin = "docker"

// command builds the exec.Cmd behind the helpers below. The child inherits
// the environment untouched: nothing the lab runs reads a kubeconfig, so
// there is nothing to pin.
func command(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...) // #nosec G204 -- fixed lab tooling (docker) with lab-controlled args
}

// cmdError wraps a failed command with its invocation and, when captured,
// what it said on stderr — so a probe's failure reads "No such container" or
// "permission denied", not just "exit status 1".
func cmdError(name string, args []string, err error, stderr []byte) error {
	if msg := strings.TrimSpace(string(stderr)); msg != "" {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
}

// runQuiet executes a command, showing its output only if it fails.
func runQuiet(name string, args ...string) error {
	var buf bytes.Buffer
	cmd := command(name, args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		_, _ = os.Stderr.Write(buf.Bytes())
		return cmdError(name, args, err, nil)
	}
	return nil
}

// outputQuiet captures stdout and keeps stderr off the terminal; for
// probe-style commands whose failures are expected (an image not yet pulled,
// a node container that does not exist). The error still carries the command
// and its stderr, so a caller that does report the failure says what docker
// said.
func outputQuiet(name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := command(name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), cmdError(name, args, err, stderr.Bytes())
	}
	return stdout.String(), nil
}

// waitFor polls probe up to attempts times, sleeping interval between tries,
// and reports whether it ever succeeded. The one wait loop for every
// "component is eventually up" check.
func waitFor(attempts int, interval time.Duration, probe func() bool) bool {
	for range attempts {
		if probe() {
			return true
		}
		time.Sleep(interval)
	}
	return false
}

// notReached words a wait loop's failure. readErr is the last status read's
// own error: when the read against the apiserver itself failed, the message
// says so and carries the apiserver's words, instead of the empty status a
// failed read leaves behind — which would read exactly like a CR the
// controller had never touched.
func notReached(subject, want, last string, readErr error, hint string) error {
	if readErr != nil {
		return fmt.Errorf("%s: the status read failed: %w;\n%s", subject, readErr, hint)
	}
	return fmt.Errorf("%s never reached %s (last status: %q);\n%s", subject, want, last, hint)
}
