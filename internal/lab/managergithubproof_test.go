package lab

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agentlab/internal/config"
)

const (
	llmisvcCRDComponent            = "kserve-llmisvc-crd"
	llmisvcRuntimeConfigsComponent = "kserve-runtime-configs"
)

func TestManagerGitHubProofRendersIsolatedClients(t *testing.T) {
	for name, servingEnabled := range map[string]bool{"control plane": false, "full serving": true} {
		t.Run(name, func(t *testing.T) {
			stubWorkspacesProbe(t, true)
			configuration, providers, grant := commitLab(t)
			configuration.Platform.ManagerGitHubProof = true
			configuration.Platform.Devctl.Enabled = true
			configuration.Platform.Serving.Enabled = servingEnabled
			exchange := agentManagerCommitFor(configuration, providers, grant, nil)
			require.NotNil(t, exchange)
			postRenderers, err := componentPostRenderers(configuration, defaultDevImageNames(configuration), map[string][]dexLocalhostTarget{
				componentSkillManager: {{componentSkillManager, dexIssuerURLFlag}},
			}, nil, nil)
			require.NoError(t, err)
			values, _, _, _ := renderWorkspaceTemplates(t, configuration, func(data *tmplData) {
				data.PostRenderers = postRenderers
				data.WorkspaceProviders = providers
				data.WorkspaceGrant = grant
				data.AgentManagerCommit = exchange
				data.GitHubApp = true
			})
			broker := mapAt(values, "muster", "muster", "oauth", "server", "tokenExchangeBroker")
			require.Len(t, mapAt(broker, "targets"), 1)
			require.Len(t, mapAt(broker, "brokerClients"), 4)
			require.Len(t, mapAt(broker, "clientAudiences"), 4)
			versions := map[string]string{agentManagerMCPServer: "3.0.0-rc.10", clusterManager: "0.37.0-rc.1", modelManagerMCPServer: "1.20.0-rc.10", componentSkillManager: "0.2.0-rc.1"}
			for _, client := range exchange.BrokerClients() {
				t.Run(client.Name, func(t *testing.T) {
					require.Equal(t, versions[client.Name], mapAt(values, "components", client.Name)["versionRange"])
					require.Equal(t, true, mapAt(values, "components", client.Name)["enabled"])
					require.Equal(t, []any{config.WorkspaceProviderGitHub}, asSlice(mapAt(broker, "clientAudiences")[client.Name]))
					require.Equal(t, client.Secret, mapAt(broker, "brokerClients", client.Name, "clientCredentialsSecretRef")["name"])
					github := mapAt(values, client.Name, config.WorkspaceProviderGitHub)
					require.Equal(t, true, github["enabled"])
					require.Equal(t, config.WorkspaceProviderGitHub, mapAt(github, "tokenExchange")["audience"])
					require.Equal(t, client.Secret, mapAt(github, "tokenExchange")["existingSecret"])
					require.Nil(t, github["authorizationServer"])
				})
			}
			require.Equal(t, "5.63.2", mapAt(values, "components", "muster")["versionRange"])
			require.NotEmpty(t, mapAt(values, "components", componentSkillManager)["postRenderers"])
			for _, component := range []string{llmisvcCRDComponent, llmisvcResourcesComponent, llmisvcRuntimeConfigsComponent} {
				require.Equal(t, true, mapAt(values, "components", component)["enabled"])
			}
			require.Equal(t, gitHubAppSecret, mapAt(values, componentSkillManager, config.WorkspaceProviderGitHub, "app")["secretName"])
		})
	}
}

func TestManagerGitHubCredentialIsolationAndReuse(t *testing.T) {
	t.Chdir(t.TempDir())
	first, err := ensureManagerGitHubFiles(agentManagerMCPServer)
	require.NoError(t, err)
	second, err := ensureManagerGitHubFiles(clusterManager)
	require.NoError(t, err)
	secret, err := os.ReadFile(first[config.GitHubClientSecretKey])
	require.NoError(t, err)
	other, err := os.ReadFile(second[config.GitHubClientSecretKey])
	require.NoError(t, err)
	require.NotEqual(t, string(secret), string(other))
	reused, err := ensureManagerGitHubFiles(agentManagerMCPServer)
	require.NoError(t, err)
	kept, err := os.ReadFile(reused[config.GitHubClientSecretKey])
	require.NoError(t, err)
	require.Equal(t, string(secret), string(kept))
	for _, files := range []map[string]string{first, second} {
		for _, path := range files {
			info, err := os.Stat(filepath.Clean(path))
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		}
	}
}
