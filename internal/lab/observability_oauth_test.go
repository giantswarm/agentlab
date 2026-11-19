package lab

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

func TestMCPPrometheusOAuthValues(t *testing.T) {
	manifest, err := renderTemplate(config.Default(), mcpPrometheusTemplate, nil)
	require.NoError(t, err)
	releases, err := fluxReleases(string(manifest))
	require.NoError(t, err)
	require.Len(t, releases, 1)

	var values map[string]any
	require.NoError(t, yaml.Unmarshal(releases[0].Values, &values))
	oauth := mapAt(values, "app", "oauth")
	require.Equal(t, true, oauth["enabled"])
	require.Equal(t, "dex", oauth["provider"])
	require.Equal(t, true, oauth["allowPrivateIPJWKS"])
	require.NotContains(t, oauth, "allowPrivateURLs")
	require.Equal(t, []any{config.AgentPlatformClientID}, oauth["trustedAudiences"])
	require.Equal(t, map[string]any{nameKey: "dex-ca", "key": filepath.Base(caCertPath)}, mapAt(oauth, "dexCASecret"))
}
