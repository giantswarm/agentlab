# Backstage

Backstage deploys **with the platform** — the chart's `backstage`
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
explicitly](tls.md)). Without it, the first click
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

## Users need no catalog entity

RHDH's `emailLocalPartMatchingUserEntityName` resolver refuses a login it cannot
map onto a `User` entity. Giant Swarm's resolver only consults the catalog when
Dex reports `federated_claims.connector_id` of `giantswarm-ad` or
`giantswarm-github`. This lab's static-password connector reports `local`, so it
falls straight through to `email.split('@')[0]` and issues
`user:default/<localpart>` regardless. The catalog entities the lab renders
(from your configured users) exist only so the users and groups show up as real
things in the UI.

## The muster plugin

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
[`trustedPeers` points the other way round](identity.md#trustedpeers-points-the-other-way-round).

The MCP servers page groups servers into **Agent Platform**,
**Infrastructure** and **Registered servers** by the tool-group label the
shipping chart stamps on the CR (see [The fake fleet and the tool-group
label](platform.md#the-fake-fleet-and-the-tool-group-label)); a family's members
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

## The agent create flow

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

## Backstage gotchas

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
