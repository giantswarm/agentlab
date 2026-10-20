package lab

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The egress proof's evidence is Substrate's egress gateway's own record of
// the probe's actor, never the agent's reply: every connection the actor
// opens is tunneled to the gateway, which writes
//   - one access-log line per HTTP request (`request …`, info or error), the
//     actor as ate.actor.name, the destination as http.host (tls.sni when TLS
//     was refused at the handshake), endpoint= when it forwarded the request
//     and error= with its reason when it answered itself;
//   - one `CONNECT tunnel terminated` line per tunnel it accepted, the actor
//     as actor_name and the destination address as target (debug, the level
//     Substrate's manifests run the gateway at);
//   - one `upstream tcp connected` line per upstream connection it opened, the
//     address as endpoint (debug).
//
// A raw TCP connection that no protocol detection answers gets the tunnel line
// and no upstream line: the gateway never connected it on.

// egressGatewayRequest is one access-log line of the actor's.
type egressGatewayRequest struct {
	time, id, method, host, path, status, endpoint, authority, reason string
}

// forwarded says the gateway passed the request on to its destination.
func (r egressGatewayRequest) forwarded() bool { return r.endpoint != "" }

func (r egressGatewayRequest) String() string {
	s := fmt.Sprintf("%s request.id=%s %s %s%s http.status=%s", r.time, r.id, r.method, r.host, r.path, r.status)
	if r.forwarded() {
		return s + " endpoint=" + r.endpoint
	}
	return s + fmt.Sprintf(" error=%q", r.reason)
}

// egressGatewayTunnel is one CONNECT tunnel the gateway accepted for the actor.
type egressGatewayTunnel struct {
	time, connection, target string
}

func (t egressGatewayTunnel) String() string {
	return fmt.Sprintf("%s connection.id=%s CONNECT tunnel terminated target=%s", t.time, t.connection, t.target)
}

// egressGatewayRecord is what the gateway recorded of one actor in the window
// read, and the upstream addresses it connected to in that window.
type egressGatewayRecord struct {
	actor     string
	requests  []egressGatewayRequest
	tunnels   []egressGatewayTunnel
	upstreams []string
}

// readEgressGatewayRecord reads the actor's lines out of the gateway's log:
// <time>\t<level>\t<target>[\t<message>]\t<key=value …>.
func readEgressGatewayRecord(logs, actor string) egressGatewayRecord {
	rec := egressGatewayRecord{actor: actor}
	for _, line := range strings.Split(logs, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		at := fields[0]
		switch {
		case strings.HasPrefix(fields[2], "request "):
			kv := logfmtFields(strings.TrimPrefix(fields[2], "request "))
			if kv["ate.actor.name"] != actor {
				continue
			}
			rec.requests = append(rec.requests, egressGatewayRequest{
				time: at, id: kv["request.id"], method: kv["http.method"], host: cmp.Or(kv["http.host"], kv["tls.sni"]), path: kv["http.path"],
				status: kv["http.status"], endpoint: kv["endpoint"], authority: kv["substrate.connect.authority"], reason: kv[egressLogErrorField],
			})
		case len(fields) >= 5 && fields[2] == "proxy::gateway" && fields[3] == "CONNECT tunnel terminated":
			kv := logfmtFields(fields[4])
			if kv["actor_name"] != actor {
				continue
			}
			rec.tunnels = append(rec.tunnels, egressGatewayTunnel{time: at, connection: kv["connection.id"], target: kv["target"]})
		case len(fields) >= 5 && fields[2] == "upstream tcp" && fields[3] == "connected":
			if endpoint := logfmtFields(fields[4])["endpoint"]; endpoint != "" {
				rec.upstreams = append(rec.upstreams, endpoint)
			}
		}
	}
	return rec
}

// readEgressGatewayLog reads the egress gateway's log of the last since.
func readEgressGatewayLog(since time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), egressFindingsReadLimit)
	defer cancel()
	logs, err := podLogs(ctx, substrateNamespace, "deploy/"+atenetEgressDeployment, atenetEgressContainer, since)
	if err != nil {
		return "", fmt.Errorf("the log of Substrate's egress gateway (deploy/%s) could not be read: %w", atenetEgressDeployment, err)
	}
	return logs, nil
}

// requestsTo are the actor's requests to the URL's host, its path too when the
// URL names one beyond "/", and of the method when one is given.
func (rec egressGatewayRecord) requestsTo(rawURL, method string) []egressGatewayRequest {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(rec.requests), func(r egressGatewayRequest) bool {
		return r.host != u.Hostname() || (method != "" && r.method != method) || (strings.Trim(u.Path, "/") != "" && r.path != u.Path)
	})
}

