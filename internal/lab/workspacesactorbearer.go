package lab

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The muster stage of `workspaces-test --part caller-credential`: a Session
// admitted with a grant carries the provider muster kagent adds where the
// installation names muster as verifying the Actor's own JWT, and its
// sandbox calls muster's /mcp with a placeholder Authorization; the egress
// gateway replaces it with the Actor's JWT for muster's audience and muster
// serves the call as the person the Session's actor is bound to. A Session
// without a workspace sends the placeholder as it is, which muster refuses.

const (
	// musterProvider is the reserved provider instance of muster's actor
	// bearer.
	musterProvider = "muster"
	// musterInClusterURL is muster's MCP endpoint as an Agent's
	// RemoteMCPServer names it.
	musterInClusterURL = "http://muster.agent-platform.svc.cluster.local:8090/mcp"
)

// hasMusterProvider says kagent recorded the provider muster on session.
func hasMusterProvider(session *apiv1alpha1.Session) bool {
	return slices.ContainsFunc(session.GetProviders(), func(p *apiv1alpha1.SessionProvider) bool {
		return p.GetAudience() == musterProvider && strings.EqualFold(p.GetScheme(), providerSchemeBearer)
	})
}

// musterActorCommand posts an MCP initialize to muster with the placeholder
// and prints the HTTP status.
func musterActorCommand() string {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agentlab-probe","version":"0"}}}`
	return `echo "` + probeMarker + ` pwd=$(pwd)"; ` +
		`if command -v curl >/dev/null; then s=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Authorization: ` + callerCredentialPlaceholder + `' -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' --data '` + body + `' '` + musterInClusterURL + `' 2>/dev/null); ` +
		`else s=$(wget -S -O /dev/null --header='Authorization: ` + callerCredentialPlaceholder + `' --header='Content-Type: application/json' --header='Accept: application/json, text/event-stream' --post-data='` + body + `' '` + musterInClusterURL + `' 2>&1 | grep -o 'HTTP/[0-9.]* [0-9][0-9][0-9]' | tail -1 | awk '{print $2}'); fi; ` +
		`echo "` + probeMarker + ` api-status=$(echo "$s" | tr ' ' '_')"; ` +
		`echo "` + probeMarker + ` jwt-env=$(env | grep -E '` + jwtPattern + `' | cut -d= -f1 | sort -u | tr '\n' ' ')"; ` +
		`echo "` + probeMarker + ` jwt-files=$(grep -rlE '` + jwtPattern + `' /data /workspace "$PWD" 2>/dev/null | sort -u | tr '\n' ' ')"`
}

const (
	musterActorPurpose = "the command sends one MCP initialize request with a placeholder Authorization header to the lab's own muster, prints only the HTTP status, and checks that no JWT sits in your environment or your files by printing the names of matching variables and files, never a value."
	musterActorExplain = "What it does: this is agentlab's test of the egress gateway on a throwaway lab cluster. " + musterActorPurpose + " The gateway, not you, may replace the placeholder."
)

// probeMuster runs the initialize from the Session's sandbox: answered 200
// for a Session in the workspace, refused for one without.
func (cp *callerCredentialProof) probeMuster(session *apiv1alpha1.Session, inWorkspace bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), cp.timeout)
	defer cancel()
	var v callerCredentialValues
	if _, _, err := probeTurnWith(ctx, cp.api, session, musterActorCommand(), musterActorExplain, musterActorPurpose, func(reply string) (err error) {
		v, err = parseCallerCredentialTurn(reply)
		return err
	}); err != nil {
		return err
	}
	logs, _ := podLogs(ctx, "agent-platform", "deploy/muster", "", 0)
	var lines []string
	actor := substrateActorName(session.GetId())
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, actor) || (strings.Contains(line, "actor_bearer") && strings.Contains(line, session.GetId())) {
			lines = append(lines, excerpt(line, 400))
		}
	}
	note("%s: muster's initialize from the sandbox answered %s; muster's log lines for its actor %s: %s", session.GetId(), v.status, actor, strings.Join(lines, " | "))
	if len(v.env) > 0 || len(v.files) > 0 {
		return fmt.Errorf("a JWT in the sandbox of Session %s: environment variables %v, files %v", session.GetId(), v.env, v.files)
	}
	if inWorkspace && v.status != "200" {
		return fmt.Errorf("muster answered the workspace Session %s's initialize %s, want 200: the Actor's JWT for muster's audience served as the bound person", session.GetId(), v.status)
	}
	if !inWorkspace && v.status == "200" {
		return fmt.Errorf("muster answered the Session %s without a workspace 200 on a placeholder: an actor JWT reached muster for a Session kagent admitted no muster provider to", session.GetId())
	}
	return nil
}
