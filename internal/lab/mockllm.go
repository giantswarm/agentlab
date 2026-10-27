package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The codex coding proof's model (docs/platform.md "The coding agents
// proof"): a mock OpenAI Responses API in the shape of kagent's mockllm,
// scripted for coding-test's turns, so `agentlab
// coding-test --harness codex` reaches a verdict on a lab with no OpenAI or
// Bedrock account. It runs the way a proof's fake runs (fakecontainer.go):
// `agentlab mockllm` in a container on the kind network, reached by the
// cluster as the Service agentlab-mockllm in the platform namespace, which
// the lab's ModelConfig agentlab-mockllm (OpenAI provider, Responses API)
// names as its base URL. It serves the Responses API's stream itself (the
// events of kagent's mockllm, which codex reads), so the lab carries no SDK
// for it. The script answers each of the proof's prompts with
// the shell command or MCP call the prompt asks for, the way kagent's codex
// e2e fixtures do, then a fixed reply once the runtime returns the tool's
// output: the readings the proof judges are those outputs, what the sandbox
// printed, never words of the script's. The one reading the script gives
// itself is the code word the resumed turn recalls. A shell call goes to the
// shell tool the request offers: codex's `shell_command`, or its unified
// `exec_command`, whose command keeps running past its yield and is waited
// for with an empty `write_stdin` until it exits. The script also answers
// harness-matrix-test's delegation (mockLLMDelegation).

const (
	// mockLLMCommand is the hidden command the container runs.
	mockLLMCommand = "mockllm"
	// mockLLMContainerSuffix names the container <cluster>-mockllm.
	mockLLMContainerSuffix = "mockllm"
	// mockLLMService is the Service pods reach the mockllm through, in the
	// platform namespace, on fakeServicePort.
	mockLLMService = "agentlab-mockllm"
	// MockLLMModelConfig is the kagent ModelConfig on the mockllm, and
	// mockLLMSecret its key's Secret (a placeholder: the mock checks a
	// key's presence only).
	MockLLMModelConfig = "agentlab-mockllm"
	mockLLMSecret      = "agentlab-mockllm" // #nosec G101 -- a Secret's name
	mockLLMSecretKey   = "OPENAI_API_KEY"   // #nosec G101 -- a key's name
	// mockLLMModel is the model the ModelConfig names and the script
	// answers as: a codex model, whose tool search the runtime enables.
	mockLLMModel = "gpt-5.2-codex"
	// mockLLMHealthPath answers "ok"; mockLLMRequestsPath the requests the
	// mock answered and those no script entry matched.
	mockLLMHealthPath   = "/healthz"
	mockLLMRequestsPath = "/requests"
	mockLLMWhat         = "the mockllm, the codex coding proof's model"
)

// mockLLMURL is the base URL the ModelConfig names: the Service, the
// Responses API under /v1.
func mockLLMURL() string {
	return fmt.Sprintf("http://%s.%s.svc/v1", mockLLMService, platformNamespace)
}

// mockLLMTurn is one turn of the script: the prompt it answers (a substring
// of the turn's message), the tool call it answers with, and the reply once
// the tool's output is back.
type mockLLMTurn struct {
	name, prompt string
	// command is the shell command the runtime runs; tool the MCP tool it
	// calls instead (on the agent's binding, found through tool search),
	// with arguments, a JSON object.
	command, tool, arguments string
	reply                    string
	// echo replies with the tool's output item as the runtime returned it
	// instead of reply: a runtime that reports no tool results to its
	// caller (codex's MCP calls) still relays the tool's own answer.
	echo bool
}

// mockLLMScript is the script of the proof's codex turns. The prompts are
// substrings of the proof's own (codingprobe.go) the others do not carry.
// devctlArguments, call_tool's arguments of `coding-test --part devctl`'s
// wait, add that turn.
func mockLLMScript(devctlArguments string) []mockLLMTurn {
	codex := codingRuntimes[config.CodingRuntimeCodex]
	var devctl []mockLLMTurn
	if devctlArguments != "" {
		devctl = append(devctl, mockLLMTurn{name: "devctl", prompt: devctlHarnessPrompt, tool: musterCallTool, arguments: devctlArguments, echo: true})
	}
	return append(devctl, []mockLLMTurn{
		{name: "sandbox", prompt: "own sandbox check", command: codingSandboxCommand(codex), reply: "The sandbox check ran; its readings are the command's output."},
		{name: "mcp", prompt: "Do not use a shell for this", tool: musterCallTool, arguments: musterCallToolArguments(codingTestReadTool), reply: "The tool answered; its answer is the call's output."},
		{name: "heartbeat", prompt: "Start this test lab's heartbeat", command: codingHeartbeatCommand(), reply: "The heartbeat runs."},
		{name: "heartbeat-read", prompt: "Read this test lab's heartbeat", command: codingHeartbeatReadCommand(), reply: "The heartbeat's ticks are the command's output."},
		{name: "codeword", prompt: "Remember the code word", command: codingCodeWordCommand(), reply: "The file is written."},
		{name: "resume", prompt: "the code word I asked you to remember", command: codingResumeCommand(), reply: probeMarker + " code-word=" + codingTestCodeWord},
		{name: "toolchain", prompt: toolchainsPrompt, command: toolchainsCheckCommand(), reply: "The toolchain check ran; its readings are the command's output."},
		{name: "sign-in", prompt: signInTestPrompt, tool: musterCallTool, arguments: musterCallToolArguments(fixtureSignedOutTool), echo: true},
	}...)
}

