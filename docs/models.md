# Models

Which models the agents can run on, and how the lab wires them. The default
is the Anthropic `ModelConfig` the kagent chart renders from `aiModel` (see
[Agents](agents.md)); this page covers everything beyond it: static extra
model configs, model servers on the lab host, the managed mode where
model-manager fronts those servers, and the platform's own serving on llm-d.

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
    # Ollama's native API. A host with less than 24 GiB of VRAM needs
    # OLLAMA_CONTEXT_LENGTH set, or agent prompts are cut (the
    # context-length note below).
    - name: local-llama
      provider: Ollama
      model: llama3.3
      baseUrl: http://192.168.1.10:11434
```

`agentlab configure` asks for these interactively (the "extra model configs"
confirm in the platform group); `agentlab platform` (or `agentlab up`)
applies them and waits for the kagent controller to accept each one. The CRs
are rendered at the kagent API version the chart line serves —
`kagent.dev/v1alpha3` on the 4.x line, `kagent.dev/v1alpha2` on a released
3.x chart (kagent 0.10); the ModelConfig spec is the same in both. Entries
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
field for it), and `Ollama` requires one (its `host`) and is keyless. An
`OpenAI` entry may set `reasoningEffort` (`none`, `minimal`, `low`,
`medium`, `high` or `xhigh`), rendered as the ModelConfig's
`openAI.reasoningEffort`, and an `Ollama` entry may set `think` (`true` or
`false`, kagent 1.0.3 or newer), rendered as the ModelConfig's
`ollama.think`; `reasoningEffort: none` and `think: false` switch a local
model's thinking off (see
[Agent proofs without an Anthropic key](#agent-proofs-without-an-anthropic-key)).
Providers needing more than a model + endpoint + key (AzureOpenAI, Bedrock,
Vertex) are out of the lab's vocabulary — create their ModelConfigs by hand.

## Local backends on the lab host (Ollama, Lemonade/NPU, LM Studio)

A model server on the lab host itself is the cheapest self-hosted endpoint,
and everything that can go wrong is host-side plumbing, not kagent:

- **Address**: pods reach the host only through the kind docker network's
  gateway — `docker network inspect kind` names it, typically `172.21.0.1`.
  That IP goes in `baseUrl`; `localhost` would be the agent pod itself.
- **Docker in a VM (Docker Desktop on macOS or Windows, Colima, a podman
  machine)**: the kind gateway is a bridge address *inside that VM*, so it is
  not the machine your model server runs on and no bind address can make it
  one. The server is reachable as **`host.docker.internal`** instead
  (`host.containers.internal` under podman), which resolves only from inside
  the cluster. Nothing to configure: the lab dials the gateway from inside the
  node, and where that does not answer it tries the alias and uses whichever
  does. `agentlab configure` reports which address it found —

  ```
  LM Studio   api v1 on :1234 — pods reach it at host.docker.internal
              (172.18.0.1 is inside the container runtime's VM, not this machine)
  ```

  — and `agentlab platform` wires that one. This applies to every host backend
  equally, not just LM Studio.
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
- **Context length (Ollama)**: unless told otherwise, Ollama sizes a
  model's context from the host's VRAM: 4,096 tokens below 24 GiB, 32,768
  up to 48 GiB, 256k above (capped at the model's own maximum). A host
  without a GPU has 0 B and gets 4,096, which Ollama logs when it starts:
  `vram-based default context total_vram="0 B" default_num_ctx=4096`. Apple
  silicon counts the GPU's share of unified memory, so smaller Macs are in
  the 4k tier too. Every model call of an agent turn sends the system
  prompt, all tool schemas and the conversation so far, so the prompt grows
  with each tool result. Once it no longer fits the 4,096, Ollama cuts it
  **silently** to half the context: the first 4 tokens and the last 2,046.
  The system prompt and the tool schemas at the front are gone. The agent
  ignores its instructions or its tools, the API response shows nothing,
  and the only trace is a warning in the server log:

  ```
  level=WARN msg="truncating input prompt" limit=2050 prompt=10735 keep=4 new=2050
  ```

  The fix is on the host, set the same way as the keep-alive:
  `OLLAMA_CONTEXT_LENGTH=32768` in the Ollama service environment. On a
  systemd host, run `systemctl edit ollama` and add
  `Environment="OLLAMA_CONTEXT_LENGTH=32768"` under `[Service]`. On macOS,
  run `launchctl setenv OLLAMA_CONTEXT_LENGTH 32768`. Then restart Ollama.
  Once the next turn has loaded the model, `ollama ps` shows `32768` in its
  `CONTEXT` column. 32,768 holds an agent's prompt plus several turns of tool
  results. Ollama's docs suggest 64,000 for agent work if the RAM allows:
  the cache for the whole context is reserved when the model loads. For
  `extraModels` the host setting is the only fix. An entry carries no
  per-model context, and the `/v1` alias has no field for one. For the
  ModelConfigs model-manager wires, see
  [giantswarm/model-manager#157](https://github.com/giantswarm/model-manager/issues/157).

All three are keyless OpenAI-compatible endpoints, so the entries are
minimal:

```yaml
platform:
  extraModels:
    # Ollama on the host, via its OpenAI-compatible /v1 alias. The alias
    # takes no context size: below 24 GiB of VRAM, set
    # OLLAMA_CONTEXT_LENGTH on the host (the context-length note above).
    # reasoningEffort: none keeps a thinking model's answer out of its
    # thinking (Agent proofs without an Anthropic key, below).
    - name: ollama-local
      provider: OpenAI
      model: qwen3.5:9b
      baseUrl: http://172.21.0.1:11434/v1
      reasoningEffort: none
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

### Agent proofs without an Anthropic key

`toolsets-test`, `skills-test` and `klaus-gateway-test` run their agents on
`default-model-config`, the Anthropic ModelConfig. Without
`$ANTHROPIC_API_KEY`, pass `--model-config <name>` to run them on a model
on the host. A CPU does it with **`qwen3.5:2b`** (2.7 GB) on Ollama, with
the model's thinking switched off, on kagent's native `Ollama` provider or
on Ollama's `/v1` alias:

```bash
ollama pull qwen3.5:2b
```

```yaml
platform:
  extraModels:
    - name: qwen35-2b
      provider: Ollama
      model: qwen3.5:2b
      baseUrl: http://172.21.0.1:11434
      think: false              # thinking off, see below
    - name: qwen35-2b-v1
      provider: OpenAI
      model: qwen3.5:2b
      baseUrl: http://172.21.0.1:11434/v1
      reasoningEffort: none     # thinking off, see below
```

```bash
agentlab platform
agentlab skills-test --model-config qwen35-2b
```

On a 12-core Zen 5 laptop CPU with no GPU, Ollama on 8 of its cores with
8 threads and `OLLAMA_CONTEXT_LENGTH=32768`, `skills-test` passes in under a
minute. Its turn is three model calls of 1.8k to 2.8k tokens, 3–6 s each
once the model is loaded, the whole turn 7–13 s. The two entries do equally
well: five runs each passed 4 on the native provider and 3 on the `/v1`
alias, and every miss was the 2B model leaving the skill's answer out of a
reply it did give. The loaded model takes 2.8 GB with the 32k context.
Apply the context-length note above first. `skills-test`'s agent binds no
tools and stays under 4,096 tokens, but an agent with muster's tools carries
their schemas on top, and every tool result makes the conversation longer.

- **Thinking has to be off.** Qwen3.5, Qwen3 and Granite 4.2 think by
  default under Ollama. With thinking on, `qwen3.5:2b` calls both tools
  correctly on `skills-test` but writes its answer only into its thinking,
  and the agent returns an empty reply. `granite4.2:3b` does answer, but it
  thinks 1,500–2,000 tokens a call, which takes 2–3 minutes each on a CPU
  and misses the proof's 180 s turn bound. `think: false` on an `Ollama`
  entry is sent as the chat request's `think` field; it needs kagent 1.0.3
  or newer, whose ModelConfig CRD has `ollama.think`. `reasoningEffort:
  none` on the `/v1` alias reaches Ollama as `reasoning_effort`; it needs
  the kagent line (meta chart 4.x), since the 3.x chart's kagent refuses
  `none`.
- **Ollama's threads follow the host, not a core limit.** Ollama starts one
  thread per physical core of the host, also under `taskset` or in a
  container with a cpuset. Limited to 8 of 12 cores it still runs 12
  threads (`n_threads = 12` in its log), and a call of the same turn takes
  47 s to 2 min instead of 3–6 s. Give it `num_thread` for the cores it
  has: a Modelfile with `PARAMETER num_thread 8` on top of the model
  (`ollama create`), which every provider then gets.
- **The ModelConfigs model-manager wires** use the native `Ollama` provider
  without `think`, so a thinking model answers empty there
  ([giantswarm/model-manager#161](https://github.com/giantswarm/model-manager/issues/161)).
  Use one of the entries above for the proofs.
- **Why `qwen3.5:2b`.** It passed five tool-calling cases, each run five
  times against Ollama on the same CPU with thinking off: pick a tool and
  its arguments, a three-argument call, an enum argument, no call when none
  is needed, and acting on a tool result.

| Model (Ollama tag) | Download | Passed | Seconds a call |
|---|---|---|---|
| **`qwen3.5:2b`** | 2.7 GB | 25/25 | 3.0 |
| `granite4.2:3b` | 2.2 GB | 25/25 | 3.3 |
| `qwen2.5:0.5b` (`models-test`'s default) | 0.4 GB | 22/25 | 0.8 |

- **`granite4.2:3b`** is the alternative when many sessions share one long
  prompt. It passed the cases but has not been run through the proofs. It
  is a dense transformer, so Ollama reuses the cached system prompt and
  tool schemas from one conversation to the next. Qwen3.5 is a hybrid (most
  of its layers are Gated DeltaNet), and for those llama.cpp reads the
  whole prompt again in every new conversation.
- **`qwen2.5:0.5b` is for `models-test` only.** Its turn asks for "pong"
  and calls no tool. On tool calls the model missed the enum argument in 3
  of 5 runs.

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
what was found). The flag off is the chart's component off: the rendered
values state `components.model-manager.enabled: false`, because the meta
chart runs model-manager by default since 4.24.0 — with no backend, which in
the lab would only crash-loop on the Dex localhost address. Each endpoint is **autodetected at platform time**: the kind
docker network's gateway (`docker network inspect kind`, the same address the
section above documents for `extraModels`), or the container runtime's host
alias where that gateway is inside its VM — whichever answers when dialled
from inside the node. So nobody types `172.21.0.1`. Set `endpoints.<backend>`
for a server the lab cannot find that way, such as one elsewhere on the LAN;
such a backend is kept whether or not one answers locally.

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

## Model serving on llm-d (`platform.serving`)

The serving slice a Giant Swarm installation runs, on the kind node: the
platform serves a model itself instead of fronting a server on the host.
`agentlab configure --serving` turns it on (the form asks too; it needs the
agents runtime, which the served model is wired into), `agentlab platform`
brings it up, `agentlab serving-test` proves it. What the switch installs:

- **cert-manager** (the Giant Swarm `cert-manager-app`, images from gsoci),
  before the platform chart: the llm-d controller's webhook certificate is a
  cert-manager Certificate from a self-signed Issuer, and the cainjector puts
  the CA on its webhook configurations. Installed into `cert-manager` the way
  the observability stack is, with the Giant Swarm-only objects a kind cluster
  cannot take turned off.
- **The chart's serving components**: `kserve-llmisvc-crd` (the
  `LLMInferenceService` CRDs), `kserve-llmisvc-resources` (the llm-d
  controller), `kserve-runtime-configs` (the well-known
  `LLMInferenceServiceConfig`s the controller composes a model's pods from,
  their llm-d images from gsoci) and the `modelServing` switch — the
  connectivity chart's serving namespace `model-serving`, the published
  presets and their discovery ConfigMap, and the **models Gateway** `models`:
  an agentgateway Gateway of its own at `https://models.<domain>`, TLS on the
  lab's wildcard certificate, its JWT policy against the lab Dex (a request
  without a Dex token is answered 401 at the Gateway), which the meta chart
  names to the KServe control plane as the Gateway every model's route
  attaches to. The llm-d controller is the platform's one KServe controller:
  its release renders the control plane's shared objects (the
  `inferenceservice-config`, the webhook certificate Issuer, the default
  `ClusterStorageContainer`); the classic KServe control plane (`kserve-crd`,
  `kserve-resources`) is gone from the chart with the classic
  `InferenceService` path (giantswarm/agent-platform#574).
- **model-manager's `kserve` backend**, appended to the backends the one
  model-manager fronts (`backends: [ollama, kserve]` with a host Ollama;
  `backend: kserve` alone without a host server). It composes a published
  preset into an `LLMInferenceService` as the caller, waits for it and wires
  the served model into kagent as a `ModelConfig` whose `baseUrl` is the
  model's route on the models Gateway, the caller's token forwarded (the
  Gateway admits nothing else).
- **One preset of the lab's**, `qwen2-5-0-5b-cpu`: the node has no GPU, so
  the lab publishes a `ServingPreset` next to the chart's shipped ones — Qwen2.5
  0.5B Instruct (~1 GiB of BF16 weights, tool calling through vLLM's hermes
  parser) on the **llm-d CPU runtime**, `gsoci.azurecr.io/giantswarm/llm-d-cpu`
  at the tag the well-known template pins for `llm-d-cuda`, requesting no
  GPU. The template's image is the one exception a preset makes (the
  well-known template names the CUDA build a kind node cannot run), and the
  preset's description says so; model-manager places a preset that requests
  no GPU on the node's CPU capacity, judged against its allocatable memory.
  The shipped GPU presets stay published — the portal lists them, and a fit
  check says why none fits here.

