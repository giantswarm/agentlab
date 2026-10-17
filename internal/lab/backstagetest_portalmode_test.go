package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// indexHTML is a portal index.html with the given injected frontend config.
func indexHTML(configs string) []byte {
	return []byte(`<html><head><script type="backstage.io/config">` + configs + `</script></head><body></body></html>`)
}

// TestServedAgentPlatform: the last injected config that sets
// app.portals.agentPlatform decides, a boolean or the string an env override
// leaves; a Backstage that injects none serves the classic portal.
func TestServedAgentPlatform(t *testing.T) {
	for _, tc := range []struct {
		name    string
		configs string
		want    bool
	}{
		{"2.x: no app.portals", `[{"context":"app-config.yaml","data":{"app":{"title":"Portal"}}}]`, false},
		{"shell on", `[{"data":{"app":{"portals":{"agentPlatform":true}}}}]`, true},
		{"env string", `[{"data":{"app":{}}},{"data":{"app":{"portals":{"agentPlatform":"true"}}}}]`, true},
		{"last one wins", `[{"data":{"app":{"portals":{"agentPlatform":true}}}},{"data":{"app":{"portals":{"agentPlatform":false}}}}]`, false},
		{"unset later keeps it", `[{"data":{"app":{"portals":{"agentPlatform":true}}}},{"data":{"app":{"title":"x"}}}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := servedAgentPlatform(indexHTML(tc.configs))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("served shell = %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := servedAgentPlatform([]byte("<html></html>")); err == nil {
		t.Error("an index.html without injected config passed")
	}
}

// TestCheckServedPortal: a match passes; a lab configured for the shell whose
// Backstage serves the classic portal fails naming the cause and the image,
// never quietly.
func TestCheckServedPortal(t *testing.T) {
	const image = "gsoci.azurecr.io/giantswarm/backstage:2.97.0"
	if err := checkServedPortal(config.PortalAgentPlatform, true, image); err != nil {
		t.Errorf("shell served as configured: %v", err)
	}
	if err := checkServedPortal(config.PortalClassic, false, image); err != nil {
		t.Errorf("classic served as configured: %v", err)
	}
	if err := checkServedPortal("", true, image); err != nil {
		t.Errorf("the default mode is the shell: %v", err)
	}
	err := checkServedPortal(config.PortalAgentPlatform, false, image)
	if err == nil {
		t.Fatal("a classic-only Backstage on a shell lab passed")
	}
	for _, want := range []string{"app.portals.agentPlatform", image, "before 3.x", config.DefaultChartVersion} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if err := checkServedPortal(config.PortalClassic, true, image); err == nil {
		t.Error("the shell on a classic lab passed")
	}
}
