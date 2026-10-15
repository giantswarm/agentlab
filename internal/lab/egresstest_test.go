package lab

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

var testEgressTarget = egressProbeTarget{
	origin: DefaultEgressOrigin, undeclared: egressTestUndeclaredURL, muster: "http://muster.agent-platform.svc:8090/mcp",
	tcpHost: egressTestTCPHost, tcpAddress: "140.82.121.4", tcpPort: egressTestTCPPort,
}

// TestEgressProbeCommandParses: the probe is valid bash naming every
// destination.
func TestEgressProbeCommandParses(t *testing.T) {
	cmd := egressProbeCommand(testEgressTarget)
	if out, err := exec.Command("bash", "-n", "-c", cmd).CombinedOutput(); err != nil { // #nosec G204 -- bash -n parses, runs nothing
		t.Fatalf("bash -n: %v: %s\n%s", err, out, cmd)
	}
	for _, want := range []string{DefaultEgressOrigin + "/", egressTestUndeclaredURL, testEgressTarget.muster, "github.com 22", "140.82.121.4 22"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the probe does not reach %q:\n%s", want, cmd)
		}
	}
}

// TestEgressVerdicts: each stage passes on the readings of a gated actor and
// fails, naming the destination, on the readings of an open one.
func TestEgressVerdicts(t *testing.T) {
	gated := parseCodingProbe(strings.Join([]string{
		probeMarker + " declared=rc0 status=200   HTTP/1.1 200 OK",
		probeMarker + " undeclared=rc1 status=403 wget: server returned error: HTTP/1.1 403 Forbidden",
		probeMarker + " muster=rc1 status=401 wget: server returned error: HTTP/1.1 401 Unauthorized",
		probeMarker + " tcp-name=rc1 bash: connect: Connection refused",
		probeMarker + " tcp-address=rc124 ",
	}, "\n"))
	open := parseCodingProbe(strings.Join([]string{
		probeMarker + " declared=rc1 status=403 wget: server returned error: HTTP/1.1 403 Forbidden",
		probeMarker + " undeclared=rc0 status=200",
		probeMarker + " muster=rc1 status=403 wget: server returned error: HTTP/1.1 403 Forbidden",
		probeMarker + " tcp-name=rc0 SSH-2.0-babeld-2a1b3c4d",
		probeMarker + " tcp-address=rc0 SSH-2.0-babeld-2a1b3c4d",
	}, "\n"))
	for _, c := range []struct {
		name    string
		verdict func(codingProbe, egressProbeTarget) (string, error)
		names   string
	}{
		{"declared", codingProbe.declared, DefaultEgressOrigin},
		{"undeclared", codingProbe.undeclared, egressTestUndeclaredURL},
		{"muster answer", codingProbe.muster, testEgressTarget.muster},
		{"raw tcp", codingProbe.rawTCP, "github.com:22"},
	} {
		if v, err := c.verdict(gated, testEgressTarget); err != nil {
			t.Errorf("%s on a gated actor: %v", c.name, err)
		} else if !strings.Contains(v, c.names) {
			t.Errorf("%s verdict %q does not name %s", c.name, v, c.names)
		}
		if v, err := c.verdict(open, testEgressTarget); err == nil {
			t.Errorf("%s on an open actor passed: %s", c.name, v)
		} else if !strings.Contains(err.Error(), c.names) {
			t.Errorf("%s failure %q does not name %s", c.name, err, c.names)
		}
		if _, err := c.verdict(codingProbe{}, testEgressTarget); err == nil {
			t.Errorf("%s passed on a reply without the probe's lines", c.name)
		}
	}
}

// TestEgressRawTCPByAddress: a banner by address alone fails too — a name
// the actor's resolver refuses must not pass for the gate's refusal.
func TestEgressRawTCPByAddress(t *testing.T) {
	p := codingProbe{"tcp-name": "rc1 bash: github.com: Name or service not known", "tcp-address": "rc0 SSH-2.0-babeld"}
	if _, err := p.rawTCP(testEgressTarget); err == nil || !strings.Contains(err.Error(), "140.82.121.4:22") {
		t.Errorf("a banner by address: %v", err)
	}
}

// TestDenialVerdict: the undeclared host's refusal must carry the policy's
// reason; another reason or none at all fails.
func TestDenialVerdict(t *testing.T) {
	denied := egressRefusal{method: "CONNECT", host: "proxy.golang.org:443", status: "403", reason: egressPolicyDenied, count: 1}
	if v, err := denialVerdict("proxy.golang.org", []egressRefusal{denied}); err != nil || !strings.Contains(v, egressPolicyDenied) {
		t.Errorf("the policy's denial: %q, %v", v, err)
	}
	other := denied
	other.reason = "actor identity check failed"
	if _, err := denialVerdict("proxy.golang.org", []egressRefusal{other}); err == nil || !strings.Contains(err.Error(), "another reason") {
		t.Errorf("another reason: %v", err)
	}
	if _, err := denialVerdict("proxy.golang.org", nil); err == nil || !strings.Contains(err.Error(), "no refusal") {
		t.Errorf("no refusal: %v", err)
	}
}

// TestCreateAgentArgsEgress: the spec's egress is create_agent's egress
// argument and the chart's agent.egress; none declared composes neither.
func TestCreateAgentArgsEgress(t *testing.T) {
	spec := agentSpec{Name: testAgentName, ModelConfig: defaultModelConfig, Egress: []string{DefaultEgressOrigin}}
	args, err := createAgentArgs(spec)
	if err != nil || !reflect.DeepEqual(args[egressKey], spec.Egress) {
		t.Errorf("create_agent egress: %v, %v", args, err)
	}
	agent, _ := agentValues(spec)["agent"].(map[string]any)
	if !reflect.DeepEqual(agent[egressKey], spec.Egress) {
		t.Errorf("agent.egress: %v", agent)
	}
	spec.Egress = nil
	args, _ = createAgentArgs(spec)
	agent, _ = agentValues(spec)["agent"].(map[string]any)
	if _, ok := args[egressKey]; ok {
		t.Errorf("no egress composes %v", args)
	}
	if _, ok := agent[egressKey]; ok {
		t.Errorf("no egress renders %v", agent)
	}
}

// TestDenialVerdictReadsTheTLSDestination: a refusal at the TLS handshake
// names its destination as tls.sni alone, as the gateway logs it.
func TestDenialVerdictReadsTheTLSDestination(t *testing.T) {
	logs := "2026-10-15T20:29:29.356210Z\terror\trequest request.id=9 gateway=default/default listener=listener0 http.method=GET http.path=/ http.version=HTTP/1.1 http.status=403 tls.sni=proxy.golang.org protocol=http ate.atespace=kagent error=\"actor egress policy denied TLS destination\" reason=Authorization duration=0ms\n"
	v, err := denialVerdict("proxy.golang.org", egressRefusals(logs))
	if err != nil || !strings.Contains(v, "403 GET proxy.golang.org/") {
		t.Errorf("denialVerdict = %q, %v", v, err)
	}
}
