# Workspaces: the lab's GitHub

A workspace is a git repository a Session works in, cloned, pushed and turned
into a pull request as the signed-in person. Proving that end to end needs a
GitHub with an App, OAuth sign-in for several people with different access, a
private repository, real pushes and pull requests. Against github.com that
means browser consent per account and real accounts; the lab instead serves a
GitHub of its own, `agentlab github-fake --workspaces`, headless and the same
on every machine. A workspace provider instance points at it the way it points
at a GitHub Enterprise Server: API base `https://<fake>/api/v3`, git base
`https://<fake>`.

## The fixture

What the fake holds is a fixture file; the embedded default is
[`internal/lab/templates/github-fixture.yaml`](../internal/lab/templates/github-fixture.yaml)
and `--fixture <file>` replaces it.

**The App**: id `1`, slug `agentlab-workspaces`, client id
`Iv1.agentlab-workspaces`, installed on both owners (installation `1` on
`agentlab-org`, `2` on `agentlab-oss`), with contents and pull request write.

**The repositories**, each one commit on `main`:

| Repository | Visibility | Language | Topics | Archived | Fork | `pushed_at` |
|---|---|---|---|---|---|---|
| `agentlab-org/platform-api` | public | Go | api, go | | | 2026-09-01 |
| `agentlab-org/platform-infra` | **private** | HCL | infrastructure | | | 2026-08-15 |
| `agentlab-org/monorepo` | public, **large** | Go | monorepo | | | 2026-09-20 |
| `agentlab-org/legacy-tool` | public | Python | legacy | yes | | 2025-01-10 |
| `agentlab-oss/kagent` | public | Go | agents, kubernetes | | yes | 2026-09-25 |
| `agentlab-oss/docs` | public | MDX | docs | | | 2026-07-01 |

`monorepo` carries 64 MiB of incompressible content (seeded by its name, so
every lab clones the same bytes) for the measurements; `--large-repo-mib <n>`
sets another size. Its `size` field is the repository's size on disk in KiB,
as GitHub reports it.

**The users** are the lab's Dex users (`agentlab.yaml`'s `users`):

| Login | Reads | Pushes to |
|---|---|---|
| `admin` | everything | everything (the archived repository refuses every push) |
| `dev` | everything but `agentlab-org/platform-infra` | `platform-api`, `monorepo`, `agentlab-oss/*` |
| `viewer` | the public repositories | nothing |

A public repository is read by everyone, a signed-out caller included; a
private one by the users that name it and by the App's installation on its
owner. `dev` is the member without access, `viewer` the one who cannot push.

## What it serves

| Area | Endpoints |
|---|---|
| The App | `GET /app`, `/app/installations`, `/orgs/{o}/installation`, `/users/{u}/installation`, `/repos/{o}/{r}/installation`; `POST /app/installations/{id}/access_tokens` (optionally limited to `repositories`). The App's JWT is RS256, signed by the key the lab generated, issued by the App id, valid for at most ten minutes. |
| Listings | `GET /orgs/{o}/repos`, `/users/{u}/repos` (`type`), `/user/repos` (`visibility`), `/installation/repositories`, `/repos/{o}/{r}`: `language`, `topics`, `archived`, `fork`, `pushed_at`, `size`, `permissions`; `sort` (`full_name`, `pushed`, `updated`), `direction`, `per_page`, `page` with the `Link` header. |
| OAuth | `GET /login/oauth/authorize` consents at once for the lab user its `login` parameter names and requires PKCE (`S256`); `POST /login/oauth/access_token` answers access tokens valid eight hours and refresh tokens that **rotate**: each redeems once, a second redemption answers `bad_refresh_token`. `DELETE /api/v3/applications/{client_id}/grant` revokes every token of the person, `/token` the one access token (Basic auth with the client id and secret). `GET /user`. |
| git | Smart HTTP at `https://<fake>/<owner>/<repo>[.git]`, served by git's own `upload-pack` and `receive-pack` in stateless RPC mode (what `git http-backend` runs; protocol v0 to v2): clone, fetch and push, the token as the Basic auth password. A private repository answers 401 without a credential and "not found" to a user who does not read it; a push needs write, 401 without a credential, 403 naming the user otherwise. A push moves `pushed_at`. |
| Pull requests | `POST` and `GET /repos/{o}/{r}/pulls`, and the GraphQL operations of `gh pr create` on `/api/graphql`; `GET /api/v3/meta` reports a GitHub Enterprise version, as gh expects. |

A credential the fake did not issue (the sandbox's placeholder that the egress
gateway did not replace) and one it issued that expired or was revoked answer
GitHub's 401 `Bad credentials` wherever they are presented.

## The request log

Every request is a line of the container's log and an entry of
`GET /_fake/requests`: time, method, path, status, the user (a login, or
`installation/<id>`) and the kind of credential: `none`, `placeholder`,
`revoked-token`, `app-jwt`, `installation-token`, `user-token` or
`oauth-client`. Only the path is logged: no token, OAuth code or verifier
reaches it. `GET /_fake/pulls` lists every pull request with its author.

## Running it

The fake serves TLS with a leaf from the lab CA, for
`github.<platform.domain>` and `127.0.0.1` (the CA's name constraints admit
nothing else), so every lab client and Substrate's egress gateway, which
trusts the lab's trust bundle, trust it. `agentlab github-fake credentials`
generates what it reads into `certs/github-fake/`, next to the CA, never
leaving the machine:

| File | What |
|---|---|
| `tls.crt`, `tls.key` | the fake's TLS pair, re-minted when the CA or the domain changes |
| `app.pem` | the App's private key (PKCS #1, as GitHub hands it out), for the provider |
| `app.pub` | its public key, for the fake |
| `client-secret` | the OAuth client secret |

The fake needs `git` on its PATH (any git; the optional `http-backend` is not
used). On the kind network it runs in an image that carries git, as the lab's own user, with the credentials mounted:

```bash
./agentlab github-fake credentials
docker run -d --rm --name agentlab-github --network kind --user "$(id -u):$(id -g)" \
  -v "$PWD/agentlab:/agentlab:ro" -v "$PWD/certs/github-fake:/credentials:ro" \
  -p 127.0.0.1:8443:8443 gsoci.azurecr.io/giantswarm/golang:1.25.3 \
  /agentlab github-fake --workspaces --credentials /credentials --listen 0.0.0.0:8443 --data-dir /tmp/github
```

A pod reaches it as `github.<platform.domain>` through a host alias to the
container's address on the kind network; on this host the published port does:

```bash
host=github.127.0.0.1.nip.io:8443
git -c http.sslCAInfo=certs/ca.crt clone "https://x-access-token:$TOKEN@$host/agentlab-org/platform-api.git"
SSL_CERT_FILE=certs/ca.crt GH_HOST=$host GH_ENTERPRISE_TOKEN=$TOKEN \
  gh pr create -R "$host/agentlab-org/platform-api" --head my-branch --base main --title "…" --body "…"
```

`$TOKEN` is a user token from the OAuth flow above (authorize with `login=dev`
and a PKCE challenge, then redeem the code with the client secret and the
verifier).
