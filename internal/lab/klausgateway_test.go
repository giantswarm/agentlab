package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestKlausGatewayValues: the block's data — the in-cluster controller
// target, the proof's fixture as the default agent, the two lab Secrets, the
// public muster URL, and a callback base URL without a port whatever the edge
// port is (a Gateway API hostname carries none).
func TestKlausGatewayValues(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.GatewayPort = 8521
	v := klausGatewayValuesFor(cfg)
	if v.A2ATarget != "grpc://agentgateway.agent-platform.svc.cluster.local:8080" || v.AgentNamespace != kagentNamespace {
		t.Errorf("a2a target %q namespace %q", v.A2ATarget, v.AgentNamespace)
	}
	if v.DefaultAgent != klausGatewayTestAgent {
		t.Errorf("default agent %q, want the proof's fixture %s", v.DefaultAgent, klausGatewayTestAgent)
	}
	if v.SlackSecret != klausGatewaySlackPlaceholder || v.KeysSecret != klausGatewayOBOKeys {
		t.Errorf("secrets %q / %q", v.SlackSecret, v.KeysSecret)
	}
	if v.MusterURL != "https://muster.127.0.0.1.nip.io:8521" {
		t.Errorf("muster URL %q", v.MusterURL)
	}
	if v.CallbackBaseURL != "https://agentgateway.127.0.0.1.nip.io" {
		t.Errorf("callback base URL %q, want the hostname without the edge port", v.CallbackBaseURL)
	}
}

// TestKlausGatewayHint: the summary says how to turn it on, and names the
// target, the link Secret and a dev image when on.
func TestKlausGatewayHint(t *testing.T) {
	cfg := config.Default()
	if got := klausGatewayHint(cfg); !strings.Contains(got, "not wired") || !strings.Contains(got, "--klaus-gateway") {
		t.Fatalf("off: %q", got)
	}
	cfg.Platform.KlausGateway.Enabled = true
	cfg.Platform.DevImages = map[string]string{klausGatewayComponent: "klaus-gateway:dev"}
	got := klausGatewayHint(cfg)
	for _, want := range []string{"klaus-gateway:dev", klausGatewayInClusterTarget, klausGatewayLinksSecret, "klaus-gateway-test"} {
		if !strings.Contains(got, want) {
			t.Fatalf("hint lacks %q: %q", want, got)
		}
	}
}

// TestKlausGatewayLogTarget: `agentlab logs klaus-gateway` tails the
// component's Deployment in the platform namespace.
func TestKlausGatewayLogTarget(t *testing.T) {
	target, ok := logTargets[klausGatewayComponent]
	if !ok || target.namespace != platformNamespace || target.target != "deploy/klaus-gateway" {
		t.Fatalf("log target %+v (%v)", target, ok)
	}
}
