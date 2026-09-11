package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The proof's default model per backend — every one small and tool-calling
// capable, which a kagent agent turn requires: agents send tool schemas with
// every request, and a model without them fails each turn with "does not
// support tools" (smollm2:135m pulls fine and then does exactly that).
const (
	// ModelsTestModelOllama is ~400 MB from the Ollama registry.
	ModelsTestModelOllama = "qwen2.5:0.5b"
	// ModelsTestModelLemonade is the smallest FastFlowLM (NPU) model of
	// Lemonade's catalog carrying the tool-calling label (3.1 GB); the
	// smaller *-FLM models cannot call tools.
	ModelsTestModelLemonade = "qwen3-4b-FLM"
	// ModelsTestModelLMStudio is an LM Studio hub reference (~2 GB), so the
	// download resolves the variant that fits the host — GGUF on Linux and
	// NVIDIA, MLX on Apple silicon.
	ModelsTestModelLMStudio = "ibm/granite-4-micro"
)

// ModelsTestModelFor is the default proof model of a backend.
func ModelsTestModelFor(backend string) string {
	if spec, known := backendSpec(backend); known {
		return spec.proofModel
	}
	return ""
}

// ModelsTestModelDefaults words the per-backend defaults for `--model`'s help
// text, so the flag cannot drift from the table.
func ModelsTestModelDefaults() string {
	parts := make([]string, 0, len(config.ModelManagerBackends))
	for _, b := range config.ModelManagerBackends {
		parts = append(parts, fmt.Sprintf("%s on %s", ModelsTestModelFor(b), b))
	}
	return strings.Join(parts, ", ")
}

