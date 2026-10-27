package lab

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// devctl's MCP server (`devctl serve`, platform.devctl) runs in the lab the
// way gazelle runs it: devctl's own released chart, never the meta chart's,
// in the platform namespace, where muster watches the MCPServer devctl the
// chart renders (tools x_devctl_<tool>). The server acts on GitHub with the
// person's token of the workspaces' github instance App: muster keeps that
// App as the provider App github beside the workspace-manager's pin (the
// values template adds it to the registration's providerApps, the shape
// gazelle's configuration declares) and attaches the person's grant of it to
// every devctl call. No credential of the server's own, no CircleCI token:
// the waits read CircleCI through GitHub's commit statuses. `agentlab
// coding-test --part devctl` proves the chain (codingdevctl.go).
const (
	devctlChart     = "oci://gsoci.azurecr.io/charts/giantswarm/devctl"
	devctlRelease   = "devctl"
	devctlMCPServer = "devctl"
	// devctlToolPrefix starts every tool the server's MCPServer serves
	// through muster.
	devctlToolPrefix = "x_devctl_"
	// devctlGrantApp is the provider App whose grant muster attaches to a
	// devctl call: the github instance's, whose name it keeps.
	devctlGrantApp = config.WorkspaceProviderGitHub
)

// devctlValuesYAML are the chart's values in the lab: the MCPServer
// registered and the grant of the github instance's App; no installation for
// rollout_wait (the lab's cluster is none of the fleet's).
const devctlValuesYAML = `github:
  grantApp: ` + devctlGrantApp + `
muster:
  mcpServer:
    enabled: true
`

// devctlValues are devctlValuesYAML as Helm takes them.
func devctlValues() (map[string]any, error) {
	var values map[string]any
	if err := yaml.Unmarshal([]byte(devctlValuesYAML), &values); err != nil {
		return nil, fmt.Errorf("devctl's lab values: %w", err)
	}
	return values, nil
}

// ensureDevctl installs devctl's chart while platform.devctl is on and
// uninstalls it while it is off. After the platform install: the MCPServer
// CRD ships with muster.
func ensureDevctl(cfg *config.Config) error {
	if !cfg.DevctlEnabled() {
		if !helmReleaseExists(platformNamespace, devctlRelease) {
			return nil
		}
		step("Uninstalling devctl's MCP server (platform.devctl is off)")
		return helmUninstall(platformNamespace, devctlRelease, true, ociChartInstallTimeout)
	}
	step("Installing devctl %s's MCP server (MCPServer %s, the grant of the %s instance's App)", cfg.DevctlVersion(), devctlMCPServer, devctlGrantApp)
	values, err := devctlValues()
	if err != nil {
		return err
	}
	return installOCIChart(cfg, platformNamespace, devctlRelease, devctlChart, cfg.DevctlVersion(), values, ociChartInstallTimeout)
}

// devctlHint is the platform-up summary line of the switch.
func devctlHint(cfg *config.Config) string {
	if !cfg.DevctlEnabled() {
		return "  devctl's MCP server is off (platform.devctl in agentlab.yaml; `agentlab configure --devctl` turns it on)."
	}
	return fmt.Sprintf("  devctl %s's MCP server: %s* through muster as the person, on their grant of the %s instance's App.\n"+
		"  Proof: `agentlab coding-test --part devctl`.", cfg.DevctlVersion(), devctlToolPrefix, devctlGrantApp)
}
