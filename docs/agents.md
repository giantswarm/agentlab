# Agents (kagent API v2)

The platform's agent runtime is an **optional component**, on by default
(`platform.agents` in `agentlab.yaml`; headlessly:
`agentlab configure --defaults --agents=false`). When enabled, the chart's
**kagent** component installs kagent API v2 (`api.kagent.dev/v1alpha3`) with
the platform's Substrate: every agent is an `AgentTemplate` paired by an
`Agent` of the same name with the platform's Go ADK **Harness** `kagent`
(`spec.templateRef`, `spec.harnessRef`; the connectivity chart renders the
Harness and digest-pinned runtime image), compiled
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

Agents run against the **kagent namespace's default `ModelConfig`**: the one
annotated `agent-platform.giantswarm.io/is-default-model-config: "true"`.
The chart declares no model, so the lab declares it the way an installation
does: `agentlab platform` applies the ModelConfig `anthropic` in `kagent`
after the install, annotated the default, on the lab's `aiModel` setting
(`agentlab.yaml`, default `claude-sonnet-4-6`, the BOM's own default) with
prompt caching on (`anthropic.promptCaching: true`, `cacheTTL: "5m"`) and no
`baseUrl`, which the chart's LLM routing policy sets on admission. The lab
renders none of `kagent.providers`, `kagent.defaultModelConfig` and
`kagent.modelConfigs`, which the chart refuses from agent-platform
4.175.0-rc.22. On an older release the values carry the model: from
4.175.0-rc.15 the `kagent.modelConfigs` entry `anthropic` marked
`default: true` with `kagent.defaultModelConfig.enabled: false`, before it
`kagent.providers` and the kagent chart's `default-model-config`. On every
line the ModelConfig references the Secret `kagent/kagent-anthropic` (key
`ANTHROPIC_API_KEY`), and `agentlab platform` waits for the default to be
Accepted, carry the annotation and resolve its Secret.

The proofs never assume the model's name. One that is given no
`--model-config` asks agent-manager for the namespace's default
(`list_model_configs`, the entry with `default: true`) as the person it runs
as. agent-manager reports the default from 2.19, the chart's own range from
4.175.0-rc.15 on. An agent-manager that reports no `default` at
all (an older line) answers `default-model-config`.

The API key is a **real
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
`create_agent` pins a branch to, `refreshSkills`). Without a token GitHub allows 60 requests an hour **per egress
address**, and the kind cluster shares this machine's — a handful of
`backstage-test` runs within an hour exhaust it, after which discovery comes
back truncated (the proof refuses that by name) until the window resets.

A token lifts that, from a recorded source (`githubToken.source` in
`agentlab.yaml`; `agentlab configure --github-token-source
op://<vault>/<item>/<field>`, or `<file>#<path>` of a SOPS file): every `up`
and `platform` hand it to `beekeeper secret copy --to-secret`, which writes the
token in its own process; agentlab never reads the value, and a placement that
fails fails the run. The lab never takes `$GITHUB_TOKEN` either (only
`self-update` reads it): a token exported in its environment reaches no
consumer. Without a source `discover` names the Secret the lab lacks,
`agentlab-github-token` (key `GITHUB_TOKEN`) in `agent-platform`, and `up` and
`platform` warn on stderr, naming `githubToken.source`, unless the skills
GitHub App is recorded; `agents-test` then reports its `refreshSkills` step as
a SKIP with the same reason.

`agentlab up`/`platform` place **or update** the Secret `agentlab-github-token` (key `GITHUB_TOKEN`) in
`agent-platform`: the portal takes it through `extraEnvVarsSecrets` and the
lab's app-config overlay (`integrations.github[].token: ${GITHUB_TOKEN}`),
agent-manager through the chart's `skills.github.tokenSecret`. Both
then call GitHub authenticated (5000 requests an hour; the rate-limit headers
say so). A copy in `kagent` that an earlier agentlab placed is removed. A fine-grained token with public read access is enough; a token that
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

**The migrate run reads no GitHub token.** The connectivity chart's
`agent-manager migrate` run (the cut-over to the `api.kagent.dev` line)
resolves public skill repositories without a credential and reports a private
ref as pending; neither `githubToken.source` nor the skills GitHub App reaches
it. The chart ships it as the suspended CronJob
`agent-platform-connectivity-agent-manager-migrate` in `kagent`, never
scheduled. The `post-install,post-upgrade` hook
`agent-platform-connectivity-agent-manager-migrate-start` creates the run
`agent-platform-connectivity-agent-manager-migrate-<hash>` from it, once per
pod template: an upgrade that changes nothing in it starts no new run. The run
is owned by the CronJob, not the release, so its failure never fails the
`agentlab platform` upgrade; read it in its Job, its log and the ConfigMap
`kagent/agent-manager-migrate-report`. To run it again:

