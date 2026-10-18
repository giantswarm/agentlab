# klaus-gateway

Swarmgeist ([giantswarm/klaus-gateway](https://github.com/giantswarm/klaus-gateway)),
the fleet's Slack bridge, in the lab. It runs its conversations on the kagent
controller — A2A v1 over gRPC through agentgateway, the roster from
`ListAgents`, one `Session` per Slack thread, human in the
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
- **The component trusts the lab CA for muster**: it dials muster's public
  URL for every Session (agent-manager's tools), the workspace listing and the
  OBO discovery, and inside a pod that is the edge, whose certificate the lab
  CA signs. A `postRenderers` strategic merge (HACKS.md U32) mounts Secret
  `agent-platform/dex-ca` read-only at `/etc/klaus-gateway/lab-ca` and sets
  `SSL_CERT_FILE` to its `ca.crt`; the image's own roots stay.
- **The component's Slack Web API** is the proof's fake: a second
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
   above), mounts no `obo-store` volume, is not `Recreate`, carries the
   lab's Slack Web API base and trusts the lab CA for muster; Role
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
   target, which the host-mode gateway never touches. Its Session starts
   through agent-manager over muster's public URL, verified with the lab CA;
   a failed turn names that URL and the CA source. With the
   workspace-manager behind muster, the person holds the grant of the App
   muster pins it to before the mention, whatever an earlier step or run
   left: the gateway lists a new thread's workspaces (klaus-gateway 4.9.0 and
   later hold a listing muster refuses behind the Connect prompt). On the
   lab's GitHub the step grants it and drops the grant after the turn; a
   real provider's App the person must have granted, else the run fails with
   muster's sign-in link. A workspace prompt (the person has workspaces,
   whatever an earlier proof left in their name) is answered with No
   workspace: the turn proves the gateway, not a workspace.

Then the proof's records are removed (the count is back where it was), and
the host-mode cleanup takes the fixtures and the Sessions with it. On a
lab without the component the proof says so and passes on the host-mode
assertions alone.

## Workspaces per thread

`agentlab klaus-gateway-test --workspaces` adds the workspace a thread's
Session works in, after the host-mode assertions and before the component.
It needs the workspace-manager (`configure --workspaces`, the provider
instance `fake`) and fails before anything runs without it, naming why; the
rest of the proof is the same with or without the flag. The host gateway
restarts on its stores with `--workspace-tools-prefix=x_workspace-manager_`
and `agentlab-klaus-gateway-test-ws`, an Agent on the `kagent` Harness that
may reach the lab's GitHub, as its default agent; the person and
`dev@lab.local` (the colleague, linked too) connect to `fake`. Two
workspaces: `agentlab-gateway-ws` over the public `agentlab-oss/docs` and
`agentlab-oss/kagent`, and `agentlab-gateway-ws-private` over
`agentlab-org/platform-api` and the private `agentlab-org/platform-infra`,
which only the admin reads (the second only where the App of the lab's
GitHub is installed on `agentlab-org`; otherwise steps Wf and Wg are skipped
with the reason).

- **Wa.** A new thread's mention is answered by the workspace prompt and
  nothing else: no turn, no Session. Picking `agentlab-gateway-ws` starts a
  Session on its volume, and the held message's turn lists `/workspace`
  holding a clone of each repository; the volume holds the Session's
  directory with its setup record naming the same clones.
- **Wb, Wc.** Two more turns, and one after a gateway restart, ask nothing
  and run on the same Session (one `session_bound`, one `workspace_chosen`).
- **Wd.** *No workspace* starts a Session with no volume, in an empty
  working directory with no mirrors.
- **We.** The slash command (`/swarmgeist`) opens the agent picker with the
  Workspace select preset to *No workspace*; a submission with the agent and
  `agentlab-gateway-ws` starts the Session in it, the thread asks nothing.
  Reopened, the preset is that workspace with its "Last used with" hint; a
  change of agent redraws it to *No workspace*, the change back to the
  workspace.
- **Wf.** The colleague picks the private workspace: refused privately,
  naming `agentlab-org/platform-infra` and not the repository they read; kagent
  (as them), agent-manager's `list_sessions` and Substrate hold nothing new,
  and the thread is bound to no Session.
- **Wg.** The admin's thread on the private workspace; the colleague
  mentions in it, the admin allows them, and the colleague's turn is refused
  privately naming the repository: no `turn_dispatch` of theirs, the
  Session's task count unchanged.
- **Wh.** The colleague without the grant: their grant of the App muster
  pins the workspace-manager to is dropped (`core_auth_logout`), and muster
  answers `list_workspaces` and `authorize_workspace` with `auth_required`
  and its sign-in link (`/oauth/proxy/start`), not the manager's
  `connect_required`. A new thread of theirs gets the Connect prompt
  privately, its `connector_connect` button opening muster's sign-in link,
  with no workspace prompt and no "could not be listed" note. A gateway
  release before 4.9.0-rc.1 (the first with that prompt) is not judged on the
  prompt, and the PASS line says so; a dev build always is.

The steps' Sessions, Agent, workspaces and provider sign-ins are removed
before the component runs, and the run fails on any of them left.

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

## Decisions

`agentlab decisions-test` proves klaus-gateway's decisions and conversations: a service puts a
question to a person or a team through `POST /decisions`, and the answer calls
a muster tool as the person who gave it. The gateway runs on the host as in
`klaus-gateway-test`, with `--reviews-enabled` and the proof's ServiceAccount
(`agent-platform/agentlab-decisions`, created and deleted by the run) as the
one allowed caller; its token has the audience `klaus-gateway` and the gateway
verifies it with a TokenReview through the lab's kubeconfig. The gateway
trusts the lab CA (`SSL_CERT_FILE`), since every answer is a call to muster.

1. No token is 401; a person no member of the fake workspace has the email
   of is 422.
2. A team decision renders its question as a header and a Choose button per
   option, the recommended one primary; a click answers it, the message is
   rewritten to `Answered by <@…>`, and muster's log shows the forwarded
   id_token accepted and the tool call under the user's subject.
3. A decision for the user by email goes to a direct message (the fake names
   it D… as Slack does); the own-words button opens the modal, and its
   submission with an option and text is the answer.
4. A reply in a team decision's thread is the answer.
5. An answer tool muster does not have is refused: the refusal is the status
   line under the buttons and the decision stays open.
6. `POST /decisions/{id}/close` with `defaulted` removes the buttons; a
   click on the old message is told privately that it was not answered in
   time.
7. `POST /conversations` opens a conversation with the user: a direct message
   whose opening text is a markdown block with `<from> · reply in this
   thread` under it. The user's reply in its thread calls a muster tool as
   them (muster's log shows the forwarded id_token and the call under the
   user's subject), is marked delivered with a reaction and gets no note;
   `POST /conversations/{id}/messages` lands in the same thread, and an
   unknown conversation is 404.
8. A reply whose tool muster does not have gets a note in the thread:
   `Not delivered to <from>: <reason>`.

How Slack renders the message and the modal, and the `users:read.email`
scope a person's decision needs, are verified with the installation's Slack
app, per the agentlab-first exception.