// none words the absence of the actor's requests to a destination.
func (rec egressGatewayRecord) none(dest string) error {
	return fmt.Errorf("the egress gateway (deploy/%s) recorded no request of actor %s to %s (%d request(s) of the actor in the window)", atenetEgressDeployment, rec.actor, dest, len(rec.requests))
}

// declared: the gateway forwarded the actor's request to the declared origin.
func (rec egressGatewayRecord) declared(t egressProbeTarget) (string, error) {
	got := rec.requestsTo(t.origin, "GET")
	if len(got) == 0 {
		return "", rec.none(t.origin)
	}
	if i := slices.IndexFunc(got, egressGatewayRequest.forwarded); i >= 0 {
		return fmt.Sprintf("%s reached, the gateway's record: %s", t.origin, got[i]), nil
	}
	return "", fmt.Errorf("%s, the Agent's declared origin, not reached from the actor, the gateway's record: %s", t.origin, got[len(got)-1])
}

// undeclared: the gateway answered the actor's request itself with its 403
// and the policy's reason, and forwarded none.
func (rec egressGatewayRecord) undeclared(t egressProbeTarget) (string, error) {
	got := rec.requestsTo(t.undeclared, "")
	if len(got) == 0 {
		return "", rec.none(t.undeclared)
	}
	if i := slices.IndexFunc(got, egressGatewayRequest.forwarded); i >= 0 {
		return "", fmt.Errorf("%s, outside the Agent's egress, reached from the actor, the gateway's record: %s", t.undeclared, got[i])
	}
	for _, r := range slices.Backward(got) {
		if r.status == egressGateStatus && strings.Contains(r.reason, egressPolicyDenied) {
			return fmt.Sprintf("%s denied by the egress gate, the gateway's record: %s", t.undeclared, r), nil
		}
	}
	return "", fmt.Errorf("the egress gateway refused %s for another reason than %d %q, the gateway's record: %s", t.undeclared, 403, egressPolicyDenied, got[len(got)-1])
}

// muster: the gateway forwarded the probe's GET to muster, which answered it
// itself (401 without a token), with no egress entry for it.
func (rec egressGatewayRecord) muster(t egressProbeTarget) (string, error) {
	got := rec.requestsTo(t.muster, "GET")
	if len(got) == 0 {
		return "", rec.none(t.muster)
	}
	if i := slices.IndexFunc(got, egressGatewayRequest.forwarded); i >= 0 {
		return fmt.Sprintf("muster (%s) answered the actor, the gateway's record: %s", t.muster, got[i]), nil
	}
	return "", fmt.Errorf("muster (%s), the agent's MCP server, not reached from the actor, the gateway's record: %s", t.muster, got[len(got)-1])
}

// rawTCP: the gateway took the actor's tunnel to the destination's address
// and connected none of it on: no upstream connection to the tunnel's target,
// no request forwarded to it. No tunnel at all is no evidence and fails.
func (rec egressGatewayRecord) rawTCP(t egressProbeTarget) (string, error) {
	port := strconv.Itoa(t.tcpPort)
	address := net.JoinHostPort(t.tcpAddress, port)
	tunnels := slices.DeleteFunc(slices.Clone(rec.tunnels), func(tn egressGatewayTunnel) bool {
		_, p, err := net.SplitHostPort(tn.target)
		return err != nil || p != port
	})
	if !slices.ContainsFunc(tunnels, func(tn egressGatewayTunnel) bool { return tn.target == address }) {
		return "", fmt.Errorf("the egress gateway (deploy/%s) recorded no tunnel of actor %s to %s (tunnels to port %s: %d): no record proves the raw TCP connection was held at the gate", atenetEgressDeployment, rec.actor, address, port, len(tunnels))
	}
	var held []string
	for _, tn := range tunnels {
		if slices.Contains(rec.upstreams, tn.target) {
			return "", fmt.Errorf("raw TCP to %s, outside every egress list, passed the actor's egress: the gateway connected upstream to %s after %s", tn.target, tn.target, tn)
		}
		if i := slices.IndexFunc(rec.requests, func(r egressGatewayRequest) bool { return r.forwarded() && r.authority == tn.target }); i >= 0 {
			return "", fmt.Errorf("raw TCP to %s, outside every egress list, passed the actor's egress: the gateway forwarded %s", tn.target, rec.requests[i])
		}
		held = append(held, tn.String())
	}
	return fmt.Sprintf("raw TCP to %s:%s held at the gate, no upstream connection, the gateway's record: %s", t.tcpHost, port, strings.Join(held, "; ")), nil
}
