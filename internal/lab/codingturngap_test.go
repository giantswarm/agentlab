package lab

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestJudgeTurnGap pins the turn gap stage's verdict on the heartbeat: ticks
// in its own turn, none between the turns, ticks again in the next; in a
// workspace Session a call of the first turn answered 200.
func TestJudgeTurnGap(t *testing.T) {
	ended := time.Date(2026, 10, 19, 17, 47, 8, 0, time.UTC)
	gap := turnGap{ended: ended, next: ended.Add(codingTurnGap)}
	tick := func(offset time.Duration, status string) string {
		return fmt.Sprintf("%d:%s", ended.Add(offset).Unix(), status)
	}
	frozen := []string{tick(-6*time.Second, "200"), tick(-3*time.Second, "200"), tick(5*time.Second, "200"), tick(codingTurnGap+time.Second, "200"), tick(codingTurnGap+4*time.Second, "200")}
	for name, tc := range map[string]struct {
		ticks       []string
		inWorkspace bool
		want        string
	}{
		"frozen between turns":            {frozen, true, ""},
		"frozen, outside a workspace":     {[]string{tick(-3*time.Second, "401"), tick(codingTurnGap, "401")}, false, ""},
		"ran in the gap":                  {append(frozen, tick(time.Minute, "200")), true, "ticked 1 times in the"},
		"never ran":                       {nil, true, "never ticked in the turn that started it"},
		"gone after the gap":              {frozen[:3], true, "not again in the next turn"},
		"muster refused in the workspace": {[]string{tick(-3*time.Second, "401"), tick(codingTurnGap, "401")}, true, "none of the heartbeat's 1 calls"},
	} {
		t.Run(name, func(t *testing.T) {
			ticks, err := parseHeartbeat(strings.Join(tc.ticks, " "))
			if err != nil {
				t.Fatalf("parseHeartbeat: %v", err)
			}
			verdict, err := judgeTurnGap(ticks, gap, tc.inWorkspace)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("judgeTurnGap: %v", err)
				}
				if !strings.Contains(verdict, "not once in the") {
					t.Errorf("the verdict %q does not word the gap", verdict)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("judgeTurnGap = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestParseHeartbeatRefusesAForeignLine(t *testing.T) {
	if _, err := parseHeartbeat("1760896028:200 cat: can't open"); err == nil {
		t.Error("parseHeartbeat accepted a line that is no tick")
	}
}

// TestJudgeMusterAroundTurns pins the verdicts on muster's log over the gap
// (no call of the actor at all) and after delete_session (no call of it
// accepted; refusals counted).
func TestJudgeMusterAroundTurns(t *testing.T) {
	const (
		actor   = "spiffe://substrate-actor.local/actor/kagent/session-s1"
		other   = "spiffe://substrate-actor.local/actor/kagent/session-s10"
		session = "kagent/s1"
	)
	ended := time.Date(2026, 10, 19, 17, 47, 8, 0, time.UTC)
	gap := turnGap{ended: ended, next: ended.Add(codingTurnGap)}
	audit := func(at time.Time, outcome, who string) string {
		return fmt.Sprintf(`{"time":%q,"level":"INFO","msg":%q,"subsystem":"AUDIT"}`, at.Format(time.RFC3339Nano),
			"[AUDIT] action=actor_bearer outcome="+outcome+" subject=CiQwOGE4... target=muster details=actor="+who+" session="+session)
	}
	inTurn := audit(ended.Add(-time.Second), "success", actor)
	inGap := audit(ended.Add(time.Minute), "success", actor)
	if _, err := judgeGapAudit(strings.Join([]string{inTurn, audit(ended.Add(time.Minute), "success", other), audit(gap.next, "success", actor)}, "\n"), actor, gap); err != nil {
		t.Errorf("judgeGapAudit on calls in the turns and another actor's in the gap: %v", err)
	}
	if _, err := judgeGapAudit(inTurn+"\n"+inGap, actor, gap); err == nil || !strings.Contains(err.Error(), "outside a turn") {
		t.Errorf("judgeGapAudit on a call in the gap = %v, want a call outside a turn", err)
	}

	const sawNone = "saw none"
	deleted := ended.Add(5 * time.Minute)
	until := deleted.Add(codingAfterDeleteWatch)
	for name, tc := range map[string]struct {
		lines []string
		want  string
		fails bool
	}{
		"no call after":              {[]string{inTurn}, sawNone, false},
		"refused after":              {[]string{inTurn, strings.Replace(audit(deleted.Add(5*time.Second), "failure", actor), "session="+session, "error=actor is not bound", 1)}, "refused all 1 calls", false},
		"accepted after":             {[]string{audit(deleted.Add(5*time.Second), "success", actor)}, "accepted 1 actor-JWT calls", true},
		"accepted before the unbind": {[]string{audit(deleted.Add(-time.Second), "success", actor)}, sawNone, false},
		"another actor after":        {[]string{audit(deleted.Add(5*time.Second), "success", other)}, sawNone, false},
	} {
		t.Run(name, func(t *testing.T) {
			verdict, err := judgeAfterDelete(strings.Join(tc.lines, "\n"), actor, deleted, until)
			got := verdict
			if err != nil {
				got = err.Error()
			}
			if (err != nil) != tc.fails || !strings.Contains(got, tc.want) {
				t.Errorf("judgeAfterDelete = %q, %v; want %q (failing %v)", verdict, err, tc.want, tc.fails)
			}
		})
	}
}
