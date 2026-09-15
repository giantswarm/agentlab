//go:build !darwin && !linux && !windows

package machineid

import (
	"fmt"
	"runtime"
)

// readMachineID has no source to read on this OS. The caller falls back.
func readMachineID() (string, error) {
	return "", fmt.Errorf("no machine identifier source on %s", runtime.GOOS)
}
