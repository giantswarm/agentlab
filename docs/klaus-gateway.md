# klaus-gateway

Swarmgeist ([giantswarm/klaus-gateway](https://github.com/giantswarm/klaus-gateway)),
the fleet's Slack bridge, in the lab. It runs its conversations on the kagent
controller — A2A v1 over gRPC through agentgateway, the roster from
`ListAgentTemplates`, one `AgentInstance` per Slack thread, human in the
loop as kagent's HITL extension — every call made as the person behind the
turn, whose Dex id_token the gateway forwards. Slack is its only channel
(since 2.0, giantswarm/klaus-gateway#319), and no workspace answers the lab,
so the proof stands in for Slack on both sides: a fake Slack Web API that
records what each thread shows, and signed Events API callbacks and Block
Kit clicks as the people's messages and buttons. The lab carries the
gateway in two shapes, and `agentlab klaus-gateway-test` proves both:

- **On the host** — the released image or a local build, its a2a target the
  lab's *public* `grpcs://` hostname with the lab CA and the JWT policy at
  the edge, bolt stores in a run directory. Always part of the proof; the
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
  the Events API adapter makes no outbound call at start, and the proof
  signs its events with the `signing-secret`) and `agentlab-klaus-gateway-obo`
  (`state-key`, `store-key` — random, generated once; `store-key` seals every
  link in the link Secret, so a new key would orphan them). Then it turns
  `components.klaus-gateway` on with the `klausGateway:` block of
  `state/agent-platform-values.yaml`: `a2a` on the in-cluster target,
  `namespace: kagent`, the proof's fixture `agentlab-klaus-gateway-test` as
  `defaultAgent` (the Slack adapter refuses to start without one; the name
  resolves on a turn, never at start); `slack.enabled` in `events` mode on
  the placeholder Secret (no `web` key: the Slack-only chart's closed schema
  has none); `obo.enabled` with
  `store: secret`, `musterUrl` the lab muster's public URL, `callbackBaseUrl`
  `https://agentgateway.<domain>` (no port: the connectivity chart derives
  the hostname of the OBO route it renders on the edge from it, and a
  Gateway API hostname carries none) and `existingSecret` the keys Secret;
  `serviceMonitor.enabled` following `platform.observability`. The meta
  chart's own `klausGateway` block keeps the static lifecycle driver, no
  second agentgateway and the disruption guards. The chart's edge route for
  the channels (`agentgatewayRoute`) stays off — the public leg is the
  host-mode proof's.
- **The component's Slack Web API** is the proof's fake: the lab's one
  patch on the component (a `postRenderers` strategic merge, HACKS.md U26)
  sets `KLAUS_GATEWAY_SLACK_API_BASE`, which the chart has no value for, to
  `http://agentlab-slack-api.agent-platform.svc.cluster.local/api` — a
  selector-less Service that `klaus-gateway-test` points at its fake while it
  runs and removes afterwards. Nothing calls it otherwise: the Events API
  adapter calls the Web API only for an event. With the component on, the
  fake runs as a container on the `kind` docker network — the proof's own
  binary (`agentlab slack-fake`, hidden) in the lab's probe image, the
  EndpointSlice on its address — so pods reach it container to container and
  a host firewall never sees the traffic; the proof reaches it through a port
  published on loopback. The binary must be a static Linux build (`make
  build`, or `CGO_ENABLED=0 go build`; the releases are), or name one with
  `--slack-fake-binary`. A probe pod fetches the fake through the Service
  before anything else runs.
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
   above), mounts no `obo-store` volume, is not `Recreate` and carries the
   lab's Slack Web API base; Role
   `klaus-gateway-obo-links` has one rule — the core group's `secrets`,
   `resourceNames: [klaus-gateway-obo-links]`, exactly `get`/`update`/`patch`;
   the RoleBinding binds the Deployment's ServiceAccount to it; the link
   Secret carries `helm.sh/resource-policy: keep`.
7. **The store.** Two link records are written **through the gateway's own
   store package** (`github.com/giantswarm/klaus-gateway/pkg/auth/musterlink`,
   `SecretStore.Put`) with the lab's `store-key`, so they are sealed byte for
   byte as the gateway seals them (AES-256-GCM, the JSON of the link); the
   Secret's count grows by two and both read back unchanged. The first is the
   person's link — the Dex subject and e-mail, the lab user's id_token as its
   cached token with its expiry — filed under the Slack user step 9 sends
   as; the second a lab user's with a fake refresh token. Their Slack user
   ids carry the prefix `UAGENTLABTEST`; a leftover of an aborted run is
   removed first, by that prefix (or the earlier `agentlab-klaus-gateway-test-`).
8. **Pod loss.** The pod is deleted — the node-loss shape of the incident the
   Secret store fixed, not a rolling restart. The replacement must be Ready
   within 60 s (the elapsed time is reported; the claim needed minutes when
   the node was gone), and its `obo link store ready` record must report the
   same link count; both records read back unchanged afterwards.
9. **The in-cluster leg.** Through a port-forward to the replacement pod,
   one signed `@bot` mention as the person linked in step 7 is answered in
   the fake thread with the word, its `turn_dispatch` record under the
   person's e-mail and subject — the component forwarding the link's cached
   id_token and talking to the controller over the in-cluster plaintext h2c
   target, which the host-mode gateway never touches.

Then the proof's records are removed (the count is back where it was), and
the host-mode cleanup takes the fixtures and the AgentInstances with it. On a
lab without the component the proof says so and passes on the host-mode
assertions alone.

## What the lab cannot prove

**A real OBO sign-in, and a link's refresh.** muster refuses a CIMD
`client_id` on a private-IP hostname (`*.127.0.0.1.nip.io`), so the
gateway's `/auth/slack/link` flow can be started but never completes here,
and no muster refresh token exists for the gateway to spend. The proof links
the person the way a sign-in leaves them linked — a record in the link store,
written through the gateway's own package, whose cached id_token is the lab
user's Dex id_token and whose expiry is that token's `exp` — and the gateway
forwards it unmodified: `TokenFor` serves a cached, unexpired id_token
without calling muster, the path a linked person's turn takes between
refreshes. The proof requires the token to have an hour left at the start
(the gateway's refresher would spend the placeholder refresh token five
minutes before the expiry) and fails on any `token_refresh` record. The
refresh itself (muster's token endpoint, the rotation, `invalid_grant`
dropping the link) is the gateway's unit tests' and an installation's; that
a linked person is not asked to sign in again after a pod loss is an
installation proof too. Should a lab sign-in ever matter, the way is a
registered client instead of CIMD: a DCR client at muster's
`POST /oauth/register`, `KLAUS_GATEWAY_OBO_CLIENT_ID` on the gateway, and the
lab CA through `SSL_CERT_FILE` — none of it wired here.

**Slack itself**: the fake answers what the gateway asks (`auth.test`,
`users.info`, `{"ok":true,"ts":…}` for the rest) and renders nothing, so how
Slack draws a stream, a card or the native stop button, the scopes the app
manifest grants and Slack's own event retries are verified on the first
installation that runs the release, per the agentlab-first exception.

**The bolt → Secret import** (`obo.storePath` with the Secret store, the
first start reading the file): the import needs the bolt file on a claim the
component no longer mounts; the lab proves the store the fleet ends up on.

**The egress policy** of the connectivity chart (the gateway's route to the
kube-apiserver for the Secret store, giantswarm/agent-platform#443): the lab
runs no Cilium and cannot see it.
