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

// run executes a command with output streamed to the terminal.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...) // #nosec G204 -- fixed lab tooling (kind/kubectl/helm) with lab-controlled args
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
	cmd := exec.Command(name, args...) // #nosec G204 -- fixed lab tooling (kind/kubectl/helm) with lab-controlled args
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		_, _ = os.Stderr.Write(buf.Bytes())
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// output captures a command's stdout (stderr goes to the terminal).
func output(name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(name, args...) // #nosec G204 -- fixed lab tooling (kind/kubectl/helm) with lab-controlled args
	cmd.Stdout = &buf
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return buf.String(), err
}

// outputQuiet captures stdout and swallows stderr; for probe-style commands
// whose failures are expected.
func outputQuiet(name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(name, args...) // #nosec G204 -- fixed lab tooling (kind/kubectl/helm) with lab-controlled args
	cmd.Stdout = &buf
	err := cmd.Run()
	return buf.String(), err
}

// pipeInto feeds input to a command's stdin, showing output only on failure.
func pipeInto(input []byte, name string, args ...string) error {
	var buf bytes.Buffer
	cmd := exec.Command(name, args...) // #nosec G204 -- fixed lab tooling (kind/kubectl/helm) with lab-controlled args
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		_, _ = os.Stderr.Write(buf.Bytes())
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
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
