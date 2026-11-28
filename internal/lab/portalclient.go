package lab

import "github.com/giantswarm/agentlab/internal/config"

// valuesBackstage and valuesOIDCClientID are the meta chart's key of the
// portal's own OAuth client, backstage.oidcClientId.
const (
	valuesBackstage    = "backstage"
	valuesOIDCClientID = "oidcClientId"
)

// portalClientChartCarries reports whether the configured meta chart takes
// backstage.oidcClientId, so the portal signs in with its own Dex client
// (config.PortalClientID) instead of the shared agent-platform one. An older
// chart forwards an unknown backstage key to the backstage chart, whose
// schema refuses it, so the lab sets the key only where the chart carries
// it. A chart that cannot be read is a note and the shared client.
func portalClientChartCarries(cfg *config.Config) bool {
	if !cfg.Platform.Enabled || !cfg.Backstage.Enabled {
		return false
	}
	chart := platformChartFor(cfg)
	v, err := platformChartValuesOf(chart)
	if err != nil {
		note("the platform chart %s could not be read for the portal's own client (%v): the portal signs in with %s", chart, err, config.AgentPlatformClientID)
		return false
	}
	return v.carries(valuesBackstage, valuesOIDCClientID)
}