// The script's wire form, the Responses API's own field names.
type (
	// mockLLMScriptEntry answers a request whose last input item
	// contains match with response; command is the shell command of a
	// shell call, which goes to the shell tool the request offers.
	mockLLMScriptEntry struct {
		match    string
		command  string
		response mockLLMResponse
		// echo answers with the last input item as the reply's text.
		echo bool
	}
	mockLLMResponse struct {
		ID                string           `json:"id"`
		Object            string           `json:"object"`
		CreatedAt         int64            `json:"created_at"`
		Model             string           `json:"model"`
		Output            []mockLLMItem    `json:"output"`
		ParallelToolCalls bool             `json:"parallel_tool_calls"`
		Tools             []any            `json:"tools"`
		Status            string           `json:"status"`
		Usage             map[string]int64 `json:"usage"`
	}
	mockLLMItem struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Status string `json:"status"`
		Role   string `json:"role,omitempty"`
		// Content is a message's []mockLLMContent: an interface, so a
		// message's empty content stays on the wire as [] (codex reads a
		// message item only with it) and other items carry none.
		Content   any    `json:"content,omitempty"`
		CallID    string `json:"call_id,omitempty"`
		Execution string `json:"execution,omitempty"`
		Namespace string `json:"namespace,omitempty"`
		Name      string `json:"name,omitempty"`
		// Arguments is a JSON string on a function call, an object on a
		// tool search.
		Arguments any `json:"arguments,omitempty"`
	}
	mockLLMContent struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Annotations []any  `json:"annotations"`
	}
	mockLLMShellArguments struct {
		Command string `json:"command"`
		// TimeoutMS bounds the command; codex's default of seconds
		// cuts the probe's searches and lookups.
		TimeoutMS int `json:"timeout_ms"`
	}
	// mockLLMExecArguments are codex's unified exec_command's: the
	// command, how long the call waits for it before yielding (codex
	// clamps it to 30 s) and how long it may run.
	mockLLMExecArguments struct {
		Cmd         string `json:"cmd"`
		YieldTimeMS int    `json:"yield_time_ms"`
		TimeoutMS   int    `json:"timeout_ms"`
	}
	// mockLLMWriteStdinArguments wait for a running exec_command's
	// session: no input, up to the yield (codex allows minutes on an
	// empty write).
	mockLLMWriteStdinArguments struct {
		SessionID   int    `json:"session_id"`
		Chars       string `json:"chars"`
		YieldTimeMS int    `json:"yield_time_ms"`
	}
	mockLLMSearchArguments struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
)

// mockLLMItemDone is the status of every item the script answers with;
// the item types and a reply's content type.
const (
	mockLLMItemDone     = "completed"
	mockLLMMessage      = "message"
	mockLLMFunctionCall = "function_call"
	// mockLLMToolSearchCall is codex's search for a deferred MCP tool.
	mockLLMToolSearchCall = "tool_search_call"
	// mockLLMFunctionCallOutput is the runtime's input item returning a
	// function call's output.
	mockLLMFunctionCallOutput = "function_call_output"
	mockLLMOutputText         = "output_text"
	// The shell tools of codex: the classic one, and the unified exec
	// and its stdin, which codex 0.163 and later offer instead.
	mockLLMShellTool      = "shell_command"
	mockLLMExecTool       = "exec_command"
	mockLLMWriteStdinTool = "write_stdin"
	// mockLLMExecYieldMS is the longest an exec_command call waits.
	mockLLMExecYieldMS = 30_000
)

