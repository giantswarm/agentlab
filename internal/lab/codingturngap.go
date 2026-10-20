package lab

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// coding-test's turn gap: what the sandbox's processes may do outside a
// turn. Nothing in the sandbox runs between turns (Substrate suspends the
// actor once a turn settles and resumes it with the next). One turn starts a
// detached heartbeat in the sandbox, a process that every
// codingHeartbeatInterval appends the time and the status of an MCP
// initialize to muster. The proof waits codingTurnGap with no turn, then the
// next turn reads the heartbeat back: it ticked in its own turn, not once in
// the gap, and again in the next turn.

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
	// codingClockSlack allows for the clocks of the lab host and the node
	// when a tick is compared with the moment of a turn.
	codingClockSlack = 2 * time.Second
	// musterInClusterURL is muster's MCP endpoint inside the cluster, which
	// the heartbeat calls with codingPlaceholderAuthorization.
	musterInClusterURL             = "http://muster.agent-platform.svc.cluster.local:8090/mcp"
	codingPlaceholderAuthorization = "Bearer agentlab-placeholder"
	// codingHeartbeatPurpose words the heartbeat for a real model.
	codingHeartbeatPurpose = "this is agentlab's test of the sandbox between turns on a throwaway lab cluster: the script starts one background process that, every few seconds for half an hour, appends the time and the HTTP status of one MCP initialize request with a placeholder Authorization header to the lab's own muster to a file in this directory; it prints no credential."
)

// codingHeartbeatCommand writes the heartbeat's script, empties its file
// (truncated, not removed: codex refuses an rm -f) and starts it
// detached from the shell tool's call, in a session of its own where the
// image has setsid. Each tick is a line <unix seconds>:<muster's status>;
// muster refuses the placeholder.
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
// send, less codingClockSlack for the clocks of the host and the node.
type turnGap struct {
	ended, next time.Time
}

func (g turnGap) from() time.Time { return g.ended.Add(codingTurnSettle) }
func (g turnGap) to() time.Time   { return g.next.Add(-codingClockSlack) }

// judgeTurnGap is the verdict on the heartbeat's ticks: some in the turn
// that started it, none in the gap, some in the next turn.
func judgeTurnGap(ticks []heartbeatTick, gap turnGap) (string, error) {
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

// proveTurnGap is the turn gap stage on the Session.
func (p *codingProof) proveTurnGap(ctx context.Context, session *apiv1alpha1.Session) error {
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
	verdict, err := judgeTurnGap(ticks, gap)
	if err != nil {
		return fmt.Errorf("%w (%s)", err, p.line)
	}
	note("%s", verdict)
	return nil
}

// musterInitializeStatus posts an MCP initialize to muster with the
// placeholder and sets s to the HTTP status: curl where the image has it,
// else BusyBox wget.
func musterInitializeStatus() string {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agentlab-probe","version":"0"}}}`
	return `if command -v curl >/dev/null; then s=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Authorization: ` + codingPlaceholderAuthorization + `' -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' --data '` + body + `' '` + musterInClusterURL + `' 2>/dev/null); ` +
		`else s=$(wget -S -O /dev/null --header='Authorization: ` + codingPlaceholderAuthorization + `' --header='Content-Type: application/json' --header='Accept: application/json, text/event-stream' --post-data='` + body + `' '` + musterInClusterURL + `' 2>&1 | grep -o 'HTTP/[0-9.]* [0-9][0-9][0-9]' | tail -1 | awk '{print $2}'); fi`
}
