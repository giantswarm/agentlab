package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The test's person, the devctl documents' keys and a merge record.
const (
	testDevctlLogin    = "octo"
	testDevctlCommand  = "command"
	testDevctlRefused  = "refused"
	testDevctlTimeout  = "timeout"
	testDevctlExitCode = "exitCode"
	testDevctlGreen    = "green"
	testDevctlRed      = "red"
	testDevctlVerdict  = "verdict"
	// testDevctlCallTool is muster's call_tool as Claude Code names it.
	testDevctlCallTool = "mcp__muster__" + musterCallTool
	testDevctlMergedOK = "MERGED " + testDevctlLogin + " 88ca319d"
)

// The first turn passes with a person's push and pull request, and names
// what failed otherwise.
func TestJudgeDevctlPullRequest(t *testing.T) {
	ok := map[string]string{devctlProbeUser: rcSuccess + " " + testDevctlLogin + " User", devctlProbePush: rcSuccess, devctlProbePRCreate: rcSuccess + " https://github.com/giantswarm/devctl-mcp-proof/pull/7", devctlProbePR: "7 " + testDevctlLogin}
	login, number, err := judgeDevctlPullRequest(ok)
	if err != nil || login != testDevctlLogin || number != 7 {
		t.Fatalf("got %q #%d %v, want %s #7", login, number, err, testDevctlLogin)
	}
	for name, tc := range map[string]struct {
		key, value, want string
	}{
		"an App's bot":         {devctlProbeUser, "rc0 agentlab-agents[bot] Bot", "a person's login"},
		"no gh user":           {devctlProbeUser, "rc1 HTTP 401", "a person's login"},
		"push refused":         {devctlProbePush, "rc128 denied", "git push"},
		"no pull request":      {devctlProbePRCreate, "rc1 already exists", "gh pr create"},
		"another author":       {devctlProbePR, "7 someone", "want the person " + testDevctlLogin},
		"no number":            {devctlProbePR, "no pull requests found", "the number and the author"},
		"a number that is not": {devctlProbePR, "x " + testDevctlLogin, "the number and the author"},
	} {
		v := map[string]string{}
		for k, val := range ok {
			v[k] = val
		}
		v[tc.key] = tc.value
		if _, _, err := judgeDevctlPullRequest(v); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, tc.want)
		}
	}
}

// devctlDoc is a devctl document as call_tool's text, nested once more as a
// harness's tool result carries it.
func devctlDoc(t *testing.T, fields map[string]any) string {
	t.Helper()
	doc, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := json.Marshal(map[string]any{toolResultContent: []map[string]any{{fieldTypeKey: toolResultText, toolResultText: string(doc)}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(wrapped)
}

// The agent's calls are read from its tool results in order, however
// deep the document is quoted; other tools' results are not calls.
func TestDevctlCalls(t *testing.T) {
	tools := []harnessToolResult{
		{name: "Bash", output: probeMarker + " merged=" + testDevctlMergedOK},
		{name: testDevctlCallTool, output: devctlDoc(t, map[string]any{testDevctlCommand: "pr wait", testDevctlExitCode: 0, testDevctlVerdict: testDevctlGreen})},
		{name: testDevctlCallTool, output: devctlDoc(t, map[string]any{testDevctlCommand: "pr merge", testDevctlExitCode: 0, testDevctlVerdict: "merged", "mergedBy": testDevctlLogin})},
		{name: testDevctlCallTool, output: devctlDoc(t, map[string]any{testDevctlCommand: "release wait", testDevctlExitCode: 2, testDevctlVerdict: testDevctlTimeout})},
	}
	got := devctlCalls(tools)
	want := []devctlCall{
		{command: devctlCommandPRWait, verdict: testDevctlGreen},
		{command: devctlCommandPRMerge, verdict: "merged", mergedBy: testDevctlLogin},
		{command: devctlCommandReleaseWait, exitCode: 2, verdict: testDevctlTimeout},
	}
	if len(got) != len(want) {
		t.Fatalf("calls %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if c := devctlCalls([]harnessToolResult{{output: devctlDoc(t, map[string]any{testDevctlCommand: "pr merge", "mergedBy": map[string]any{"login": testDevctlLogin}})}}); len(c) != 1 || c[0].mergedBy != testDevctlLogin {
		t.Errorf("a merger as a user object: %+v", c)
	}
	// A nested object opens before "command" (encoding/json sorts the keys):
	// the call is the document's, not the check's.
	nested := devctlDoc(t, map[string]any{"checks": []map[string]any{{nameKey: "ci/circleci: slow-ci", "state": "failure"}}, testDevctlCommand: "pr wait", testDevctlExitCode: 1, testDevctlVerdict: testDevctlRed})
	if c := devctlCalls([]harnessToolResult{{output: nested}}); len(c) != 1 || c[0] != (devctlCall{command: devctlCommandPRWait, exitCode: 1, verdict: testDevctlRed}) {
		t.Errorf("a document with a nested object before its command: %+v", c)
	}
}

// The turn passes with each tool once, in order, exit 0 each, merged by the
// person; it fails on a repeated wait, a missing step, a failed step and
// another merger.
func TestJudgeDevctlWaits(t *testing.T) {
	good := []devctlCall{{command: devctlCommandPRWait, verdict: testDevctlGreen}, {command: devctlCommandPRMerge, mergedBy: testDevctlLogin}, {command: devctlCommandReleaseWait, verdict: "available"}}
	if err := judgeDevctlWaits(good, testDevctlMergedOK, testDevctlLogin); err != nil {
		t.Fatalf("refused: %v", err)
	}
	for name, tc := range map[string]struct {
		calls  []devctlCall
		merged string
		want   string
	}{
		"a wait called twice": {append([]devctlCall{{command: devctlCommandPRWait, verdict: testDevctlTimeout, exitCode: 2}}, good...), testDevctlMergedOK, "want each of"},
		"no release wait":     {good[:2], testDevctlMergedOK, "want each of"},
		"merge refused":       {[]devctlCall{good[0], {command: devctlCommandPRMerge, exitCode: 5, verdict: testDevctlRefused}, good[2]}, testDevctlMergedOK, "pr merge answered exitCode 5"},
		"another merger":      {[]devctlCall{good[0], {command: devctlCommandPRMerge, mergedBy: "bot"}, good[2]}, testDevctlMergedOK, "pr_merge names bot"},
		"GitHub's merger":     {good, "MERGED someone x", "GitHub names someone"},
		"still open":          {good, "OPEN  ", "the state, the merger"},
		"not merged":          {good, "CLOSED " + testDevctlLogin + " x", "want MERGED"},
	} {
		if err := judgeDevctlWaits(tc.calls, tc.merged, testDevctlLogin); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, tc.want)
		}
	}
}

// The proof's own pr_wait passes as one green call past muster's timeout
// with rising progress, and fails otherwise.
func TestJudgeDevctlProgress(t *testing.T) {
	env := func(text string, isError bool) *toolEnvelope {
		e := &toolEnvelope{IsError: isError}
		e.Content = append(e.Content, struct {
			Text string `json:"text"`
		}{Text: text})
		return e
	}
	green := env(`{"command":"pr wait","exitCode":0,"verdict":"green"}`, false)
	notes := []progressNote{{Progress: 1}, {Progress: 2}, {Progress: 3}}
	if err := judgeDevctlProgress(devctlProgressCall{env: green, notes: notes, elapsed: 10 * time.Minute}, 30*time.Second); err != nil {
		t.Fatalf("refused: %v", err)
	}
	for name, tc := range map[string]struct {
		pc   devctlProgressCall
		want string
	}{
		testDevctlRed:      {devctlProgressCall{env: env(`{"exitCode":1,"verdict":"red"}`, false), notes: notes, elapsed: time.Hour}, "want exitCode 0"},
		"auth required":    {devctlProgressCall{env: env(`{"exitCode":8,"verdict":"auth_required"}`, true), notes: notes, elapsed: time.Hour}, "want exitCode 0"},
		"within a timeout": {devctlProgressCall{env: green, notes: notes, elapsed: 20 * time.Second}, "within muster's per-call timeout"},
		"no progress":      {devctlProgressCall{env: green, elapsed: time.Hour}, "no progress notification"},
		"falling progress": {devctlProgressCall{env: green, notes: []progressNote{{Progress: 2}, {Progress: 1}}, elapsed: time.Hour}, "must rise"},
	} {
		if err := judgeDevctlProgress(tc.pc, 30*time.Second); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, tc.want)
		}
	}
}

