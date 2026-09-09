# The agent platform (muster + Kubernetes MCP)

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
  holds. So Helm keeps owning the release: `agentlab platform` is one
  idempotent upgrade-or-install with the kstatus wait through the **embedded
  Helm** — Helm 4's SDK in the binary, no `helm` on the machine required; the
  same `helm upgrade --install … --wait`, no post-renderer, no
  `--force-conflicts` — and the release it writes is a regular one, so `helm
  upgrade` from a shell (`KUBECONFIG=state/kubeconfig`) stays the day-2 tool.
  Never drop the value on a lab: the first upgrade without it makes the
  release self-managed.
- The chart is **pinned** to an exact release, `platform.chartVersion` in
  `agentlab.yaml` (the default is the release this agentlab was verified
  with). The lab never floats; bump the pin deliberately, with a lab run.
  The one exception is deliberate too: the [dev channel](#dev-channel),
  where `platform.chartBranch` follows a branch's newest dev build — and
  still installs an exact version, written into `chartVersion`.

The platform installs as part of `agentlab up` (it is enabled in the default
configuration); on an already-running cluster the steps are also standalone:

```bash
./agentlab platform       # upgrade-or-install of the chart (embedded Helm), then waits for every component
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

## Installing an unreleased chart

Point `platform.chartPath` at a local checkout's chart directory and re-run
`agentlab platform` — nothing needs a release or a push:

```yaml
platform:
  chartPath: /path/to/agent-platform/helm/agent-platform
```

(or `agentlab configure --defaults --chart-path /path/to/agent-platform/helm/agent-platform`;
`--chart-path ""` clears it). `chartVersion` is ignored while it is set, and
the boot says which chart it installed. The directory is read, never written.

## Dev images

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

## Dev channel

The stable channel is the pin above: `platform.chartVersion`, an exact
release. The **dev channel** follows a branch of agent-platform instead —
the dev builds gitsemver publishes for every commit of a branch that has
branch publishing on (`gen.ci.branchPublish` in giantswarm/github): charts
tagged `X.Y.Z-dev.<branch>.<YYYY-MM-DD>.<HH-MM-SS>.h<sha>` next to the
releases in `oci://gsoci.azurecr.io/charts/giantswarm/agent-platform`, each
coupled to the images its commit built. That is how a team works on the
next platform line together before anything is released: every lab installs
the branch's newest build from published artifacts only — no checkout, no
dev images, no local registry.

```bash
agentlab configure --defaults --chart-branch poc/kagent-main
export ANTHROPIC_API_KEY=sk-ant-...
agentlab up
agentlab platform-test && agentlab test && agentlab backstage-test
```

`configure` resolves the branch right away and says what it picked:

```
  chart      agent-platform 3.22.1-dev.poc-kagent-main.2026-09-10.08-12-33.h7f841be (branch poc/kagent-main, dev channel)
  substrate  true (the dev channel's kagent needs it; `configure --substrate=false` overrides)
```

and `agentlab.yaml` carries both:

```yaml
platform:
  chartBranch: poc/kagent-main   # the dev channel: chartVersion follows this branch's newest dev build
  chartVersion: 3.22.1-dev.poc-kagent-main.2026-09-10.08-12-33.h7f841be   # written by the resolver
```

How the resolution works: the branch is spelled the way gitsemver embeds it
(lowercased, anything outside `[a-z0-9]` collapsed to one hyphen —
`poc/kagent-main` is `poc-kagent-main`; a long name is shortened around a
`--` marker, which the filter accepts too), the registry's tags are listed
through the embedded Helm's registry client (anonymously, as pulls are), the
tags whose prerelease is `dev.<that name>.…` are the branch's builds, and the
highest semver among them is the newest — the pick a Flux `OCIRepository`
with `semver: "*-*"` and a `semverFilter` on the branch makes. `up` and
`platform` re-resolve on every run, so the lab follows the branch like Flux
would: a newer build is a new revision of the release on the next
`agentlab platform`. Everything downstream — `render`, the image preload,
the install, `helm -n agent-platform history` — sees the exact version
written to `chartVersion`, as on the stable channel; `render` and the
proofs never resolve.

- **Freezing a build**: `agentlab platform --pin` writes
  `platform.chartPinned: true` and stops re-resolving, so the lab keeps the
  build under test; `--pin=false` follows the branch again. A new
  `--chart-branch` (or `""`, back to the stable channel) always starts
  unpinned.
- **No build yet**: a branch without dev builds is refused with the two ways
  out — the branch's publish has not run (agent-platform needs
  `gen.ci.branchPublish` and a commit on the branch), or pin a tag by hand.
