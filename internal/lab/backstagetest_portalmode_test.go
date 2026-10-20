package lab

import (
	"net/http"
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
	for _, want := range []string{"app.portals.agentPlatform", image, "Backstage 2.97.0", "Backstage 3.x", config.DefaultChartVersion} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if err := checkServedPortal(config.PortalClassic, true, image); err == nil {
		t.Error("the shell on a classic lab passed")
	}
}

// TestBackstageVersion: the version a refusal names is the image's tag, the
// digest dropped; an image without one is named whole.
func TestBackstageVersion(t *testing.T) {
	for image, want := range map[string]string{
		"gsoci.azurecr.io/giantswarm/backstage:2.97.0":                "2.97.0",
		"gsoci.azurecr.io/giantswarm/backstage:3.4.1@sha256:0123abcd": "3.4.1",
		"localhost:5001/backstage:dev-abc":                            "dev-abc",
		"localhost:5001/backstage@sha256:0123abcd":                    "localhost:5001/backstage@sha256:0123abcd",
		"": unknownImage,
	} {
		if got := backstageVersion(image); got != want {
			t.Errorf("backstageVersion(%q) = %q, want %q", image, got, want)
		}
	}
}

// TestEveryPlatformRunChecksThePortalMode: `up` and `platform` both refuse a
// lab whose Backstage cannot serve backstage.portal, once the portal runs —
// a shell lab on a classic-only Backstage never reads as up, so no shell
// spec starts against it.
func TestEveryPlatformRunChecksThePortalMode(t *testing.T) {
	calls := callSites(t)
	if calls["checkPortalMode"]["platformUp"] != 1 {
		t.Errorf("checkPortalMode is called from %v, want once from platformUp itself", calls["checkPortalMode"])
	}
}

// The portal's HSTS header fails the check; a response without it passes.
func TestCheckNoHSTS(t *testing.T) {
	if err := checkNoHSTS(http.Header{}); err != nil {
		t.Errorf("no header: %v", err)
	}
	h := http.Header{}
	h.Set(hstsHeader, "max-age=15552000; includeSubDomains")
	if err := checkNoHSTS(h); err == nil || !strings.Contains(err.Error(), "max-age=15552000") {
		t.Errorf("HSTS header: got %v, want the header named", err)
	}
}
