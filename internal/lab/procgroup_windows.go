//go:build windows

package lab

import "os/exec"

// ownProcessGroup leaves cmd as it is: Windows has no process groups to
// signal.
func ownProcessGroup(*exec.Cmd) {}

// signalGroup kills cmd itself, its children left as Windows leaves them.
func signalGroup(cmd *exec.Cmd, _ bool) { _ = cmd.Process.Kill() }
