package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// coding-test's turn boundaries: what the sandbox's processes may do outside
// a turn. The egress gateway sets the Actor's JWT on every request of a bound
// actor to muster and checks no turn; what keeps a sandbox from calling
// muster between turns is that nothing in it runs then (Substrate suspends
// the actor once a turn settles and resumes it with the next), and what
// keeps it from calling muster after delete_session is that the actor's
// binding is gone (muster refuses an unbound actor) along with the actor.
//
//   - turn gap: one turn starts a detached heartbeat in the sandbox, a
//     process that every codingHeartbeatInterval appends the time and the
//     status of an MCP initialize to muster. The
//     proof waits codingTurnGap with no turn, then the next turn reads the
//     heartbeat back: it ticked in its own turn, not once in the gap, and
//     again in the next turn. In a workspace Session muster accepted the
//     heartbeat's calls in the turn and audited no call of the actor in the
//     gap.
//   - after delete: the workspace Session deleted through agent-manager's
//     delete_session; muster logs the actor unbound, and in the
//     codingAfterDeleteWatch that follows audits no accepted call of it (a
//     call it sees is refused as unbound).

const (
	// codingTestHeartbeat is the heartbeat's file in the Session's
	// directory, codingTestHeartbeatScript its script.
	codingTestHeartbeat       = ".agentlab-heartbeat"
	codingTestHeartbeatScript = ".agentlab-heartbeat.sh"
	// codingHeartbeatInterval is the heartbeat's period, and
	// codingHeartbeatTicks bounds its run (about half an hour).
	codingHeartbeatInterval = 3 * time.Second
	codingHeartbeatTicks    = 600
	// codingTurnGap is the wait between the turn that starts the heartbeat
	// and the one that reads it: a running process ticks about thirty times
	// in it.
	codingTurnGap = 90 * time.Second
	// codingTurnSettle is how long after a turn's answer Substrate may take
	// to suspend the actor (the turn's quiescence and the checkpoint); ticks
	// in it count as the turn's.
	codingTurnSettle = 20 * time.Second
	// codingHeartbeatReadDelay is how long the reading turn's command waits
	// before it reads, so the resumed heartbeat ticks in that turn.
	codingHeartbeatReadDelay = 8 * time.Second
	// codingAfterDeleteWatch is how long muster's log is watched after
	// delete_session for a call of the deleted Session's actor.
	codingAfterDeleteWatch = 30 * time.Second
	// codingHeartbeatPurpose words the heartbeat for a real model.
	codingHeartbeatPurpose = "this is agentlab's test of the sandbox between turns on a throwaway lab cluster: the script starts one background process that, every few seconds for half an hour, appends the time and the HTTP status of one MCP initialize request with a placeholder Authorization header to the lab's own muster to a file in this directory; it prints no credential."
)

// codingHeartbeatCommand writes the heartbeat's script, empties its file
// (truncated, not removed: codex refuses an rm -f) and starts it
// detached from the shell tool's call, in a session of its own where the
// image has setsid. Each tick is a line <unix seconds>:<muster's status>;
// outside a workspace Session muster refuses the placeholder.
func codingHeartbeatCommand() string {
	return `cat > ` + codingTestHeartbeatScript + ` <<'AGENTLAB_EOF'
i=0
while [ "$i" -lt ` + strconv.Itoa(codingHeartbeatTicks) + ` ]; do
  t=$(date +%s)
  ` + musterInitializeStatus() + `
  echo "$t:${s:-none}" >> ` + codingTestHeartbeat + `
  i=$((i+1))
  sleep ` + strconv.Itoa(int(codingHeartbeatInterval/time.Second)) + `
done
AGENTLAB_EOF
: > ` + codingTestHeartbeat + `
if command -v setsid >/dev/null; then setsid sh ` + codingTestHeartbeatScript + ` </dev/null >/dev/null 2>&1 &
else nohup sh ` + codingTestHeartbeatScript + ` </dev/null >/dev/null 2>&1 &
fi
sleep ` + strconv.Itoa(int(2*codingHeartbeatInterval/time.Second)) + `
echo "` + probeMarker + ` heartbeat-started=$(wc -l < ` + codingTestHeartbeat + ` 2>/dev/null || echo 0)"`
}

