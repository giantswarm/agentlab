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
controller with its proxy, the node plugin, the class, whose
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

### Turning it on

```sh
agentlab configure --workspaces   # needs --agents (on by default)
agentlab up                       # or `agentlab platform` on a running lab
agentlab workspaces-test          # --storage-only: the storage alone
agentlab workspaces-test --part sync   # workspaces defined through the manager, their mirrors
agentlab workspaces-test --part access # provider sign-in and the access check at a Session's start
agentlab workspaces-test --part gitlab # the provider instance gitlab, end to end (skips without one)
agentlab configure --workspaces-cleanup-after 10m && agentlab platform   # the sessions part's short cleanup window
agentlab workspaces-test --part sessions # ten parallel Sessions in their session directories, and the cleanup
agentlab configure --workspaces-provider fake,fake-bearer && agentlab platform   # a Bearer provider
agentlab workspaces-test --part caller-credential # a Session's actor JWT on its Bearer provider's API host
```

With the switch off, nothing of it is installed, and `agentlab platform` on a
lab that had it removes it.

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
   template, and the step reports the skip with that refusal.
9. A claude Session admitted on the volume ([the Session admission](#the-session-admission)),
   left out with `--storage-only`:
   - The rig: the workspace-manager's `/.well-known/jwks.json` publishes the
     lab's grant key (its key id quoted), kagent's jwt admission names that
     key set, the manager's issuer and the audience, and the Harness `claude`
     is Ready.
   - The mirror laid out the way the workspace sync lays it out:
     `mirrors/agentlab-org/platform-api.git`.
   - A claude Agent `agentlab-workspaces-claude` Ready on the Harness
     `claude`.
   - A Session without a volume, as the admin: its turn, a Bash probe, runs in
     `/data/workspace` and sees no `/mirrors`. Its creator is the person the
     grants name.
   - Refused at create, each with kagent's reason: a volume without a grant,
     a grant signed by a key the manager does not publish, and a grant for
     another person.
   - A Session with the volume source the manager's grant allows
     (`sessions/${SESSION_ID}` read-write at `/workspace`, `mirrors`
     read-only at `/mirrors`) and a grant signed with the lab's key: its turn
     runs in `/workspace`, sees `agentlab-org/platform-api.git` under
     `/mirrors`, is refused a write into the mirrors, and writes a file that
     a pod then reads on the volume at `sessions/<Session id>`.
   - The session directory: the turn's own
     `git clone /mirrors/agentlab-org/platform-api.git` succeeds as the
     harness user (its uid:gid quoted), and the runtime's setup clone
     `agentlab-org/platform-api` is in its directory (its `origin/HEAD`
     quoted). Either failing fails the step with git's message and the
     controller's version.
   - The Sessions and the Agent removed.
10. Everything removed: the proof's namespace `agentlab-workspaces-test` with
    its claim, and the volume's directory gone from the export.

`--ready-timeout` bounds each wait (default 5m). A resume on
another node is proven on a cloud installation, not in the lab: the lab's
export is one node's disk.

### The sync part

`agentlab workspaces-test --part sync [email]` defines workspaces through the
workspace-manager's tools, through muster as `[email]` (default the admin; a
member of [the lab's Organization](#the-labs-organization)), and proves what
the sync keeps on their volumes. It runs on [the provider instance
`fake`](#the-provider-instance-fake), which it drives as an owner would: a
push, a force-push and a topic removed, as the App's installation, with the
key the lab generated. Without the workspace-manager registered with muster,
or without the instance `fake`, it fails at once with the reason. It runs no
Session and no model.

1. **Preview**: `preview_workspace` for two drafts. The filter draft has the
   languages `HCL` and `MDX` and the topics `infrastructure` and `docs` on
   both owners, `agentlab-org` and `agentlab-oss`, and selects the private
   `agentlab-org/platform-infra` and the public `agentlab-oss/docs`. The
   list draft names `agentlab-org/platform-api` and `agentlab-oss/kagent` (a
   fork, which a name selects). Each repository is printed with the selector
   that takes it. Where the App is not installed on `agentlab-org`, the
   private repository's step is skipped with that reason and the filter
   covers `agentlab-oss` alone.
2. **Create**: `create_workspace` for both, `agentlab-sync-filter` and
   `agentlab-sync-list`; `list_workspaces` for `lab` lists both and
   `get_workspace` answers each.
3. **Outside the Organization**: the viewer is refused `list_workspaces` for
   `lab` as not a member, its own listing is empty, and `get_workspace` of
   each answers permission denied.
4. **The first cycle**, which the create starts: a bare mirror of each
   repository at `mirrors/<owner>/<name>.git` on the workspace's volume, read
   by a pod: bare, its `HEAD`, branches and tags, `gc.auto=0` and
   `gc.pruneExpire=never`. Each head is the provider's (`git ls-remote`) and
   the manifest's. The request log holds a fetch of each of the four
   repositories.
5. **No-op**: `sync_workspace` with nothing changed. The cycle ends
   `NoChange` without a sync job, the request log holds listings and no git
   request, and the manifest (the ConfigMap the sync writes) is byte for byte
   the one before.
6. **Push**: a commit pushed to `agentlab-oss/docs`, then a cycle. The
   request log holds git requests for `docs` alone, the manifest carries its
   new head, and the other head is unchanged.
7. **Force-push**: a session directory `sessions/agentlab-sync-proof` with a
   `git clone --shared` of the mirror (its objects borrowed through git
   alternates, none of its own), then a force-push of `docs` and a cycle that
   fetches only it. The mirror's branch is at the new head, the overwritten
   commit stays in the mirror unreachable (repacked with
   `--keep-unreachable` while a session directory borrows from it), and the
   clone reads every object it reaches: none missing, `git fsck
   --connectivity-only` clean, the overwritten commit's file readable.
   `get_workspace` lists the session directory.
8. **Topic removed**: the topic `docs` taken off `agentlab-oss/docs`. The next
   cycle drops it from the selection (`droppedAt` in the manifest, one
   repository selected), fetches nothing, and its mirror stays on the volume
   with its borrower.
9. **Borrower gone**: the session directory removed, as its Session's end
   would remove it. The next cycle, `NoChange` with a sync job that looks
   after the volume, deletes the dropped mirror and its manifest entry.
10. **Update**: `update_workspace` replaces `kagent` with `docs` by name in
    the list workspace. Its cycle mirrors `docs` and deletes `kagent`'s
    mirror, which no session directory borrows.
11. **Delete**: `delete_workspace` for both. Each workspace, its claim and its
    PersistentVolume go, nothing of it is left under the export, no session
    directory being left.
12. **Restore**: `agentlab-oss/docs` back to the fixture's topics and the head
    it had, and this host's clone removed. A failed run removes the same.

The workspace-manager answers an owner's listing from its cache for a
minute: a cycle that must see a change at the provider is requested once a
minute has passed since the last listing, and the part starts a minute after
the provider's last change in the request log. The no-op is shown by the
request log and the manifest, never by timing. The run takes about six
minutes; `--ready-timeout` bounds each cycle.

### The access part

`agentlab workspaces-test --part access [email]` proves the access check at a
Session's start: a person signed in to every provider of a workspace, and able
to read every repository of its last sync, gets a workspace grant from the
workspace-manager's `authorize_workspace` and starts a Session with it through
agent-manager's `start_session`. Anyone else is refused, naming what is
missing, and nothing of the refused start exists. It runs as two people of
[the lab's Organization](#the-labs-organization): the admin and `[email]`
(default `dev@lab.local`, who does not read `agentlab-org/platform-infra`). It
uses [the provider instance `fake`](#the-provider-instance-fake), the
[Session admission](#the-session-admission) and agent-manager 1.12.0 or later
(`start_session` with `workspaceGrant`). Without any of them it fails at once
with the reason.

1. **Preconditions**: the workspace-manager connected to muster. The
   manager publishes the lab's grant key, kagent's admission trusts its key
   set, and the claude Agent `agentlab-workspaces-claude` is Ready on the
   Harness `claude`. Both people are signed in to muster and to kagent.
2. **Private repository**: `agentlab-org/platform-infra` is in the workspace
   where the lab GitHub's App is installed on `agentlab-org`. Otherwise the
   step is skipped with that reason, and so is step 5.
3. **Workspace**: `create_workspace` for `agentlab-access`, over
   `agentlab-oss/docs` and the private repository, as the admin. The part
   waits for its first cycle and reads the volume's CSI driver and handle.
4. **Not connected**: `[email]` is disconnected first (`list_providers` shows
   no sign-in). `authorize_workspace` answers `connect_required`, with the
   reason and the provider's sign-in link, and no grant. A start on the
   workspace's volume without a grant (kagent `CreateSession` as the person)
   is refused by the admission. `start_session` names a workspace only
   through a grant. Without one it starts a Session with no volume and no
   provider token, as before workspaces.
5. **Connect**: `connect_provider` as `[email]`. Its link is walked through
   the lab GitHub's consent and the manager's callback
   ([a person's sign-in, headlessly](#a-persons-sign-in-headlessly)), and
   `list_providers` then shows `fake` connected.
6. **Refused**: `authorize_workspace` as the connected `[email]` answers
   `refused`, naming `agentlab-org/platform-infra` in its reason and its
   `unreadable`.
7. **Grant**: a commit pushed to `agentlab-oss/docs` after the sync, the
   admin connected, then `authorize_workspace` as the admin. Within the
   manager's one-minute listing cache, the call repeats until the push shows.
   The grant names the admin (`sub`), the workspace, its volume (driver and
   handle), the mounts (`sessions/${SESSION_ID}` read-write, `mirrors`
   read-only) and `agentlab-oss/docs` as changed since the sync.
8. **Start**: `start_session` as the admin with the grant. agent-manager
   creates the Session on kagent as the person, and kagent reports it on the
   workspace's volume.
9. **Admission**: three grants refused by kagent's admission, each sent
   straight to kagent's `CreateSession` with the workspace's volume:
   - the admin's grant used by `[email]`;
   - an expired grant: the manager's claims re-signed with the lab's grant
     key, the manager's own, with the expiry five minutes past;
   - a grant for another workspace and its volume, signed the same way.

   For the first two, `start_session` is also refused, by agent-manager
   before kagent, each with its reason. The default proof's admission step
   already refuses a grant signed by a key the manager does not publish, so
   that refusal is not repeated here.
10. **Disconnect**: `disconnect_provider` as the admin. The lab GitHub's
    request log shows the revocation (`DELETE
    /api/v3/applications/<client>/grant` or `/token`, answered 2xx),
    `list_providers` no sign-in, and `authorize_workspace` answers
    `connect_required` again. A grant issued before stays valid until it
    expires: a revocation after the start is not checked again.
11. **Latency**: every `authorize_workspace` call is timed, and the count,
    minimum, median and maximum are printed.
12. **Restore**: the Sessions, both sign-ins, the Agent, the session
    directories, the workspace and its volume removed, and `agentlab-oss/docs`
    back to its head. A failed run removes the same.

After every refused start (steps 4, 6 and 9), the part lists what exists for
the person: the Agent's Sessions in kagent, agent-manager's `list_sessions`,
and Substrate's actors. Nothing may be new. One Session runs a model turn,
the admitted one: its first message is a one-word reply on the default
ModelConfig. On the instance `github`, the same steps need one recorded
browser sign-in of a person's account; the headless part does not run them.

### The GitLab part

`agentlab workspaces-test --part gitlab [email]` proves
[the provider instance `gitlab`](#the-provider-instance-gitlab) end to end
against a real GitLab, as the admin (connected to GitLab as
`platform.workspaces.gitlab.person`) and `[email]` (default `dev@lab.local`,
never connected). Without the instance, or without any of its settings, every
stage skips with the reason and the part passes: it never fails for a lab
that has no GitLab.

1. **Preconditions**: the workspace-manager connected to muster with the
   instance `gitlab` in `list_providers`, the token exchange's client in
   place, the [Session admission](#the-session-admission), the claude Agent
   Ready, both people signed in.
2. **Picker**: `list_repositories` for the group as the admin lists its
   projects, what the Dev Portal's workspace editor shows, with no
   provider-specific request.
3. **Not connected**: the token exchange (`POST /token` as kagent's client)
   for `[email]` and the audience `gitlab` answers `invalid_target` with the
   connect link as `error_uri`.
4. **Connect**: `connect_provider` as the admin, walked headlessly through
   GitLab's sign-in form, the OAuth application's consent and the manager's
   callback; `list_providers` reads connected.
5. **Token**: the exchange releases the admin's GitLab token for the audience
   `gitlab` and refuses an unknown audience. GitLab's API names the person
   with it (Bearer), git's smart HTTP lists the project with it (Basic, user
   `oauth2`), and the person writes a branch, a commit and a merge request
   with it from the host, all removed again.
6. **Workspace**: `create_workspace` over the project, its first cycle: the
   bare mirror at GitLab's head and the manifest naming it.
7. **Start**: `authorize_workspace` grants, naming GitLab's host for the
   audience `gitlab` and no token; `start_session` through agent-manager
   creates the Session on the volume, whose turn works in its directory with
   the clone at GitLab's head and holds no credential beyond the platform's
   own variables.
8. **Push from the Session**: the Session pushes an empty commit to a proof
   branch as the person. It reaches GitLab only through kagent's token
   broker, which exchanges the turn caller's token at the workspace-manager
   for each host the grant names; until the platform wires that broker, and
   while GitLab's API (Bearer) and git (Basic) on one host need two
   credentials where the egress binds one per host, the stage reports the
   push's answer and skips.
9. **No workspace**: a Session started without a workspace works in the
   default directory, holds no credential beyond the platform's own, and
   `git ls-remote` of the private project is refused.
10. **Disconnect**: `disconnect_provider`: the exchange answers with the
    connect link again, and GitLab refuses the token released before.
11. **Restore**: the Sessions, the Agent, the workspace, its volume, the proof
    branches and merge requests removed.

### The sessions part

`agentlab workspaces-test --part sessions [--sessions n]` proves what a
workspace is for: Sessions started in parallel on one workspace, each at once
in a session directory of its own on the workspace's one volume, on every
Harness, and the directories' cleanup after their Session's last turn. It runs
as the admin on [the provider instance `fake`](#the-provider-instance-fake),
with the [Session admission](#the-session-admission), agent-manager 1.13.0 or
later (`start_session` without a first message), workspace-manager 0.4.0 or
later run with kagent's address (agent-platform 4.128.0 or later derives it)
and a short cleanup window: `agentlab configure --workspaces-cleanup-after
10m` (`platform.workspaces.sessionsCleanupAfter`, the manager's
`sessions.cleanupAfter`; the part takes 5 to 15 minutes), then `agentlab
platform`. Without any of them it fails at once with the reason.

1. **Preconditions**: the workspace-manager connected to muster, with
   `--kagent-address` and the window; the lab's grant key published and
   trusted by kagent's admission; the Agents `agentlab-workspaces-claude` on
   the Harness `claude` and `agentlab-workspaces-kagent` on the Harness
   `kagent` (the Go ADK) Ready.
2. **Workspace**: the sync part's filter workspace `agentlab-sync-filter`
   (`agentlab-oss/docs`, and the private `agentlab-org/platform-infra` where
   the App is installed on its owner), created as the admin; its first
   cycle.
3. **Push**: a push to `agentlab-oss/docs` after that cycle; the admin
   connected to `fake`.
4. **Start**: `--sessions` Sessions (default 10, at least 3), alternating the
   two Harnesses, started at once: each asks `authorize_workspace` for a grant
   that names `docs` changed since the sync, starts through `start_session`
   with it, and runs its first turn. The turn lists its working directory
   (`/workspace`) and the `origin/HEAD` of every clone at
   `<owner>/<repository>`, `docs` at the pushed head, and writes a file only
   it knows (`agentlab-only-<token>`).
5. **Directories**: a probe on the volume reads one directory per Session
   under `sessions/<session id>`, each with the setup's record
   (`.kagent/workspace.json`: when it was set up, what was cloned, `docs`
   fetched) and its own file alone. The lab GitHub's request log since the
   push holds exactly one fetch (`GET .../info/refs`) of `docs` per Session
   and none of any other repository.
6. **Isolation**: a second turn on every Session at once looks for every
   `agentlab-only-*` file it can reach and tries a write into `/mirrors`: it
   finds its own file only, and the write is refused.
7. **Delete**: a cycle brings the mirrors up to the push; the last Session is
   deleted, and no cycle is requested: within 2 minutes its directory is
   gone and the workspace's last cycle has the trigger `sessionDeleted` (the
   workspace-manager lists kagent's Sessions every `--sessions-poll` and
   removes a deleted one's directory at once, from 0.6.0; before, without
   that flag, the stage requests the next cycle, which removes it), the
   other directories, the mirrors' heads and the volume untouched.
8. **No workspace**: a Session started without a grant on each Harness works
   in an empty directory, sees no `/mirrors`, and its turn completes.
9. **Cleanup**: the Sessions suspended, the window waited out, a cycle:
   every directory removed and the Workspace's `SessionsCleanedUp` condition
   `True` (the part fails naming its reason otherwise). One Session per
   Harness resumed works in a fresh directory, the clones back and its file
   of before gone; the kagent Harness's first turn says the directory was set
   up again. The claude Harness does not say so yet: the part notes it as a
   known gap.
10. **Report**: per Session the times of `start_session` called and
    answered, the first turn sent, the directory set up (the setup's record)
    and the first turn ended, and the setup's seconds, as `timing
    session=<id> harness=<h> ... setupSeconds=<s>` lines and in
    `state/workspaces-sessions-report.json` with the directories, the heads,
    the fetch counts and the condition.

Everything is removed on every path: the Sessions, the Agents, the session
directories, the workspace, the admin's sign-in, and `docs` back at its head.

### The caller-credential part

`agentlab workspaces-test --part caller-credential` proves a Session's caller
credential on its Bearer provider end to end: kagent compiles, on the Actor of
a Session admitted with a grant, a credential header on each of the grant's
**Bearer** providers' hosts, an actor JWT that Substrate's egress gateway mints
for that Actor, bound to the provider's audience, and injects in place of the
`Authorization` the sandbox sent. A **Basic** provider's host (a git host) gets
none yet. It runs as the admin on [the provider instance
`fake-bearer`](#the-provider-instance-fake-bearer), the only instance whose
grant names a Bearer host (the API of `fake` is on its git host), with the
[Session admission](#the-session-admission) and agent-manager's
`start_session`: `agentlab configure --workspaces-provider fake,fake-bearer`,
then `agentlab platform`. Without them it fails at once with the reason.

1. **Preconditions**: the workspace-manager connected to muster and listing
   `fake-bearer`; the lab's grant key published and trusted by kagent's
   admission; the Agent `agentlab-workspaces-bearer` on the Harness `claude`
   Ready, its egress `https://github-bearer.<domain>` and
   `https://api.github-bearer.<domain>`.
2. **Workspace**: the admin connected to `fake-bearer`; the workspace
   `agentlab-caller-credential` over `agentlab-oss/docs` on it and its first
   cycle.
3. **Session**: `start_session` with the admin's grant; kagent's record of
   the Session carries the providers of the grant, among them
   `api.github-bearer.<domain>` as a `Bearer` provider of audience
   `fake-bearer` (a grant without it fails here, naming the manager).
4. **The Actor's JWT**: a turn runs `curl` (else `wget`) in the sandbox to
   `https://api.github-bearer.<domain>/agentlab/caller-credential/<token>`
   with `Authorization: Bearer agentlab-placeholder`, and names the
   environment variables and the files under `/data`, `/workspace` and its
   working directory that hold a JWT. The lab GitHub's request log must hold
   that request with the credential `jwt`, `aud` containing `fake-bearer` and
   `sub` `atespaces:<atespace>:actors:session-<id>`, the Session's Actor; the
   sandbox must hold no JWT. A kagent that compiles no caller credential lets
   the placeholder through: the stage fails naming the credential the API host
   received (`placeholder`) instead of the Actor's JWT; a request that never
   reached the fake fails naming the sandbox's answer (the gateway's 403 when
   the host is not in the Actor's egress).
5. **No workspace**: a Session of the same Agent without a grant sends the
   same request: no `jwt` entry for it in the log.

The fake verifies no JWT it did not sign: it answers such a bearer GitHub's
401 and logs its `aud` and `sub`, never the token. Everything is removed on
every path: the Sessions, the Agent, the session directories, the workspace
and the admin's sign-in to `fake-bearer`.

### The measurements

`agentlab workspaces-measure [email]` measures what the parts prove: how long
a workspace's sync, a Session's directory and the git work in it take on the
workspace volume, how much the volume stores, and whether the design's sizing
defaults hold (a repository not yet mirrored counts 1.5 × the size its
provider reports, once synced its measured mirror; the headroom is max(4 GiB,
50 % of the need); an online expansion where the class provisions a size). It
works through muster as `[email]` (default the admin, a member of the lab's
Organization) on [the provider instance `fake`](#the-provider-instance-fake),
with the [Session admission](#the-session-admission) and agent-manager's
`start_session`; Kubernetes is only read, apart from the probe pods.

Two workspaces, each created and deleted once per run (`--runs`, default 5):

| Workspace | Repositories | Pushes go to | Checkout, status and build in |
|---|---|---|---|
| `agentlab-measure-small` | `agentlab-oss/docs`, `agentlab-oss/kagent` | `docs` | `kagent` |
| `agentlab-measure-large` | `agentlab-org/monorepo` (64 MiB), or with `--large-repo-mib <n>` a repository of n MiB the command adds to the lab's GitHub (`agentlab-org/monorepo-<n>mib`) and removes afterwards | the large one | the large one |

The two have an owner each, so the manager's one-minute listing cache of one
never shortens the other's cycle. Per run and workspace, each figure from the
call to the first read that sees its end (reads every 500 ms):

| Figure | From → to |
|---|---|
| First sync | `create_workspace` called → `get_workspace` reports the first cycle's result |
| No-op cycle | `sync_workspace` called once the listing cache let go, nothing pushed → the cycle's `NoChange` (a cycle ending otherwise is not counted, and says so) |
| Changed cycle | `sync_workspace` called after a push → the cycle's result, the mirror at the pushed head |
| `authorize_workspace` | each call through muster → its granted answer |
| Session directory setup | `start_session` called with the grant → the setup's record `.kagent/workspace.json` in the session directory: the actor Running, every clone in place, the pushed repository fetched |
| Setup in the actor | the first turn sent → the same record |
| On-demand fetch | after another push, the setup's own `git fetch --quiet --prune origin` timed in the Session's sandbox by its second turn → the fetch ended at the pushed head |
| First checkout, `git status`, build | in a pod with the Session's mounts (its directory at `/workspace`, the mirrors read-only at `/mirrors`): `git switch <default branch>` in the Session's clone (the Harness's first checkout), `git status --porcelain`, `go build ./...` with a cold build cache beside the clone; then the same in a clone of a copy of the mirror on an `emptyDir`, the node's local disk |
| Session directory size | `du -sb` of the directory after the turns and the checkout, the build's cache and output removed |

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
cannot report is listed as not available with the reason, never estimated.
In the lab the NFS export lives on the node's own disk, so the volume against
the local disk measures the NFS protocol, not a network.

Everything the command creates is removed on every path: the Sessions, the
session directories, the workspaces and their volumes, the Agent
`agentlab-workspaces-measure` (Harness `claude`), the probe pods, the
person's sign-in at `fake`, the pushed repositories back at their heads and a
generated repository.

**Another cluster with the platform**: `--kube-context <context>` (of your own
kubeconfig) and `--muster-url <url>`, as the person `muster auth login
--endpoint <url>` signed in, with `--organization`, `--provider`,
`--small-repos owner/a,owner/b` (the first is checked out and built) and
`--large-repo owner/name`. The command pushes only to the lab's GitHub, so
there the changed cycle and the on-demand fetch are not available;
`--agentgateway-url` and `--agent namespace/name` (an Agent whose Sessions
reach the provider) measure the Sessions, which are otherwise not available.

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
person signed in to its provider, which [the access part](#the-access-part)
sets up. There, the manager's grant names the person by email, the Session's
creator as kagent's admission compares it, and admits the Session.

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
`platform.workspaces.provider` names: `fake`, the default while the key is
empty; `fake-bearer`, beside `fake` only; `github`; `gitlab`; or several,
such as `fake,github,gitlab` (`agentlab configure --workspaces-provider …`).
`fake`, `fake-bearer` and `github` are instances of the chart's `github` kind, `gitlab` of its `gitlab` kind, each
with a Secret of its own that the rendered values reference and nothing else
reads. The wiring every instance shares is in place with the switch alone:
the manager's public base URL `https://workspace-manager.<domain>:<gatewayPort>`,
the route the chart renders for it, and the lab Dex's redirect URI
`<base URL>/signin`
([Platform](platform.md#the-workspace-managers-provider-instances-platformworkspacesprovider)).
`platform-test` proves the manager registered with muster and Connected,
rolled out, `list_providers` as the admin naming every configured
instance and nothing else, and [the lab's Organization](#the-labs-organization).

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
which is refused as not a member. A workspace a lab user creates names
`organization: lab`.

### The provider instance fake

[The lab's GitHub](#the-labs-github) as a GitHub Enterprise-shaped instance,
nothing to register and no account needed. `agentlab up` and `agentlab
platform` run it (`agentlab github-fake --workspaces`) in a container on the
kind network beside the nodes, the way a proof's fake runs, from a copy of
the binary under `state/` (a mounted executable stays busy while its
container runs, and a rebuild over it would be refused); a running one is
replaced, so the fixture is its state, and `agentlab platform-down` and
`agentlab down` remove it. The binary must be a static Linux build (`make
build`, or `CGO_ENABLED=0 go build`; the releases are): the container's image
has no dynamic loader, so `up` and `platform` refuse a dynamically linked one
before their first step, naming the build, rather than after the storage
install when the container would start. Pods reach it as `https://github.<domain>`: CoreDNS
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

### The provider instance fake-bearer

The same [lab's GitHub](#the-labs-github), App and Secret as `fake`, shaped
like github.com: `url: https://github-bearer.<domain>` (the git host, Basic)
and `apiURL: https://api.github-bearer.<domain>` (the REST API at the root of
a host of its own, Bearer), so the workspace-manager's grant names a Bearer API
host beside the Basic git host, as one for github.com does. CoreDNS sends both
names to the fake's Service, its TLS leaf carries them, and the fake serves
the API at the root of the API host (`agentlab github-fake --api-host`): the
answers' web and clone URLs name the git host, their API URLs and `Link`
headers the API host. `platform.workspaces.provider` names it beside `fake`
(`fake,fake-bearer`), whose container serves it; the callback URL is
`<base URL>/callback/fake-bearer`, the audience of its credentials
`fake-bearer`. [The caller-credential part](#the-caller-credential-part)
proves it.

#### A person's sign-in, headlessly

`agentlab workspaces-signin-test` connects people to the instance the way a
browser does and proves what only a connected person sees. A workspace of the
lab's Organization over `agentlab-org/platform-api` and `platform-infra` is
created and synced as the admin. The admin and `dev@lab.local` then each
connect: `connect_provider` through muster as the person, its link walked with
the person's fixture login added, the fake's consent (302) to the manager's
`/callback/fake`, which sends a browser without the manager's page session to
Dex (303), Dex's local login with the person's lab credentials (a connector
chooser in front of it is passed through), `/signin` setting the page session
and returning to the callback, which completes the sign-in (200), and
`list_providers` showing the person connected. `authorize_workspace` then
answers `granted` for the admin and `refused` naming
`agentlab-org/platform-infra` for the developer, who cannot read it. The
workspace is deleted and both people disconnected at the end.

The walk runs on this host: the platform's names on the edge's loopback port,
Dex on its issuer URL, and `github.<domain>`, which the edge has no route for,
on the loopback port the fake's container publishes. Each hop is printed as
method, URL without its query, and status, and a walk that stops says at which
hop, with which status and the page's heading. The walk is
`connectWorkspaceProvider` in `internal/lab/workspacesignin.go`, which any
proof that needs a connected person calls.

### The provider instance github

`agentlab configure --workspaces-provider github` (or `fake,github`) gives the
workspace-manager a real GitHub. The instance renders once a GitHub App of
the lab's own is in place (the ids in `agentlab.yaml`, the keys in the Secret
`agent-platform/workspace-github`:
[Platform](platform.md#the-workspace-managers-provider-instances-platformworkspacesprovider)).
With `platform.workspaces.github.privateKeySource` and `clientSecretSource`
recorded, every `up` and `platform` places the two keys through the
operator's secret tooling, so a recreated lab keeps the instance; without
them the Secret is placed by hand, and a lab without it lists only `fake`.

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

### The provider instance gitlab

`agentlab configure --workspaces-provider fake,gitlab` (or any list naming
`gitlab`) gives the workspace-manager a GitLab, CE or EE, self-managed or
gitlab.com, of the `gitlab` kind. `agentlab.yaml` records what is public:

```yaml
platform:
  workspaces:
    provider: fake,gitlab
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
  `api read_user read_repository write_repository`, redirect URI each lab's
  `https://workspace-manager.<domain>:<gatewayPort>/callback/gitlab`
  (`agentlab configure` prints it), confidential.
- A **group access token** of the group, role Reporter, scopes `read_api
  read_repository`: the sync lists the group's projects and mirrors them over
  HTTPS with it.
- The **person**: a GitLab user with Developer on the project, signing in
  with a password (no second factor), so the part walks GitLab's sign-in form
  headlessly.
- The lab's node reaches the GitLab host over HTTPS, and the host's
  certificate chains to a public root.

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
as GitHub reports it. `monorepo` and `kagent` carry a Go module (`go.mod`,
`main.go`) for the measurements' build.

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
| The App | `GET /app`, `/app/installations`, `/orgs/{o}/installation`, `/users/{u}/installation`, `/repos/{o}/{r}/installation`; `POST /app/installations/{id}/access_tokens` (optionally limited to `repositories`). The App's JWT is RS256, signed by the key the lab generated, issued by the App id, valid for at most ten minutes. `PUT` and `DELETE /app/installations/{id}/suspended`: a suspended installation is listed with `suspended_at` and `suspended_by`, issues no token (403), its tokens are refused (403), and neither they nor the App's user tokens act on the owner's repositories (its public ones stay readable); `agentlab github-fake suspend <owner>`, `unsuspend <owner>` and `installations` drive it from the host, the lab's stand-in for an organization owner's Suspend on github.com. |
| Listings | `GET /orgs/{o}/repos`, `/users/{u}/repos` (`type`), `/user/repos` (`visibility`), `/installation/repositories`, `/repos/{o}/{r}`: `language`, `topics`, `archived`, `fork`, `pushed_at`, `size`, `permissions`; `sort` (`full_name`, `pushed`, `updated`), `direction`, `per_page`, `page` with the `Link` header. |
| Topics | `GET /repos/{o}/{r}/topics`; `PUT /repos/{o}/{r}/topics` with `names` replaces them, lower-cased, for a caller that writes to the repository (an installation token on its owner, a user with write). It is no push: `pushed_at` stays. |
| OAuth | `GET /login/oauth/authorize` consents at once for the lab user its `login` parameter names and requires PKCE (`S256`); `POST /login/oauth/access_token` answers access tokens valid eight hours and refresh tokens that **rotate**: each redeems once, a second redemption answers `bad_refresh_token`. `DELETE /api/v3/applications/{client_id}/grant` revokes every token of the person, `/token` the one access token (Basic auth with the client id and secret). `GET /user`. |
| git | Smart HTTP at `https://<fake>/<owner>/<repo>[.git]`, served by git's own `upload-pack` and `receive-pack` in stateless RPC mode (what `git http-backend` runs; protocol v0 to v2): clone, fetch and push, the token as the Basic auth password. A private repository answers 401 without a credential and "not found" to a user who does not read it; a push needs write, 401 without a credential, 403 naming the user otherwise. A push moves `pushed_at`. |
| Pull requests | `POST` and `GET /repos/{o}/{r}/pulls`, and the GraphQL operations of `gh pr create` on `/api/graphql`; `GET /api/v3/meta` reports a GitHub Enterprise version, as gh expects. |

A credential the fake did not issue (the sandbox's placeholder that the egress
gateway did not replace, or a JWT such as a Session Actor's) and one it issued
that expired or was revoked answer GitHub's 401 `Bad credentials` wherever
they are presented.

### The request log

Every request is a line of the container's log and an entry of
`GET /_fake/requests`: time, host, method, path (as it came, at the root on
an API host), status, the user (a login, or `installation/<id>`) and the kind
of credential: `none`, `placeholder`, `revoked-token`, `app-jwt`,
`installation-token`, `user-token`, `oauth-client`, or `jwt`, a JWT the fake
did not issue, with its `aud` and `sub` claims. Only the path and those claims
are logged: no token, OAuth code or verifier reaches it. `GET /_fake/pulls` lists every pull request with its author.
`PUT /_fake/repos/{owner}/{repo}` with `generatedMiB` adds a public repository
of that many generated MiB and a Go module to a fixture owner (the
measurements' `--large-repo-mib`), `DELETE` removes it again; the fixture's
repositories are never removed.

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