// mockLLMConfig is the script for the MCP tools on mcpServer: per turn the
// tool call answering the prompt, then the reply answering the tool's output
// (matched by its call id, which the runtime's last input item carries).
func mockLLMConfig(mcpServer, devctlArguments string) ([]mockLLMScriptEntry, error) {
	var entries []mockLLMScriptEntry
	for _, turn := range mockLLMScript(devctlArguments) {
		callID := "call_agentlab_" + turn.name
		if turn.command != "" {
			arguments, err := json.Marshal(mockLLMShellArguments{Command: turn.command, TimeoutMS: codingProbeTimeoutMS})
			if err != nil {
				return nil, err
			}
			entry := mockLLMEntry(turn.name, turn.prompt, mockLLMItem{
				ID: "fc_agentlab_" + turn.name, Type: mockLLMFunctionCall, Status: mockLLMItemDone,
				CallID: callID, Name: mockLLMShellTool, Arguments: string(arguments),
			})
			entry.command = turn.command
			entries = append(entries, entry)
		} else {
			// codex defers an MCP server's tools behind its tool search:
			// the search first, then the call in the server's namespace.
			namespace := codexMCPToolNamespace(mcpServer)
			searchID := "search_agentlab_" + turn.name
			entries = append(entries,
				mockLLMEntry(turn.name, turn.prompt, mockLLMItem{
					ID: "ts_agentlab_" + turn.name, Type: mockLLMToolSearchCall, Status: mockLLMItemDone,
					CallID: searchID, Execution: "client",
					Arguments: mockLLMSearchArguments{Query: namespace + " " + turn.tool, Limit: 10},
				}),
				mockLLMEntry(turn.name+"_call", searchID, mockLLMItem{
					ID: "fc_agentlab_" + turn.name, Type: mockLLMFunctionCall, Status: mockLLMItemDone,
					CallID: callID, Namespace: namespace, Name: turn.tool, Arguments: turn.arguments,
				}))
		}
		reply := mockLLMEntry(turn.name+"_reply", callID, mockLLMMessageOf(turn.name, turn.reply))
		reply.echo = turn.echo
		entries = append(entries, reply)
	}
	return entries, nil
}

// mockLLMEntry is one Responses API answer of the script: output, with the
// usage the turn's status reports, for a request whose last input item
// contains match.
func mockLLMEntry(name, match string, output mockLLMItem) mockLLMScriptEntry {
	return mockLLMScriptEntry{
		match: match,
		response: mockLLMResponse{
			ID: "resp_agentlab_" + name, Object: "response", CreatedAt: 1_760_000_000, Model: mockLLMModel,
			Output: []mockLLMItem{output}, Tools: []any{}, Status: mockLLMItemDone,
			Usage: map[string]int64{"input_tokens": 20, "output_tokens": 10, "total_tokens": 30},
		},
	}
}

// mockLLMMessageOf is the assistant message of the script's reply text.
func mockLLMMessageOf(name, text string) mockLLMItem {
	return mockLLMItem{
		ID: "msg_agentlab_" + name, Type: mockLLMMessage, Status: mockLLMItemDone, Role: "assistant",
		Content: []mockLLMContent{{Type: mockLLMOutputText, Text: text, Annotations: []any{}}},
	}
}

// mockLLMMatch is the script's answer to a request offering tools, whose
// message items' texts by role are texts: a wait for an exec_command still
// running, else a step of the matrix's delegation, else the first entry
// whose match the last input item (as JSON; the input itself when it is a
// string) contains, its shell call on the shell tool offered; false for
// none.
func mockLLMMatch(script []mockLLMScriptEntry, last string, texts map[string]string, tools []string) (mockLLMResponse, bool) {
	if wait, ok := mockLLMExecWait(last); ok {
		return wait, true
	}
	if step, ok := mockLLMDelegation(last, texts, tools); ok {
		return step, true
	}
	for _, entry := range script {
		if strings.Contains(last, entry.match) {
			if entry.echo {
				return entry.echoed(last), true
			}
			return entry.onShellTool(tools), true
		}
	}
	return mockLLMResponse{}, false
}

// echoed is the entry's reply with last, the tool's output item, as its
// text.
func (e mockLLMScriptEntry) echoed(last string) mockLLMResponse {
	response := e.response
	message := response.Output[0]
	message.Content = []mockLLMContent{{Type: mockLLMOutputText, Text: last, Annotations: []any{}}}
	response.Output = []mockLLMItem{message}
	return response
}

// onShellTool is the entry's response with its shell call on the tool the
// request offers: exec_command where it offers that and no shell_command.
func (e mockLLMScriptEntry) onShellTool(tools []string) mockLLMResponse {
	if e.command == "" || !slices.Contains(tools, mockLLMExecTool) || slices.Contains(tools, mockLLMShellTool) {
		return e.response
	}
	response := e.response
	call := response.Output[0]
	arguments, _ := json.Marshal(mockLLMExecArguments{Cmd: e.command, YieldTimeMS: mockLLMExecYieldMS, TimeoutMS: codingProbeTimeoutMS})
	call.Name, call.Arguments = mockLLMExecTool, string(arguments)
	response.Output = []mockLLMItem{call}
	return response
}

