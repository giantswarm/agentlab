# Workspaces

A workspace is one read-write-many volume shared by its sync and every Session
on it: bare mirrors of its repositories and a directory per Session. Each
Session's actor mounts its own directory read-write and the mirrors read-only,
at sub-paths, through Agent Substrate. The lab brings two things the workspace
proofs need and a kind cluster lacks: the [storage](#storage) and
[a GitHub of its own](#the-labs-github), which it gives the workspace-manager
as its [provider instance `fake`](#the-provider-instance-fake); a
[real GitHub App](#the-provider-instance-github) is the other instance.

## Storage

Substrate mounts a workspace's volume without a PVC: ate-api-server calls the
CSI driver's controller over the network (mTLS with Substrate's pod identity)
and atelet calls the node plugin's socket, both found through a cluster-scoped
`CSIDriverConfig`. The kind cluster's local-path provisioner is
read-write-once and no CSI driver. So with `platform.workspaces` on,
`agentlab up` (and `agentlab platform`) installs, before the platform chart,
the lab's stand-in for an installation's read-write-many class (EFS, Azure
Files over NFS), set up for Substrate the way Substrate's own kind setup sets
up a driver:

| Piece | What |
|---|---|
| The `nfs-server` Deployment and Service (namespace `agentlab-workspaces`) | An NFS server on the control plane (the host kernel's nfsd), exporting the node's `/var/lib/agentlab-workspaces`; every volume is a directory under it |
| The NFS CSI driver `nfs.csi.k8s.io` | kubernetes-csi/csi-driver-nfs v4.13.4: the `csi-nfs-controller` Deployment (provisioner and resizer) beside the server, and the `csi-nfs-node` DaemonSet on every node with Substrate's `/var/lib/ate` mounted `Bidirectional`, so atelet sees the mounts the plugin publishes there |
| The controller endpoint | An Envoy in the controller's pod behind the Service `csi-nfs-controller` (port 50051). It requires a client certificate from Substrate's pod-identity CA with ate-api-server's SPIFFE ID. Its own serving certificate is a `PodCertificateRequest` the service-DNS signer answers for `csi-nfs-controller.agentlab-workspaces.svc`, the TLS server name the registration sets explicitly (an unset name would skip verification) |
| The StorageClass `agentlab-workspaces` | Read-write-many, NFSv4.1, `Immediate` binding, delete-reclaiming, expandable; POSIX modes and symbolic links as on a local disk, so git works on it. No snapshots: a Session is a directory on the workspace's volume, not a clone of it |
| The `CSIDriverConfig` `nfs.csi.k8s.io` | Substrate's registration of the driver: the controller endpoint, the node socket override, mTLS with the pod identity. The chart renders it from the lab's `workspaces:` values (the class with `storageClass.create: false`, the driver, the Substrate preview gate) once its release takes the key; until then the lab applies its own after the install (`HACKS.md` U30) |

Every image is pinned and preloaded into the lab like the platform's.

**Host prerequisite:** the kind nodes share the host's kernel, so the NFS
server is the host's nfsd and every mount the host's NFS client. The host
must have the `nfsd` and `nfs` kernel modules available (loaded, or found by
`modprobe`); the kernel loads them by itself when the server starts and at
the first mount, with no `modprobe` by hand. The install checks first and
names a missing module (on Debian and Ubuntu the kernel's
`linux-modules-extra` package carries them).

`agentlab status` prints a `workspaces` line: the NFS server and its node, the
controller with its proxy, the node plugin, the class, and whose
CSIDriverConfig registers the driver. `agentlab platform-down` and
`agentlab down` remove the pieces and clean the node: the mounts under
`/var/lib/ate` and the export's bytes.

### Turning it on

```sh
agentlab configure --workspaces   # needs --agents (on by default)
agentlab up                       # or `agentlab platform` on a running lab
agentlab workspaces-test --storage-only
```

With the switch off, nothing of it is installed, and `agentlab platform` on a
lab that had it removes it.

### The proof

`agentlab workspaces-test --storage-only` is headless:

1. The storage in place: the NFS server, the controller with its proxy, the
   node plugin Ready, the class and the registration.
2. A read-write-many claim of 1Gi from the class, bound.
3. A bare mirror seeded on the volume under `mirrors/`, from a repository with
   an executable and a symbolic link.
4. Session a: a pod with `sessions/a` mounted read-write and `mirrors/`
   read-only, both sub-paths of the one claim. A `git clone --shared` of the
   mirror and its checkout keep the executable bit and the link, a commit
   works, and a write into the mirrors is refused.
5. Session b: `sessions/b` likewise. Nothing of session a is visible, and its
   own shared clone works.
6. The whole volume mounted read-only: both sessions' directories visible, a
   write refused.
7. The controller endpoint without Substrate's client certificate: refused
   (the TLS alert quoted).
8. The actor-level mount through ate-api-server, on the Substrate chart
   version it names: an `ActorTemplate` (atespace `kagent`, on the worker pool and
   snapshot storage of the Harness `kagent/kagent`, the chart's gVisor
   sandbox, the lab's alpine pinned by digest) declaring two
   existing volumes, `session` read-write at `/workspace` and `mirrors`
   read-only at `/mirrors`; two actors on the lab's alpine supply both from the
   claim's one PersistentVolume (driver `nfs.csi.k8s.io`, its volume handle),
   `session` read-write-many at `sessions/a` and `sessions/b`, `mirrors`
   read-only-many at `mirrors`. A reference with an unknown driver and one
   with a handle no PersistentVolume holds are refused at create, with
   ate-api-server's reason. Each actor reports through the volume: its own
   session's files and nothing of the other's, the mirrors readable and a
   write into them refused, a file of its own written. Actor a is paused and
   resumed: its heartbeat carries on with the same boot id, still reading its
   file and the mirrors. Both actors are deleted; the PersistentVolume and
   every file stay. A Substrate without existing volumes refuses the
   template, and the step reports the skip with that refusal.
9. Everything removed: the proof's namespace `agentlab-workspaces-test` with
   its claim, and the volume's directory gone from the export.

`--storage-only` is required: a harness turn against a workspace is not part
of the proof yet. `--ready-timeout` bounds each wait (default 5m). A resume on
another node is proven on a cloud installation, not in the lab: the lab's
export is one node's disk.

## The provider instances

With workspaces on, the workspace-manager gets the provider instances
`platform.workspaces.provider` names: `fake`, the default while the key is
empty; `github`; or both, `fake,github` (`agentlab configure
--workspaces-provider …`). Each is an instance of the chart's `github` kind
with a Secret of its own that the rendered values reference and nothing else
reads. The wiring every instance shares is in place with the switch alone:
the manager's public base URL `https://workspace-manager.<domain>:<gatewayPort>`,
the route the chart renders for it, and the lab Dex's redirect URI
`<base URL>/signin`
([Platform](platform.md#the-workspace-managers-provider-instances-platformworkspacesprovider)).
`platform-test` proves the manager registered with muster and Connected,
rolled out, and `list_providers` as the admin naming every configured
instance and nothing else.

### The provider instance fake

[The lab's GitHub](#the-labs-github) as a GitHub Enterprise-shaped instance,
nothing to register and no account needed. `agentlab up` and `agentlab
platform` run it (`agentlab github-fake --workspaces`) in a container on the
kind network beside the nodes, the way a proof's fake runs, from a copy of
the binary under `state/` (a mounted executable stays busy while its
container runs, and a rebuild over it would be refused); a running one is
replaced, so the fixture is its state, and `agentlab platform-down` and
`agentlab down` remove it. Pods reach it as `https://github.<domain>`: CoreDNS
sends the name to the selector-less Service `agent-platform/agentlab-github`
(port 443 onto the container), the answer keeps the name, and the fake's TLS
leaf (the lab CA's) carries it. This host reaches it on the loopback port the
container publishes, printed by the run with its request log.

The instance is `url: https://github.<domain>` (the API under `/api/v3`, the
kind's own derivation), the fixture's App (id `1`, client
`Iv1.agentlab-workspaces`) and its credentials: the App's private key and the
client secret the lab generated once under `certs/github-fake/`, placed ahead
of the install as the Secret `agent-platform/workspace-fake` (keys
`private-key`, `client-secret`) and referenced from the values. The manager's
HTTP client takes the system roots, so the values name the lab CA file the
chart mounts for Dex as its `SSL_CERT_FILE`; the image's own roots stay, read
from its certificate directory regardless, so a `github` instance beside the
fake still verifies github.com. The App's callback URL,
`<base URL>/callback/fake`, is any absolute URL to the fake, and the sign-in
consents at once for the lab user the `login` parameter names.

### The provider instance github

`agentlab configure --workspaces-provider github` (or `fake,github`) gives the
workspace-manager a real GitHub. The instance renders once a GitHub App of
the lab's own is in place (the ids in `agentlab.yaml`, the keys in the Secret
`agent-platform/workspace-github`:
[Platform](platform.md#the-workspace-managers-provider-instances-platformworkspacesprovider)).

The App is registered once, by an owner of the organization that holds it,
in GitHub's UI (Settings, Developer settings, GitHub Apps), and serves every
lab:

- **Callback URLs**: each lab's `https://workspace-manager.<domain>:<gatewayPort>/callback/github`
  (`agentlab configure` prints it; a GitHub App takes up to ten).
- **User-to-server tokens with expiry** on; no device flow, no webhook.
- **Repository permissions**: Metadata read; Contents, Pull requests and
  Issues read and write; Checks read.
- Installed on the organizations whose repositories the lab's workspaces
  mirror.

It is an App of its own: the lab Dex's [sign-in App](github-signin.md) reads a
profile and nothing on repositories, and muster's `github` server has its own
client with muster's callback.

## The lab's GitHub


A workspace is a git repository a Session works in, cloned, pushed and turned
into a pull request as the signed-in person. Proving that end to end needs a
GitHub with an App, OAuth sign-in for several people with different access, a
private repository, real pushes and pull requests. Against github.com that
means browser consent per account and real accounts; the lab instead serves a
GitHub of its own, `agentlab github-fake --workspaces`, headless and the same
on every machine. A workspace provider instance points at it the way it points
at a GitHub Enterprise Server: API base `https://<fake>/api/v3`, git base
`https://<fake>`.

### The fixture

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

### What it serves

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

### The request log

Every request is a line of the container's log and an entry of
`GET /_fake/requests`: time, method, path, status, the user (a login, or
`installation/<id>`) and the kind of credential: `none`, `placeholder`,
`revoked-token`, `app-jwt`, `installation-token`, `user-token` or
`oauth-client`. Only the path is logged: no token, OAuth code or verifier
reaches it. `GET /_fake/pulls` lists every pull request with its author.

### Running it

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
used). With the [provider instance `fake`](#the-provider-instance-fake) the
lab runs it this way itself, in the Debian golang image the storage proof's
git runs in, as the lab's own user, with the credentials mounted — the same
run by hand, for a fake of one's own:

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
