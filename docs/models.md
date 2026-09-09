# Models

Which models the agents can run on, and how the lab wires them. The default
is the Anthropic `ModelConfig` the kagent chart renders from `aiModel` (see
[Agents](agents.md)); this page covers everything beyond it: static extra
model configs, model servers on the lab host, and the managed mode where
model-manager fronts those servers.

## Extra model configs (self-hosted, OpenRouter, Gemini, OpenAI)

Beyond the default Anthropic ModelConfig, `platform.extraModels` in
`agentlab.yaml` adds more — a self-hosted OpenAI-compatible endpoint (vLLM,
llama.cpp, LM Studio), OpenRouter, Gemini, a plain GPT model, or an Ollama
host. Each entry becomes a lab-labeled `ModelConfig` CR in the `kagent`
namespace, selectable when composing an agent (the kagent UI's model dropdown,
or `modelConfig` on an `Agent` CR):

```yaml
platform:
  extraModels:
    # A self-hosted vLLM — any OpenAI-compatible endpoint works the same way.
    # No apiKeyEnv: the endpoint is keyless, a placeholder key is shipped.
    - name: qwen3-8-27b
      provider: OpenAI
      model: qwen3-8-27b
      baseUrl: https://qwen.example.internal/v1
    # OpenRouter: also just an OpenAI-compatible endpoint plus a key.
    - name: openrouter-deepseek
      provider: OpenAI
      model: deepseek/deepseek-chat
      baseUrl: https://openrouter.ai/api/v1
      apiKeyEnv: OPENROUTER_API_KEY
    - name: gemini-flash
      provider: Gemini
      model: gemini-2.5-flash
      apiKeyEnv: GEMINI_API_KEY
    - name: local-llama
      provider: Ollama
      model: llama3.3
      baseUrl: http://192.168.1.10:11434
```

`agentlab configure` asks for these interactively (the "extra model configs"
confirm in the platform group); `agentlab platform` (or `agentlab up`)
applies them and waits for the kagent controller to accept each one. Entries
removed from `agentlab.yaml` are **pruned** on the next run — the managed-by
label scopes the pruning to lab-created ModelConfigs, so the chart's default
one is never touched.