// codingHeartbeatReadCommand waits for the resumed heartbeat to tick, then
// prints every tick on one line.
func codingHeartbeatReadCommand() string {
	return "sleep " + strconv.Itoa(int(codingHeartbeatReadDelay/time.Second)) + "; echo \"" + probeMarker + " heartbeat=$(tr '\\n' ' ' < " + codingTestHeartbeat + " 2>/dev/null)\""
}

// codingHeartbeatPrompt starts the heartbeat; codingHeartbeatReadPrompt
// reads it back. Their openings are what the mockllm's script matches.
func codingHeartbeatPrompt(rt codingRuntime) string {
	return "Start this test lab's heartbeat: run this exact script once with your " + rt.shellTool + " (" + codingHeartbeatPurpose + ") and reply with its " + probeMarker + " line, verbatim:\n\n" + codingHeartbeatCommand()
}

func codingHeartbeatReadPrompt(rt codingRuntime) string {
	return "Read this test lab's heartbeat: run this exact command once with your " + rt.shellTool + ": `" + codingHeartbeatReadCommand() + "` and reply with its " + probeMarker + " line, verbatim."
}

// heartbeatTick is one line of the heartbeat: when it ran and muster's
// answer to its call.
type heartbeatTick struct {
	at     time.Time
	status string
}

// parseHeartbeat reads the heartbeat reading: <unix seconds>:<status>
// tokens, space-separated.
func parseHeartbeat(reading string) ([]heartbeatTick, error) {
	var ticks []heartbeatTick
	for _, field := range strings.Fields(reading) {
		at, status, _ := strings.Cut(field, ":")
		sec, err := strconv.ParseInt(at, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("the heartbeat's line %q is no <unix seconds>:<status>", field)
		}
		ticks = append(ticks, heartbeatTick{at: time.Unix(sec, 0), status: status})
	}
	return ticks, nil
}

// turnGap is the window between two turns that the sandbox must not run in:
// from the first turn's answer plus codingTurnSettle to the second turn's
// send, less unbindClockSlack for the clocks of the host and the node.
type turnGap struct {
	ended, next time.Time
}

func (g turnGap) from() time.Time { return g.ended.Add(codingTurnSettle) }
func (g turnGap) to() time.Time   { return g.next.Add(-unbindClockSlack) }

// judgeTurnGap is the verdict on the heartbeat's ticks: some in the turn
// that started it, none in the gap, some in the next turn; in a workspace
// Session (wantAccepted) muster answered a call of the first turn 200.
func judgeTurnGap(ticks []heartbeatTick, gap turnGap, wantAccepted bool) (string, error) {
	var before, in, after []heartbeatTick
	for _, t := range ticks {
		switch {
		case !t.at.After(gap.from()):
			before = append(before, t)
		case t.at.Before(gap.to()):
			in = append(in, t)
		default:
			after = append(after, t)
		}
	}
	window := gap.to().Sub(gap.from()).Round(time.Second)
	switch {
	case len(before) == 0:
		return "", fmt.Errorf("the heartbeat never ticked in the turn that started it (%d ticks read): the detached process did not run", len(ticks))
	case len(in) > 0:
		return "", fmt.Errorf("the heartbeat ticked %d times in the %s between the turns, first at %s: a process in the sandbox runs outside a turn (Substrate did not suspend the actor once the turn settled)", len(in), window, in[0].at.UTC().Format(time.RFC3339))
	case len(after) == 0:
		return "", fmt.Errorf("the heartbeat ticked %d times in its turn, last at %s, and not again in the next turn: the process did not survive the gap, so the freeze is unproven", len(before), before[len(before)-1].at.UTC().Format(time.RFC3339))
	}
	if wantAccepted && !slices.ContainsFunc(before, func(t heartbeatTick) bool { return t.status == "200" }) {
		return "", fmt.Errorf("muster answered none of the heartbeat's %d calls in its turn 200 (%s): in a workspace Session the egress gateway sets the Actor's JWT on muster's host", len(before), tickStatuses(before))
	}
	return fmt.Sprintf("the heartbeat ticked %d times in its turn (muster %s), last at %s; not once in the %s between the turns; %d times in the next turn from %s",
		len(before), tickStatuses(before), before[len(before)-1].at.UTC().Format(time.RFC3339), window, len(after), after[0].at.UTC().Format(time.RFC3339)), nil
}

