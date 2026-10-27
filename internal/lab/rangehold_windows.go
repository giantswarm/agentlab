//go:build windows

package lab

import "os"

// pidAlive keeps every holder on Windows, which has no signal-0 probe: a
// holder is pruned only where its liveness can be told.
func pidAlive(int) bool { return true }

// resignal ends the process the way an unhandled interrupt would.
func resignal(os.Signal) { os.Exit(1) }
