package machineid

import "syscall"

// readMachineID returns macOS's hardware UUID. `kern.uuid` is derived from
// the hardware (a name-based UUID, not a random one) and survives reboots and
// upgrades; the per-boot value macOS keeps is a different sysctl,
// `kern.bootsessionuuid`, so there is no risk of reading the wrong one.
func readMachineID() (string, error) {
	return syscall.Sysctl("kern.uuid")
}
