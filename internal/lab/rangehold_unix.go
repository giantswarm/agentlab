//go:build !windows

package lab

import (
	"errors"
	"os"
	"syscall"
)

// pidAlive reports whether a process of this host runs under pid (one of
// another user's counts).
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// resignal delivers sig to this process again once its handler is stopped,
// so the signal takes its default course.
func resignal(sig os.Signal) {
	if s, ok := sig.(syscall.Signal); ok {
		_ = syscall.Kill(os.Getpid(), s)
	}
}
