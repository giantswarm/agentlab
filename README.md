# agentlab

A local lab for the **Giant Swarm agent platform**: muster + the Kubernetes
MCP server (and optionally Giant Swarm Backstage) running on a kind cluster,
so the whole platform can be tested and demoed end to end — Claude Code →
muster → mcp-kubernetes → apiserver, and Backstage → muster — on a laptop.

The platform needs an identity provider, so the lab bundles its own **Dex**:
users that exist nowhere but this cluster (no GitHub, no GitLab, no Google, no
Keycloak), RBAC driven by the `groups` claim, and the apiserver, muster and
Backstage all trusting the same issuer.

The whole lab is **one Go binary**. It asks for every configuration option
through an interactive form (`agentlab configure`), persists the answers to
`agentlab.yaml`, and renders every manifest from embedded templates — there is no
YAML to hand-edit and no shell to source.

## Requirements

`go` (>= 1.25), `docker`, `kind` (>= 0.31), `kubectl`, `helm` (**>= 4** — Helm 3
cannot store the umbrella chart's release any more: the dependency archives put
the release Secret over etcd's 1 MiB cap, see
[agent-platform-standalone#21](https://github.com/giantswarm/agent-platform-standalone/issues/21);
Helm 4's plugin-only post-renderer contract is handled by a generated plugin,
see below), `git`.

(The old script stack also needed openssl, curl, jq and python3; the binary
does all of that itself.)

## Quick start

Install the `agentlab` binary one of three ways:

```bash
# From source (clone this repo first):
go build -o agentlab .

# Via go install:
go install github.com/giantswarm/agentlab@latest

# From a GitHub release — assets are named agentlab-<os>-<arch>:
curl -Lo agentlab https://github.com/giantswarm/agentlab/releases/latest/download/agentlab-linux-amd64
chmod +x agentlab
```

Then bring the lab up:

```bash
export ANTHROPIC_API_KEY=sk-ant-...   # optional: powers the agents + Backstage AI chat
./agentlab configure       # interactive form: cluster, users, components
./agentlab up              # certs, kind cluster, Dex, RBAC, the agent platform — verified
./agentlab platform-test   # headless proof: Dex -> muster -> mcp-kubernetes -> apiserver
```

Then trust the lab CA once and point Claude Code at the platform (`.mcp.json`
in this repo already does the latter):

```bash
./agentlab trust             # once per machine: green locks everywhere (one sudo prompt)
export NODE_USE_SYSTEM_CA=1  # Node >= 22.15 (older Node: export NODE_EXTRA_CA_CERTS=$PWD/certs/ca.crt)
claude mcp add --transport http muster https://muster.127.0.0.1.nip.io/mcp
# in Claude Code:  /mcp  ->  authenticate  ->  Dex login page  ->  done
```

The trust step is optional — see [TLS: one lab CA, trusted
explicitly](#tls-one-lab-ca-trusted-explicitly) for what it does, how to
revert it (`agentlab untrust`), and the untrusted fallback.

`agentlab configure --defaults` skips the form and writes the canonical lab:
the **agent platform on** (it is what the lab exists to test) behind the
agentgateway edge, Backstage on, three users, Dex on 32000.
`--platform=false` gives a bare kind+Dex OIDC sandbox;
`--backstage=false` skips the portal. On a plain terminal or screen reader,
`agentlab configure --accessible` runs the form as one prompt per question.

When a **new** configuration is created (interactively or via `--defaults`),
every host-side port is probed on 127.0.0.1 first — the address all kind port
mappings bind — and any default that is already occupied is moved to a nearby
free port, with a message saying what moved where (443 falls back to 8443).
An existing `agentlab.yaml` is never renumbered: its cluster's port mappings
are already fixed, and while the lab runs its own ports would probe as
occupied.

To exercise the identity itself:

```bash
./agentlab test                    # asserts RBAC for every configured user
./agentlab login dev@lab.local     # headless, instant
./agentlab browser                 # real Dex login page in the browser
export KUBECONFIG=$PWD/kubeconfig.oidc
kubectl auth whoami
```

Tear down with `./agentlab down`.

## TLS: one lab CA, trusted explicitly

Everything the lab serves over TLS — the agentgateway edge
(`*.127.0.0.1.nip.io`) and the bundled Dex (`https://localhost:32000/dex`) —
chains to a **lab CA** that `agentlab up` mints per machine into `certs/`
(key 0600, gitignored, never leaves the machine). Every in-cluster consumer
trusts it automatically (muster's trust pool, Backstage, the apiserver's OIDC
flags). Your browser and your Node don't, until you say so:

```bash
./agentlab trust      # one sudo prompt; ./agentlab untrust reverts it
```

`trust` installs `certs/ca.crt` into the system trust store (Linux
`update-ca-certificates`/`update-ca-trust`, the macOS system keychain, the
Windows root store — the same mechanism as mkcert, via smallstep/truststore)
and, when the NSS `certutil` tool is installed, into the Firefox/Chromium NSS
profiles. Every lab URL then gets a green lock, the Dex login included.
Trust changes are always explicit: `up` only *probes* and points here, `down`
never touches a trust store, and `untrust` removes exactly the lab CA.

The CA you are trusting is deliberately narrow:

- **X.509 name constraints** pin it to the lab's own names (`platform.domain`,
  `localhost`, the Dex in-cluster names) and to `127.0.0.0/8`, so a leaked CA
  key cannot sign for the web at large. (Non-critical for old-verifier
  compatibility; Go, OpenSSL, Chrome, Firefox and macOS enforce them anyway.)
- **Leafs live 825 days** — Apple's cap: macOS rejects longer-lived TLS server
  certs *even under a user-trusted root* — and re-mint automatically from the
  unchanged CA, so a leaf rotation never repeats the trust step.
- Changing `platform.domain` **re-mints the CA** (the constraints pin the
  domain). `agentlab up`/`certs` say so loudly: run `agentlab trust` again —
  it also sweeps the replaced CA out of the stores — and recreate a running
  cluster (`agentlab down && agentlab up`), whose apiserver pinned the old CA
  at boot.

Skipping the trust step keeps the old behavior: browser warnings once per
hostname, and `export NODE_EXTRA_CA_CERTS=$PWD/certs/ca.crt` for Node
clients. The headless `*-test` commands trust `certs/ca.crt` directly and
never need any of this.

### Node and Claude Code

With the CA in the system store, Node **>= 22.15** picks it up with one env
var — the per-shell `NODE_EXTRA_CA_CERTS` export is gone:

```bash
export NODE_USE_SYSTEM_CA=1
claude mcp add --transport http muster https://muster.127.0.0.1.nip.io/mcp
```

Older Node keeps needing `NODE_EXTRA_CA_CERTS` (it ignores system stores
entirely); the `agentlab up` output prints the right line for the Node it
detects.

### Known gaps

- **Firefox without `certutil`**: Firefox reads its own NSS database, not the
  system store. `agentlab trust` covers it only when NSS tools are installed
  (`apt install libnss3-tools`, `dnf install nss-tools`, `pacman -S nss`,
  `brew install nss` — then re-run `agentlab trust`). Alternative: set
  `security.enterprise_roots.enabled` to `true` in `about:config`, which
  makes Firefox honor the system store.
- **WSL2**: the browser lives on the Windows side, which has its own trust
  store. `agentlab trust` inside WSL covers curl/Node/Claude Code there;
  import `certs/ca.crt` on the Windows side manually (an admin
  `certutil.exe -addstore root ca.crt`, or certmgr.msc) for the browser.

### Bring your own certificate

If you own a domain you can skip lab-CA trust for the edge entirely: point a
wildcard record (`*.lab.example.com` → 127.0.0.1) at loopback, mint a real
wildcard cert with whatever ACME tooling you already run (certbot, lego,
step, cert-manager — DNS-01, since a laptop lab is not publicly reachable),
and hand the pair to the lab:

```yaml
platform:
  domain: lab.example.com
  tls:
    certFile: /path/to/fullchain.pem
    keyFile: /path/to/privkey.pem
```

The edge then serves your certificate instead of a minted wildcard (renewals:
re-run `agentlab platform` after the files change). Caveat: the Dex login
page still serves the lab-CA cert — the issuer cannot move under your domain
yet ([#20](https://github.com/giantswarm/agentlab/issues/20)) — so
the login hop keeps warning until you `agentlab trust`.

## The agent platform (muster + Kubernetes MCP)

The lab's centerpiece: Giant Swarm's
[agent-platform-standalone](https://github.com/giantswarm/agent-platform-standalone)
umbrella chart, wired to the lab Dex, so Claude Code can drive a Kubernetes
MCP server living inside the kind cluster. It is one plain Helm chart —
muster, valkey and the MCP registrations are pinned subcharts (`Chart.lock`
is the BOM) — so unlike the old agent-platform meta-package there is **no
Flux** and no HelmRelease indirection anywhere in this lab.

The chart has no release yet
([PR #11](https://github.com/giantswarm/agent-platform-standalone/pull/11)), so
`agentlab platform` vendors it from git at a pinned SHA (`platform.apsRef` in
`agentlab.yaml`) into `.vendor/` (not `vendor/` — that would flip the Go
toolchain into vendored-build mode) and installs from the local path. Once released,
that step becomes a plain `helm install oci://…/agent-platform-standalone`.

The platform installs as part of `agentlab up` (it is enabled in the default
configuration); on an already-running cluster the steps are also standalone:

```bash
./agentlab platform       # vendors the chart + muster + valkey + mcp-kubernetes
./agentlab platform-test  # headless proof of the whole chain
```

muster runs with `hostNetwork`, so it binds `:8090` on the node, and the
rendered kind config publishes that onto the Mac (host port `platform.musterPort`,
default 8090) — no port-forward. Port mappings are fixed at node-creation time,
so changing the port means `agentlab down && agentlab up`; the stopgap on an old
cluster is `kubectl -n agent-platform port-forward svc/muster 8090:8090`.

### The request path

```
Claude Code ──https://muster.127.0.0.1.nip.io/mcp──> agentgateway ──> muster ──> mcp-kubernetes ──> kube-apiserver
                     │                                 (edge, TLS)     │
                     │ 401 + WWW-Authenticate                          │ OIDC discovery + token exchange
                     └──── browser ────────────────────────────────────┴──> https://localhost:32000/dex
```

muster is the OAuth **server** towards Claude Code (DCR, `/oauth/authorize`,
`/oauth/token`) and an OAuth **client** of Dex. The `muster` staticClient in
the rendered Dex config closes that loop; its `redirectURIs` must equal
`<oauth.server.baseUrl>/oauth/callback`.

`mcp-kubernetes` is deliberately unauthenticated on the cluster network
(the bundled `MCPServer` CR carries no auth block) and talks to the apiserver
with its own ServiceAccount. muster is the single enforcement point.

### Agents (kagent)

The platform's agent runtime is an **optional component**, on by default
(`platform.agents` in `agentlab.yaml`; headlessly:
`agentlab configure --defaults --agents=false`). On real clusters agent
delivery runs through Flux/GitOps, which this lab does not run as a GitOps
loop — skip the runtime when agents are not what you are testing. (Backstage's
agent create flow does need Flux's source+helm controllers as its delivery
engine; the lab installs exactly those two when Backstage and agents are both
enabled — see [The agent create flow](#the-agent-create-flow).) When enabled, the
umbrella's **kagent** component installs and `Agent` CRs run against a
**default `ModelConfig`** that the kagent chart renders from the lab's
`aiModel` setting (`agentlab.yaml`, default `claude-sonnet-4-6` — the BOM's
own default).

The ModelConfig references the Secret `kagent/kagent-anthropic`. The API key
is a **real credential**, so unlike the lab's throwaway passwords it never
enters `agentlab.yaml` or the rendered `state/` files: `agentlab platform`
creates the Secret from `$ANTHROPIC_API_KEY` on the host (created once, left
alone; delete it and re-run to rotate). Without the env var the install still
succeeds — the ModelConfig then points at a Secret that does not exist yet,
which is harmless until an `Agent` CR is created: that agent's pod sits in
`CreateContainerConfigError` (missing Secret) and recovers on its own once the
Secret lands. To supply the key later, either export it and re-run
`agentlab platform` (idempotent — it only fills the gap), or create the Secret
directly:

```bash
kubectl -n kagent create secret generic kagent-anthropic \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-...
```

The same key powers Backstage's AI chat via a second Secret,
`backstage/backstage-anthropic` (see
[Backstage gotchas](#backstage-gotchas) for its no-key behavior).

The kagent UI is host-published like the other components:
`http://localhost:8081` (`platform.agentsPort` in `agentlab.yaml`). The UI does
no OAuth in this lab, so it needs none of the issuer tricks — the `kagent-ui`
Service is simply `type: NodePort`, pinned to node port 30880 by
`agentlab post-render` (the chart's Service template renders no `nodePort`
field — HACKS.md U9), and the kind config maps that onto the host. Like every
kind port mapping it is fixed at node-creation time, so a cluster created
before this mapping existed needs `agentlab down && agentlab up`; the stopgap
there is the old port-forward:

```bash
kubectl -n kagent port-forward svc/kagent-ui 8081:8080
```

Lab deviations on the kagent side, same spirit as the table below: the
controller runs `auth.mode: unsecure` (upstream's local-dev mode — the GS
default `trusted-proxy` decodes bearer claims *without verification* and
depends on a JWT-validating agentgateway this lab does not run), and the
ServiceMonitor / OTel exporters are off (no Prometheus Operator, no OTLP
gateway in kind). The umbrella also renders the shared `RemoteMCPServer`
pointing agents at muster; note that kagent forwards the *caller's* token to
muster, so agent tool calls through muster need a real Dex token on the way
in — headless pokes at the unsecured controller API won't have one.

### Extra model configs (self-hosted, OpenRouter, Gemini, OpenAI)

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

### Observability (Prometheus + mcp-prometheus)

An **optional component, on by default** (`platform.observability` in
`agentlab.yaml`; skip it headlessly with `agentlab configure --defaults
--observability=false`): a minimal Prometheus scraping the cluster plus
[mcp-prometheus](https://github.com/giantswarm/mcp-prometheus), registered in
muster as `x_mcp-prometheus_<tool>` — so Claude Code (or a kagent agent) can
answer "how is the control plane's CPU?" or "how much memory does pod X use?"
with real PromQL (`x_mcp-prometheus_execute_query`, `…_execute_range_query`,
label/metadata discovery, and the rest of its 18 read-only tools).

The Prometheus is the Giant Swarm
[kube-prometheus-stack](https://github.com/giantswarm/kube-prometheus-stack-app)
chart — the **observability bundle's own pinned constituent** (same version,
gsoci images, curated relabelings). The bundle itself cannot run here: since
v3 it renders Flux HelmReleases wired for the MC→WC model (a hardcoded remote
kubeconfig Secret, `dependsOn` on HelmReleases this lab lacks, Alloy configs
generated by observability-operator against Mimir) and it deliberately ships
**no local PromQL endpoint** — collection goes Alloy → remote-write → Mimir on
the management cluster. The lab installs the constituent directly with plain
helm and re-enables the Prometheus server and node-exporter: prometheus-operator,
kube-state-metrics, node-exporter and one emptyDir Prometheus (~5 pods,
~0.5 GB) scraping the apiserver, kubelet/cAdvisor (pod CPU/memory) and the
node (control-plane CPU/memory). Values and the reasons for every toggle:
`internal/lab/templates/kube-prometheus-stack-values.yaml.tmpl`; chart
versions are Go-const pins in `internal/lab/observability.go` (like Flux's —
no BOM covers them).

mcp-prometheus follows the mcp-kubernetes stance exactly: unauthenticated on
the cluster network, muster is the single enforcement point. Its MCPServer CR
comes from the umbrella's `agent-platform-mcps.mcpServers` values — with a
`group` deliberately **outside** the chart's `muster.families` (`kubernetes`,
`prometheus`): a `prometheus`-family entry would surface the tools as
`x_prometheus_<tool>` with a required `management_cluster` argument, which is
the multi-cluster UX, not this single-cluster lab's. What the lab exercises is
the mcp-prometheus tool chain against a plain local Prometheus; the GS
production shape (Alloy → Mimir, `X-Scope-OrgID` tenancy) is out of scope.

The platform's own monitors ride along: with observability on, the umbrella's
muster ServiceMonitor + PrometheusRule, the kagent ServiceMonitor, the valkey
PodMonitor and mcp-prometheus's own ServiceMonitor are all enabled, and the
lab Prometheus scrapes them (its monitor/rule selectors are opened with
`*NilUsesHelmValues: false` — upstream's default would only select monitors
carrying the kps release label).

**Backstage's own metrics views ride along too.** The Clusters and
Deployments pages query Mimir through gs-backend's `MimirService`, hardcoded
to `https://observability.<baseDomain>/prometheus/api/v1/query`; the umbrella
sets `mimirEnabled: false` because standalone installations have no such
endpoint. With observability on, the lab provides exactly that endpoint — an
HTTPRoute on the edge (`observability.<domain>`, `/prometheus` prefix-strip →
the lab Prometheus, whose query API is what Mimir's is compatible with) — and
overrides `mimirEnabled: true` in its app-config overlay. The gs frontend
attributes samples without Mimir's `cluster_id` label to the installation
itself, which is exactly right for a single-cluster lab, so the
Deployments/Clusters metrics show real numbers. Note the endpoint is
unauthenticated read-only PromQL on the (localhost-only) lab edge: a real MC
fronts it with an auth gateway validating the Bearer token, plain Prometheus
ignores it — wider than muster's OAuth, accepted for the lab.

`agentlab platform-test` grows a phase when the component is on: it lists the
`x_mcp-prometheus_*` tools through muster, runs `execute_query` with `up`,
asserts the platform itself is being scraped (muster, valkey,
mcp-prometheus, and kagent when agents run all report `up == 1`), and then
runs the Deployments page's exact workload query against the edge
observability endpoint — proving Dex → muster → mcp-prometheus → Prometheus
and the Backstage metrics path end to end. Logs:
`agentlab logs prometheus`, `agentlab logs mcp-prometheus`.

### Why `localhost` and not `127.0.0.1`

The issuer was originally `https://127.0.0.1:32000/dex`. muster refuses that:
`mcp-oauth`'s `ValidateIssuerURL` rejects any issuer whose host **parses as a
loopback or private IP**, unconditionally — `allowPrivateIPOIDC` only relaxes the
*dial-time* SSRF guard, not this static check. A hostname that merely *resolves*
to 127.0.0.1 passes, because `net.ParseIP("localhost")` returns nil.

So the lab issuer is `https://localhost:32000/dex`. Nothing else changed: the
Dex cert already carried `DNS:localhost` in its SAN, and `localhost` resolves to
127.0.0.1 on the Mac, inside the kind node, and inside any `hostNetwork` pod — the
same one-URL trick, just spelled with a name.

### Lab-specific deviations from a real management cluster

| What | Why |
|---|---|
| `gatewayApi.gateway.create: true` — the chart-owned agentgateway Gateway **is** the public edge | A real MC fronts the platform with the cluster's shared Envoy Gateway; kind has none, so the data-plane Gateway itself terminates TLS for `*.127.0.0.1.nip.io` with the lab's wildcard cert (the chart's own standalone/kind mode, `ingress.mode: agentgateway-muster`). The Gateway API CRDs are embedded in the binary and applied before the install; a lab-owned NodePort Service pins the edge onto the kind port mapping (HACKS.md U10), and a CoreDNS rewrite points `*.127.0.0.1.nip.io` at it inside pods (outside, nip.io answers 127.0.0.1 by itself). |
| One Dex client, `agent-platform` | The chart's `global.identity` convention: muster and Backstage share the client, so a Backstage-forwarded token natively carries an audience muster trusts. The extra `dex-k8s-authenticator` client exists only as the cross-client audience target Backstage's GS auth provider requests by default. |
| `networkPolicy.enabled: false`, `kyvernoPolicies.enabled: false` | The umbrella's own policy objects. No Cilium and no Kyverno in kind, so both would render CRs whose API groups the cluster does not serve. |
| The muster/kagent ServiceMonitors, valkey PodMonitor and muster PrometheusRule follow `platform.observability` | Without it there is no Prometheus Operator, so none of those CRDs exist and the releases fail to render. With it they are scraped by the lab Prometheus — whose selectors are opened up (`*NilUsesHelmValues: false`) because upstream's default selects only monitors carrying the kps release label, and the platform's monitors come from other releases. Flipping observability rolls the muster pod once (the toggle changes its metrics-exporter env). |
| `platform.observability`: the GS kube-prometheus-stack constituent installed directly, Prometheus server re-enabled, instead of the observability-bundle | The bundle is MC-shaped (Flux HelmReleases with a hardcoded remote kubeconfig, Alloy → Mimir, no local PromQL endpoint). See [Observability](#observability-prometheus--mcp-prometheus). |
| `muster.rbac.{mcpServerEditor,workflowEditor}.subjects` → `oidc:platform-admins` | The umbrella binds muster's editor Roles to Giant Swarm's admin groups, which do not exist here. Rebound to the lab's own admin group (`--oidc-groups-prefix=oidc:`, same spelling as the lab RBAC). Lists replace, so the GS groups are dropped. |
| muster patched to `hostNetwork` + `maxSurge: 0` | Same issuer trick as the apiserver and Backstage. `maxSurge: 0` because two hostNetwork pods cannot both bind `:8090` on a one-node cluster. Applied by `agentlab post-render` — Helm 4 accepts only plugin-type post-renderers, so the install generates a `postrenderer/v1` plugin in `state/helm-plugins/` whose command is the agentlab binary itself, and passes it via `HELM_PLUGINS` + `--post-renderer agentlab-postrender`. The plain-Helm replacement for the Flux `postRenderers` the meta-package forwarded to helm-controller. |
| `components.kagent.enabled` from `platform.agents`, `controller.auth.mode: unsecure`, kagent ServiceMonitor + OTel off | Agents are part of what the lab tests, so kagent is on by default (the umbrella defaults it off) but optional — `platform.agents: false` skips the runtime. `unsecure` because the GS `trusted-proxy` mode assumes a JWT-validating agentgateway in front; no Prometheus Operator / OTLP gateway in kind. See [Agents (kagent)](#agents-kagent). |
| `kagent.ui.service.type: NodePort`, nodePort 30880 pinned by `agentlab post-render` | On a real MC the UI sits behind the agentgateway edge; this lab publishes it through the kind port mapping instead (host side `platform.agentsPort`, default 8081). The chart's Service template renders no `nodePort` field, so the fixed node port is a post-render patch (HACKS.md U9). |
| The chart vendored at a pinned git SHA | Component versions are the chart's own tested BOM (`Chart.lock`); the lab no longer pins its own. The only lab-side pin is `platform.apsRef` — the chart repo commit — so two runs still install the same thing. |

### Platform gotchas

- **Switching from the old meta-package needs a clean slate.** Both installs use
  the Helm release name `agent-platform`, but the old one rendered Flux
  HelmReleases and the new one renders the workloads directly — upgrading across
  that boundary races helm-controller uninstalls against the fresh install.
  `agentlab platform` refuses if it finds the old HelmReleases; run
  `agentlab platform-down` first (an old cluster keeps its now-idle `flux-system`,
  which is harmless).
- **`allowPublicClientRegistration` must be on for Claude Code's login.**
  Claude Code registers over DCR as a public client on a random loopback port,
  so none of the other registration gates can be opened for it: it cannot send
  a registration token, `trustedPublicRegistrationRedirectURIs` cannot match a
  random port, and `http`/`https` are deliberately stripped from
  `trustedPublicRegistrationSchemes` by mcp-oauth's config validation. The
  muster chart renders the key since 5.7.2 (muster#1118); before that,
  `agentlab post-render` had to edit it into the rendered ConfigMap
  (HACKS.md U1, now fixed upstream).
- **A `Recreate` strategy cannot be patched onto an existing Deployment.** The API
  server has already defaulted `spec.strategy.rollingUpdate`, and a patch that
  flips the type without also deleting that field fails with
  `rollingUpdate: Forbidden`. `maxSurge: 0` achieves the same thing without the
  conflict.
- **A disabled kagent still creates its namespace.** The umbrella's
  `templates/namespace.yaml` is gated only on `kagent.kagent.namespaceOverride`,
  not on `components.kagent.enabled`, so an empty `kagent` namespace appears.
  Harmless — Helm owns it and removes it on uninstall.
- **Kubernetes tools carry the server-name prefix.** The umbrella's bundled
  `mcp-kubernetes` MCPServer declares no muster *family*, so its tools use
  per-server prefixing: `call_tool(name=x_mcp-kubernetes_list, arguments={...})`.
  (Real fleet installations register per-cluster servers with a `kubernetes`
  family and a `management_cluster` instance argument instead.)
- **Tool results are double-wrapped.** `result.content[0].text` is JSON whose
  `content[0].text` is the actual payload — two decode hops.

## Backstage

Backstage deploys **with the platform** — the umbrella chart's `backstage`
component, on by default (`backstage.enabled` in `agentlab.yaml`), published
through the agentgateway edge. It is **Giant Swarm's own Backstage** — the
build behind [devportal.giantswarm.io](https://devportal.giantswarm.io/) —
with Dex as its **only** identity provider. Not upstream Backstage and not
RHDH: the GS build is the one that carries the first-party `muster` plugin,
which is the whole reason to run a portal in this lab at all.

```bash
./agentlab up            # the whole stack, Backstage included
open https://backstage.127.0.0.1.nip.io
```

The image is published **anonymously** to `gsoci.azurecr.io/giantswarm/backstage`
— no Giant Swarm registry credentials needed. Since 0.200.25 it is multi-arch
(`linux/amd64` + `linux/arm64`), so it runs natively on Apple Silicon; tags
before that are amd64-only and fail to pull on an arm64 host.

Sign In takes you to the same Dex login page, and you come back as a real
Backstage identity with the groups from the token.

The edge and Dex serve certs signed by the lab CA; after a one-time
`agentlab trust` the whole flow — Backstage, the Dex login redirect and back —
is green locks (see [TLS: one lab CA, trusted
explicitly](#tls-one-lab-ca-trusted-explicitly)). Without it, the first click
lands on a browser TLS warning once per hostname (Backstage itself always
trusts the CA server-side, via the mounted `dex-ca` Secret).
`agentlab backstage-test` sidesteps all of this by trusting `certs/ca.crt`
directly.

What the lab adds on top of the chart's own app-config
(`agent-platform-backstage-app-config`):

- **`hostNetwork: true` on the Backstage pod** (`agentlab post-render`, same
  patch as muster). The issuer is `https://localhost:32000/dex`, and from
  inside a normal pod that is the pod's own loopback; on the host network it
  is the node's, which is the Dex NodePort — the same URL the browser uses.
  `dnsPolicy: ClusterFirstWithHostNet` keeps cluster DNS, so the CoreDNS
  rewrite still routes `https://muster.127.0.0.1.nip.io/mcp` to the edge.
- **The lab catalog overlay** (`agentlab-backstage-app-config` +
  `agentlab-backstage-catalog`): the users/groups entities, the
  `agent-deployment` scaffolder Template behind the agent create flow, and an
  in-memory sqlite database — no Postgres needed for a lab portal.
- **The shared `agent-platform` Dex client** carries Backstage's callback
  (`/api/auth/oidc-agent-platform/handler/frame` — the chart's provider name),
  and the `kubernetes` client trusts it as a peer so the Kubernetes plugin can
  mint apiserver-audience tokens (`components.backstage.extraScopes`).

### Users need no catalog entity

RHDH's `emailLocalPartMatchingUserEntityName` resolver refuses a login it cannot
map onto a `User` entity. Giant Swarm's resolver only consults the catalog when
Dex reports `federated_claims.connector_id` of `giantswarm-ad` or
`giantswarm-github`. This lab's static-password connector reports `local`, so it
falls straight through to `email.split('@')[0]` and issues
`user:default/<localpart>` regardless. The catalog entities the lab renders
(from your configured users) exist only so the users and groups show up as real
things in the UI.

### The muster plugin

Lives under **Agent Platform → MCP Servers**, with tabs for the dashboard,
MCP servers, workflows and a tool explorer.

```
browser ──/api/muster/*──> Backstage backend ──MCP streamable-http──> muster :8090
   │                              │
   │ backstage-muster-            │ Authorization: Bearer <the same token>
   └─ authorization: <id_token>   └──> mcp-kubernetes ──> apiserver
```

The browser forwards the signed-in user's Dex id_token in a
`backstage-muster-authorization` header; the backend promotes it to
`Authorization: Bearer` on the MCP session. The browser never speaks MCP itself.
muster accepts the token because its `aud` carries `muster` — see
[`trustedPeers` points the other way round](#trustedpeers-points-the-other-way-round).

`agentlab backstage-test` drives the whole sign-in headlessly for every
configured user and then proves the muster hop with that user's own token:

```
=== dev@lab.local ===
  dex asserted    groups=[developers] email=dev@lab.local
  token audience  [kubernetes muster backstage]
  backstage user  user:default/dev
  ownership refs  [user:default/dev]
  muster servers  [(mcp-kubernetes, Connected)]
  muster workflows [lab-cluster-overview]
  muster core tools 28 exposed
  agent deploy template registered (template:default/agent-deployment)
```

### The agent create flow

**Agent Platform → Agents → New agent** (`/agents/new`) composes an agent from
a form (installation, name, model, system prompt) and its Deploy button applies
the result **directly to the cluster**: the frontend calls the scaffolder with
the hidden catalog template `template:default/agent-deployment`, which runs the
`kube:apply` action with the *user's* per-installation OIDC token on a composed
`OCIRepository` + `HelmRelease` (the `agent` chart from gsoci, values inlined).
No pull request, no GitOps repo — but the applied resources are **Flux CRs**,
so something on the cluster has to turn them into an installed chart.

The lab supplies both halves:

- **The template.** Real installations load the Template entity from
  [giantswarm/backstage-catalogs](https://github.com/giantswarm/backstage-catalogs/tree/main/templates/agent-deployment);
  the lab embeds a verbatim copy
  (`internal/lab/templates/static/agent-deployment-template.yaml`) into the
  `backstage-catalog` ConfigMap and registers it as a file location, so the
  catalog needs no network. Without it every deploy dies with
  `404 Template template:default/agent-deployment not found` (HACKS.md U7).
- **The delivery engine.** `agentlab backstage` installs the fluxcd-community
  `flux2` chart with **only source-controller and helm-controller** (release
  `flux` in `flux-system`, values in `state/flux-values.yaml`) — enough to
  reconcile exactly the two kinds the flow applies, still no GitOps loop.
  Skipped when the platform or agents are disabled: with no kagent there is no
  `ModelConfig` to build an agent on and the flow is unusable anyway.

Everything lands in the selected ModelConfig's namespace (`kagent`): one shared
`OCIRepository/agent` tracking `semver: x.x.x`, one `HelmRelease` per agent
named after its slug. The lab omits `agentPlatform.fluxServiceAccountName`
(composed HelmReleases then carry no `spec.serviceAccountName`), so
helm-controller applies with its own — there is no Flux multi-tenancy admission
policy here. RBAC still applies to the *apply* step itself: it runs with the
signed-in user's token, so `platform-admins` can deploy agents and `developers`
(edit only in `demo`) cannot — which is the platform behavior, not a lab bug.

One more platform gap stands between "HelmRelease installed" and a running
agent: upstream does not currently publish the `golang-adk` runtime image at
kagent's own tag, so the agent pod would ImagePullBackOff. `agentlab up` heals
this automatically by standing in the newest published release (HACKS.md U8).

### Backstage gotchas

- **`app.extensions` replaces, it does not merge.** The image ships a list of
  ~17 enabled extensions, `page:agent-platform` among them. Setting
  `app.extensions` in the lab config to disable one entry silently discards the
  whole shipped list — and with it the Agent Platform page the muster plugin
  attaches to, so the plugin vanishes with no error anywhere. The lab therefore
  sets no `app.extensions` at all. To disable anything you must restate every
  entry you still want, and re-check the list on each image bump.
- **`gs.installations` is required once `gs:` exists.** The `gs` block as a
  whole is optional, but `installations` inside it is not: omit it and config
  *schema* validation kills startup with
  `Config must have required property 'installations' at /gs`. One dummy entry
  is enough.
- **`gs.authProvider` is effectively mandatory.** Without it the backend boots
  and the page renders, but the sidebar's cluster-access element calls
  `useApi(gsAuthApiRef)` unconditionally and throws a runaway React loop. There
  is no catalog-only / no-auth mode.
- **Dex must be up before Backstage starts.** `waitForIssuerMetadata` retries
  five times with backoff and then fails startup on purpose, so the pod
  crash-loops until Dex answers. Same shape as the apiserver's OIDC discovery
  trap, except this one also swallows the static frontend: every route returns
  `503 Service has not started up yet`, which reads like a crash rather than a
  dependency problem.
- **`ai-chat` needs `$ANTHROPIC_API_KEY` at deploy time.** The image's base
  app-config already reads the key from that env var, and the lab overlay sets
  `aiChat.model` to the lab's `aiModel` (claude-\* routes to Anthropic);
  `agentlab backstage` creates the `backstage-anthropic` Secret from the host
  env and injects the env var (as `optional:`, so the pod boots without it).
  When the key is absent, ai-chat is simply unconfigured — and its
  assistant-ui runtime then logs five `Maximum update depth exceeded` errors
  on the signed-out page before React bails out. Cosmetic (the page renders,
  login works), and disabling the plugin would mean restating the whole
  extensions list per the first gotcha; the lab accepts the noise instead.
- **A muster with no workflows looks like a broken plugin.** The workflow list
  is the plugin's main surface, and a fresh muster has none, so the tab renders
  empty. `agentlab platform` seeds one (`lab-cluster-overview`) that lists
  namespaces and pods through `mcp-kubernetes`, exercising the whole chain from
  one click.
- **`allowMutations` no longer exists.** `app-config.example.yaml` still
  documents `muster.installations[].allowMutations` as a read-only safety gate;
  it was removed and nothing reads it. The real guard is the downstream MCP
  server's RBAC — and `mcp-kubernetes` here uses the chart's `standard` profile,
  which **can write**. Drop it to `readonly` in the mcp-kubernetes values
  template for a read-only demo.

## The users

The default configuration ships three users (password: `password`):

| User | Groups in the token | Effective access |
|---|---|---|
| `admin@lab.local`  | `platform-admins`, `developers` | `cluster-admin` |
| `dev@lab.local`    | `developers` | `edit` inside `ns/demo` only |
| `viewer@lab.local` | `viewers` | `view` cluster-wide |

Users are fully configurable: `agentlab configure` lets you keep, edit, remove
and add users, with per-user passwords and group membership. The **groups are
a fixed vocabulary** — RBAC binds exactly `platform-admins` → cluster-admin,
`developers` → edit-in-demo, `viewers` → view — and the form only offers those
three. Passwords are bcrypt-hashed automatically; the hash is cached in
`agentlab.yaml` so renders stay deterministic (no spurious Dex pod rolls). After
editing users, run `agentlab reload`.

## How the identity plumbing works

```
                    issuer: https://localhost:32000/dex
                                    |
  Mac: localhost:32000 ---> kind port mapping ---.
                                                 +--> NodePort 32000 --> Dex :5556
  apiserver static pod (hostNetwork) ------------'
       localhost:32000
```

The single trick that makes this work is the **shared issuer URL**. The
apiserver has to validate tokens against the same URL the browser was
redirected to. Because the apiserver static pod runs with `hostNetwork`, its
`localhost:32000` lands on the node's NodePort — and kind maps that same port
onto the Mac. One URL, valid from both sides. (32000 is the default; the Dex
port is a form question, constrained to the NodePort range 30000-32767.)

Everything else follows from that:

- `agentlab up` mints the name-constrained lab CA and a Dex server cert
  (crypto/x509) whose SAN carries both `IP:127.0.0.1` and `DNS:localhost`. The
  issuer uses the **name**, not the IP — see
  [Why `localhost` and not `127.0.0.1`](#why-localhost-and-not-127001).
- The rendered kind config bind-mounts `certs/` into `/etc/kubernetes/pki/dex`
  on the node. kubeadm already mounts `/etc/kubernetes/pki` into the apiserver
  pod, so a subdirectory of it is the one place the CA is visible without extra
  plumbing.
- The apiserver gets `--oidc-issuer-url`, `--oidc-client-id`, `--oidc-ca-file`,
  `--oidc-username-claim=email`, `--oidc-groups-claim=groups` and `oidc:` prefixes.
- RBAC binds `Group: oidc:platform-admins` (etc.) to ClusterRoles.

Every manifest the binary applies is also written to `state/` (gitignored), so
`kubectl diff -f state/dex.yaml` and plain reading remain possible. The
templates live in `internal/lab/templates/`.

## Why Dex v2.45.1 specifically

`groups` on `staticPasswords` **landed in Dex v2.45.0** (Feb 2026,
[PR #4456](https://github.com/dexidp/dex/pull/4456), closing
[issue #1080](https://github.com/dexidp/dex/issues/1080) after eight years).
Same release added `name`, `preferredUsername` and configurable `emailVerified`.

On **v2.44.0 and earlier**, `staticPasswords` returns only
`UserID`/`Username`/`Email` with `EmailVerified` hardcoded to `true` — no
groups, which is the historical reason people bolted LDAP or Keycloak onto Dex
just to get a lab going. That is no longer necessary.

Watch out:
- The upstream Helm chart `dexidp/dex` 0.24.1 still defaults to appVersion
  **2.44.0**. You must override `image.tag`.
- `giantswarm/dex-app` v2.2.3 is based on Dex **v2.43.2**, and its template
  hardcodes `enablePasswordDB: false` with no `staticPasswords` support at all.
  This lab therefore uses plain manifests, not `dex-app`.

The Dex image is a form question (`dexImage` in `agentlab.yaml`) for when the
next version lands.

## Gotchas that cost time

- **A client certificate beats a bearer token.** `kubectl --token=...` against
  the kind kubeconfig silently keeps authenticating as `kubernetes-admin`. You
  need a kubeconfig with no client cert — that is what `agentlab login` builds
  (`kubeconfig.oidc`). This will produce convincing false positives in a test
  suite if you miss it.
- **kind switches kubeadm config generations between releases** — v0.31 emits
  `kubeadm.k8s.io/v1beta3` (`extraArgs` is a *map*), v0.32+ emits v1beta4
  (`extraArgs` is a *list* of name/value pairs) — and a kubeadmConfigPatch
  whose apiVersion does not match is ignored **silently**: no error, the
  OIDC flags just never appear and every token is rejected. The rendered kind
  config therefore carries the patch in BOTH flavors; whichever matches
  applies, the other is a no-op. If the flags ever vanish after a kind bump
  (a v1beta5 one day), verify with:
  ```bash
  docker exec agentlab-control-plane grep oidc /etc/kubernetes/manifests/kube-apiserver.yaml
  ```
- **The apiserver keeps retrying OIDC discovery — no bounce needed.** On a
  cold `kind create`, Dex does not exist yet and the apiserver logs
  `oidc authenticator: initializing plugin: … connection refused` — but on
  Kubernetes 1.35 it retries every 10 seconds forever and initializes on the
  first tick after Dex answers (verified empirically; earlier versions of this
  lab bounced the static pod because older apiservers gave up for good).
  `agentlab up`'s verification loop simply waits out the next retry tick. If
  tokens are still rejected minutes after Dex is up, read the apiserver log
  for those `oidc.go` lines rather than restarting things.
- **Scopes are not optional.** `--oidc-username-claim=email` needs the client to
  request the `email` scope and `--oidc-groups-claim=groups` needs `groups`.
  Ask for `openid` alone and the apiserver rejects the token with
  `parse username claims "email": claim not present`, which reads like a
  misconfiguration on the apiserver side but is really a missing scope.
- **Dex needs a writable `/tmp`** even with `readOnlyRootFilesystem: true`; it
  renders its config through a temp file. Hence the `emptyDir`.
- **Dex storage must not be `memory` in this lab.** A config edit rolls the
  Dex pod by design (`agentlab reload`), and with in-memory storage every roll
  mints new signing keys — the apiserver then rejects **all** tokens with
  `failed to verify id token signature` until its JWKS cache refreshes,
  minutes after the very edit that prompted the reload. The lab uses Dex's
  CRD-backed `kubernetes` storage instead: keys persist across rolls, tokens
  keep verifying, and the state still dies with the cluster.
- Kubernetes 1.35 still accepts the `--oidc-*` flags. The modern alternative is
  `--authentication-config` (structured `AuthenticationConfiguration`, which
  also supports CEL claim mappings). The flags are simpler and were kept here.

## Wiring another app to this Dex

The rendered Dex config carries the shared `agent-platform` static client:

```
issuer:        https://localhost:32000/dex
client id:     agent-platform
client secret: agent-platform-lab-secret
redirect URIs: https://muster.127.0.0.1.nip.io/oauth/callback
               https://backstage.127.0.0.1.nip.io/api/auth/oidc-agent-platform/handler/frame
```

The Backstage redirect path carries the **provider name** from the app-config,
not the literal word `oidc` — Backstage serves each provider at
`/api/auth/<provider>/handler/frame`, and the umbrella names it
`oidc-agent-platform`. More clients means editing the Dex template
(`internal/lab/templates/dex.yaml.tmpl`), rebuilding and `agentlab reload`.

### `trustedPeers` points the other way round

To let client A mint a token whose audience client B accepts, A requests the
scope `audience:server:client_id:B` — and **B** must list **A** in its
`trustedPeers`. It is a grant published by the audience, not a capability
claimed by the caller.

So "Backstage may act on the Kubernetes API" is spelled:

```yaml
- id: kubernetes
  trustedPeers:
    - backstage      # <- the *caller* is listed on the *audience's* client
```

Getting this backwards fails closed and loudly, which is the one mercy:

```
$ curl -u backstage:... -d 'scope=...audience:server:client_id:kubernetes' .../token
{"error":"invalid_request",
 "error_description":"Client can't request scope(s) [\"audience:server:client_id:kubernetes\"]"}
```

The lab uses one token for three audiences. `gs.auth.extraScopes` in the
Backstage template asks for both cross-client scopes, so a single Dex id_token
comes back with `aud: ["kubernetes", "muster", "backstage"]` and is accepted by
the apiserver (`--oidc-client-id=kubernetes`), by muster
(`trustedAudiences: [muster]`) and by Backstage itself.

## If you outgrow static passwords

`staticPasswords` means editing `agentlab.yaml` and reloading. If a demo needs
users created live, put a lightweight LDAP behind Dex's `ldap` connector
instead:

- [lldap](https://github.com/lldap/lldap) — has a web UI, ships an
  [official Dex example config](https://github.com/lldap/lldap/blob/main/example_configs/dex_config.yml).
- [glauth](https://github.com/glauth/glauth) — config-file only, stateless,
  more GitOps-friendly.

Both give real groups on any Dex version. Keycloak is not needed for this.

## Layout

```
main.go                        the CLI (cobra): one subcommand per lifecycle step
internal/config/               agentlab.yaml schema, defaults, validation
internal/forms/                the interactive configuration forms (huh)
internal/lab/                  everything operational:
  certs.go                       the name-constrained lab CA + 825-day leaf certs
  trust.go                       agentlab trust/untrust (system + NSS stores, via smallstep/truststore)
  up.go down.go                  lifecycle; checksum-stamped Dex apply
  test.go                        RBAC assertions for every configured user
  login.go browser.go            password grant / authorization-code flow
  platform.go platformtest.go    agent platform install + MCP smoke test
  backstage.go backstagetest.go  Backstage deploy + headless sign-in proof
  postrender.go                  helm post-renderer (hostNetwork, route strip, nodePort pin)
  helmplugin.go                  generates the Helm 4 postrenderer plugin wrapping it
  templates/                     every manifest, rendered from agentlab.yaml
agentlab.yaml                    your configuration (gitignored; `agentlab configure`)
state/                         rendered manifests, for inspection (gitignored)
.vendor/                       agent-platform-standalone checkout (gitignored)
.mcp.json                      registers muster as an MCP server for Claude Code
```

Useful passthroughs: `agentlab logs dex|muster|backstage` tails logs; the
effective Dex config is `kubectl -n dex get secret dex-config -o jsonpath='{.data.config\.yaml}' | base64 -d`,
muster's is `kubectl -n agent-platform get cm muster-config -o jsonpath='{.data.config\.yaml}'`.
