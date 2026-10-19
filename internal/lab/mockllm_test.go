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
	return mockLLMCallOffering(t, srv, nil, items...)
}

// mockLLMCallOffering is mockLLMCall on a request offering the function
// tools named tools.
func mockLLMCallOffering(t *testing.T, srv *httptest.Server, tools []string, items ...any) mockLLMItem {
	t.Helper()
	type tool struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	offered := make([]tool, 0, len(tools))
	for _, name := range tools {
		offered = append(offered, tool{Type: "function", Name: name})
	}
	body, err := json.Marshal(struct {
		Model  string `json:"model"`
		Input  []any  `json:"input"`
		Stream bool   `json:"stream"`
		Tools  []tool `json:"tools"`
	}{mockLLMModel, items, true, offered})
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
	if strings.Contains(string(raw), `"type":"message","status":"in_progress"`) && !strings.Contains(string(raw), `"content":[]`) {
		t.Fatalf("the stream adds a message item without its empty content, which codex needs: %s", excerpt(string(raw), 600))
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
	kind := mockLLMFunctionCallOutput
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
	if call.Type != "function_call" || call.Name != musterCallTool || call.Namespace != "mcp__agentlab_coding_muster" || stringOf(call.Arguments) != musterCallToolArguments(codingTestReadTool) {
		t.Fatalf("after the search the mock answered %+v, wanted %s running %s in the binding's namespace", call, musterCallTool, codingTestReadTool)
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
// harness's function_response parts; the outputs' lines win where the reply
// relays one too, what the command printed over the model's word for it.
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
	if ct.probe["uid"] != "1000" || ct.probe["code-word"] != "agentlab-saffron" || ct.probe["file"] != "agentlab-saffron" {
		t.Fatalf("probe %v: wanted uid and the file from the output, the code word from the reply", ct.probe)
	}
	if got := ct.toolOutput(codingTestReadTool); !strings.Contains(got, "agentlab-coding") {
		t.Fatalf("the MCP call's output reads %q, wanted the result naming the agent", got)
	}
	if ct.toolOutput("absent") != "" {
		t.Fatal("a tool the turn did not call has an output")
	}
}

// On a codex that offers the unified exec_command instead of shell_command
// (0.163 and later), the script's shell calls go to exec_command, a command
// still running at the call's yield is waited for with an empty write_stdin
// on its session, and the reply follows the command's final output.
func TestMockLLMScriptWaitsForExecCommand(t *testing.T) {
	handler, _, err := mockLLMHandler("agentlab-coding-muster")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	codex := codingRuntimes[config.CodingRuntimeCodex]
	tools := []string{mockLLMExecTool, mockLLMWriteStdinTool, "list_mcp_resources"}
	prompt := userItem(codingSandboxPrompt(codex))

	call := mockLLMCallOffering(t, srv, tools, prompt)
	var exec mockLLMExecArguments
	if err := json.Unmarshal([]byte(stringOf(call.Arguments)), &exec); err != nil {
		t.Fatalf("the call's arguments %v: %v", call.Arguments, err)
	}
	if call.Name != mockLLMExecTool || exec.Cmd != codingSandboxCommand(codex) || exec.TimeoutMS < 60_000 || exec.YieldTimeMS != mockLLMExecYieldMS {
		t.Fatalf("the mock called %s with %+v, wanted exec_command with the probe, minutes to run and the longest yield", call.Name, exec)
	}

	running := testInputItem{Type: mockLLMFunctionCallOutput, CallID: call.CallID, Output: "Chunk ID: 1\nWall time: 30.0 seconds\nProcess running with session ID 7\nOutput:\nAGENTLAB_PROBE uid=1000\n"}
	wait := mockLLMCallOffering(t, srv, tools, prompt, call, running)
	var stdin mockLLMWriteStdinArguments
	if err := json.Unmarshal([]byte(stringOf(wait.Arguments)), &stdin); err != nil {
		t.Fatalf("the wait's arguments %v: %v", wait.Arguments, err)
	}
	if wait.Name != mockLLMWriteStdinTool || stdin.SessionID != 7 || stdin.Chars != "" || stdin.YieldTimeMS < 60_000 || wait.CallID == call.CallID {
		t.Fatalf("on a running command the mock answered %s %+v (call %s), wanted an empty write_stdin on session 7 waiting minutes, under a call id of its own", wait.Name, stdin, wait.CallID)
	}

	again := mockLLMCallOffering(t, srv, tools, prompt, call, running, wait, testInputItem{Type: mockLLMFunctionCallOutput, CallID: wait.CallID, Output: "Process running with session ID 7\nOutput:\n"})
	if again.Name != mockLLMWriteStdinTool || again.CallID == wait.CallID {
		t.Fatalf("on a command still running the mock answered %+v, wanted another wait", again)
	}
	exited := testInputItem{Type: mockLLMFunctionCallOutput, CallID: again.CallID, Output: "Process exited with code 0\nOutput:\nAGENTLAB_PROBE file=agentlab-saffron\n"}
	if reply := mockLLMCallOffering(t, srv, tools, prompt, call, running, wait, exited); reply.Type != mockLLMMessage {
		t.Fatalf("after the command exited the mock answered %+v, wanted the reply", reply)
	}

	if call := mockLLMCallOffering(t, srv, []string{mockLLMShellTool, mockLLMExecTool}, prompt); call.Name != mockLLMShellTool {
		t.Fatalf("on a codex offering both shell tools the mock called %s, wanted shell_command", call.Name)
	}
}
