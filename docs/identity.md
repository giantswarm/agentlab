# Identity: Dex, users and RBAC

The lab bundles its own Dex so the platform has an identity provider that
exists nowhere else: static users, a fixed group vocabulary bound to RBAC,
and one issuer URL that the apiserver, muster and Backstage all trust. This
page is the reference for that layer — who the users are, why the plumbing is
shaped the way it is, and how to hang another client off the same Dex.

## The users

The default configuration ships three users (password: `password`):

| User | Groups in the token | Effective access |
|---|---|---|
| `admin@lab.local`  | `platform-admins`, `developers` | `cluster-admin` |
| `dev@lab.local`    | `developers` | `edit` inside `ns/demo` only |
| `viewer@lab.local` | `viewers` | `view` cluster-wide |

`agentlab configure --tenants` adds two more, one per organization of
`agentlab tenants-test`, when no user carries their group:

| User | Groups in the token | Effective access |
|---|---|---|
| `alice@lab.local` | `tenant-a` | the platform tenant; with `tenants-test`, `agent-platform:user` in its first organization |
| `bob@lab.local`   | `tenant-b` | the platform tenant; with `tenants-test`, `agent-platform:user` in its second organization |

Users are fully configurable: `agentlab configure` lets you keep, edit, remove
and add users, with per-user passwords and group membership. The **groups are
a fixed vocabulary** — RBAC binds exactly `platform-admins` → cluster-admin,
`developers` → edit-in-demo, `viewers` → view, and nothing to `tenant-a` and
`tenant-b`, which `tenants-test` binds per organization — and the form only
offers those five. Passwords are bcrypt-hashed automatically; the hash is cached in
`agentlab.yaml` so renders stay deterministic (no spurious Dex pod rolls). After
editing users, run `agentlab reload`.

## The tenant model