// mockLLMExecRunning is how codex's exec_command and write_stdin report a
// command still running past the call's yield.
var mockLLMExecRunning = regexp.MustCompile(`Process running with session ID (\d+)`)

// mockLLMExecWait answers the output of an exec_command (or of a wait for
// one) whose command still runs with a write_stdin waiting for it, its call
// id the output's own with a suffix, so the turn's reply still matches the
// command's final output; false for any other item.
func mockLLMExecWait(last string) (mockLLMResponse, bool) {
	var item struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
	}
	if json.Unmarshal([]byte(last), &item) != nil || item.Type != mockLLMFunctionCallOutput {
		return mockLLMResponse{}, false
	}
	// The output is a string or a list of content items: either way its
	// text is in the item's JSON as written.
	running := mockLLMExecRunning.FindStringSubmatch(last)
	if running == nil {
		return mockLLMResponse{}, false
	}
	session, err := strconv.Atoi(running[1])
	if err != nil {
		return mockLLMResponse{}, false
	}
	arguments, _ := json.Marshal(mockLLMWriteStdinArguments{SessionID: session, YieldTimeMS: codingProbeTimeoutMS})
	callID := item.CallID + "_wait"
	return mockLLMEntry("wait", "", mockLLMItem{
		ID: "fc_" + callID, Type: mockLLMFunctionCall, Status: mockLLMItemDone,
		CallID: callID, Name: mockLLMWriteStdinTool, Arguments: string(arguments),
	}).response, true
}

// The matrix's delegation on codex's multi-agent tools (codex 0.164.0's
// default multi_agent_v1, a namespace codex defers behind its tool search on
// a model that has one): the parent's prompt, which names a Shared child, is
// answered with spawn_agent of that child's role and mockLLMDelegateTask,
// then wait_agent on the agent it spawned. The child's own request, whose
// last input item is that task, is answered with the codeword its developer
// instructions (the child's system prompt) carry, or none. The parent's
// reply is the child's final message from the wait's output or from codex's
// subagent notification. The script holds no codeword: one reaches the
// parent's reply only through the child's request.
const (
	mockLLMMultiAgentNamespace = "multi_agent_v1"
	mockLLMSpawnAgentTool      = "spawn_agent"
	mockLLMWaitAgentTool       = "wait_agent"
	mockLLMToolSearchOutput    = "tool_search_output"
	mockLLMDelegateTask        = "The test lab's harness matrix asks you for your codeword."
	mockLLMDelegateCallID      = "call_agentlab_delegate"
	mockLLMDelegateWaitCallID  = mockLLMDelegateCallID + "_wait"
	mockLLMDelegateSearchID    = "search_agentlab_delegate"
	mockLLMSubagentNotified    = "<subagent_notification>"
	mockLLMSubagentNotifiedEnd = "</subagent_notification>"
)

var (
	mockLLMDelegateTo = regexp.MustCompile(regexp.QuoteMeta(matrixDelegatePrompt) + `"([^"]+)"`)
	mockLLMCodeword   = regexp.MustCompile(`CODEWORD=([A-Za-z0-9_-]+)`)
)

type (
	mockLLMSpawnArguments struct {
		AgentType string `json:"agent_type"`
		Message   string `json:"message"`
	}
	mockLLMWaitArguments struct {
		Targets   []string `json:"targets"`
		TimeoutMS int      `json:"timeout_ms"`
	}
	// mockLLMInputItem is what the script reads of an input item; a
	// message's content and a call output's output are each a string or a
	// list of content items.
	mockLLMInputItem struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		CallID  string          `json:"call_id"`
		Content json.RawMessage `json:"content"`
		Output  json.RawMessage `json:"output"`
	}
)

// mockLLMText is the text of a content or output: the string, or its
// content items' texts joined by newlines.
func mockLLMText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		texts = append(texts, part.Text)
	}
	return strings.Join(texts, "\n")
}

