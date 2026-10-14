package lab

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// mcpAnswer is one answer of scriptedMuster: an HTTP status and its body.
type mcpAnswer struct {
	status int
	body   string
}

// mcpContentKey is an MCP result's content list.
const mcpContentKey = "content"

// mcpTextContent is an MCP content list of one text item, none for "".
func mcpTextContent(text string) []map[string]any {
	if text == "" {
		return []map[string]any{}
	}
	return []map[string]any{{fieldType: fieldText, fieldText: text}}
}

// callToolAnswer is muster's 200 answer to a call_tool whose result text is
// the target tool's envelope (nil: call_tool answers without the tool's
// result).
func callToolAnswer(t *testing.T, envelope map[string]any) mcpAnswer {
	t.Helper()
	var inner []byte
	if envelope != nil {
		var err error
		if inner, err = json.Marshal(envelope); err != nil {
			t.Fatal(err)
		}
	}
	return callToolText(t, string(inner))
}

// rpcAnswer is a JSON-RPC answer to request id with the given result.
func rpcAnswer(id int, result map[string]any) []byte {
	answer, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return answer
}

// callToolText is muster's 200 answer to a call_tool with the given result
// text.
func callToolText(t *testing.T, text string) mcpAnswer {
	t.Helper()
	return mcpAnswer{status: http.StatusOK, body: string(rpcAnswer(2, map[string]any{mcpContentKey: mcpTextContent(text)}))}
}

// toolResult is a target tool's envelope: its text, its isError verdict and,
// when given, its structuredContent.
func toolResult(text string, isError bool, structured map[string]any) map[string]any {
	env := map[string]any{"isError": isError, mcpContentKey: mcpTextContent(text)}
	if structured != nil {
		env["structuredContent"] = structured
	}
	return env
}

// scriptedMuster is a muster whose answer to every call_tool the script
// picks from the target tool's name and how often it was called before.
func scriptedMuster(t *testing.T, script func(tool string, n int) mcpAnswer) *musterSession {
	t.Helper()
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Arguments struct {
					Name string `json:"name"`
				} `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &rpc)
		mu.Lock()
		tool := rpc.Params.Arguments.Name
		a := script(tool, calls[tool])
		calls[tool]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	return &musterSession{client: srv.Client(), url: srv.URL, token: testToken}
}

// TestCallToolReadsHTTPStatusFirst: muster's refusal at the HTTP layer (a 401
// whose body is an OAuth error object) is an error naming the status and the
// body, not a malformed or empty tool result.
func TestCallToolReadsHTTPStatusFirst(t *testing.T) {
	const oauthError = `{"error":"invalid_token","error_description":"the access token expired"}`
	s := scriptedMuster(t, func(string, int) mcpAnswer {
		return mcpAnswer{status: http.StatusUnauthorized, body: oauthError}
	})
	_, err := s.callToolEnvelope(workspaceManagerToolPrefix+"get_workspace", nil)
	var httpErr *musterHTTPError
	if !errors.As(err, &httpErr) || httpErr.status != http.StatusUnauthorized {
		t.Fatalf("a 401: %v, want a musterHTTPError with status 401", err)
	}
	for _, want := range []string{"HTTP 401", "invalid_token", "the access token expired"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if errors.Is(err, errEmptyToolAnswer) || strings.Contains(err.Error(), "payload shape") {
		t.Errorf("a 401 read as a tool answer: %v", err)
	}
}

// TestCallToolEnvelopeEmptyAnswer: a call_tool answer without the tool's
// result is errEmptyToolAnswer; a result of another shape is not.
func TestCallToolEnvelopeEmptyAnswer(t *testing.T) {
	empty := scriptedMuster(t, func(string, int) mcpAnswer { return callToolAnswer(t, nil) })
	if _, err := empty.callToolEnvelope(workspaceManagerToolPrefix+"delete_workspace", nil); !errors.Is(err, errEmptyToolAnswer) {
		t.Errorf("an empty answer: %v, want errEmptyToolAnswer", err)
	}
	if _, err := empty.callServerTool(workspaceManagerToolPrefix+"get_workspace", nil); !errors.Is(err, errEmptyToolAnswer) {
		t.Errorf("an empty answer to a read: %v, want an error", err)
	}
	odd := scriptedMuster(t, func(string, int) mcpAnswer { return callToolText(t, "not an envelope") })
	_, err := odd.callToolEnvelope(workspaceManagerToolPrefix+"get_workspace", nil)
	if err == nil || errors.Is(err, errEmptyToolAnswer) || !strings.Contains(err.Error(), "payload shape") {
		t.Errorf("a malformed answer: %v, want the payload shape error", err)
	}
}
