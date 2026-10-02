package lab

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The egress gateway's log of a golden boot whose git fetch failed: the
// ref listing passes, the upload-pack POST is answered from a policy call
// that was refused at connect (giantswarm/agentlab#336), and debug noise.
const egressRefusalLog = "2026-10-02T06:04:50.626592Z\tdebug\tupstream request\trequest.id=185 target=github.com:443 endpoint=140.82.121.4:443 http.status=200\n" +
	"2026-10-02T06:04:50.626756Z\tinfo\trequest request.id=185 gateway=default/default http.method=GET http.host=github.com http.path=/giantswarm/agent-skills/info/refs?service=git-upload-pack http.status=200 protocol=http\n" +
	"2026-10-02T06:04:20.280186Z\terror\trequest request.id=184 gateway=default/default listener=listener0 src.addr=10.244.0.203:58816 http.method=GET http.host=github.com http.path=/giantswarm/agent-skills/info/refs?service=git-upload-pack http.version=HTTP/2.0 http.status=403 protocol=http ate.atespace=ate-golden error=\"actor egress policy denied: code: 'Unknown error', message: \\\"upstream call failed: Connect: Connection refused (os error 111)\\\", source: UpstreamCallFailed(Connect: Connection refused (os error 111))\" reason=Authorization duration=0ms substrate.connect.authority=\"140.82.121.4:443\"\n" +
	"2026-10-02T06:04:50.628842Z\terror\trequest request.id=186 gateway=default/default listener=listener0 src.addr=10.244.0.203:55878 http.method=POST http.host=github.com http.path=/giantswarm/agent-skills/git-upload-pack http.version=HTTP/2.0 http.status=403 protocol=http ate.atespace=ate-golden error=\"actor egress policy denied: code: 'Unknown error', message: \\\"upstream call failed: Connect: Connection refused (os error 111)\\\", source: UpstreamCallFailed(Connect: Connection refused (os error 111))\" reason=Authorization duration=0ms substrate.connect.authority=\"140.82.121.4:443\"\n" +
	"2026-10-02T06:05:20.628842Z\terror\trequest request.id=188 gateway=default/default http.method=POST http.host=github.com http.path=/giantswarm/agent-skills/git-upload-pack http.status=403 error=\"actor egress policy denied: code: 'Unknown error', message: \\\"upstream call failed: Connect: Connection refused (os error 111)\\\", source: UpstreamCallFailed(Connect: Connection refused (os error 111))\" reason=Authorization\n"

const refusedPolicyCall = `actor egress policy denied: code: 'Unknown error', message: "upstream call failed: Connect: Connection refused (os error 111)", source: UpstreamCallFailed(Connect: Connection refused (os error 111))`

func TestEgressRefusalsNameTheDestinationAndTheGatewaysReason(t *testing.T) {
	got := egressRefusals(egressRefusalLog)
	want := []string{
		"403 GET github.com/giantswarm/agent-skills/info/refs?service=git-upload-pack: " + refusedPolicyCall,
		"403 POST github.com/giantswarm/agent-skills/git-upload-pack (2×): " + refusedPolicyCall,
	}
	if len(got) != len(want) {
		t.Fatalf("egressRefusals = %v, want %d refusals", got, len(want))
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("refusal %d = %q, want %q", i, got[i].String(), want[i])
		}
		if !got[i].policyCall() {
			t.Errorf("refusal %d is not read as a failed policy call: %q", i, got[i].reason)
		}
	}
}

func TestEgressRefusalsAreEmptyWithoutAnErrorAccessLine(t *testing.T) {
	logs := "2026-10-02T06:04:50Z\tinfo\trequest http.method=GET http.host=github.com http.status=200\n2026-10-02T06:04:51Z\terror\tresource_manager\tsomething else\n"
	if got := egressRefusals(logs); len(got) != 0 {
		t.Fatalf("egressRefusals = %v, want none", got)
	}
	r := egressRefusal{status: "502", reason: "upstream call failed: Connect: Connection refused"}
	if r.policyCall() {
		t.Fatalf("a destination's refusal %q is read as a failed policy call", r.reason)
	}
}

func TestLogfmtFieldsKeepQuotedValuesWhole(t *testing.T) {
	got := logfmtFields(`a=1 b="two words" c="say \"hi\"" d=`)
	want := map[string]string{"a": "1", "b": "two words", "c": `say "hi"`, "d": ""}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("field %s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

func apiServerPod(name, hash string) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:            name,
		Labels:          map[string]string{"app": substrateAPIDeployment, "pod-template-hash": hash},
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: substrateAPIDeployment + "-" + hash}},
	}}
}

func apiEndpoint(pod, ip string) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{Addresses: []string{ip}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: pod}}
}

// A pod that carries the api-server's selector labels but serves nothing
// joins the control plane's Service: the finding names it.
func TestForeignAPIEndpointsNameAPodThatIsNoAPIServer(t *testing.T) {
	probe := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "probe-api", Labels: map[string]string{"app": substrateAPIDeployment}}}
	pods := []corev1.Pod{apiServerPod("ate-api-server-68cdbb59f9-n2rph", "68cdbb59f9"), apiServerPod("ate-api-server-68cdbb59f9-r92mq", "68cdbb59f9"), probe}
	endpoints := []discoveryv1.EndpointSlice{{Endpoints: []discoveryv1.Endpoint{
		apiEndpoint("ate-api-server-68cdbb59f9-n2rph", "10.244.0.207"),
		apiEndpoint("ate-api-server-68cdbb59f9-r92mq", "10.244.0.199"),
		apiEndpoint("probe-api", "10.244.0.59"),
	}}}
	got := strings.Join(foreignAPIEndpoints(endpoints, pods), "\n")
	if !strings.Contains(got, "pod probe-api [10.244.0.59]") || strings.Contains(got, "n2rph") || !strings.Contains(got, "no pod of Deployment "+substrateAPIDeployment) {
		t.Fatalf("foreignAPIEndpoints = %q, want the probe pod named as no pod of %s and the api-servers left out", got, substrateAPIDeployment)
	}
}

func TestForeignAPIEndpointsSayWhenEveryEndpointIsAnAPIServer(t *testing.T) {
	pods := []corev1.Pod{apiServerPod("ate-api-server-68cdbb59f9-n2rph", "68cdbb59f9")}
	endpoints := []discoveryv1.EndpointSlice{{Endpoints: []discoveryv1.Endpoint{apiEndpoint("ate-api-server-68cdbb59f9-n2rph", "10.244.0.207")}}}
	got := strings.Join(foreignAPIEndpoints(endpoints, pods), "\n")
	if !strings.Contains(got, "1 endpoint(s), every one a pod of Deployment "+substrateAPIDeployment) {
		t.Fatalf("foreignAPIEndpoints = %q, want every endpoint confirmed as an api-server", got)
	}
}