// mockLLMDelegation is the delegation's step answering the last input item,
// in a request whose message texts by role are texts and which offers
// tools; false for an item outside the delegation.
func mockLLMDelegation(last string, texts map[string]string, tools []string) (mockLLMResponse, bool) {
	var item mockLLMInputItem
	if json.Unmarshal([]byte(last), &item) != nil {
		return mockLLMResponse{}, false
	}
	switch item.Type {
	case mockLLMMessage:
		text := mockLLMText(item.Content)
		switch {
		case strings.Contains(text, mockLLMDelegateTask):
			codeword := "none"
			if found := mockLLMCodeword.FindStringSubmatch(texts["developer"]); found != nil {
				codeword = found[1]
			}
			return mockLLMReply("delegate_child", "CODEWORD="+codeword), true
		case strings.Contains(text, mockLLMSubagentNotified):
			if message, ok := mockLLMNotifiedMessage(text); ok {
				return mockLLMReply("delegate", message), true
			}
			return mockLLMReply("delegate", text), true
		}
		child := mockLLMDelegateTo.FindStringSubmatch(text)
		if child == nil {
			return mockLLMResponse{}, false
		}
		if !slices.Contains(tools, mockLLMMultiAgentNamespace) {
			return mockLLMEntry("delegate_search", "", mockLLMItem{
				ID: "ts_agentlab_delegate", Type: mockLLMToolSearchCall, Status: mockLLMItemDone,
				CallID: mockLLMDelegateSearchID, Execution: "client",
				Arguments: mockLLMSearchArguments{Query: mockLLMSpawnAgentTool + " " + mockLLMWaitAgentTool + " subagent", Limit: 10},
			}).response, true
		}
		return mockLLMSpawn(child[1]), true
	case mockLLMToolSearchOutput:
		child := mockLLMDelegateTo.FindStringSubmatch(texts["user"])
		if item.CallID != mockLLMDelegateSearchID || child == nil {
			return mockLLMResponse{}, false
		}
		return mockLLMSpawn(child[1]), true
	case mockLLMFunctionCallOutput:
		output := mockLLMText(item.Output)
		switch {
		case item.CallID == mockLLMDelegateCallID:
			var spawned struct {
				AgentID string `json:"agent_id"`
			}
			if json.Unmarshal([]byte(output), &spawned) != nil || spawned.AgentID == "" {
				return mockLLMReply("delegate", output), true
			}
			return mockLLMWait(1, spawned.AgentID), true
		case strings.HasPrefix(item.CallID, mockLLMDelegateWaitCallID):
			var waited struct {
				Status   map[string]json.RawMessage `json:"status"`
				TimedOut bool                       `json:"timed_out"`
			}
			if json.Unmarshal([]byte(output), &waited) != nil {
				return mockLLMReply("delegate", output), true
			}
			for _, status := range waited.Status {
				if message, ok := mockLLMCompleted(status); ok {
					return mockLLMReply("delegate", message), true
				}
			}
			if waited.TimedOut {
				if waits, agent, ok := mockLLMWaitOf(item.CallID); ok {
					return mockLLMWait(waits+1, agent), true
				}
			}
			return mockLLMReply("delegate", output), true
		}
	}
	return mockLLMResponse{}, false
}

// mockLLMReply is the script's final reply text.
func mockLLMReply(name, text string) mockLLMResponse {
	return mockLLMEntry(name+"_reply", "", mockLLMMessageOf(name, text)).response
}

// mockLLMSpawn is spawn_agent of role with the delegated task, a fresh
// context (no fork of the parent's history).
func mockLLMSpawn(role string) mockLLMResponse {
	arguments, _ := json.Marshal(mockLLMSpawnArguments{AgentType: role, Message: mockLLMDelegateTask})
	return mockLLMEntry("delegate_spawn", "", mockLLMItem{
		ID: "fc_agentlab_delegate", Type: mockLLMFunctionCall, Status: mockLLMItemDone,
		CallID: mockLLMDelegateCallID, Namespace: mockLLMMultiAgentNamespace, Name: mockLLMSpawnAgentTool, Arguments: string(arguments),
	}).response
}

// mockLLMWait is the wait-th wait_agent on agent, up to the longest a probe
// runs; its call id carries both, so the output of a wait that timed out
// names the agent to wait on again.
func mockLLMWait(wait int, agent string) mockLLMResponse {
	arguments, _ := json.Marshal(mockLLMWaitArguments{Targets: []string{agent}, TimeoutMS: codingProbeTimeoutMS})
	callID := fmt.Sprintf("%s_%d_%s", mockLLMDelegateWaitCallID, wait, agent)
	return mockLLMEntry("delegate_wait", "", mockLLMItem{
		ID: "fc_" + callID, Type: mockLLMFunctionCall, Status: mockLLMItemDone,
		CallID: callID, Namespace: mockLLMMultiAgentNamespace, Name: mockLLMWaitAgentTool, Arguments: string(arguments),
	}).response
}

// mockLLMWaitOf is the wait count and agent of a mockLLMWait call id.
func mockLLMWaitOf(callID string) (int, string, bool) {
	rest, ok := strings.CutPrefix(callID, mockLLMDelegateWaitCallID+"_")
	if !ok {
		return 0, "", false
	}
	count, agent, ok := strings.Cut(rest, "_")
	wait, err := strconv.Atoi(count)
	if !ok || err != nil || agent == "" {
		return 0, "", false
	}
	return wait, agent, true
}

