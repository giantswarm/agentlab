package machineid

import "golang.org/x/sys/windows/registry"

// readMachineID returns MachineGuid, which Windows writes once when it is
// installed. WOW64_64KEY reads the 64-bit view of the registry, so a 32-bit
// agentlab build sees the same value as a 64-bit one rather than its own
// redirected copy.
func readMachineID() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", err
	}
	defer func() { _ = k.Close() }()

	id, _, err := k.GetStringValue("MachineGuid")
	return id, err
}
