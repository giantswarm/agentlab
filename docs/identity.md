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

Users are fully configurable: `agentlab configure` lets you keep, edit, remove
and add users, with per-user passwords and group membership. The **groups are
a fixed vocabulary** — RBAC binds exactly `platform-admins` → cluster-admin,
`developers` → edit-in-demo, `viewers` → view — and the form only offers those
three. Passwords are bcrypt-hashed automatically; the hash is cached in
`agentlab.yaml` so renders stay deterministic (no spurious Dex pod rolls). After
editing users, run `agentlab reload`.

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

Dex rotates its signing key every 6 hours (its default; the lab sets no
`expiry`). It checks every 30 seconds whether `nextRotation` of its stored
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
