package machineid

import "syscall"

// readMachineID returns macOS's hardware UUID. `kern.uuid` is a name-based
// UUID derived from the hardware, so it survives reboots, upgrades and a full
// reinstall — it identifies the computer for its life, which is longer than
// the Linux and Windows values last. The per-boot value macOS keeps is a
// different sysctl, `kern.bootsessionuuid`, so there is no risk of reading
// the wrong one.
func readMachineID() (string, error) {
	return syscall.Sysctl("kern.uuid")
}