// mockLLMCompleted is the final message of a completed agent status as
// codex writes it, {"completed": message}; false for another status.
func mockLLMCompleted(status json.RawMessage) (string, bool) {
	var final struct {
		Completed *string `json:"completed"`
	}
	if json.Unmarshal(status, &final) != nil || final.Completed == nil {
		return "", false
	}
	return *final.Completed, true
}

// mockLLMNotifiedMessage is the completed child's final message in codex's
// subagent notification, {"agent_path", "status"} between its markers.
func mockLLMNotifiedMessage(text string) (string, bool) {
	_, body, _ := strings.Cut(text, mockLLMSubagentNotified)
	body, _, _ = strings.Cut(body, mockLLMSubagentNotifiedEnd)
	var notified struct {
		Status json.RawMessage `json:"status"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &notified) != nil {
		return "", false
	}
	return mockLLMCompleted(notified.Status)
}

// mockLLMEvent is one server-sent event of the Responses API's stream: its
// type, sequence number and the fields that event carries.
type mockLLMEvent struct {
	Type           string           `json:"type"`
	SequenceNumber int              `json:"sequence_number"`
	Response       *mockLLMResponse `json:"response,omitempty"`
	OutputIndex    *int             `json:"output_index,omitempty"`
	ItemID         string           `json:"item_id,omitempty"`
	ContentIndex   *int             `json:"content_index,omitempty"`
	Item           *mockLLMItem     `json:"item,omitempty"`
	Part           *mockLLMContent  `json:"part,omitempty"`
	Delta          *string          `json:"delta,omitempty"`
	Text           *string          `json:"text,omitempty"`
	Arguments      *string          `json:"arguments,omitempty"`
	Name           string           `json:"name,omitempty"`
}

// mockLLMStream is the Responses API's stream of a final response, the
// events kagent's mockllm sends: created and in progress, each item's own
// (a message's content part and text, a function call's arguments, a tool
// search's item done), then completed with the usage.
func mockLLMStream(response mockLLMResponse) []mockLLMEvent {
	inProgress := response
	inProgress.Status, inProgress.Output = "in_progress", []mockLLMItem{}
	events := []mockLLMEvent{
		{Type: "response.created", Response: &inProgress},
		{Type: "response.in_progress", Response: &inProgress},
	}
	for i, item := range response.Output {
		index := i
		added := item
		added.Status = "in_progress"
		switch item.Type {
		case mockLLMMessage:
			added.Content = []mockLLMContent{}
			events = append(events, mockLLMEvent{Type: "response.output_item.added", OutputIndex: &index, Item: &added})
			contents, _ := item.Content.([]mockLLMContent)
			for c, content := range contents {
				c, content := c, content
				empty := mockLLMContent{Type: content.Type, Annotations: []any{}}
				at := mockLLMEvent{OutputIndex: &index, ItemID: item.ID, ContentIndex: &c}
				partAdded, delta, textDone, partDone := at, at, at, at
				partAdded.Type, partAdded.Part = "response.content_part.added", &empty
				delta.Type, delta.Delta = "response.output_text.delta", &content.Text
				textDone.Type, textDone.Text = "response.output_text.done", &content.Text
				partDone.Type, partDone.Part = "response.content_part.done", &content
				events = append(events, partAdded, delta, textDone, partDone)
			}
		case mockLLMFunctionCall:
			added.Arguments = ""
			arguments := stringOf(item.Arguments)
			events = append(events,
				mockLLMEvent{Type: "response.output_item.added", OutputIndex: &index, Item: &added},
				mockLLMEvent{Type: "response.function_call_arguments.delta", OutputIndex: &index, ItemID: item.ID, Delta: &arguments},
				mockLLMEvent{Type: "response.function_call_arguments.done", OutputIndex: &index, ItemID: item.ID, Arguments: &arguments, Name: item.Name})
		}
		done := item
		events = append(events, mockLLMEvent{Type: "response.output_item.done", OutputIndex: &index, Item: &done})
	}
	return append(events, mockLLMEvent{Type: "response.completed", Response: &response})
}

// writeMockLLMStream writes the stream as server-sent events, numbered in
// order, then [DONE].
func writeMockLLMStream(w http.ResponseWriter, response mockLLMResponse) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for seq, event := range mockLLMStream(response) {
		event.SequenceNumber = seq
		data, err := json.Marshal(event)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// codexMCPToolNamespace is the namespace the codex runtime puts an MCP
// server's tools in.
func codexMCPToolNamespace(server string) string {
	return "mcp__" + strings.ReplaceAll(server, "-", "_")
}

// mockLLMRequests is what the mockllm answered: the Responses API requests,
// and those no script entry matched (answered 404).
type mockLLMRequests struct {
	Answered  int `json:"answered"`
	Unmatched int `json:"unmatched"`
	// Log is one line per request: its status, the tools it offers and
	// its last input item, what the script matched or did not.
	Log []string `json:"log"`
	// Watched is, per request in Log's order, the watched names its body
	// carries anywhere: in the instructions or any input item, what the
	// runtime presented to the model.
	Watched [][]string `json:"watched"`
}

// mockLLMWatched is the watched names a request body carries.
func mockLLMWatched(body []byte, watch []string) []string {
	seen := []string{}
	for _, name := range watch {
		if bytes.Contains(body, []byte(name)) {
			seen = append(seen, name)
		}
	}
	return seen
}

// watchedSince is the requests from index from on that carry name, and how
// many requests there were from there.
func (r mockLLMRequests) watchedSince(from int, name string) (carried, requests int) {
	for i := from; i < len(r.Watched); i++ {
		requests++
		if slices.Contains(r.Watched[i], name) {
			carried++
		}
	}
	return carried, requests
}

// mockLLMLogItem bounds a logged input item.
const mockLLMLogItem = 400

// mockLLMRequest is what the mock reads of a Responses API request: its
// last input item as JSON (the input itself when it is a string), the one
// the script matches, whether it asks for a stream, the tools it offers, the
// texts of its message items by role (each role's joined by newlines), and
// its log line: the tools (a namespace's tools after its name) and that
// item.
func mockLLMRequest(body []byte) (last string, stream bool, tools []string, texts map[string]string, line string) {
	type tool struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	var req struct {
		Input  json.RawMessage `json:"input"`
		Stream bool            `json:"stream"`
		Tools  []tool          `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", false, nil, nil, "unreadable: " + err.Error()
	}
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, t.Name)
		name := firstNonEmpty(t.Name, t.Type)
		for _, inner := range t.Tools {
			name += " " + inner.Name
		}
		names = append(names, name)
	}
	last = string(req.Input)
	var items []json.RawMessage
	var text string
	switch {
	case json.Unmarshal(req.Input, &items) == nil && len(items) > 0:
		last = string(items[len(items)-1])
	case json.Unmarshal(req.Input, &text) == nil:
		last = text
	}
	texts = map[string]string{}
	for _, raw := range items {
		var item mockLLMInputItem
		if json.Unmarshal(raw, &item) != nil || item.Type != mockLLMMessage {
			continue
		}
		texts[item.Role] = strings.TrimPrefix(texts[item.Role]+"\n"+mockLLMText(item.Content), "\n")
	}
	return last, req.Stream, tools, texts, fmt.Sprintf("tools [%s] last %s", excerpt(strings.Join(names, ", "), mockLLMLogItem), excerpt(last, mockLLMLogItem))
}

