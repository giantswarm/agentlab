# Agents (kagent)

The platform's agent runtime is an **optional component**, on by default
(`platform.agents` in `agentlab.yaml`; headlessly:
`agentlab configure --defaults --agents=false`). Agent delivery runs through
Flux on every installation — the portal and agent-manager write each agent as
a `HelmRelease` of the agent chart — and in this lab through the platform
chart's bundled engine (see [The agent create flow](backstage.md#the-agent-create-flow));
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
[Backstage gotchas](backstage.md#backstage-gotchas) for its no-key behavior).

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
controller's gRPC API is a `GRPCRoute` on the edge with the chart's JWT
`Strict` policy in front — every bearer verified against the lab Dex,
`x-user-id` set from the verified email claim — and the controller runs the
fleet's `auth.mode: trusted-proxy`, re-deriving the caller from the same
bearer; `agentlab platform-test` asserts both (a call without a token refused
at the edge; a forged header attributed to the token's subject). Every agent
is an `AgentTemplate` admitted by the platform `Harness kagent` (labelled
`agent-platform.giantswarm.io/harness: kagent`) and runs as an actor on Agent
Substrate — the `WorkerPool kagent-default`'s gVisor workers in the kagent
namespace, the control plane in `ate-system` — which the chart ships and the
lab installs nothing of; kagent's `kagent_v2` database lives on the platform's
CNPG `Cluster kagent-pg` next to Substrate's (see [Agent Substrate and the
platform Postgres](platform.md#agent-substrate-and-the-platform-postgres--from-the-chart)).
The agent chart 1.x renders one `RemoteMCPServer` per agent pointing it at
muster with its toolset header; kagent forwards the *caller's* token
(`KAGENT_PROPAGATE_TOKEN`), so agent tool calls through muster are the
person's. Lab deviations on the kagent side, same spirit as the [deviations
table](platform.md#lab-specific-deviations-from-a-real-management-cluster):
the JWKS source is the lab Dex, the snapshot store the bundled RustFS, the
ServiceMonitor / OTel exporters are off (the line serves no /metrics, no
OTLP gateway in kind). A git skill of an `AgentTemplate` is fetched during
the golden boot under Substrate's egress gate; `agentlab skills-test` is
that proof, see [The skills proof](platform.md#the-skills-proof-the-golden-boot).
On a chart that still serves `agents.kagent.dev` (the 0.10 product's 3.x
line) the Agent CRs and the heals above apply instead.
