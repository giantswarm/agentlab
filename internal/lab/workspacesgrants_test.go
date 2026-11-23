package lab

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agentlab/internal/config"
)

const (
	grantIssuerKey  = "issuer"
	grantAuthURLKey = "authUrl"
)

func TestWorkspaceProofPreservesHeldGrants(t *testing.T) {
	for _, provider := range []string{config.WorkspaceProviderGitHub, config.WorkspaceProviderGitLab} {
		t.Run(provider, func(t *testing.T) {
			configuration := grantProofConfiguration(t, provider)
			var dropped atomic.Int32
			person := grantProofPerson(t, provider, "", &dropped)
			if provider == config.WorkspaceProviderGitHub {
				require.NoError(t, grantProofProvider(configuration, person, provider, time.Second))
			} else {
				_, err := connectWorkspaceProvider(configuration, person, provider, nil)
				require.NoError(t, err)
			}
			require.Empty(t, runWorkspaceGrants.release())
			require.Zero(t, dropped.Load())
		})
	}
}

func TestWorkspaceProofReleasesCreatedGrant(t *testing.T) {
	configuration := grantProofConfiguration(t, config.WorkspaceProviderGitHub)
	var dropped atomic.Int32
	person := grantProofPerson(t, config.WorkspaceProviderGitHub, "https://signin.example.invalid/start", &dropped)
	require.NoError(t, grantProofProvider(configuration, person, config.WorkspaceProviderGitHub, time.Second))
	require.Equal(t, []string{person.user.Email + "/" + config.WorkspaceProviderGitHub}, runWorkspaceGrants.release())
	require.Equal(t, int32(1), dropped.Load())
}

func grantProofConfiguration(t *testing.T, provider string) *config.Config {
	t.Helper()
	t.Chdir(t.TempDir())
	server := customObject(musterMCPServerGVK, platformNamespace, workspaceManagerMCPServer, nil)
	server.Object["spec"] = map[string]any{"auth": map[string]any{"authorizationServer": map[string]any{grantIssuerKey: "https://issuer.example.invalid"}}}
	newFakeLab(t, server)
	previous := runWorkspaceGrants
	runWorkspaceGrants = &workspaceGrantLedger{}
	t.Cleanup(func() { runWorkspaceGrants = previous })
	configuration := config.Default()
	configuration.Platform.Workspaces.Enabled = true
	configuration.Platform.Workspaces.Provider = provider
	configuration.Platform.Workspaces.GrantProvider = provider
	return configuration
}

func grantProofPerson(t *testing.T, provider, link string, dropped *atomic.Int32) *workspacePerson {
	t.Helper()
	session := scriptedMuster(t, func(tool string, _ int) mcpAnswer {
		switch tool {
		case "core_auth_login":
			if link != "" {
				return callToolAnswer(t, toolResult("Sign in", false, map[string]any{grantAuthURLKey: link}))
			}
			return callToolAnswer(t, toolResult("Already connected", false, nil))
		case workspaceManagerToolPrefix + "list_providers":
			return callToolAnswer(t, toolResult("Connected", false, map[string]any{"providers": []map[string]any{{nameKey: provider, "connected": true}}}))
		case "core_auth_logout":
			dropped.Add(1)
			return callToolAnswer(t, toolResult("Disconnected", false, nil))
		default:
			t.Errorf("unexpected MCP tool %s", tool)
			return mcpAnswer{status: http.StatusBadRequest}
		}
	})
	return &workspacePerson{user: &config.User{Email: "proof@lab.invalid"}, session: session}
}