What the lab leaves out: the GPU pool (no taint, no node selector, no
accelerator RuntimeClass), the pre-pull DaemonSet (it selects GPU nodes by
label), the cache claim (kind's local-path volume keeps a root-owned root the
KServe storage-initializer cannot write into; a preset's weights download
into its pod's own storage at every start, a 1 GiB model) and external-dns
(the Gateway's data plane is reached in-cluster through a CoreDNS rewrite of
`models.<domain>`, and by the proof through a port-forward). The images the
serving pods run are side-loaded like every platform image; the well-known
templates' own images — the 17 GB CUDA runtime among them — are templates of
pods, not pods, and stay out of the pull set.

The proof, `agentlab serving-test`, leaves nothing behind:

```
agentlab serving-test
==> The serving control plane: the llm-d controller, the well-known template, the models Gateway, the lab preset
==> Calling the model-manager API without a token                 -> 401 at the gateway
==> Logging in to Dex as admin@lab.local
==> The kserve backend through the gateway with the Dex token
==> The published presets: the lab's qwen2-5-0-5b-cpu among the shipped ones
==> The fit: qwen2-5-0-5b-cpu against the node's CPU capacity      -> fits, allocatable budget
==> Loading qwen2-5-0-5b-cpu on kserve                             -> LLMInferenceService composed on the CPU runtime
==> Waiting for qwen2-5-0-5b-cpu to serve                          -> the weights download, vLLM starts, Ready
==> The served model as model-manager reports it                   -> LLMInferenceService, routed on the models Gateway
==> The ModelConfig model-manager wired into kagent                -> OpenAI provider at the model's route, backend label
==> A completion through the models Gateway                        -> 401 without a token, 200 with the person's
==> Agent turn on the wired ModelConfig                            -> the runtime dials the route; the documented negative (below)
==> Unloading qwen2-5-0-5b-cpu                                     -> LLMInferenceService and ModelConfig gone
```

`--preset` serves another published preset (a GPU preset does not fit the
node; the fit says so and the run stops there), `--skip-chat` skips the agent
turn, `--ready-timeout` bounds the serve (default 20m: the download from the
Hugging Face Hub and the CPU runtime's start).

**The agent turn is the lab's documented negative.** The wired ModelConfig
sends an agent to the model's route on the models Gateway with the caller's
token, and the Gateway's certificate is the lab CA's. The agent runtime — the
Go ADK Harness in its Substrate sandbox — trusts the public roots of its image
and nothing else, and the platform has no knob to hand it another CA; on an
installation the Gateway's certificate is a public one and the turn goes
through. The proof drives the turn all the same and requires it to fail on
exactly that verification (`x509: certificate signed by unknown authority`)
and on nothing else, which proves the runtime dials the route the ModelConfig
names; any other outcome fails the run, an answer passes it. The completion
through the Gateway the step before is the same request with the same token
shape, sent from the host, which trusts the lab CA.
