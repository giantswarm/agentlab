package machineid

// readMachineID returns the systemd machine ID, falling back to the D-Bus one
// that predates it and that non-systemd distributions still write. Both hold
// 32 bare hex digits.
func readMachineID() (string, error) {
	return firstFile("/etc/machine-id", "/var/lib/dbus/machine-id")
}
