package lab

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/giantswarm/agentlab/internal/config"
)

// mockLLMCall posts one streamed Responses API request whose input is
// items, as codex sends them, and returns the one output item the stream
// carried, after checking it completes with usage.
func mockLLMCall(t *testing.T, srv *httptest.Server, items ...any) mockLLMItem {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": mockLLMModel, "input": items, "stream": true})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer agentlab-mockllm")
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the mock answered %d for the last input item %v", resp.StatusCode, items[len(items)-1])
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var item *mockLLMItem
	var usage any
	for _, line := range strings.Split(string(raw), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event struct {
			Type     string       `json:"type"`
			Item     *mockLLMItem `json:"item"`
			Response struct {
				Usage any `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		switch event.Type {
		case "response.output_item.done":
			item = event.Item
		case "response.completed":
			usage = event.Response.Usage
		}
	}
	if item == nil || usage == nil {
		t.Fatalf("the stream carried item %v usage %v, wanted one item and the usage: %s", item, usage, excerpt(string(raw), 400))
	}
	return *item
}

// testInputItem is one Responses API input item as codex sends it.
type testInputItem struct {
	Type    string             `json:"type"`
	Role    string             `json:"role,omitempty"`
	Content []testInputContent `json:"content,omitempty"`
	CallID  string             `json:"call_id,omitempty"`
	Output  string             `json:"output,omitempty"`
}

type testInputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func userItem(text string) testInputItem {
	return testInputItem{Type: mockLLMMessage, Role: "user", Content: []testInputContent{{Type: "input_text", Text: text}}}
}

// outputOf is the runtime's input item returning a call's output.
func outputOf(call mockLLMItem) testInputItem {
	kind := "function_call_output"
	if call.Type == "tool_search_call" {
		kind = "tool_search_output"
	}
	return testInputItem{Type: kind, CallID: call.CallID, Output: "AGENTLAB_PROBE uid=1000"}
}

// The script answers each of coding-test's codex prompts with the tool call
// the prompt asks for, then a reply once the tool's output is back, and
// counts a request outside it.
func TestMockLLMScriptAnswersTheCodexTurns(t *testing.T) {
	handler, _, err := mockLLMHandler("agentlab-coding-muster")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	codex := codingRuntimes[config.CodingRuntimeCodex]

	shellTurns := []struct{ prompt, command string }{
		{codingSandboxPrompt(codex), codingSandboxCommand(codex)},
		{codingCodeWordPrompt(codex), codingCodeWordCommand()},
		{codingResumePrompt(codex), codingResumeCommand()},
	}
	for _, turn := range shellTurns {
		call := mockLLMCall(t, srv, userItem(turn.prompt))
		var args mockLLMShellArguments
		if err := json.Unmarshal([]byte(stringOf(call.Arguments)), &args); err != nil {
			t.Fatalf("the call's arguments %v: %v", call.Arguments, err)
		}
		if call.Type != "function_call" || call.Name != "shell_command" || args.Command != turn.command || args.TimeoutMS < 60_000 {
			t.Fatalf("on %q the mock called %s %s with %q (timeout %dms), wanted shell_command with the prompt's command and minutes to run it", excerpt(turn.prompt, 60), call.Type, call.Name, args.Command, args.TimeoutMS)
		}
		if reply := mockLLMCall(t, srv, userItem(turn.prompt), call, outputOf(call)); reply.Type != mockLLMMessage {
			t.Fatalf("after the output of %s the mock answered %+v, wanted the reply", call.CallID, reply)
		}
	}

	search := mockLLMCall(t, srv, userItem(codingMCPCallPrompt()))
	if search.Type != "tool_search_call" {
		t.Fatalf("on the MCP prompt the mock answered %+v, wanted the tool search", search)
	}
	call := mockLLMCall(t, srv, userItem(codingMCPCallPrompt()), search, outputOf(search))
	if call.Type != "function_call" || call.Name != codingTestReadTool || call.Namespace != "mcp__agentlab_coding_muster" {
		t.Fatalf("after the search the mock answered %+v, wanted %s in the binding's namespace", call, codingTestReadTool)
	}
	if reply := mockLLMCall(t, srv, userItem(codingMCPCallPrompt()), call, outputOf(call)); reply.Type != mockLLMMessage {
		t.Fatalf("after the MCP call's output the mock answered %+v", reply)
	}

	resp, err := srv.Client().Post(srv.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"m","input":"not in the script"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	resp, err = srv.Client().Get(srv.URL + mockLLMRequestsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var counts mockLLMRequests
	if err := json.NewDecoder(resp.Body).Decode(&counts); err != nil {
		t.Fatal(err)
	}
	if counts.Answered != 9 || counts.Unmatched != 1 || len(counts.Log) != 10 {
		t.Fatalf("the mock counts %d answered, %d unmatched, %d log lines; wanted 9, 1 and 10", counts.Answered, counts.Unmatched, len(counts.Log))
	}
	if last := counts.Log[9]; !strings.HasPrefix(last, "404 ") || !strings.Contains(last, "not in the script") {
		t.Fatalf("the unmatched request logs as %q, wanted its 404 and its input", last)
	}
}

// A codex agent's readings on the mockllm are in its tools' outputs, the
// harness's function_response parts; the reply's lines win where both
// carry one.
func TestTurnProbeReadsTheToolOutputs(t *testing.T) {
	part := func(kind string, data any) *a2a.Part {
		p := a2a.NewDataPart(data)
		p.Metadata = map[string]any{toolActivityTypeKeys[0]: kind}
		return p
	}
	type result struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Response struct {
			Result any `json:"result"`
		} `json:"response"`
	}
	resultPart := func(name string, value any) *a2a.Part {
		r := result{ID: "item_1", Name: name}
		r.Response.Result = value
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		return part(functionResponseType, data)
	}
	upstreamResultPart := func(name string, value any) *a2a.Part {
		p := resultPart(name, value)
		p.Metadata = map[string]any{toolActivityTypeKeys[1]: functionResponseType}
		return p
	}
	tr := &turn{}
	tr.addArtifact(&a2a.Artifact{ID: "a1", Parts: a2a.ContentParts{part("function_call", map[string]any{"args": "echo AGENTLAB_PROBE uid=0"})}}, false)
	tr.addArtifact(&a2a.Artifact{ID: "a2", Parts: a2a.ContentParts{resultPart("command_execution", struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exitCode"`
	}{"AGENTLAB_PROBE uid=1000\nAGENTLAB_PROBE file=agentlab-saffron\n", 0})}}, false)
	tr.addArtifact(&a2a.Artifact{ID: "a3", Parts: a2a.ContentParts{upstreamResultPart("agentlab-coding-muster."+codingTestReadTool, []string{"agentlab-coding"})}}, false)
	tr.addArtifact(&a2a.Artifact{ID: "a4", Parts: a2a.ContentParts{a2a.NewTextPart("AGENTLAB_PROBE code-word=agentlab-saffron\nAGENTLAB_PROBE file=from-the-reply")}}, false)

	if len(tr.tools) != 2 {
		t.Fatalf("the turn read %d tool results, wanted the 2 function_response parts: %+v", len(tr.tools), tr.tools)
	}
	ct := codingTurn{reply: tr.text(), probe: parseTurnProbe(tr.text(), tr.toolOutputs()), tools: tr.tools}
	if ct.probe["uid"] != "1000" || ct.probe["code-word"] != "agentlab-saffron" || ct.probe["file"] != "from-the-reply" {
		t.Fatalf("probe %v: wanted uid from the output, the code word and the file from the reply", ct.probe)
	}
	if got := ct.toolOutput(codingTestReadTool); !strings.Contains(got, "agentlab-coding") {
		t.Fatalf("the MCP call's output reads %q, wanted the result naming the agent", got)
	}
	if ct.toolOutput("absent") != "" {
		t.Fatal("a tool the turn did not call has an output")
	}
}