```bash
kubectl -n kagent create job agent-platform-connectivity-agent-manager-migrate-rerun-1 \
  --from=cronjob/agent-platform-connectivity-agent-manager-migrate
```

The chart documents the run in agent-platform's
[UPGRADE.md](https://github.com/giantswarm/agent-platform/blob/main/UPGRADE.md)
("the migrate run is started by a hook from a suspended CronJob") and in
[`templates/kagent/migrate-cronjob.yaml`](https://github.com/giantswarm/agent-platform/blob/main/helm/agent-platform-connectivity/templates/kagent/migrate-cronjob.yaml);
its values (`agentManager.migration.*`) are in the connectivity chart's
[README](https://github.com/giantswarm/agent-platform/blob/main/helm/agent-platform-connectivity/README.md).

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
Flux labels and reads it back `managed: gitops`; agent-manager answers the
change's dry run `gitops_owned` in mode apply (and `unsupported` in mode
commit while it offers no commit mode); `update_agent` is refused and the live
release stays as it was. The agent goes away at the end of the run. The pull
requests of mode commit are `agentlab backstage-test`'s commit stage (below).

## Commit mode

The lab's agent-manager offers commit mode (`get_info` reports
`capabilities.commit`) while the platform runs agents, the workspaces'
`github` instance renders (`platform.workspaces.github`, its App's ids and
Secret in place), muster keeps that instance's App (the workspace-manager's
pin or one provider App per instance) and the agent-manager the install
resolves is 2.22.0-rc.3 or newer (`components.agent-manager.versionRange` in
`platform.valuesFiles` admits a release candidate). `agentlab platform`
prints which.

