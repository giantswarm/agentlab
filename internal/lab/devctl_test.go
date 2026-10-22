package lab

import "testing"

// devctl's lab values register the MCPServer on the github instance's grant.
func TestDevctlValues(t *testing.T) {
	values, err := devctlValues()
	if err != nil {
		t.Fatal(err)
	}
	github, _ := values["github"].(map[string]any)
	muster, _ := values["muster"].(map[string]any)
	server, _ := muster["mcpServer"].(map[string]any)
	if github["grantApp"] != devctlGrantApp || server["enabled"] != true {
		t.Errorf("values %v, want the MCPServer on with grantApp %s", values, devctlGrantApp)
	}
}
