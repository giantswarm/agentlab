package lab

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// coding-test's no turn stage: a workspace Session's actor resumed with no
// turn running, as a runtime that keeps its sandbox running between turns
// would leave it. kagent keeps muster's host off the actor's egress policy
// outside a turn, so the egress gateway refuses the heartbeat's calls there
// (403, before muster sees them) and muster audits none; the next turn
// opens the host again and its calls are answered 200.
//
// The proof waits for kagent to suspend the actor after the turn gap's
// reading turn, reads the actor's egress policy (no rule on muster's host),
// resumes the actor through ate-api, lets the heartbeat tick for
// codingNoTurnWindow, suspends the actor again and reads the heartbeat in
// the next turn.

const (
	// codingNoTurnWindow is how long the actor runs with no turn: the
	// heartbeat ticks about five times in it.
	codingNoTurnWindow = 15 * time.Second
	// codingNoTurnSuspendWait bounds the wait for kagent to suspend the
	// actor after a turn.
	codingNoTurnSuspendWait = 2 * time.Minute
)

// noTurnWindow is when the actor ran with no turn: from its resume to its
// suspend, and the next turn's send.
type noTurnWindow struct {
	resumed, suspended, next time.Time
}

// judgeNoTurn is the verdict on the heartbeat's ticks: some in the window,
// every one of them refused 403 at the egress gateway, and one answered 200
// in the next turn.
//
// A tick carries its whole second, read off the node's clock, which is the
// host's: a tick of second S ran in [S, S+1). The window holds the seconds
// from the resume's up to the suspend's, the next turn those after the
// send's; a tick of the suspend's or the send's second may be either, so it
// is judged in neither, never as a call with no turn that the next turn made.
func judgeNoTurn(ticks []heartbeatTick, w noTurnWindow) (string, error) {
	var in, after []heartbeatTick
	resumed, suspended, next := w.resumed.Truncate(time.Second), w.suspended.Truncate(time.Second), w.next.Truncate(time.Second)
	for _, t := range ticks {
		switch {
		case t.at.Before(resumed):
		case t.at.Before(suspended):
			in = append(in, t)
		case t.at.After(next):
			after = append(after, t)
		}
	}
	window := w.suspended.Sub(w.resumed).Round(time.Second)
	switch {
	case len(in) == 0:
		return "", fmt.Errorf("the heartbeat never ticked in the %s the actor ran with no turn: the resumed sandbox ran nothing, so the refusal is unproven", window)
	case slices.ContainsFunc(in, func(t heartbeatTick) bool { return t.status != "403" }):
		return "", fmt.Errorf("the heartbeat's %d calls to muster in the %s with no turn were answered %s, wanted 403 from the egress gateway for every one: muster's host is on the actor's egress policy outside a turn", len(in), window, tickStatuses(in))
	case !slices.ContainsFunc(after, func(t heartbeatTick) bool { return t.status == "200" }):
		return "", fmt.Errorf("muster answered none of the heartbeat's %d calls in the next turn 200 (%s): the turn did not open muster's host again", len(after), tickStatuses(after))
	}
	return fmt.Sprintf("the heartbeat's %d calls in the %s the actor ran with no turn refused at the egress gateway (%s); in the next turn muster %s",
		len(in), window, tickStatuses(in), tickStatuses(after)), nil
}

// policyAllowsHost reports whether an egress policy has an HTTP or HTTPS
// rule naming host.
func policyAllowsHost(policy *ateapi.EgressPolicy, host string) bool {
	return slices.ContainsFunc(policy.GetRules(), func(rule *ateapi.EgressRule) bool {
		return slices.Contains(rule.GetHttp().GetHostnames(), host) || slices.Contains(rule.GetHttps().GetHostnames(), host)
	})
}

// awaitActorState waits until the actor is in state, bounded by wait.
func awaitActorState(ctx context.Context, api *ateAPI, name string, state ateapi.ActorState, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		actor, err := api.GetActor(ctx, &ateapi.GetActorRequest{Actor: &ateapi.ObjectRef{Atespace: kagentNamespace, Name: name}})
		if err != nil {
			return fmt.Errorf("GetActor %s: %w", name, err)
		}
		if actor.GetStatus().GetState() == state {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the actor %s is %s after %s, wanted %s", name, strings.TrimPrefix(actor.GetStatus().GetState().String(), "ACTOR_STATE_"), wait, strings.TrimPrefix(state.String(), "ACTOR_STATE_"))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// proveNoTurn is the no turn stage on the workspace Session whose heartbeat
// the turn gap started.
func (p *codingProof) proveNoTurn(ctx context.Context, session *apiv1alpha1.Session) error {
	u, err := url.Parse(musterInClusterURL)
	if err != nil {
		return err
	}
	host := u.Hostname()
	api, err := dialAteAPI(ctx)
	if err != nil {
		return err
	}
	defer api.Close()
	name := substrateActorName(session.GetId())
	actor := &ateapi.ObjectRef{Atespace: kagentNamespace, Name: name}
	if err := awaitActorState(ctx, api, name, ateapi.ActorState_ACTOR_STATE_SUSPENDED, codingNoTurnSuspendWait); err != nil {
		return fmt.Errorf("kagent did not suspend the actor after the turn: %w (%s)", err, p.line)
	}
	policy, err := api.GetActorEgressPolicy(ctx, &ateapi.GetActorEgressPolicyRequest{Actor: actor})
	if err != nil {
		return fmt.Errorf("GetActorEgressPolicy %s: %w", name, err)
	}
	if policyAllowsHost(policy, host) {
		return fmt.Errorf("the suspended actor %s's egress policy (version %d) allows %s with no turn running: kagent keeps a Session's actor-JWT hosts off it between turns (%s)", name, policy.GetMetadata().GetVersion(), host, p.line)
	}
	note("between turns the actor %s's egress policy (version %d, %d rules) has no rule on %s", name, policy.GetMetadata().GetVersion(), len(policy.GetRules()), host)

	var w noTurnWindow
	w.resumed = time.Now()
	if _, err := api.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: actor}); err != nil {
		return fmt.Errorf("ResumeActor %s with no turn: %w", name, err)
	}
	note("resumed the actor %s through ate-api with no turn; letting the heartbeat run for %s", name, codingNoTurnWindow)
	time.Sleep(codingNoTurnWindow)
	if _, err := api.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: actor}); err != nil {
		return fmt.Errorf("SuspendActor %s after the window with no turn: %w", name, err)
	}
	w.suspended = time.Now()
	w.next = time.Now()
	read, err := p.turn(ctx, session, codingHeartbeatReadPrompt(p.rt))
	if err != nil {
		return err
	}
	ticks, err := parseHeartbeat(read.probe["heartbeat"])
	if err != nil {
		return err
	}
	verdict, err := judgeNoTurn(ticks, w)
	if err != nil {
		return fmt.Errorf("%w (%s)", err, p.line)
	}
	note("%s", verdict)
	logs, err := p.rig.musterLog()
	if err != nil {
		return err
	}
	bound, err := boundActor(logs, kagentNamespace+"/"+session.GetId(), p.rig.started.Add(-unbindClockSlack))
	if err != nil {
		return err
	}
	// The audit window is the resume to the suspend: the next turn is sent
	// at once after it, and its MCP session's first calls reach muster
	// within the clock slack.
	verdict, err = judgeGapAudit(logs, bound, turnGap{ended: w.resumed.Add(-codingTurnSettle), next: w.suspended})
	if err != nil {
		return fmt.Errorf("%w (%s)", err, p.line)
	}
	note("%s with no turn running", verdict)
	return nil
}
