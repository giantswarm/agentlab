package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kagent-dev/mockllm"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The codex coding proof's model (docs/platform.md "The coding agents
// proof"): kagent's mockllm, scripted for coding-test's turns, so `agentlab
// coding-test --harness codex` reaches a verdict on a lab with no OpenAI or
// Bedrock account. It runs the way a proof's fake runs (fakecontainer.go):
// `agentlab mockllm` in a container on the kind network, reached by the
// cluster as the Service agentlab-mockllm in the platform namespace, which
// the lab's ModelConfig agentlab-mockllm (OpenAI provider, Responses API)
// names as its base URL. The script answers each of the proof's prompts with
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
	mockLLMScriptEntry struct {
		Name     string          `json:"name"`
		Match    mockLLMMatch    `json:"match"`
		Response mockLLMResponse `json:"response"`
	}
	mockLLMMatch struct {
		MatchType string `json:"match_type"`
		Input     string `json:"input"`
	}
	mockLLMResponse struct {
		ID                string           `json:"id"`
		Object            string           `json:"object"`
		CreatedAt         int64            `json:"created_at"`
		Model             string           `json:"model"`
		Output            []mockLLMItem    `json:"output"`
		ParallelToolCalls bool             `json:"parallel_tool_calls"`
		Tools             []any            `json:"tools"`
		Usage             map[string]int64 `json:"usage"`
	}
	mockLLMItem struct {
		ID        string           `json:"id"`
		Type      string           `json:"type"`
		Status    string           `json:"status"`
		Role      string           `json:"role,omitempty"`
		Content   []mockLLMContent `json:"content,omitempty"`
		CallID    string           `json:"call_id,omitempty"`
		Execution string           `json:"execution,omitempty"`
		Namespace string           `json:"namespace,omitempty"`
		Name      string           `json:"name,omitempty"`
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
	}
	mockLLMSearchArguments struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
)

// mockLLMItemDone is the status of every item the script answers with,
// mockLLMMessage the type of a reply's item and of a person's input.
const (
	mockLLMItemDone = "completed"
	mockLLMMessage  = "message"
)

// mockLLMConfig is the mockllm's configuration for the script, the MCP
// tools on mcpServer: per turn the tool call answering the prompt, then the
// reply answering the tool's output (matched by its call id, which the
// runtime's last input item carries).
func mockLLMConfig(mcpServer string) (mockllm.Config, error) {
	var entries []mockLLMScriptEntry
	for _, turn := range mockLLMScript() {
		callID := "call_agentlab_" + turn.name
		if turn.command != "" {
			arguments, err := json.Marshal(mockLLMShellArguments{Command: turn.command})
			if err != nil {
				return mockllm.Config{}, err
			}
			entries = append(entries, mockLLMEntry(turn.name, turn.prompt, mockLLMItem{
				ID: "fc_agentlab_" + turn.name, Type: "function_call", Status: mockLLMItemDone,
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
					ID: "fc_agentlab_" + turn.name, Type: "function_call", Status: mockLLMItemDone,
					CallID: callID, Namespace: namespace, Name: turn.tool, Arguments: "{}",
				}))
		}
		entries = append(entries, mockLLMEntry(turn.name+"_reply", callID, mockLLMItem{
			ID: "msg_agentlab_" + turn.name, Type: mockLLMMessage, Status: mockLLMItemDone, Role: "assistant",
			Content: []mockLLMContent{{Type: "output_text", Text: turn.reply, Annotations: []any{}}},
		}))
	}
	raw, err := json.Marshal(struct {
		OpenAIResponse []mockLLMScriptEntry `json:"openai_response"`
	}{entries})
	if err != nil {
		return mockllm.Config{}, err
	}
	var cfg mockllm.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return mockllm.Config{}, fmt.Errorf("the mockllm script: %w", err)
	}
	return cfg, nil
}

// mockLLMEntry is one Responses API answer of the script: output, with the
// usage the turn's status reports, for a request whose last input item
// contains match.
func mockLLMEntry(name, match string, output mockLLMItem) mockLLMScriptEntry {
	return mockLLMScriptEntry{
		Name:  "agentlab_" + name,
		Match: mockLLMMatch{MatchType: "contains", Input: match},
		Response: mockLLMResponse{
			ID: "resp_agentlab_" + name, Object: "response", CreatedAt: 1_760_000_000, Model: mockLLMModel,
			Output: []mockLLMItem{output}, Tools: []any{},
			Usage: map[string]int64{"input_tokens": 20, "output_tokens": 10, "total_tokens": 30},
		},
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
	cfg, err := mockLLMConfig(mcpServer)
	if err != nil {
		return nil, 0, err
	}
	provider := mockllm.NewOpenAIResponseProvider(cfg.OpenAIResponse)
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
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		provider.Handle(rec, r)
		mu.Lock()
		defer mu.Unlock()
		if rec.status == http.StatusNotFound {
			counts.Unmatched++
		} else {
			counts.Answered++
		}
	})
	return mux, len(cfg.OpenAIResponse), nil
}

// statusRecorder records the status a handler answered with; Flush passes
// through for the streamed answers.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
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
