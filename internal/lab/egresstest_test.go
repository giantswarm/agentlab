package lab

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

var testEgressTarget = egressProbeTarget{
	origin: DefaultEgressOrigin, undeclared: egressTestUndeclaredURL, muster: "http://muster.agent-platform.svc.cluster.local:8090/mcp",
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

// The egress gateway's log of a gated probe turn, as agentlab-1 recorded it
// (fields trimmed): the declared origin forwarded, the undeclared host refused
// at the TLS handshake, the probe's GET to muster forwarded beside the
// runtime's own MCP POST, the raw TCP tunnels to SSH held with no upstream
// connection, another actor's request, and an upstream connection of the
// actor's model call.
const (
	egressTestActor = "session-01a15311-dacc-7efd-8df2-57002d4166f0"
	gatedEgressLog  = "2026-10-19T07:30:42.032788Z\tinfo\trequest request.id=4 listener=listener2 endpoint=muster.agent-platform.svc.cluster.local:8090 http.method=POST http.host=muster.agent-platform.svc.cluster.local http.path=/mcp http.status=200 protocol=http ate.actor.name=" + egressTestActor + " ate.atespace=kagent substrate.connect.authority=\"10.96.129.58:8090\"\n" +
		"2026-10-19T07:30:46.574322Z\tdebug\tupstream tcp\tconnected\tendpoint=160.79.104.10:443 transport=tls connect_ms=12\n" +
		"2026-10-19T07:30:47.047018Z\tinfo\trequest request.id=8 listener=listener0 endpoint=example.com:443 src.addr=10.244.0.38:33456 http.method=GET http.host=example.com http.path=/ http.status=200 protocol=http ate.actor.name=" + egressTestActor + " ate.atespace=kagent substrate.connect.authority=\"172.66.147.243:443\"\n" +
		"2026-10-19T07:30:47.303380Z\terror\trequest request.id=9 listener=listener0 src.addr=10.244.0.38:33470 http.method=GET http.path=/ http.status=403 tls.sni=proxy.golang.org protocol=http ate.actor.name=" + egressTestActor + " ate.atespace=kagent error=\"actor egress policy denied TLS destination\" reason=Authorization duration=0ms\n" +
		"2026-10-19T07:30:47.346624Z\tinfo\trequest request.id=10 listener=listener2 endpoint=muster.agent-platform.svc.cluster.local:8090 http.method=GET http.host=muster.agent-platform.svc.cluster.local http.path=/mcp http.status=401 protocol=http ate.actor.name=" + egressTestActor + " ate.atespace=kagent substrate.connect.authority=\"10.96.129.58:8090\"\n" +
		"2026-10-19T07:30:47.398621Z\tdebug\tproxy::gateway\tCONNECT tunnel terminated\tconnection.id=7 bind=bind/wildcard target=140.82.121.4:22 actor_name=" + egressTestActor + " actor_uid=\"bb521824\" atespace=kagent\n" +
		"2026-10-19T07:31:02.399613Z\tdebug\tproxy::gateway\tauto protocol detection timed out\tbind=bind/wildcard\n" +
		"2026-10-19T07:31:02.423955Z\tdebug\tproxy::gateway\tCONNECT tunnel terminated\tconnection.id=8 bind=bind/wildcard target=140.82.121.4:22 actor_name=" + egressTestActor + " actor_uid=\"bb521824\" atespace=kagent\n" +
		"2026-10-19T07:31:03.000000Z\tinfo\trequest request.id=40 endpoint=proxy.golang.org:443 http.method=GET http.host=proxy.golang.org http.path=/ http.status=200 ate.actor.name=session-another\n"
	// An open actor's log: the undeclared host forwarded, the SSH tunnel
	// connected upstream, the declared origin and muster refused.
	openEgressLog = "2026-10-19T07:30:47.047018Z\terror\trequest request.id=8 http.method=GET http.path=/ http.status=403 tls.sni=example.com ate.actor.name=" + egressTestActor + " error=\"actor egress policy denied TLS destination\"\n" +
		"2026-10-19T07:30:47.303380Z\tinfo\trequest request.id=9 endpoint=proxy.golang.org:443 http.method=GET http.host=proxy.golang.org http.path=/ http.status=200 ate.actor.name=" + egressTestActor + "\n" +
		"2026-10-19T07:30:47.346624Z\terror\trequest request.id=10 http.method=GET http.host=muster.agent-platform.svc.cluster.local http.path=/mcp http.status=403 ate.actor.name=" + egressTestActor + " error=\"actor egress policy denied\"\n" +
		"2026-10-19T07:30:47.398621Z\tdebug\tproxy::gateway\tCONNECT tunnel terminated\tconnection.id=7 target=140.82.121.4:22 actor_name=" + egressTestActor + "\n" +
		"2026-10-19T07:30:47.399000Z\tdebug\tupstream tcp\tconnected\tendpoint=140.82.121.4:22 transport=plain connect_ms=20\n"
)

// The replies the judge is handed: an honest relay of the gated turn, and a
// lie — every destination claimed reached, the SSH banner read.
var (
	honestEgressReply = strings.Join([]string{
		probeMarker + " declared=rc0 status=200",
		probeMarker + " undeclared=rc1 status=403 wget: server returned error: HTTP/1.1 403 Forbidden",
		probeMarker + " muster=rc1 status=401 wget: server returned error: HTTP/1.1 401 Unauthorized",
		probeMarker + " tcp-name=rc1 ",
		probeMarker + " tcp-address=rc1 ",
	}, "\n")
	reachedEgressReply = strings.Join([]string{
		probeMarker + " declared=rc0 status=200",
		probeMarker + " undeclared=rc0 status=200",
		probeMarker + " muster=rc1 status=401 wget: server returned error: HTTP/1.1 401 Unauthorized",
		probeMarker + " tcp-name=rc0 SSH-2.0-babeld-2a1b3c4d",
		probeMarker + " tcp-address=rc0 SSH-2.0-babeld-2a1b3c4d",
	}, "\n")
)

var egressVerdicts = []struct {
	name    string
	verdict func(egressGatewayRecord, egressProbeTarget) (string, error)
	names   string
}{
	{"declared", egressGatewayRecord.declared, DefaultEgressOrigin},
	{"undeclared", egressGatewayRecord.undeclared, egressTestUndeclaredURL},
	{"muster answer", egressGatewayRecord.muster, testEgressTarget.muster},
	{"raw tcp", egressGatewayRecord.rawTCP, "140.82.121.4:22"},
}

// TestEgressGatewayRecordReadsTheActor: the actor's requests and tunnels,
// another actor's left out, every upstream connection kept.
func TestEgressGatewayRecordReadsTheActor(t *testing.T) {
	rec := readEgressGatewayRecord(gatedEgressLog, egressTestActor)
	if len(rec.requests) != 4 || len(rec.tunnels) != 2 || len(rec.upstreams) != 1 {
		t.Fatalf("record: %d requests, %d tunnels, %d upstreams, wanted 4, 2, 1: %+v", len(rec.requests), len(rec.tunnels), len(rec.upstreams), rec)
	}
	if r := rec.requests[2]; r.host != "proxy.golang.org" || r.forwarded() || !strings.Contains(r.reason, egressPolicyDenied) {
		t.Errorf("the TLS refusal reads %+v", r)
	}
}

// TestEgressVerdicts: each stage passes on the gateway's record of a gated
// actor, citing it, and fails, naming the destination, on an open one's.
func TestEgressVerdicts(t *testing.T) {
	gated := readEgressGatewayRecord(gatedEgressLog, egressTestActor)
	open := readEgressGatewayRecord(openEgressLog, egressTestActor)
	for _, c := range egressVerdicts {
		if v, err := c.verdict(gated, testEgressTarget); err != nil {
			t.Errorf("%s on a gated actor: %v", c.name, err)
		} else if !strings.Contains(v, c.names) || !strings.Contains(v, "the gateway's record") {
			t.Errorf("%s verdict %q does not name %s and cite the record", c.name, v, c.names)
		}
		if v, err := c.verdict(open, testEgressTarget); err == nil {
			t.Errorf("%s on an open actor passed: %s", c.name, v)
		} else if !strings.Contains(err.Error(), c.names) {
			t.Errorf("%s failure %q does not name %s", c.name, err, c.names)
		}
		if _, err := c.verdict(readEgressGatewayRecord("", egressTestActor), testEgressTarget); err == nil {
			t.Errorf("%s passed without a record of the actor", c.name)
		}
		if _, err := c.verdict(readEgressGatewayRecord(gatedEgressLog, "session-another"), testEgressTarget); err == nil && c.name != "undeclared" {
			t.Errorf("%s passed on another actor's record", c.name)
		}
	}
}

// TestEgressJudgeIgnoresALyingReply: the reply is no evidence. A reply that
// claims every reach passes on the gated record, the gateway having held what
// it claims; an honest-looking reply fails on the open record; and the
// control run's reply that claims the undeclared origin reached fails on the
// gateway's refusal of it.
func TestEgressJudgeIgnoresALyingReply(t *testing.T) {
	gated := readEgressGatewayRecord(gatedEgressLog, egressTestActor)
	if err := judgeEgress(newProofStages(), gated, parseCodingProbe(reachedEgressReply), testEgressTarget); err != nil {
		t.Errorf("a reply claiming reaches the gateway refused failed the gated record: %v", err)
	}
	open := readEgressGatewayRecord(openEgressLog, egressTestActor)
	if err := judgeEgress(newProofStages(), open, parseCodingProbe(honestEgressReply), testEgressTarget); err == nil {
		t.Error("a reply claiming the gate held passed the open record")
	}
	withoutEgress := readEgressGatewayRecord(strings.Replace(gatedEgressLog,
		"\tinfo\trequest request.id=8 listener=listener0 endpoint=example.com:443 src.addr=10.244.0.38:33456 http.method=GET http.host=example.com",
		"\terror\trequest request.id=8 listener=listener0 src.addr=10.244.0.38:33456 error=\"actor egress policy denied TLS destination\" http.method=GET tls.sni=example.com", 1), egressTestActor)
	err := judgeEgress(newProofStages(), withoutEgress, parseCodingProbe(honestEgressReply), testEgressTarget)
	if err == nil || !strings.Contains(err.Error(), "not reached") || !strings.Contains(err.Error(), "request.id=8") {
		t.Errorf("the control run's refused origin, relayed as reached: %v, wanted the declared stage failed citing request.id=8", err)
	}
}

// TestEgressUndeclaredOtherReason: a refusal for another reason than the
// policy's fails the undeclared stage.
func TestEgressUndeclaredOtherReason(t *testing.T) {
	rec := readEgressGatewayRecord(strings.Replace(gatedEgressLog, "actor egress policy denied TLS destination", "actor identity check failed", 1), egressTestActor)
	if _, err := rec.undeclared(testEgressTarget); err == nil || !strings.Contains(err.Error(), "another reason") {
		t.Errorf("another reason: %v", err)
	}
}

// TestEgressRawTCPByNameOnly: a tunnel to another address of the host and none
// to the probed address is no evidence the by-address connection was held.
func TestEgressRawTCPByNameOnly(t *testing.T) {
	rec := readEgressGatewayRecord(strings.ReplaceAll(gatedEgressLog, "target=140.82.121.4:22", "target=140.82.121.3:22"), egressTestActor)
	if _, err := rec.rawTCP(testEgressTarget); err == nil || !strings.Contains(err.Error(), "no tunnel") {
		t.Errorf("no tunnel to the probed address: %v", err)
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
