package lab

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestJudgeUnbound pins the delete_session stage's verdict on muster's log:
// it passes only when the actor muster logged bound at the Session's start
// is logged unbound by the same person after the delete.
func TestJudgeUnbound(t *testing.T) {
	const (
		session = "kagent/s1"
		actor   = "spiffe://substrate-actor.local/actor/kagent/session-s1"
		admin   = "CiQwOGE4Njg0Yi1kYjg4LTRiNzMtOTBhOS0zY2QxNjYxYmUzNjYSBWxvY2Fs"
	)
	start := time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC)
	deleted := start.Add(time.Minute)
	line := func(at time.Time, msg, sub, session, actor string) string {
		return fmt.Sprintf(`{"time":%q,"level":"INFO","msg":%q,"subsystem":"CredentialProvider","sub":%q,"session":%q,"actor":%q}`,
			at.Format(time.RFC3339Nano), msg, sub, session, actor)
	}
	bound := line(start.Add(time.Second), musterSessionBound, admin, session, actor)
	other := line(start.Add(2*time.Second), musterSessionBound, admin, "kagent/s2", "spiffe://substrate-actor.local/actor/kagent/session-s2")
	unbound := func(at time.Time, sub, actor string) string {
		return line(at, musterSessionUnbound, sub, "", actor)
	}
	for name, tc := range map[string]struct {
		lines []string
		want  string
	}{
		"bound, then unbound after the delete": {[]string{bound, other, `{"time":"x","msg":"tools/call request"}`, unbound(deleted.Add(time.Second), admin, actor)}, ""},
		"unbound within the clocks' slack":     {[]string{bound, unbound(deleted.Add(-time.Second), admin, actor)}, ""},
		"never unbound":                        {[]string{bound, other}, "no session_unbound for the actor " + actor},
		"another actor unbound":                {[]string{bound, other, unbound(deleted.Add(time.Second), admin, "spiffe://substrate-actor.local/actor/kagent/session-s2")}, "no session_unbound"},
		"unbound before the delete":            {[]string{bound, unbound(deleted.Add(-30*time.Second), admin, actor)}, "no session_unbound"},
		"unbound by another person":            {[]string{bound, unbound(deleted.Add(time.Second), "someone-else", actor)}, `unbound by "someone-else"`},
		"never bound":                          {[]string{other, unbound(deleted.Add(time.Second), admin, actor)}, "no session_bound for the Session " + session},
		"bound before the window":              {[]string{line(start.Add(-time.Minute), musterSessionBound, admin, session, actor), unbound(deleted.Add(time.Second), admin, actor)}, "no session_bound"},
	} {
		events := musterBindingEvents("muster-7d9c 2026 "+strings.Join(tc.lines, "\nmuster-7d9c 2026 "), start)
		got, err := judgeUnbound(events, session, deleted)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want == "" && got.Actor != actor:
			t.Errorf("%s: bound actor %q, want %q", name, got.Actor, actor)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
