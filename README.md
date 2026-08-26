# dex-lab

A self-contained OIDC lab: **kind + Dex**, with users that exist nowhere but
this cluster. No GitHub, no GitLab, no Google, no Keycloak.

Dex is the OIDC provider. The Kubernetes apiserver trusts it. RBAC is driven by
the `groups` claim that Dex puts in the id_token.

## Requirements

`docker`, `kind` (>= 0.31), `kubectl`, `openssl`, `curl`, `jq`, `python3`.

## Quick start

```bash
make up      # ~1 min: certs, kind cluster, Dex
make test    # asserts RBAC for all three users
```

Then log in as a lab user:

```bash
make login USER_EMAIL=dev@lab.local     # headless, instant
make browser                            # real Dex login page in the browser
export KUBECONFIG=$PWD/kubeconfig.oidc
kubectl auth whoami
```

Tear down with `make down`.

## The users

Password for all three is `password`.

| User | Groups in the token | Effective access |
|---|---|---|
| `admin@lab.local`  | `platform-admins`, `developers` | `cluster-admin` |
| `dev@lab.local`    | `developers` | `edit` inside `ns/demo` only |
| `viewer@lab.local` | `viewers` | `view` cluster-wide |

They are defined in `manifests/dex.yaml` under `staticPasswords`. To add one,
edit that file and run `make reload`. Generate the bcrypt hash with:

```bash
htpasswd -bnBC 10 "" 'your-password' | tr -d ':\n'
```

## How it works

```
                    issuer: https://127.0.0.1:32000/dex
                                    |
  Mac: localhost:32000 ---> kind port mapping ---.
                                                 +--> NodePort 32000 --> Dex :5556
  apiserver static pod (hostNetwork) ------------'
       127.0.0.1:32000
```

The single trick that makes this work is the **shared issuer URL**. The
apiserver has to validate tokens against the same URL the browser was
redirected to. Because the apiserver static pod runs with `hostNetwork`, its
`127.0.0.1:32000` lands on the node's NodePort — and kind maps that same port
onto the Mac. One URL, valid from both sides.

Everything else follows from that:

- `scripts/gen-certs.sh` mints a CA and a server cert with `IP:127.0.0.1` in the SAN.
- `kind-config.yaml` bind-mounts `certs/` into `/etc/kubernetes/pki/dex` on the
  node. kubeadm already mounts `/etc/kubernetes/pki` into the apiserver pod, so
  a subdirectory of it is the one place the CA is visible without extra plumbing.
- The apiserver gets `--oidc-issuer-url`, `--oidc-client-id`, `--oidc-ca-file`,
  `--oidc-username-claim=email`, `--oidc-groups-claim=groups` and `oidc:` prefixes.
- `manifests/rbac.yaml` binds `Group: oidc:platform-admins` (etc.) to ClusterRoles.

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

## Gotchas that cost time

- **A client certificate beats a bearer token.** `kubectl --token=...` against
  the kind kubeconfig silently keeps authenticating as `kubernetes-admin`. You
  need a kubeconfig with no client cert — that is what
  `scripts/mk-kubeconfig.sh` builds. This will produce convincing false
  positives in a test suite if you miss it.
- **kind 0.31 still emits `kubeadm.k8s.io/v1beta3`**, even for Kubernetes 1.35,
  where `extraArgs` is a *map*. v1beta4 changed it to a list of name/value
  pairs. A patch with the wrong apiVersion is ignored **silently** — no error,
  the flags just never appear. Verify with:
  ```bash
  docker exec dexlab-control-plane grep oidc /etc/kubernetes/manifests/kube-apiserver.yaml
  ```
- **The apiserver gives up on OIDC discovery after ~40 seconds.** On a cold
  `kind create`, Dex does not exist yet, so the authenticator logs
  `oidc: authenticator not initialized` four times and then stays broken
  forever — every token is rejected and nothing retries. `make up` bounces the
  static pod once Dex is serving (`make restart-apiserver` if you ever need it
  by hand). This is the single most confusing failure in this setup, because
  the apiserver looks perfectly healthy.
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

There is a spare `backstage` static client in `manifests/dex.yaml`:

```
issuer:        https://127.0.0.1:32000/dex
client id:     backstage
client secret: backstage-lab-secret
redirect URI:  http://localhost:7007/api/auth/oidc/handler/frame
```

It lists `kubernetes` in `trustedPeers`, so it can mint cross-client tokens the
apiserver will accept. Add more clients under `staticClients` and `make reload`.

## If you outgrow static passwords

`staticPasswords` means editing YAML and reloading. If a demo needs users
created live, put a lightweight LDAP behind Dex's `ldap` connector instead:

- [lldap](https://github.com/lldap/lldap) — has a web UI, ships an
  [official Dex example config](https://github.com/lldap/lldap/blob/main/example_configs/dex_config.yml).
- [glauth](https://github.com/glauth/glauth) — config-file only, stateless,
  more GitOps-friendly.

Both give real groups on any Dex version. Keycloak is not needed for this.

## Layout

```
kind-config.yaml           cluster + port mapping + apiserver OIDC flags
manifests/dex.yaml         Dex config (users, clients), Deployment, NodePort
manifests/rbac.yaml        ClusterRoleBindings keyed on OIDC groups
scripts/gen-certs.sh       CA + server cert with IP:127.0.0.1 SAN
scripts/up.sh down.sh      lifecycle
scripts/login.sh           headless login (password grant)
scripts/login-browser.py   authorization-code flow through the browser
scripts/mk-kubeconfig.sh   token-only kubeconfig
scripts/test.sh            RBAC assertions for all three users
```
