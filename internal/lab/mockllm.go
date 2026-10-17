package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
// itself is the code word the resumed turn recalls.

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
	// calls instead (on the agent's binding, found through tool search).
	command, tool string
	reply         string
}

// mockLLMScript is the script of the proof's codex turns. The prompts are
// substrings of the proof's own (codingprobe.go) the others do not carry.
func mockLLMScript() []mockLLMTurn {
	codex := codingRuntimes[config.CodingRuntimeCodex]
	return []mockLLMTurn{
		{name: "sandbox", prompt: "own sandbox check", command: codingSandboxCommand(codex), reply: "The sandbox check ran; its readings are the command's output."},
		{name: "mcp", prompt: "Do not use a shell for this", tool: codingTestReadTool, reply: "The tool answered; its answer is the call's output."},
		{name: "codeword", prompt: "Remember the code word", command: codingCodeWordCommand(), reply: "The file is written."},
		{name: "resume", prompt: "the code word I asked you to remember", command: codingResumeCommand(), reply: probeMarker + " code-word=" + codingTestCodeWord},
	}
}

// The script's wire form, the Responses API's own field names.
type (
	// mockLLMScriptEntry answers a request whose last input item
	// contains match with response.
	mockLLMScriptEntry struct {
		match    string
		response mockLLMResponse
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
	mockLLMSearchArguments struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
)

// mockLLMCommandTimeout is the milliseconds a scripted shell command may
// take: the sandbox probe's token search alone may take a minute.
const mockLLMCommandTimeout = 300_000

// mockLLMItemDone is the status of every item the script answers with;
// the item types and a reply's content type.
const (
	mockLLMItemDone     = "completed"
	mockLLMMessage      = "message"
	mockLLMFunctionCall = "function_call"
	mockLLMOutputText   = "output_text"
)

// mockLLMConfig is the script for the MCP tools on mcpServer: per turn the
// tool call answering the prompt, then the reply answering the tool's output
// (matched by its call id, which the runtime's last input item carries).
func mockLLMConfig(mcpServer string) ([]mockLLMScriptEntry, error) {
	var entries []mockLLMScriptEntry
	for _, turn := range mockLLMScript() {
		callID := "call_agentlab_" + turn.name
		if turn.command != "" {
			arguments, err := json.Marshal(mockLLMShellArguments{Command: turn.command, TimeoutMS: mockLLMCommandTimeout})
			if err != nil {
				return nil, err
			}
			entries = append(entries, mockLLMEntry(turn.name, turn.prompt, mockLLMItem{
				ID: "fc_agentlab_" + turn.name, Type: mockLLMFunctionCall, Status: mockLLMItemDone,
				CallID: callID, Name: "shell_command", Arguments: string(arguments),
			}))
		} else {
			// codex defers an MCP server's tools behind its tool search:
			// the search first, then the call in the server's namespace.
			namespace := codexMCPToolNamespace(mcpServer)
			searchID := "search_agentlab_" + turn.name
			entries = append(entries,
				mockLLMEntry(turn.name, turn.prompt, mockLLMItem{
					ID: "ts_agentlab_" + turn.name, Type: "tool_search_call", Status: mockLLMItemDone,
					CallID: searchID, Execution: "client",
					Arguments: mockLLMSearchArguments{Query: namespace + " " + turn.tool, Limit: 10},
				}),
				mockLLMEntry(turn.name+"_call", searchID, mockLLMItem{
					ID: "fc_agentlab_" + turn.name, Type: mockLLMFunctionCall, Status: mockLLMItemDone,
					CallID: callID, Namespace: namespace, Name: turn.tool, Arguments: "{}",
				}))
		}
		entries = append(entries, mockLLMEntry(turn.name+"_reply", callID, mockLLMItem{
			ID: "msg_agentlab_" + turn.name, Type: mockLLMMessage, Status: mockLLMItemDone, Role: "assistant",
			Content: []mockLLMContent{{Type: mockLLMOutputText, Text: turn.reply, Annotations: []any{}}},
		}))
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

// mockLLMMatch is the script's answer to a request: the first entry whose
// match the last input item (as JSON; the input itself when it is a
// string) contains, false for none.
func mockLLMMatch(script []mockLLMScriptEntry, last string) (mockLLMResponse, bool) {
	for _, entry := range script {
		if strings.Contains(last, entry.match) {
			return entry.response, true
		}
	}
	return mockLLMResponse{}, false
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
}

// mockLLMLogItem bounds a logged input item.
const mockLLMLogItem = 400

// mockLLMRequest is what the mock reads of a Responses API request: its
// last input item as JSON (the input itself when it is a string), the one
// the script matches, whether it asks for a stream, and its log line: the
// tools it offers the model (a namespace's tools after its name) and that
// item.
func mockLLMRequest(body []byte) (last string, stream bool, line string) {
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
		return "", false, "unreadable: " + err.Error()
	}
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
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
	return last, req.Stream, fmt.Sprintf("tools [%s] last %s", excerpt(strings.Join(names, ", "), mockLLMLogItem), excerpt(last, mockLLMLogItem))
}

// ServeMockLLM serves the script for the MCP tools on mcpServer on listen
// until ctx ends.
func ServeMockLLM(ctx context.Context, listen, mcpServer string) error {
	handler, entries, err := mockLLMHandler(mcpServer)
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
// Responses API under /v1, its health and its request counts; entries is
// the script's length.
func mockLLMHandler(mcpServer string) (handler http.Handler, entries int, err error) {
	script, err := mockLLMConfig(mcpServer)
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
		last, stream, line := mockLLMRequest(body)
		response, matched := mockLLMMatch(script, last)
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

// startMockLLM runs the mockllm for the MCP tools on mcpServer and points
// the Service at it, proven reachable from a pod.
func startMockLLM(cfg *config.Config, binary, mcpServer string) (*mockLLM, error) {
	c, err := startFakeContainer(cfg, binary, fakeContainerSpec{
		what: mockLLMWhat, suffix: mockLLMContainerSuffix, command: mockLLMCommand,
		healthPath: mockLLMHealthPath, args: []string{"--mcp-server", mcpServer},
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
