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
  `agentlab-backstage-catalog`): the users/groups entities and an in-memory
  sqlite database — no Postgres needed for a lab portal.
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
  MCP servers page groups (11 CRs via /api/kubernetes/proxy):
    Agent Platform      2 rows: agent-manager, model-manager
    Infrastructure      4 rows: capi, kubernetes, prometheus, mcp-kubernetes
    Registered servers  2 rows: lab-oauth-fixture, mcp-prometheus
  fallback without any agent-platform.giantswarm.io/tool-group label: 8 rows, all under Registered servers; 9 of 11 CRs carry the label today
```

## The agent create flow

**Agent Platform → Agents → New agent** (`/agents/new`) composes an agent from
a form (installation, name, model, system prompt), a skills step and a tools
step, and its Deploy button is **agent-manager's `create_agent`** called
through the muster plugin's backend (`POST /api/muster/call`) with the
signed-in person's own forwarded Dex id_token — the same tool an MCP session
drives. The portal composes no manifest: the review page is agent-manager's
`validate_agent` dry run rendered verbatim (the `OCIRepository` tracking the
Generic agent chart at **`1.x`**, the `HelmRelease` as the tenant
ServiceAccount `kagent-flux`, the values it composed), and Deploy applies
exactly that. Skills come from the gs backend's discovery
(`GET /api/gs/agent-skills?repoUrl=…`), every entry pinned to the head commit
the listing was read at, so a selected skill becomes a chart `skills[]` entry
`{name, path, git: {url, commit}}` — never a branch. There is no scaffolder
task, no `kube:apply` and no OIDC token minted by the portal any more; the
`agent-deployment` Template the 0.x portal drove is gone from the lab catalog
(HACKS.md U7).

What lands is a Flux `HelmRelease` per agent in the ModelConfig's namespace
(`kagent`) next to one shared `OCIRepository/agent` per namespace, reconciled
by the platform chart's bundled engine as `kagent-flux` (a ServiceAccount and
RoleBinding the connectivity component renders from
`kagent.fluxServiceAccountName`, named into agent-manager's
`flux.helmReleaseServiceAccount`); the render is a `kagent.dev/v1alpha3`
`AgentTemplate` the platform Harness admits and compiles into a golden
snapshot, plus the agent's own `RemoteMCPServer` carrying its toolset — see
[Agents](agents.md). RBAC applies to the write as the person: `platform-admins`
deploy, a `viewers`-group user gets agent-manager's `forbidden: …`.

## The Agent Platform proof

`agentlab backstage-test` drives the Dev Portal's Agent Platform pages on
kagent API v2 the way a person does, through the routes the browser uses and
with each user's own forwarded token, after the sign-in half above. Nothing it
asserts is composed in the lab: the wizard's spec goes to agent-manager, and
what the proof checks is what the portal shows.

**The create path** (as the first `platform-admins` user):

- skill discovery for `https://github.com/giantswarm/agent-skills` — every
  entry carries the listing's head commit as a full id;
- `get_info` and `list_model_configs` through the portal;
- the dry run (`validate_agent`): valid, mode `create`, the `OCIRepository` at
  `1.x`, the `HelmRelease` as `kagent-flux`, `values.agent.harness` = the
  platform Harness `get_info` names, no `values.agent.runtime`, the toolset as
  composed, the skill pinned to the discovered commit;
- Deploy (`create_agent`) through the same route, then the shared readiness
  wait of [Agents](agents.md): `requestedBy` is the person, agent-manager's log
  says `caller=<person>`, the `HelmRelease` carries **exactly** the dry run's
  values as `kagent-flux` next to `OCIRepository/agent` at `1.x`, the
  `AgentTemplate` is Ready on the platform Harness with the admission label,
  the display-name and icon-url annotations and the skill, its
  `RemoteMCPServer` carries `X-Muster-Toolset`;
- `get_agent_status` through the portal says `ready` on that Harness (the
  detail page's poll); a second create of the name answers `conflict: …`, a
  viewer's create `forbidden: …`.

**The agents list** (as every user), read as the portal reads it — the
`agenttemplates` and `remotemcpservers` of the installation through
`/api/kubernetes/proxy` with the person's token: a `platform-admins` user sees
the agent with its readiness (the portal's derivation from
`status.harnesses[]`), its toolset off the carrier and the owning
`HelmRelease` (the Flux provenance label). A developer or viewer meets the
apiserver's **403** and the portal shows the installation as unreadable —
the lab's RBAC (`rbac.yaml.tmpl`) binds `platform-admins` to `cluster-admin`,
`viewers` to `view` and `developers` to `edit` in `demo`, none of which reads
`kagent.dev`; the 0.10 line's `agents.kagent.dev` was never readable for them
either, so this is no loss of the migration and the proof asserts it as it is
(a change of the lab's grants fails here by name). Fleet installations grant
reads differently; what a non-admin sees there is the migration rehearsal's
question, not this lab's.

**The chat** (as the admin, every other user as the boundary):
`GET /kagent/installations` offers the installation and `GET /kagent/me`
resolves the person; a session (the person's `AgentInstance`) is created on
the first message with a `requestId` and a repeat answers the same instance;
the turn streams (`…/messages/stream`, SSE frames of A2A v1 events) and the
agent's tool call reaches muster as the person (muster's
`forwarded_id_token_accepted` audit event names them, in the turn's window);
the instance is quiescent after the turn — the gateway gives the worker back
and the runtime is a snapshot, the logical state still `READY` (`SUSPENDED`
is the explicit suspend) — and the next message resumes the conversation
intact (a codeword told in turn one, recalled in turn two); rename, the tasks list,
`session-states` and `session-usage` read; another user does not list the
session and reads it as 404; the delete leaves nothing.

**HITL and Stop**, on a second agent the lab writes itself with
`muster.requireApproval: true` (chart ≥ 1.1.0; the proof asks the shared
`OCIRepository` to fetch a newer release when it holds an older one): a tool
prompt pauses the task in `input-required` with a `tool_approval_request` under
the HITL extension; `POST …/answer` approving it, naming the task, resumes the
same task — rounds until it completes; then a long turn is cancelled
server-side from the task the stream named (`POST …/tasks/:taskId/cancel`),
the task settles `canceled`, and the following turn completes.

**The edit path** (the detail page's kebab, as the admin): `get_agent` reports
the pins written at create; `validate_agent{update}` and `update_agent` with a
new description change exactly `agent.description`; `validate_agent{update,
refreshSkills}` and `update_agent{refreshSkills}` pin every git skill to
`list_skills`' head of the repository and nothing else (discovery read the
same head, so the pin stays); the agent is Ready after; a viewer's
`update_agent`/`delete_agent` are `forbidden: …`; `delete_agent` of the agent
while the fixture still needs the chart keeps `OCIRepository/agent` and says
which release for; `delete_agent` of the fixture, the last release the proof
holds, records the source's fate. A GitOps-owned release (refused as
`conflict: …`) has no fixture in the lab and is not asserted.

Both agents and every session are removed on every path, leftovers of an
aborted run first. Not provable headlessly and out of scope: the browser's
versioned persisted query cache (a stale v1alpha2 entry never renders) — that
is the frontend's own guard, exercised by the plugin's tests, not by a portal
API the lab can drive.

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
  the key reaches the pod from a Secret in Backstage's namespace, injected as
  `optional:` so the pod boots without it.
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
