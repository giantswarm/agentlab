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
  current-context stays what it was; only a `kind-<cluster>` entry left in it
  by an earlier kind, whose CA belongs to a cluster since recreated (`x509:
  certificate signed by unknown authority`), is removed by `up`. The same view from a shell:
  `agentlab pods -n agent-platform`; for `describe` and events,
  `KUBECONFIG=state/kubeconfig kubectl -n agent-platform describe pod <name>`.
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
- **After a Dex signing-key rotation, new tokens get 401 for up to 10
  minutes.** The apiserver fetches Dex's JWKS again only once its cached copy
  is within 30 seconds of Dex's 10-minute `max-age`; until then a token signed
  by the new key fails with `failed to verify id token signature`, and the
  portal's inventory read answers 401 and hides the installation. That is the
  lab apiserver's cache, not a portal failure: a rotation test waits it out
  ([Signing-key rotation and the apiserver's JWKS cache](identity.md#signing-key-rotation-and-the-apiservers-jwks-cache)).
- Kubernetes 1.37 (the embedded kind's node image) still accepts the `--oidc-*`
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
- **A node that does not stop is reported broken, not waited on.** Every
  call that stops, removes or starts the node is bounded (two minutes; the
  wait for a dying node above, 90 s): a node whose kernel waits on a mount
  nobody serves answers none of them, and `agentlab down` or `agentlab up`
  then exits non-zero with `broken: <node> did not stop within <bound>; needs
  a host-level fix`. Such a node comes back only after the mount it waits on
  is lazily unmounted on the host (as root) and the container removed; the
  lab's workspace teardown order (docs/workspaces.md) keeps it from happening.
- **The image cache manifest only records what a registry can serve.**
  `state/preload-images.txt` is snapshotted from the node after every boot;
  an image built on the host and side-loaded (a `platform.devImages` swap,
  `backstage-dev:<tag>`) runs under the lab's own name for local builds,
  `localhost/backstage-dev:<tag>` — a ref no registry serves — and once the
  local copy is pruned every boot would try to pull it. Images the host cache
  knows without a registry digest are therefore left out of the snapshot and
  remembered as local builds.
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
- **`component chart(s) could not be rendered, the registry did not answer
  through the retries`** (or `cannot render agent-platform …`) stops `up` or
  `platform` before the install: this host could not resolve or reach the
  chart registry, retries included (the `rendering …: the registry did not
  answer (…)` lines above it carry the resolver's or the dialer's words —
  `lookup gsoci.azurecr.io: no such host` is DNS). The install is not
  attempted because the lab's patches (the `dex-localhost` sidecar,
  `platform.devImages`) are read off those renders; without them
  mcp-kubernetes would crash-loop on the lab Dex and the install would fail
  on its `HelmRelease` minutes later. Fix the host's network or DNS
  (`getent hosts gsoci.azurecr.io`), then run the command the message names —
  `agentlab platform` on a running cluster.
- **`component chart(s) could not be rendered, the registry has no such chart
  through the retries`** stops `up` or `platform` before the install for the
  same reason: the registry answered, but not with the chart the component's
  `OCIRepository` asks for — `failed to perform "FetchReference" … not found`,
  or `no tag matches semverFilter …`. The `rendering …: the registry has no
  such chart yet` lines above it are the retries (some two minutes), which
  carry a branch build through the moment its tag is listed before its
  manifest is served. The message names the component, the chart and the
  version: check the version the meta chart or a `platform.valuesFiles`
  overlay pins (`components.<name>.versionRange`, `semverFilter`), or wait
  until that build is published, then run `agentlab platform` again.
- **Restarts everywhere at the same minute are the host's disk.** When
  kube-scheduler, kube-controller-manager, Kyverno, Flux, the agentgateway
  controller and kagent all show restarts with the same age, each exited
  cleanly after `Failed to renew lease … context deadline exceeded`: etcd sat
  in an fdatasync behind the host's writeback (`slow fdatasync` in
  `kubectl -n kube-system logs etcd-<cluster>-control-plane`, `waiting for
  writeback completion` in `journalctl -k`). While they are down, the
  fail-closed Kyverno webhook refuses writes and a proof's cleanup can fail.
  A lab created by agentlab v0.67.1 or later runs etcd with `--unsafe-no-fsync` and
  does not stall this way; an older lab gets it with `agentlab down && agentlab up`
  (`docker exec <cluster>-control-plane grep unsafe-no-fsync
  /etc/kubernetes/manifests/etcd.yaml` shows which one you have).


## The host's network

- **An intermittent 503 or DNS error from the gateway mid-turn is usually the
  host's network.** Symptom: an agent turn fails with a 503 (or the gateway
  logs a DNS failure for `api.anthropic.com`), a retry succeeds, and the
  provider's status page shows no incident. The gateway resolves the provider
  through CoreDNS, the kind node and then the host's resolver, and connects
  over the host's routes. A host whose IPv6 route and DNS server come from
  router advertisements (typical on Wi-Fi) can lose either for a moment, and a
  resolver or connection that picked the IPv6 path stalls until it falls back.
  Check the host with `resolvectl status` and `ip -6 route show default`, and
  the path from the lab with
  `docker exec <clusterName>-control-plane curl -4 -sS -o /dev/null -w '%{http_code}\n' https://api.anthropic.com/`
  and the same with `-6` (`404` means reachable). `agents-test`,
  `skills-test`, `toolsets-test` and `models-test` retry an agent's first
  turn once, so one transient failure does not turn them red; a second
  failure is reported. A host that keeps flapping is fixed on the host, not in
  the lab: prefer IPv4 in `/etc/gai.conf` (`precedence ::ffff:0:0/96  100`) or
  pin a stable DNS server.
