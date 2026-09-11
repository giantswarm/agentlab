package lab

import (
	"testing"
	"time"
)

// TestMusterCallsIn: muster's security audit and tools/call lines are
// counted for the person from the instant on — the email on the accepted
// token, the subject's prefix on the call; older lines and other people's
// are not.
func TestMusterCallsIn(t *testing.T) {
	since := time.Date(2026, 9, 11, 17, 0, 0, 0, time.UTC)
	logs := `{"time":"2026-09-11T16:59:59Z","level":"INFO","msg":"tools/call request","subject":"CiRjNGNl...","tool":"call_tool"}
{"time":"2026-09-11T17:01:45.454362876Z","level":"INFO","msg":"security_audit","audit":{"event_type":"forwarded_id_token_accepted","details":{"email":"admin@lab.local"}}}
{"time":"2026-09-11T17:01:45.454951896Z","level":"INFO","msg":"tools/call request","subsystem":"MCP-Protocol","subject":"CiRjNGNl...","tool":"call_tool"}
{"time":"2026-09-11T17:01:45.460875333Z","level":"INFO","msg":"tool call","tool":"call_tool","outcome":"ok"}
{"time":"2026-09-11T17:01:46Z","level":"INFO","msg":"tools/call request","subject":"CiRvdGhlcg...","tool":"call_tool"}
{"time":"2026-09-11T17:01:47Z","level":"INFO","msg":"security_audit","audit":{"event_type":"forwarded_id_token_accepted","details":{"email":"dev@lab.local"}}}
{"time":"2026-09-11T17:01:48Z","level":"INFO","msg":"tools/call request","subject":"CiRjNGNl...","tool":"list_tools"}
garbage line
`
	got := musterCallsIn(logs, since, "admin@lab.local", "CiRjNGNlNWQ0ZC0xMjM0EgVsb2NhbA")
	if got.accepted != 1 || got.calls != 1 {
		t.Errorf("counted %+v, want 1 accepted and 1 call", got)
	}
	if !subjectMatches("CiRjNGNl...", "CiRjNGNlNWQ0") || subjectMatches("CiRvdGhlcg...", "CiRjNGNlNWQ0") || subjectMatches("", "x") || subjectMatches("x", "") {
		t.Error("subjectMatches compares the logged prefix with the token's subject")
	}
}

// TestAncestorConditions: the conditions of every parent or ancestor fold
// into type → status.
func TestAncestorConditions(t *testing.T) {
	got := ancestorConditions([]any{
		map[string]any{fieldConditions: []any{map[string]any{fieldType: conditionAccepted, fieldStatus: conditionTrue}, map[string]any{fieldType: conditionResolvedRefs, fieldStatus: conditionTrue}}},
		map[string]any{fieldConditions: []any{map[string]any{fieldType: "Attached", fieldStatus: conditionTrue}}},
	})
	if got[conditionAccepted] != conditionTrue || got[conditionResolvedRefs] != conditionTrue || got["Attached"] != conditionTrue || len(got) != 3 {
		t.Errorf("conditions = %v", got)
	}
	if got := ancestorConditions(nil); len(got) != 0 {
		t.Errorf("no ancestors = %v", got)
	}
}
