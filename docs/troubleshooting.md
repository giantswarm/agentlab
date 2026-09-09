# Troubleshooting

The gotchas that cost time, collected. Component-specific ones live next to
their component: [platform gotchas](platform.md#platform-gotchas),
[Backstage gotchas](backstage.md#backstage-gotchas), and the host-side
plumbing for model servers under
[local backends on the lab host](models.md#local-backends-on-the-lab-host-ollama-lemonadenpu).

## Cluster, Dex and the apiserver

- **The lab never uses your kubeconfig's current-context.** Every command that
  talks to the cluster first exports the kind cluster's kubeconfig to
  `state/kubeconfig` and builds its embedded Helm and Kubernetes client from
  that file alone — so a shell with no current-context (or one pointing at a
  real cluster) proves the same lab as any other, and a lab that is not
  running fails by name (`no kubeconfig for kind cluster "agentlab"`). Your
  own kubeconfig is never touched: kind is embedded in `agentlab` and writes
  the cluster's admin kubeconfig (the `kind-<cluster>` context) to
  `state/kubeconfig` only, so nothing merges into `~/.kube/config` and your
  current-context stays what it was. The same view from a shell:
  `KUBECONFIG=state/kubeconfig kubectl -n agent-platform get pods`.
  A probe whose status read fails reports the apiserver's message (`the status
  read failed: ... connection refused`), never an empty status.
- **A client certificate beats a bearer token.** `kubectl --token=...` against
  the kind kubeconfig silently keeps authenticating as `kubernetes-admin`. You
  need a kubeconfig with no client cert — that is what `agentlab login` builds
  (`kubeconfig.oidc`). This will produce convincing false positives in a test
  suite if you miss it.
- **The apiserver keeps retrying OIDC discovery — no bounce needed.** On a
  cold cluster creation, Dex does not exist yet and the apiserver logs
  `oidc authenticator: initializing plugin: … connection refused` — but on
  Kubernetes >= 1.35 it retries every 10 seconds forever and initializes on the
  first tick after Dex answers (verified empirically; earlier versions of this
  lab bounced the static pod because older apiservers gave up for good).
  `agentlab up`'s verification loop simply waits out the next retry tick. If
  tokens are still rejected minutes after Dex is up, read the apiserver log
  for those `oidc.go` lines rather than restarting things.
- **Scopes are not optional.** `--oidc-username-claim=email` needs the client to
  request the `email` scope and `--oidc-groups-claim=groups` needs `groups`.
  Ask for `openid` alone and the apiserver rejects the token with
  `parse username claims "email": claim not present`, which reads like a
  misconfiguration on the apiserver side but is really a missing scope.
- **Dex needs a writable `/tmp`** even with `readOnlyRootFilesystem: true`; it
  renders its config through a temp file. Hence the `emptyDir`.
- **Dex storage must not be `memory` in this lab.** A config edit rolls the
  Dex pod by design (`agentlab reload`), and with in-memory storage every roll
  mints new signing keys — the apiserver then rejects **all** tokens with
  `failed to verify id token signature` until its JWKS cache refreshes,
  minutes after the very edit that prompted the reload. The lab uses Dex's
  CRD-backed `kubernetes` storage instead: keys persist across rolls, tokens
  keep verifying, and the state still dies with the cluster.
- Kubernetes 1.36 (the embedded kind's node image) still accepts the `--oidc-*`
  flags. The modern alternative is
  `--authentication-config` (structured `AuthenticationConfiguration`, which
  also supports CEL claim mappings). The flags are simpler and were kept here.
- **`agentlab down` can lose a race with docker and leave an exited node.**
  kind's delete is `docker rm -f` of the node container; docker gives
  it ten seconds after SIGKILL to exit and then gives up (`could not kill
  container: ... did not receive an exit event`) — a node busy with an
  `agentlab up` side-load in another shell has taken 44 s. The container
  stays in `docker ps -a` as `Exited (137)`, kind still lists the cluster,
  and docker's restart policy does not fire (an API kill counts as a manual
  stop). `agentlab down` therefore waits (up to 90 s) for the node to exit
  and deletes again, and `agentlab up` on a cluster whose node is not running
  starts the container (`docker start`, which kind supports) and waits for
  the apiserver before touching anything — instead of failing while kind
  reads the kubeconfig off the node, with a runc `nsexec ... No such file or
  directory` or `container ... is not running`. Do not run `down` while another `agentlab`
  process is using the cluster (`ps -eo pid,args | grep '[a]gentlab '`).
- **The image cache manifest only records what a registry can serve.**
  `state/preload-images.txt` is snapshotted from the node after every boot;
  an image built on the host and side-loaded (a `platform.devImages` swap,
  `backstage-dev:<tag>`) shows up there as `docker.io/library/backstage-dev:<tag>`
  — a Docker Hub ref that does not exist — and once the local copy is pruned
  every boot would ask Docker Hub for it (`denied: requested access to the
  resource is denied` in the dockerd log, once per ref). Images the host cache
  knows without a registry digest are therefore left out of the snapshot.
- **`failed to load image: command "docker exec ... ctr ... images import
  --all-platforms ..." failed with error: exit status 1` during `up`** is the
  node's containerd refusing an archive whose multi-platform index names blobs
  the archive does not carry — what a plain `docker save` writes under Docker's
  containerd image store (Docker Desktop, new Docker 29 installs), where only
  the host platform's blobs were ever pulled (kubernetes-sigs/kind#3795). The
  lab saves with `docker save --platform` wherever it exists — Docker 28 or
  newer — and imports that archive through the embedded kind, so seeing this
  means an older docker on that store: upgrade it. The boot went on regardless;
  the affected images were pulled by the node.
