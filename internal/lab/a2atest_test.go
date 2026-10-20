package lab

import (
	"slices"
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

// TestMusterTaskCallsIn: a denied task is judged by its own tools — a
// meta-tool's request by name, call_tool by the tool it dispatches — so the
// person's other muster calls in the window (a portal tab's call_tool
// polls, another tool's dispatch) and other subjects' never count.
func TestMusterTaskCallsIn(t *testing.T) {
	const listTool = "x_kubernetes_list"
	tools, err := musterToolNames([]hitlTool{
		{ID: "denied-1", Name: fakeTool, Args: map[string]any{"pattern": "kubernetes"}},
		{ID: "denied-2", Name: musterMetaCallTool, Args: map[string]any{nameKey: listTool}},
		{ID: "denied-3", Name: fakeTool},
	})
	if err != nil || !slices.Equal(tools, []string{fakeTool, listTool}) {
		t.Fatalf("musterToolNames = %v, %v", tools, err)
	}
	if _, err := musterToolNames(nil); err == nil {
		t.Error("no requested tool must not judge the task clean")
	}
	if _, err := musterToolNames([]hitlTool{{ID: "denied-4", Name: musterMetaCallTool}}); err == nil {
		t.Error("a call_tool naming no tool must not judge the task clean")
	}

	since := time.Date(2026, 10, 20, 10, 28, 0, 0, time.UTC)
	const subject = "CiRjNGNlNWQ0ZC0xMjM0EgVsb2NhbA"
	portal := `{"time":"2026-10-20T10:28:38.229035738Z","msg":"tools/call request","transportSessionID":"","subject":"CiRjNGNl...","tool":"call_tool"}
{"time":"2026-10-20T10:28:38.231Z","msg":"tool dispatched","subject":"CiRjNGNl...","server":"agent-manager","tool":"x_agent-manager_list_agents","outcome":"ok"}
{"time":"2026-10-20T10:28:39Z","msg":"tools/call request","subject":"CiRvdGhlcg...","tool":"filter_tools"}
{"time":"2026-10-20T10:27:59Z","msg":"tools/call request","subject":"CiRjNGNl...","tool":"filter_tools"}
garbage line
`
	if got := musterTaskCallsIn(portal, since, subject, tools); len(got) != 0 {
		t.Errorf("the portal's polls, another subject's and older lines counted: %v", got)
	}
	ran := portal + `{"time":"2026-10-20T10:28:40Z","msg":"tools/call request","subject":"CiRjNGNl...","tool":"filter_tools"}
{"time":"2026-10-20T10:28:41Z","msg":"tools/call request","subject":"CiRjNGNl...","tool":"call_tool"}
{"time":"2026-10-20T10:28:41.1Z","msg":"tool dispatched","subject":"CiRjNGNl...","server":"agentlab-mcp-kubernetes","tool":"x_kubernetes_list","outcome":"ok"}
`
	if got := musterTaskCallsIn(ran, since, subject, tools); len(got) != 2 {
		t.Errorf("the task's filter_tools request and its x_kubernetes_list dispatch: %v", got)
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
