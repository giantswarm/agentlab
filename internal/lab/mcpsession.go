package lab

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
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
		"params": map[string]any{"name": name, "arguments": args},
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
	res, err := s.callTool("call_tool", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	var env toolEnvelope
	inner := innerText(res)
	if err := json.Unmarshal([]byte(inner), &env); err != nil || len(env.Content) == 0 {
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
