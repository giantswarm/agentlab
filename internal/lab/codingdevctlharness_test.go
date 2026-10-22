package lab

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// With the devctl wait's arguments the mockllm answers the wait's prompt
// with call_tool running x_devctl_pr_wait on the agent's binding, after the
// tool search.
func TestMockLLMScriptAnswersTheDevctlWait(t *testing.T) {
	hp := &devctlHarnessProof{devctlProof: &devctlProof{repository: DefaultDevctlProofRepository, number: 42}}
	arguments, err := hp.waitArguments()
	if err != nil {
		t.Fatal(err)
	}
	handler, _, err := mockLLMHandler("agentlab-devctl-muster", arguments, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	prompt := devctlHarnessWaitPrompt(DefaultDevctlProofRepository, 42)
	search := mockLLMCall(t, srv, userItem(prompt))
	if search.Type != mockLLMToolSearchCall {
		t.Fatalf("on the wait's prompt the mock answered %+v, wanted the tool search", search)
	}
	call := mockLLMCall(t, srv, userItem(prompt), search, outputOf(search))
	if call.Type != mockLLMFunctionCall || call.Name != musterCallTool || stringOf(call.Arguments) != arguments {
		t.Fatalf("after the search the mock answered %+v, wanted %s with %s", call, musterCallTool, arguments)
	}
	if !strings.Contains(arguments, `"name":"x_devctl_pr_wait"`) || !strings.Contains(arguments, `"number":42`) {
		t.Fatalf("the wait's arguments %s name no x_devctl_pr_wait on #42", arguments)
	}
	if reply := mockLLMCall(t, srv, userItem(prompt), call, outputOf(call)); reply.Type != mockLLMMessage {
		t.Fatalf("after the wait's output the mock answered %+v", reply)
	}

	without, _, err := mockLLMHandler("agentlab-devctl-muster", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	plain := httptest.NewServer(without)
	defer plain.Close()
	body, err := json.Marshal(map[string]any{"model": mockLLMModel, "input": prompt})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := plain.Client().Post(plain.URL+"/v1/responses", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("without the devctl arguments the mock answered the wait's prompt with %d, wanted 404", resp.StatusCode)
	}
}

func TestJudgeDevctlHarnessWait(t *testing.T) {
	green := devctlCall{command: devctlCommandPRWait, verdict: "green"}
	for _, tc := range []struct {
		name    string
		calls   []devctlCall
		elapsed time.Duration
		want    string
	}{
		{"one green call past the timeout", []devctlCall{green}, 11 * time.Minute, ""},
		{"no call", nil, 11 * time.Minute, "want pr wait once"},
		{"two calls", []devctlCall{green, green}, 11 * time.Minute, "want pr wait once"},
		{"red", []devctlCall{{command: devctlCommandPRWait, exitCode: 1, verdict: "red"}}, 11 * time.Minute, "exitCode 1"},
		{"within the timeout", []devctlCall{green}, 20 * time.Second, "within muster's per-call timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := judgeDevctlHarnessWait(tc.calls, tc.elapsed, 30*time.Second)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestJudgeDevctlCallLog(t *testing.T) {
	const repo = "giantswarm/devctl-mcp-proof"
	line := func(tool, login, repository string, exitCode int) string {
		return `{"time":"2026-10-22T20:00:00Z","level":"INFO","msg":"tool call","tool":"` + tool + `","login":"` + login + `","userId":"1","repository":"` + repository + `","verdict":"green","exitCode":` + map[int]string{0: "0", 8: "8"}[exitCode] + `,"duration":614000000000}` + "\n"
	}
	got, err := judgeDevctlCallLog("starting\n"+line("release_wait", "teemow", repo, 0)+line("pr_wait", "teemow", repo, 0), repo, "teemow")
	if err != nil || !strings.Contains(got, "as teemow") || !strings.Contains(got, "10m14s") {
		t.Fatalf("got %q, %v; want the call as teemow", got, err)
	}
	if _, err := judgeDevctlCallLog(line("pr_wait", "anonymous", repo, 8), repo, "teemow"); err == nil || !strings.Contains(err.Error(), "as anonymous") {
		t.Fatalf("an anonymous call: got %v, want it named", err)
	}
	if _, err := judgeDevctlCallLog(line("pr_wait", "teemow", "giantswarm/other", 0), repo, "teemow"); err == nil || !strings.Contains(err.Error(), "logged no") {
		t.Fatalf("another repository's call: got %v, want none found", err)
	}
}

// The devctl wait's reply is the tool's output item as the runtime returned
// it, so a runtime that reports no tool results still relays the document.
func TestMockLLMEchoesTheDevctlWaitOutput(t *testing.T) {
	hp := &devctlHarnessProof{devctlProof: &devctlProof{repository: DefaultDevctlProofRepository, number: 7}}
	arguments, err := hp.waitArguments()
	if err != nil {
		t.Fatal(err)
	}
	handler, _, err := mockLLMHandler("agentlab-devctl-muster", arguments, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	prompt := devctlHarnessWaitPrompt(DefaultDevctlProofRepository, 7)
	search := mockLLMCall(t, srv, userItem(prompt))
	call := mockLLMCall(t, srv, userItem(prompt), search, outputOf(search))
	doc := `{"command":"pr wait","exitCode":0,"verdict":"green"}`
	output := testInputItem{Type: mockLLMFunctionCallOutput, CallID: call.CallID, Output: doc}
	reply := mockLLMCall(t, srv, userItem(prompt), call, output)
	raw, _ := json.Marshal(reply.Content)
	var content []mockLLMContent
	if err := json.Unmarshal(raw, &content); err != nil || len(content) != 1 {
		t.Fatalf("the reply's content %s: %v", raw, err)
	}
	calls := devctlCalls([]harnessToolResult{{name: musterCallTool, output: content[0].Text}})
	if len(calls) != 1 || calls[0].command != devctlCommandPRWait || calls[0].exitCode != 0 || calls[0].verdict != "green" {
		t.Fatalf("the reply %s read as %+v, want the pr wait document", content[0].Text, calls)
	}
}
