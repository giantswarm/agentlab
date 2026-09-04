package lab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// ModelsTestModel is the model the proof pulls and deletes on the ollama
// backend: small (~400 MB) and tool-calling capable, which a kagent agent turn
// requires (agents send tool schemas with every request; smollm2:135m pulls
// fine and then fails "does not support tools").
const ModelsTestModel = "qwen2.5:0.5b"

// ModelsTestModelLemonade is the proof's model on the lemonade backend: the
// smallest FastFlowLM (NPU) model of Lemonade's catalog with the tool-calling
// label (3.1 GB); the smaller *-FLM models (qwen3-0.6b-FLM, ...) cannot call
// tools, so an agent turn on them fails.
const ModelsTestModelLemonade = "qwen3-4b-FLM"

// ModelsTestModelFor is the default proof model of a backend.
func ModelsTestModelFor(backend string) string {
	if backend == config.ModelManagerBackendLemonade {
		return ModelsTestModelLemonade
	}
	return ModelsTestModel
}

// modelsTestAgent is the throwaway kagent Agent the proof runs one turn on.
const modelsTestAgent = "agentlab-models-test"

// modelField and backendField are the request fields naming a model and its
// backend in the model-manager API.
const (
	modelField   = "model"
	backendField = "backend"
)

// ModelsTest is the headless end-to-end proof of managed models on ONE of the
// configured backends, through the platform path only: the model-manager REST
// API behind the agentgateway route with a lab user's Dex token (and a 401
// without one), then list -> pull with observable progress -> the auto-created
// kagent ModelConfig (native keyless Ollama provider on ollama, OpenAI
// provider against Lemonade's /api/v1 on lemonade, carrying the backend label)
// Accepted -> a kagent agent turn against it -> the MCP tools through muster
// -> unload -> delete (gone from the host server, the ModelConfig gone). Every
// request names the backend — the one model-manager fronting all servers
// resolves by it. Leaves nothing behind.
func ModelsTest(cfg *config.Config, email, backendName, model string) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.ModelManagerEnabled() {
		return fmt.Errorf("platform.modelManager is off in %s (needs platform.agents too) — enable it and run `agentlab platform` first", config.File)
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}
	mm := cfg.Platform.ModelManager
	backendName = strings.TrimSpace(backendName)
	if backendName == "" {
		backendName = mm.Primary()
	}
	if !slices.Contains(mm.Backends, backendName) {
		return fmt.Errorf("backend %q is not among platform.modelManager.backends (%s)", backendName, strings.Join(mm.Backends, ", "))
	}
	if model == "" {
		model = ModelsTestModelFor(backendName)
	}
	// The backend qualifier of every read and write of the proof.
	q := "?backend=" + url.QueryEscape(backendName)
	// Generous: the first agent turn loads the model into the host server.
	client, err := labHTTPClient(180 * time.Second)
	if err != nil {
		return err
	}
	api := modelManagerAPI{client: client, base: cfg.ModelManagerBaseURL() + "/api/v1"}

	step("Calling the model-manager API without a token (%s)", cfg.ModelManagerBaseURL())
	status, _, header, err := api.do(http.MethodGet, "/backend", nil)
	if err != nil {
		return fmt.Errorf("model-manager is not reachable through the edge: %w — run `agentlab platform` first", err)
	}
	if status != http.StatusUnauthorized {
		return fmt.Errorf("unauthenticated GET /api/v1/backend answered %d, wanted 401: the route's JWT policy is not enforcing", status)
	}
	note("401 without a token (WWW-Authenticate: %s)", firstNonEmpty(header.Get("WWW-Authenticate"), "-"))

	step("Logging in to Dex as %s", email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	api.token = token
	note("got an id_token")

	step("Backend %s through the gateway with the Dex token", backendName)
	var backend struct {
		Backend      string          `json:"backend"`
		Version      string          `json:"version"`
		Endpoint     string          `json:"endpoint"`
		Healthy      bool            `json:"healthy"`
		Capabilities map[string]bool `json:"capabilities"`
		Wiring       struct {
			Namespace string `json:"namespace"`
			AutoWire  bool   `json:"autoWire"`
		} `json:"wiring"`
	}
	if err := api.getJSON("/backend"+q, &backend); err != nil {
		return err
	}
	if backend.Backend != backendName || !backend.Healthy {
		return fmt.Errorf("backend %q healthy=%v, wanted a healthy %s backend (endpoint %s)", backend.Backend, backend.Healthy, backendName, backend.Endpoint)
	}
	caps := make([]string, 0, len(backend.Capabilities))
	for name, on := range backend.Capabilities {
		if on {
			caps = append(caps, name)
		}
	}
	slices.Sort(caps)
	note("backend %s %s at %s, healthy; capabilities: %s", backendName, backend.Version, backend.Endpoint, strings.Join(caps, ", "))
	note("wiring: ModelConfigs in %s, autoWire=%v", backend.Wiring.Namespace, backend.Wiring.AutoWire)

	step("Listing models on %s", backendName)
	names, err := api.modelNames("/models" + q)
	if err != nil {
		return err
	}
	note("%d models: %s", len(names), strings.Join(names, ", "))
	if slices.Contains(names, model) {
		note("%s is left over from an earlier run — deleting it first", model)
		if err := api.deleteModel(model, backendName); err != nil {
			return err
		}
	}

	step("Pulling %s on %s (progress via GET /api/v1/jobs/{id})", model, backendName)
	started := time.Now()
	status, body, _, err := api.do(http.MethodPost, "/models/pull", map[string]any{modelField: model, backendField: backendName})
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("POST /models/pull answered %d, wanted 202: %.300s", status, body)
	}
	var pull struct {
		Job struct {
			ID string `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal(body, &pull); err != nil || pull.Job.ID == "" {
		return fmt.Errorf("pull did not return a job id: %.300s", body)
	}
	note("job %s accepted", pull.Job.ID)
	if err := api.waitJob(pull.Job.ID, 30*time.Minute); err != nil {
		return err
	}
	note("pulled in %s", time.Since(started).Round(time.Second))

	step("The job is attributed to the caller (requestedBy)")
	var job struct {
		RequestedBy string `json:"requestedBy"`
	}
	if err := api.getJSON("/jobs/"+pull.Job.ID, &job); err != nil {
		return err
	}
	if job.RequestedBy != user.Email {
		return fmt.Errorf("job %s carries requestedBy=%q, wanted %q: model-manager did not learn the caller from the forwarded token", pull.Job.ID, job.RequestedBy, user.Email)
	}
	note("requestedBy=%s", job.RequestedBy)

	step("Auto-created kagent ModelConfig")
	mcName := ""
	found := waitFor(30, 2*time.Second, func() bool {
		var m struct {
			ModelConfig struct {
				Name string `json:"name"`
			} `json:"modelConfig"`
			Capabilities []string `json:"capabilities"`
		}
		if err := api.getJSON("/models/"+model+q, &m); err != nil {
			return false
		}
		mcName = m.ModelConfig.Name
		if mcName != "" && !slices.Contains(m.Capabilities, "tools") {
			note("warning: %s reports no `tools` capability; the agent turn below will likely fail", model)
		}
		return mcName != ""
	})
	if !found {
		return fmt.Errorf("GET /models/%s never reported a wired ModelConfig (autoWire=%v)", model, backend.Wiring.AutoWire)
	}
	if err := waitModelConfigAccepted(mcName); err != nil {
		return err
	}
	spec, err := outputQuiet("kubectl", "-n", kagentNamespace, "get", modelConfigResource, mcName,
		"-o", `jsonpath={.spec.provider} {.spec.model} {.spec.ollama.host}{.spec.openAI.baseUrl} backend={.metadata.labels.model-manager\.giantswarm\.io/backend} managed-by={.metadata.labels.app\.kubernetes\.io/managed-by}`)
	if err != nil {
		return err
	}
	// model-manager writes the native keyless Ollama provider for ollama and
	// the OpenAI provider on /api/v1 (placeholder key) for lemonade.
	wantProvider, providerNote := config.ProviderOllama, "keyless native provider"
	if backendName == config.ModelManagerBackendLemonade {
		wantProvider, providerNote = config.ProviderOpenAI, "OpenAI-compatible /api/v1, placeholder key"
	}
	if !strings.HasPrefix(spec, wantProvider+" ") {
		return fmt.Errorf("ModelConfig %s is not the %s provider model-manager writes for %s: %s", mcName, wantProvider, backendName, spec)
	}
	if !strings.Contains(spec, " backend="+backendName+" ") {
		return fmt.Errorf("ModelConfig %s does not carry the model-manager.giantswarm.io/backend=%s label: %s", mcName, backendName, spec)
	}
	note("ModelConfig %s: %s (%s)", mcName, strings.TrimSpace(spec), providerNote)

	// The user's identity, not a ServiceAccount: wiring writes a ModelConfig
	// into the kagent namespace as the caller (downstream OAuth), so a user
	// without RBAC there is refused by the apiserver, while the admin who
	// pulled the model was allowed.
	if viewer := cfg.FindUserInGroup("viewers"); viewer != nil {
		step("Wiring %s as %s — expecting the apiserver's Forbidden (view role, no write in %s)", model, viewer.Email, kagentNamespace)
		viewerToken, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
			viewer.Email, viewer.Password, musterLoginScopes)
		if err != nil {
			return err
		}
		viewerAPI := modelManagerAPI{client: client, base: api.base, token: viewerToken}
		status, body, _, err := viewerAPI.do(http.MethodPost, "/models/wire", map[string]any{modelField: model, backendField: backendName})
		if err != nil {
			return err
		}
		if status/100 == 2 {
			return fmt.Errorf("%s wired %s (HTTP %d) although the view role cannot write ModelConfigs — model-manager is not acting as the caller (ServiceAccount fallback?)", viewer.Email, model, status)
		}
		if !strings.Contains(strings.ToLower(string(body)), "forbidden") {
			return fmt.Errorf("POST /models/wire as %s answered %d without the apiserver's Forbidden: %.300s", viewer.Email, status, body)
		}
		note("%s: HTTP %d, %s", viewer.Email, status, excerpt(string(body), 120))
	}

	step("Agent turn on %s (kagent Agent, runtime go -> host %s)", mcName, config.BackendServerName(backendName))
	reply, err := agentTurn(client, cfg, mcName, "Reply with exactly the word pong and nothing else.")
	if err != nil {
		return err
	}
	note("agent replied: %q", excerpt(reply, 120))

	step("MCP tools through muster (x_%s_*)", modelManagerMCPServer)
	session, err := openMusterSession(cfg, token, "models-test")
	if err != nil {
		return err
	}
	tools, err := session.listTools()
	if err != nil {
		return err
	}
	toolPrefix := "x_" + modelManagerMCPServer + "_"
	var mmTools []string
	for _, t := range tools {
		if strings.HasPrefix(t, toolPrefix) {
			mmTools = append(mmTools, strings.TrimPrefix(t, toolPrefix))
		}
	}
	if len(mmTools) == 0 {
		return fmt.Errorf("muster aggregates no %s tools; check `kubectl -n %s get mcpservers.muster.giantswarm.io %s`", toolPrefix, platformNamespace, modelManagerMCPServer)
	}
	slices.Sort(mmTools)
	note("%d tools: %s", len(mmTools), strings.Join(mmTools, ", "))
	text, err := session.callServerTool(toolPrefix+"get_model", map[string]any{modelField: model, backendField: backendName})
	if err != nil {
		return err
	}
	if !strings.Contains(text, model) {
		return fmt.Errorf("%sget_model did not describe %s: %.300s", toolPrefix, model, text)
	}
	note("%sget_model sees %s (%s)", toolPrefix, model, excerpt(text, 100))

	step("Unloading %s", model)
	if status, body, _, err := api.do(http.MethodPost, "/models/unload", map[string]any{modelField: model, backendField: backendName}); err != nil {
		return err
	} else if status/100 != 2 {
		return fmt.Errorf("POST /models/unload answered %d: %.300s", status, body)
	}
	unloaded := waitFor(15, 2*time.Second, func() bool {
		loaded, err := api.modelNames("/loaded" + q)
		return err == nil && !slices.Contains(loaded, model)
	})
	if !unloaded {
		return fmt.Errorf("%s still listed by GET /loaded after unload", model)
	}
	note("not loaded any more")

	step("Deleting %s", model)
	if err := api.deleteModel(model, backendName); err != nil {
		return err
	}
	if _, err := outputQuiet("kubectl", "-n", kagentNamespace, "get", modelConfigResource, mcName); err == nil {
		gone := waitFor(15, 2*time.Second, func() bool {
			_, err := outputQuiet("kubectl", "-n", kagentNamespace, "get", modelConfigResource, mcName)
			return err != nil
		})
		if !gone {
			return fmt.Errorf("ModelConfig %s survived the delete", mcName)
		}
	}
	note("ModelConfig %s is gone", mcName)
	endpoint, err := resolveBackendEndpoint(cfg, backendName)
	if err != nil {
		return err
	}
	server := config.BackendServerName(backendName)
	remaining, err := hostServerModels(backendName, endpoint)
	if err != nil {
		return fmt.Errorf("reading the host %s's models at %s: %w", server, endpoint, err)
	}
	if slices.ContainsFunc(remaining, func(m HostModel) bool { return m.ID == model }) {
		return fmt.Errorf("host %s still has %s after the delete", server, model)
	}
	note("host %s at %s no longer has it (%d models left)", server, endpoint, len(remaining))
	text, err = session.callServerTool(toolPrefix+"list_models", map[string]any{backendField: backendName})
	if err != nil {
		return err
	}
	if strings.Contains(text, `"`+model+`"`) {
		return fmt.Errorf("%slist_models still lists %s", toolPrefix, model)
	}
	note("%slist_models agrees", toolPrefix)

	fmt.Println()
	fmt.Println("PASS: no token -> 401 at the gateway; Dex token -> model-manager REST through agentgateway")
	fmt.Printf("PASS: %s backend: list -> pull %s (progress) -> ModelConfig %s (%s provider, backend label) Accepted -> agent turn -> unload -> delete\n", backendName, model, mcName, wantProvider)
	fmt.Printf("PASS: muster aggregates x_%s_* and calls them (get_model, list_models)\n", modelManagerMCPServer)
	fmt.Printf("PASS: the caller's identity — job requestedBy=%s; a viewer's wire is Forbidden by the apiserver (user RBAC, not the ServiceAccount's)\n", user.Email)
	return nil
}

// modelManagerAPI is a minimal client of the model-manager REST API through
// the edge, with the Dex token as Bearer once known.
type modelManagerAPI struct {
	client *http.Client
	base   string
	token  string
}

func (a *modelManagerAPI) do(method, path string, payload any) (int, []byte, http.Header, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, a.base+path, body)
	if err != nil {
		return 0, nil, nil, err
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, resp.Header, nil
}

func (a *modelManagerAPI) getJSON(path string, out any) error {
	status, body, _, err := a.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET %s answered %d: %.300s", path, status, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("GET %s: parsing %.200s: %w", path, body, err)
	}
	return nil
}

// modelNames lists the model names of a /models-shaped response.
func (a *modelManagerAPI) modelNames(path string) ([]string, error) {
	var list struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := a.getJSON(path, &list); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Models))
	for _, m := range list.Models {
		names = append(names, m.Name)
	}
	slices.Sort(names)
	return names, nil
}

// deleteModel deletes a model on a backend (its ModelConfig rides along) and
// waits until the backend's inventory no longer lists it.
func (a *modelManagerAPI) deleteModel(model, backend string) error {
	q := "?backend=" + url.QueryEscape(backend)
	status, body, _, err := a.do(http.MethodDelete, "/models/"+model+q, nil)
	if err != nil {
		return err
	}
	if status/100 != 2 && status != http.StatusNotFound {
		return fmt.Errorf("DELETE /models/%s answered %d: %.300s", model, status, body)
	}
	gone := waitFor(15, 2*time.Second, func() bool {
		names, err := a.modelNames("/models" + q)
		return err == nil && !slices.Contains(names, model)
	})
	if !gone {
		return fmt.Errorf("%s still listed after the delete", model)
	}
	note("deleted (status %d)", status)
	return nil
}

// waitJob polls one job until it finishes, printing progress as it crosses
// each tenth — the observable-progress part of the proof.
func (a *modelManagerAPI) waitJob(id string, timeout time.Duration) error {
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
		if err := a.getJSON("/jobs/"+id, &job); err != nil {
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

// agentTurn creates a throwaway kagent Agent on the ModelConfig, waits for
// it to be Ready, sends one A2A message/send and returns the agent's text.
// The Agent is deleted on every path.
func agentTurn(client *http.Client, cfg *config.Config, modelConfig, prompt string) (string, error) {
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
	if err := pipeInto([]byte(manifest), "kubectl", "apply", "-f", "-"); err != nil {
		return "", err
	}
	defer func() {
		_ = runQuiet("kubectl", "-n", kagentNamespace, "delete", "agents.kagent.dev", modelsTestAgent, "--ignore-not-found", "--wait=false")
	}()
	if err := runQuiet("kubectl", "-n", kagentNamespace, "wait", "--for=condition=Ready", "--timeout=240s",
		"agents.kagent.dev/"+modelsTestAgent); err != nil {
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
		reply, lastErr = a2aSend(client, a2aURL, payload, prompt)
		return lastErr == nil
	})
	if !answered {
		return "", fmt.Errorf("A2A turn against %s failed: %w", modelsTestAgent, lastErr)
	}
	return reply, nil
}

// a2aSend posts one JSON-RPC message/send and returns the agent's text: the
// last text part that is not the prompt, wherever the Task/Message shape put
// it (status.message, artifacts, history).
func a2aSend(client *http.Client, url, payload, prompt string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
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
			// Deterministic order: sorted keys.
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
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

func decodeJSONBody(resp *http.Response, out any) error {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %.200s", resp.Status, raw)
	}
	return json.Unmarshal(raw, out)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func excerpt(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
