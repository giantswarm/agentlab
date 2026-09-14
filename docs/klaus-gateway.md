# klaus-gateway

Swarmgeist ([giantswarm/klaus-gateway](https://github.com/giantswarm/klaus-gateway)),
the fleet's Slack bridge, in the lab. It runs its conversations on the kagent
controller — A2A v1 over gRPC through agentgateway, the roster from
`ListAgentTemplates`, one `AgentInstance` per channel thread, human in the
loop as kagent's HITL extension — every call made as the person behind the
turn, whose Dex id_token the gateway forwards. The lab carries it in two
shapes, and `agentlab klaus-gateway-test` proves both:

- **On the host** — the released image or a local build, its a2a target the
  lab's *public* `grpcs://` hostname with the lab CA and the JWT policy at
  the edge, a bolt store in a run directory. Always part of the proof; the
  public leg an installation exposes to Swarmgeist's own cluster. See
  [The Swarmgeist proof](platform.md#the-swarmgeist-proof-klaus-gateway-on-kagent-api-v2).
- **As the meta chart's component** (`platform.klausGateway`) — the
  installation's shape: `components.klaus-gateway` on the *in-cluster*
  target `grpc://agentgateway.agent-platform.svc.cluster.local:8080`, its own
  ServiceAccount and RBAC, and the OBO link store in a Kubernetes Secret,
  the store the fleet runs since klaus-gateway 1.3.0. This page.

## What the lab does

`platform.klausGateway` in `agentlab.yaml` (`agentlab configure
--klaus-gateway`; off by default; needs `platform.agents`):

```yaml
platform:
  klausGateway:
    enabled: true
  devImages:
    klaus-gateway: klaus-gateway:dev-1a2b3c     # optional: a build of the checkout
```

- **`agentlab platform`** creates two Secrets in `agent-platform` before the
  install and leaves them alone afterwards: `agentlab-klaus-gateway-slack`
  (`bot-token`, `signing-secret` — placeholders no Slack workspace answers;
  the gateway refuses OBO without the Slack adapter, and the Events API
  adapter makes no outbound call at start) and `agentlab-klaus-gateway-obo`
  (`state-key`, `store-key` — random, generated once; `store-key` seals every
  link in the link Secret, so a new key would orphan them). Then it turns
  `components.klaus-gateway` on with the `klausGateway:` block of
  `state/agent-platform-values.yaml`: `a2a` on the in-cluster target,
  `namespace: kagent`, the proof's fixture `agentlab-klaus-gateway-test` as
  `defaultAgent` (the Slack adapter refuses to start without one; the name
  resolves on a turn, never at start); `web.enabled: true`; `slack.enabled`
  in `events` mode on the placeholder Secret; `obo.enabled` with
  `store: secret`, `musterUrl` the lab muster's public URL, `callbackBaseUrl`
  `https://agentgateway.<domain>` (no port: the connectivity chart derives
  the hostname of the OBO route it renders on the edge from it, and a
  Gateway API hostname carries none) and `existingSecret` the keys Secret;
  `serviceMonitor.enabled` following `platform.observability`. The meta
  chart's own `klausGateway` block keeps the static lifecycle driver, no
  second agentgateway and the disruption guards. The chart's edge route for
  the channels (`agentgatewayRoute`) stays off — the public leg is the
  host-mode proof's.
- **What the chart renders** with `obo.store: secret`: Deployment
  `klaus-gateway` (`KLAUS_GATEWAY_OBO_STORE=secret`, no store volume, the
  default RollingUpdate strategy — a mounted ReadWriteOnce claim is what used
  to force Recreate), the empty link Secret `klaus-gateway-obo-links` with
  `helm.sh/resource-policy: keep`, and Role + RoleBinding
  `klaus-gateway-obo-links` granting the gateway's ServiceAccount
  `get`/`update`/`patch` on exactly that Secret (`resourceNames`; no
  `create`, which `resourceNames` cannot scope — the chart creates the
  Secret). `agentlab logs klaus-gateway` tails the pod.
- **A build of the checkout** swaps in through the dev-image loop
  ([Dev images](platform.md#dev-images)): the target `klaus-gateway` replaces
  the image of Deployment `klaus-gateway`'s container `klaus-gateway`, read
  off the component's render (`gsoci.azurecr.io/giantswarm/klaus-gateway`).
  Refused while the component is off — there would be nothing to swap.

  ```bash
  cd ~/projects/giantswarm/klaus-gateway
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o klaus-gateway-linux-amd64 .
  docker build -t klaus-gateway:dev-$(git rev-parse --short HEAD) .
  # agentlab.yaml: platform.devImages: {klaus-gateway: klaus-gateway:dev-<sha>}
  agentlab platform && agentlab klaus-gateway-test
  ```

- **Removing the key** removes the component on the next `agentlab
  platform` (the chart uninstalls the release; the kept link Secret and the
  two lab Secrets stay).

## The proof

`agentlab klaus-gateway-test` runs the five host-mode assertions first
([The Swarmgeist proof](platform.md#the-swarmgeist-proof-klaus-gateway-on-kagent-api-v2))
and, on a lab with the component, continues on it:

6. **The render.** Deployment `klaus-gateway` runs the Secret store (the env
   above), mounts no `obo-store` volume and is not `Recreate`; Role
   `klaus-gateway-obo-links` has one rule — the core group's `secrets`,
   `resourceNames: [klaus-gateway-obo-links]`, exactly `get`/`update`/`patch`;
   the RoleBinding binds the Deployment's ServiceAccount to it; the link
   Secret carries `helm.sh/resource-policy: keep`.
7. **The store.** Two link records are written **through the gateway's own
   store package** (`github.com/giantswarm/klaus-gateway/pkg/auth/musterlink`,
   `SecretStore.Put`) with the lab's `store-key`, so they are sealed byte for
   byte as the gateway seals them (AES-256-GCM, the JSON of the link); the
   Secret's count grows by two and both read back unchanged. Their Slack user
   ids carry the prefix `agentlab-klaus-gateway-test-`; a leftover of an
   aborted run is removed first, by that prefix.
8. **Pod loss.** The pod is deleted — the node-loss shape of the incident the
   Secret store fixed, not a rolling restart. The replacement must be Ready
   within 60 s (the elapsed time is reported; the claim needed minutes when
   the node was gone), and its `obo link store ready` record must report the
   same link count; both records read back unchanged afterwards.
9. **The in-cluster leg.** Through a port-forward to the replacement pod,
   `GET /web/agents` as the person lists the fixture with its display name
   and icon (and hides the unadmitted one), and one turn answers — the
   component talking to the controller over the in-cluster plaintext h2c
   target, which the host-mode gateway never touches.

Then the proof's records are removed (the count is back where it was), and
the host-mode cleanup takes the fixtures and the AgentInstances with it. On a
lab without the component the proof says so and passes on the host-mode
assertions alone.

## What the lab cannot prove

**A real OBO sign-in.** The linker's callback checks the muster identity's
e-mail against the Slack workspace's (`users.info` with the bot token), which
the placeholder credentials cannot answer, and muster refuses a CIMD
`client_id` on a private-IP hostname (`*.127.0.0.1.nip.io`) — the component's
`/auth/slack/link` flow can be started but never completes here. The store
is proven with records written through the package; that a linked person is
not asked to sign in again after a pod loss is an installation proof (the
first installation on the Secret store). Should a lab sign-in ever matter,
the way is a registered client instead of CIMD: a DCR client at muster's
`POST /oauth/register`, `KLAUS_GATEWAY_OBO_CLIENT_ID` on the gateway, and the
lab CA through `SSL_CERT_FILE` — none of it wired here.

**Slack itself** (the `@`-mention, the thread reply, `/agent`, the Block Kit
surface): no workspace, verified on the first installation that runs the
release, per the agentlab-first exception.

**The bolt → Secret import** (`obo.storePath` with the Secret store, the
first start reading the file): the import needs the bolt file on a claim the
component no longer mounts; the lab proves the store the fleet ends up on.

**The egress policy** of the connectivity chart (the gateway's route to the
kube-apiserver for the Secret store, giantswarm/agent-platform#443): the lab
runs no Cilium and cannot see it.