// modelsTestAgent is the throwaway agent the proof runs one turn on: created
// through agent-manager as the person, on the ModelConfig model-manager wired.
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
// provider against Lemonade's /api/v1 or LM Studio's /v1, carrying the backend
// label) Accepted -> a kagent agent turn against it -> the MCP tools through
// muster -> unload -> the teardown the server supports. Every request names
// the backend — the one model-manager fronting all servers resolves by it.
//
// On a server that deletes over its API the teardown is a delete (gone from
// the host, the ModelConfig gone) and the run leaves nothing behind. On
// lmstudio LM Studio serves no delete, so the run proves the 501 refusal and
// unwires instead, and says in its closing note that the model stays
// downloaded — the one proof that leaves something behind, by design.
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
	spec, known := backendSpec(backendName)
	if !known {
		return fmt.Errorf("unknown host model server backend %q", backendName)
	}
	if model == "" {
		model = spec.proofModel
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
	// The lmstudio driver reports no version (LM Studio serves none), and
	// Info.Version is omitempty, so name what the discovery names instead of
	// printing a hole where a version would be.
	version := backend.Version
	if version == "" {
		version = "(reports no version)"
	}
	note("backend %s %s at %s, healthy; capabilities: %s", backendName, version, backend.Endpoint, strings.Join(caps, ", "))
	note("wiring: ModelConfigs in %s, autoWire=%v", backend.Wiring.Namespace, backend.Wiring.AutoWire)

	// What the backend claims it can do has to match what its server really
	// offers, in both directions: a driver claiming delete for an LM Studio
	// (which has none) would fail at the end of the proof, and an Ollama that
	// lost it would pass one that no longer proves a delete.
	step("The advertised delete capability matches %s", config.BackendServerName(backendName))
	if backend.Capabilities["delete"] != spec.deleteOverREST {
		return fmt.Errorf("backend %s reports delete=%v, wanted %v: %s %s delete a model through its API",
			backendName, backend.Capabilities["delete"], spec.deleteOverREST,
			config.BackendServerName(backendName), map[bool]string{true: "can", false: "cannot"}[spec.deleteOverREST])
	}
	note("delete=%v, as %s offers", spec.deleteOverREST, config.BackendServerName(backendName))

	step("Listing models on %s", backendName)
	names, err := api.modelNames("/models" + q)
	if err != nil {
		return err
	}
	note("%d models: %s", len(names), strings.Join(names, ", "))
	if slices.ContainsFunc(names, func(n string) bool { return sameModel(n, model) }) {
		// A server that cannot delete keeps what an earlier run pulled; the
		// pull below then finds it downloaded and completes at once.
		if !spec.deleteOverREST {
			note("%s is already downloaded and %s cannot delete it — the pull below is a no-op",
				model, config.BackendServerName(backendName))
		} else {
			note("%s is left over from an earlier run — deleting it first", model)
			if err := api.deleteModel(model, backendName); err != nil {
				return err
			}
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
	mc, err := readKagentObject(modelConfigResource, mcName)
	if err != nil {
		return err
	}
	// `spec` is the backend's table entry here, so the summary keeps its own
	// name.
	mcSpec := modelConfigSummary(mc)
	// model-manager writes the native keyless Ollama provider for ollama and
	// the OpenAI provider for the servers behind an OpenAI-compatible API
	// (backends.go says which, and on what path).
	if !strings.HasPrefix(mcSpec, spec.provider+" ") {
		return fmt.Errorf("ModelConfig %s is not the %s provider model-manager writes for %s: %s", mcName, spec.provider, backendName, mcSpec)
	}
	if !strings.Contains(mcSpec, " backend="+backendName+" ") {
		return fmt.Errorf("ModelConfig %s does not carry the model-manager.giantswarm.io/backend=%s label: %s", mcName, backendName, mcSpec)
	}
	// The path is the only thing the two OpenAI-provider wirings differ by
	// (/api/v1 for Lemonade, /v1 for LM Studio), and the note below claims
	// it, so check it rather than print it: the wrong one still wires and
	// still looks Accepted, then fails as an agent turn that times out after
	// four minutes — LM Studio answers 200 with an error body on a path it
	// does not serve.
	if spec.agentPath != "" {
		baseURL, _, _ := unstructured.NestedString(mc.Object, "spec", "openAI", "baseUrl")
		if !strings.HasSuffix(baseURL, spec.agentPath) {
			return fmt.Errorf("ModelConfig %s does not point at %s's OpenAI-compatible %s: %s",
				mcName, config.BackendServerName(backendName), spec.agentPath, mcSpec)
		}
	}
	note("ModelConfig %s: %s (%s)", mcName, strings.TrimSpace(mcSpec), spec.providerNote)

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

	const pongPrompt = "Reply with exactly the word pong and nothing else."
	session, err := openMusterSession(cfg, token, "models-test")
	if err != nil {
		return err
	}
	step("Agent turn on %s: an agent created through agent-manager as %s, Ready on Harness %s, one A2A turn through the edge (runtime -> host %s)", mcName, user.Email, kagentHarness, config.BackendServerName(backendName))
	reply, err := agentTurn(cfg, session, token, mcName, pongPrompt)
	if err != nil {
		return err
	}
	note("agent replied: %q", excerpt(reply, 120))

	step("MCP tools through muster (x_%s_*)", modelManagerMCPServer)
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
	if !namesModel(text, model) {
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

	endpoint, err := resolveBackendEndpoint(cfg, backendName)
	if err != nil {
		return err
	}
	if dockerIsPodman() && cfg.Platform.ModelManager.EndpointFor(backendName) == "" {
		// This step runs on the host, and the host cannot dial the pods'
		// address under podman (host.containers.internal, runtime.go): the
		// autodetected server is this machine's, so read it on loopback.
		endpoint = loopbackBase(backendName)
	}
	server := config.BackendServerName(backendName)
	teardown := "delete"
	if !spec.deleteOverREST {
		teardown = fmt.Sprintf("delete refused (%d) -> unwire", http.StatusNotImplemented)
		if err := proveDeleteRefused(&api, session, backendName, model, mcName, endpoint, toolPrefix); err != nil {
			return err
		}
	} else {
		step("Deleting %s", model)
		if err := api.deleteModel(model, backendName); err != nil {
			return err
		}
		if err := waitModelConfigGone(mcName); err != nil {
			return err
		}
		note("ModelConfig %s is gone", mcName)
		remaining, base, err := hostInventory(backendName, endpoint)
		if err != nil {
			return fmt.Errorf("reading the host %s's models at %s: %w", server, base, err)
		}
		if hasModel(remaining, model) {
			return fmt.Errorf("host %s at %s still has %s after the delete", server, base, model)
		}
		note("host %s at %s no longer has it (%d models left)", server, base, len(remaining))
		text, err = session.callServerTool(toolPrefix+"list_models", map[string]any{backendField: backendName})
		if err != nil {
			return err
		}
		if strings.Contains(text, `"`+model+`"`) {
			return fmt.Errorf("%slist_models still lists %s", toolPrefix, model)
		}
		note("%slist_models agrees", toolPrefix)
	}

	fmt.Println()
	fmt.Println("PASS: no token -> 401 at the gateway; Dex token -> model-manager REST through agentgateway")
	fmt.Printf("PASS: %s backend: list -> pull %s (progress) -> ModelConfig %s (%s provider, backend label) Accepted -> agent turn -> unload -> %s\n",
		backendName, model, mcName, spec.provider, teardown)
	fmt.Printf("PASS: muster aggregates x_%s_* and calls them (get_model, list_models)\n", modelManagerMCPServer)
	fmt.Printf("PASS: the caller's identity — job requestedBy=%s; a viewer's wire is Forbidden by the apiserver (user RBAC, not the ServiceAccount's)\n", user.Email)
	if !spec.deleteOverREST {
		fmt.Printf("NOTE: %s is still downloaded on the host — %s has no delete over its API. Remove it there: `%s`\n",
			model, server, fmt.Sprintf(spec.removeHint, model))
	}
	return nil
}

// hostInventory reads a host model server's downloaded models from THIS
// machine — the ground truth behind the proof's delete assertions — and
// returns the base it actually read, so every message about the result names
// that one rather than the endpoint that was asked for.
//
// The endpoint the platform uses is the one PODS dial, and some of those
// addresses exist only inside the cluster: Docker Desktop's
// host.docker.internal, podman's host.containers.internal. That, and only
// that, is what the loopback fallback is for. It fires on the dial error that
// says so — the name did not resolve — and not on any error: a configured LAN
// endpoint (docs/models.md supports one) that is merely down must fail the
// read, never silently retarget it at a local server of the same kind, whose
// different library would make the delete assertions report on a machine they
// never touched.
//
// Only the HOST is replaced, never the port: an endpoint on a non-default port
// falls back to the same port on loopback. Substituting the backend's default
// port instead would dial a different server, which is the confusion this
// whole function exists to prevent.
//
// The caller above picks loopback outright for the one case it can predict
// (podman with no configured endpoint), which keeps a guaranteed-failing dial
// out of the run; this is the net for the cases it cannot. Both earn their
// keep: neither is the other's leftover.
func hostInventory(backend, endpoint string) ([]HostModel, string, error) {
	models, err := hostModelsFn(backend, endpoint)
	if err == nil {
		return models, endpoint, nil
	}
	if !isUnresolvedHost(err) {
		return nil, endpoint, err
	}
	loopback := loopbackFor(backend, endpoint)
	if loopback == endpoint {
		return nil, endpoint, err
	}
	models, loopbackErr := hostModelsFn(backend, loopback)
	if loopbackErr != nil {
		return nil, endpoint, fmt.Errorf("%w (and %s: %w)", err, loopback, loopbackErr)
	}
	note("%s does not resolve from this machine (it is the address pods dial); read %s instead", endpoint, loopback)
	return models, loopback, nil
}

// isUnresolvedHost reports whether a failed read failed because the endpoint's
// host does not exist for this machine. The dial already asked the resolver,
// so its answer is the evidence — no second lookup, no deadline of our own,
// and nothing for a resolver that synthesises addresses for unknown names to
// get wrong.
func isUnresolvedHost(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// loopbackFor is the endpoint with its host replaced by loopback, keeping the
// port it named; loopbackBaseFn supplies the base (and the default port) when
// the endpoint carries none.
func loopbackFor(backend, endpoint string) string {
	base := loopbackBaseFn(backend)
	u, err := url.Parse(endpoint)
	if err != nil || u.Port() == "" {
		return base
	}
	b, err := url.Parse(base)
	if err != nil {
		return base
	}
	b.Host = net.JoinHostPort(b.Hostname(), u.Port())
	return b.String()
}

// waitModelConfigGone waits for a ModelConfig to disappear from the kagent
// namespace. It asks the apiserver through objectExists, whose (false, nil)
// means NotFound and nothing else: reading "gone" out of ANY failed read
// would turn an apiserver restart or a lost context during the teardown into
// a PASS for an object that is still there.
func waitModelConfigGone(mcName string) error {
	gvr, err := gvrFor(modelConfigResource)
	if err != nil {
		return err
	}
	var lastErr error
	gone := waitFor(15, 2*time.Second, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		exists, err := objectExists(ctx, gvr, kagentNamespace, mcName)
		lastErr = err
		return err == nil && !exists
	})
	if !gone {
		if lastErr != nil {
			return fmt.Errorf("reading ModelConfig %s: %w", mcName, lastErr)
		}
		return fmt.Errorf("ModelConfig %s survived", mcName)
	}
	return nil
}

// proveDeleteRefused is the teardown of a backend whose server cannot delete a
// model (LM Studio: that is `lms rm` on the host, which no pod can run). The
// refusal is a stronger proof than skipping the step: the platform must answer
// 501 rather than pretend, the model must still be there afterwards — a
// refused delete that removed something would be worse than one that refuses —
// and the ModelConfig must still come off through the supported route, which
// shows wiring and inventory are independent.
func proveDeleteRefused(api *modelManagerAPI, session *musterSession,
	backendName, model, mcName, endpoint, toolPrefix string) error {
	server := config.BackendServerName(backendName)
	step("Deleting %s — expecting the refusal (%s has no delete over its API)", model, server)
	status, body, _, err := api.do(http.MethodDelete, "/models/"+model+"?backend="+url.QueryEscape(backendName), nil)
	if err != nil {
		return err
	}
	if status/100 == 2 {
		return fmt.Errorf("DELETE /models/%s answered %d: %s cannot delete a model, so the platform must refuse instead of reporting success",
			model, status, server)
	}
	// The contract is the status, not a word in the body: 501 is what maps
	// the driver's ErrUnsupported, and accepting any non-2xx that happens to
	// say "unsupported" would pass a 400 validation error or a gateway's 502
	// while the PASS line went on claiming a 501.
	if status != http.StatusNotImplemented {
		return fmt.Errorf("DELETE /models/%s answered %d, want %d (the platform's mapping of the driver's unsupported delete): %.300s",
			model, status, http.StatusNotImplemented, body)
	}
	if !strings.Contains(strings.ToLower(string(body)), "unsupported") {
		return fmt.Errorf("DELETE /models/%s answered %d but not with the unsupported code: %.300s", model, status, body)
	}
	note("HTTP %d, %s", status, excerpt(string(body), 120))

	// The refusal must have changed nothing: still downloaded, still wired,
	// still listed.
	remaining, base, err := hostInventory(backendName, endpoint)
	if err != nil {
		return fmt.Errorf("reading the host %s's models at %s: %w", server, base, err)
	}
	if !hasModel(remaining, model) {
		return fmt.Errorf("host %s at %s no longer has %s after a refused delete", server, base, model)
	}
	if _, err := outputQuiet("kubectl", "-n", kagentNamespace, "get", modelConfigResource, mcName); err != nil {
		return fmt.Errorf("ModelConfig %s disappeared after a refused delete: %w", mcName, err)
	}
	note("nothing changed: still downloaded on the host, ModelConfig %s still there", mcName)

	step("Unwiring %s — the teardown %s does offer", model, server)
	if status, body, _, err := api.do(http.MethodPost, "/models/unwire",
		map[string]any{modelField: model, backendField: backendName}); err != nil {
		return err
	} else if status/100 != 2 {
		return fmt.Errorf("POST /models/unwire answered %d: %.300s", status, body)
	}
	if err := waitModelConfigGone(mcName); err != nil {
		return err
	}
	note("ModelConfig %s is gone", mcName)
	// Unwiring touches the ModelConfig only — the weights stay.
	// The base the first read settled on, so the fallback (and its note) does
	// not run a second time.
	remaining, err = hostModelsFn(backendName, base)
	if err != nil {
		return fmt.Errorf("reading the host %s's models at %s: %w", server, base, err)
	}
	if !hasModel(remaining, model) {
		return fmt.Errorf("host %s at %s lost %s to an unwire, which must only remove the ModelConfig", server, base, model)
	}
	text, err := session.callServerTool(toolPrefix+"list_models", map[string]any{backendField: backendName})
	if err != nil {
		return err
	}
	if !namesModel(text, model) {
		return fmt.Errorf("%slist_models no longer lists %s, which is still downloaded", toolPrefix, model)
	}
	note("%slist_models still lists it (%d models on the host)", toolPrefix, len(remaining))
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
		return err == nil && !slices.ContainsFunc(names, func(n string) bool { return sameModel(n, model) })
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

// agentTurn creates the proof's throwaway agent on the ModelConfig through
// agent-manager as the person (a chat-only agent: no tools, so the model
// needs none), waits until it is Ready on the platform Harness (readyAgent),
// drives one turn on it as the same person through the edge and returns the
// agent's text. The agent is removed on every path.
func agentTurn(cfg *config.Config, session *musterSession, token, modelConfig, prompt string) (string, error) {
	defer func() {
		if err := removeAgent(modelsTestAgent); err != nil {
			note("cleanup: %v", err)
		}
	}()
	if agentExists(modelsTestAgent) {
		note("removing the leftover agent %s from an earlier run", modelsTestAgent)
		if err := removeAgent(modelsTestAgent); err != nil {
			return "", err
		}
	}
	// The model itself is loaded by the host server on the first turn (the
	// turn timeout covers it).
	if _, _, err := readyAgent(agentManagerWriter{session}, agentSpec{
		Name: modelsTestAgent, ModelConfig: modelConfig, DisplayName: "agentlab models-test", Toolset: []string{presetNone},
		Description:   "Throwaway agent of `agentlab models-test` on the model the run pulled; deleted by the same run.",
		SystemMessage: "You are a terse assistant. Answer in one short line.",
	}, agentReadyTimeout); err != nil {
		return "", err
	}
	return firstTurnAs(cfg, modelsTestAgent, token, prompt)
}

// modelConfigExists reports whether the ModelConfig is there; a read that
// fails counts as absent, as the CLI probe's non-zero exit did.
func modelConfigExists(name string) bool {
	_, err := readKagentObject(modelConfigResource, name)
	return err == nil
}

// modelConfigSummary words a ModelConfig the way the proof's jsonpath did:
// "<provider> <model> <ollama host or openAI baseUrl> backend=<backend label>
// managed-by=<managed-by label>" — the string the provider and label
// assertions read.
func modelConfigSummary(mc *unstructured.Unstructured) string {
	provider, _, _ := unstructured.NestedString(mc.Object, "spec", "provider")
	model, _, _ := unstructured.NestedString(mc.Object, "spec", "model")
	ollamaHost, _, _ := unstructured.NestedString(mc.Object, "spec", "ollama", "host")
	openAIBaseURL, _, _ := unstructured.NestedString(mc.Object, "spec", "openAI", "baseUrl")
	labels := mc.GetLabels()
	return fmt.Sprintf("%s %s %s%s backend=%s managed-by=%s", provider, model, ollamaHost, openAIBaseURL,
		labels["model-manager.giantswarm.io/backend"], labels[managedByLabel])
}

// One rule for "is this the model we asked about", used by every comparison
// in the run.
//
// Case-folded, because model-manager's lmstudio and lemonade drivers resolve
// names with strings.EqualFold: `--model IBM/granite-4-micro` otherwise
// pulls, wires and answers an agent turn, and then the run aborts on a
// byte-exact compare of the name the platform accepted.
//
// And on JSON text, matched as a quoted "name" field rather than anywhere in
// the document: LM Studio keys share prefixes (`ibm/granite-4-micro` next to
// `ibm/granite-4-micro@q4_k_m`), so a substring check passes on a neighbour
// and would report a model as "still listed" after it had been dropped.
func sameModel(a, b string) bool { return strings.EqualFold(a, b) }

// namesModel reports whether an MCP tool's JSON answer carries the model as a
// "name" field.
func namesModel(text, model string) bool {
	for _, m := range jsonStringField(text, "name") {
		if sameModel(m, model) {
			return true
		}
	}
	return false
}

// hasModel reports whether a host inventory holds the model.
func hasModel(models []HostModel, model string) bool {
	return slices.ContainsFunc(models, func(m HostModel) bool { return sameModel(m.ID, model) })
}

// jsonStringField collects the values of every `"<field>": "..."` in text —
// enough for the tool answers this proof reads, which are objects or arrays of
// objects, without decoding a schema per tool.
func jsonStringField(text, field string) []string {
	var out []string
	needle := `"` + field + `"`
	for rest := text; ; {
		i := strings.Index(rest, needle)
		if i < 0 {
			return out
		}
		rest = rest[i+len(needle):]
		colon := strings.Index(rest, ":")
		if colon < 0 {
			return out
		}
		v := strings.TrimLeft(rest[colon+1:], " \t\n\r")
		if !strings.HasPrefix(v, `"`) {
			rest = v
			continue
		}
		v = v[1:]
		end := strings.Index(v, `"`)
		if end < 0 {
			return out
		}
		out = append(out, v[:end])
		rest = v[end+1:]
	}
}

func decodeJSONBody(resp *http.Response, out any) error {
	raw, err := readJSONBody(resp)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// readJSONBody returns a 200's body, for a caller that decodes it itself.
func readJSONBody(resp *http.Response) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxProbeBody {
		return nil, fmt.Errorf("answer larger than %d bytes", maxProbeBody)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %.200s", resp.Status, raw)
	}
	return raw, nil
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
