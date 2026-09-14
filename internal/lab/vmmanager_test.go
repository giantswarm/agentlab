package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestVMManagerTemplate pins the registration to what the proof and the
// tool-group check look for: the name muster prefixes the tools with, the
// forward-token auth block (vm-manager validates the person's token itself),
// the agent-platform tool group the chart stamps on the platform's own
// management surface, and the host-service marker that tells the tool-group
// proof this labelled server is the lab's registration, not a fixture.
func TestVMManagerTemplate(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.VMManager.Enabled = true
	raw, err := renderTemplate(cfg, vmManagerTemplate, func(d *tmplData) { d.VMManagerURL = vmManagerURL("http://172.21.0.1:8100") })
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)
	for _, want := range []string{
		"kind: MCPServer",
		"name: " + vmManagerMCPServer,
		"namespace: " + platformNamespace,
		"url: http://172.21.0.1:8100" + vmManagerMCPPath,
		"type: streamable-http",
		"type: oauth",
		"forwardToken: true",
		"autoStart: true",
		managedByLabel + ": " + managedByAgentlabValue,
		vmManagerHostServiceLabel + ": " + vmManagerMCPServer,
		toolGroupLabel + ": " + toolGroupAgentPlatform,
		"agentlab.giantswarm.io/purpose:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered registration missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, fleetFixtureLabel) {
		t.Errorf("the registration must not read as a fixture:\n%s", out)
	}
	if got := strings.Count(out, "kind: MCPServer"); got != 1 {
		t.Errorf("want exactly one MCPServer, got %d:\n%s", got, out)
	}
}

// TestVMManagerEnv: the environment hands vm-manager exactly the lab's
// identity — the one issuer URL, the lab CA, the platform client and its
// audience as the trusted one — and a listen address pods can dial.
func TestVMManagerEnv(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.VMManager.Port = 8123
	out, err := vmManagerEnv(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"VM_MANAGER_LISTEN=0.0.0.0:8123\n",
		"VM_MANAGER_OAUTH_ENABLED=true\n",
		"VM_MANAGER_OAUTH_BASE_URL=http://localhost:8123\n",
		"VM_MANAGER_OAUTH_PROVIDER=dex\n",
		"DEX_ISSUER_URL=" + cfg.Issuer() + "\n",
		"DEX_CLIENT_ID=" + config.AgentPlatformClientID + "\n",
		"DEX_CLIENT_SECRET=" + config.AgentPlatformClientSecret + "\n",
		"DEX_CA_FILE=/",
		"/" + caCertPath + "\n",
		"VM_MANAGER_OAUTH_ALLOW_PRIVATE_URLS=true\n",
		"SSO_ALLOW_PRIVATE_IPS=true\n",
		"OAUTH_TRUSTED_AUDIENCES=" + config.AgentPlatformClientID + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("env missing %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "#") || strings.Contains(line, "=") {
			continue
		}
		t.Errorf("line %q is neither a comment nor KEY=value", line)
	}
}

// TestVMManagerIdent: a vm-manager is recognised by its build-info series,
// with the version it carries; every other exposition is not one.
func TestVMManagerIdent(t *testing.T) {
	cases := []struct {
		name, body, wantVersion string
		wantOK                  bool
	}{
		{"vm-manager", "# HELP vm_manager_build_info Build information.\n# TYPE vm_manager_build_info gauge\nvm_manager_build_info{commit=\"abc\",go_version=\"go1.26\",version=\"0.4.0\"} 1\n", "0.4.0", true},
		{"dev build", "vm_manager_build_info{version=\"dev-6f1665c\",commit=\"unknown\"} 1\n", "dev-6f1665c", true},
		{"no version label", "vm_manager_build_info{commit=\"abc\"} 1\n", "unknown version", true},
		{"another exporter", "# TYPE go_goroutines gauge\ngo_goroutines 12\nmodel_manager_build_info{version=\"1\"} 1\n", "", false},
		{"html", "<html><body>ok</body></html>", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			version, ok := vmManagerIdent([]byte(tc.body))
			if ok != tc.wantOK || version != tc.wantVersion {
				t.Fatalf("got (%q, %v), want (%q, %v)", version, ok, tc.wantVersion, tc.wantOK)
			}
		})
	}
}

// TestVMManagerEndpointOverride: an explicit endpoint is dialed as given
// (trailing slash dropped), with no detection.
func TestVMManagerEndpointOverride(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.VMManager.Endpoint = "http://host.docker.internal:8100/"
	got, err := resolveVMManagerEndpoint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://host.docker.internal:8100"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if u := vmManagerURL(got); u != "http://host.docker.internal:8100/mcp" {
		t.Fatalf("mcp url %q", u)
	}
}

// TestVMImageHasGolden reads the image policy the way the proof decides
// whether to require attestation.
func TestVMImageHasGolden(t *testing.T) {
	cases := []struct {
		name   string
		policy string
		want   bool
	}{
		{"fresh build", `{"pcr11":{"enter-initrd":"aa"},"pcr13":{}}`, false},
		{"golden recorded", `{"pcr11":{},"golden":{"sha256":{"0":"aa","7":"bb"}}}`, true},
		{"golden empty", `{"golden":{}}`, false},
		{"no policy", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img := vmImage{ID: "giantswarm-vm-base", Version: "0.1.0"}
			if tc.policy != "" {
				img.Policy = []byte(tc.policy)
			}
			if got := img.hasGolden(); got != tc.want {
				t.Fatalf("hasGolden = %v, want %v", got, tc.want)
			}
			if img.ref() != "giantswarm-vm-base_0.1.0" {
				t.Fatalf("ref %q", img.ref())
			}
		})
	}
}

// TestVMManagerFound: only an explicit "pods cannot reach it" leaves the
// wiring off; unknown reachability enrolls, like the model servers.
func TestVMManagerFound(t *testing.T) {
	cases := []struct {
		name string
		vm   *HostVMManager
		want bool
	}{
		{"none", nil, false},
		{"not probed", &HostVMManager{Version: "1"}, true},
		{"on the gateway", &HostVMManager{Version: "1", Probed: true, PodHost: gatewayIP}, true},
		{"on the alias", &HostVMManager{Version: "1", Probed: true, PodHost: hostDockerInternal}, true},
		{"unreachable", &HostVMManager{Version: "1", Probed: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Discovery{VMManager: tc.vm}
			if got := d.VMManagerFound(); got != tc.want {
				t.Fatalf("VMManagerFound = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStripANSI: the console tail reads as text — colours, cursor moves and
// the terminal's title/prompt marks removed, the words kept.
func TestStripANSI(t *testing.T) {
	in := "[\x1b[0;32m  OK  \x1b[0m] Reached target \x1b[0;1;39mMulti-User System\x1b[0m.\x1b]3008;start=abc;user=root\x07 done \x1b[!p\x1b[?7h"
	if got, want := stripANSI(in), "[  OK  ] Reached target Multi-User System. done "; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
