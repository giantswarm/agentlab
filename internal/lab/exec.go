package lab

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// step prints a top-level progress line, matching the ==> style the shell
// scripts used.
func step(format string, a ...any) {
	fmt.Printf("==> "+format+"\n", a...)
}

func note(format string, a ...any) {
	fmt.Printf("    "+format+"\n", a...)
}

// run executes a command with output streamed to the terminal.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runQuiet executes a command, showing output only if it fails.
func runQuiet(name string, args ...string) error {
	var buf bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		os.Stderr.Write(buf.Bytes())
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// output captures a command's stdout (stderr goes to the terminal).
func output(name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = &buf
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return buf.String(), err
}

// outputQuiet captures stdout and swallows stderr; for probe-style commands
// whose failures are expected.
func outputQuiet(name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = &buf
	err := cmd.Run()
	return buf.String(), err
}

// pipeInto feeds input to a command's stdin, showing output only on failure.
func pipeInto(input []byte, name string, args ...string) error {
	var buf bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		os.Stderr.Write(buf.Bytes())
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
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
