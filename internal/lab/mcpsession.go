package lab

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// JSON keys the proofs pass to and read from the platform's tools, named once
// so the same word is not spelled in a dozen places (nameKey lives in
// kubeconfig.go).
const (
	modelConfigKey  = "modelConfig"
	descriptionKey  = "description"
	serverKey       = "server"
	resourceTypeKey = "resourceType"
	argsKey         = "args"
	// presetFullName is the built-in preset that resolves to the whole
	// catalogue; presetFull (toolsetstest.go) is its selector.
	presetFullName     = "full"
	presetNoneName     = "none"
	presetReadOnlyName = "read-only"
	// toolKey names a tool: a workflow step's, a ToolInfo kind.
	toolKey = "tool"
	// resourceNamespaces is the mcp-kubernetes resourceType the proofs list:
	// every user may, and the answer names the platform namespace.
	resourceNamespaces = "namespaces"
	// taskStateFailed is the terminal failure state of a model-manager job
	// and of a scaffolder task alike.
	taskStateFailed = "failed"
)

// musterSession is one MCP Streamable-HTTP session against the lab muster
// through the edge, authenticated with a Dex id_token as Bearer (muster lists
// the agent-platform client under trustedAudiences). The same path Claude
// Code takes after its browser login; PlatformTest carries an inline copy of
// this dance, the models proof reuses it through this helper.
type musterSession struct {
	client *http.Client
	url    string
	token  string
	id     string
	seq    int
	// headers are sent on every request of the session on top of the MCP
	// ones — the way an agent's kagent runtime sends the X-Muster-Toolset
	// header its Agent CR declares (headersFrom). Set per request through
	// setHeader; muster reads the header on each request, never binds it to
	// the session.
	headers map[string]string
}

// setHeader adds (or, with an empty value, removes) a header the session
// sends on every following request.
func (s *musterSession) setHeader(name, value string) {
	if s.headers == nil {
		s.headers = map[string]string{}
	}
	if value == "" {
		delete(s.headers, name)
		return
	}
	s.headers[name] = value
}

// openMusterSession initializes an MCP session and returns it ready for
// tools/call requests.
func openMusterSession(cfg *config.Config, token, clientName string) (*musterSession, error) {
	client, err := labHTTPClient(60 * time.Second)
	if err != nil {
		return nil, err
	}
	s := &musterSession{client: client, url: cfg.MusterBaseURL() + "/mcp", token: token}
	resp, err := s.post("", fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":%q,"version":"1"}}}`, clientName))
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.id = resp.Header.Get("Mcp-Session-Id")
	if s.id == "" {
		return nil, fmt.Errorf("no MCP session id — muster rejected the token:\n%.300s", strings.TrimSpace(string(body)))
	}
	if resp, err := s.post(s.id, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	s.seq = 1
	return s, nil
}

func (s *musterSession) post(sessionID, payload string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, s.url, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	for name, value := range s.headers {
		req.Header.Set(name, value)
	}
	return s.client.Do(req)
}

// callTool runs one MCP tools/call and returns the parsed JSON-RPC response.
func (s *musterSession) callTool(name string, args map[string]any) (map[string]any, error) {
	if args == nil {
		args = map[string]any{}
	}
	s.seq++
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": s.seq, "method": "tools/call",
		"params": map[string]any{nameKey: name, "arguments": args},
	})
	if err != nil {
		return nil, err
	}
	resp, err := s.post(s.id, string(payload))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	parsed, err := parseMCPResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing muster response to %s: %w\n%.300s", name, err, strings.TrimSpace(string(raw)))
	}
	if rpcErr, ok := parsed["error"].(map[string]any); ok {
		return nil, fmt.Errorf("muster %s: %v", name, rpcErr["message"])
	}
	return parsed, nil
}

// listTools returns the names of every tool muster aggregates (its core
// list_tools tool, the way the Backstage muster plugin and Claude Code see it).
func (s *musterSession) listTools() ([]string, error) {
	res, err := s.callTool("list_tools", nil)
	if err != nil {
		return nil, err
	}
	var toolList struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(innerText(res)), &toolList); err != nil {
		return nil, fmt.Errorf("parsing list_tools payload: %w", err)
	}
	names := make([]string, 0, len(toolList.Tools))
	for _, t := range toolList.Tools {
		names = append(names, t.Name)
	}
	return names, nil
}