// tickStatuses counts the ticks by muster's answer.
func tickStatuses(ticks []heartbeatTick) string {
	counts := map[string]int{}
	for _, t := range ticks {
		counts[t.status]++
	}
	var parts []string
	for _, status := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%s×%d", status, counts[status]))
	}
	return strings.Join(parts, " ")
}

// actorBearerAudits are muster's actor-JWT audit lines of the actor in
// [from, to): those that accepted it and those that refused it.
func actorBearerAudits(logs, actor string, from, to time.Time) (accepted, refused []string) {
	for _, raw := range strings.Split(logs, "\n") {
		start := strings.IndexByte(raw, '{')
		if start < 0 {
			continue
		}
		var line musterLogLine
		if json.Unmarshal([]byte(raw[start:]), &line) != nil || line.Time.Before(from) || !line.Time.Before(to) ||
			!strings.HasPrefix(line.Msg, actorBearerAuditPrefix) {
			continue
		}
		if !strings.Contains(line.Msg, "actor="+actor+" ") && !strings.HasSuffix(line.Msg, "actor="+actor) {
			continue
		}
		if strings.Contains(line.Msg, "outcome=success") {
			accepted = append(accepted, line.Msg)
		} else {
			refused = append(refused, line.Msg)
		}
	}
	return accepted, refused
}

// judgeGapAudit is the verdict on muster's log over the gap: no call of the
// actor, accepted or refused.
func judgeGapAudit(logs, actor string, gap turnGap) (string, error) {
	accepted, refused := actorBearerAudits(logs, actor, gap.from(), gap.to())
	if n := len(accepted) + len(refused); n > 0 {
		return "", fmt.Errorf("muster audited %d actor-JWT calls of the actor %s between the turns (%d accepted): a call reached muster outside a turn: %s", n, actor, len(accepted), excerpt(strings.Join(append(accepted, refused...), " | "), 600))
	}
	return fmt.Sprintf("muster audited no call of the actor %s between the turns", actor), nil
}

// judgeAfterDelete is the verdict on muster's log from the actor's unbind
// on (delete_session settles the Session's runtime work first, and the
// calls of a turn still running then are the Session's): no accepted call
// of the deleted Session's actor; the refused ones are counted.
func judgeAfterDelete(logs, actor string, unbound, until time.Time) (string, error) {
	accepted, refused := actorBearerAudits(logs, actor, unbound, until)
	if len(accepted) > 0 {
		return "", fmt.Errorf("muster accepted %d actor-JWT calls of the actor %s after delete_session unbound it: %s", len(accepted), actor, excerpt(strings.Join(accepted, " | "), 600))
	}
	if len(refused) > 0 {
		return fmt.Sprintf("muster refused all %d calls of the unbound actor %s after delete_session: %s", len(refused), actor, excerpt(refused[0], 300)), nil
	}
	return fmt.Sprintf("muster accepted no call of the actor %s in the %s after delete_session unbound it, and saw none: its sandbox went with the Session", actor, until.Sub(unbound).Round(time.Second)), nil
}