// ServeMockLLM serves the script for the MCP tools on mcpServer on listen
// until ctx ends, recording which of the watch names each request carries.
func ServeMockLLM(ctx context.Context, listen, mcpServer, devctlArguments string, watch []string) error {
	handler, entries, err := mockLLMHandler(mcpServer, devctlArguments, watch)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	fmt.Printf("mockllm on %s (%d script entries, MCP tools of %s)\n", listen, entries, mcpServer)
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

// mockLLMHandler is the mockllm's API for the MCP tools on mcpServer: the
// Responses API under /v1, its health and its request counts with the watch
// names each request carries; entries is the script's length.
func mockLLMHandler(mcpServer, devctlArguments string, watch []string) (handler http.Handler, entries int, err error) {
	script, err := mockLLMConfig(mcpServer, devctlArguments)
	if err != nil {
		return nil, 0, err
	}
	var mu sync.Mutex
	var counts mockLLMRequests
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+mockLLMHealthPath, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "ok") })
	mux.HandleFunc("GET "+mockLLMRequestsPath, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(counts)
	})
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		last, stream, tools, texts, line := mockLLMRequest(body)
		response, matched := mockLLMMatch(script, last, texts, tools)
		status := http.StatusOK
		switch {
		case !matched:
			status = http.StatusNotFound
			http.Error(w, "no script entry matches the last input item: "+excerpt(last, mockLLMLogItem), status)
		case stream:
			writeMockLLMStream(w, response)
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		}
		line = fmt.Sprintf("%d %s", status, line)
		fmt.Println(line)
		mu.Lock()
		defer mu.Unlock()
		counts.Log = append(counts.Log, line)
		counts.Watched = append(counts.Watched, mockLLMWatched(body, watch))
		if matched {
			counts.Answered++
		} else {
			counts.Unmatched++
		}
	})
	return mux, len(script), nil
}