A commit opens its pull request as the person with their grant of the
`github` instance's App — the grant they made once for the workspace-manager
— the way an installation does it without pinning agent-manager's MCP
registration to an App (agent-manager's `githubTokenExchange`):

- agent-manager presents the person's forwarded ID token to muster's RFC 8693
  broker (`https://muster.<domain>/oauth/token`, through the edge, under the
  lab CA) as the confidential client `agent-manager`, whose secret the lab
  generates once (`certs/agent-manager-commit/`) and places as the Secret
  `agent-platform/agent-manager-github-exchange`;
- muster (`muster.oauth.server.tokenExchangeBroker`, the lab Dex its trusted
  subject issuer) releases that person's own grant of the App
  (`https://github.com/apps/<appId>`) to that client and for that one
  audience, `agent-manager-github`; a person without the grant gets
  `auth_required` on a commit.

Apply mode, every read and every other proof keep the ID token: nothing else
needs the grant. The App is installed with contents and pull-request write on
the instance's `proofRepositories`; the first is the commit target of the
proof. `agentlab backstage-test`'s commit stage ([Backstage](backstage.md#the-agent-platform-proof))
drives the portal's create, edit and delete as pull requests on it and closes
every one again.

## Organizations

`platform.organizations` (on by default; `agentlab configure
--organizations[=false]`) renders the chart's
`agentPlatformOrganizations.enabled: true` with the LLM listener it needs
(`llmRouting.enabled`): the meta chart installs agent-platform-operator, and
the connectivity chart renders the organizations' roles
(`agent-platform:organizations-admin`, `admin`, `editor`, `user`) and their
admission policies, and selects the organization namespaces by the label
`agent-platform.giantswarm.io/organization` (Substrate's credential provider,
the workers' admission, kagent's authorizer). muster serves those
namespaces' `MCPServer`s only once the chart sets its
`mcpServerNamespaceSelector` ([MCP servers per tenant](identity.md#mcp-servers-per-tenant)). It takes effect with the agents runtime on a
chart from agent-platform 4.175.0-rc.4 on (`config.OrganizationsChartFloor`), the default chart included;
below it, `up` says so and leaves the chart's earlier shape alone. The
tenants mode the chart removed (`platform.tenants`, agent-manager's tenant
reconciler) is gone from the lab: an `agentlab.yaml` that still names it
loads, and the key is not written back.

Each organization is an `AgentPlatformOrganization` in a parent namespace,
reconciled by the operator into an organization namespace of its own,
`agents-<name>-<random>`. On an installation the parent is a KaaS
`org-<name>` namespace; the lab has no KaaS, so its parent is the fixed root
namespace `agentlab-organizations`.

**The default organization.** `agentlab up` (and `agentlab platform`) keeps
the organization `agentlab-organizations/default` (display name `Lab`) and
waits until the operator reports it `Ready`. Then, as an organization's
creator does after `Ready`, it writes the first admins, `organization-admins`
to `agent-platform:admin` for the group `platform-admins`; the editors,
`organization-editors` to `agent-platform:editor` for the group
`developers`; and the organization's model: the ModelConfig `anthropic` on
the lab's model, marked default, its key in the tenant credential
`tenant-credential-anthropic`, placed the way the lab places the Anthropic
key everywhere (`anthropic.go`: the recorded source, the host environment,
else the placeholder; agentlab never reads the value). The chart's admission
policy fills in the LLM listener as its `baseUrl`. Last, the first developer
creates an agent's HelmRelease there as a server-side dry run with their own
Dex token, which RBAC and every admission policy of the namespace (the
HelmRelease field lock among them) judge; `up` fails if it is refused. The
summary names the namespace. The lab's values also count the operator's
ServiceAccount quota under `count/serviceaccounts` (HACKS.md U42).

## The organizations proof

`agentlab organizations-test` (`tenants-test` is an alias) is the proof
(giantswarm/agentlab#1149, the agent-platform-organizations plan). It keeps
two organizations, `agentlab-a` and `agentlab-b`, in the root namespace, and
binds one member to each as an editor by the group in their token
(RoleBinding `agentlab-members`, subject `oidc:tenant-a` or
`oidc:tenant-b`): `alice@lab.local` and `bob@lab.local`, which `agentlab
configure --organizations` adds ([The member users](identity.md#the-member-users)).
A member who is also in `platform-admins`, `viewers` or `developers` is
refused: each reaches agents outside their organization. Each organization
gets the lab's model.

Each member creates the agent `agentlab-organizations` in their own
organization as themselves: a HelmRelease of the agent chart written through
the Kubernetes API with their Dex token, which the chart's HelmRelease field
lock admits (the operator's chart source and Helm ServiceAccount). The agent
becomes `Ready` on the organization's copy of the platform Harness, and a
turn as its member answers, the credential provider reading credentials for
the namespace's atespace from that namespace only. Then:

- **The same answer on every surface.** The agents each member sees through
  the Kubernetes API (a list in every organization namespace as the
  member), through the kagent API at the edge (`ListAgents`) and in Slack
  (klaus-gateway's per-caller roster, giantswarm/klaus-gateway#354) are one
  set: their own organization's agent, nothing of the other's. The platform
  tenant's agents in `kagent` are not judged (they go with
  giantswarm/agent-manager#166). The Dev Portal surface is skipped until its
  organization page and New agent picker land
  (giantswarm/backstage#3363).
- **The negative cross-organization tests.** Each is a verdict that names
  its surface and the boundary it proves, and each has a twin in the
  member's own organization that is admitted, so a refusal for another
  reason fails it:
  - Kubernetes API (RBAC): the first member's lists of agents, agent
    templates, RemoteMCPServers, muster MCPServers, HelmReleases,
    ModelConfigs and Secrets in the other organization answer 403 (their own
    are answered; an editor reads no Secret anywhere); their create of an
    agent there answers 403, in their own it is admitted (dry runs); a
    SelfSubjectAccessReview for `list sessions.api.kagent.dev` there is
    denied (its twin: `list agents.api.kagent.dev` in their own is allowed);
  - kagent API and A2A (kagent's authorizer): `CreateSession` and a first
    A2A message on the other organization's agent are `PermissionDenied`, no
    Session of it is made and no credential read for its atespace; an
    Agent's create, change and delete there are `PermissionDenied`, while the
    same in their own organization pass; `ListSessions` with `all_creators`
    is `PermissionDenied` for both members and answered for the admin;
  - Slack: each roster is the member's own organization's, a pick of the
    other organization's agent is refused with nothing reaching it, and a
    turn in a thread on it is refused;
  - muster: with an `MCPServer` in each organization namespace, each member
    lists muster's namespace's servers and their own organization's; with
    their RoleBinding gone a moment, their own goes too; the first member's
    `core_service_start` and `core_auth_login` naming the other
    organization's server read word for word as for a server that does not
    exist;
  - models and the llm listener: the first member's agent naming the other
    organization's ModelConfig is refused at admission or fails on that
    reference, and no credential of the other organization is read for its
    atespace, so the other's key never travels through the listener for it;
    the same agent on their own model is admitted;
  - egress gateway: the first member's agent (declaring
    `https://example.com`) probes that origin, the API server and
    Substrate's API on 443 from its actor; judged from the gateway's record,
    the declared origin is forwarded (the twin) and each in-cluster request
    is refused by the gate for the actor's egress policy, or held before any
    connection opened: the probe's own line shows no HTTP status and a name
    resolution or connection failure. An HTTP answer from the endpoint, a TLS
    or certificate failure, or an ambiguous failure fails the check.
- **The management-cluster profile.** The operator's ResourceQuota
  `agent-platform-organization-objects` admits ServiceAccounts up to its hard
  `count/serviceaccounts` and refuses the next; with a Kyverno PolicyException in
  `policy-exceptions` that lifts every rule for `agents-*`, a WorkerPool
  patch and delete, a worker-labelled pod and an exec into a worker are still
  refused by the chart's ValidatingAdmissionPolicies (as the admin,
  cluster-admin); the credential provider's live grants (`/statusz`) give each
  organization's atespace its own namespace only. Secret access per
  component (giantswarm/agent-platform#1269), each read made with the
  ServiceAccount's own token (TokenRequest, no client certificate) against a
  dummy workload cluster kubeconfig `org-lab/agentlab-kubeconfig`:
  ate-controller gets `ate-system/egress-mitm-ca-pool` and is refused
  everything else; both credential providers get the model key in `kagent`
  and in an organization namespace and are refused the kubeconfig, the pool
  and a list; the connectivity install hooks, whose identity exists only
  during a hook event, get it from the release's own hook manifests for the
  check (removed afterwards) and read the pool only. A get the authorizer
  allows may answer NotFound; a refusal must be 403. The cleanup check starts a member-owned READY session, records its organization in a suspended installation Kustomization's settled inventory, then removes that inventory entry and waits for the operator to report `Orphaned=True`. Administrative deletion must remove the organization, namespace, actors and snapshot prefix, and refuse another resume of the session. A seeded control object under the second organization's UID must survive that cleanup. The check requires kagent 1.31.1-rc.3 or newer and an operator that supports the configured path-style store; it preserves the five-minute snapshot cleanup delay.

### The credentials part

`organizations-test --part credentials` proves an organization's keys are
typed only into agent-manager's credential API, never into a tool call
(agent-manager docs/organization-credentials.md). It keeps its own
organization, `agentlab-organizations/agentlab-credentials` (a previous
run's is erased first), with `platform-admins` as its admins, and acts as
the first admin. The key it types is a random value made in the process and
printed only as its SHA-256. Each verdict:

- `set_organization_model` through muster (`x_agent-manager_set_organization_model`)
  for a new model without a key writes nothing: no ModelConfig, no
  `tenant-credential-*` value, and the answer names a pending credential
  operation.
- The same call from three MCP client generations, each on mcp-go's client:
  a 2026-07-28 client declaring URL elicitation gets `input_required`, its
  message prefixed `[agent-manager]` by muster (giantswarm/muster#1812); a
  2025-11-25 client that declared it at initialize gets `-32042` with the
  form's URL; a client without it gets a result whose `credentials[].next`
  carries the link, and from agent-manager 2.22.0 (giantswarm/agent-manager#329)
  `structuredContent` and a `resource_link` (skipped on an older one).
- The form's save, the way the portal's form posts it
  (giantswarm/backstage#3364): `GET /api/v1/credential-operations/{operation}`
  with the person's token answers the confirmation with a strong `ETag` and
  `Cache-Control: no-store`; another lab user's read is refused (404); `POST`
  with `If-Match` and only the requested Secret/key returns the receipt, and
  the original call retried returns it too. The lab's portal reaches
  agent-manager through muster only, so the part calls the agent-manager
  Service's REST listener on a port-forward, the hop a portal backend takes.
  Without commit mode the save writes the Secret live and its SHA-256 is
  compared.
- In commit mode, the receipt's pull request is read back with your `gh`
  login: opened by the person, its commit authored by them, the Secret file
  `<contentPrefix>/<parent>/<name>/<secret>-secret.enc.yaml` SOPS-encrypted
  and no file carrying the value.
- Flux: the person merges the pull request, and the organization's own
  Kustomization, which agent-platform-operator keeps in commit mode
  (`--flux-namespace`, `--flux-source-name`) with `decryption.provider: sops`
  and `--flux-decryption-secret`, applies and decrypts it: the Secret's
  SHA-256 matches and the ModelConfig exists. The age identity is the lab's,
  generated once into `certs/organization-credentials-sops/` and placed as
  that Secret (an existing Secret the lab did not write is left alone). The
  repository's `.sops.yaml` must encrypt the content path for its recipient;
  the part prints the rule to commit when it does not. After a merge, the
  run removes the organization's content path from the base branch with
  your git credentials.
- A sign-in a server of the organization's namespace needs: an OAuth
  `MCPServer` there, modelled on `lab-oauth-fixture` without a pinned
  client, reads `Auth Required`, and `core_auth_login` for
  `<namespace>/<name>` is a URL elicitation whose message names that key.
- `delete_organization` erases the organization: without `confirm` it says
  what goes, the confirmed dry run deletes nothing, the confirmed call
  deletes it; the namespace goes and no RoleBinding anywhere refers to it.

What the part stands on is read from the live lab and never configured
by it; each missing piece is a `SKIP` naming its fix: agent-manager's
`credentials.uiURL` (its `--credential-ui-url`), muster speaking MCP
2026-07-28, commit mode (`githubTokenExchange`, `tenants.commit.repository`)
and the person's grant of the workspaces' `github` App, a Ready
kustomize-controller (`flux-engine.instance.components`), and
agent-platform-operator's commit-mode flags. muster's
`mcpServerNamespaceSelector` must select the organization namespace for the
sign-in verdict.

`organizations-test --part cleanup` runs the orphan cleanup check alone, creating the proof agents and a member-owned session directly through kagent without a model turn. The default run includes this check after all other organization checks. The former `--part erasure` spelling remains an alias. See the operator's [glossary](https://github.com/giantswarm/agent-platform-operator/blob/main/GLOSSARY.md) for the distinction between organization deletion and cleanup.

The report prints one `PASS` or `SKIP` line per verdict, `[surface]
boundary — what was seen`; a failure stops the run as `FAIL [surface]
boundary: ...`. `--skip-slack` leaves klaus-gateway out (its lines read
`SKIP`); `--gateway-image` and `--gateway-binary` pick the gateway as for
`klaus-gateway-test` (on macOS the image runs on the `kind` network, HACKS.md
U35). Successful runs delete both proof organizations and remove the installation inventory fixture and its temporary operator setting. Failed cleanup retains the organizations and fixtures for diagnosis and reports the failing step. Other failures remove the transient agents, Sessions, PolicyException and quota ServiceAccounts.

The proof leaves out muster's per-organization sign-in: a server of an
organization namespace that signs in needs muster to read its client Secret
there, and the organizations' subject policy refuses a RoleBinding of
muster's ServiceAccount, which lives in another namespace; and its callback
(`/oauth/ns/<namespace>/callback`) names a namespace the operator generates,
which the lab Dex's static redirect URIs cannot list ahead.

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
muster with its toolset header. Sessions started through agent-manager are
bound to the person in muster; the egress gateway authenticates MCP calls
with the actor's JWT in place of the runtime's placeholder. Lab deviations
on the kagent side, same spirit as the [deviations
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
through the message's context id (the Session's id; started for the person
through agent-manager's `start_session` over muster, idempotent on
`requestId`, and deleted afterwards through its `delete_session`),
and carries the human-in-the-loop extension request (`A2A-Extensions:
https://kagent.dev/extensions/hitl/v1`), so a tool bound with
`requireApproval` pauses the task at `input-required` with a decidable
`tool_approval_request` instead of a plain notice. The answer is consumed
as a stream (`SendStreamingMessage`: the submitted task, working, the
artifacts, the terminal status). `start_session` binds the Session's actor
to the person in muster, so the runtime's MCP calls carry the actor's JWT
and muster serves them as the person, never the platform. A Session
created straight at the controller (`CreateSession`) is not bound: the
platform Harness forwards no caller token, so its runtime reaches muster
with no credential.

`agentlab a2a-test` is the proof of that path, with the assertions the
surfaces rely on: the route and its policy exist and are Accepted; a call
without a token is refused at the edge (`Unauthenticated` — on a raw
HTTP/2 POST the gRPC frame gets the trailers-only `grpc-status 16`, a plain
POST gets 401); a forged `x-user-id` beside a valid token is replaced
(`GetCurrentUser` names the token's person); `ListAgents` lists the proof's
Agent Ready on the platform Harness with the display-name and icon-url
annotations as Swarmgeist reads them; `start_session` is idempotent; a turn
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
| `model-config.yaml` | Its model, a ModelConfig object: `openai-chatgpt-codex` in `kagent` (`openAI.credentialType: chatgptOAuth`, `openAI.apiFormat: responses`, the login Secret `kagent-chatgpt-login`). An installation declares it next to its agents; on the lab the `platform.extraModels` entry `chatgpt-login` with `credentialType: chatgptOAuth` is the same model, whose ModelConfig agentlab renders itself ([Extra model configs](models.md#extra-model-configs-self-hosted-openrouter-gemini-openai)) |
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
