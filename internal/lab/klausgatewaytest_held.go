package lab

import (
	"fmt"
	"strings"

	mastersemver "github.com/Masterminds/semver/v3"
	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
)

// klausGatewayHeldSince is the first klaus-gateway that keeps what a person
// waits on across a restart — a message parked while its sender signs in, a
// paused approval — in the thread's row of its routing store.
const klausGatewayHeldSince = "4.1.0-rc.15"

// gatewayKeepsHeld reports whether the gateway the log names (gatewayVersion's
// "klaus-gateway <version> (<sha>)") keeps held state across a restart. Only
// a release or a release candidate is compared; anything else (a dev build of
// a branch, a version that does not parse) is the build under proof.
func gatewayKeepsHeld(described string) bool {
	fields := strings.Fields(described)
	if len(fields) < 2 {
		return true
	}
	v, err := mastersemver.NewVersion(strings.TrimPrefix(fields[1], "v"))
	if err != nil || (v.Prerelease() != "" && !strings.HasPrefix(v.Prerelease(), "rc.")) {
		return true
	}
	return !v.LessThan(mastersemver.MustParse(klausGatewayHeldSince))
}

// restartMidSignIn has user, who has no link, open a thread; once the thread
// shows the sign-in prompt the gateway stops, the link a completed sign-in
// leaves is written to its link store, and the gateway starts again. The
// parked message must be answered without being sent again: the restarted
// gateway read it back from its routing store and replayed it for the now
// linked user.
func (p *slackProof) restartMidSignIn(gw *gatewayProcess, linksPath string, key []byte, user string, link *musterlink.Link) (*slackTurn, error) {
	ts, err := p.driver.mention(user, "", klausGatewayWordPrompt)
	if err != nil {
		return nil, err
	}
	msgs, ok := waitThread(p.fake, p.channel, ts, klausGatewayReplyWait, func(msgs []slackMessage) bool {
		for _, m := range msgs {
			if _, found := m.action(slackActionSignIn); found && (m.Recipient == user || m.Method == slackPostMessage) {
				return true
			}
		}
		return false
	})
	if !ok {
		return nil, fmt.Errorf("the unlinked %s got no sign-in prompt within %s; the thread shows: %s", user, klausGatewayReplyWait, threadLine(msgs))
	}
	if err := gw.stop(); err != nil {
		return nil, err
	}
	if err := seedBoltLink(linksPath, key, user, link); err != nil {
		return nil, err
	}
	if err := gw.start(); err != nil {
		return nil, fmt.Errorf("restarting the gateway: %w", err)
	}
	turn, err := p.awaitTurn(&slackThread{ts: ts, user: user}, len(msgs))
	if err != nil {
		return nil, fmt.Errorf("the message parked before the restart was not replayed: %w", err)
	}
	if err := assertSlackTurnSaid(turn, klausGatewayWord); err != nil {
		return nil, fmt.Errorf("the replayed message: %w", err)
	}
	return turn, nil
}

// restartMidApproval has user ask the tool-using question in a new thread;
// once the turn pauses on the approval card the gateway restarts, and
// Approve on the card the previous process posted must resume the paused
// task in place to completion.
func (p *slackProof) restartMidApproval(api *kagentAPI, gw *gatewayProcess, user string) (*decisionOutcome, error) {
	t := &slackThread{user: user}
	turn, err := p.firstTurn(t, klausGatewayToolPrompt)
	if err != nil {
		return nil, fmt.Errorf("the tool-using turn: %w", err)
	}
	if turn.record.Outcome != outcomeInputReq {
		return nil, fmt.Errorf("the tool-using turn did not pause for approval (%s)", turnFailure(turn))
	}
	session, err := p.boundSession(api, t)
	if err != nil {
		return nil, err
	}
	if err := gw.stop(); err != nil {
		return nil, err
	}
	if err := gw.start(); err != nil {
		return nil, fmt.Errorf("restarting the gateway: %w", err)
	}
	out, err := p.decideUntilSettled(api, session, t, turn, true)
	if err != nil {
		return nil, fmt.Errorf("approving after the restart: %w", err)
	}
	if out.finalState != a2a.TaskStateCompleted {
		return nil, fmt.Errorf("the task approved after the restart (%s) ended %s at the controller, not completed", out.taskID, out.finalState)
	}
	return out, nil
}
