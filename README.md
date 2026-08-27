# dex-lab

A self-contained OIDC lab: **kind + Dex**, with users that exist nowhere but
this cluster. No GitHub, no GitLab, no Google, no Keycloak.

Dex is the OIDC provider. The Kubernetes apiserver trusts it. RBAC is driven by
the `groups` claim that Dex puts in the id_token.

The whole lab is **one Go binary**. It asks for every configuration option
through an interactive form (`dexlab configure`), persists the answers to
`dexlab.yaml`, and renders every manifest from embedded templates — there is no
YAML to hand-edit and no shell to source.

## Requirements

`go` (>= 1.25), `docker`, `kind` (>= 0.31), `kubectl`, `helm`, `git`.

(The old script stack also needed openssl, curl, jq and python3; the binary
does all of that itself.)

## Quick start

```bash
go build -o dexlab .
./dexlab configure   # interactive form: cluster, users, components
./dexlab up          # ~1 min: certs, kind cluster, Dex, RBAC, verification
./dexlab test        # asserts RBAC for every configured user
```

`dexlab configure --defaults` skips the form and writes the canonical lab
(three users, Dex on 32000, optional components off); add `--platform`
and/or `--backstage` to enable components non-interactively. On a plain
terminal or screen reader, `dexlab configure --accessible` runs the form as
one prompt per question.

Then log in as a lab user:

```bash
./dexlab login dev@lab.local     # headless, instant
./dexlab browser                 # real Dex login page in the browser
export KUBECONFIG=$PWD/kubeconfig.oidc
kubectl auth whoami
```

Tear down with `./dexlab down`.

## The users

The default configuration ships three users (password: `password`):

| User | Groups in the token | Effective access |
|---|---|---|
| `admin@lab.local`  | `platform-admins`, `developers` | `cluster-admin` |
| `dev@lab.local`    | `developers` | `edit` inside `ns/demo` only |
| `viewer@lab.local` | `viewers` | `view` cluster-wide |

Users are fully configurable: `dexlab configure` lets you keep, edit, remove
and add users, with per-user passwords and group membership. The **groups are
a fixed vocabulary** — RBAC binds exactly `platform-admins` → cluster-admin,
`developers` → edit-in-demo, `viewers` → view — and the form only offers those
three. Passwords are bcrypt-hashed automatically; the hash is cached in
`dexlab.yaml` so renders stay deterministic (no spurious Dex pod rolls). After
editing users, run `dexlab reload`.

## How it works

```
                    issuer: https://localhost:32000/dex
                                    |
  Mac: localhost:32000 ---> kind port mapping ---.
                                                 +--> NodePort 32000 --> Dex :5556
  apiserver static pod (hostNetwork) ------------'
       localhost:32000
```

The single trick that makes this work is the **shared issuer URL**. The
apiserver has to validate tokens against the same URL the browser was
redirected to. Because the apiserver static pod runs with `hostNetwork`, its
`localhost:32000` lands on the node's NodePort — and kind maps that same port
onto the Mac. One URL, valid from both sides. (32000 is the default; the Dex
port is a form question, constrained to the NodePort range 30000-32767.)

Everything else follows from that:

- `dexlab up` mints a CA and a server cert (crypto/x509) whose SAN carries both
  `IP:127.0.0.1` and `DNS:localhost`. The issuer uses the **name**, not the IP —
  see [Why `localhost` and not `127.0.0.1`](#why-localhost-and-not-127001).
- The rendered kind config bind-mounts `certs/` into `/etc/kubernetes/pki/dex`
  on the node. kubeadm already mounts `/etc/kubernetes/pki` into the apiserver
  pod, so a subdirectory of it is the one place the CA is visible without extra
  plumbing.
- The apiserver gets `--oidc-issuer-url`, `--oidc-client-id`, `--oidc-ca-file`,
  `--oidc-username-claim=email`, `--oidc-groups-claim=groups` and `oidc:` prefixes.
- RBAC binds `Group: oidc:platform-admins` (etc.) to ClusterRoles.

Every manifest the binary applies is also written to `state/` (gitignored), so
`kubectl diff -f state/dex.yaml` and plain reading remain possible. The
templates live in `internal/lab/templates/`.

## Why Dex v2.45.1 specifically

`groups` on `staticPasswords` **landed in Dex v2.45.0** (Feb 2026,
[PR #4456](https://github.com/dexidp/dex/pull/4456), closing
[issue #1080](https://github.com/dexidp/dex/issues/1080) after eight years).
Same release added `name`, `preferredUsername` and configurable `emailVerified`.

On **v2.44.0 and earlier**, `staticPasswords` returns only
`UserID`/`Username`/`Email` with `EmailVerified` hardcoded to `true` — no
groups, which is the historical reason people bolted LDAP or Keycloak onto Dex
just to get a lab going. That is no longer necessary.

Watch out:
- The upstream Helm chart `dexidp/dex` 0.24.1 still defaults to appVersion
  **2.44.0**. You must override `image.tag`.
- `giantswarm/dex-app` v2.2.3 is based on Dex **v2.43.2**, and its template
  hardcodes `enablePasswordDB: false` with no `staticPasswords` support at all.
  This lab therefore uses plain manifests, not `dex-app`.

The Dex image is a form question (`dexImage` in `dexlab.yaml`) for when the
next version lands.

## Gotchas that cost time

- **A client certificate beats a bearer token.** `kubectl --token=...` against
  the kind kubeconfig silently keeps authenticating as `kubernetes-admin`. You
  need a kubeconfig with no client cert — that is what `dexlab login` builds
  (`kubeconfig.oidc`). This will produce convincing false positives in a test
  suite if you miss it.
- **kind 0.31 still emits `kubeadm.k8s.io/v1beta3`**, even for Kubernetes 1.35,
  where `extraArgs` is a *map*. v1beta4 changed it to a list of name/value
  pairs. A patch with the wrong apiVersion is ignored **silently** — no error,
  the flags just never appear. Verify with:
  ```bash
  docker exec dexlab-control-plane grep oidc /etc/kubernetes/manifests/kube-apiserver.yaml
  ```
- **The apiserver keeps retrying OIDC discovery — no bounce needed.** On a
  cold `kind create`, Dex does not exist yet and the apiserver logs
  `oidc authenticator: initializing plugin: … connection refused` — but on
  Kubernetes 1.35 it retries every 10 seconds forever and initializes on the
  first tick after Dex answers (verified empirically; earlier versions of this
  lab bounced the static pod because older apiservers gave up for good).
  `dexlab up`'s verification loop simply waits out the next retry tick. If
  tokens are still rejected minutes after Dex is up, read the apiserver log
  for those `oidc.go` lines rather than restarting things.
- **Scopes are not optional.** `--oidc-username-claim=email` needs the client to
  request the `email` scope and `--oidc-groups-claim=groups` needs `groups`.
  Ask for `openid` alone and the apiserver rejects the token with
  `parse username claims "email": claim not present`, which reads like a
  misconfiguration on the apiserver side but is really a missing scope.
- **Dex needs a writable `/tmp`** even with `readOnlyRootFilesystem: true`; it
  renders its config through a temp file. Hence the `emptyDir`.
- Kubernetes 1.35 still accepts the `--oidc-*` flags. The modern alternative is
  `--authentication-config` (structured `AuthenticationConfiguration`, which
  also supports CEL claim mappings). The flags are simpler and were kept here.

## Wiring another app to this Dex

The rendered Dex config carries a `backstage` static client:

```
issuer:        https://localhost:32000/dex
client id:     backstage
client secret: backstage-lab-secret
redirect URI:  http://localhost:7007/api/auth/oidc-lab/handler/frame
```

The redirect path carries the **provider name** from the app's config, not the
literal word `oidc` — Backstage serves each provider at
`/api/auth/<provider>/handler/frame`. This lab names the provider `oidc-lab`,
so that is what the client must allow. More clients means editing the Dex
template (`internal/lab/templates/dex.yaml.tmpl`), rebuilding and
`dexlab reload`.

### `trustedPeers` points the other way round

To let client A mint a token whose audience client B accepts, A requests the
scope `audience:server:client_id:B` — and **B** must list **A** in its
`trustedPeers`. It is a grant published by the audience, not a capability
claimed by the caller.

So "Backstage may act on the Kubernetes API" is spelled:

```yaml
- id: kubernetes
  trustedPeers:
    - backstage      # <- the *caller* is listed on the *audience's* client
```

Getting this backwards fails closed and loudly, which is the one mercy:

```
$ curl -u backstage:... -d 'scope=...audience:server:client_id:kubernetes' .../token
{"error":"invalid_request",
 "error_description":"Client can't request scope(s) [\"audience:server:client_id:kubernetes\"]"}
```

The lab uses one token for three audiences. `gs.auth.extraScopes` in the
Backstage template asks for both cross-client scopes, so a single Dex id_token
comes back with `aud: ["kubernetes", "muster", "backstage"]` and is accepted by
the apiserver (`--oidc-client-id=kubernetes`), by muster
(`trustedAudiences: [muster]`) and by Backstage itself.

## Backstage

`dexlab backstage` deploys **Giant Swarm's own Backstage** — the build behind
[devportal.giantswarm.io](https://devportal.giantswarm.io/) — into the cluster
with Dex as its **only** identity provider. Not upstream Backstage and not RHDH:
the GS build is the one that carries the first-party `muster` plugin, which is
the whole reason to run a portal in this lab at all.

```bash
./dexlab up
./dexlab platform      # muster must exist first, or the plugin has nothing to talk to
./dexlab backstage     # first run pulls and loads a 2.4GB image
open http://localhost:7007
```

(Or enable both components in `dexlab configure` and a single `dexlab up`
deploys the whole stack.)

The image is published **anonymously** to `gsoci.azurecr.io/giantswarm/backstage`
— no Giant Swarm registry credentials needed. It is `linux/amd64` only, so on an
Apple Silicon Mac it runs through the kind node's Rosetta binfmt handler; expect
a slower first boot, not a failure.

Sign In takes you to the same Dex login page, and you come back as a real
Backstage identity with the groups from the token.

The first click on **Sign In** lands on a browser TLS warning: Dex serves a cert
signed by the lab CA, which the browser does not trust (Backstage itself trusts
it via `NODE_EXTRA_CA_CERTS`, but that only covers the server-to-server hop).
Accept it once for `https://localhost:32000` and the login proceeds normally.
`dexlab backstage-test` sidesteps this entirely by trusting `certs/ca.crt`.

Three things make it work:

- **`hostNetwork: true` on the Backstage pod.** The issuer is
  `https://localhost:32000/dex`, and from inside a normal pod that is the pod's
  own loopback. On the host network it is the node's, which is the Dex
  NodePort — the same URL the browser uses. It is also what puts muster
  (`http://localhost:8090/mcp`) and the apiserver (`https://localhost:6443`)
  within reach, since both are published on the node too.
- **`NODE_EXTRA_CA_CERTS`.** Dex serves a cert signed by the lab CA. Without
  this you do not get a TLS error — you get `Failed to fetch issuer metadata`,
  and then **every** route including `/` answers `503`. See the gotchas below.
- **The provider is named `oidc-lab`, not `oidc`.**
  `plugins/auth-backend-module-gs` registers exactly one login provider, and
  only if its name starts with `oidc-` *and* equals `gs.authProvider`. The name
  also forms the callback path the Dex client has to allow.

### Users need no catalog entity

RHDH's `emailLocalPartMatchingUserEntityName` resolver refuses a login it cannot
map onto a `User` entity. Giant Swarm's resolver only consults the catalog when
Dex reports `federated_claims.connector_id` of `giantswarm-ad` or
`giantswarm-github`. This lab's static-password connector reports `local`, so it
falls straight through to `email.split('@')[0]` and issues
`user:default/<localpart>` regardless. The catalog entities the lab renders
(from your configured users) exist only so the users and groups show up as real
things in the UI.

### The muster plugin

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
[`trustedPeers` points the other way round](#trustedpeers-points-the-other-way-round).

`dexlab backstage-test` drives the whole sign-in headlessly for every
configured user and then proves the muster hop with that user's own token:

```
=== dev@lab.local ===
  dex asserted    groups=[developers] email=dev@lab.local
  token audience  [kubernetes muster backstage]
  backstage user  user:default/dev
  ownership refs  [user:default/dev]
  muster servers  [(dexlab-mcp-kubernetes, Connected)]
  muster workflows [lab-cluster-overview]
  muster core tools 28 exposed
```

### Backstage gotchas

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
- **`ai-chat` ships enabled and has no LLM key here.** Its assistant-ui runtime
  logs five `Maximum update depth exceeded` errors on the signed-out page and
  then React bails out. Cosmetic — the page renders and login works — but it is
  the first thing you will see in the console. Disabling it means restating the
  whole extensions list, per the first gotcha; the lab accepts the noise instead.
- **A muster with no workflows looks like a broken plugin.** The workflow list
  is the plugin's main surface, and a fresh muster has none, so the tab renders
  empty. `dexlab platform` seeds one (`lab-cluster-overview`) that lists
  namespaces and pods through `mcp-kubernetes`, exercising the whole chain from
  one click.
- **`allowMutations` no longer exists.** `app-config.example.yaml` still
  documents `muster.installations[].allowMutations` as a read-only safety gate;
  it was removed and nothing reads it. The real guard is the downstream MCP
  server's RBAC — and `mcp-kubernetes` here uses the chart's `standard` profile,
  which **can write**. Drop it to `readonly` in the mcp-kubernetes values
  template for a read-only demo.

## Giant Swarm agent platform (muster + Kubernetes MCP)

Runs Giant Swarm's
[agent-platform-standalone](https://github.com/giantswarm/agent-platform-standalone)
umbrella chart against this Dex, so Claude Code can drive a Kubernetes MCP
server living inside the kind cluster. It is one plain Helm chart — muster,
valkey and the MCP registrations are pinned subcharts (`Chart.lock` is the
BOM) — so unlike the old agent-platform meta-package there is **no Flux** and
no HelmRelease indirection anywhere in this lab.

The chart has no release yet
([PR #11](https://github.com/giantswarm/agent-platform-standalone/pull/11)), so
`dexlab platform` vendors it from git at a pinned SHA (`platform.apsRef` in
`dexlab.yaml`) into `vendor/` and installs from the local path. Once released,
that step becomes a plain `helm install oci://…/agent-platform-standalone`.

```bash
./dexlab platform       # vendors the chart + muster + valkey + mcp-kubernetes
./dexlab platform-test  # headless proof of the whole chain
```

muster runs with `hostNetwork`, so it binds `:8090` on the node, and the
rendered kind config publishes that onto the Mac (host port `platform.musterPort`,
default 8090) — no port-forward. Port mappings are fixed at node-creation time,
so changing the port means `dexlab down && dexlab up`; the stopgap on an old
cluster is `kubectl -n agent-platform port-forward svc/muster 8090:8090`.

Then point Claude Code at it (`.mcp.json` in this repo already does):

```bash
claude mcp add --transport http muster http://localhost:8090/mcp
# in Claude Code:  /mcp  ->  authenticate  ->  Dex login page  ->  done
```

### The request path

```
Claude Code ──http://localhost:8090/mcp──> muster ──> mcp-kubernetes ──> kube-apiserver
                     │                       │
                     │ 401 + WWW-Authenticate│ OIDC discovery + token exchange
                     └──── browser ──────────┴──> https://localhost:32000/dex
```

muster is the OAuth **server** towards Claude Code (DCR, `/oauth/authorize`,
`/oauth/token`) and an OAuth **client** of Dex. The `muster` staticClient in
the rendered Dex config closes that loop; its `redirectURIs` must equal
`<oauth.server.baseUrl>/oauth/callback`.

`mcp-kubernetes` is deliberately unauthenticated on the cluster network
(`auth.mode: none` on the `MCPServer` CR) and talks to the apiserver with its own
ServiceAccount. muster is the single enforcement point.

### Why `localhost` and not `127.0.0.1`

The issuer was originally `https://127.0.0.1:32000/dex`. muster refuses that:
`mcp-oauth`'s `ValidateIssuerURL` rejects any issuer whose host **parses as a
loopback or private IP**, unconditionally — `allowPrivateIPOIDC` only relaxes the
*dial-time* SSRF guard, not this static check. A hostname that merely *resolves*
to 127.0.0.1 passes, because `net.ParseIP("localhost")` returns nil.

So the lab issuer is now `https://localhost:32000/dex`. Nothing else changed: the
Dex cert already carried `DNS:localhost` in its SAN, and `localhost` resolves to
127.0.0.1 on the Mac, inside the kind node, and inside any `hostNetwork` pod — the
same one-URL trick, just spelled with a name.

### Lab-specific deviations from a real management cluster

| What | Why |
|---|---|
| `ingress.mode: muster-direct` (+ `components.agentgateway.enabled: false`, the default) | muster serves `/mcp` itself. The `agentgateway-muster` topology needs the agentgateway controller and a `GatewayClass`. |
| `ingress.parentRefs` set to a placeholder, rendered `HTTPRoute` stripped | The chart hard-fails on empty `parentRefs` in **all** modes — even `muster-direct` assumes a public Gateway. This lab has no Gateway at all (hostNetwork + kind port mapping), so the values carry a placeholder to pass the guard and `dexlab post-render` strips the route. Also spares the lab the Gateway API CRDs, its only would-be consumer. |
| `agent-platform-mcps.agentgateway.enabled: false` | The umbrella defaults it to **true**; left on it renders `AgentgatewayBackend` CRs whose CRD is not installed and the release fails. |
| `components.dicebear.enabled: false` | On by default; the avatar service is only reachable through the agentgateway edge this lab does not run. |
| `networkPolicy.enabled: false`, `kyvernoPolicies.enabled: false` | The umbrella's own policy objects. No Cilium and no Kyverno in kind, so both would render CRs whose API groups the cluster does not serve. |
| `valkey.valkey.metrics.podMonitor.enabled: false`, `muster…serviceMonitor.enabled: false`, `muster…prometheusRule.enabled: false` | No Prometheus Operator, so `PodMonitor` / `ServiceMonitor` / `PrometheusRule` have no CRD. |
| `muster.rbac.{mcpServerEditor,workflowEditor}.subjects` → `oidc:platform-admins` | The umbrella binds muster's editor Roles to Giant Swarm's admin groups, which do not exist here. Rebound to the lab's own admin group (`--oidc-groups-prefix=oidc:`, same spelling as the lab RBAC). Lists replace, so the GS groups are dropped. |
| muster patched to `hostNetwork` + `maxSurge: 0` | Same issuer trick as the apiserver and Backstage. `maxSurge: 0` because two hostNetwork pods cannot both bind `:8090` on a one-node cluster. Applied by `dexlab post-render` (`helm --post-renderer`, the binary invoking itself) — the plain-Helm replacement for the Flux `postRenderers` the meta-package forwarded to helm-controller. |
| The chart vendored at a pinned git SHA | Component versions are the chart's own tested BOM (`Chart.lock`); the lab no longer pins its own. The only lab-side pin is `platform.apsRef` — the chart repo commit — so two runs still install the same thing. |

### Platform gotchas

- **Switching from the old meta-package needs a clean slate.** Both installs use
  the Helm release name `agent-platform`, but the old one rendered Flux
  HelmReleases and the new one renders the workloads directly — upgrading across
  that boundary races helm-controller uninstalls against the fresh install.
  `dexlab platform` refuses if it finds the old HelmReleases; run
  `dexlab platform-down` first (an old cluster keeps its now-idle `flux-system`,
  which is harmless).
- **`allowPublicClientRegistration` is a no-op in the muster chart (still at
  5.5.6).** It exists in `values.yaml`, `values.schema.json`, the README table
  and the chart's unit tests, but `templates/configmap.yaml` never renders it —
  so DCR stays gated and Claude Code's login dies at `/oauth/register` with
  *"Registration requires authentication"*. Neither of the other gates can help:
  Claude Code cannot send a registration token, its loopback port is random (so
  `trustedPublicRegistrationRedirectURIs` cannot match), and `http`/`https` are
  deliberately stripped from `trustedPublicRegistrationSchemes` by mcp-oauth's
  config validation. `dexlab post-render` works around it by editing the key
  into the rendered config — surgically, so everything else in the ConfigMap
  stays chart-rendered and muster bumps via the chart need no hand-copying (the
  old meta-package setup froze the whole ConfigMap and had to pin muster for it).
- **A `Recreate` strategy cannot be patched onto an existing Deployment.** The API
  server has already defaulted `spec.strategy.rollingUpdate`, and a patch that
  flips the type without also deleting that field fails with
  `rollingUpdate: Forbidden`. `maxSurge: 0` achieves the same thing without the
  conflict.
- **A disabled kagent still creates its namespace.** The umbrella's
  `templates/namespace.yaml` is gated only on `kagent.kagent.namespaceOverride`,
  not on `components.kagent.enabled`, so an empty `kagent` namespace appears.
  Harmless — Helm owns it and removes it on uninstall.
- **Family tools need an instance argument.** The `kubernetes` group is rendered as
  a muster *family* (`instanceArg: management_cluster`), so calls are
  `call_tool(name=x_kubernetes_list, arguments={management_cluster: "dexlab-mcp-kubernetes", ...})`.
  The instance is the **`MCPServer` CR name**, not the `cluster:` field of the
  `mcpServers` entry.
- **Tool results are double-wrapped.** `result.content[0].text` is JSON whose
  `content[0].text` is the actual payload — two decode hops.

## If you outgrow static passwords

`staticPasswords` means editing `dexlab.yaml` and reloading. If a demo needs
users created live, put a lightweight LDAP behind Dex's `ldap` connector
instead:

- [lldap](https://github.com/lldap/lldap) — has a web UI, ships an
  [official Dex example config](https://github.com/lldap/lldap/blob/main/example_configs/dex_config.yml).
- [glauth](https://github.com/glauth/glauth) — config-file only, stateless,
  more GitOps-friendly.

Both give real groups on any Dex version. Keycloak is not needed for this.

## Layout

```
main.go                        the CLI (cobra): one subcommand per lifecycle step
internal/config/               dexlab.yaml schema, defaults, validation
internal/forms/                the interactive configuration forms (huh)
internal/lab/                  everything operational:
  certs.go                       CA + server cert (IP:127.0.0.1 + DNS:localhost SAN)
  up.go down.go                  lifecycle; checksum-stamped Dex apply
  test.go                        RBAC assertions for every configured user
  login.go browser.go            password grant / authorization-code flow
  platform.go platformtest.go    agent platform install + MCP smoke test
  backstage.go backstagetest.go  Backstage deploy + headless sign-in proof
  postrender.go                  helm post-renderer (hostNetwork, DCR fix, route strip)
  templates/                     every manifest, rendered from dexlab.yaml
dexlab.yaml                    your configuration (gitignored; `dexlab configure`)
state/                         rendered manifests, for inspection (gitignored)
vendor/                        agent-platform-standalone checkout (gitignored)
.mcp.json                      registers muster as an MCP server for Claude Code
```

Useful passthroughs: `dexlab logs dex|muster|backstage` tails logs; the
effective Dex config is `kubectl -n dex get secret dex-config -o jsonpath='{.data.config\.yaml}' | base64 -d`,
muster's is `kubectl -n agent-platform get cm muster-config -o jsonpath='{.data.config\.yaml}'`.