// toolEnvelope is the target tool's full result as muster's call_tool
// meta-tool serialises it into result.content[0].text: the tool's own
// content, its isError verdict and, for tools that carry machine-readable
// output (core_auth_login's sign-in URL), structuredContent.
type toolEnvelope struct {
	IsError bool `json:"isError"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent map[string]any `json:"structuredContent"`
}

// callToolEnvelope runs one tool through muster's call_tool — the way every
// aggregated server tool (x_<server>_<tool>) and every core_* tool is reached
// — and returns the tool's full result envelope; the caller judges isError.
func (s *musterSession) callToolEnvelope(name string, args map[string]any) (*toolEnvelope, error) {
	if args == nil {
		args = map[string]any{}
	}
	res, err := s.callTool("call_tool", map[string]any{nameKey: name, "arguments": args})
	if err != nil {
		return nil, err
	}
	var env toolEnvelope
	inner := innerText(res)
	if err := json.Unmarshal([]byte(inner), &env); err != nil || len(env.Content) == 0 {
		// A refusal by muster itself — a tool outside the request's toolset,
		// an unknown tool — is call_tool's own error result: plain text, no
		// inner envelope. Hand it back as one so callers judge isError alike.
		result, _ := res["result"].(map[string]any)
		if isErr, _ := result["isError"].(bool); isErr && strings.TrimSpace(inner) != "" {
			env = toolEnvelope{IsError: true}
			env.Content = append(env.Content, struct {
				Text string `json:"text"`
			}{Text: inner})
			return &env, nil
		}
		return nil, fmt.Errorf("unexpected call_tool payload shape for %s: %.300s", name, inner)
	}
	return &env, nil
}

// callServerTool runs one aggregated server tool (x_<server>_<tool>) through
// muster's call_tool and returns the tool's text payload. Tool results are
// double-wrapped — result.content[0].text is JSON whose own content[0].text
// is the actual payload — and the inner isError flag is the tool's verdict.
func (s *musterSession) callServerTool(name string, args map[string]any) (string, error) {
	env, err := s.callToolEnvelope(name, args)
	if err != nil {
		return "", err
	}
	if env.IsError {
		return "", fmt.Errorf("%s failed: %.300s", name, env.Content[0].Text)
	}
	return env.Content[0].Text, nil
}

// toolAnnotations are the MCP tool annotations muster forwards from the
// serving MCPServer (or derives, for a workflow whose step tools are all
// read-only). Pointers: an absent hint is not a false one.
type toolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

// readOnly reports whether the tool is annotated read-only.
func (a *toolAnnotations) readOnly() bool {
	return a != nil && a.ReadOnlyHint != nil && *a.ReadOnlyHint
}

// toolInfo is one entry of list_tools / filter_tools (muster's ToolInfo):
// the exposed name, the owning server (empty for core tools and workflows),
// the kind (tool | workflow | core) and the annotations, when any.
type toolInfo struct {
	Name        string           `json:"name"`
	Server      string           `json:"server,omitempty"`
	Kind        string           `json:"kind,omitempty"`
	Annotations *toolAnnotations `json:"annotations,omitempty"`
}

// presetInfo is one entry of filter_tools' presets list.
type presetInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	BuiltIn     bool   `json:"built_in"`
}

// filterToolsResponse is muster's FilterToolsResponse as the toolset feature
// extended it: the tools resolved for the caller, the selectors echoed back,
// the selectors that matched nothing for this caller, the presets when asked
// for, and the size of the catalogue the filter ran over (within the
// toolset, when one applies).
type filterToolsResponse struct {
	Tools            []toolInfo   `json:"tools"`
	Toolset          []string     `json:"toolset"`
	ToolsetUnmatched []string     `json:"toolset_unmatched"`
	Presets          []presetInfo `json:"presets"`
	TotalTools       int          `json:"total_tools"`
	FilteredCount    int          `json:"filtered_count"`
	Truncated        bool         `json:"truncated"`
}

// names returns the tool names, sorted.
func (r *filterToolsResponse) names() []string {
	out := make([]string, 0, len(r.Tools))
	for _, t := range r.Tools {
		out = append(out, t.Name)
	}
	slices.Sort(out)
	return out
}

// filterTools runs muster's filter_tools meta-tool with the given arguments
// (plus a limit high enough to never truncate — the default is 5) and
// decodes the response. A toolset error (unknown preset, reserved selector,
// empty header) is an error result and comes back as err.
func (s *musterSession) filterTools(args map[string]any) (*filterToolsResponse, error) {
	if args == nil {
		args = map[string]any{}
	}
	if _, ok := args["limit"]; !ok {
		args["limit"] = 1000
	}
	res, err := s.callTool("filter_tools", args)
	if err != nil {
		return nil, err
	}
	result, _ := res["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); isErr {
		return nil, fmt.Errorf("filter_tools: %s", strings.TrimSpace(innerText(res)))
	}
	var out filterToolsResponse
	if err := json.Unmarshal([]byte(innerText(res)), &out); err != nil {
		return nil, fmt.Errorf("parsing filter_tools payload: %w\n%.300s", err, innerText(res))
	}
	return &out, nil
}

// describeToolResponse is the part of muster's DescribeToolResponse the
// proofs read: the name, the owning server, the kind and the annotations.
type describeToolResponse struct {
	Name        string           `json:"name"`
	Server      string           `json:"server,omitempty"`
	Kind        string           `json:"kind,omitempty"`
	Annotations *toolAnnotations `json:"annotations,omitempty"`
}

// describeTool runs muster's describe_tool meta-tool. A tool outside the
// request's toolset is an error result and comes back as err.
func (s *musterSession) describeTool(name string) (*describeToolResponse, error) {
	res, err := s.callTool("describe_tool", map[string]any{nameKey: name})
	if err != nil {
		return nil, err
	}
	result, _ := res["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); isErr {
		return nil, fmt.Errorf("describe_tool %s: %s", name, strings.TrimSpace(innerText(res)))
	}
	var out describeToolResponse
	if err := json.Unmarshal([]byte(innerText(res)), &out); err != nil {
		return nil, fmt.Errorf("parsing describe_tool payload: %w\n%.300s", err, innerText(res))
	}
	return &out, nil
}
