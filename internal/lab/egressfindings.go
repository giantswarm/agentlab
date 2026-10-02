package lab

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Substrate's egress gateway and the control plane its policy calls go to:
// the gateway container of deploy/atenet-egress writes one access-log line
// per request, at level error for every request it answered with an error;
// the actor identity check, the egress policy and the credential providers
// are gRPC calls to the headless Service api, served by Deployment
// ate-api-server.
const (
	atenetEgressDeployment  = "atenet-egress"
	atenetEgressContainer   = "agentgateway"
	substrateAPIService     = "api"
	substrateAPIDeployment  = "ate-api-server"
	egressFindingsMaxLines  = 5
	egressFindingsReadLimit = 30 * time.Second
)

// substratePolicyCallWords are the gateway's words for a request it answered
// from a failed call to the control plane (its policy decision, not the
// destination's answer).
var substratePolicyCallWords = []string{"actor egress policy", "actor identity check", "credential provider"}

// egressRefusal is one kind of request the egress gateway answered with an
// error: the destination, the status and the gateway's reason, with how
// often it happened in the window read.
type egressRefusal struct {
	method, host, path, status, reason string
	count                              int
}

func (r egressRefusal) String() string {
	times := ""
	if r.count > 1 {
		times = fmt.Sprintf(" (%d×)", r.count)
	}
	return fmt.Sprintf("%s %s %s%s%s: %s", r.status, r.method, r.host, r.path, times, r.reason)
}

// policyCall says whether the gateway answered from a failed call to the
// control plane rather than from the destination.
func (r egressRefusal) policyCall() bool {
	return slices.ContainsFunc(substratePolicyCallWords, func(w string) bool { return strings.Contains(r.reason, w) })
}

// egressFindings is what Substrate's egress gateway refused in the evidence
// window, as verdict lines: its distinct refusals, most recent last, and,
// when one came from a failed policy call, the control plane's endpoints that
// are no pod of Deployment ate-api-server (a foreign pod in the Service is a
// policy call refused at connect). Nothing refused: no lines.
func egressFindings() []string {
	ctx, cancel := context.WithTimeout(context.Background(), egressFindingsReadLimit)
	defer cancel()
	logs, err := podLogs(ctx, substrateNamespace, "deploy/"+atenetEgressDeployment, atenetEgressContainer, skillsEvidenceLogSince)
	if err != nil {
		return []string{fmt.Sprintf("Substrate's egress gateway (deploy/%s): its log could not be read: %v", atenetEgressDeployment, err)}
	}
	refusals := egressRefusals(logs)
	if len(refusals) == 0 {
		return nil
	}
	lines := []string{fmt.Sprintf("Substrate's egress gateway (deploy/%s) refused in the last %s:", atenetEgressDeployment, skillsEvidenceLogSince)}
	for _, r := range refusals {
		lines = append(lines, "  "+r.String())
	}
	if slices.ContainsFunc(refusals, egressRefusal.policyCall) {
		lines = append(lines, controlPlaneEndpointFindings(ctx)...)
	}
	return lines
}

// egressRefusals reads the gateway's error access-log lines out of its log:
// distinct (status, destination, reason), the last egressFindingsMaxLines in
// order of their latest occurrence.
func egressRefusals(logs string) []egressRefusal {
	var refusals []egressRefusal
	for _, line := range strings.Split(logs, "\n") {
		// <time>\t<level>\trequest <key=value …>
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 || fields[1] != "error" || !strings.HasPrefix(fields[2], "request ") {
			continue
		}
		kv := logfmtFields(strings.TrimPrefix(fields[2], "request "))
		r := egressRefusal{method: kv["http.method"], host: kv["http.host"], path: kv["http.path"], status: kv["http.status"], reason: kv["error"], count: 1}
		if i := slices.IndexFunc(refusals, func(o egressRefusal) bool {
			return o.status == r.status && o.host == r.host && o.path == r.path && o.reason == r.reason
		}); i >= 0 {
			r.count += refusals[i].count
			refusals = slices.Delete(refusals, i, i+1)
		}
		refusals = append(refusals, r)
	}
	if len(refusals) > egressFindingsMaxLines {
		refusals = refusals[len(refusals)-egressFindingsMaxLines:]
	}
	return refusals
}

