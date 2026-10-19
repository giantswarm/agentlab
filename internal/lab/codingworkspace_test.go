package lab

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestJudgeActorBearerAudit pins the mcp call stage's verdict on muster's
// log in a workspace Session: it passes only on an accepted actor-JWT audit
// event of the actor muster logged bound to the Session, naming that
// binding's person and Session.
func TestJudgeActorBearerAudit(t *testing.T) {
	const (
		session = "kagent/s1"
		actor   = "spiffe://substrate-actor.local/actor/kagent/session-s1"
		other   = "spiffe://substrate-actor.local/actor/kagent/session-s10"
		admin   = "CiQwOGE4Njg0Yi1kYjg4LTRiNzMtOTBhOS0zY2QxNjYxYmUzNjYSBWxvY2Fs"
	)
	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	bound := fmt.Sprintf(`{"time":%q,"level":"INFO","msg":%q,"subsystem":"CredentialProvider","sub":%q,"session":%q,"actor":%q}`,
		start.Add(time.Second).Format(time.RFC3339Nano), musterSessionBound, admin, session, actor)
	audit := func(at time.Time, msg string) string {
		return fmt.Sprintf(`{"time":%q,"level":"INFO","msg":%q,"subsystem":"AUDIT"}`, at.Format(time.RFC3339Nano), msg)
	}
	at := start.Add(time.Minute)
	accepted := audit(at, "[AUDIT] action=actor_bearer outcome=success subject=CiQwOGE4... target=muster details=actor="+actor+" session="+session)
	for name, tc := range map[string]struct {
		lines []string
		want  string
	}{
		"accepted as the bound person": {[]string{bound, `{"time":"x","msg":"tools/call request"}`, accepted}, ""},
		"another actor accepted": {[]string{bound,
			audit(at, "[AUDIT] action=actor_bearer outcome=success subject=CiQwOGE4... target=muster details=actor="+other+" session=kagent/s10")}, "audited no actor-JWT request of the actor " + actor},
		"refused for the binding": {[]string{bound,
			audit(at, "[AUDIT] action=actor_bearer outcome=failure subject=CiQwOGE4... target=muster details=actor="+actor+" session="+session+" error=the binding does not list muster")}, "not as accepted for its person"},
		"accepted as someone else": {[]string{bound,
			audit(at, "[AUDIT] action=actor_bearer outcome=success subject=someone- target=muster details=actor="+actor+" session="+session)}, "not as accepted for its person"},
		"audited before the start": {[]string{bound, audit(start.Add(-time.Hour), "[AUDIT] action=actor_bearer outcome=success subject=CiQwOGE4... target=muster details=actor="+actor+" session="+session)}, "logged no actor-JWT audit event"},
		"no audit at all":          {[]string{bound}, "muster before 5.47.0"},
		"never bound":              {[]string{accepted}, "no session_bound for the Session " + session},
	} {
		t.Run(name, func(t *testing.T) {
			verdict, err := judgeActorBearerAudit(strings.Join(tc.lines, "\n"), session, start)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("judgeActorBearerAudit: %v", err)
				}
				if !strings.Contains(verdict, "CiQwOGE4...") {
					t.Errorf("the verdict %q names no person", verdict)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("judgeActorBearerAudit = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
