# Workspaces

A workspace is one read-write-many volume shared by its sync and every Session
on it: bare mirrors of its repositories and a directory per Session. Each
Session's actor mounts its own directory read-write and the mirrors read-only,
at sub-paths, through Agent Substrate. The lab brings what the workspace
proofs need and a kind cluster lacks, the [storage](#storage), and gives the
workspace-manager its [provider instances](#the-provider-instances): a
[real GitHub App](#the-provider-instance-github) of the lab's own and a
[real GitLab](#the-provider-instance-gitlab).

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
| The `CSIDriverConfig` `nfs.csi.k8s.io` | Substrate's registration of the driver: the controller endpoint, the node socket override, mTLS with the pod identity. The chart renders it from the lab's `workspaces:` values (the class with `storageClass.create: false`, the driver, the Substrate preview gate) where its release does (agent-platform 4.127.0 and newer); while the component renders the install applies carry none — a release that takes the key and renders nothing from it included — the lab applies its own after the install (`HACKS.md` U30) |

Every image is pinned and preloaded into the lab like the platform's.

**Host prerequisite:** the kind nodes share the host's kernel, so the NFS
server is the host's nfsd and every mount the host's NFS client. The host
must have the `nfsd` and `nfs` kernel modules available (loaded, or found by
`modprobe`); the kernel loads them by itself when the server starts and at
the first mount, with no `modprobe` by hand. The install checks first and
names a missing module (on Debian and Ubuntu the kernel's
`linux-modules-extra` package carries them).

`agentlab status` prints a `workspaces` line: the NFS server and its node, the
controller with its proxy (a leftover without it worded as such), the node plugin, the class, whose
CSIDriverConfig registers the driver, and the chart's CSI companion when it
runs ([The CSI companion](#the-csi-companion)). `agentlab platform-down`, `agentlab
down` and `agentlab platform` with the switch off take it apart in one order:
the workspace claims are deleted, the pods that mount a workspace volume are
deleted and waited for, the volumes are deleted, and every NFS mount and the
driver's publishes under `/var/lib/ate` are unmounted inside the nodes, all
while the NFS server still runs; only then the server goes (with its
namespace, or with the cluster on `down`) and the export's bytes with it. A
server that stopped first would leave a node's NFS client waiting on it for
good, a node that can then be neither stopped nor removed.

### Rootless Podman: the moosefs and hostpath storages

Under rootless Podman on Linux the nodes run in your user's user namespace,
where the kernel mounts neither the `nfsd` filesystem nor an NFS export: the
`nfs-server` pod logs `Unable to access /proc/fs/nfsd` and never listens, and
every NFS mount is refused. `platform.workspaces.storage` picks what backs the
volumes:

| Value | What |
|---|---|
| `nfs` | The NFS server and csi-driver-nfs above |
| `moosefs` | A MooseFS 4.59.2 master and chunkserver in the cluster and moosefs-csi 1.0.0: every volume a directory of the MooseFS filesystem, which the plugins mount through FUSE (a user namespace allows it) and publish with a bind mount |
| `hostpath` | kubernetes-csi/csi-driver-host-path v1.17.1, as Substrate's own `ate-setup csi hostpath` installs it: every volume a directory of the node's `/var/lib/agentlab-workspaces` (beside the driver's `state.json`), published with a bind mount, which a user namespace allows |
| empty (the default) | `moosefs` under rootless Podman on Linux without the companion, `nfs` everywhere else |

`agentlab configure --workspaces-storage <nfs|moosefs|hostpath>` sets it;
`""` returns to the default.

The MooseFS storage serves the whole workspace flow, where the hostpath
storage meets the staging limits below: moosefs-csi stages nothing, as
csi-driver-nfs does not, so the kubelet and atelet publish one volume side
by side; and its plugins read the master's address from their flags, never
from a request's secrets, which Substrate's CSI calls carry none of (a
driver that mounts with per-request secrets, JuiceFS's for one, cannot serve
Substrate's actors). Its pieces:

| Piece | What |
|---|---|
| The `mfsmaster` and `mfschunkserver` Deployments (namespace `agentlab-workspaces`) | On the control plane; the master's metadata under the node's `/var/lib/agentlab-workspaces/moosefs/master` (seeded empty on the first start), the chunkserver's chunks and identity beside it; the master's Service is the address the chunkserver and the plugins dial |
| The `csi-moosefs-controller` Deployment | The driver `csi.moosefs.com` in controller mode with the provisioner and the resizer, and the same Envoy mTLS proxy as the NFS controller's, behind the Service `csi-moosefs-controller` (port 50051, TLS server name `csi-moosefs-controller.agentlab-workspaces.svc`); the driver's FUSE mount at the node's `/var/lib/agentlab-workspaces/moosefs/mnt`, so the node sees the volumes' directories |
| The `csi-moosefs-node` DaemonSet | The driver in node mode on every node, registered at `/var/lib/kubelet/plugins/csi.moosefs.com/csi.sock`, the kubelet's pods directory and Substrate's `/var/lib/ate` mounted `Bidirectional` |
| The StorageClass `agentlab-workspaces` | Read-write-many on every node, `Immediate` binding, delete-reclaiming, expandable (a volume's quota is the claim's size); no mount options |
| The `CSIDriverConfig` `csi.moosefs.com` | As the NFS driver's, from the same `workspaces:` values |

`csiCompanion` is refused with it: the companion's preset mounts the lab's
NFS volumes. The images are Docker Hub's community editions, pinned by
digest (gsoci carries no mirror of them). A volume's root is the driver's
`0755`, owned by root: a pod's `fsGroup` opens it to the group (moosefs-csi
applies it as the volume's group, `0775`), where csi-driver-nfs's class
opens it to everyone (`mountPermissions: 0777`). The FUSE mounts live in the
plugins' containers: a node plugin's restart takes its node's publishes
with it until the pods using them are started again.

The hostpath storage:

| Piece | What |
|---|---|
| The `csi-hostpath-node` DaemonSet (namespace `agentlab-workspaces`) | The driver `hostpath.csi.k8s.io` on the control plane, serving the controller and the node service on `/var/lib/kubelet/plugins/csi-hostpath/csi.sock`, with the provisioner, the resizer and the registrar beside it; Substrate's `/var/lib/ate` mounted `Bidirectional` |
| The controller endpoint | The `csi-hostpath-controller` Deployment: the same Envoy mTLS proxy as the NFS controller's, alone, dialling the driver's socket on the node, behind the Service `csi-hostpath-controller` (port 50051, TLS server name `csi-hostpath-controller.agentlab-workspaces.svc`) |
| The StorageClass `agentlab-workspaces` | Read-write-many on the one node, `Immediate` binding, delete-reclaiming, expandable; no mount options |
| The `CSIDriverConfig` `hostpath.csi.k8s.io` | As the NFS driver's, from the same `workspaces:` values |

A volume is a directory of one node, so the hostpath storage needs the
single-node lab: `substrateNodes` above 0 is refused with it, and so is
`csiCompanion`, whose preset mounts the lab's NFS volumes. Nothing is
installed on the host and no kernel module is needed. The plugin image is
`registry.k8s.io`'s, pinned by digest (gsoci carries no mirror of it).

The hostpath driver keeps one staging path per volume on a node and refuses
a second (`volume … is already staged at …`). Two orchestrators stage on the
node, each at its own path: the kubelet for a pod's claim and atelet for an
actor's existing volume. So a pod and an actor cannot hold one volume at the
same time on this storage: a workspace sync beside a running Session, or
`workspaces-test --storage-only`'s report pod reading the volume its actors
hold. Actors sharing a volume among themselves (a Session's directory
read-write and the mirrors read-only, two Sessions of one workspace) need a
Substrate that stages a multi-node volume once per node; earlier releases
stage it once per target and are refused at the second mount. The claims,
the sessions' sub-paths and the controller endpoint's refusal pass on any
release. csi-driver-nfs advertises no staging, which is why the NFS storage
meets neither limit; the whole workspace flow needs it.

### Turning it on

```sh
agentlab configure --workspaces   # with --agents (on by default); without, see below
agentlab up                       # or `agentlab platform` on a running lab
agentlab workspaces-test          # --storage-only: the storage alone
agentlab workspaces-test --harness codex --model-config <name> # the Session on the codex Harness
agentlab workspaces-test --part sync     # every kind of sync cycle on the github instance's proof repositories
agentlab workspaces-test --part sessions # Sessions on both Harnesses with a requested sync cycle
agentlab workspaces-test --part git      # git and gh as the person from a Session on both Harnesses, on GitHub (--toolchains github: gh from the toolchain)
agentlab workspaces-test --part gitlab   # the provider instance gitlab, end to end (skips without one)
```

With the switch off, nothing of it is installed, and `agentlab platform` on a
lab that had it removes it.

### Workspaces need the agents

The workspaces live in kagent's namespace, where every Session runs and mounts
its workspace, and the workspace-manager applies its sync account and Roles
there; Agent Substrate comes with the agents (`platform.agents`). The lab
refuses `platform.workspaces` without `platform.agents` before anything
installs, as the platform chart refuses `workspaces.enabled` without
`components.kagent.enabled` at render time. Switching the agents off in place
takes `--workspaces=false` with it; once the chart's substrate release is gone,
`agentlab platform` removes the lab's own `CSIDriverConfig` and the
`ate-system` and `podcertificate-controller-system` namespaces (Substrate's
CRDs stay), and `agentlab status` words a `CSIDriverConfig` left while
Substrate's control plane is not running as a leftover, never as registered.

### The CSI companion

On an installation the workspace volumes come from the cluster's own file
driver (Amazon EFS, Azure Files over NFS), and Substrate mounts them through
the connectivity chart's CSI companion (`workspaces.substrate.csiCompanion`,
agent-platform 4.128.0 and newer): the driver's image run a second time by
the platform — a controller behind an Envoy mTLS proxy that admits
ate-api-server alone, a node plugin on the Substrate workers with
`/var/lib/ate` mounted `Bidirectional` — and the `CSIDriverConfig` the chart
derives from it. `platform.workspaces.csiCompanion` is the lab's one path for
proving a change to the companion before merge:

```sh
agentlab configure --workspaces --workspaces-csi-companion
agentlab up                             # or `agentlab platform` on a running lab
agentlab workspaces-test --storage-only # the actors mount through the companion
```

| Piece | What |
|---|---|
| The lab's NFS driver | Unchanged but for its name: it serves the volumes as `efs.csi.aws.com` (csi-driver-nfs's `--drivername`), the driver of the companion's `efs` preset, and the StorageClass `agentlab-workspaces` provisions with it. Substrate mounts a volume through the driver that provisioned it, and the chart registers its preset's driver only |
| The companion | The chart's `efs` preset with the lab's NFS plugin in place of the EFS driver, which crashloops without EFS: `image.repository: giantswarm/nfsplugin`, `image.tag` the lab's csi-driver-nfs release, csi-driver-nfs's arguments in each mode under the preset's driver name. Everything else is the chart's own render: the `csi-companion-controller` Deployment and Service with the proxy, the `csi-companion-node` DaemonSet, their policies, and the `CSIDriverConfig` `efs.csi.aws.com` pointing at them. The lab sets no `csiDriver` and applies no registration of its own |

`agentlab platform` waits for the companion's controller and node plugin
after the install and names them in the summary; `agentlab status` adds the
companion's state to the `workspaces` line. The proof then runs against the
companion: its controller endpoint refuses a connection without Substrate's
client certificate, and the actors mount their session and mirror sub-paths
of the claim's volume through it. A companion chart change is proven with
the change's chart (`platform.chartPath` to a checkout, or its dev build's
version): `helm template` of the connectivity chart for the render, the
proof for the mount.

A chart without the companion's values is refused before anything changes.
Switching between the lab's own registration and the companion renames the
driver: a StorageClass's provisioner cannot change in place, so `agentlab
platform` takes the workspace storage apart in its one order first, the
workspace volumes with it, and installs it under the new name.

### The proof

`agentlab workspaces-test` is headless:

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
   template, and the step reports the skip with that refusal. A fresh lab
   has no atespace `kagent` until kagent runs an actor there: the step
   creates it through ate-api-server, the way kagent does, and says so. The
   template's golden actor gets neither volume (Substrate supplies an
   existing volume only to an actor that names it), so the actor script idles
   there instead of exiting, which would crash the golden.
9. A claude Session admitted on the volume ([the Session admission](#the-session-admission)),
   left out with `--storage-only`:
   - The rig: the workspace-manager's `/.well-known/jwks.json` publishes the
     lab's grant key (its key id quoted), kagent's jwt admission names that
     key set, the manager's issuer and the audience, and the Harness `claude`
     is Ready.
   - The mirror laid out the way the workspace sync lays it out:
     `mirrors/agentlab/platform-api.git`.
   - A claude Agent `agentlab-workspaces-claude` Ready on the Harness
     `claude`.
   - A Session without a volume, as the admin: its turn, a Bash probe, runs in
     `/data/workspace` and sees no `/mirrors`. Its creator is the person the
     grants name.
   - Refused at create, each with kagent's reason: a volume without a grant,
     a grant signed by a key the manager does not publish, a grant for
     another person, and two refused naming
     `admission grant is for another namespace`: a grant for the Agents of
     namespace `agentlab-workspaces-other` presented on the Agent in `kagent`,
     and a grant without `agent_namespace`.
   - A Session with the volume source the manager's grant allows
     (`sessions/${SESSION_ID}` read-write at `/workspace`, `mirrors`
     read-only at `/mirrors`) and a grant signed with the lab's key: its turn
     runs in `/workspace`, sees `agentlab/platform-api.git` under
     `/mirrors`, is refused a write into the mirrors, and writes a file that
     a pod then reads on the volume at `sessions/<Session id>`.
   - The session directory: the turn's own
     `git clone /mirrors/agentlab/platform-api.git` succeeds as the
     harness user (its uid:gid quoted), and the runtime's setup clone
     `agentlab/platform-api` is in its directory (its `origin/HEAD`
     quoted). Either failing fails the step with git's message and the
     controller's version.
   - The Sessions and the Agent removed.
10. Everything removed: the proof's namespace `agentlab-workspaces-test` with
    its claim, and the volume's directory gone from the export. A
    directory a live PersistentVolume of the class still backs (another
    Workspace's) is named and left in place; one no volume backs fails the
    run.

`--ready-timeout` bounds each wait (default 5m). A resume on
another node is proven on a cloud installation, not in the lab: the lab's
export is one node's disk.

### The sync part

`agentlab workspaces-test --part sync [--provider github|gitlab]` proves the
workspace-manager's sync cycles on the real provider, never a stand-in: a
workspace over the instance's two proof repositories
(`platform.workspaces.<instance>.proofRepositories`, see
[the proof repositories](#the-proof-repositories)), defined through the
manager's tools through muster as the admin, a member of the lab's
Organization. A fetch is read on the volume: `git fetch` writes a mirror's
`FETCH_HEAD`, which a clone and a mirror left unchanged never touch.

1. **Preconditions**: the workspace-manager reachable through muster, the
   admin holding the instance's grant ([the grant](#the-grant-a-proof-waits-for)),
   the pushed repository cloned on this host (shallow, no working tree) and
   the scratch branch `agentlab-workspaces-proof` an interrupted run left
   deleted.
2. **First cycle**: `create_workspace`; the cycle the create starts mirrors
   both repositories bare at `mirrors/<owner>/<name>.git`, each default
   branch at the provider's head (`git ls-remote`), `gc.pruneExpire=never`,
   the manifest naming each; a requested cycle then settles the listing the
   first one may have read from the manager's minute-long cache.
3. **No-op**: `sync_workspace` with nothing changed ends `NoChange` with no
   sync Job, no mirror's `FETCH_HEAD` moves, and the manifest is byte for
   byte the same.
4. **Push**: an empty commit pushed to the scratch branch of the first
   repository with the person's own git credentials, then requested cycles
   (up to five, 15 s apart, while the provider's listing catches up) until
   one fetches it: that mirror alone fetched, the branch at the pushed commit
   in it, no default branch moved, the second repository untouched.
5. **Force-push**: a session directory with a `git clone --shared` of the
   mirror (git alternates), a force-push over the pushed commit, and the
   cycle after which the clone still reads every object (`fsck
   --connectivity-only`, the overwritten commit's tree), the overwritten
   commit kept unreachable in the mirror; `get_workspace` lists the session
   directory.
6. **Dropped**: `update_workspace` selecting the second repository alone:
   the first leaves the selection, marked dropped in the manifest, its mirror
   kept while the session directory borrows from it.
7. **Borrower gone**: the session directory removed, the next cycle deletes
   the dropped mirror and its manifest entry.
8. **Update**: `update_workspace` selecting both again clones the first
   anew at the provider's head.
9. **Delete**: `delete_workspace`; the claim and the volume go, nothing of
   it left on the export. The scratch branch and the host's clone are
   removed on every path; no default branch is ever written.

### The sessions part

`agentlab workspaces-test --part sessions [--provider github|gitlab]
[--sessions n]` proves many Sessions on one workspace at once, the shape of
a Session's setup racing the sync: on a workspace over the instance's proof
repositories, after a push to the scratch branch of the first since the
workspace's first cycle:

1. **Preconditions**: the workspace-manager with its kagent address and a
   cleanup window of 5m to 15m (`agentlab configure
   --workspaces-cleanup-after 10m`, then `agentlab platform`), the
   [Session admission](#the-session-admission), an Agent Ready on the claude
   and on the kagent (Go ADK) Harness, the admin holding the instance's
   grant.
2. **Push**: one grant per Session from `authorize_workspace`, each naming
   the pushed repository changed since the sync.
3. **Start**: `sync_workspace` and every Session's `start_session` with its
   grant released at once (`--sessions`, default 10, at least 3, alternating
   the Harnesses); each first turn lists its directory and every clone's
   heads and writes a file only it knows.
4. **Directories**: one directory per Session, each set up once, whose setup
   record (`.kagent/workspace.json`) names the pushed repository fetched from
   the provider exactly once and nothing else; every clone at the
   provider's default heads, the pushed repository's scratch branch at the
   pushed commit; the requested cycle fetched the pushed commit into the
   mirror.
5. **Isolation**: a second turn on each Session finds only its own file
   anywhere it can look, and a write into the mirrors is refused.
6. **Delete**: a deleted Session's directory goes with no cycle requested
   (the manager's Session poll), the mirrors' heads and every other
   directory untouched.
7. **No workspace**: a Session started without a workspace on each Harness
   works in an empty directory with no mirrors.
8. **Cleanup**: the Sessions suspended, the window waited out, a cycle
   removes every directory and the Workspace reads `SessionsCleanedUp=True`;
   one Session per Harness resumed works in a fresh directory.
9. **Report**: each Session's timings (`start_session` called and answered,
   first turn sent, directory set up, first turn ended) in
   `workspaces-sessions-report.json` in the state directory, and as `timing`
   lines.

### The git part

`agentlab workspaces-test --part git` proves a workspace Session works with
git and `gh` as the person, on GitHub through the `github` instance's App,
on the claude and the kagent (Go ADK) Harness, with the person's token never
in the sandbox and no GitHub MCP server. What happened at GitHub is read back
from GitHub's own API, by the Session as the person; nothing stands in for
GitHub.

1. **Preconditions**: the instance's [proof repositories](#the-proof-repositories),
   the workspace-manager, the [Session admission](#the-session-admission), an
   Agent Ready on each Harness, the admin holding the instance's
   [grant](#the-grant-a-proof-waits-for); the branches
   `agentlab-workspaces-git-*` an interrupted run left on the pushed
   repository deleted.
2. **Workspace**: `create_workspace agentlab-git` over the pushed repository,
   its first cycle.
3. **Push and pull request**, per Harness: a Session started through
   `start_session` with the admin's grant; one turn in its clone switches to
   `agentlab-workspaces-git-<run>-<harness>`, commits and `git push`es,
   runs `gh api user` (a person's login, type `User`: an App's installation
   token answers no `/user`), `gh pr create`, reads the pull request's author
   and the push's actor (GitHub's repository activity) back, both the
   person, and closes the pull request with its branch deleted. The commit's
   committer is the turn's person. `GH_TOKEN` is the Harness's inert
   placeholder, and no GitHub token (`gh?_…`, `github_pat_…`) is in the
   environment, `/data`, `/workspace`, the working directory or the home
   directory: names are printed, never values.
4. **Pause and resume**, per Harness: the same Session leaves a file
   uncommitted on a new branch and asks the person whether to commit it; it
   is suspended while the person answers, resumed, and the answer's turn
   finds the same volume, working directory, branch and untracked file, the
   setup record's time unchanged (not set up again).
5. **No workspace**, per Harness: a Session started without a grant;
   `gh api user` and a `git push` to the pushed repository are refused, and
   no GitHub token is anywhere it looks.

`--toolchains github[@<version>],…` starts every Session of the part with
those toolchains through `start_session` (agent-manager's toolchains, see
[The toolchains part](platform.md#the-toolchains-part) for what the lab
needs). With `github` the write turn's `gh` must be the toolchain's,
`/opt/kagent/tools/github/bin/gh`: the part then proves the workspace's git
and `gh` work on a Harness image that ships no `gh` of its own.

The Sessions, the Agents, the workspace and every branch of the run are
removed on every path.

### The proof repositories

The sync, sessions and git parts run on a provider instance's two proof
repositories, recorded in `agentlab.yaml`:

```yaml
platform:
  workspaces:
    github:
      proofRepositories:
        - example-org/workspace-proof  # pushed to: its scratch branch only
        - example-org/other-repo       # left as it is
    gitlab:
      proofRepositories:               # <group>/<project>, nested groups too
        - workspaces/workspace-proof
        - workspaces/other
```

The instance's App reaches both. The first takes pushes to the scratch
branch `agentlab-workspaces-proof`, which the parts create and delete: a
default branch protected against direct pushes stays untouched. On
`github` the pushes go out with the person's own git credentials (their
credential helper, `gh auth git-credential` for one); on `gitlab` as the
instance's person, with the password the GitLab part reads, handed to git
through its environment. Without the list the part skips with the reason.

### The grant a proof waits for

A GitHub App's grant takes github.com's sign-in in a browser, which no
headless walk makes. A part that finds the admin without the grant of the
`github` instance's App prints muster's sign-in link, writes it to
`workspaces-grant-github.url` in the state directory and waits up to 15
minutes for `list_providers` to show it connected: open the link in a
browser signed in to GitHub, which takes GitHub's consent of the lab's App
once and redirects to muster's callback. The grant is dropped at the run's
end, as every proof's. A GitLab's grant is walked headlessly through its
sign-in form, as in the GitLab part.

### The GitLab part

`agentlab workspaces-test --part gitlab [email]` proves
[the provider instance `gitlab`](#the-provider-instance-gitlab) end to end
against a real GitLab, with muster pinning the workspace-manager to the
GitLab OAuth application (`platform.workspaces.grantProvider: gitlab`), or
keeping one provider App per instance, GitLab's beside GitHub's
(`grantProvider: apps`, [provider Apps side by side](#provider-apps-side-by-side)),
as the admin (granting it as `platform.workspaces.gitlab.person`) and
`[email]` (default `dev@lab.local`, never granting it). Without the
instance, any of its settings, or with the pin on another instance, every
stage skips with the reason and the part passes: it never fails for a lab
that has no GitLab. With `apps`, the part grants and drops GitLab's App by
its name (`core_auth_login` / `core_auth_logout` with `app=gitlab`): the
second person's `list_providers` shows `gitlab` not connected and
`core_auth_login app=gitlab` answers the sign-in link; the admin's
`list_providers` after the grant lists every instance with its own status
(`github` connected too when the admin granted the GitHub App in a
browser); after the drop `gitlab` is not connected and every other instance
reads as before.

1. **Preconditions**: the workspace-manager reachable through muster and
   pinned to the instance's application, the
   [Session admission](#the-session-admission), the claude Agent Ready, both
   people signed in to muster.
2. **Not connected**: `[email]`, who has not granted GitLab's application,
   gets muster's sign-in link from `list_providers`.
3. **Connect**: the admin grants it: `core_auth_login`'s link walked
   headlessly through GitLab's sign-in form and the application's consent to
   muster's callback; `list_providers` lists `gitlab` of kind `gitlab`,
   connected.
4. **Picker**: `list_repositories` for the group as the admin lists its
   projects, what the Dev Portal's workspace editor shows, with no
   provider-specific request.
5. **Workspace**: `create_workspace` over the project, its first cycle: the
   bare mirror on the volume at the head the manifest names.
6. **Start**: `authorize_workspace`, which reads the project with the
   admin's GitLab token muster attaches, grants, naming GitLab's host for the
   audience `gitlab` and no token; `start_session` through agent-manager
   creates the Session on the volume, whose turn works in its directory with
   the clone at the mirrored head and holds no credential beyond the
   platform's own variables (`GH_TOKEN` and `GITLAB_TOKEN`, the Harness's
   inert placeholder the egress gateway replaces, `GLAB_IS_OAUTH2` and
   `GLAB_CONFIG_DIR`, glab's configuration naming the workspace's GitLab
   hosts, are among them; a `GITLAB_TOKEN` of any other value is a credential).
7. **Push and merge request from the Session**: the Session pushes an empty
   commit to a proof branch and deletes the branch again. It holds no GitLab
   credential, so GitLab accepts both only through Substrate's egress
   gateway, which fetches the person's token from muster for a Session bound
   to them and sends it as Basic with the user `oauth2` (muster's
   `credentialProvider.gitUsers`, `gitlab: oauth2`). Between the two, on a
   Harness that gives `glab` its placeholder (`GITLAB_TOKEN` set to kagent's inert placeholder, sent as an
   OAuth2 bearer through `GLAB_IS_OAUTH2`), `glab api user` names the person
   and `glab mr create` opens a merge request of the branch authored by them,
   which the Session closes before it deletes the branch; on an older Harness
   the stage reports glab's answer and leaves the merge request unjudged.
8. **No workspace**: a Session started without a workspace works in the
   default directory, holds no credential beyond the platform's own and no
   `GITLAB_TOKEN`, and `git ls-remote` of the private project is refused.
9. **Disconnect**: the admin drops the grant in muster (`core_auth_logout`),
   and `list_providers` answers muster's sign-in link again; the admin grants
   it once more for the restore.
10. **Restore**: the Sessions, the Agent, the workspace and its volume
    removed, and the admin's grant dropped.

### The measurements

`agentlab workspaces-measure` measures what the proofs prove, on a cluster
with the platform, a lab's or an installation's: how long a workspace's
sync, a Session's directory and the git work in it take on the workspace
volume, how much the volume stores, and whether the design's sizing defaults
hold (a repository not yet mirrored counts 1.5 × the size its provider
reports, once synced its measured mirror; the headroom is max(4 GiB, 50 % of
the need); an online expansion where the class provisions a size). It names
the cluster with `--kube-context <context>` (of your own kubeconfig) and
`--muster-url <url>`, and works through muster as the person `muster auth
login --endpoint <url>` signed in, with `--organization`, `--provider` (the
provider instance), `--small-repos owner/a,owner/b` (the first is checked
out, built and pushed to) and `--large-repo owner/name`, all required;
Kubernetes is only read, apart from the probe pods.

```sh
agentlab workspaces-measure --kube-context <context> --muster-url <url> \
  --organization <organization> --provider <instance> \
  --small-repos owner/a,owner/b --large-repo owner/big \
  --agentgateway-url <url> --agent <namespace>/<name>
```

Two workspaces, each created and deleted once per run (`--runs`, default 5):
`agentlab-measure-small` over the small repositories and
`agentlab-measure-large` over the large one. Give them an owner each, so the
manager's one-minute listing cache of one never shortens the other's cycle.
Per run and workspace, each figure from the call to the first read that sees
its end (reads every 500 ms):

| Figure | From → to |
|---|---|
| First sync | `create_workspace` called → `get_workspace` reports the first cycle's result |
| No-op cycle | `sync_workspace` called once the listing cache let go, nothing pushed → the cycle's `NoChange` (a cycle ending otherwise is not counted, and says so) |
| Changed cycle | `sync_workspace` called after a push to the scratch branch → the cycle's `Succeeded` |
| `authorize_workspace` | each call through muster → its granted answer |
| Session directory setup | `start_session` called with the grant → the setup's record `.kagent/workspace.json` in the session directory: the actor Running, every clone in place, the pushed repository fetched |
| Setup in the actor | the first turn sent → the same record |
| On-demand fetch | after another push, the setup's own `git fetch --quiet --prune origin` timed in the Session's sandbox by its second turn → the fetch ended at the pushed head |
| First checkout, `git status`, build | in a pod with the Session's mounts (its directory at `/workspace`, the mirrors read-only at `/mirrors`): `git switch <default branch>` in the Session's clone (the Harness's first checkout), `git status --porcelain`, then the build with a cold module and build cache beside the clone as two figures: its download (`go list -deps ./...` through the module proxy, `--goproxy`, default the image's `proxy.golang.org`; a vendored module downloads nothing) and its compile (`go build ./...` with the proxy off); then the same in a clone of a copy of the mirror on an `emptyDir`, the node's local disk |
| Session directory size | `du -sb` of the directory after the turns and the checkout, the build's cache and output removed |

`--agentgateway-url` and `--agent namespace/name` (an Agent whose Sessions
reach the provider) measure the Sessions; without them the Session figures
are not available.

The pushes go to a scratch branch `agentlab-workspaces-measure-<start time>`
of the first small repository and of the large one, with your own git
credentials (`gh auth setup-git` for GitHub; `--git-url` names another
provider's git base URL, default `https://github.com`): an empty commit on
the default branch's head per push, which moves the repository's `pushed_at`
and so starts a changed cycle, the manager mirroring every ref. The branch is
deleted at the end. A repository you cannot push to has its changed cycle
and on-demand fetch not available, with git's answer, and the run goes on.

Run 1 also times **one online expansion** of the small workspace's volume
where its StorageClass allows volume expansion (`agentlab-workspaces` does):
`update_workspace` raises the workspace's `minimum` a GiB above the claim's
request, the manager raises the claim's request, the driver resizes the
volume; on an elastic class it is listed as not available. The last run reads
the **standing storage**: the claim's capacity, the volume's use (`du` of
`mirrors/` and `sessions/`), what the filesystem under it reports (`df`: on
the lab's export or an elastic share that is not the volume's own), the
manager's need and size, each mirror's measured size against the provider's
(`preview_workspace`'s `sizeKiB`, GitHub's `size`) with the factor against
1.5, and whether the capacity left the headroom max(4 GiB, 50 % of the need)
free.

The output is a JSON document and a Markdown table, written to the run
directory (`state/workspaces-measure-<time>/`, or `--run-dir`) as
`workspaces-measure.json` and `workspaces-measure.md`, and printed: per
workspace and figure what it measured from and to, its samples, median, p90
(nearest rank) and max, the StorageClass and the driver. A figure the cluster
cannot report is listed as not available with the reason, never estimated:
a repository without a root `go.mod`, or whose build fails, has its build
not available with the build's last error line, and the run goes on. A probe
pod that fails costs only its own figures of that run: every figure it
reported is kept, the report lists it under **Probes that failed** (run,
workspace, probe, reason), and the run goes on. A run that ends early still
writes the report: the figures no run reached are not available, and the
report says why the run ended. On a lab the NFS export lives on the node's
own disk, so the volume against the local disk measures the NFS protocol,
not a network. `--ready-timeout` bounds each wait.

Everything the command creates is removed on every path: the Sessions, the
session directories, the workspaces and their volumes, the probe pods and
the scratch branch. A Session the cleanup cannot delete is named in the
report under **Sessions left**, for a person to delete in kagent.

Every call to muster and kagent, the cleanup's included, carries the
person's current token: the renewed tokens of the person's muster sign-in,
taken shortly before the current one expires (muster renews them once they
have expired, so the measurement waits those seconds). A measurement of many
runs outlives any one token; each renewal is said, and the cleanup's last
line counts the Sessions the measurement started and deleted.

A workspace an interrupted run left under a measurement's name has its
session directories cleared and is deleted before the run. One that is still
deleting goes only at its manager's next deletion cycle: its session
directories are cleared so that cycle lets it go, the run measures under
`<name>-<start time>`, and the report names the leftover.

## The Session admission

kagent admits a Session that names a workspace volume only with a workspace
grant: a short-lived JWT the workspace-manager signs, which kagent's jwt
Session admission verifies against the manager's published key set. It checks
the issuer, the audience, the person (the grant's `sub` against the Session's
creator), the volume and the mounts. The platform renders neither half yet
(`HACKS.md` U31). With the workspaces and the agents on, the lab adds them:

| Piece | What |
|---|---|
| The signing key | An EC P-256 key generated once under `certs/workspace-grant/grant.pem`, placed as the Secret `agent-platform/workspace-manager-grant` (key `grant`) ahead of the install and named in `workspace-manager.grant.signingKey`. The manager publishes its public half at `/.well-known/jwks.json` (workspace-manager 0.2.0 and newer), under its RFC 7638 thumbprint |
| kagent's admission | `kagent.controller.session.admission.jwt`: the key set through the manager's Service (`http://workspace-manager.agent-platform.svc:8080/.well-known/jwks.json`), the manager's base URL as the issuer, `kagent` as the audience |
| The claude Harness | `kagent.harnesses`: the Harness `claude` on the chart's claude-harness image, its snapshots at `s3://ate-snapshots/claude`, a turn limited to $1 and 10 model turns |

`agentlab status` adds a `Session admission:` part to its workspaces line: what
kagent trusts, the key's Secret and the Harness's readiness.

The proof signs its grants itself, with the lab's key, in the manager's
claims (`iss`, `sub`, `aud`, `exp`, `nbf`, `iat`, `jti`, `workspace`, `volume`,
`mounts`). A grant verifies only through the key set the manager publishes. It
does not call `authorize_workspace`: a grant needs a synced workspace and a
person who granted its provider's App ([the person's
grant](#the-persons-grant-musters)). The manager's grant names the person by
email, the Session's creator as kagent's admission compares it, and admits
the Session.

A claude Session works in its volume directory, with a clone of each mirror,
from kagent 1.9.0; its agent's own clone of a mirror needs kagent 1.10.0,
whose harness trusts the mirrors in the agent's git
([kagent-upstream#409](https://github.com/giantswarm/kagent-upstream/issues/409)).
On an older line the step fails: below 1.9.0 the turn reports
`/data/workspace` with no setup clone
([agent-platform#936](https://github.com/giantswarm/agent-platform/issues/936)),
and on 1.9.x the agent's own clone fails on git's dubious-ownership check.

## The provider instances

With workspaces on, the workspace-manager gets the provider instances
`platform.workspaces.provider` names: `github`, the default while the key is
empty; `gitlab`; or both, `github,gitlab` (`agentlab configure
--workspaces-provider …`). `github` is an instance of the chart's `github`
kind, `gitlab` of its `gitlab` kind, each with a Secret of its own that the rendered values reference and nothing else
reads. An instance renders once its App or application is in place; with
workspaces on and none in place yet, no instance renders, and muster's
credential provider releases the configured instance (github.com's OAuth
issuer before the App id). The wiring every instance shares is in place with
the switch alone:
the manager's public base URL `https://workspace-manager.<domain>:<gatewayPort>`
and the route the chart renders for it
([Platform](platform.md#the-workspace-managers-provider-instances-platformworkspacesprovider)).
`platform-test` proves the manager registered with muster and reachable,
rolled out, `list_providers` as the admin naming every configured
instance and nothing else, and [the lab's Organization](#the-labs-organization).

### The person's grant, muster's

From workspace-manager 0.10.0 a person's sign-in at a provider is muster's:
muster pins the manager's registration to one instance's App
(`muster.mcpServer.auth` in the values), the person grants that App once
(`core_auth_login`, `grantScope: subject`), and muster attaches the person's
App token to every call of the manager's tools, the Dex ID token beside it.
A person who has not granted it gets muster's sign-in link instead of a
tool's answer; the manager stores no token and serves no sign-in pages.

The instance is `platform.workspaces.grantProvider` (`agentlab configure
--workspaces-grant-provider <instance>`), empty for the first configured one.
The lab pins only a manager it
resolves to 0.10.0 or newer, the way Flux resolves
`components.workspace-manager.versionRange` (from the chart, or a range in
`platform.valuesFiles` such as a release candidate's); an earlier manager
keeps its own sign-in and the token exchange the lab registers for it. The
pin's OAuth client is the instance's Secret, which carries the client id
under `client-id` for muster beside `client-secret`: the lab writes it into
`workspace-github` or `workspace-gitlab` ahead of the install. The App's callback URL is muster's,
`https://muster.<domain>:<gatewayPort>/oauth/proxy/callback`.

#### Provider Apps side by side

`platform.workspaces.grantProvider: apps` (`agentlab configure
--workspaces-grant-provider apps`, two or more instances) pins nothing: the
lab renders no `muster.mcpServer.auth.provider`, and an agent-platform chart
that keeps one muster provider App per instance
(`agent-platform.workspaceManager.providerApps`) derives an App of each
instance with person sign-in, GitHub's and GitLab's side by side, and the
registration's `providerApps` naming them all. A person grants each App once
by name (`core_auth_login app=<instance>`), `list_providers` lists each
instance connected or not, and `core_auth_logout app=<instance>` drops one
App's grant and no other. The lab writes `client-id` into every instance's
Secret. On a chart without provider Apps the registration falls back to the
chart's single-App pin; `workspaces-test --part gitlab` then fails its
preconditions naming the MCPServer's `providerApps`.

A person's grant of a GitHub App takes github.com's sign-in in a person's
browser: the real GitHub App login, through a browser worker with an SSO
session; no headless walk grants it. A proof that calls the manager's tools
as a person finds the grant held, or answers muster's sign-in link, and
`platform-test` then ends its workspace-manager stage at that link: its
verdict passes the link and reports `list_providers` and `list_workspaces`
as SKIP, never as proven. The
headless sign-in walk exists only for GitLab
([the GitLab part](#the-gitlab-part)).

### What a run leaves: nothing

The proofs share one lab with each other and with the Dev Portal's
Playwright suite, so none of them assumes a clean lab or leaves state
behind (`internal/lab/workspacefixtures.go`):

- Every Workspace a proof creates carries `agentlab.giantswarm.io/fixture:
  workspace` and the run's id under `agentlab.giantswarm.io/run`; the
  manager takes no labels, so the lab sets them on its object after
  `create_workspace`.
- Every grant of the pinned App a proof uses goes into the run's ledger and
  is dropped in muster (`core_auth_logout`) when the command ends, on a
  failure too: after `platform-test`, `workspaces-test` or
  `klaus-gateway-test` no
  lab user holds a grant, so the portal's sign-in specs and the gateway's
  workspace prompt see the same lab used as fresh.
- `workspaces-test` and `klaus-gateway-test` start by removing what an earlier run left: the lab's
  labelled Workspaces of another run, and the portal suite's `e2e-ws-<run>-…`
  of a run that started more than three hours ago (the suite's own
  leftover age, so a portal run going on keeps its own). Each is deleted
  with its deletion cycle requested at once; one a live Session's directory
  still holds stays deleting, which no prompt or picker offers.

### The lab's Organization

The workspace tools act only for a member of a workspace's Organization, and
an Organization the manager is not given has no members. With workspaces on,
the values give it one, `lab`, whose members are the lab Dex's
`platform-admins` and `developers`; the `viewers` stay outside. On a chart
that forwards `workspaces.organizations` (agent-platform 4.127.0 and newer)
it is rendered there, `[{name: lab, memberGroups: [platform-admins,
developers]}]`, which the chart turns into the manager's
`--organization=lab=platform-admins,developers`; on an older chart that takes
the workspaces values, as the subchart's own
`workspace-manager.organizations` map. `platform-test` calls
`list_workspaces` for `lab` as the admin, which answers, and as the viewer,
who is kept out: refused as not a member when they hold the pinned
provider App's grant, or asked by muster's `auth_required` and its sign-in
link to grant it first; the stage's note says which. A workspace a lab user
creates names
`organization: lab`.

### The provider instance github

`agentlab configure --workspaces-provider github` (the default, or
`github,gitlab`) gives the workspace-manager a real GitHub. The instance renders once a GitHub App of
the lab's own is in place (`--workspaces-github-app-id` and
`--workspaces-github-client-id` in `agentlab.yaml`, the keys in the Secret
`agent-platform/workspace-github`:
[Platform](platform.md#the-workspace-managers-provider-instances-platformworkspacesprovider)).
With `platform.workspaces.github.privateKeySource` and `clientSecretSource`
recorded (`--workspaces-github-private-key-source`,
`--workspaces-github-client-secret-source`), every `up` and `platform` places the two keys through the
operator's secret tooling, so a recreated lab keeps the instance; without
them the Secret is placed by hand, and a lab without it renders no `github`
instance.

The App is registered once, by an owner of the organization that holds it,
in GitHub's UI (Settings, Developer settings, GitHub Apps), and serves every
lab:

- **Callback URLs**: each lab's muster callback
  `https://muster.<domain>:<gatewayPort>/oauth/proxy/callback`
  (`agentlab configure` prints it; a GitHub App takes up to ten).
- **User-to-server tokens with expiry** on; no device flow, no webhook.
- **Repository permissions**: Metadata read; Contents, Pull requests and
  Issues read and write; Checks read.
- Installed on the organizations whose repositories the lab's workspaces
  mirror.

It is an App of its own: the lab Dex's [sign-in App](github-signin.md) reads a
profile and nothing on repositories, and muster's `github` server has its own
client with muster's callback.

### The provider instance gitlab

`agentlab configure --workspaces-provider github,gitlab` (or any list naming
`gitlab`) gives the workspace-manager a GitLab, CE or EE, self-managed or
gitlab.com, of the `gitlab` kind. `agentlab.yaml` records what is public:

```yaml
platform:
  workspaces:
    provider: github,gitlab
    gitlab:
      url: https://gitlab.example.com   # https, no path
      clientId: <the OAuth application's application id>
      group: workspaces                 # the group the proof's workspace mirrors
      project: workspace-proof          # one of its projects
      person: <a GitLab username>       # who the proof signs in to GitLab as
      tokenSource: <reference>          # the group access token
      clientSecretSource: <reference>   # the OAuth application's secret
      passwordSource: <reference>       # the person's GitLab password
```

The three values are the Secret `agent-platform/workspace-gitlab` (keys
`token`, `client-secret`, `person-password`). With the sources recorded, every
`up` and `platform` places them through the operator's secret tooling;
agentlab never reads a value. The instance renders once the URL, the client
id and the first two keys are in place; the part's person needs the third.

GitLab's side, set up once by an administrator of the instance:

- An instance-wide **OAuth application** (Admin Area, Applications), scopes
  `api read_user read_repository write_repository`, redirect URIs each lab's
  muster callback `https://muster.<domain>:<gatewayPort>/oauth/proxy/callback`
  (`agentlab configure` prints it), confidential. A lab with a new muster host
  adds its callback to the application before its first connect; GitLab
  answers any other with "The redirect URI included is not valid".
- A **group access token** of the group, role Reporter, scopes `read_api
  read_repository`: the sync lists the group's projects and mirrors them over
  HTTPS with it.
- The **person**: a GitLab user with Developer on the project, signing in
  with a password (no second factor), so the part walks GitLab's sign-in form
  headlessly.
- The lab's node reaches the GitLab host over HTTPS, and the host's
  certificate chains to a public root.

With `grantProvider: gitlab`, muster's pin of the workspace-manager names
GitLab's endpoints, `/oauth/revoke` included once the workspace-manager's
chart renders it (0.11.0-rc.6 and newer): a disconnect in muster
(`core_auth_logout`, muster 5.47.0 or newer) revokes the person's tokens at
GitLab, and their next sign-in walks the application's consent again.