// unboundAt is when muster logged the actor unbound after the delete was
// called.
func unboundAt(events []musterBindingEvent, actor string, deleteCalled time.Time) time.Time {
	for _, e := range events {
		if e.Msg == musterSessionUnbound && e.Actor == actor && !e.Time.Before(deleteCalled.Add(-unbindClockSlack)) {
			return e.Time
		}
	}
	return deleteCalled
}

// boundActor is the actor muster logged bound to the Session since the
// instant.
func boundActor(logs, session string, since time.Time) (string, error) {
	for _, e := range musterBindingEvents(logs, since) {
		if e.Msg == musterSessionBound && e.Session == session {
			return e.Actor, nil
		}
	}
	return "", fmt.Errorf("muster logged no %s for the Session %s", musterSessionBound, session)
}

// proveTurnGap is the turn gap stage on the Session.
func (p *codingProof) proveTurnGap(ctx context.Context, session *apiv1alpha1.Session) error {
	inWorkspace := p.rig != nil
	started, err := p.turn(ctx, session, codingHeartbeatPrompt(p.rt))
	if err != nil {
		return err
	}
	gap := turnGap{ended: time.Now()}
	note("heartbeat started (%s ticks read in its turn); no turn for %s", firstNonEmpty(started.probe["heartbeat-started"], "no"), codingTurnGap)
	time.Sleep(codingTurnGap)
	gap.next = time.Now()
	read, err := p.turn(ctx, session, codingHeartbeatReadPrompt(p.rt))
	if err != nil {
		return err
	}
	ticks, err := parseHeartbeat(read.probe["heartbeat"])
	if err != nil {
		return err
	}
	verdict, err := judgeTurnGap(ticks, gap, inWorkspace)
	if err != nil {
		return fmt.Errorf("%w (%s)", err, p.line)
	}
	note("%s", verdict)
	if !inWorkspace {
		return nil
	}
	logs, err := p.rig.musterLog()
	if err != nil {
		return err
	}
	actor, err := boundActor(logs, kagentNamespace+"/"+session.GetId(), p.rig.started.Add(-unbindClockSlack))
	if err != nil {
		return err
	}
	verdict, err = judgeGapAudit(logs, actor, gap)
	if err != nil {
		return fmt.Errorf("%w (%s)", err, p.line)
	}
	note("%s", verdict)
	return nil
}

// proveAfterDelete deletes the workspace Session through agent-manager's
// delete_session and judges muster's log: the actor unbound, no call of it
// accepted after.
func (p *codingProof) proveAfterDelete(ctx context.Context, session *apiv1alpha1.Session) error {
	id := session.GetId()
	called := time.Now()
	deleteCtx, cancel := context.WithTimeout(ctx, sessionDeleteTimeout)
	err := p.sessions.remove(deleteCtx, id)
	cancel()
	if err != nil {
		return err
	}
	name := kagentNamespace + "/" + id
	since := p.rig.started.Add(-unbindClockSlack)
	deadline := time.Now().Add(unbindLogWait)
	var bound musterBindingEvent
	var unbound time.Time
	for {
		logs, err := p.rig.musterLog()
		if err != nil {
			return err
		}
		events := musterBindingEvents(logs, since)
		if bound, err = judgeUnbound(events, name, called); err == nil {
			unbound = unboundAt(events, bound.Actor, called)
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w (%s)", err, p.line)
		}
		time.Sleep(3 * time.Second)
	}
	note("delete_session: muster logged the actor %s of %s unbound at %s; watching its log for %s", bound.Actor, id, unbound.UTC().Format(time.RFC3339), codingAfterDeleteWatch)
	time.Sleep(codingAfterDeleteWatch)
	logs, err := p.rig.musterLog()
	if err != nil {
		return err
	}
	verdict, err := judgeAfterDelete(logs, bound.Actor, unbound, time.Now())
	if err != nil {
		return fmt.Errorf("%w (%s)", err, p.line)
	}
	note("%s", verdict)
	return nil
}