- **The fallback that needs no resolver**: `platform.chartVersion` accepts a
  full dev tag as it is — `agentlab configure --chart-version
  3.22.1-dev.poc-kagent-main.2026-09-10.08-12-33.h7f841be` validates and
  Helm pulls that exact tag (look it up with `crane ls
  gsoci.azurecr.io/charts/giantswarm/agent-platform | grep -- -dev.poc-kagent-main`).
  The lab then does not follow the branch, and Substrate needs
  `--substrate` explicitly.
- `chartBranch` and `chartPath` are mutually exclusive: a local chart has no
  builds to follow.
- The dev-tag schema lives in one place, `devTagFilter` in
  `internal/lab/chartbranch.go`. When gitsemver ships the RFC's successor
  schema (`X.Y.Z-b<crc32 of the branch>t<timestamp>c<sha>`), that function
  switches and nothing else changes.

Component channels: agentlab sets nothing per component. The meta chart's
dev builds carry their siblings' channels in their own values (the branch's
`components.<name>.semverFilter`), so selecting the meta chart's dev channel
selects the whole line.

### Substrate

The dev channel's kagent (kagent main, API v2) runs every agent as an actor
on [Substrate](https://github.com/kagent-dev/substrate) — sandboxed (gVisor)
worker pods of a `WorkerPool`, an API server, a per-node agent (`atelet`) and
the actors' ingress/egress data plane (`atenet`) — and its controller does
not start without it. The lab installs Substrate as cluster infrastructure
ahead of the platform when `platform.substrate.enabled` says so; unset, the
knob follows the channel — on with `chartBranch` while agents are on, off on
the stable channel, whose released kagent ignores it — and `configure
--substrate[=false]` pins it either way.

What `agentlab up` (and `platform`) does, idempotently:

1. checks the apiserver serves `certificates.k8s.io/v1beta1` — the
   `PodCertificateRequest` and `ClusterTrustBundle` gates the lab's kind
   config turns on for every cluster (inert for the released platform). A
   cluster created by an earlier agentlab lacks them, and feature gates are
   fixed at `kind create`: that is `agentlab down && agentlab up`, said
   before anything installs;
2. creates the bootstrap the chart mounts but does not render: the four
   CA/JWT pool Secrets (`service-dns-ca-pool` and `pod-identity-ca-pool` in
   `podcertificate-controller-system`, `actor-id-jwt-pool` and
   `actor-id-ca-pool` in `ate-system`) — never regenerated once they exist,
   a rotated root would orphan every certificate issued from it — the
   `actor-id-ca-certs` trust anchor derived from the actor-id pool, and
   `ate-api-authentication`, ate-api-server's authentication config
   pointing at the cluster's own ServiceAccount issuer. Upstream does this
   with `kubectl-ate admin make-ca-pool` / `make-jwt-pool` and a shell step
   between two `helm install`s; the lab embeds a Go port of the two commands
   (`substratepools.go`, from substrate 0.0.26) and creates everything first,
   so one waited install suffices (HACKS.md U22);
3. installs `substrate-crds` and `substrate` 0.0.26 from
   `oci://ghcr.io/kagent-dev/substrate/helm` into `ate-system` with the
   chart's own values (`state/substrate-values.yaml`), the images
   side-loaded like the platform's plus the gVisor worker image the
   `WorkerPool` names, and waits for ate-api-server, atelet and atenet;
4. checks the `SandboxConfig gvisor-default` the chart ships is there. The
   `WorkerPool` and the `Harness`es come with the dev channel's kagent.

`agentlab platform-down` uninstalls Substrate after the platform (whose
teardown deletes kagent's `WorkerPool` through finalizers ate-controller has
to be running for). Substrate's bundled PostgreSQL requests 1 CPU / 1 GiB,
so the dev channel's floor is one CPU and about 2 GiB above the stable lab's
— see [Docker resources](getting-started.md#docker-resources).

**Proofs on the dev channel.** `platform-test`, `test` and the sign-in half
of `backstage-test` run unchanged (platform-test expects a kagent scrape
target only while a controller ServiceMonitor exists — the dev channel's
kagent serves no metrics listener, and the lab renders none for it). The
agent proofs — `agents-test`, `toolsets-test`, `models-test`'s agent turn,
`backstage-test`'s agents pages — drive the released kagent's API (Agent CRs
delivered as HelmReleases); kagent API v2 has `AgentTemplate`s and
`Harness`es instead, so on the dev channel they do not apply until the
proofs dispatch on the API the cluster serves, the dev channel's next step.

## The request path

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

## Signing in to a downstream server (muster as OAuth client)

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

## The fake fleet and the tool-group label

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

## Toolsets (declared tool access)

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
data the page reads — see [The muster plugin](backstage.md#the-muster-plugin).

## Lab-specific deviations from a real management cluster

| What | Why |
|---|---|
| `gatewayApi.gateway.create: true` — the chart-owned agentgateway Gateway **is** the public edge | A real MC fronts the platform with the cluster's shared Envoy Gateway; kind has none, so the data-plane Gateway itself terminates TLS for `*.127.0.0.1.nip.io` with the lab's wildcard cert (the chart's own standalone/kind mode, `ingress.mode: agentgateway-muster`). The Gateway API CRDs are embedded in the binary and applied before the install; a lab-owned NodePort Service pins the edge onto the kind port mapping (HACKS.md U10), and a CoreDNS rewrite points `*.127.0.0.1.nip.io` at it inside pods (outside, nip.io answers 127.0.0.1 by itself). |
| One Dex client, `agent-platform` | The chart's `global.identity` convention: muster and Backstage share the client, so a Backstage-forwarded token natively carries an audience muster trusts. The extra `dex-k8s-authenticator` client exists only as the cross-client audience target Backstage's GS auth provider requests by default. |
| `networkPolicy.enabled: false`, `kyvernoPolicies.enabled: false` | The chart's own policy objects. No Cilium and no Kyverno in kind, so both would render CRs whose API groups the cluster does not serve. |
| The muster/kagent ServiceMonitors, valkey PodMonitor and muster PrometheusRule follow `platform.observability` | Without it there is no Prometheus Operator, so none of those CRDs exist and the releases fail to render. With it they are scraped by the lab Prometheus — whose selectors are opened up (`*NilUsesHelmValues: false`) because upstream's default selects only monitors carrying the kps release label, and the platform's monitors come from other releases. Flipping observability rolls the muster pod once (the toggle changes its metrics-exporter env). |
| `platform.observability`: the GS kube-prometheus-stack constituent installed directly, Prometheus server re-enabled, instead of the observability-bundle | The bundle is MC-shaped (Flux HelmReleases with a hardcoded remote kubeconfig, Alloy → Mimir, no local PromQL endpoint). See [Observability](observability.md). |
| `muster.muster.oauth.mcpClient.enabled: true` + the `lab-oauth-fixture` MCPServer | The chart leaves muster's OAuth *client* role — the proxy behind `core_auth_login` and the portal's Sign in — off; real installations turn it on, and without it no per-server sign-in can be exercised. The fixture is the one `Auth Required` downstream to sign in to (muster's own protected `/mcp`); see [Signing in to a downstream server](#signing-in-to-a-downstream-server-muster-as-oauth-client). |
| The fake-fleet MCPServers (`<family>-lab-01`, `<family>-lab-02`) with `agent-platform.giantswarm.io/tool-group: infrastructure` | One cluster and a family-less bundled `mcp-kubernetes` look nothing like the federated fleet the portal's server groups, fleet coverage and Tools step are built for. Six `Auth Required` family members fake it, carrying the tier label the fleet charts stamp; see [The fake fleet and the tool-group label](#the-fake-fleet-and-the-tool-group-label). |
| `muster.rbac.{mcpServerEditor,workflowEditor}.subjects` → `oidc:platform-admins` | The chart binds muster's editor Roles to Giant Swarm's admin groups, which do not exist here. Rebound to the lab's own admin group (`--oidc-groups-prefix=oidc:`, same spelling as the lab RBAC). Lists replace, so the GS groups are dropped. |
| muster patched to `hostNetwork` + `maxSurge: 0` | Same issuer trick as the apiserver and Backstage. `maxSurge: 0` because two hostNetwork pods cannot both bind `:8090` on a one-node cluster. A Kustomize strategic-merge patch in `components.muster.postRenderers`, which the chart forwards to muster's `HelmRelease` and the bundled helm-controller applies over the muster chart's render. |
| The `dex-localhost` sidecar on mcp-kubernetes, model-manager, agent-manager and mcp-prometheus | Those servers validate the forwarded Dex token themselves and must reach the issuer URL `https://localhost:32000/dex`, but all listen on `:8080` and cannot share the host network. A socat sidecar on the pod's own loopback forwards `:32000` to the Dex Service (HACKS.md U13) — a `postRenderers` patch on each component, and on the lab's own mcp-prometheus `HelmRelease`. |
| `components.kagent.enabled` from `platform.agents`, `controller.auth.mode: unsecure`, kagent ServiceMonitor + OTel off | Agents are part of what the lab tests, so kagent is on by default (the chart defaults it off) but optional — `platform.agents: false` skips the runtime. `unsecure` because the GS `trusted-proxy` mode assumes a JWT-validating agentgateway in front; no Prometheus Operator / OTLP gateway in kind. See [Agents (kagent)](agents.md). |
| `kagent.ui.service.type: NodePort`, nodePort 30880 pinned by the kagent `postRenderers` patch | On a real MC the UI sits behind the agentgateway edge; this lab publishes it through the kind port mapping instead (host side `platform.agentsPort`, default 8081). The chart's Service template renders no `nodePort` field, so the fixed node port is a patch (HACKS.md U9). |
| `components.flux.enabled: true`, `gitops.self.enabled: false` | The lab shape (see [The agent platform](#the-agent-platform-muster--kubernetes-mcp)): a management cluster runs its own Flux and installs the chart through it; the lab has none, so the chart brings the engine — and must not adopt its own release, because the lab installs charts and images that are not released. |
| The chart pinned to an exact release (`platform.chartVersion`) | Component versions are the chart's own ranges, resolved by its Flux at reconcile time (the fleet's dogfooding track). The chart itself never floats in the lab: two runs install the same thing, and a bump is a deliberate edit with a lab run behind it. |
| Substrate 0.0.26 installed by the lab ahead of the platform, bootstrap included (`platform.substrate.enabled`, implied by the dev channel) | The dev channel's kagent runs its agents as Substrate actors and cannot start without it, and Substrate is not a meta-chart component yet: its CA/JWT bootstrap is imperative (`kubectl-ate admin make-ca-pool`). The lab ports the two bootstrap commands, creates every object before one waited install and needs the apiserver gates its kind config turns on. See [Dev channel](#dev-channel). |
| `mcp-prometheus` as a lab-rendered Flux `HelmRelease` | The one release the lab installs outside the chart rides the same engine, as the same tenant identity, so its lab-only sidecar is a `postRenderers` patch like the others and there is exactly one Helm writer (the embedded Helm, for the chart) and one Flux engine on the cluster. |

## Platform gotchas

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
  the embedded Helm's waited uninstall runs the chart's pre-delete hooks — delete the
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
  adopts it, and the uninstall (the ordered teardown) removes it with the
  connectivity release. The lab creates no namespace of its own for kagent.
- **Kubernetes tools carry the server-name prefix.** The chart's bundled
  `mcp-kubernetes` MCPServer declares no muster *family*, so its tools use
  per-server prefixing: `call_tool(name=x_mcp-kubernetes_list, arguments={...})`. The fake fleet's members are the lab's only family servers and they stay
  `Auth Required`, so no `x_kubernetes_*` family tools appear in a session.
  (Real fleet installations register per-cluster servers with a `kubernetes`
  family and a `management_cluster` instance argument instead.)
- **Tool results are double-wrapped.** `result.content[0].text` is JSON whose
  `content[0].text` is the actual payload — two decode hops.
