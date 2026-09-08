# Agents (kagent)

The platform's agent runtime is an **optional component**, on by default
(`platform.agents` in `agentlab.yaml`; headlessly:
`agentlab configure --defaults --agents=false`). On real clusters agent
delivery runs through Flux/GitOps, which this lab does not run as a GitOps
loop — skip the runtime when agents are not what you are testing. (Backstage's
agent create flow does need Flux's source+helm controllers as its delivery
engine; the lab installs exactly those two when Backstage and agents are both
enabled — see [The agent create flow](backstage.md#the-agent-create-flow).) When enabled, the
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
[Backstage gotchas](backstage.md#backstage-gotchas) for its no-key behavior).

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

Lab deviations on the kagent side, same spirit as the [deviations
table](platform.md#lab-specific-deviations-from-a-real-management-cluster):
the controller runs `auth.mode: unsecure` (upstream's local-dev mode — the GS
default `trusted-proxy` decodes bearer claims *without verification* and
depends on a JWT-validating agentgateway this lab does not run), and the
ServiceMonitor / OTel exporters are off (no Prometheus Operator, no OTLP
gateway in kind). The umbrella also renders the shared `RemoteMCPServer`
pointing agents at muster; note that kagent forwards the *caller's* token to
muster, so agent tool calls through muster need a real Dex token on the way
in — headless pokes at the unsecured controller API won't have one.