// logfmtFields parses agentgateway's key=value fields; a value in double
// quotes may hold spaces and backslash-escaped quotes.
func logfmtFields(s string) map[string]string {
	fields := map[string]string{}
	for s = strings.TrimLeft(s, " \t"); s != ""; s = strings.TrimLeft(s, " \t") {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key, rest := s[:eq], s[eq+1:]
		var value strings.Builder
		if strings.HasPrefix(rest, `"`) {
			i := 1
			for ; i < len(rest) && rest[i] != '"'; i++ {
				if rest[i] == '\\' && i+1 < len(rest) {
					i++
				}
				value.WriteByte(rest[i])
			}
			rest = rest[min(i+1, len(rest)):]
		} else {
			end := strings.IndexAny(rest, " \t")
			if end < 0 {
				end = len(rest)
			}
			value.WriteString(rest[:end])
			rest = rest[end:]
		}
		fields[key] = value.String()
		s = rest
	}
	return fields
}

// controlPlaneEndpointFindings reads the endpoints of the control plane's
// Service and names those that are no pod of Deployment ate-api-server.
func controlPlaneEndpointFindings(ctx context.Context) []string {
	k, err := labKube()
	if err != nil {
		return []string{fmt.Sprintf("  the control plane's Service %s/%s could not be read: %v", substrateNamespace, substrateAPIService, err)}
	}
	endpoints, err := k.clientset.DiscoveryV1().EndpointSlices(substrateNamespace).List(ctx, metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + substrateAPIService})
	if err != nil {
		return []string{fmt.Sprintf("  the control plane's Service %s/%s could not be read: %v", substrateNamespace, substrateAPIService, err)}
	}
	pods, err := k.clientset.CoreV1().Pods(substrateNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return []string{fmt.Sprintf("  the pods behind Service %s/%s could not be read: %v", substrateNamespace, substrateAPIService, err)}
	}
	return foreignAPIEndpoints(endpoints.Items, pods.Items)
}

// foreignAPIEndpoints names the endpoints of the control plane's Service
// that are no pod of Deployment ate-api-server; when every endpoint is one,
// it says so, so the verdict looks elsewhere.
func foreignAPIEndpoints(endpointSlices []discoveryv1.EndpointSlice, pods []corev1.Pod) []string {
	byName := map[string]corev1.Pod{}
	for _, p := range pods {
		byName[p.Name] = p
	}
	var foreign []string
	total := 0
	for _, s := range endpointSlices {
		for _, e := range s.Endpoints {
			total++
			if e.TargetRef == nil || e.TargetRef.Kind != "Pod" {
				foreign = append(foreign, fmt.Sprintf("%v (no pod)", e.Addresses))
				continue
			}
			if pod, ok := byName[e.TargetRef.Name]; !ok || !ownedByDeployment(pod, substrateAPIDeployment) {
				foreign = append(foreign, fmt.Sprintf("pod %s %v", e.TargetRef.Name, e.Addresses))
			}
		}
	}
	if len(foreign) == 0 {
		return []string{fmt.Sprintf("  the control plane's Service %s/%s routes to %d endpoint(s), every one a pod of Deployment %s", substrateNamespace, substrateAPIService, total, substrateAPIDeployment)}
	}
	return []string{fmt.Sprintf("  the control plane's Service %s/%s routes to %s, no pod of Deployment %s: a pod in %s carrying the api-server's selector labels takes a share of the gateway's policy calls and refuses them",
		substrateNamespace, substrateAPIService, strings.Join(foreign, ", "), substrateAPIDeployment, substrateNamespace)}
}

// ownedByDeployment says whether a pod belongs to a ReplicaSet of the named
// Deployment (the ReplicaSet is named <deployment>-<pod-template-hash>).
func ownedByDeployment(pod corev1.Pod, deployment string) bool {
	hash := pod.Labels["pod-template-hash"]
	return hash != "" && slices.ContainsFunc(pod.OwnerReferences, func(o metav1.OwnerReference) bool {
		return o.Kind == "ReplicaSet" && o.Name == deployment+"-"+hash
	})
}
