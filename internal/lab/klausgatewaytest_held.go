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

// gatewayKeepsHeld reports whether the gateway the log names keeps held state
// across a restart.
func gatewayKeepsHeld(described string) bool {
	return gatewayAtLeast(described, klausGatewayHeldSince)
}

// gatewayAtLeast reports whether the gateway the log names (gatewayVersion's
// "klaus-gateway <version> (<sha>)") is since or newer. Only a release or a
// release candidate is compared; anything else (a dev build of a branch, a
// version that does not parse) is the build under proof.
func gatewayAtLeast(described, since string) bool {
	fields := strings.Fields(described)
	if len(fields) < 2 {
		return true
	}
	v, err := mastersemver.NewVersion(strings.TrimPrefix(fields[1], "v"))
	if err != nil || (v.Prerelease() != "" && !strings.HasPrefix(v.Prerelease(), "rc.")) {
		return true
	}
	return !v.LessThan(mastersemver.MustParse(since))
}

// restartMidSignIn has user, who has no link, ask in owner's conversation —
// a person with no link opens none, the picker lists the agents with their
// permissions — so the gateway parks the message and prompts them to sign
// in. Once the thread shows the prompt the gateway stops, the link a
// completed sign-in leaves is written to its link store, and the gateway
// starts again. The parked message must come back without being sent again:
// the restarted gateway reads it back from its routing store and replays it
// for the now linked user, which, a newcomer's, asks the owner to allow them;
// Allow runs it in the thread.
func (p *slackProof) restartMidSignIn(gw *gatewayProcess, linksPath string, key []byte, owner *slackThread, user string, link *musterlink.Link) (*slackTurn, error) {
	shown := len(p.fake.thread(p.channel, owner.ts))
	if _, err := p.driver.mention(user, owner.ts, klausGatewayWordPrompt); err != nil {
		return nil, err
	}
	if _, _, err := p.awaitSignIn(user, owner.ts, shown); err != nil {
		return nil, err
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
	var consent slackMessage
	msgs, ok := waitThread(p.fake, p.channel, owner.ts, klausGatewayReplyWait, func(msgs []slackMessage) bool {
		for _, m := range msgs[min(shown, len(msgs)):] {
			if _, found := m.action(slackAccessAllow); found && m.Recipient == owner.user {
				consent = m
				return true
			}
		}
		return false
	})
	if !ok {
		return nil, fmt.Errorf("the message parked before the restart was not replayed: %s got no access prompt for %s within %s; the thread shows: %s", owner.user, user, klausGatewayReplyWait, threadLine(msgs))
	}
	before := len(p.fake.thread(p.channel, owner.ts))
	if err := p.driver.click(owner.user, consent, slackAccessAllow); err != nil {
		return nil, err
	}
	turn, err := p.awaitTurn(owner, before)
	if err != nil {
		return nil, fmt.Errorf("the replayed message after Allow: %w", err)
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