- **The host's kernel settings changed after a lab boot.** A lab node
  created before agentlab's host-kernel guard (HACKS.md U27) sets
  `kernel.panic=10`, `kernel.panic_on_oops=1` and `vm.overcommit_memory=1`
  on the host, plus `kernel.core_pattern=core`, which switches off
  systemd-coredump. This happens under a rootful engine, at every node start,
  including every host boot. `agentlab up` warns about such a node;
  `agentlab down && agentlab up` recreates it with the guard. Then put the
  host's own values back:
  `sudo sysctl --system && sudo sysctl -w kernel.panic=0 kernel.panic_on_oops=0 vm.overcommit_memory=0`
  (the three kubelet keys are kernel defaults a host's sysctl.d rarely sets).
  A new node leaves them alone; if `up` still reports a change, the warning
  names the key and the command that restores it.

## Rootless Podman's kernel limits

Under rootless Podman on Linux every container of the lab, the kind node's
own included, runs as your user, so two per-user kernel defaults bound the
whole lab. Neither can be raised from inside the node.

- **A pod fails to start with `unable to create session key: disk quota
  exceeded`.** Every container gets a session keyring, counted against your
  user's key quota (`kernel.keys.maxkeys` 200 and `maxbytes` 20000 by
  default; `/proc/key-users` shows the use). A full lab comes close to it on
  its own, and leftover containers of other tools (testcontainers whose
  reaper never ran) count too. Raise it and keep it:
  `printf 'kernel.keys.maxkeys = 20000\nkernel.keys.maxbytes = 400000\n' | sudo tee /etc/sysctl.d/90-rootless-keys.conf && sudo sysctl --system`.
- **Services stop reaching their pods while the pods answer on their own
  addresses.** kube-proxy logs `Failed to execute iptables-restore` with
  `sendmsg() failed: Message too long` and keeps the rules it last wrote, so
  every Service whose pods changed since points at addresses that are gone
  (an edge's connect to the kagent controller times out, muster cannot reach
  agent-manager). Inside a user namespace iptables cannot force a socket
  buffer above `net.core.wmem_default` (212992 bytes by default), and the
  lab's ruleset outgrows it. Raising it on the host takes effect in the
  running node at kube-proxy's next sync:
  `printf 'net.core.wmem_default = 4194304\nnet.core.rmem_default = 4194304\n' | sudo tee /etc/sysctl.d/91-rootless-netlink.conf && sudo sysctl --system`.

