package lab

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// The Agent Platform pages on kagent main, through the routes the portal's
// backend (plugins/agent-platform-backend) serves the frontend: the agents
// list — AgentTemplates joined with their Ready Harnesses — and a chat turn:
// a session (the user's AgentInstance) and one message (an A2A SendMessage),
// every call carrying the user's forwarded Dex id_token in
// backstage-kagent-authorization, so kagent attributes them to the person.
// The sessions routes and their bodies are the released plugin's; the
// agent-templates route is the portal's kagent-main addition
// (`GET /kagent/agent-templates?installation=&namespace=`, answering the
// `{error, data: [...]}` envelope with one summary per AgentTemplate: `ref`,
// `harnesses[{name, ready}]`, `ready`, the CR under `resource`). A portal that
// still speaks kagent 0.10 answers 404 to it — reported as such, not as a crash.

const (
	// portalKagentAuthHeader carries the user's Dex id_token to the portal's
	// agent-platform backend (KAGENT_AUTH_HEADER in plugins/agent-platform).
	portalKagentAuthHeader = "backstage-kagent-authorization"
	portalKagentAPI        = "/api/agent-platform/kagent"
	portalAgentsPath       = portalKagentAPI + "/agent-templates?namespace=" + kagentNamespace
	portalSessionsPath     = portalKagentAPI + "/sessions"
	portalChatPrompt       = "Reply with exactly the word pong and nothing else."
	portalSessionName      = "agentlab backstage-test"
	// backstageTestAgent is the AgentTemplate the proof brings along: the
	// list must show it, the chat turn runs on it.
	backstageTestAgent = "agentlab-backstage-test"
)

