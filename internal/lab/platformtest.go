package lab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
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
		return fmt.Errorf("muster is not reachable at %s — run `agentlab platform` first", cfg.MusterBaseURL())
	}

	step("Logging in to Dex as %s", email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
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
	_ = resp.Body.Close()
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		return fmt.Errorf("no session id — muster rejected the token:\n%s", strings.TrimSpace(string(body)))
	}
	note("session %s", sessionID)
	if resp, err := post(sessionID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	call := func(payload string) (map[string]any, error) {
		resp, err := post(sessionID, payload)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
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
	// The umbrella's bundled MCPServer CR declares no family, so muster uses
	// per-server prefixing: x_<server>_<tool>, no management_cluster argument.
	toolPrefix := "x_" + cfg.MCPServerName() + "_"
	shown := 0
	for _, t := range toolList.Tools {
		if strings.HasPrefix(t.Name, toolPrefix) && shown < 8 {
			note("%s", t.Name)
			shown++
		}
	}
	if shown == 0 {
		return fmt.Errorf("muster aggregates no %s tools", toolPrefix)
	}

	step("Calling %slist namespaces through muster", toolPrefix)
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"call_tool","arguments":{"name":%q,"arguments":{"resourceType":"namespaces"}}}}`, toolPrefix+"list")
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

	verdict := "PASS: Claude Code -> muster (Dex) -> mcp-kubernetes -> kind apiserver"
	if cfg.Platform.Observability {
		// Same singleton prefixing as mcp-kubernetes: the lab's mcpServers
		// entry deliberately keeps the server out of muster's families
		// (see agent-platform-values.yaml.tmpl).
		promPrefix := "x_" + mcpPrometheusRelease + "_"
		step("Prometheus tools muster is aggregating")
		shown = 0
		for _, t := range toolList.Tools {
			if strings.HasPrefix(t.Name, promPrefix) && shown < 8 {
				note("%s", t.Name)
				shown++
			}
		}
		if shown == 0 {
			return fmt.Errorf("muster aggregates no %s tools", promPrefix)
		}

		// promQL runs one instant query through muster and returns the tool's
		// rendered answer (result.content[0].text is JSON whose own
		// content[0].text is the actual payload — the same two decode hops as
		// call_tool above).
		promQL := func(query string) (string, error) {
			q, _ := json.Marshal(query)
			payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"call_tool","arguments":{"name":%q,"arguments":{"query":%s}}}}`, promPrefix+"execute_query", q)
			res, err := call(payload)
			if err != nil {
				return "", err
			}
			var wrapped struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal([]byte(innerText(res)), &wrapped); err != nil || len(wrapped.Content) == 0 {
				return "", fmt.Errorf("unexpected execute_query payload shape")
			}
			return wrapped.Content[0].Text, nil
		}

		// `up` is non-empty as soon as Prometheus completes its first scrape,
		// so a short retry absorbs a just-booted lab.
		step("Calling %sexecute_query (PromQL: up) through muster", promPrefix)
		var inner string
		queried := waitFor(15, 4*time.Second, func() bool {
			var err error
			if inner, err = promQL("up"); err != nil {
				return false
			}
			// The tool renders the Prometheus query result as text; an answer
			// with scrape targets in it proves collection AND the query path.
			return strings.Contains(inner, "up")
		})
		if !queried {
			return fmt.Errorf("execute_query never returned scrape targets (last payload: %.200s)", inner)
		}
		if len(inner) > 160 {
			inner = inner[:160] + "..."
		}
		note("query result: %s", strings.ReplaceAll(inner, "\n", " "))

		// The platform's own monitors: muster ServiceMonitor, valkey
		// PodMonitor and mcp-prometheus's ServiceMonitor (plus kagent's when
		// agents run) are enabled with observability, and the lab Prometheus
		// selects monitors from every release
		// (…NilUsesHelmValues: false, kube-prometheus-stack-values.yaml.tmpl).
		// A fresh install needs the operator to reload targets plus one 30s
		// scrape interval, hence the generous retry.
		expected := []string{componentMuster, "valkey", mcpPrometheusRelease}
		if cfg.Platform.Agents {
			expected = append(expected, "kagent")
		}
		step("Verifying Prometheus scrapes the platform itself (%s)", strings.Join(expected, ", "))
		var missing []string
		scraped := waitFor(30, 5*time.Second, func() bool {
			series, err := promQL(`up{namespace=~"agent-platform|kagent|monitoring"} == 1`)
			if err != nil {
				return false
			}
			missing = missing[:0]
			for _, want := range expected {
				if !strings.Contains(series, want) {
					missing = append(missing, want)
				}
			}
			return len(missing) == 0
		})
		if !scraped {
			return fmt.Errorf("prometheus is not scraping %s;\n"+
				"check `kubectl -n %s get servicemonitors,podmonitors -A` and the targets via %sget_targets",
				strings.Join(missing, ", "), platformNamespace, promPrefix)
		}
		note("all platform targets are up: %s", strings.Join(expected, ", "))
		verdict += "\nPASS: Claude Code -> muster (Dex) -> mcp-prometheus -> Prometheus (platform targets scraped)"

		// The Backstage metrics path: gs-backend's MimirService queries
		// https://observability.<domain>/prometheus/api/v1/query — the lab
		// serves it via observability-route.yaml.tmpl. Run the exact
		// workload query shape the Deployments page uses and expect the
		// muster deployment in the answer (present whenever the platform
		// runs, unlike backstage's own).
		obsQueryURL := cfg.ObservabilityBaseURL() + "/api/v1/query?query=" +
			url.QueryEscape(`max without(app, container, customer, endpoint, instance, job, pipeline, pod, provider, region, service, service_priority) (kube_deployment_spec_replicas)`)
		step("Querying the Backstage metrics endpoint on the edge (%s)", cfg.ObservabilityBaseURL())
		body := ""
		answered := waitFor(10, 3*time.Second, func() bool {
			resp, err := client.Get(obsQueryURL)
			if err != nil {
				return false
			}
			defer func() { _ = resp.Body.Close() }()
			raw, _ := io.ReadAll(resp.Body)
			body = string(raw)
			return resp.StatusCode == http.StatusOK &&
				strings.Contains(body, `"deployment":"muster"`)
		})
		if !answered {
			return fmt.Errorf("the edge observability endpoint never answered the Deployments-page query "+
				"(last body: %.200s);\ncheck `kubectl -n monitoring get httproute observability` and the edge",
				body)
		}
		note("the Deployments-page query answers through the edge (deployment=muster found)")
		verdict += "\nPASS: Backstage metrics path -> edge -> Prometheus (Mimir-shaped /prometheus API)"
	}

	fmt.Println()
	fmt.Println(verdict)
	return nil
}

// musterTokenProbe is the smallest end-to-end auth check: a Dex password
// grant for the admin user, then an MCP initialize with the id_token as
// Bearer. A session id back means muster's whole token-validation chain
// (JWKS / userinfo over the lab CA) works. PlatformTest proves the same and
// more; this exists so `up` can verify and heal cheaply (see
// ensureMusterValidatesTokens).
func musterTokenProbe(cfg *config.Config) error {
	admin := cfg.AdminUser()
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		admin.Email, admin.Password, "openid email groups profile")
	if err != nil {
		return err
	}
	client, err := labHTTPClient(10 * time.Second)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, cfg.MusterBaseURL()+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agentlab-up","version":"1"}}}`))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Mcp-Session-Id") == "" {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200] + "..."
		}
		return fmt.Errorf("muster rejected the token: %s", msg)
	}
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
	for line := range strings.SplitSeq(string(trimmed), "\n") {
		line = strings.TrimSpace(line)
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			if json.Unmarshal([]byte(strings.TrimSpace(data)), &out) == nil {
				return out, nil
			}
		}
	}
	return nil, fmt.Errorf("neither JSON nor SSE data frame")
}
