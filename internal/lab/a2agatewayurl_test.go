package lab

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// The meta chart derives the agent cards' address as https://agentgateway.<domain>,
// without a port. A lab whose edge is not on 443 names its port, and a lab on
// 443 leaves the chart's own derivation in place.
func TestPlatformValuesA2AGatewayURL(t *testing.T) {
	for _, test := range []struct {
		port int
		want string
	}{
		{port: 8443, want: "https://agentgateway.lab.example:8443"},
		{port: 443, want: ""},
	} {
		t.Run(fmt.Sprint(test.port), func(t *testing.T) {
			cfg := config.Default()
			cfg.Platform.Domain = "lab.example"
			cfg.Platform.GatewayPort = test.port
			raw, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl", nil)
			require.NoError(t, err)
			var values struct {
				Kagent struct {
					Controller map[string]any `yaml:"controller"`
				} `yaml:"kagent"`
			}
			require.NoError(t, yaml.Unmarshal(raw, &values))
			require.NotNil(t, values.Kagent.Controller, "kagent.controller is rendered")
			got, _ := values.Kagent.Controller["a2aGatewayUrl"].(string)
			require.Equal(t, test.want, got)
		})
	}
}
