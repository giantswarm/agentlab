//go:build unix

package tui

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup gives the child its own process group and wires
// cmd.Cancel to signal the whole group.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Group gone or signal denied: fall back to the direct child.
		// Process.Kill reports os.ErrProcessDone if it already exited.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