## vm-manager

- **`vm-manager-test` fails at `get_vm_attestation` with `golden mismatch:
  pcr 0` after the lab followed a vm-manager release.** The golden values on
  the pod's state claim were recorded for another firmware: PCR 0 is the OVMF
  of the vm-manager pod image, pinned per release; a release that moves the
  pin says `re-record golden PCRs` in its notes, with the same guest image.
  Install, boot and `READY=1` all succeed; only the quotes fail. The proof
  prints the build the pod boots with (`get_host`'s `firmware`). Clear the
  stale values with `vm-manager image golden <image> --clear` and record them
  again inside the pod —
  [recording the image's golden PCR values](vm-manager.md#recording-the-images-golden-pcr-values).

## agentlab.yaml and the release running it

- **`open agentlab.yaml: too many levels of symbolic links`** (macOS) — a
  release from v0.120.0 up to the fix of
  [#1106](https://github.com/giantswarm/agentlab/issues/1106) made the lab's
  `agentlab.yaml` in `~/Library/Application Support/agentlab/labs/<lab>/` a
  link to itself: on macOS the lab's directory and its configuration folder are
  the same folder. The link holds nothing. Update agentlab, then run
  `agentlab configure --cluster-name <lab>` from a directory without an
  `agentlab.yaml`: it removes the link and writes the configuration again under
  that name. Or remove the link yourself
  (`rm "$HOME/Library/Application Support/agentlab/labs/<lab>/agentlab.yaml"`)
  and configure the lab again. A command run in the lab's directory, or in the
  old directory of a lab moved there, stops at the link before it can remove
  it, and `--lab <lab>` does not find the lab while the link is there. A lab
  from before the freedesktop folders that a broken release moved lost its
  `agentlab.yaml` that way (its `certs/` and `state/` are intact): configure it
  again the same way, under the same name.

- **After updating: the portal moved to `portal.<domain>`** — this release
  serves the portal at `https://portal.<domain>` instead of
  `https://backstage.<domain>`. An existing lab keeps the old route and Dex
  redirect URI until its manifests are re-rendered — `agentlab list` and
  `open portal` already name the new URL — so move it over once:
  `agentlab platform && agentlab reload`. A fresh `agentlab up` needs nothing.
- **`agentlab.yaml names a field this agentlab release does not know`** — the
  file was written by a newer release (a switch this release lacks, such as
  `platform.serving`). Every run writes the file back, so a release that did not
  know the field would drop it silently and the next `platform` would uninstall
  what it configured; the refusal is deliberate. Update agentlab — a shared lab's
  checkout carries the release binary its holders run — or remove the field.
- **`a field of the release is owned by another field manager and could not be
  taken over`** — an install or upgrade of one of the lab's releases (the
  platform, kube-prometheus-stack) was refused by server-side apply although the
  lab forces conflicts, which takes over the fields an earlier install's field
  manager owns (`helm` from a client-side install, or a binary of another name).
  The conflicting objects and fields are in the Helm log printed above the
  line. What is left cannot be resolved by an upgrade: recreate the lab.
- **`platform workload field(s) were written outside the platform's Helm
  releases`** — a workload carries a hand's patch beside the render (the line
  names the workload, the fields and the writer, `kubectl-patch` for one), and
  no platform run removes it. Remove each field with the inverse edit (`kubectl
  patch --type=json -p '[{"op":"remove","path":"/spec/template/spec/hostNetwork"}]'`,
  or `kubectl edit`), then `agentlab status` reads `workloads   as the releases
  render them` and platform-test passes its first stage.
