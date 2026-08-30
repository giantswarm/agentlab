//go:build windows

package tui

import "os/exec"

// configureProcessGroup is a no-op on Windows: there are no Unix process
// groups, so cancel falls back to exec's default kill of the direct child,
// with WaitDelay unblocking the pipes if grandchildren linger.
func configureProcessGroup(cmd *exec.Cmd) {}
