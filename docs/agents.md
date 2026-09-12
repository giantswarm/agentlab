# Agents (kagent API v2)

The platform's agent runtime is an **optional component**, on by default
(`platform.agents` in `agentlab.yaml`; headlessly:
`agentlab configure --defaults --agents=false`). When enabled, the chart's
**kagent** component installs kagent API v2 (`kagent.dev/v1alpha3`) with the
platform's Substrate: every agent is an `AgentTemplate` admitted by the
platform's Go ADK **Harness** `kagent` (the connectivity chart renders it,
digest-pinned runtime image, `KAGENT_PROPAGATE_TOKEN` on), compiled by the
controller into a golden snapshot and run as a Substrate actor; a
conversation is an `AgentInstance`, its turns A2A messages through the edge
as the signed-in person (`docs/platform.md` "Dev channel" and "Substrate").
There is no per-agent Deployment or pod, and no `agents.kagent.dev`: the 0.x
Agent CR, its runtime-image heal and its CRD patch are gone with the line
that had them.

## One agent = one HelmRelease of the agent chart (1.x)

Agent delivery runs through Flux on every installation. An agent is a
`HelmRelease` of the Generic [`agent` chart](https://github.com/giantswarm/agent)
in the `kagent` namespace, rendering from the namespace's shared
`OCIRepository agent` that tracks the chart at `1.x`; the platform chart's
bundled engine renders it as the tenant ServiceAccount `kagent-flux` (the
connectivity chart's `kagent.fluxServiceAccountName`). One release renders:

- the `AgentTemplate` named after the release — `spec.description`,
  `spec.systemPrompt`, `spec.modelConfig.name`, `spec.skills[]`, the
  annotations `ui.giantswarm.io/display-name` and `ui.giantswarm.io/icon-url`,
  and the admission label `agent-platform.giantswarm.io/harness: <agent.harness>`
  (default `kagent`) the Harness's `allowedAgentTemplates` selector matches;
  a template no Harness admits is created and never becomes Ready;
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
  `status.harnesses[]` (`get_agent_status`: `ready | progressing | failed` —
  the last with the condition's message, or the Harnesses of the namespace
  and what they admit when none admits the template).
- **a HelmRelease applied directly** — the operator's kubectl path, the same
  objects by hand.

In the lab both are one helper (`internal/lab/agent.go`): `readyAgent`
writes an `agentSpec` through an `agentWriter` (`agentManagerWriter` on a
muster session, `portalAgentManagerWriter` through the portal's muster
backend, `helmReleaseWriter` directly) and waits with `waitAgentReady` until
the platform Harness reports Ready on the desired revision — or fails fast
with the reason when the release's render is refused, a Harness condition is
False for good, or no Harness admits the template. `removeAgent` takes the
release and whatever render is left. Every agent proof — `agents-test`,
`toolsets-test`, `models-test`'s turn, `backstage-test`'s own agent — creates
its agents this way; `skills-test` alone writes a raw `AgentTemplate`, on
purpose: it is the golden-boot diagnostic below the chart (a private
fixture's `credentialRef`, the control boot without the skill).

## The model

Agents run against a **default `ModelConfig`** that the connectivity chart
renders from the lab's `aiModel` setting (`agentlab.yaml`, default
`claude-sonnet-4-6` — the BOM's own default), referencing the Secret
`kagent/kagent-anthropic`. The API key is a **real credential**, so unlike
the lab's throwaway passwords it never enters `agentlab.yaml` or the rendered
`state/` files: `agentlab platform` creates the Secret from
`$ANTHROPIC_API_KEY` on the host (created once, left alone; delete it and
re-run to rotate). Without the env var the install still succeeds — the
ModelConfig then points at a Secret that does not exist yet, which is
harmless until an agent is created: its template's `ResolvedRefs` stays
False on the Harness until the Secret lands. To supply the key later, either
export it and re-run `agentlab platform` (idempotent — it only fills the
gap), or create the Secret directly:

```bash
kubectl -n kagent create secret generic kagent-anthropic \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-...
```

The same key powers Backstage's AI chat via a second Secret,
`backstage/backstage-anthropic` (see
[Backstage gotchas](backstage.md#backstage-gotchas) for its no-key behavior).
Further models — the host model servers through model-manager,
`platform.extraModels` — are their own ModelConfigs; see [Models](models.md).

## The GitHub token

Two consumers of the lab resolve skills through GitHub's REST API: the
portal's skill discovery (`GET /api/gs/agent-skills?repoUrl=…`, the create
wizard's skills step) and agent-manager (`list_skills`, the commit
`create_agent` pins a branch to, `refreshSkills`, the migrate Job of the
cut-over). Without a token GitHub allows 60 requests an hour **per egress
address**, and the kind cluster shares this machine's — a handful of
`backstage-test` runs within an hour exhaust it, after which discovery comes
back truncated (the proof refuses that by name) until the window resets.

`$GITHUB_TOKEN` on the host lifts that: `agentlab up`/`platform` create **or
update** the Secret `agentlab-github-token` (key `GITHUB_TOKEN`) in
`agent-platform` — the portal takes it through `extraEnvVarsSecrets` and the
lab's app-config overlay (`integrations.github[].token: ${GITHUB_TOKEN}`),
agent-manager through the chart's `skills.github.tokenSecret` — and in
`kagent` for the migrate Job (`agentManager.migration.githubToken`), which then
call GitHub authenticated (5000 requests an hour; the rate-limit headers say
so). A fine-grained token with public read access is enough; a token that
reads a private skill repository lets agent-manager resolve skills from it.
As with the Anthropic key, the token is a real credential: it travels host
environment -> Secret and never enters `agentlab.yaml`, `state/` (the
rendered values carry the Secret's *name*), a log line or a process's argv.
Re-running with a new token rotates the Secret.

Without the variable the lab is as before: the values name no Secret, the
consumers call GitHub unauthenticated, and a Secret an earlier run created
stays — unreferenced — until `agentlab down` (a run that merely lacks the
export never deletes a credential; `kubectl -n agent-platform delete secret
agentlab-github-token` does). `backstage-test`, `agents-test` and the
rehearsal print the window before their first skill-resolving step
(`GitHub API window (unauthenticated: this machine's shared window): 12 of 60
requests remaining, resets 14:32:10 CEST`) and, when it is exhausted, wait once
until the reset instead of failing on a truncated listing.

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
Every agent is an `AgentTemplate` admitted by the platform `Harness kagent`
(labelled `agent-platform.giantswarm.io/harness: kagent`) and runs as an actor
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

## Turns through the edge: native gRPC, as the surfaces drive them

The controller serves gRPC only (`kagent.api.v1alpha1` for the control
plane, `lf.a2a.v1` for the turns) behind the connectivity chart's
`GRPCRoute` `kagent-controller` on the edge — native gRPC over HTTP/2, the
five services matched by name — with the `AgentgatewayPolicy`
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
`authorization: Bearer <id_token>`; every A2A call exactly one
`x-kagent-agent-instance-id` — the `AgentInstance` that holds the
conversation, created for the person with `CreateAgentInstance`
(idempotent on `request_id`) and deleted afterwards — and the
human-in-the-loop extension request (`A2A-Extensions:
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
(`GetCurrentUser` names the token's person); `ListAgentTemplates` lists the
proof's agent Ready on the platform Harness with the display-name and
icon-url annotations as Swarmgeist reads them; `CreateAgentInstance` is
idempotent; a turn streams its answer; on the proof's agent — the Generic
chart with `toolset: [preset:read-only]` and `muster.requireApproval: true`
(chart ≥ 1.1.0) — a tool call pauses the task, the person's approval
resumes it to completed with muster logging the call under the person, a
rejection ends it without the call; `CancelTask` on a running turn ends it
server-side (`GetTask` reports it canceled) and the instance takes a
following turn. It leaves nothing behind: the instances over gRPC
(`ListAgentInstances` lists none of its), the agent's release and render on
the cluster.
