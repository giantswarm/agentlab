# Agents (kagent API v2)

The platform's agent runtime is an **optional component**, on by default
(`platform.agents` in `agentlab.yaml`; headlessly:
`agentlab configure --defaults --agents=false`). When enabled, the chart's
**kagent** component installs kagent API v2 (`api.kagent.dev/v1alpha3`) with
the platform's Substrate: every agent is an `AgentTemplate` paired by an
`Agent` of the same name with the platform's Go ADK **Harness** `kagent`
(`spec.templateRef`, `spec.harnessRef`; the connectivity chart renders the
Harness, digest-pinned runtime image, `KAGENT_PROPAGATE_TOKEN` on), compiled
by the controller into a golden snapshot and run as a Substrate actor; a
conversation is a `Session` of the Agent, its turns A2A messages through the
edge as the signed-in person (`docs/platform.md` "Dev channel" and
"Substrate"). There is no per-agent Deployment or pod; the `Agent` of this
line is a pairing, not the 0.x runtime CR of the same kind name.

## One agent = one HelmRelease of the agent chart (1.x)

Agent delivery runs through Flux on every installation. An agent is a
`HelmRelease` of the Generic [`agent` chart](https://github.com/giantswarm/agent)
in the `kagent` namespace, rendering from the namespace's shared
`OCIRepository agent` that tracks the chart at `1.x`; the platform chart's
bundled engine renders it as the tenant ServiceAccount `kagent-flux` (the
connectivity chart's `kagent.fluxServiceAccountName`). One release renders:

- the `AgentTemplate` named after the release — `spec.description`,
  `spec.systemPrompt`, `spec.modelConfig.name`, `spec.skills[]`, the
  annotations `ui.giantswarm.io/display-name` and `ui.giantswarm.io/icon-url`;
- the `Agent` named after the release, pairing that template
  (`spec.templateRef`) with the Harness `agent.harness` names
  (`spec.harnessRef`, default `kagent`); an Agent whose Harness does not
  exist is created and never becomes Ready (`ResolvedRefs=False`);
- unless the toolset is exactly `["preset:none"]`, one `RemoteMCPServer`
  named after the agent that points at muster (`muster.url`) and carries the
  toolset as the static `X-Muster-Toolset` header (`spec.headersFrom`, the
  selectors joined by `,`), labelled `kagent.dev/discovery: disabled`; the
  template binds it as its tools. A release without a `toolset` value renders
  the server without the header — implicit full access, the way agents that
  predate toolsets look.

The HelmRelease's `values.toolset` is the stable anchor of the toolset; skills
are `{name, git: {url, commit}, path}` or `{name, oci: <ref@sha256:…>}`
entries pinned at write time (the CRD refuses a branch, a tag or a short SHA).

Two writers put the same two Flux objects there, and the proofs use both:

- **agent-manager as the signed-in person** — the platform path. Backstage's
  create flow and every MCP client call `x_agent-manager_create_agent` through
  muster; agent-manager pins the skills (`list_skills` reports the head
  commits; `update_agent` with `refreshSkills` re-pins), validates the values
  against the chart's `values.schema.json`, writes the HelmRelease (and the
  OCIRepository when the namespace has none) with the field manager
  `agent-manager` as the caller (`requestedBy`), and reads readiness back from
  the Agent's status (`get_agent_status`: `ready | progressing | failed` —
  the last with the condition's message).
- **a HelmRelease applied directly** — the operator's kubectl path, the same
  objects by hand.

In the lab both are one helper (`internal/lab/agent.go`): `readyAgent`
writes an `agentSpec` through an `agentWriter` (`agentManagerWriter` on a
muster session, `portalAgentManagerWriter` through the portal's muster
backend, `helmReleaseWriter` directly) and waits with `waitAgentReady` until
the Agent, on the platform Harness, reports Ready on the desired revision —
or fails fast with the reason when the release's render is refused, an Agent
condition is False for good, or the Agent references another Harness.
`removeAgent` takes the release and whatever render is left. Every agent
proof — `agents-test`, `toolsets-test`, `models-test`'s turn,
`backstage-test`'s own agent — creates its agents this way; `skills-test`
alone writes a raw `AgentTemplate` and its `Agent`, on purpose: it is the
golden-boot diagnostic below the chart (a private fixture's `credentialRef`,
the control boot without the skill).

## The model

Agents run against a **default `ModelConfig`** that the connectivity chart
renders from the lab's `aiModel` setting (`agentlab.yaml`, default
`claude-sonnet-4-6` — the BOM's own default), referencing the Secret
`kagent/kagent-anthropic` (key `ANTHROPIC_API_KEY`). The API key is a **real
credential**, so unlike the lab's throwaway passwords it never enters
`agentlab.yaml` or the rendered `state/` files. The Secret exists after every
`agentlab up` and `agentlab platform`, so the ModelConfig resolves on a fresh
lab too (the run waits for `ResolvedRefs`), from the first of (the portal's
copy for Backstage's AI chat, `agent-platform/agentlab-anthropic`, by the same
rules; see [Backstage](backstage.md)):

- **A recorded source** (`aiKey.source` in `agentlab.yaml`; `agentlab
  configure --ai-key-source op://<vault>/<item>/<field>`, or
  `<file>#<path>` of a SOPS file): a *reference*, never a value. Every `up`
  and `platform` hand it to the operator's secret tooling, `beekeeper secret
  copy <source> --to-secret kind-<clusterName>/kagent/kagent-anthropic/ANTHROPIC_API_KEY`
  ([beekeeper](https://github.com/giantswarm/beekeeper), on PATH; `configure`
  refuses a source without it), which reads the value in its own process and
  writes it straight into the lab's apiserver — agentlab sees the value's
  length, never the value. A recreated lab (`down`, `up`) carries the key
  again without a manual step; a Secret already holding a real key is left
  alone (delete it and re-run to place the source again). A source that
  cannot be placed — the tooling missing, the vault not answering, no lease
  on the lab — fails the run naming the Secret and the source: a lab whose
  configuration says where the key is never runs on a placeholder.
- **`$ANTHROPIC_API_KEY`** on the host, when no source is recorded: the
  Secret is created from it in-process (left alone once it holds a real key;
  delete it and re-run to rotate).
- **A placeholder** otherwise, as for an [extra model](models.md) without a
  key: the ModelConfig resolves and agents start, but every turn on it fails
  at Anthropic until the key is placed — the run says so, loudly, with the
  three ways: the `beekeeper secret copy … --to-secret` call above (it
  replaces the placeholder; the next run leaves the real key alone), the
  source recorded, or the variable exported and `agentlab platform` re-run.
  Without any of those, run the agent proofs on a host model:
  [Agent proofs without an Anthropic key](models.md#agent-proofs-without-an-anthropic-key).

A person without the tooling places the key with
`kubectl --context kind-<clusterName> -n kagent create secret generic
kagent-anthropic --from-file=ANTHROPIC_API_KEY=…` (over the placeholder:
`delete secret` first).

The same key powers Backstage's AI chat (see
[Backstage gotchas](backstage.md#backstage-gotchas) for its no-key behavior).
Further models — the host model servers through model-manager,
`platform.extraModels` — are their own ModelConfigs; see [Models](models.md).
Without a key, the agent proofs run on a host model through `--model-config`:
see [Agent proofs without an Anthropic key](models.md#agent-proofs-without-an-anthropic-key).

## The GitHub token

Two consumers of the lab resolve skills through GitHub's REST API: the
portal's skill discovery (`GET /api/gs/agent-skills?repoUrl=…`, the create
wizard's skills step) and agent-manager (`list_skills`, the commit
`create_agent` pins a branch to, `refreshSkills`, the migrate Job of the
cut-over). Without a token GitHub allows 60 requests an hour **per egress
address**, and the kind cluster shares this machine's — a handful of
`backstage-test` runs within an hour exhaust it, after which discovery comes
back truncated (the proof refuses that by name) until the window resets.

A token lifts that, from a recorded source (`githubToken.source` in
`agentlab.yaml`; `agentlab configure --github-token-source
op://<vault>/<item>/<field>`, or `<file>#<path>` of a SOPS file): every `up`
and `platform` hand it to `beekeeper secret copy --to-secret`, which writes the
token in its own process; agentlab never reads the value, and a placement that
fails fails the run. agentlab never reads `$GITHUB_TOKEN` either: a token
exported in its environment reaches no consumer. Without a source `discover`
and `platform` name the Secret the lab lacks, `agentlab-github-token` (key
`GITHUB_TOKEN`) in `agent-platform`.

`agentlab up`/`platform` place **or update** the Secret `agentlab-github-token` (key `GITHUB_TOKEN`) in
`agent-platform` — the portal takes it through `extraEnvVarsSecrets` and the
lab's app-config overlay (`integrations.github[].token: ${GITHUB_TOKEN}`),
agent-manager through the chart's `skills.github.tokenSecret` — and in
`kagent` for the migrate Job (`agentManager.migration.githubToken`), which then
call GitHub authenticated (5000 requests an hour; the rate-limit headers say
so). A fine-grained token with public read access is enough; a token that
reads a private skill repository lets agent-manager resolve skills from it.
The token travels vault -> Secret through beekeeper only and never enters
`agentlab.yaml`, `state/` (the rendered values carry the Secret's *name*), a
log line or a process's argv. Re-running after a rotation in the vault
rotates the Secret.

**A skills GitHub App** is the way a lab reaches GitHub without any token: agent-manager's chart takes a read-only App (`skills.github.app.secretName`) and resolves skills with its installation tokens, at the App's rate limit. `agentlab configure --github-app-id <id> --github-app-installation-id <id> --github-app-private-key-source op://<vault>/<item>/<field>` records it as `githubApp` in `agentlab.yaml` (the ids are public; the key is a reference). A lab that already records the workspaces GitHub App (`platform.workspaces.github`, docs/workspaces.md) records only its installation, `agentlab configure --github-app-installation-id <id>`: agent-manager then resolves skills as that App, its id and private key source taken from the workspaces entry. With the workspaces App's OAuth client recorded too (`clientId` and `clientSecretSource`, or `githubApp.clientId` and `githubApp.clientSecretSource` for an App of its own), the portal's skill discovery calls GitHub as the same App: every `up` and `platform` write the Secret `agentlab-portal-github-app` (`GITHUB_APP_ID` and `GITHUB_APP_CLIENT_ID` applied, `GITHUB_APP_PRIVATE_KEY` and `GITHUB_APP_CLIENT_SECRET` placed by beekeeper), the portal's `extraEnvVarsSecrets` take it, and the overlay's `integrations.github` lists the App with `publicAccess: true`, so a public repository of any owner gets an installation token. Every `up` and `platform` apply the ids to the Secret `agentlab-github-app` in `agent-platform` and hand the key to `beekeeper secret copy --to-secret` (key `private-key`, never read by agentlab; a failed placement fails the run), and the values name the Secret for agent-manager. The chart refuses the App together with `tokenSecret`, so `githubApp` and `githubToken.source` are not recorded together.

Without either the lab is as before: the values name no Secret, the
consumers call GitHub unauthenticated, and a Secret an earlier run created
stays — unreferenced — until `agentlab down` (a run that merely lacks the
export never deletes a credential; `kubectl -n agent-platform delete secret
agentlab-github-token` does). `backstage-test`, `agents-test` and the
rehearsal print the window before their first skill-resolving step
(`GitHub API window (unauthenticated: this machine's shared window): 12 of 60
requests remaining, resets 14:32:10 CEST`) and, when it holds fewer requests
than the run spends (`backstage-test`: 30, its discovery and the create pin),
wait once until the reset instead of failing on a truncated listing. The
wait is skipped only when every consumer the run reads through carries a
credential: agent-manager for `agents-test`, agent-manager and the portal
for `backstage-test`.
`backstage-test` runs its `refreshSkills` steps (E4/E5) only when
agent-manager's Deployment carries a GitHub credential; without one it skips
them with a verdict naming the anonymous rate limit.

## Editing an agent applied from git

An agent whose HelmRelease a Flux Kustomization applies carries the
Kustomization's provenance labels (`kustomize.toolkit.fluxcd.io/name` and
`/namespace`); agent-manager reports it `managed: gitops` and refuses every
live write with `gitops_owned`, because the next reconciliation would undo
it. Such an agent is changed with a pull request: `update_agent` in
`mode: commit` rewrites the release's file in agent-manager's directory of the
owning repository and opens the pull request as the person. The portal's edit
page dry-runs every change with `validate_agent` (`update: true`) first and
offers the write only once the dry run is valid, so the dry run takes `mode`
too and answers as the write would: `gitops_owned` in mode apply, valid with
the composed manifests in mode commit.

`agentlab agents-gitops-test` is that edit, headless and as the person: the
platform's agent-manager composes a release, the proof applies it with the
Flux labels and reads it back `managed: gitops`; the platform's own
agent-manager (not pinned to a GitHub App) answers the change's dry run
`gitops_owned` in mode apply and `unsupported` in mode commit, as its writes
do; then, against a temporary copy of the agent-manager release pinned to the
lab Dex as its GitHub App and to the fake GitHub API (the shape `models-test`
uses, see [Models](models.md) "Dry runs, GitOps-owned objects and commit
mode"), `validate_agent` in mode apply still answers `gitops_owned`, `force`
or not, `validate_agent` in mode commit answers a valid update whose
HelmRelease carries the change and opens nothing, and `update_agent` in mode
commit opens exactly one pull request authored by the person whose file is
the dry run's manifest byte for byte, while the live release's
`resourceVersion` stays as it was and a mode apply write is still refused.
The commit target is named (`repository`, `branch`, `path`): the lab's
engine runs no kustomize-controller, so no Kustomization resolves it from
the release's labels here. The agent, the copy, the fake and its Service go
away at the end of the run.

## Organizations as tenants

`platform.tenants` (`agentlab configure --tenants`) renders the chart's
`tenants.organizations.enabled: true`: agent-manager's tenant reconciler runs
(`--multi-tenant`) and the chart admits the agent workers of every namespace
labelled `giantswarm.io/organization`. The reconciler keeps in each such
namespace, always, the agent chart's OCIRepository `agent`, the agents' Flux
ServiceAccount and its RoleBinding (`kagent-flux`) and, when the platform's
network policies are on, their copies — labelled
`agent-manager.giantswarm.io/tenant-object=namespace` — and, while the
namespace holds an agent, copies of the platform Harness, its WorkerPool and
the pool's ServiceAccount, the default ModelConfig and its key Secret,
labelled `…/tenant-object=agents`. The switch needs the agents runtime and a
chart that carries it (giantswarm/agent-platform#1037 and #1050; until a
release does, a dev build through `--chart-branch`).

Substrate's credential provider reads a namespace's Secrets only where its
policy allows it. Until the chart selects the organizations' namespaces by
label (giantswarm/agent-platform#1041, Substrate 1.12), the lab's values list
the two namespaces `tenants-test` uses by hand in
`substrate.credentialProvider.namespacePolicies` — each read by the atespace
of its own name, and by `ate-golden` — next to the chart's two entries. The
list goes with #1041. A `platform.valuesFiles` overlay that sets that list
replaces the lab's.

`agentlab tenants-test [email email]` is the proof
(giantswarm/agentlab#739). It creates `org-agentlab-tenants-a` and
`org-agentlab-tenants-b`, labelled, and binds one person to
`agent-platform:editor` in each (default: the first two users outside
`platform-admins`, `dev@lab.local` and `viewer@lab.local`; a platform admin is
refused, being cluster-admin). It calls agent-manager's REST API (`/api/v1`,
on a port-forward to its pod) as each person with their Dex token (audiences
`kubernetes` and `agent-platform`) and checks:

1. before an agent, each namespace holds only the always-kept objects;
2. a person's create in the other organization is refused (403; under a name
   that does not exist there, so a person with the lab's cluster-wide `view`
   reaches the create and does not get 409); the create
   in their own (201, `requestedBy` the person) makes the reconciler copy the
   Harness, the WorkerPool with agent-manager's
   `--tenant-worker-pool-replicas`, its ServiceAccount, the ModelConfig and
   its Secret;
3. the WorkerPool's workers are Running and ready, the agent `ready`, and a
   turn as the person (through the edge) answers, the credential provider
   reading for the namespace's atespace from that namespace only (its log);
4. the same in the second organization;

   then, with both organizations' agents up and the platform tenant's
   (`agentlab-tenants-platform`, created by the admin in `kagent` at the
   start), the authorizer through the edge (steps E) and in Slack (steps S),
   as the tenant users of [The tenant model](identity.md#the-tenant-model)
   — the first user in group `tenant-a` and in `tenant-b`, `alice@lab.local`
   and `bob@lab.local` after `agentlab configure --tenants`:

   - E1. the first tenant user, whose group is bound nowhere yet, lists
     (`ListAgents` with no namespace) the platform tenant's agents only;
   - E2. each group bound to `agent-platform:user` in its organization
     (RoleBinding `agentlab-tenants-test-users`, subject `oidc:<group>`):
     each person lists exactly their organization's agents and the platform
     tenant's, read back against the cluster;
   - E3. the first user's `CreateSession` and first A2A message on the second
     organization's agent are `PermissionDenied`, no Session of it is made
     (every creator's, listed by the admin) and the credential provider reads
     nothing for its atespace; on their own organization's agent the
     `CreateSession` succeeds;
   - E4. the user's `CreateAgent`, `UpdateAgent` and `DeleteAgent` in their
     own organization are `PermissionDenied` and its agent is unchanged;
   - E5. the first editor creates, changes (a label) and deletes an Agent in
     their organization through the edge (on a Harness that does not exist,
     so it never boots); `CreateAgent`, `UpdateAgent` and `DeleteAgent` in the
     other organization are `PermissionDenied` and its agent is unchanged;
   - E6. `ListSessions` with `all_creators` is `PermissionDenied` for both
     users and answered for the admin;
   - S. klaus-gateway on the host (klaus-gateway-test's shape: `--gateway-image`
     or `--gateway-binary`, default the component's release; it needs
     giantswarm/klaus-gateway#486, 4.10.0 or later), both users linked:
     S1. `@bot agents` as both at the same time: each roster lists their
     organization's agent labelled `organization <name>` and the platform
     tenant's labelled `platform`, nothing of the other organization, and the
     first person's roster is the same when asked again; S2. the first person
     submits the agent picker with the other organization's agent: refused
     with a note naming that tenant, and nothing reaches it (no
     `turn_dispatch`, no Session, no credential read) — the picker is how a
     person starts another agent in Slack: a mention runs the thread's agent
     or the default, it names none; S3. the second person
     starts a conversation with their agent from the picker and it answers;
     S4. the first person mentions the bot in that thread (allowed by its
     person when the gateway asks): refused with the same note, the Session's
     tasks unchanged (klaus-gateway 4.10.0-rc.1 answers the held message's
     replay after the Allow with its generic "could not be picked up again"
     note; the person sends again and gets the refusal, HACKS.md U36). On
     macOS the image runs on the `kind` network with its ports published on
     loopback (Docker Desktop's host networking is off, HACKS.md U35);
   - M. muster's MCP servers per tenant
     ([MCP servers per tenant](identity.md#mcp-servers-per-tenant)), on a
     sign-in fixture `agentlab-tenants-mcp` in each organization (muster's own
     protected `/mcp`, pinned to the lab Dex with the issuer of
     `lab-oauth-fixture`, its client Secret readable by muster there):
     M1. through muster (`core_mcpserver_list`, `list_tools`), each tenant
     user lists the servers of muster's namespace and their organization's
     fixture, and is offered the sign-in to that one only; the second user,
     their group's RoleBinding gone a moment, lists muster's namespace's
     only; M2. the second user signs in to their fixture; the first user's
     call of one of its tools, `core_service_start` and `core_auth_login` for
     it are answered word for word as for a server that does not exist;
     M3. an `MCPServer` of the first organization with `auth.forwardToken`,
     `auth.requiredAudiences`, `auth.tokenExchange`, `family` or
     `auth.type: sigv4` reads `Failed` with muster's refusal naming the field;
     M4. the first user signs in to their fixture: muster sends
     `redirect_uri` `<muster>/oauth/ns/<namespace>/callback` and the sign-in
     ends there; `lab-oauth-fixture` (muster's namespace, the same issuer)
     still awaits its own sign-in on `/oauth/proxy/callback`; the admin signs
     in to the first organization's fixture and the second's still awaits its
     own, on its namespace's callback; M5. the first editor creates
     `agentlab-tenants-muster` in the first organization (toolset
     `preset:full`), and a turn as the first user reports tools of their
     fixture and of agent-manager, none of the other organization's, and a
     second turn has it call `core_mcpserver_list`: it lists the first
     user's fixture and agent-manager, not the second organization's (the
     agent is deleted again). The same through Slack (klaus-gateway) is not
     proved yet;
5. as the admin (cluster-admin): a WorkerPool patch and delete and a pod
   labelled `ate.dev/worker-pool` (server-side dry runs), and an exec into a
   worker, are refused by the chart's `agent-platform-tenant-*` admission
   policies; the network policy copies exist — skipped while agent-manager
   copies none (the lab runs with network policies off); the credential
   provider's live grants (`/statusz`) give each tenant's atespace its own
   namespace only — skipped while its policy has no `ownNamespaceSelector`
   entry;
6. deleting each namespace's last agent removes the copies and keeps the
   always-kept objects (giantswarm/agent-manager#175: the delete also
   removes the OCIRepository, which the reconciler creates again; the proof
   asserts the end state only).

A lab without the switch fails before any write, naming
`agentlab configure --tenants` and `agentlab platform`; so does a credential
policy that does not list the two namespaces, a lab without the tenant users,
and a kagent controller without the RBAC authorizer (its ConfigMap's
`KAGENT_AUTHORIZATION_MODE` not `kubernetes`, the groups not read from
`groups`, or the prefixes not the lab API server's `oidc:` — a chart before
agent-platform 4.151, or values without the lab's prefixes), and a muster
before 5.48.0, or one with `mcpServersInAllNamespaces` off in its
`config.yaml`. The namespaces, their agents
and bindings, and the platform tenant's agent go at the end of the run, also after a failure, and at the
start of the next one.

## The UI and the controller's route

The kagent UI is host-published like the other components:
`http://localhost:8081` (`platform.agentsPort` in `agentlab.yaml`);
`agentlab open agents` opens it. Being plain HTTP on loopback, it involves the
lab CA in nothing — no trust question, no browser warning. The UI does
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

The topology is the installation's (the 4.x line, kagent API v2): the
controller serves gRPC only (`kagent.api.v1alpha1` for the control plane,
`lf.a2a.v1` for the turns) as a `GRPCRoute` on the edge with the chart's JWT
`Strict` policy in front — every bearer verified against the lab Dex,
`x-user-id` set from the verified email claim — and the controller runs the
fleet's `auth.mode: trusted-proxy`, re-deriving the caller from the same
bearer; `agentlab platform-test` asserts both (a call without a token refused
at the edge; a forged header attributed to the token's subject), and the
proofs' turns take that path as the person (`internal/lab/kagentapi.go`).
Every agent is an `AgentTemplate` paired by its `Agent` with the platform
`Harness kagent` (`spec.harnessRef`) and runs as an actor
on Agent Substrate — the `WorkerPool kagent-default`'s gVisor workers in the
kagent namespace, the control plane in `ate-system` — which the chart ships
and the lab installs nothing of; kagent's `kagent_v2` database lives on the
platform's CNPG `Cluster kagent-pg` next to Substrate's (see [Agent Substrate
and the platform
Postgres](platform.md#agent-substrate-and-the-platform-postgres--from-the-chart)).
The agent chart 1.x renders one `RemoteMCPServer` per agent pointing it at
muster with its toolset header; the Harness re-emits the *caller's* bearer on
every MCP call (`KAGENT_PROPAGATE_TOKEN`), so agent tool calls through muster
are the person's, never the platform's — they need a real Dex token on the way
in. Lab deviations on the kagent side, same spirit as the [deviations
table](platform.md#lab-specific-deviations-from-a-real-management-cluster):
the JWKS source is the lab Dex, the snapshot store the bundled RustFS, the
ServiceMonitor / OTel exporters are off (the line serves no /metrics, no
OTLP gateway in kind). A git skill of an `AgentTemplate` is fetched during
the golden boot, which Substrate's egress gate has to let through;
`agentlab skills-test` is that proof, see [The skills
proof](platform.md#the-skills-proof-the-golden-boot).

An agent that never becomes Ready names what Substrate's egress gateway
refused in the verdict of every proof that waits for it: each distinct
refusal (status, method, destination, the gateway's reason), and, when the
gateway answered from a failed call to the control plane (`actor egress
policy …`, `actor identity check …`, `credential provider …`) rather than
from the destination, the endpoints of the control plane's Service
`ate-system/api` that are no pod of Deployment `ate-api-server`. A pod in
`ate-system` that carries the api-server's selector labels and serves
nothing takes a share of those calls and refuses them; the git fetch of the
golden boot then reads as a 403 from github.com.

## Turns through the edge: native gRPC, as the surfaces drive them

The controller serves gRPC only (`kagent.api.v1alpha1` for the control
plane, `lf.a2a.v1` for the turns) behind the connectivity chart's
`GRPCRoute` `kagent-controller` on the edge — native gRPC over HTTP/2, the
services matched by name — with the `AgentgatewayPolicy`
`kagent-controller-jwt` validating the person's Dex id_token (JWT `Strict`)
and rewriting `x-user-id` from the token's email claim for the controller's
`trusted-proxy` authenticator; whatever `x-user-id` a caller sent is
replaced. Swarmgeist (klaus-gateway) and the Dev Portal's backend speak to
the controller this way, and so do the proofs' turns
(`internal/lab/kagentapi.go`): one gRPC connection on the lab's TLS
transport (`agentgateway.<domain>:<gateway port>`, the lab CA trusted), the
`a2a-go/v2` client on its gRPC transport for the turns and the stubs
generated from kagent's protos (`internal/kagent/gen`, see
[development.md](development.md)) for the rest. Every call carries
`authorization: Bearer <id_token>`; every A2A call names the Agent as its
tenant (`<namespace>/<name>`) and the `Session` that holds the conversation
through the message's context id (the Session's id; created for the person
with `CreateSession`, idempotent on `request_id`, and deleted afterwards),
and carries the human-in-the-loop extension request (`A2A-Extensions:
https://kagent.dev/extensions/hitl/v1`), so a tool bound with
`requireApproval` pauses the task at `input-required` with a decidable
`tool_approval_request` instead of a plain notice. The answer is consumed
as a stream (`SendStreamingMessage`: the submitted task, working, the
artifacts, the terminal status). The Harness re-emits the caller's bearer
on every MCP call (`KAGENT_PROPAGATE_TOKEN`), so muster sees the person,
never the platform — agent tool calls through muster need a real Dex token
on the way in.

`agentlab a2a-test` is the proof of that path, with the assertions the
surfaces rely on: the route and its policy exist and are Accepted; a call
without a token is refused at the edge (`Unauthenticated` — on a raw
HTTP/2 POST the gRPC frame gets the trailers-only `grpc-status 16`, a plain
POST gets 401); a forged `x-user-id` beside a valid token is replaced
(`GetCurrentUser` names the token's person); `ListAgents` lists the proof's
Agent Ready on the platform Harness with the display-name and icon-url
annotations as Swarmgeist reads them; `CreateSession` is idempotent; a turn
streams its answer; on the proof's agent — the Generic
chart with `toolset: [preset:read-only]` and `muster.requireApproval: true`
(chart ≥ 1.1.0) — a tool call pauses the task, the person's approval
resumes it to completed with muster logging the call under the person, a
rejection ends it without the call (every request is decided until the task
settles, within five minutes: how many tool calls the model needs is its
own; a request asked again after its decision fails the step, the decision
did not resume the task); `CancelTask` on a running turn ends it
server-side (`GetTask` reports it canceled) and the session takes a
following turn. It leaves nothing behind: the sessions over gRPC
(`ListSessions` lists none of its), the agent's release and render on
the cluster.

## The codex coder

`fixtures/agents/codex-coder/` is one coding agent's definition, the same on
the lab and on an installation: a codex agent on a ChatGPT subscription
login whose Sessions work in a workspace of `giantswarm` repositories. It is
the inputs of the platform's own tools, applied through muster as the person,
nothing applied by hand:

| File | What |
|---|---|
| `values.yaml` | The agent-platform values for its model: the `kagent.modelConfigs` entry `openai-chatgpt-codex` (`credentialType: chatgptOAuth`, `apiFormat: responses`, the login Secret `kagent-chatgpt-login`). On the lab a `platform.valuesFiles` overlay; on an installation the same entry in its values |
| `agent.json` | agent-manager's `create_agent` input: `codex-coder` on the Harness `codex` and that ModelConfig, the toolset `preset:read-only`, a system message for branch-per-change work in `/workspace` |
| `workspace.json` | workspace-manager's `create_workspace` input: the workspace `giantswarm`, one source on the provider instance `github`, owner `giantswarm`, the public repositories by name. `organization` and the provider instance are the lab's (`lab`, `github`); an installation sets its own, and adds the private repositories it is meant to see with `update_workspace` |

The agent carries no repository and no workspace: a workspace is chosen when
a Session starts. Read and write access is the person's own, through the
workspace's provider: the person grants the provider instance's GitHub App
once in muster, `authorize_workspace` checks that the person reads every
repository and signs the grant, `start_session` takes it, and the egress
gateway sets the person's App token on the provider's git and API hosts per
request ([Workspaces](workspaces.md#the-write-part)). The sandbox holds no
token; `git push` and `gh pr create` work as the person, and only where the
App is installed and the person may write.

What it needs where it runs:

- The codex Harness: `platform.coding` with `codex` among
  `platform.coding.harnesses` on the lab; `kagent.harnesses` on an
  installation ([The coding agents proof](platform.md#the-coding-agents-proof)).
- The login Secret `kagent/kagent-chatgpt-login` (`access_token`,
  `refresh_token`, `account_id`, labelled `kagent.dev/chatgpt-login: "true"`):
  one `codex login --device-auth` of its own per lab or installation, since
  the refresh token rotates on every use and two refreshers on one login
  spend each other's. On the lab the three keys are placed from that login's
  `auth.json` with the operator's secret tooling, never read by a session,
  and the Secret labelled; on an installation it is a SOPS Secret
  (agent-platform `docs/chatgpt-login.md`).
- The workspaces with a GitHub provider instance whose App is installed on
  `giantswarm` with Contents, Pull requests and Issues read and write
  ([The provider instance github](workspaces.md#the-provider-instance-github)),
  and the Session admission.

The order, as the person through muster (`agentlab login --audience
agent-platform` gives a lab user the token for it):

1. `x_workspace-manager_create_workspace` with `workspace.json`, then
   `x_workspace-manager_get_workspace giantswarm` until `lastSync.result` is
   `Succeeded`.
2. `x_agent-manager_create_agent` with `agent.json`, then
   `x_agent-manager_get_agent_status codex-coder` until it is Ready.
3. `x_workspace-manager_authorize_workspace giantswarm`: `granted` with a
   `grant`, or `connect_required` with muster's sign-in for the App.
4. `x_agent-manager_start_session codex-coder` with `workspaceGrant` and the
   first message; `x_agent-manager_get_session` reads the answer.

`go test ./fixtures/...` checks the definition against what the tools refuse
at write time and the model it names against the values entry.
