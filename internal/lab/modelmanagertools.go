package lab

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// modelManagerTools is the proofs' client of model-manager: its
// x_model-manager_* tools through muster as the signed-in person, the one
// path the portal and agents take. model-manager's answers are the JSON its
// REST API would return; a refusal is "<code>: <message>" (not_found,
// unsupported, conflict, does_not_fit, …) and comes back as a *toolRefusal.
type modelManagerTools struct {
	session *musterSession
}

// toolName is the aggregated name muster exposes the model-manager tool as.
func (m *modelManagerTools) toolName(tool string) string {
	return "x_" + modelManagerMCPServer + "_" + tool
}

// toolRefusal is a tool's error result: the tool ran and said no.
type toolRefusal struct {
	tool string
	text string
}

func (e *toolRefusal) Error() string { return e.tool + " refused: " + excerpt(e.text, 300) }

// code is model-manager's error code, the word before the first colon.
func (e *toolRefusal) code() string {
	code, _, _ := strings.Cut(e.text, ":")
	return strings.TrimSpace(code)
}

// refusalCode is the model-manager error code of a refused call, "" for any
// other error (or none).
func refusalCode(err error) string {
	var refusal *toolRefusal
	if errors.As(err, &refusal) {
		return refusal.code()
	}
	return ""
}

// call runs one model-manager tool and returns its JSON text.
func (m *modelManagerTools) call(tool string, args map[string]any) (string, error) {
	name := m.toolName(tool)
	env, err := m.session.callToolEnvelope(name, args)
	if err != nil {
		return "", err
	}
	text := ""
	if len(env.Content) > 0 {
		text = env.Content[0].Text
	}
	if env.IsError {
		return "", &toolRefusal{tool: name, text: text}
	}
	return text, nil
}

// getJSON runs one tool and decodes its answer into out.
func (m *modelManagerTools) getJSON(tool string, args map[string]any, out any) error {
	text, err := m.call(tool, args)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("%s: parsing %.200s: %w", m.toolName(tool), text, err)
	}
	return nil
}

// modelNames lists, sorted, the model names list_models ("models") or
// list_loaded_models ("loaded") answers for the backend.
func (m *modelManagerTools) modelNames(tool, backend string) ([]string, error) {
	type named struct {
		Name string `json:"name"`
	}
	var list struct {
		Models []named `json:"models"`
		Loaded []named `json:"loaded"`
	}
	if err := m.getJSON(tool, map[string]any{backendField: backend}, &list); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Models)+len(list.Loaded))
	for _, entry := range append(list.Models, list.Loaded...) {
		names = append(names, entry.Name)
	}
	slices.Sort(names)
	return names, nil
}

// deleteModel deletes a model on a backend (its ModelConfig rides along) and
// waits until the backend's inventory no longer lists it; one already gone
// counts as deleted.
func (m *modelManagerTools) deleteModel(model, backend string) error {
	_, err := m.call("delete_model", map[string]any{modelField: model, backendField: backend})
	if err != nil && refusalCode(err) != "not_found" {
		return err
	}
	gone := waitFor(15, 2*time.Second, func() bool {
		names, err := m.modelNames("list_models", backend)
		return err == nil && !slices.ContainsFunc(names, func(n string) bool { return sameModel(n, model) })
	})
	if !gone {
		return fmt.Errorf("%s still listed after the delete", model)
	}
	note("deleted")
	return nil
}

// waitJob polls one job through get_job until it finishes, printing progress
// as it crosses each tenth — the observable-progress part of the proof.
func (m *modelManagerTools) waitJob(id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	lastBucket := -1
	for time.Now().Before(deadline) {
		var job struct {
			Phase          string  `json:"phase"`
			Status         string  `json:"status"`
			Percent        float64 `json:"percent"`
			BytesCompleted int64   `json:"bytesCompleted"`
			BytesTotal     int64   `json:"bytesTotal"`
			Error          string  `json:"error"`
		}
		if err := m.getJSON("get_job", map[string]any{"id": id}, &job); err != nil {
			return err
		}
		if bucket := int(job.Percent) / 10; bucket > lastBucket && job.BytesTotal > 0 {
			note("%3.0f%%  %s / %s  %s", job.Percent, humanBytes(job.BytesCompleted), humanBytes(job.BytesTotal), job.Status)
			lastBucket = bucket
		}
		switch job.Phase {
		case "succeeded":
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("job %s %s: %s", id, job.Phase, firstNonEmpty(job.Error, job.Status))
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("job %s did not finish within %s", id, timeout)
}

// musterRefusesAnonymous proves the identity boundary in front of
// model-manager: muster answers an MCP request without a token with 401.
func musterRefusesAnonymous(cfg *config.Config) error {
	client, err := labHTTPClient(30 * time.Second)
	if err != nil {
		return err
	}
	url := cfg.MusterBaseURL() + "/mcp"
	step("Calling muster without a token (%s)", url)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agentlab","version":"1"}}}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("muster is not reachable through the edge: %w — run `agentlab platform` first", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("muster answered an unauthenticated initialize with %d, wanted 401", resp.StatusCode)
	}
	note("401 without a token (WWW-Authenticate: %s)", firstNonEmpty(resp.Header.Get("WWW-Authenticate"), "-"))
	return nil
}