// kagentRequest is one call to the portal's agent-platform backend as the
// user: the Backstage identity token, the installation, and the user's Dex
// id_token in the header the backend promotes to kagent.
func (ps *portalSession) kagentRequest(method, path string, body any) (int, []byte, error) {
	var payload string
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = string(raw)
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	req, err := http.NewRequest(method, ps.cfg.BackstageBaseURL()+path+separator+"installation="+platformRelease, strings.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+ps.bsToken)
	req.Header.Set(portalKagentAuthHeader, ps.dexIDToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ps.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

// portalAgentRow is one agent of the portal's list as the proof reads it.
type portalAgentRow struct {
	name      string
	namespace string
	ready     bool
	// readiness is what the row said, for the note.
	readiness string
}

// portalAgentRows reads the agents list off the route's payload — a list, or
// an object carrying one under `data` (the portal's envelope), `agents` or
// `items` — each row naming the agent (`ref.name`/`ref.namespace`, `name` and
// `namespace`, or metadata.name) with its readiness: a `ready` bool, or a
// state/status/readiness string that reads Ready.
func portalAgentRows(payload any) ([]portalAgentRow, error) {
	items, ok := payload.([]any)
	if !ok {
		m, isMap := payload.(map[string]any)
		if !isMap {
			return nil, fmt.Errorf("the agents payload is a %T, neither a list nor an object", payload)
		}
		for _, key := range []string{"data", "agents", "items"} {
			if list, found := m[key].([]any); found {
				items, ok = list, true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("the agents payload carries no agents/items list (keys %v)", mapKeys(m))
		}
	}
	rows := make([]portalAgentRow, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("an agents row is a %T, not an object", item)
		}
		row := portalAgentRow{name: stringAt(m, nameKey), namespace: stringAt(m, "namespace")}
		if ref, ok := m["ref"].(map[string]any); ok {
			row.name = firstNonEmpty(row.name, stringAt(ref, nameKey))
			row.namespace = firstNonEmpty(row.namespace, stringAt(ref, "namespace"))
		}
		if meta, ok := m["metadata"].(map[string]any); ok {
			row.name = firstNonEmpty(row.name, stringAt(meta, nameKey))
			row.namespace = firstNonEmpty(row.namespace, stringAt(meta, "namespace"))
		}
		if row.name == "" {
			return nil, fmt.Errorf("an agents row names no agent (keys %v)", mapKeys(m))
		}
		switch ready := m["ready"].(type) {
		case bool:
			row.ready, row.readiness = ready, fmt.Sprintf("ready=%v", ready)
		default:
			for _, key := range []string{"readiness", "state", "status"} {
				if v := stringAt(m, key); v != "" {
					row.readiness = key + "=" + v
					row.ready = strings.EqualFold(v, "ready") || v == conditionTrue
					break
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// proveAgentPlatformPages drives the Agent Platform pages as the user: the
// agents list (AgentTemplates with their readiness — own, the proof's agent,
// Ready on the cluster, must be among them), and for the primary user a
// session on own with one message answered — attributed to the person by the
// forwarded token. A portal without the agents route (kagent 0.10) fails by
// name.
func proveAgentPlatformPages(ps *portalSession, primary bool, own string) error {
	status, raw, err := ps.kagentRequest(http.MethodGet, portalAgentsPath, nil)
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusNotFound:
		return fmt.Errorf("the portal backend answers 404 to %s — this Backstage (%s) still speaks kagent 0.10 (sessions over REST, agents from the cluster); the Agent Platform pages on kagent main are not runnable against it",
			portalAgentsPath, firstNonEmpty(deploymentImage(platformNamespace, componentBackstage), "image unknown"))
	case status == http.StatusForbidden && !primary:
		fmt.Printf("  agents list      %d for %s (the portal lists as the person; the person may not read AgentTemplates): %.160s\n", status, ps.user.Email, raw)
		return nil
	case status != http.StatusOK:
		return fmt.Errorf("GET %s answered %d — the portal cannot read kagent's API as it expects (kagent main serves gRPC only): %.300s", portalAgentsPath, status, raw)
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("GET %s: not JSON: %w\n%.300s", portalAgentsPath, err, raw)
	}
	rows, err := portalAgentRows(payload)
	if err != nil {
		return fmt.Errorf("GET %s: %w\n%.300s", portalAgentsPath, err, raw)
	}
	if len(rows) == 0 {
		return fmt.Errorf("GET %s lists no agents for %s although AgentTemplate %s/%s exists and is Ready on Harness %s", portalAgentsPath, ps.user.Email, kagentNamespace, own, kagentHarness)
	}
	var lines []string
	ready := 0
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("(%s, %s)", r.name, firstNonEmpty(r.readiness, "readiness unknown")))
		if r.ready {
			ready++
		}
	}
	slices.Sort(lines)
	fmt.Printf("  agents list      %d agents, %d Ready: [%s]\n", len(rows), ready, strings.Join(lines, ", "))
	idx := slices.IndexFunc(rows, func(r portalAgentRow) bool { return r.name == own })
	if idx < 0 {
		return fmt.Errorf("GET %s does not list %s, the AgentTemplate the proof created in %s — the list must show every AgentTemplate of the namespace", portalAgentsPath, own, kagentNamespace)
	}
	if !primary {
		return nil
	}
	agent := rows[idx]
	if !agent.ready {
		return fmt.Errorf("%s is Ready on Harness %s but the portal's list says %s — nothing to chat with (the list must carry the AgentTemplates' readiness)", own, kagentHarness, firstNonEmpty(agent.readiness, "readiness unknown"))
	}
	namespace := firstNonEmpty(agent.namespace, kagentNamespace)

	status, raw, err = ps.kagentRequest(http.MethodPost, portalSessionsPath, map[string]any{
		"agentNamespace": namespace, "agentName": agent.name, nameKey: portalSessionName,
	})
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("POST %s for %s/%s answered %d: %.300s", portalSessionsPath, namespace, agent.name, status, raw)
	}
	var created any
	_ = json.Unmarshal(raw, &created)
	sessionID, _ := unwrapKey(created, "id").(string)
	if sessionID == "" {
		return fmt.Errorf("POST %s answered without a session id: %.300s", portalSessionsPath, raw)
	}
	defer func() {
		if status, raw, err := ps.kagentRequest(http.MethodDelete, portalSessionsPath+"/"+sessionID, nil); err != nil || status/100 != 2 {
			note("deleting session %s: %d %v %.120s", sessionID, status, err, raw)
		}
	}()
	fmt.Printf("  session          %s on %s/%s (created for %s)\n", sessionID, namespace, agent.name, ps.user.Email)

	status, raw, err = ps.kagentRequest(http.MethodPost, portalSessionsPath+"/"+sessionID+"/messages", map[string]any{
		"agentNamespace": namespace, "agentName": agent.name, "messageId": uuid.NewString(), "text": portalChatPrompt,
	})
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusAccepted:
		return fmt.Errorf("the turn on %s outlived the portal backend's timeout (202 pending): %.200s", agent.name, raw)
	case status != http.StatusOK:
		return fmt.Errorf("POST %s/%s/messages answered %d: %.300s", portalSessionsPath, sessionID, status, raw)
	}
	var answer any
	if err := json.Unmarshal(raw, &answer); err != nil {
		return fmt.Errorf("the message answer is not JSON: %w\n%.300s", err, raw)
	}
	reply := lastTextBut(collectStrings(answer, "text"), portalChatPrompt)
	if reply == "" {
		return fmt.Errorf("the turn on %s answered without a text part: %.300s", agent.name, raw)
	}
	fmt.Printf("  chat turn        %s answered %q\n", agent.name, excerpt(reply, 80))
	return nil
}

// lastTextBut is the last non-empty text that is not the prompt echoed back.
func lastTextBut(texts []string, prompt string) string {
	var reply string
	for _, t := range texts {
		if strings.TrimSpace(t) != "" && t != prompt {
			reply = t
		}
	}
	return reply
}

// collectStrings walks a decoded JSON tree in document order and returns
// every string value stored under key.
func collectStrings(node any, key string) []string {
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if s, ok := v[key].(string); ok {
				out = append(out, s)
			}
			for _, k := range mapKeys(v) {
				walk(v[k])
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(node)
	return out
}

// mapKeys is the sorted keys of a decoded JSON object.
func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// stringAt is m[key] when it is a string, "" otherwise.
func stringAt(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