// The waits' prompt names each tool with its arguments, in order, and the
// merge record's command.
func TestDevctlWaitsPrompt(t *testing.T) {
	prompt := devctlWaitsPrompt(DefaultDevctlProofRepository, 7, "Bash tool")
	last := -1
	for _, command := range devctlCommandOrder {
		i := strings.Index(prompt, devctlProofTools[command])
		if i < 0 || i < last {
			t.Fatalf("%s missing or out of order in:\n%s", devctlProofTools[command], prompt)
		}
		last = i
	}
	for _, want := range []string{`"noReleaseWait": true`, `"pr": 7`, `"number": 7`, `"timeout": "` + devctlWaitTimeout + `"`, "pr view 7", devctlProbeMerged} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
}

// callToolProgress sends the progressToken in the call's _meta, collects
// the stream's progress notifications for that token in order, skips
// another token's, and returns the tool's envelope from the result frame.
func TestCallToolProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
				Meta      map[string]any `json:"_meta"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding the request: %v", err)
		}
		if req.Params.Name != musterCallTool || req.Params.Arguments[nameKey] != "x_devctl_pr_wait" || req.Params.Meta["progressToken"] != "tok" {
			t.Errorf("request params %+v, want call_tool of x_devctl_pr_wait with progressToken tok", req.Params)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		inner, _ := json.Marshal(map[string]any{toolResultContent: []map[string]any{{fieldTypeKey: toolResultText, toolResultText: `{"command":"pr wait","exitCode":0,"verdict":"green"}`}}})
		frames := []string{
			`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"tok","progress":1,"message":"waiting for ci/circleci: slow"}}`,
			`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"other","progress":9}}`,
			`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"tok","progress":2,"message":"green"}}`,
			`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":` + strconv.Quote(string(inner)) + `}]}}`,
		}
		for _, f := range frames {
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", f)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	s := &musterSession{client: &http.Client{Timeout: time.Second}, url: srv.URL, id: "session", seq: 1}
	env, notes, err := s.callToolProgress(context.Background(), "x_devctl_pr_wait", map[string]any{"number": 7}, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 || notes[0].Progress != 1 || notes[1].Progress != 2 || notes[0].Message != "waiting for ci/circleci: slow" {
		t.Errorf("notes %+v, want tok's two in order", notes)
	}
	if env.IsError || len(env.Content) != 1 || !strings.Contains(env.Content[0].Text, `"verdict":"green"`) {
		t.Errorf("envelope %+v, want the tool's green document", env)
	}
}
