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

// The two tools whose cluster is pinned by command; every other subprocess
// (kind, docker, git, helm's plugins) inherits the environment untouched.
const (
	kubectlBin = "kubectl"
	helmBin    = "helm"
)

// command builds the exec.Cmd behind every helper below. kubectl and helm run
// with KUBECONFIG pinned to the lab-owned kubeconfig (labKubeconfigPath, the
// kind cluster's own as exported by useClusterKubeconfig), so which cluster a
// lab command talks to is decided by agentlab.yaml — never by the shell's
// kubeconfig or its current-context, which the lab neither reads nor changes.
// An explicit --kubeconfig flag (the token-only kubeconfigs of test and up)
// still wins, as kubectl's precedence has it. kind is deliberately not
// pinned: it manages the user's own kubeconfig (create/delete cluster merge
// the admin context in and out) and reads the cluster's kubeconfig off the
// node, not off the host.
func command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...) // #nosec G204 -- fixed lab tooling (kind/kubectl/helm) with lab-controlled args
	cmd.Env = os.Environ()
	if name == kubectlBin || name == helmBin {
		// For duplicate keys os/exec keeps the last entry, so an inherited
		// KUBECONFIG is overridden, not merged with.
		cmd.Env = append(cmd.Env, "KUBECONFIG="+labKubeconfig())
	}
	return cmd
}

// cmdError wraps a failed command with its invocation and, when captured,
// what it said on stderr — so a probe's failure reads "current-context is not
// set" or "NotFound", not just "exit status 1".
func cmdError(name string, args []string, err error, stderr []byte) error {
	if msg := strings.TrimSpace(string(stderr)); msg != "" {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
}

// run executes a command with output streamed to the terminal.
func run(name string, args ...string) error {
	cmd := command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runQuiet executes a command, showing output only if it fails.
func runQuiet(name string, args ...string) error {
	return pipeInto(nil, name, args...)
}

// runQuietEnv is runQuiet with extra environment entries ("KEY=value")
// appended to the inherited environment.
func runQuietEnv(extraEnv []string, name string, args ...string) error {
	var buf bytes.Buffer
	cmd := command(name, args...)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		_, _ = os.Stderr.Write(buf.Bytes())
		return cmdError(name, args, err, nil)
	}
	return nil
}

// output captures a command's stdout (stderr goes to the terminal).
func output(name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := command(name, args...)
	cmd.Stdout = &buf
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return buf.String(), err
}

// outputQuiet captures stdout and keeps stderr off the terminal; for
// probe-style commands whose failures are expected. The error still carries
// the command and its stderr, so a caller that does report the failure says
// what kubectl said — the empty stdout of a failed read is otherwise
// indistinguishable from "no status yet".
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

// outputAll captures stdout AND stderr together, for probe-style commands
// whose diagnosis is in the error text (a probe pod's wget message).
func outputAll(name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := command(name, args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// pipeInto feeds input to a command's stdin, showing output only on failure.
func pipeInto(input []byte, name string, args ...string) error {
	var buf bytes.Buffer
	cmd := command(name, args...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		_, _ = os.Stderr.Write(buf.Bytes())
		return cmdError(name, args, err, nil)
	}
	return nil
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
// own error: when the read itself failed, the message says so and carries
// kubectl's words, instead of the empty status a failed read leaves behind —
// which, on a shell without a kubeconfig current-context, read exactly like a
// CR the controller had never touched.
func notReached(subject, want, last string, readErr error, hint string) error {
	if readErr != nil {
		return fmt.Errorf("%s: kubectl failed: %w;\n%s", subject, readErr, hint)
	}
	return fmt.Errorf("%s never reached %s (last status: %q);\n%s", subject, want, last, hint)
}

// ensureNamespace idempotently creates a namespace (the dry-run|apply trick,
// so re-runs are clean no-ops).
func ensureNamespace(ns string) error {
	manifest, err := output("kubectl", "create", "namespace", ns, "--dry-run=client", "-o", "yaml")
	if err != nil {
		return err
	}
	return pipeInto([]byte(manifest), "kubectl", "apply", "-f", "-")
}

// ensureSecretFromFiles idempotently applies a generic secret built from
// files, same dry-run|apply trick as ensureNamespace.
func ensureSecretFromFiles(ns, name string, files map[string]string) error {
	args := []string{"-n", ns, "create", "secret", "generic", name}
	for key, path := range files {
		args = append(args, "--from-file="+key+"="+path)
	}
	args = append(args, "--dry-run=client", "-o", "yaml")
	manifest, err := output("kubectl", args...)
	if err != nil {
		return err
	}
	return pipeInto([]byte(manifest), "kubectl", "apply", "-f", "-")
}

// ensureTLSSecret idempotently applies a kubernetes.io/tls secret from a cert
// and key file — the type the Gateway API's certificateRefs require, which
// ensureSecretFromFiles's generic secrets are not.
func ensureTLSSecret(ns, name, certPath, keyPath string) error {
	manifest, err := output("kubectl", "-n", ns, "create", "secret", "tls", name,
		"--cert="+certPath, "--key="+keyPath, "--dry-run=client", "-o", "yaml")
	if err != nil {
		return err
	}
	return pipeInto([]byte(manifest), "kubectl", "apply", "-f", "-")
}

// secretHasKey reports whether a secret exists and carries the given data key.
func secretHasKey(ns, name, key string) bool {
	out, err := outputQuiet("kubectl", "-n", ns, "get", "secret", name,
		"-o", "jsonpath={.data."+strings.ReplaceAll(key, ".", `\.`)+"}")
	return err == nil && strings.TrimSpace(out) != ""
}
