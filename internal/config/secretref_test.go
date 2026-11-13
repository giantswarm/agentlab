package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKubernetesSecretSource(t *testing.T) {
	for name, reference := range map[string]string{
		"kind lab":       "k8s://kind-agentlab/agent-platform/workspace-github/private-key",
		"other context":  "k8s://user@cluster.example.com/team/auth/client-secret",
		"document field": "k8s://kind-agentlab/agent-platform/provider/config.yaml#credentials.secret",
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, ValidateSecretSource(reference))
			configuration := Default()
			configuration.Platform.Workspaces.GitHub = WorkspaceGitHub{
				AppID: "5288072", ClientID: "client-id", PrivateKeySource: reference,
			}
			configuration.GitHubApp.InstallationID = "171259128"
			require.NoError(t, configuration.Validate())
		})
	}
	for name, reference := range map[string]string{
		"missing key":       "k8s://kind-agentlab/agent-platform/workspace-github",
		"empty context":     "k8s:///agent-platform/workspace-github/private-key",
		"invalid namespace": "k8s://kind-agentlab/AgentPlatform/workspace-github/private-key",
		"invalid name":      "k8s://kind-agentlab/agent-platform/workspace_github/private-key",
		"invalid key":       "k8s://kind-agentlab/agent-platform/workspace-github/private?key",
		"empty path":        "k8s://kind-agentlab/agent-platform/workspace-github/private-key#",
		"whitespace":        "k8s://kind-agentlab/agent-platform/workspace-github/private key",
		"extra segment":     "k8s://kind-agentlab/agent-platform/workspace-github/private/key",
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, ValidateSecretSource(reference))
		})
	}
}
