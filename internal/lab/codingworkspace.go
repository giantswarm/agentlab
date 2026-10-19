package lab

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// coding-test's workspace Session: on a lab with the provider instance
// fake-bearer, the proof's Session starts through agent-manager's
// start_session with the person's grant for a workspace. kagent adds the
// provider muster to it, agent-manager binds its sandbox actor to the person
// in muster, and the egress gateway sets the Actor's JWT on every request
// from the sandbox to muster: the agent's MCP binding reaches muster as the
// person with no credential in the sandbox, on every Harness. muster logs an
// audit event for each such request (muster 5.47.0 and newer), which names
// the actor and the bound Session; the mcp call stage judges it against the
// binding muster logged at start_session.

const (
	// codingTestWorkspace is the proof's workspace on fake-bearer.
	codingTestWorkspace = "agentlab-coding"
	// actorBearerAuditPrefix opens muster's audit message of an actor-JWT
	// request.
	actorBearerAuditPrefix = "[AUDIT] action=actor_bearer " // #nosec G101 -- a log message's prefix, not a credential
	// musterAuditWait bounds the wait for muster's audit line after the
	// turn.
	musterAuditWait = 30 * time.Second
)

// setupWorkspace readies the workspace Session on a lab with fake-bearer,
// or says why the Session starts without one.
func (p *codingProof) setupWorkspace() error {
	if err := fakeBearerRefusal(p.cfg, "coding-test"); err != nil {
		p.stages.skip("no workspace Session on this lab, the Session starts without one and muster sees no person on a runtime without a forwarder: %v", err)
		return nil
	}
	p.rig = newWorkspaceSessionRig(p.cfg, p.timeout, codingTestWorkspace, syncMovingRepo, false)
	if err := p.rig.sp.preconditions(p.user.Email); err != nil {
		return err
	}
	if err := p.rig.preconditions(p.user, p.api); err != nil {
		return err
	}
	p.sessions = p.rig.set
	return p.rig.createWorkspace()
}

// startWorkspaceSession starts the proof's Session in the workspace and
// requires the provider muster on it.
func (p *codingProof) startWorkspaceSession(agent string) (*apiv1alpha1.Session, error) {
	session, err := p.rig.startSession(agent, true)
	if err != nil {
		return nil, err
	}
	if !hasMusterProvider(session) {
		return nil, fmt.Errorf("kagent recorded the providers %s on the workspace Session %s, not %s: the installation names no actor-JWT provider for muster (kagent controller.session.actorJWTProviders, kagent 1.18 and newer) (%s)",
			grantProvidersLine(toGrantProviders(session.GetProviders())), session.GetId(), musterProvider, p.line)
	}
	note("Session %s started in %s through start_session, kagent records the provider %s on it", session.GetId(), codingTestWorkspace, musterProvider)
	return session, nil
}

// musterAudit waits for muster's audit line of the Session's MCP call and
// judges it.
func (p *codingProof) musterAudit(session string) (string, error) {
	deadline := time.Now().Add(musterAuditWait)
	for {
		logs, err := p.rig.musterLog()
		if err != nil {
			return "", err
		}
		verdict, err := judgeActorBearerAudit(logs, kagentNamespace+"/"+session, p.rig.started.Add(-unbindClockSlack))
		if err == nil || time.Now().After(deadline) {
			return verdict, err
		}
		time.Sleep(3 * time.Second)
	}
}

// musterLogLine is the part of a muster log line the audit judge reads.
type musterLogLine struct {
	Time time.Time `json:"time"`
	Msg  string    `json:"msg"`
}

// judgeActorBearerAudit is the verdict on muster's log since the instant:
// the session's sandbox actor bound to a person at start_session, and an
// accepted actor-JWT request of that actor, as that person, for that
// Session.
func judgeActorBearerAudit(logs, session string, since time.Time) (string, error) {
	events := musterBindingEvents(logs, since)
	i := slices.IndexFunc(events, func(e musterBindingEvent) bool {
		return e.Msg == musterSessionBound && e.Session == session
	})
	if i < 0 {
		return "", fmt.Errorf("muster logged no %s for the Session %s: agent-manager bound no person to its sandbox (muster's credentialProvider off, or agent-manager without muster.bindingsUrl)", musterSessionBound, session)
	}
	bound := events[i]
	want := []string{"outcome=success", "subject=" + truncateIdentifier(bound.Sub), "details=actor=" + bound.Actor + " session=" + bound.Session}
	var refused []string
	audited := false
	for _, raw := range strings.Split(logs, "\n") {
		start := strings.IndexByte(raw, '{')
		if start < 0 {
			continue
		}
		var line musterLogLine
		if json.Unmarshal([]byte(raw[start:]), &line) != nil || line.Time.Before(since) || !strings.HasPrefix(line.Msg, actorBearerAuditPrefix) {
			continue
		}
		audited = true
		if !strings.Contains(line.Msg, "actor="+bound.Actor+" ") && !strings.HasSuffix(line.Msg, "actor="+bound.Actor) {
			continue
		}
		if containsAll(line.Msg, want) {
			return fmt.Sprintf("muster audited the actor %s's request as the person it is bound to (%s), for the Session %s: %q", bound.Actor, truncateIdentifier(bound.Sub), bound.Session, line.Msg), nil
		}
		refused = append(refused, line.Msg)
	}
	switch {
	case len(refused) > 0:
		return "", fmt.Errorf("muster audited the actor %s of the Session %s, not as accepted for its person %s: %s", bound.Actor, session, truncateIdentifier(bound.Sub), strings.Join(refused, " | "))
	case !audited:
		return "", fmt.Errorf("muster logged no actor-JWT audit event (%q) since the Session %s started: muster before 5.47.0 audits none, or no request of its sandbox reached muster with the Actor's JWT", strings.TrimSpace(actorBearerAuditPrefix), session)
	default:
		return "", fmt.Errorf("muster audited no actor-JWT request of the actor %s, bound to the Session %s: the sandbox's MCP calls reached muster without the Actor's JWT", bound.Actor, session)
	}
}

// truncateIdentifier is muster's logging.TruncateIdentifier: the first
// eight characters of an identifier and an ellipsis.
func truncateIdentifier(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "..."
}

// containsAll says s contains every one of parts.
func containsAll(s string, parts []string) bool {
	for _, part := range parts {
		if !strings.Contains(s, part) {
			return false
		}
	}
	return true
}
