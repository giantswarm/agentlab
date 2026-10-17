package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The delete_session stage of `workspaces-test --part caller-credential`:
// the workspace Session deleted through agent-manager's delete_session as the
// person who started it, and muster's binding of its sandbox removed. At
// start_session agent-manager binds a workspace Session's sandbox actor to
// the person in muster (PUT /session-bindings; muster logs session_bound
// with the Session and the actor); delete_session unbinds it (DELETE /session-bindings;
// muster logs session_unbound with the actor) and answers a warning when
// muster did not. muster serves no read of a binding, so its log is the
// evidence.

const (
	// musterSessionBound and musterSessionUnbound are muster's log messages
	// of a binding made and removed.
	musterSessionBound   = "session_bound"
	musterSessionUnbound = "session_unbound"
	// deleteSessionIDArg is delete_session's session id.
	deleteSessionIDArg = "sessionId"
	// unbindLogWait bounds the wait for muster's session_unbound line after
	// delete_session answered.
	unbindLogWait = 30 * time.Second
	// unbindClockSlack allows for the clocks of the lab host and muster's
	// pod when a log line is compared with the moment of the call.
	unbindClockSlack = 2 * time.Second
)

// sessionDeleted is delete_session's answer, the fields the stage reads.
type sessionDeleted struct {
	Session struct {
		ID    string `json:"id"`
		State string `json:"state"`
	} `json:"session"`
	Warning string `json:"warning"`
}

// musterBindingEvent is one session_bound or session_unbound line of
// muster's log.
type musterBindingEvent struct {
	Time    time.Time `json:"time"`
	Msg     string    `json:"msg"`
	Sub     string    `json:"sub"`
	Session string    `json:"session"`
	Actor   string    `json:"actor"`
}

// musterBindingEvents are the binding lines of a muster log from the instant
// on, in the log's order.
func musterBindingEvents(logs string, since time.Time) []musterBindingEvent {
	var events []musterBindingEvent
	for _, line := range strings.Split(logs, "\n") {
		start := strings.IndexByte(line, '{')
		if start < 0 {
			continue
		}
		var e musterBindingEvent
		if json.Unmarshal([]byte(line[start:]), &e) != nil || e.Time.Before(since) {
			continue
		}
		if e.Msg == musterSessionBound || e.Msg == musterSessionUnbound {
			events = append(events, e)
		}
	}
	return events
}

// judgeUnbound is the verdict on muster's binding lines for a deleted
// Session (<namespace>/<id>, as agent-manager names it to muster): its
// sandbox actor bound at the start, and that actor unbound by the same person
// once delete_session was called. The bound line is returned.
func judgeUnbound(events []musterBindingEvent, session string, deleteCalled time.Time) (musterBindingEvent, error) {
	i := slices.IndexFunc(events, func(e musterBindingEvent) bool {
		return e.Msg == musterSessionBound && e.Session == session
	})
	if i < 0 {
		return musterBindingEvent{}, fmt.Errorf("muster logged no %s for the Session %s: start_session bound nothing to unbind (muster's credentialProvider off, or agent-manager without muster.bindingsUrl)", musterSessionBound, session)
	}
	bound := events[i]
	for _, e := range events[i+1:] {
		if e.Msg != musterSessionUnbound || e.Actor != bound.Actor {
			continue
		}
		if e.Time.Before(deleteCalled.Add(-unbindClockSlack)) {
			continue
		}
		if e.Sub != bound.Sub {
			return bound, fmt.Errorf("muster logged the actor %s of the Session %s unbound by %q, bound by %q: delete_session unbinds as the person who started it", bound.Actor, session, e.Sub, bound.Sub)
		}
		return bound, nil
	}
	return bound, fmt.Errorf("muster logged no %s for the actor %s of the Session %s after delete_session: the binding of the deleted Session's sandbox stays (it would outlive the Session until its TTL)", musterSessionUnbound, bound.Actor, session)
}

// deleteSession deletes the workspace Session through agent-manager's
// delete_session as the admin and proves muster's binding of its sandbox
// removed: an answer without a warning, and muster's session_unbound line for
// the actor it logged bound at the Session's start.
func (cp *callerCredentialProof) deleteSession(id string) error {
	called := time.Now()
	env, err := cp.admin.session.callToolEnvelope(agentManagerToolPrefix+"delete_session", map[string]any{deleteSessionIDArg: id})
	if err != nil {
		return err
	}
	if env.IsError {
		return fmt.Errorf("delete_session %s refused: %.400s", id, env.Content[0].Text)
	}
	var deleted sessionDeleted
	if err := json.Unmarshal([]byte(env.Content[0].Text), &deleted); err != nil || deleted.Session.ID != id {
		return fmt.Errorf("delete_session answered no Session %s: %.400s", id, env.Content[0].Text)
	}
	cp.set.ids = slices.DeleteFunc(cp.set.ids, func(s string) bool { return s == id })
	if deleted.Warning != "" {
		return fmt.Errorf("delete_session deleted %s and warned: %s", id, deleted.Warning)
	}
	session := kagentNamespace + "/" + id
	deadline := time.Now().Add(unbindLogWait)
	for {
		bound, err := cp.unbound(session, called)
		if err == nil {
			note("delete_session: %s %s, no warning; muster logged its actor %s bound at the start and unbound after the delete, by the same person",
				id, strings.TrimPrefix(deleted.Session.State, "RUNTIME_STATE_"), bound.Actor)
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(3 * time.Second)
	}
}

// unbound reads muster's log since the Session's start and judges it.
func (cp *callerCredentialProof) unbound(session string, deleteCalled time.Time) (musterBindingEvent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	logs, err := podLogs(ctx, platformNamespace, "deploy/"+componentMuster, componentMuster, time.Since(cp.started)+time.Minute)
	if err != nil {
		return musterBindingEvent{}, fmt.Errorf("reading muster's log: %w", err)
	}
	return judgeUnbound(musterBindingEvents(logs, cp.started.Add(-unbindClockSlack)), session, deleteCalled)
}
