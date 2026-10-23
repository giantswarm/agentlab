//go:build !windows

package lab

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a process group of its own.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends cmd's process group SIGTERM, or SIGKILL with kill.
func signalGroup(cmd *exec.Cmd, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}
