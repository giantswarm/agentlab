package lab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dexlab/internal/config"
)

// PlatformTest is the headless end-to-end proof: Dex login -> muster
// (OAuth-protected) -> the Kubernetes MCP -> the kind apiserver.
//
// Uses the Dex password grant with client_id=muster, which yields an id_token
// with aud=muster. muster accepts it directly because `muster` is listed under
// oauth.server.trustedAudiences. Claude Code instead does the full browser
// authorization-code flow; this is the CI-friendly shortcut.
func PlatformTest(cfg *config.Config, email string) error {
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}

	client, err := labHTTPClient(30 * time.Second)
	if err != nil {
		return err
	}
	if !httpUp(client, cfg.MusterBaseURL()+"/.well-known/oauth-authorization-server") {
		return fmt.Errorf("muster is not reachable at %s — run `dexlab platform` first", cfg.MusterBaseURL())
	}

	step("Logging in to Dex as %s", email)
	token, err := passwordGrant(cfg, "muster", config.MusterClientSecret,
		user.Email, user.Password, "openid email groups profile")
	if err != nil {
		return err
	}
	note("got an id_token")

	mcpURL := cfg.MusterBaseURL() + "/mcp"
	post := func(sessionID, payload string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPost, mcpURL, strings.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
		}
		return client.Do(req)
	}

	step("MCP initialize")
	resp, err := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"platform-test","version":"1"}}}`)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		return fmt.Errorf("no session id — muster rejected the token:\n%s", strings.TrimSpace(string(body)))
	}
	note("session %s", sessionID)
	if resp, err := post(sessionID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	call := func(payload string) (map[string]any, error) {
		resp, err := post(sessionID, payload)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		parsed, err := parseMCPResponse(raw)
		if err != nil {
			return nil, fmt.Errorf("parsing muster response: %w\n%s", err, strings.TrimSpace(string(raw)))
		}
		return parsed, nil
	}

	step("Kubernetes tools muster is aggregating")
	res, err := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_tools","arguments":{}}}`)
	if err != nil {
		return err
	}
	var toolList struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(innerText(res)), &toolList); err != nil {
		return fmt.Errorf("parsing list_tools payload: %w", err)
	}
	shown := 0
	for _, t := range toolList.Tools {
		if strings.HasPrefix(t.Name, "x_kubernetes_") && shown < 8 {
			note("%s", t.Name)
			shown++
		}
	}
	if shown == 0 {
		return fmt.Errorf("muster aggregates no x_kubernetes_ tools")
	}

	// The `kubernetes` group is rendered as a muster FAMILY (instanceArg
	// management_cluster), so every call must name the backing MCPServer CR.
	step("Calling x_kubernetes_list namespaces through muster")
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"call_tool","arguments":{"name":"x_kubernetes_list","arguments":{"management_cluster":%q,"resourceType":"namespaces"}}}}`, cfg.MCPServerName())
	res, err = call(payload)
	if err != nil {
		return err
	}
	// Tool results are double-wrapped: result.content[0].text is JSON whose
	// content[0].text is the actual payload — two decode hops.
	var wrapped struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(innerText(res)), &wrapped); err != nil || len(wrapped.Content) == 0 {
		return fmt.Errorf("unexpected call_tool payload shape")
	}
	var nsList struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(wrapped.Content[0].Text), &nsList); err != nil {
		return fmt.Errorf("parsing namespace list: %w", err)
	}
	names := make([]string, 0, len(nsList.Items))
	for _, item := range nsList.Items {
		names = append(names, item.Name)
	}
	note("namespaces: %s", strings.Join(names, ", "))

	fmt.Println()
	fmt.Println("PASS: Claude Code -> muster (Dex) -> mcp-kubernetes -> kind apiserver")
	return nil
}

// innerText pulls result.content[0].text out of a JSON-RPC tool response.
func innerText(rpc map[string]any) string {
	result, _ := rpc["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

// parseMCPResponse handles both plain-JSON and SSE-framed (`data: {...}`)
// MCP responses.
func parseMCPResponse(raw []byte) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	var out map[string]any
	if json.Unmarshal(trimmed, &out) == nil {
		return out, nil
	}
	for _, line := range strings.Split(string(trimmed), "\n") {
		line = strings.TrimSpace(line)
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			if json.Unmarshal([]byte(strings.TrimSpace(data)), &out) == nil {
				return out, nil
			}
		}
	}
	return nil, fmt.Errorf("neither JSON nor SSE data frame")
}
