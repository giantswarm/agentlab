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

`go` (>= 1.25), `docker` (or Podman >= 4's docker-compatible CLI), `kind`
(>= 0.31), `kubectl`, `helm` (**>= 4** — the platform install relies on Helm
4's `--wait`, which waits on the chart's Flux custom resources so the command
returns with every component Ready; Helm 3's does not, and the agent-platform
chart documents a Helm 3 install as not measured), `git`.

(The old script stack also needed openssl, curl, jq and python3; the binary
does all of that itself.)

Under **rootless Podman** the lab publishes its ports from your own network
namespace, which cannot bind anything below
`net.ipv4.ip_unprivileged_port_start` (1024 by default). `agentlab configure`
detects this and moves the agentgateway edge off its default 443 — to 8443,
so the public URLs gain `:8443` — and reports the move. Run Podman as root, or
lower the sysctl, to keep 443.

### Docker resources

The lab runs the whole platform on a single kind node, so whatever backs
docker has to fit it — a Docker Desktop VM, Colima or a podman machine on a
Mac, the host itself under rootless Podman on Linux. **CPU is the binding
constraint, and it is a hard one**: the kube-scheduler refuses
a pod whose CPU *request* does not fit, so a node that is 100m short simply
leaves pods `Pending` forever. It does not degrade, it stalls.

What a full default lab requests (measured from the chart renders at the
pinned versions, plus kind's own control plane on a live node):

| | CPU | Memory |
|---|---|---|
| kind's Kubernetes: apiserver, controller-manager, scheduler, etcd, CNI, CoreDNS | 950m | ~290 MiB |
| the agent platform: muster + valkey, agentgateway + controller, mcp-kubernetes, agent-manager, model-manager, kagent + UI + postgres, Backstage | 1080m | ~1750 MiB |
| Dex | 50m | 64 MiB |
| the chart's Flux engine: the Flux Operator plus the `FluxInstance`'s source-controller and helm-controller (the lab shape brings it with the platform — it delivers every component and the agents) | 250m | 192 MiB |
| observability: kube-state-metrics + mcp-prometheus | 300m | 328 MiB |
| **total requests** | **≈ 2.6 CPU** | **≈ 2.5 GiB** |

The memory column *understates* real use, and by a lot: the Prometheus server
(its CR sets no `resources`), the prometheus-operator and node-exporter
declare nothing at all, and Backstage requests 250 MiB while its Node process
uses several times that. A node running the platform with Backstage and
observability **off** was observed at 2.4 GiB actual — i.e. the whole
requests budget — so a full lab wants roughly twice that.

Give docker at least:

| | CPUs | Memory |
|---|---|---|
| the full default lab (platform + agents + observability + Backstage) | **4** | **6 GiB** (the floor is 5.1 GiB; whole GiB) |
| platform + agents only (`configure --backstage=false --observability=false`) | 3 | 4 GiB (3.9 GiB) |

Those are the floors `agentlab up` enforces, and they already include room for
the pods the platform creates at run time: every kagent agent is another pod,
and `models-test` and `agents-test` each create one. `agentlab up` checks the
runtime before any cluster work — it prints the measured CPUs and memory next
to this configuration's requests and floor on every boot, refuses below the
CPU floor and warns below the memory one. Give it 6 CPUs and 8 GiB if you have
them: the lab is then comfortable rather than exactly large enough.

The second row is the one that has actually been measured on a live node: it
requests ~2.3 CPU (no Backstage, no observability; the chart's Flux engine is
always part of it) and sat at 2.4 GiB of real use — measured before the engine
joined the platform, which adds 250m / 192 MiB of requests on top.

Two CPUs — what a small Docker Desktop or Colima VM gives you — is not enough
for any of it. The symptoms are specific, and worth recognising because
nothing says "out of CPU": `agentgateway` (or any late pod) sits `Pending` while
`kubectl describe node` shows CPU requests at 95%+ of allocatable; the
platform install then waits on a workload that will never start and times
out. With the platform up but the node full, `models-test` gets as far as the
agent turn and fails there — the agent's own pod cannot be scheduled, so the
Agent CR never goes `Ready`.

Disk is not usually the constraint. A first boot pulls a few GiB of images —
on the *host*, always, and side-loads them into the node — and that host
cache survives `agentlab down`, so a re-boot does not pay for them again.

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

Keep it current with `agentlab self-update`. Every command also starts with a
one-line hint on stderr while a newer release is out — a hint, never a gate:
an outdated agentlab keeps working (see "Keeping agentlab current").

Then bring the lab up:

```bash
export ANTHROPIC_API_KEY=sk-ant-...   # optional: powers the agents + Backstage AI chat
./agentlab configure       # interactive form: cluster, users, components
./agentlab up              # certs, kind cluster, Dex, RBAC, the agent platform — verified
./agentlab platform-test   # headless proof: Dex -> muster -> mcp-kubernetes -> apiserver, + the per-server OAuth sign-in challenge
./agentlab models-test     # with an Ollama on the host: pull -> ModelConfig -> agent turn -> delete, through the platform
./agentlab agents-test     # agent-manager as the signed-in user: create -> ready -> update -> delete via muster; a viewer's create is Forbidden; the ServiceAccount holds no RBAC
./agentlab toolsets-test   # declared toolsets end to end: agent-manager requires one, the Agent carries the header, muster resolves and refuses per request, agents see their toolset, a per-server sign-in scopes tools to the token, the portal's Tools step and apply path
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

### `configure` discovers this machine — on every run

Before it asks anything (or, with `--defaults`, writes anything), `agentlab
configure` probes the machine and prints what it found, then applies it to the
configuration — a fresh one and an existing `agentlab.yaml` alike, so the file
follows the host instead of freezing the first run's view of it:

```
Discovering this machine:
  tools             docker 29.7.2, kind v0.32.0, kubectl v1.36.4, helm v4.2.2
  cluster           kind "agentlab" exists — its port mappings are fixed at node creation (`agentlab down && agentlab up` to change them)
  Ollama            0.33.2 on :11434 — answers on 172.21.0.1 (the address pods dial): yes; 10 downloaded, 4 tool-calling
  Lemonade Server   11.9.0 on :13305 — answers on 172.21.0.1 (the address pods dial): yes; 4 downloaded, 3 tool-calling
  Anthropic key     $ANTHROPIC_API_KEY is set — the agents' default ModelConfig and Backstage's AI chat get the real key at deploy time

Applied to the configuration:
  platform.modelManager.backends: [ollama] -> [ollama, lemonade]
```

- **Tools**: `docker`, `kind`, `kubectl`, `helm` are looked up and their
  versions shown; a missing one (or a Helm 3) is called out here rather than
  minutes into `agentlab up`.
- **Ports**: every host-side port is probed on 127.0.0.1 — the address all
  kind port mappings bind. While **no kind node of this configuration
  exists** (a fresh lab, or after `agentlab down`), an occupied port is moved
  to a nearby free one, with a message saying what moved where (443 falls
  back to 8443). When the edge leaves 443, every public URL in this README
  gains that port suffix (`https://backstage.127.0.0.1.nip.io:8443`); the
  lab's edge Service serves that port in-cluster as well, so the ported URLs
  resolve from pods too and every proof still passes. Once the cluster exists
  its mappings are fixed at node creation, so the ports it publishes never
  count as occupied and a foreign listener on one of them is **reported**, not
  renumbered around (free it, or `agentlab down`, re-run `configure`,
  `agentlab up`).
- **Host model servers**: an Ollama on `:11434` and a Lemonade Server on
  `:13305` (their default ports) are detected with version, whether they
  listen on the kind docker gateway (pods' path to the host — the bind-address
  fix is named when they do not) and their downloaded models, counting the
  tool-calling ones. What answers becomes `platform.modelManager.backends`
  (Ollama first) and turns managed models on; a server that vanished drops
  out and, with none left, managed models go off — see [Managed
  models](#managed-models-model-manager--the-host-model-servers). A
  standalone `flm serve` (FastFlowLM's own server, default `:52625`) is
  reported but not wired: it has no management API and lists its catalog
  rather than what is downloaded — the lab drives FLM through Lemonade.
- **`$ANTHROPIC_API_KEY`**: whether it is exported, since the agents'
  default ModelConfig and Backstage's AI chat take it at deploy time.

Pins override the discovery for that run: `--model-manager[=false]` decides
the flag regardless of what answers, `--model-manager-backends ollama,lemonade`
sets the list (and its order) outright; `--platform`, `--agents`,
`--observability` and `--backstage` toggle the components as before, with or
without `--defaults`. Nothing else in an existing file is touched — users,
extra models and the pinned chart ref stay as they are.

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
[agent-platform](https://github.com/giantswarm/agent-platform) chart — the
same chart every Giant Swarm management cluster runs — wired to the lab Dex,
so Claude Code can drive a Kubernetes MCP server living inside the kind
cluster. The chart is a meta-package: it renders one Flux `OCIRepository` +
`HelmRelease` per component (muster, valkey, agentgateway, the MCP
registrations, kagent, agent-manager, model-manager, mcp-kubernetes,
Backstage, and the connectivity chart that wires them together), and its
bundled Flux engine turns those into running workloads.

The lab installs it in its **lab shape**:

- **The bundled engine is on** (`components.flux.enabled: true`, the chart's
  default): the chart brings the Flux Operator and one `FluxInstance` running
  source-controller + helm-controller under the multi-tenancy lockdown, plus
  the tenant identity `agent-platform-flux` every platform `HelmRelease` runs
  as. Nothing else in the lab installs Flux — the same engine also delivers
  the agents the Backstage create flow and agent-manager write as
  `HelmRelease`s.
- **Self-management is off** (`gitops.self.enabled: false`). With the engine
  on, the chart would by default render its own `OCIRepository` +
  `HelmRelease`, adopt the release and follow the *published* chart's version
  range — and refuse every later `helm upgrade` from the CLI. The lab exists
  to install what is **not** released yet (a local chart checkout, a dev
  image), which that `HelmRelease` would replace with whatever the registry
  holds. So the Helm CLI keeps owning the release: `agentlab platform` is one
  idempotent `helm upgrade --install … --wait` (no post-renderer, no
  `--force-conflicts`), and `helm upgrade` stays the day-2 tool. Never drop
  the value on a lab: the first upgrade without it makes the release
  self-managed.
- The chart is **pinned** to an exact release, `platform.chartVersion` in
  `agentlab.yaml` (the default is the release this agentlab was verified
  with). The lab never floats; bump the pin deliberately, with a lab run.

The platform installs as part of `agentlab up` (it is enabled in the default
configuration); on an already-running cluster the steps are also standalone:

```bash
./agentlab platform       # helm upgrade --install of the chart, then waits for every component
./agentlab platform-test  # headless proof of the whole chain
```

The lab's patches on the component charts — `hostNetwork` on muster and
Backstage, the `dex-localhost` sidecar on the MCP servers, the kagent UI
NodePort, and the dev images below — are per-component **`postRenderers`
values** (`components.<name>.postRenderers` in
`state/agent-platform-values.yaml`): Kustomize patches the chart forwards to
that component's `HelmRelease` and the bundled helm-controller applies over
the component chart's render — the same mechanism the fleet has for chart
fixes.

### Installing an unreleased chart

Point `platform.chartPath` at a local checkout's chart directory and re-run
`agentlab platform` — nothing needs a release or a push:

```yaml
platform:
  chartPath: /path/to/agent-platform/helm/agent-platform
```

(or `agentlab configure --defaults --chart-path /path/to/agent-platform/helm/agent-platform`;
`--chart-path ""` clears it). `chartVersion` is ignored while it is set, and
the boot says which chart it installed. The directory is read, never written.

### Dev images

To run a component from a build of your own, build the image, and name it
under `platform.devImages` keyed by the chart's component name (`muster`,
`backstage`, `kagent` — the controller —, `mcp-kubernetes`, `model-manager`,
`agent-manager`):

```yaml
platform:
  devImages:
    muster: muster:dev-1a2b3c
    backstage: backstage-dev:my-feature-4d5e6f
```

`agentlab platform` side-loads the image from the host docker cache into the
node, checks the node lists it before anything installs (a missing image
fails right there with its fix, instead of five minutes later as an
`ImagePullBackOff`, a helm-controller timeout and a rollback to the chart's
image), and renders it into that component's `postRenderers` as a Kustomize
image override with `imagePullPolicy: IfNotPresent`, so the swap is part of
the release: the same plain `helm upgrade` applies it, `kubectl get deploy -o
jsonpath` shows the new image, and removing the entry restores the chart's
image on the next run. Use a distinctive tag per build — kind's containerd
keeps running the old bits under a reused tag. And make it a build of its
own, not a re-tag of a registry image the kubelet has pulled (a chart that
pulls `Always`, like Backstage's, makes the kubelet pull): since Kubernetes
1.33 the kubelet remembers which image IDs it pulled and re-pulls a pod's ref
that maps to one of them (`KubeletEnsureSecretPulledImages`), so such a
re-tag ends in `ImagePullBackOff` against Docker Hub while a real build — a
new image ID, side-loaded, never pulled by the kubelet — is used as is. (A `kubectl patch` on the
Deployment still works for a quick look, but only until helm-controller's
next release of that component overwrites it — the values are the durable
path.)

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

### Signing in to a downstream server (muster as OAuth client)

muster plays two OAuth roles. Towards Claude Code and Backstage it is the
**server** (above). Towards a downstream MCP server that declares
`auth.type: oauth` it is the **client**: `core_auth_login` (what the portal's
per-server **Sign in** button calls) answers a challenge — a
`https://muster.<domain>/oauth/proxy/start?state=…` URL — the browser follows
it through the downstream's authorization server, and muster keeps the token
per session. The chart leaves that role off (`oauth.mcpClient`), so the
lab's values turn it on with the edge URL as `publicUrl` (the chart derives
the callback `/oauth/proxy/callback` and serves muster's client ID metadata
document at `/.well-known/oauth-client.json`), the way real installations run
it.

Nothing the lab aggregates needs a per-user login, though — mcp-kubernetes,
mcp-prometheus and model-manager are unauthenticated in-cluster — so
`agentlab platform` ships one downstream that does: the MCPServer
`lab-oauth-fixture` (`oauth-fixture.yaml.tmpl`, annotated as a fixture). It
points muster at its **own** protected `/mcp` endpoint (a 401 with RFC 9728
metadata at zero cost), so the CR sits at `Auth Required` until a session
signs in and every `core_auth_login` yields a fresh challenge. The
authorization server the sign-in walks through is **pinned to the lab Dex**
(`spec.auth.authorizationServer`, the shape an operator uses for a GitHub App:
Dex's authorization and token endpoints, the client from the Secret
`lab-oauth-fixture-client`, the scopes — and, as the `issuer`, muster's own
public URL, the identity the grant is filed under: it must be the server the
endpoint's RFC 9728 metadata names, or no call finds the token) rather than
discovered. Discovery would name muster's own OAuth 2.1 server,
which identifies clients by Client ID Metadata Document — and that server's
SSRF guards refuse every lab hostname, for the metadata URL and for a
registered redirect URI alike (`muster.127.0.0.1.nip.io` resolves to the
edge's cluster IP in-cluster and to loopback outside; muster exposes no knob
for mcp-oauth's `AllowPrivateIPClientMetadata`, HACKS.md U18), so no sign-in
could ever complete there. Dex matches redirect URIs exactly: the platform
client lists muster's proxy callback `/oauth/proxy/callback` next to its
server-role callback (`dex.yaml.tmpl`), and the token Dex issues carries the
platform client's audience, which the endpoint trusts. Completing the sign-in
therefore connects muster to itself and surfaces its own tools under
`x_lab-oauth-fixture_` — harmless, per session, and exactly what the toolset
proof needs (a toolset naming the server resolves to its tools only for the
session that signed in). What the self-aggregation costs: right after a muster
restart the CR reads `Failed` (muster dials itself before its listener is up)
until the reconnect backoff flips it to `Auth Required`, about a minute —
`agentlab platform` waits for that.

`agentlab platform-test` proves the path headlessly on a fresh MCP session
(the shape of one portal user's session): `list_tools` flags the fixture under
`servers_requiring_auth`; `core_auth_login` answers a challenge on
`/oauth/proxy/start` with a state; a second call answers a **fresh** state (a
re-clicked Sign in gets its own challenge, giantswarm/backstage#2203); and the
URL redeems — GET-ing it redirects to the pinned authorization server (Dex,
client `agent-platform`) rather than rejecting the state. `agentlab
backstage-test` proves the portal hop for every user: the fixture is listed
`Auth Required` and `POST /api/muster/auth/login` answers `status:
auth_required` with the same URL shape, on that user's own forwarded token.
`agentlab toolsets-test` completes the sign-in headlessly (the Dex login form,
then muster's proxy callback) and proves what it unlocks — see
[Toolsets](#toolsets-declared-tool-access).

To drive the UI: **Agent Platform → MCP Servers**, expand `lab-oauth-fixture`,
**Sign in** — the popup lands on Dex directly. After a muster pod roll the row
reads `Failed` for about a minute (Reconnect, or wait).

### The fake fleet and the tool-group label

A real installation federates many management clusters: agent-platform-mcps
renders one MCPServer per cluster and per infrastructure family — `kubernetes`,
`capi`, `prometheus` — each declaring `spec.family` (so muster exposes the
family once, `x_kubernetes_<tool>` with a `management_cluster` argument) and
labelled `muster.giantswarm.io/management-cluster: <cluster>`. The lab has one
cluster and its bundled `mcp-kubernetes` declares no family, so nothing here
looks like a fleet — yet the portal's MCP servers page, its dashboard's fleet
coverage and the agent create flow's Tools step group servers by family and by
tier. `agentlab platform` therefore ships a **fake fleet**
(`fleet-fixture.yaml.tmpl`, `fleetfixture.go`): three families × two fake
management clusters (`lab-01`, `lab-02`), six MCPServers named
`<family>-<cluster>` in `agent-platform`, each with the family block, the
management-cluster label, the `muster.giantswarm.io/type` the fleet charts
stamp, and the tier label:

```yaml
agent-platform.giantswarm.io/tool-group: infrastructure
```

**What the label is for.** It is the Agent Platform's tiering of MCP servers —
three groups the portal's MCP servers page, the Tools step, muster's toolset
presets and the docs all use under the same names: **Agent Platform**
(`agent-platform`: agent-manager, model-manager, muster's core tools),
**Infrastructure** (`infrastructure`: the kubernetes/capi/prometheus families)
and **Registered servers** (no label: whatever an installation or a user
registers, including everything the portal's Register server flow creates).
The chart that ships a server stamps it; every consumer only reads it, e.g.
`kubectl get mcpservers.muster.giantswarm.io -A -l agent-platform.giantswarm.io/tool-group=infrastructure`
lists the fleet families. Orientation and preset membership, not
authorization — that stays with the servers' OAuth and the clusters' RBAC.
In the lab only the fake fleet carries the label: `lab-oauth-fixture` and
anything you register by hand stay unlabelled on purpose, so the Registered
servers group has members; the vendored agent-manager / model-manager /
component charts bring their own labels (`agent-platform`; `infrastructure` on
the bundled `mcp-kubernetes`) once bumped to the releases that stamp them.

The members point at muster's own protected `/mcp` with `auth.type: oauth`,
like the OAuth fixture, and so read `Auth Required` — what an unconnected
forwardToken fleet member shows on a real installation — at zero cost.
Pointing them at the lab's single mcp-kubernetes instead makes muster open one
connection per member per user session; a dozen of those rate-limited
mcp-kubernetes (429) and took the session's real `mcp-kubernetes` connection
down with them. The fixture exists to be grouped, listed and selected, not
called. `agentlab platform-test` asserts the label: the `infrastructure`
selector lists every member and, of the lab's own CRs, nothing else; every
value in the cluster is one of the two the contract knows; `lab-oauth-fixture`
is unlabelled. Servers the vendored charts label are reported, not judged.

### Toolsets (declared tool access)

A **toolset** is the selector list an agent declares — the `agent` chart's
`toolset` value, rendered as the `X-Muster-Toolset` header on the agent's
muster tool entry (`spec.declarative.tools[0].headersFrom`), agent-manager's
`toolset` argument, the portal's Tools step — that bounds which of the
gateway's tools the agent's meta-tools can see and call. Selectors:
`preset:<name>`, `server:<name>`, `workflow:<name>`, `tool:<name>`; built-in
presets `read-only`, `none`, `full`; the platform chart ships `infrastructure`
and `agent-platform`, defined by the tool-group label above. muster evaluates
the header **per request** on the caller's own catalogue, so the invoking
human's identity and the backends' authorization stay the boundary: a toolset
is composition, not authorization, and it never widens what the person could
reach.

`agentlab toolsets-test` proves the feature end to end against the released
components, as the admin, and leaves nothing behind (its agents are named
`agentlab-toolset-*`, its workflows likewise):

1. **agent-manager's contract** through muster: `create_agent` without a
   toolset is refused naming the shipped presets and `preset:none`; the removed
   `toolNames` argument is refused with the explaining error; four agents are
   created with `preset:read-only`, `preset:none`, `preset:full` and
   `server:lab-oauth-fixture`.
2. **What was rendered**: the header on each Agent CR with the joined
   selectors, the value on each HelmRelease, `get_agent` reporting the same;
   the `preset:none` agent has **no** muster tool entry at all; a HelmRelease
   applied without the value (every agent that predates toolsets) renders a
   header-less entry and `list_agents` reports `implicitFullAccess: true`.
3. **muster's resolution**, in sessions of the same user carrying the header
   an agent's runtime sends: two workflows are created (`core_workflow_create`
   as the caller), one query-only and one with a destructive step, and
   `describe_tool` shows the derived `readOnlyHint` on the first only;
   `preset:read-only` resolves to exactly the tools annotated `readOnlyHint`,
   whatever their kind — the query-only workflows included (the lab's
   `lab-cluster-overview` among them), the mutating one excluded, muster's
   core tools by their own annotation: since muster 5.13.0 the reads such as
   `core_config_get` and `core_workflow_list` are in, the writes such as
   `core_workflow_delete` never are (a muster whose core tools carry no
   annotations puts none in the preset); the read-only Kubernetes call succeeds
   and the destructive call (agent-manager's `delete_agent` — the lab's
   mcp-kubernetes runs non-destructive and registers no writers) and the
   mutating workflow are refused with `tool "…" is outside the toolset
   [preset:read-only]`; `preset:none` sees and gets
   nothing; `preset:full` equals the unscoped catalogue; header errors (an
   unknown preset, the reserved `toolset:`, an inline `label:`) are error
   results, never a fall-back; two toolsets on one token — request by request
   in one session and from two sessions in parallel — each see their own
   tools; when the platform chart shipped `infrastructure` / `agent-platform`,
   each resolves to the tools of the servers carrying that label (the latter
   plus `core_*`).
4. **The runtime path** (skip with `--skip-chat`): through kagent's A2A
   endpoint behind the edge, as the user, the read-only agent lists nothing
   outside `preset:read-only` — read-only core tools may appear, no writer
   does (so kagent sends the header and the user's token) — and the
   `preset:none` agent answers a chat turn. The agents run on
   `default-model-config` (`--model-config` to pick another).
5. **The sign-in claim (ground-truth G6)**: before any sign-in,
   `server:lab-oauth-fixture` resolves to nothing for everyone
   (`toolset_unmatched` names the selector); the portal's Sign in (`POST
   /api/muster/auth/login` with the portal's own forwarded Dex id_token) yields
   the challenge, which the proof completes as the browser would; an
   agent-shaped session on the **same** id_token then resolves the fixture's
   tools and calls one; the real agent, driven through kagent with that token,
   reports them too. A second user, and the same user under a **fresh**
   id_token, still resolve nothing: muster keys a forwarded bearer's session by
   the token (`ext-<hash>`), the grant belongs to that session
   (`grantScope: session`), and the portal forwards one and the same
   id_token to muster and to kagent — which is why the claim holds in the
   portal and only there.
6. **The portal**: the Tools step's backend calls (`/api/muster/tools/filter`
   with `include_presets`, a `toolset=` resolution, an unmatched selector, an
   unknown preset relayed as muster's error), and the composer's apply path —
   the same hidden scaffolder template the wizard's Deploy drives, with the
   composer's manifest and the user's OIDC token as the secret — landing the
   `toolset` value on the HelmRelease and the header on the Agent.

`agentlab agents-test` declares `preset:read-only` for its agent and asserts
the refusal without one (agent-manager ≥ 0.4.0 requires a toolset), and
`agentlab backstage-test` proves the MCP servers page's three groups from the
data the page reads — see [The muster plugin](#the-muster-plugin).

### Agents (kagent)

The platform's agent runtime is an **optional component**, on by default
(`platform.agents` in `agentlab.yaml`; headlessly:
`agentlab configure --defaults --agents=false`). Agent delivery runs through
Flux on every installation — the portal and agent-manager write each agent as
a `HelmRelease` of the agent chart — and in this lab through the platform
chart's bundled engine (see [The agent create flow](#the-agent-create-flow));
skip the runtime when agents are not what you are testing. When enabled, the
chart's **kagent** component installs and `Agent` CRs run against a
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
Service is simply `type: NodePort`, pinned to node port 30880 by the kagent
component's `postRenderers` patch (the chart's Service template renders no
`nodePort` field — HACKS.md U9), and the kind config maps that onto the host. Like every
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
gateway in kind). The chart also renders the shared `RemoteMCPServer`
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

#### Local backends on the lab host (Ollama, Lemonade/NPU)

A model server on the lab host itself is the cheapest self-hosted endpoint,
and everything that can go wrong is host-side plumbing, not kagent:

- **Address**: pods reach the host only through the kind docker network's
  gateway — `docker network inspect kind` names it, typically `172.21.0.1`.
  That IP goes in `baseUrl`; `localhost` would be the agent pod itself.
- **Bind address**: the server must listen on `0.0.0.0` (or the bridge IP).
  The usual `127.0.0.1` default is unreachable from pods regardless of any
  firewall rule. Ollama: `OLLAMA_HOST=0.0.0.0`. Lemonade:
  `lemonade config set host=0.0.0.0`.
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
```

One Lemonade-specific note: its FastFlowLM models default to a 4096-token
context, which agent system prompts plus tool schemas outgrow quickly —
raise it once with `lemonade config set ctx_size=16384`.

#### Managed models: model-manager + the host model servers

`extraModels` wires an endpoint and manages nothing: pulling or removing a
model is CLI-on-host, and nothing shows what is downloaded or loaded. The
managed mode puts the chart's **model-manager** component
([giantswarm/model-manager](https://github.com/giantswarm/model-manager),
the service behind the Model Manager epic) in front of a model server on the
host — inventory of downloaded and loaded models, pull with progress,
load/unload, delete, and every pulled model wired into kagent automatically
as a `ModelConfig`: the native, keyless `Ollama` provider for an Ollama, the
`OpenAI` provider on `/api/v1` (placeholder key) for a Lemonade Server. One
block, which `agentlab configure` writes from what answers on the machine:

```yaml
platform:
  agents: true                 # required: the ModelConfigs land in kagent
  modelManager:
    enabled: true
    backends: [ollama, lemonade]   # the host model servers, Ollama first; kserve needs GPUs + KServe
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

`agentlab configure` detects an Ollama on `:11434` and a Lemonade Server on
`:13305` on every run (`--model-manager[=false]` pins the flag,
`--model-manager-backends` the list; the interactive form shows what was
found). Each endpoint is **autodetected at platform time** as `http://<kind
docker network gateway>:<default port>` — `docker network inspect kind`, the
same address the section above documents for `extraModels` — so nobody types
`172.21.0.1`; set `endpoints.<backend>` for a server elsewhere on the LAN
(such a backend is kept whether or not one answers locally).

What `agentlab platform` (or `up`) does with it:

- **Preflight, not README traps.** Before the install, a short-lived pod in
  the cluster fetches each server's version document (`/api/version` on
  Ollama, `/api/v1/health` on Lemonade). If that fails, the boot stops right
  there with the diagnosis and the two fixes from the section above spelled
  out — *connection refused* means the server listens on `127.0.0.1` only
  (`OLLAMA_HOST=0.0.0.0` / `lemonade config set host=0.0.0.0`), a *timeout*
  means the host firewall drops pod→host traffic on the docker bridge (allow
  the server's TCP port from the bridge subnets, inside `172.16.0.0/12`) —
  instead of a model-manager pod reporting an unhealthy backend after Helm's
  ten-minute wait, or ModelConfigs pointing at a dead endpoint.
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
lemonade` proves the Lemonade Server through the same model-manager (default:
the first of the list). `--model` picks another small, **tool-calling
capable** model; the defaults are `qwen2.5:0.5b` (~400 MB) on ollama and
`qwen3-4b-FLM` (3.1 GB, the smallest tool-calling FastFlowLM model — the
smaller `*-FLM` ones cannot call tools) on lemonade; `smollm2:135m` pulls
fine and then fails every agent turn with "does not support tools". Every
request names the backend, the ModelConfig must carry the backend label, and
the run goes through the platform path only and leaves nothing behind:

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
comes from the chart's `agent-platform-mcps.mcpServers` values — with a
`group` deliberately **outside** the chart's `muster.families` (`kubernetes`,
`prometheus`): a `prometheus`-family entry would surface the tools as
`x_prometheus_<tool>` with a required `management_cluster` argument, which is
the multi-cluster UX, not this single-cluster lab's. What the lab exercises is
the mcp-prometheus tool chain against a plain local Prometheus; the GS
production shape (Alloy → Mimir, `X-Scope-OrgID` tenancy) is out of scope.

The platform's own monitors ride along: with observability on, the chart's
muster ServiceMonitor + PrometheusRule, the kagent ServiceMonitor, the valkey
PodMonitor and mcp-prometheus's own ServiceMonitor are all enabled, and the
lab Prometheus scrapes them (its monitor/rule selectors are opened with
`*NilUsesHelmValues: false` — upstream's default would only select monitors
carrying the kps release label).

**Backstage's own metrics views ride along too.** The Clusters and
Deployments pages query Mimir through gs-backend's `MimirService`, hardcoded
to `https://observability.<baseDomain>/prometheus/api/v1/query`; the chart
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
| `networkPolicy.enabled: false`, `kyvernoPolicies.enabled: false` | The chart's own policy objects. No Cilium and no Kyverno in kind, so both would render CRs whose API groups the cluster does not serve. |
| The muster/kagent ServiceMonitors, valkey PodMonitor and muster PrometheusRule follow `platform.observability` | Without it there is no Prometheus Operator, so none of those CRDs exist and the releases fail to render. With it they are scraped by the lab Prometheus — whose selectors are opened up (`*NilUsesHelmValues: false`) because upstream's default selects only monitors carrying the kps release label, and the platform's monitors come from other releases. Flipping observability rolls the muster pod once (the toggle changes its metrics-exporter env). |
| `platform.observability`: the GS kube-prometheus-stack constituent installed directly, Prometheus server re-enabled, instead of the observability-bundle | The bundle is MC-shaped (Flux HelmReleases with a hardcoded remote kubeconfig, Alloy → Mimir, no local PromQL endpoint). See [Observability](#observability-prometheus--mcp-prometheus). |
| `muster.muster.oauth.mcpClient.enabled: true` + the `lab-oauth-fixture` MCPServer | The chart leaves muster's OAuth *client* role — the proxy behind `core_auth_login` and the portal's Sign in — off; real installations turn it on, and without it no per-server sign-in can be exercised. The fixture is the one `Auth Required` downstream to sign in to (muster's own protected `/mcp`); see [Signing in to a downstream server](#signing-in-to-a-downstream-server-muster-as-oauth-client). |
| The fake-fleet MCPServers (`<family>-lab-01`, `<family>-lab-02`) with `agent-platform.giantswarm.io/tool-group: infrastructure` | One cluster and a family-less bundled `mcp-kubernetes` look nothing like the federated fleet the portal's server groups, fleet coverage and Tools step are built for. Six `Auth Required` family members fake it, carrying the tier label the fleet charts stamp; see [The fake fleet and the tool-group label](#the-fake-fleet-and-the-tool-group-label). |
| `muster.rbac.{mcpServerEditor,workflowEditor}.subjects` → `oidc:platform-admins` | The chart binds muster's editor Roles to Giant Swarm's admin groups, which do not exist here. Rebound to the lab's own admin group (`--oidc-groups-prefix=oidc:`, same spelling as the lab RBAC). Lists replace, so the GS groups are dropped. |
| muster patched to `hostNetwork` + `maxSurge: 0` | Same issuer trick as the apiserver and Backstage. `maxSurge: 0` because two hostNetwork pods cannot both bind `:8090` on a one-node cluster. A Kustomize strategic-merge patch in `components.muster.postRenderers`, which the chart forwards to muster's `HelmRelease` and the bundled helm-controller applies over the muster chart's render. |
| The `dex-localhost` sidecar on mcp-kubernetes, model-manager, agent-manager and mcp-prometheus | Those servers validate the forwarded Dex token themselves and must reach the issuer URL `https://localhost:32000/dex`, but all listen on `:8080` and cannot share the host network. A socat sidecar on the pod's own loopback forwards `:32000` to the Dex Service (HACKS.md U13) — a `postRenderers` patch on each component, and on the lab's own mcp-prometheus `HelmRelease`. |
| `components.kagent.enabled` from `platform.agents`, `controller.auth.mode: unsecure`, kagent ServiceMonitor + OTel off | Agents are part of what the lab tests, so kagent is on by default (the chart defaults it off) but optional — `platform.agents: false` skips the runtime. `unsecure` because the GS `trusted-proxy` mode assumes a JWT-validating agentgateway in front; no Prometheus Operator / OTLP gateway in kind. See [Agents (kagent)](#agents-kagent). |
| `kagent.ui.service.type: NodePort`, nodePort 30880 pinned by the kagent `postRenderers` patch | On a real MC the UI sits behind the agentgateway edge; this lab publishes it through the kind port mapping instead (host side `platform.agentsPort`, default 8081). The chart's Service template renders no `nodePort` field, so the fixed node port is a patch (HACKS.md U9). |
| `components.flux.enabled: true`, `gitops.self.enabled: false` | The lab shape (see [The agent platform](#the-agent-platform-muster--kubernetes-mcp)): a management cluster runs its own Flux and installs the chart through it; the lab has none, so the chart brings the engine — and must not adopt its own release, because the lab installs charts and images that are not released. |
| The chart pinned to an exact release (`platform.chartVersion`) | Component versions are the chart's own ranges, resolved by its Flux at reconcile time (the fleet's dogfooding track). The chart itself never floats in the lab: two runs install the same thing, and a bump is a deliberate edit with a lab run behind it. |
| `mcp-prometheus` as a lab-rendered Flux `HelmRelease` | The one release the lab installs outside the chart rides the same engine, as the same tenant identity, so its lab-only sidecar is a `postRenderers` patch like the others and there is exactly one Helm writer (the CLI, for the chart) and one Flux engine on the cluster. |

### Platform gotchas

- **A cluster built by an earlier agentlab needs a clean slate.** Before the
  chart brought its own engine, the lab installed the agent-platform-standalone
  umbrella under the same release name and its own Flux controllers in
  `flux-system`; the chart refuses a second Flux, and there is no in-place
  migration on purpose — the kind cluster is throwaway. `agentlab platform`
  names what it found and asks for `agentlab down && agentlab up`. It also
  removes the leftovers of those versions in the working directory
  (`.vendor/`, `state/helm-plugins/`, `state/flux-values.yaml`,
  `state/mcp-prometheus-values.yaml`). To keep the cluster and its Dex
  instead, `agentlab platform-down` then `agentlab platform`: platform-down
  uninstalls the umbrella and those Flux controllers too, and the component
  releases replace the umbrella's CRDs (`crds: CreateReplace`).
- **`agentlab platform-down` is the chart's ordered teardown.** It deletes
  the lab's mcp-prometheus `HelmRelease` while the engine still runs, then
  `helm uninstall --wait` runs the chart's pre-delete hooks — delete the
  component `HelmRelease`s and wait for their releases, then delete the
  `FluxInstance` and wait for the operator to remove Flux with its CRDs,
  which takes every remaining `HelmRelease` (the agents' too) with it — and
  only then deletes the namespaces. Nothing is left with a finalizer nobody
  processes. The four Flux Operator CRDs and the prometheus-operator CRDs
  stay (Helm never removes a chart's `crds/`); a reinstall is clean. On a
  cluster an earlier agentlab built it also uninstalls the Flux controllers
  that lab installed itself (release `flux` in `flux-system`).
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
- **The `kagent` namespace follows the kagent component.** While
  `components.kagent.enabled` is true the chart's pre-install/pre-upgrade hook
  creates it ahead of the kagent `HelmRelease`, the connectivity component
  adopts it, and `helm uninstall` (the ordered teardown) removes it with the
  connectivity release. The lab creates no namespace of its own for kagent.
- **Kubernetes tools carry the server-name prefix.** The chart's bundled
  `mcp-kubernetes` MCPServer declares no muster *family*, so its tools use
  per-server prefixing: `call_tool(name=x_mcp-kubernetes_list, arguments={...})`. The fake fleet's members are the lab's only family servers and they stay
  `Auth Required`, so no `x_kubernetes_*` family tools appear in a session.
  (Real fleet installations register per-cluster servers with a `kubernetes`
  family and a `management_cluster` instance argument instead.)
- **Tool results are double-wrapped.** `result.content[0].text` is JSON whose
  `content[0].text` is the actual payload — two decode hops.

## Backstage

Backstage deploys **with the platform** — the chart chart's `backstage`
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

- **`hostNetwork: true` on the Backstage pod** (a `postRenderers` patch on
  the backstage component, the same as muster's). The issuer is `https://localhost:32000/dex`, and from
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

The MCP servers page groups servers into **Agent Platform**,
**Infrastructure** and **Registered servers** by the tool-group label the
shipping chart stamps on the CR (see [The fake fleet and the tool-group
label](#the-fake-fleet-and-the-tool-group-label)); a family's members
(`spec.family.name`) collapse into one row. The page reads the MCPServer CRs
through Backstage's Kubernetes proxy with the user's own token, and
`agentlab backstage-test` asserts the grouping from that same data with the
plugin's arithmetic: the fake-fleet families under Infrastructure, the OAuth
fixture under Registered servers, every chart-labelled server under the group
its label names, and — with every label removed from the same data — one
Registered servers list with all sections present, never an empty page.

`agentlab backstage-test` drives the whole sign-in headlessly for every
configured user and then proves the muster hop with that user's own token —
including the per-server **Sign in** path (`/api/muster/auth/login`) against
the lab's `Auth Required` fixture:

```
=== dev@lab.local ===
  dex asserted    groups=[developers] email=dev@lab.local
  token audience  [kubernetes muster backstage]
  backstage user  user:default/dev
  ownership refs  [user:default/dev]
  muster servers  [(agent-manager, Connected), (capi-lab-01, Auth Required), (capi-lab-02, Auth Required), (kubernetes-lab-01, Auth Required), (kubernetes-lab-02, Auth Required), (lab-oauth-fixture, Auth Required), (mcp-kubernetes, Connected), (mcp-prometheus, Connected), (model-manager, Connected), (prometheus-lab-01, Auth Required), (prometheus-lab-02, Auth Required)]
  sign-in challenge lab-oauth-fixture -> https://muster.127.0.0.1.nip.io/oauth/proxy/start?state=… (client id via preregistered)
  muster workflows [lab-cluster-overview]
  muster core tools 28 exposed
  agent deploy template registered (template:default/agent-deployment)
  MCP servers page groups (11 CRs via /api/kubernetes/proxy):
    Agent Platform      2 rows: agent-manager, model-manager
    Infrastructure      4 rows: capi, kubernetes, prometheus, mcp-kubernetes
    Registered servers  2 rows: lab-oauth-fixture, mcp-prometheus
  fallback without any agent-platform.giantswarm.io/tool-group label: 8 rows, all under Registered servers; 9 of 11 CRs carry the label today
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
- **The delivery engine.** The platform chart's bundled Flux — the Flux
  Operator's `FluxInstance` with source-controller and helm-controller under
  the multi-tenancy lockdown — is what turns those CRs into an installed agent
  chart. Nothing watches git: it reconciles exactly the objects that are
  applied to it. The lab installs no Flux of its own.

Everything lands in the selected ModelConfig's namespace (`kagent`): one shared
`OCIRepository/agent` tracking `semver: x.x.x`, one `HelmRelease` per agent
named after its slug. The `HelmRelease`s execute as the tenant identity
`kagent-flux` — a ServiceAccount and a namespace-scoped RoleBinding the chart's
connectivity component renders whenever kagent is on, and names into the
portal's `agentPlatform.fluxServiceAccountName` and agent-manager's
`flux.helmReleaseServiceAccount` from the one value
`kagent.fluxServiceAccountName` — because under the engine's lockdown a
`HelmRelease` without one runs as the rights-less default account and fails.
RBAC still applies to the *apply* step itself: it runs with the
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

- **The lab never uses your kubeconfig's current-context.** Every command that
  talks to the cluster first exports the kind cluster's kubeconfig to
  `state/kubeconfig` and runs its own `kubectl` and `helm` with `KUBECONFIG`
  pinned to it — so a shell with no current-context (or one pointing at a real
  cluster) proves the same lab as any other, and a lab that is not running
  fails by name (`no kubeconfig for kind cluster "agentlab"`) instead of as a
  kubectl error. Your own kubeconfig is only ever touched by kind itself
  (`kind create/delete cluster` merge the `kind-<cluster>` admin context in
  and out); `agentlab` never switches your current-context. The same view from
  a shell: `KUBECONFIG=state/kubeconfig kubectl -n agent-platform get pods`.
  A probe that fails reports what kubectl said (`kubectl failed: ... current-context
  is not set`), never an empty status.
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
- **`agentlab down` can lose a race with docker and leave an exited node.**
  `kind delete cluster` is `docker rm -f` of the node container; docker gives
  it ten seconds after SIGKILL to exit and then gives up (`could not kill
  container: ... did not receive an exit event`) — a node busy with an
  `agentlab up` side-load in another shell has taken 44 s. The container
  stays in `docker ps -a` as `Exited (137)`, kind still lists the cluster,
  and docker's restart policy does not fire (an API kill counts as a manual
  stop). `agentlab down` therefore waits (up to 90 s) for the node to exit
  and deletes again, and `agentlab up` on a cluster whose node is not running
  starts the container (`docker start`, which kind supports) and waits for
  the apiserver before touching anything — instead of failing inside `kind
  get kubeconfig` with a runc `nsexec ... No such file or directory` or
  `container ... is not running`. Do not run `down` while another `agentlab`
  process is using the cluster (`ps -eo pid,args | grep '[a]gentlab '`).
- **The image cache manifest only records what a registry can serve.**
  `state/preload-images.txt` is snapshotted from the node after every boot;
  an image built on the host and side-loaded (`kind load docker-image
  backstage-dev:<tag>`) shows up there as `docker.io/library/backstage-dev:<tag>`
  — a Docker Hub ref that does not exist — and once the local copy is pruned
  every boot would ask Docker Hub for it (`denied: requested access to the
  resource is denied` in the dockerd log, once per ref). Images the host cache
  knows without a registry digest are therefore left out of the snapshot.

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
`/api/auth/<provider>/handler/frame`, and the chart names it
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

## Keeping agentlab current

`agentlab self-update` replaces the running binary with the latest GitHub
release for your OS and architecture — the command muster and mcp-kubernetes
have too. `agentlab self-update --check` only reports the running and the
latest version, with exit status 125 when a newer one exists (for scripts). A
binary without a release version (`agentlab --version` says `dev`) is
refused: reinstall it from a release or with `go install`. A `go build` from
a checkout carries Go's pseudo-version (`v0.19.3-0.20260908…-8536d36`) and
is treated as what it is: after the tag before it, before the tag after it.

**Every release binary is verified before it is installed.** CI (the
architect orb) signs each `agentlab-<os>-<arch>` with cosign — keyless, the
CircleCI pipeline's identity, recorded in the Rekor transparency log — and
publishes the signature next to it as `agentlab-<os>-<arch>.bundle`.
`self-update` downloads both and installs the binary only after the bundle
verifies against the Sigstore public-good trust root for a CircleCI build of
`github.com/giantswarm/agentlab` (the shared
[`selfupdate-cosign`](https://github.com/giantswarm/selfupdate-cosign)
validator, the one muster and the other Giant Swarm CLIs use). A release
without a bundle for your platform is refused before anything is downloaded; a
download that does not match its signature is refused before anything is
written. Either way the installed binary stays as it is, and the error says
why. The hint below installs nothing, so it does not need the bundle.

Every command also starts with a one-line hint on stderr while a newer
release is out — devctl's per-command check, with two deliberate differences:

- **A hint, never a gate.** An outdated agentlab runs every command the same;
  nothing waits for you to update.
- **It gives up fast.** The GitHub round trip is capped at two seconds, and
  its answer is cached for an hour under your user cache directory
  (`~/.cache/agentlab/latest-release.json` on Linux,
  `~/Library/Caches/agentlab/` on macOS). A failed attempt is remembered for
  ten minutes, so a machine without internet is not held up on every command,
  and the last known answer keeps being shown meanwhile. A `GITHUB_TOKEN` in
  the environment is used when present; it only lifts GitHub's anonymous
  rate limit.

`AGENTLAB_NO_UPDATE_CHECK=1` silences the hint (`self-update` itself always
works); `dev` builds never check.

## Usage data (telemetry)

Since v0.17.0, agentlab reports **one anonymous usage signal per command you
run** — the same integration [kubectl-gs has](https://docs.giantswarm.io/reference/kubectl-gs/telemetry/)
— so Giant Swarm can see which parts of the lab get used, on which versions
and platforms, and by roughly how many people. That shapes what gets built
next.

One signal contains:

- the command (`agentlab up`, `agentlab platform-test`, …) — never its
  arguments or flags
- the agentlab version (what `agentlab --version` prints)
- operating system and processor architecture
- the library and version that sent it (`telemetrydeck-go/…`)
- a user identifier hash: SHA-256 over OS, architecture, host name, OS user
  and group IDs, user name and the MAC addresses — enough to count distinct
  users, not to identify one (see the
  [library source](https://github.com/giantswarm/telemetrydeck-go/blob/main/telemetrydeck.go))
- a random session UUID, unique per command execution

Nothing from `agentlab.yaml`, `state/`, `certs/`, the cluster, the users, the
models or the model servers is ever sent. Help output (`-h`, `--help`) and
shell completion do not count. The signal goes out in the background while the command runs
and is dropped when the network is unavailable — it never blocks or fails a
command.

Data is stored at [TelemetryDeck](https://telemetrydeck.com/) on servers in
the EU; see their [privacy FAQ](https://telemetrydeck.com/docs/guides/privacy-faq/).

**Opting out:** set `AGENTLAB_TELEMETRY_OPTOUT` to any value, or the
cross-tool [`DO_NOT_TRACK=1`](https://consoledonottrack.com/):

```bash
export AGENTLAB_TELEMETRY_OPTOUT=1
```

Working on the lab itself? `AGENTLAB_TELEMETRY_TESTMODE=1` files your signals
as test data (kept apart from the production numbers in the dashboard) and
logs delivery errors to stderr.

## Layout

```
main.go                        the CLI (cobra): one subcommand per lifecycle step
internal/config/               agentlab.yaml schema, defaults, validation
internal/forms/                the interactive configuration forms (huh)
internal/telemetry/            the one anonymous usage signal per command (TelemetryDeck; see "Usage data")
internal/update/               agentlab self-update + the newer-release hint before every command (go-selfupdate; see "Keeping agentlab current")
pkg/project/                   version, commit, build time: ldflags from make/CI, else Go's VCS build info (`agentlab --version`)
internal/lab/                  everything operational:
  certs.go                       the name-constrained lab CA + 825-day leaf certs
  trust.go                       agentlab trust/untrust (system + NSS stores, via smallstep/truststore)
  up.go down.go                  lifecycle; checksum-stamped Dex apply
  test.go                        RBAC assertions for every configured user
  login.go browser.go            password grant / authorization-code flow
  platform.go platformtest.go    agent platform install + MCP smoke test
  oauthfixture.go                the Auth Required MCPServer fixture + the per-server sign-in proof
  fleetfixture.go                the fake-fleet MCPServers (families × fake clusters, tool-group label) + the label proof
  backstage.go backstagetest.go  Backstage deploy + headless sign-in proof
  postrenderers.go               the lab's per-component postRenderers patches (hostNetwork, sidecar, nodePort, dev images)
  fluxreleases.go                image preload: resolves the component charts from the rendered OCIRepositories/HelmReleases
  observability.go               the lab Prometheus (helm) and the mcp-prometheus HelmRelease through the platform's engine
  templates/                     every manifest, rendered from agentlab.yaml
agentlab.yaml                    your configuration (gitignored; `agentlab configure`)
state/                         rendered manifests, for inspection (gitignored)
  kubeconfig                     the kind cluster's kubeconfig, exported per run — what the lab's own kubectl/helm use
  agent-platform-values.yaml     the chart's values in the lab shape, incl. the postRenderers
.mcp.json                      registers muster as an MCP server for Claude Code
```

Useful passthroughs: `agentlab logs dex|muster|backstage` tails logs; the
effective Dex config is `kubectl -n dex get secret dex-config -o jsonpath='{.data.config\.yaml}' | base64 -d`,
muster's is `kubectl -n agent-platform get cm muster-config -o jsonpath='{.data.config\.yaml}'`.