Key handling follows the Anthropic pattern: `apiKeyEnv` names a host env var
read at deploy time, and the value lands only in the Secret
`kagent/kagent-<name>` (created once, left alone — delete it and re-run to
rotate; never in `agentlab.yaml` or `state/`). The key *inside* the Secret is
provider-derived (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GOOGLE_API_KEY`)
because the kagent controller injects it as an env var of exactly that name
and the ADK runtime looks up those canonical names. That is also why keyless
endpoints still get a Secret with a placeholder value: the runtime requires
the env var to *exist* — an agent pod without it crashloops before ever
talking to the endpoint.

Two practical notes for self-hosted endpoints: the URL must be reachable
**from inside the kind node's pods** (a LAN IP or resolvable hostname —
`localhost` would be the pod itself), and a self-signed certificate needs
`insecureTLS: true` on the entry (rendered as the ModelConfig's
`tls.disableVerify`). `Gemini` takes no `baseUrl` (the CRD has no endpoint
field for it), and `Ollama` requires one (its `host`) and is keyless.
Providers needing more than a model + endpoint + key (AzureOpenAI, Bedrock,
Vertex) are out of the lab's vocabulary — create their ModelConfigs by hand.

## Local backends on the lab host (Ollama, Lemonade/NPU, LM Studio)

A model server on the lab host itself is the cheapest self-hosted endpoint,
and everything that can go wrong is host-side plumbing, not kagent:

- **Address**: pods reach the host only through the kind docker network's
  gateway — `docker network inspect kind` names it, typically `172.21.0.1`.
  That IP goes in `baseUrl`; `localhost` would be the agent pod itself.
- **Bind address**: the server must listen on `0.0.0.0` (or the bridge IP).
  The usual `127.0.0.1` default is unreachable from pods regardless of any
  firewall rule. Ollama: `OLLAMA_HOST=0.0.0.0`. Lemonade:
  `lemonade config set host=0.0.0.0`. LM Studio: `lms server start --bind
  0.0.0.0`, the app's Developer → "Serve on Local Network" toggle, or
  `LMS_SERVER_HOST=0.0.0.0`.
- **Host firewall**: on a default-deny INPUT host, pod→host traffic arrives
  on the docker bridge like any other inbound connection and gets dropped —
  allow the server's TCP port from the docker bridge subnets (they fall
  inside `172.16.0.0/12`). The symptom is an agent replying
  `Connection error.` (`kagent_error_code: API_ERROR`) while the same URL
  works from the host.
- **Keep-alive / eviction (Ollama)**: Ollama loads a model on the first
  `/api/chat` that names it — an agent on a not-loaded model works, its
  first turn pays the cold start — and evicts it when the keep-alive runs
  out. The keep-alive is set **per request**: each request's `keep_alive`,
  else the server's `OLLAMA_KEEP_ALIVE` (5m unless set), re-arms the timer
  on every hit. kagent sends no `keep_alive`, so agent turns always re-arm
  the server default, and a load through model-manager or the portal
  (`keepAlive`, even `-1`) only pre-warms until the next agent request. The
  knob for what agents experience is host-side: `OLLAMA_KEEP_ALIVE=30m` (or
  `-1` for never) in the Ollama service environment — on a systemd host
  `systemctl edit ollama` with `[Service]` `Environment="OLLAMA_KEEP_ALIVE=30m"`,
  then restart Ollama. Nothing in model-manager changes this; its
  `GET /api/v1/backend` reports the mechanics as `loading` (`onDemand`,
  `idleEviction`, `keepAliveScope: request`) so the portal can say "idle,
  loads on first request" instead of "not loaded".

Both of these are keyless OpenAI-compatible endpoints, so the entries are
minimal:

```yaml
platform:
  extraModels:
    # Ollama on the host, via its OpenAI-compatible /v1 alias.
    - name: ollama-local
      provider: OpenAI
      model: qwen3.5:9b
      baseUrl: http://172.21.0.1:11434/v1
    # Lemonade Server (lemonade-server.ai): local inference with NPU
    # acceleration on AMD Ryzen AI (XDNA2) through its FastFlowLM backend,
    # or GPU via llama.cpp. Pick a tool-calling-capable model (the model
    # list labels them) — agents send tool schemas with every turn.
    - name: lemonade-npu
      provider: OpenAI
      model: qwen3-it-4b-FLM
      baseUrl: http://172.21.0.1:13305/v1
    # LM Studio (lmstudio.ai) on the host: llama.cpp on GPU/CPU, MLX on
    # Apple silicon. The model is LM Studio's own key (`lms ls`), and it
    # must be one trained for tool use — LM Studio accepts `tools` for any
    # model and emulates them through the prompt, which agents trip over.
    - name: lmstudio-local
      provider: OpenAI
      model: ibm/granite-4-micro
      baseUrl: http://172.21.0.1:1234/v1
```

One Lemonade-specific note: its FastFlowLM models default to a 4096-token
context, which agent system prompts plus tool schemas outgrow quickly —
raise it once with `lemonade config set ctx_size=16384`. LM Studio has the
same trap per model (its context is a load-time setting) plus one of its own:
keep **just-in-time model loading** on (its default), or an agent whose
ModelConfig names a downloaded-but-unloaded model fails its first turn
instead of waiting for the load.

## Managed models: model-manager + the host model servers

`extraModels` wires an endpoint and manages nothing: pulling or removing a
model is CLI-on-host, and nothing shows what is downloaded or loaded. The
managed mode puts the chart's **model-manager** component
([giantswarm/model-manager](https://github.com/giantswarm/model-manager),
the service behind the Model Manager epic) in front of a model server on the
host — inventory of downloaded and loaded models, pull with progress,
load/unload, delete, and every pulled model wired into kagent automatically
as a `ModelConfig`: the native, keyless `Ollama` provider for an Ollama, the
`OpenAI` provider (placeholder key) on `/api/v1` for a Lemonade Server and on
`/v1` for an LM Studio. One block, which `agentlab configure` writes from what
answers on the machine:

```yaml
platform:
  agents: true                 # required: the ModelConfigs land in kagent
  modelManager:
    enabled: true
    backends: [ollama, lemonade, lmstudio]   # the host model servers, Ollama first; kserve needs GPUs + KServe
    # endpoints:                   # optional; empty autodetects the host
    #   ollama: http://192.168.1.10:11434
```

**One model-manager fronts all of them** (model-manager ≥ 0.17.0; the
chart's `model-manager.backends`): inventory, pull, load/unload, delete and
the auto-wired ModelConfigs work per backend, `GET /api/v1/backends` lists
them, every object says which `backend` it belongs to, and every ModelConfig
carries the `model-manager.giantswarm.io/backend` label. The list has an
order because the first entry is the **default backend** — where a request
that names none goes; the REST API takes `?backend=` (reads) or `"backend"`
(writes), the MCP tools a `backend` argument. The one-backend form earlier
versions wrote (`backend:` + `endpoint:`) still reads as the one-item list.

`agentlab configure` detects an Ollama on `:11434`, a Lemonade Server on
`:13305` and an LM Studio on `:1234` on every run (`--model-manager[=false]`
pins the flag, `--model-manager-backends` the list; the interactive form shows
what was found). Each endpoint is **autodetected at platform time** as `http://<kind
docker network gateway>:<default port>` — `docker network inspect kind`, the
same address the section above documents for `extraModels` — so nobody types
`172.21.0.1`; set `endpoints.<backend>` for a server elsewhere on the LAN
(such a backend is kept whether or not one answers locally).

What `agentlab platform` (or `up`) does with it:

- **Preflight, not documented traps.** Before the install, a short-lived pod
  in the cluster fetches each server's identifying document (`/api/version` on
  Ollama, `/api/v1/health` on Lemonade, `/api/v1/models` on LM Studio — which
  serves no version or health endpoint at all, so the inventory's shape is
  what identifies it). If that fails, the boot stops right there with the
  diagnosis and the two fixes from the section above spelled out —
  *connection refused* means the server listens on `127.0.0.1` only
  (`OLLAMA_HOST=0.0.0.0` / `lemonade config set host=0.0.0.0` / `lms server
  start --bind 0.0.0.0`), a *timeout* means the host firewall drops pod→host
  traffic on the docker bridge (allow the server's TCP port from the bridge
  subnets, inside `172.16.0.0/12`) — instead of a model-manager pod reporting
  an unhealthy backend after Helm's ten-minute wait, or ModelConfigs pointing
  at a dead endpoint.
- **The chart's `components.model-manager`** goes on with every listed
  backend (`model-manager.backends` plus one `model-manager.<backend>.endpoint`
  each = the detected addresses; a single entry renders the chart's `backend:`
  form), its agentgateway **route** at
  `https://agentgateway.<domain>/model-manager` and, unlike the lab's kagent
  route, **JWT validation on**: the gateway verifies the caller's Dex token
  against the lab Dex (JWKS over TLS at
  `dex.dex.svc.cluster.local:5556/dex/keys`, trusted through the lab CA) and
  answers 401 without one. model-manager checks no identity itself — the
  gateway is the boundary, the same trust model as the kagent controller
  route on real installations.
- **The portal's service side.** The chart's Backstage app-config gains
  `agentPlatform.modelManager.installations.agent-platform.apiBaseUrl:
  https://agentgateway.<domain>/model-manager`; the portal backend forwards
  the signed-in user's Dex ID token to it. The portal renders one Serving
  group per backend of the installation (giantswarm/backstage#2264); what
  else the Models tab shows is the portal's business (giantswarm/backstage#2194).
- **muster** registers the MCP endpoint (the chart's own `MCPServer` CR,
  `Connected` is waited for) and the tools surface as
  `x_model-manager_<tool>`: `list_models`, `get_model`, `list_loaded_models`,
  `pull_model`, `load_model`, `unload_model`, `delete_model`, `wire_model`,
  `unwire_model`, `list_jobs`, `get_job`, `cancel_job`, `get_backend` — ask
  Claude Code to pull a model.

The proof is `agentlab models-test` — one backend per run: `--backend
lemonade` (or `--backend lmstudio`) proves that server through the same
model-manager (default: the first of the list). `--model` picks another small,
**tool-calling capable** model; the defaults are `qwen2.5:0.5b` (~400 MB) on
ollama, `qwen3-4b-FLM` (3.1 GB, the smallest tool-calling FastFlowLM model —
the smaller `*-FLM` ones cannot call tools) on lemonade and
`ibm/granite-4-micro` (~2 GB) on lmstudio, an LM Studio hub reference so the
download resolves the variant that fits the host (GGUF on Linux and NVIDIA,
MLX on Apple silicon); `smollm2:135m` pulls fine and then fails every agent
turn with "does not support tools". Every request names the backend, the
ModelConfig must carry the backend label, and the run goes through the
platform path only.

**On lmstudio the run ends differently, and deliberately so.** LM Studio has
no delete over its API — removing a model is `lms rm` on the host, which no
pod can run — so model-manager reports `delete: false` and the proof asserts
the *refusal* instead of skipping a step: the platform must answer `501
unsupported`, the model must still be downloaded and still wired afterwards
(a refused delete that removed something would be worse than one that
refuses), and the ModelConfig must then come off through the route that does
exist, `POST /models/unwire`. The run also cross-checks the advertised
`delete` capability against what the server really offers, in both
directions. It is therefore the one backend that **does** leave something
behind — the model stays downloaded, and the last line says so.

The ollama and lemonade runs leave nothing behind:

```
agentlab models-test
==> Calling the model-manager API without a token         -> 401 at the gateway
==> Logging in to Dex as admin@lab.local
==> Backend through the gateway with the Dex token         -> ollama, healthy, capabilities
==> Listing models
==> Pulling qwen2.5:0.5b (progress via GET /api/v1/jobs/{id})
==> Auto-created kagent ModelConfig                        -> Ollama provider, Accepted
==> Agent turn on qwen2-5-0-5b (kagent Agent, runtime go -> host Ollama)
==> MCP tools through muster (x_model-manager_*)           -> get_model
==> Unloading qwen2.5:0.5b                                 -> gone from /loaded
==> Deleting qwen2.5:0.5b                                  -> gone from Ollama, ModelConfig gone, list_models agrees
```

Load and unload through model-manager (or the portal's Load) pre-warm and
evict; they do not change how long agent traffic keeps a model resident —
that is `OLLAMA_KEEP_ALIVE` on the host, see the keep-alive note in the
section above.

Both modes coexist: the static `extraModels` entries stay as they are
(labeled `managed-by: agentlab`), model-manager's ModelConfigs carry
`managed-by: model-manager`, and neither prunes the other's.
