package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// agentTurnV1 creates a throwaway kagent Agent on the ModelConfig, waits for
// it to be Ready, sends one A2A message/send and returns the agent's text.
// The Agent is deleted on every path.
func agentTurnV1(client *http.Client, cfg *config.Config, modelConfig, prompt string) (string, error) {
	manifest := fmt.Sprintf(`apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/managed-by: agentlab
spec:
  type: Declarative
  description: agentlab models-test probe (deleted after the run)
  declarative:
    runtime: go
    modelConfig: %s
    systemMessage: You are a terse assistant. Answer in one short line.
`, modelsTestAgent, kagentNamespace, modelConfig)
	ctx := context.Background()
	agents, err := gvrFor(kagentAgentResource)
	if err != nil {
		return "", err
	}
	if _, err := applyManifests(ctx, []byte(manifest)); err != nil {
		return "", err
	}
	defer func() {
		_ = deleteObject(ctx, agents, kagentNamespace, modelsTestAgent, 0)
	}()
	if err := waitCondition(ctx, agents, kagentNamespace, modelsTestAgent, "Ready", "True", 240*time.Second); err != nil {
		return "", fmt.Errorf("agent %s never became Ready: %w", modelsTestAgent, err)
	}
	// The pod may report Ready a moment before the ADK listens; a short retry
	// absorbs that, the client timeout covers the model load.
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"message":{"kind":"message","role":"user","messageId":%q,"parts":[{"kind":"text","text":%q}]}}}`,
		randHex(8), prompt)
	a2aURL := fmt.Sprintf("%s/api/a2a/%s/%s?user_id=admin@kagent.dev", cfg.KagentUIBaseURL(), kagentNamespace, modelsTestAgent)
	var reply string
	var lastErr error
	answered := waitFor(6, 5*time.Second, func() bool {
		reply, lastErr = a2aSend(client, a2aURL, "", payload, prompt)
		return lastErr == nil
	})
	if !answered {
		return "", fmt.Errorf("A2A turn against %s failed: %w", modelsTestAgent, lastErr)
	}
	return reply, nil
}

// a2aSend posts one JSON-RPC message/send and returns the agent's text: the
// last text part that is not the prompt, wherever the Task/Message shape put
// it (status.message, artifacts, history). A non-empty token goes out as
// Authorization: Bearer — the user's Dex id_token the portal forwards to
// kagent, which the agent's runtime propagates to muster (KAGENT_PROPAGATE_TOKEN).
func a2aSend(client *http.Client, url, token, payload, prompt string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("A2A answered %d: %.300s", resp.StatusCode, raw)
	}
	var rpc map[string]any
	if err := json.Unmarshal(raw, &rpc); err != nil {
		return "", fmt.Errorf("parsing the A2A response: %w: %.300s", err, raw)
	}
	if e, ok := rpc["error"].(map[string]any); ok {
		return "", fmt.Errorf("A2A error: %v", e["message"])
	}
	texts := collectStrings(rpc["result"], "text")
	var reply string
	for _, t := range texts {
		if strings.TrimSpace(t) != "" && t != prompt {
			reply = t
		}
	}
	if reply == "" {
		return "", fmt.Errorf("no text part in the A2A result: %.300s", raw)
	}
	if state := collectStrings(rpc["result"], "state"); len(state) > 0 && state[len(state)-1] == "failed" {
		return "", fmt.Errorf("A2A task failed: %s", excerpt(reply, 200))
	}
	return reply, nil
}