// mockLLMModelConfigYAML is the ModelConfig on the mockllm: the OpenAI
// provider on its Responses API, the key from the placeholder Secret.
const mockLLMModelConfigYAML = `apiVersion: %s
kind: ModelConfig
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
spec:
  provider: OpenAI
  model: %s
  apiKeySecret: %s
  apiKeySecretKey: %s
  openAI:
    baseUrl: %s
    apiFormat: responses
`

// ensureMockLLMModelConfig places the ModelConfig on the mockllm and its
// placeholder key, and waits for it Accepted. It names the Service, which
// startMockLLM points at the container once the agent's MCP binding is
// known.
func ensureMockLLMModelConfig() error {
	if err := ensureSecret(kagentNamespace, mockLLMSecret, corev1.SecretTypeOpaque, map[string][]byte{mockLLMSecretKey: []byte("agentlab-mockllm")}); err != nil {
		return fmt.Errorf("the mockllm's key Secret %s/%s: %w", kagentNamespace, mockLLMSecret, err)
	}
	gvr, err := modelConfigGVR()
	if err != nil {
		return err
	}
	mc := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(fmt.Sprintf(mockLLMModelConfigYAML, gvr.GroupVersion(), MockLLMModelConfig, kagentNamespace, managedByLabel, managedByAgentlabValue,
		mockLLMModel, mockLLMSecret, mockLLMSecretKey, mockLLMURL())), &mc.Object); err != nil {
		return err
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if _, err := k.apply(ctx, mc); err != nil {
		return fmt.Errorf("the ModelConfig %s on the mockllm: %w", MockLLMModelConfig, err)
	}
	return waitModelConfigAccepted(MockLLMModelConfig)
}

// mockLLM is the running mockllm: its container and Service.
type mockLLM struct {
	container     *fakeContainer
	removeService func()
}

// startMockLLM runs the mockllm for the MCP tools on mcpServer, watching
// for the watch names, its script with the devctl wait's turn when
// devctlArguments are given, and points the Service at it, proven reachable
// from a pod.
func startMockLLM(cfg *config.Config, binary, mcpServer, devctlArguments string, watch []string) (*mockLLM, error) {
	args := []string{"--mcp-server", mcpServer}
	if devctlArguments != "" {
		args = append(args, "--devctl-arguments", devctlArguments)
	}
	for _, name := range watch {
		args = append(args, "--watch", name)
	}
	c, err := startFakeContainer(cfg, binary, fakeContainerSpec{
		what: mockLLMWhat, suffix: mockLLMContainerSuffix, command: mockLLMCommand,
		healthPath: mockLLMHealthPath, args: args,
	})
	if err != nil {
		return nil, err
	}
	remove, err := fakeServiceForPods(cfg, mockLLMService, mockLLMWhat, c.podIP, fakeContainerPort, fakeServicePort,
		fmt.Sprintf("http://%s.%s.svc%s", mockLLMService, platformNamespace, mockLLMHealthPath))
	if err != nil {
		c.close()
		return nil, err
	}
	return &mockLLM{container: c, removeService: remove}, nil
}

// requests reads what the mockllm answered so far.
func (m *mockLLM) requests() (mockLLMRequests, error) {
	var counts mockLLMRequests
	resp, err := m.container.client.Get(m.container.hostURL + mockLLMRequestsPath)
	if err != nil {
		return counts, err
	}
	defer func() { _ = resp.Body.Close() }()
	return counts, json.NewDecoder(resp.Body).Decode(&counts)
}

// mockLLMLogTail is how many of the mockllm's request lines a failure
// quotes.
const mockLLMLogTail = 12

// logTail is the mockllm's last request lines, or why they cannot be read.
func (m *mockLLM) logTail() string {
	counts, err := m.requests()
	if err != nil {
		return "unread: " + err.Error()
	}
	log := counts.Log
	if len(log) > mockLLMLogTail {
		log = log[len(log)-mockLLMLogTail:]
	}
	return "  " + strings.Join(log, "\n  ")
}

func (m *mockLLM) close() {
	if m == nil {
		return
	}
	m.removeService()
	m.container.close()
}

// removeMockLLMModelConfig removes the ModelConfig and its Secret; best
// effort, a leftover is replaced by the next run.
func removeMockLLMModelConfig() {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if gvr, err := modelConfigGVR(); err == nil {
		if err := deleteObject(ctx, gvr, kagentNamespace, MockLLMModelConfig, 0); err != nil {
			note("cleanup: the ModelConfig %s: %v", MockLLMModelConfig, err)
		}
	}
	if err := deleteObject(ctx, gvrSecrets, kagentNamespace, mockLLMSecret, 0); err != nil {
		note("cleanup: the Secret %s/%s: %v", kagentNamespace, mockLLMSecret, err)
	}
}