What `agentlab tenants-test` asserts about who sees and runs which agent
(giantswarm/agentlab#166; the plan is giantswarm/giantswarm#36698):

- **A tenant is a namespace.** The platform tenant is `kagent`; an
  organization's tenant is a namespace labelled `giantswarm.io/organization`.
- **Kubernetes RBAC decides, at the kagent controller.** The chart's three
  ClusterRoles are bound per tenant: `agent-platform:user` (see the agents,
  start conversations), `agent-platform:editor` (also write agents),
  `agent-platform:admin`. The chart binds the platform tenant's users to
  `system:authenticated`, so every signed-in person sees its agents. The
  controller (`kagent.controller.authorization.mode: kubernetes`, kagent 1.19)
  asks the API server with a SubjectAccessReview for the caller's email and
  the `groups` claim of the token the edge verified, each with the `oidc:`
  prefix the lab's API server puts before them
  (`kagent.controller.authorization.usernamePrefix` and `groupsPrefix` in the
  lab's values; the fleet's API servers use none).
- **What a person sees**: `ListAgents` with no namespace lists the agents of
  every tenant the person may `list` agents in — the platform tenant and the
  organizations bound to their groups — and nothing else; a person bound to
  no organization lists the platform tenant only.
- **What a person runs**: `CreateSession`, and a first A2A message with no
  Session, on an agent of a tenant the person is not bound to are
  `PermissionDenied`, and no turn reaches the model.
- **What a person writes**: an editor creates, changes and deletes agents in
  their organization and nowhere else; a user writes nothing.
- **Everyone's sessions**: `ListSessions` with `all_creators` needs a
  cluster-wide `list` on `sessions.api.kagent.dev`, which no tenant role
  grants: refused to a person, answered to a cluster-admin (and to
  workspace-manager's ServiceAccount, which the chart grants it with
  workspaces on).
- **Slack asks as the person**: klaus-gateway (4.10.0 and later) fetches each
  person's roster as that person, across tenants, each row naming its tenant
  (`platform`, or `organization <name>` for `org-<name>`), and refuses an
  agent of another tenant — a pick, or a turn as a thread's collaborator —
  with a note naming the tenant, sending nothing to it.

A person with a cluster-wide read (`viewers`, `platform-admins`) sees every
tenant, as on an installation.

### MCP servers per tenant

With `platform.tenants` on, muster serves the `MCPServer`s of the
organization namespaces (`org-…`) next to its own namespace's
(`muster.muster.mcpServersInAllNamespaces`, muster 5.48.0 or later, where the
chart's range starts from agent-platform 4.157.1 on). What `tenants-test` asserts (the
agent-permissions plan's D9):

- **Kubernetes RBAC decides what a person sees.** muster lists the
  `MCPServer`s of each namespace with the person's own Dex token: a person
  sees the servers of every namespace where they may `list`
  `mcpservers.muster.giantswarm.io` — muster's namespace through the chart's
  `agent-platform:mcpserver-viewer` binding to every signed-in person, an
  organization through `agent-platform:user` or `:editor` there. A person in
  no organization sees muster's namespace only. The token must carry the
  audience muster checks before it calls the API server
  (`writesAsCaller.kubernetesAudience`): the lab sets `kubernetes`, the client
  id the lab's API server trusts, which every lab token for muster carries
  (the fleet's API servers trust `dex-k8s-authenticator`, muster's default).
- **A server a person may not see does not exist for them**: not in
  `core_mcpserver_list` or `list_tools`, and a call of its tools,
  `core_service_start` and `core_auth_login` for it are answered as for a
  server that does not exist.
- **A tenant server gets no identity from muster**: an `MCPServer` of an
  organization namespace that sets `auth.forwardToken`,
  `auth.requiredAudiences`, `auth.tokenExchange`, `family` or
  `auth.type: sigv4` is not started and reads `Failed` with the reason.
- **A tenant server's sign-in stays in its namespace.** It is keyed
  `<namespace>/<name>`, its tools are `x_<namespace>_<name>_<tool>`, it signs
  in through `<muster>/oauth/ns/<namespace>/callback` with a client of its own
  namespace, and its token is filed under its namespace and issuer: a sign-in
  to it gives no token to a server of the same issuer in muster's namespace or
  in another organization. The lab Dex lists the callbacks of `tenants-test`'s
  two namespaces on the platform client.
- **A tenant namespace's sign-ins go with its servers.** muster forgets them
  when the namespace's last server, or the namespace, goes (a server deleted
  next to others takes the issuers only it named): a namespace created again
  with the same name starts with no sign-in, and
  `core_auth_login` there answers a sign-in link, not a connect. Every
  `tenants-test` run creates its namespaces again, so a second run right
  after the first signs in again.
- **Through an agent, as the person**: an organization's agent bound to
  muster lists, in a turn as the person, the servers that person sees.

What an organization sets up for a server of its own that signs in with a
client of its namespace (`authorizationServer.clientCredentialsSecretRef`):
muster reads that Secret with its own ServiceAccount, and no chart grants it a
read outside muster's namespace (least privilege). So whoever declares the
`MCPServer` also declares, in its namespace, a Role with `get` on that Secret
alone (`resourceNames`) and a RoleBinding of it to muster's ServiceAccount
(`muster` in `agent-platform`), as muster's how-to ["Servers outside muster's
namespace"](https://github.com/giantswarm/muster/blob/main/docs/how-to/connecting-non-rfc9728-mcp-servers.md#servers-outside-musters-namespace)
describes. `tenants-test`'s fixture does this for
`agentlab-tenants-mcp` (the Role and RoleBinding
`agentlab-tenants-mcp-secret-reader`). Without them, muster answers the
sign-in with `Cannot sign in to '<namespace>/<name>': the server's sign-in is
not ready: <reason>`, and the reason names the Secret and the Role.

### The portal per tenant

The Dev Portal (Backstage with giantswarm/backstage#3161) reads the same
model as the person, through its own backend routes. What `tenants-test`
asserts (steps P, [The Agent Platform proof](backstage.md#the-agent-platform-proof)):

- **The roster per tenant.** The portal asks the controller which namespaces
  hold an agent the person may list (`GET /api/agent-platform/kagent/agent-namespaces`,
  `AgentService/ListAgents` with no namespace, as the person, through the
  agentgateway edge's GRPCRoute) and reads the Agents of each through the
  Kubernetes API as the person: a tenant user's roster is their
  organization's agents and the platform tenant's, labelled `organization
  <name>` and `platform`.
- **Where a person may create**: the create flow's tenants are agent-manager's
  `get_info` `namespaces.managed` as the person — an editor of one
  organization gets that one — and a create elsewhere is `forbidden`.
- **What a person may change**: the agent page asks the API server
  (`SelfSubjectAccessReview` for `update` and `delete` on the agent's
  HelmRelease): a user gets no, an editor yes in their organization only.
- **What a person may run**: a session start on another tenant's agent is the
  controller's refusal (`forbidden: kagent: …` from `start_session`), and
  nothing reaches the agent.
- **Who created an agent**: agent-manager records the caller's email as
  `agent-platform.giantswarm.io/created-by`, and the portal shows it. It is
  never an authorization input. `GET /kagent/me` answers the token's claims
  on kagent 1.19: its `email` is the person, its `sub` is Dex's subject. The
  portal's "Created by: Me" compares the creator with that `sub`, so it keeps
  nothing (giantswarm/backstage#3161; the browser suite's `tenants.spec.ts`
  shows it).
- **No tenant, no agent**: a person bound in no organization, with no platform
  agent, reads an empty roster, not an error. The chart binds
  `agent-platform:platform-user` to every signed-in person in `kagent`, so the
  platform tenant is never refused: the portal shows "No agents yet" and its
  invitation to create one, not its "No agents for you yet" naming the roles,
  which needs every namespace refused.

## How the identity plumbing works

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

- `agentlab up` mints the name-constrained lab CA and a Dex server cert
  (crypto/x509) whose SAN carries both `IP:127.0.0.1` and `DNS:localhost`. The
  issuer uses the **name**, not the IP — see
  [Why `localhost` and not `127.0.0.1`](#why-localhost-and-not-127001).
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

## Why `localhost` and not `127.0.0.1`

The issuer was originally `https://127.0.0.1:32000/dex`. muster refuses that:
`mcp-oauth`'s `ValidateIssuerURL` rejects any issuer whose host **parses as a
loopback or private IP**, unconditionally — `allowPrivateIPOIDC` only relaxes the
*dial-time* SSRF guard, not this static check. A hostname that merely *resolves*
to 127.0.0.1 passes, because `net.ParseIP("localhost")` returns nil.

So the lab issuer is `https://localhost:32000/dex`. Nothing else changed: the
Dex cert already carried `DNS:localhost` in its SAN, and `localhost` resolves to
127.0.0.1 on the Mac, inside the kind node, and inside any `hostNetwork` pod — the
same one-URL trick, just spelled with a name.

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

The Dex image is a form question (`dexImage` in `agentlab.yaml`) for when the
next version lands.

## Wiring another app to this Dex

The rendered Dex config carries the shared `agent-platform` static client:

```
issuer:        https://localhost:32000/dex
client id:     agent-platform
client secret: agent-platform-lab-secret
redirect URIs: https://muster.127.0.0.1.nip.io/oauth/callback
               https://backstage.127.0.0.1.nip.io/api/auth/oidc-agent-platform/handler/frame
```

The Backstage redirect path carries the **provider name** from the app-config,
not the literal word `oidc` — Backstage serves each provider at
`/api/auth/<provider>/handler/frame`, and the chart names it
`oidc-agent-platform`. More clients means editing the Dex template
(`internal/lab/templates/dex.yaml.tmpl`), rebuilding and `agentlab reload`.

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

## Signing-key rotation and the apiserver's JWKS cache

The lab's Dex rotates its signing key every 30 days (`signer.config.keysRotationPeriod:
720h`; Dex's default is 6 hours), so a lab that lives for days does not meet
the apiserver's window below every 6 hours. A key that expired while the node
was down still rotates at Dex's first start after it. Dex checks every 30 seconds whether `nextRotation` of its stored
signing keys has passed, mints a new key, and keeps the retired one in its
JWKS as a verification key until the last token it signed has expired:
rotation time plus the id-token lifetime, 24 hours by default
([`rotation.go`](https://github.com/dexidp/dex/blob/v2.45.1/server/signer/rotation.go)).
It serves the JWKS with `Cache-Control: max-age=600, must-revalidate`, a
fixed 10 minutes
([`handlers.go`](https://github.com/dexidp/dex/blob/v2.45.1/server/handlers.go)).

The apiserver discovers the key set from the issuer's discovery document
([Kubernetes: OpenID Connect tokens](https://kubernetes.io/docs/reference/access-authn-authz/authentication/#openid-connect-tokens));
the documentation says nothing about when it fetches it again, which is the
authenticator's code. The lab's apiserver (Kubernetes 1.37, the embedded
kind's node image, configured by the `--oidc-*` flags) verifies tokens with
go-oidc v2.5.0's remote key set
([`oidc.go` at v1.37.0](https://github.com/kubernetes/kubernetes/blob/v1.37.0/staging/src/k8s.io/apiserver/plugin/pkg/authenticator/token/oidc/oidc.go),
[go-oidc `jwks.go` at v2.5.0](https://github.com/coreos/go-oidc/blob/v2.5.0/jwks.go)):

- A token whose key id is in the cached key set verifies from the cache, with
  no fetch, however old the cache is.
- A token with an unknown key id fetches the JWKS again only when the cached
  set is within 30 seconds of its `max-age`. Before that it is refused at once
  with `failed to verify id token signature`, and nothing is fetched. There is
  no schedule: only such a token, after the cache's expiry, makes the fetch.

So after a rotation:

| Token | In the lab |
|---|---|
| Signed by the **new** key | Refused until 9 min 30 s after the apiserver's last JWKS fetch, accepted from the first request after that. A rotation test waits up to **10 minutes**; if the last fetch was more than 9 min 30 s before the rotation, the first such token is accepted at once. |
| Signed by the **retired** key | Accepted until the token's own `exp`. The cached set holds the retired key, and after a fetch Dex's JWKS still carries it for 24 hours. No wait makes it refused earlier; a test of the refusal uses an expired token. |

The wait is the apiserver's, so a rotation test reads it from there. Its last
fetch is the `apiserver_authentication_jwt_authenticator_jwks_fetch_last_timestamp_seconds`
metric (the `StructuredAuthenticationConfigurationJWKSMetrics` gate, on by
default since 1.35;
[Kubernetes metrics](https://kubernetes.io/docs/reference/instrumentation/metrics/)):

```sh
kubectl --kubeconfig state/kubeconfig get --raw /metrics | grep jwks_fetch_last_timestamp
```

A new-key token is accepted from that timestamp plus 570 seconds. Until then
every surface that authenticates to the apiserver with the person's token
answers 401: the portal's inventory read among them, which hides the
installation before the session list runs. That is the lab apiserver's cache,
not a portal failure. The agent gateway's JWT policies keep their own copy of
the JWKS; with its 5-minute refresh, fresh tokens were measured refused for
4 min 47 s after a rotation in this lab
([agentgateway-upstream#46](https://github.com/giantswarm/agentgateway-upstream/issues/46)).

`agentlab platform-test`, `test`, `backstage-test` and `agents-test` start
with the stage `apiserver OIDC`: a fresh token of the first user at `whoami`. Refused while Dex publishes its key, it
is this window: the stage presents a new token every 15 seconds until the
apiserver fetches again, at most 10 min 30 s, and the proof goes on. Any other
refusal fails the stage at once. A rotation is forced for a test by setting
the SigningKey object's `nextRotation` to a past time (`kubectl -n dex patch
signingkeies.dex.coreos.com openid-connect-keys --type merge -p
'{"nextRotation":"<past RFC 3339 time>"}'`); Dex rotates within 30 seconds.

Nothing in the flags shortens the apiserver's wait: the `--oidc-*` flags and
the `AuthenticationConfiguration` file have no JWKS cache setting, and the
`max-age` is Dex's. Kubernetes after 1.37 moves to go-oidc v3, which fetches
again on every unknown key id, rate-limited to six fetches a minute; this
section is re-checked when the embedded kind's node image moves.

## If you outgrow static passwords

To sign in as a real account — yourself, for a demo or for using the portal as
you — turn on the [GitHub sign-in](github-signin.md): a `github` connector on
this same Dex, rendered from `agentlab.yaml` with the App's client secret
placed as a Secret, next to the local users.

`staticPasswords` means editing `agentlab.yaml` and reloading. If a demo needs
users created live, put a lightweight LDAP behind Dex's `ldap` connector
instead:

- [lldap](https://github.com/lldap/lldap) — has a web UI, ships an
  [official Dex example config](https://github.com/lldap/lldap/blob/main/example_configs/dex_config.yml).
- [glauth](https://github.com/glauth/glauth) — config-file only, stateless,
  more GitOps-friendly.

Both give real groups on any Dex version. Keycloak is not needed for this.
