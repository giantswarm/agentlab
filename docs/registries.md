# The lab's registries

agentlab runs two kinds of [zot](https://zotregistry.dev) container next to
the kind nodes. Both are on the `kind` docker network:

| Container | One per | What it is for |
|---|---|---|
| `<clusterName>-registry` | lab | The **lab registry**: what the lab pushes and the cluster pulls back. The `harness` and coding Harness dev images, the vm-manager guest image, and a chart directory's connectivity chart. |
| `agentlab-mirror` | machine | The **registry mirror**: a pull-through cache of `gsoci.azurecr.io` that every lab's nodes pull through. |

Both run the zot release the run resolves (see
[zot follows its releases](#zot-follows-its-releases)), with a configuration
agentlab writes and mounts read-only, and their storage in a named volume of
the container's name. A recreated container keeps what it holds.

## The lab registry

The lab registry is published on the host's loopback at
`platform.devRegistryPort` (5001 for the first lab). The host pushes to
`localhost:<port>`, and pods and atelet reach it as
`<clusterName>-registry:5000`. [Dev images](platform.md#dev-images) and
[An unreleased component chart](platform.md#an-unreleased-component-chart)
describe what goes into it.

Every `up` and `platform` run ensures it. A missing registry is created, a
stopped one started, and a paused one unpaused. One whose zot release or
configuration differs from what the run resolved and wrote is recreated on
the same volume.

- **Its configuration** is written to the lab's `state/registry/config.json`.
  It is zot's shipped configuration without the extensions: no search, so no
  CVE scanning and none of the Trivy databases zot downloads every two hours
  (about 0.35 GiB a day per lab). Docker's manifest types are accepted
  (`compat: docker2s2`), log level `info`.
- **Its storage** is the volume `<clusterName>-registry`. `agentlab down`
  removes the container and the volume.
- **The one-time move.** A registry created by an agentlab before this one
  keeps its storage in its own container layer. On its first run, agentlab
  copies that storage into the volume: the old container is stopped and
  renamed `<clusterName>-registry-moving`, then a `docker cp` stream fills
  the new container before it starts. Then the old container is removed, so
  what was pushed by hand stays. If the move fails, the old container is put
  back as it was and the run fails, naming the step. If a move was
  interrupted, the next run starts it again from the `-moving` container.

## The registry mirror

Each node's containerd pulls `gsoci.azurecr.io` through the mirror. The
mirror is listed first in the node's
`/etc/containerd/certs.d/gsoci.azurecr.io/hosts.toml`, and gsoci itself is the
fallback:

```toml
server = "https://gsoci.azurecr.io"

[host."http://agentlab-mirror:5000"]
  capabilities = ["pull", "resolve"]
```

The mirror fetches an image from gsoci the first time a node asks for it. It
fetches only the platforms that node asks for, and streams the image to the
node while it downloads. After that, every node of every lab on the machine
gets the image from the mirror. The second lab's pull goes over the docker
bridge instead of the uplink.

containerd reads `hosts.toml` on every pull and tries the next host on any
error from the mirror: a connection refused, a DNS miss, a 404 or a 5xx. A
mirror that is missing, stopped or failing costs one failed request, and the
pull goes on from gsoci. That is why the mirror never fails a run. If agentlab
cannot create it, the run warns and the nodes pull from gsoci directly.

`registryMirror` in `agentlab.yaml` is on by default. It is a node setting,
like `substrateNodes`, not under `platform`. Every `up` and `platform` run
applies it to every node of the cluster (the Substrate workers too):

- **On:** the run ensures the mirror like the lab registry (created, started,
  unpaused, or recreated on a new release or configuration, the volume kept),
  writes `hosts.toml` into each node, and notes whether the control-plane node
  reaches the mirror. On `up` this happens right after the cluster is
  created, before Dex or anything else pulls.
- **Off** (`agentlab configure --registry-mirror=false`): the run removes
  `hosts.toml` from the nodes, which then pull from gsoci directly. The
  mirror stays for the other labs.

Neither way needs a recreate. The file lives in the node container's own
layer, so it survives a node restart and goes with the node.

**The mirror's configuration** is written to
`~/.local/state/agentlab/mirror/config.json`, beside the labs
(`$XDG_STATE_HOME`). It is zot's sync extension, on demand for
`gsoci.azurecr.io/giantswarm/**`:

- `onDemand` without `pollInterval`: zot fetches only what a node asks for
  and never crawls gsoci. `manifestCheckInterval` is unset, so every tag
  request is checked against gsoci and a tag cannot go stale. A request by
  digest is served from the cache.
- `stream`: zot answers a manifest at once and serves blob reads from the
  download in flight, shared by every client asking for the same image.
  Without it, the first pull of a large image would wait for zot to copy all
  of it, and CRI cancels a pull that reads nothing for five minutes.
- `writeTimeout: 30m`: zot's default of 60 s is a deadline per response and
  would cut a streamed blob off on a slow link.
- `compat: [docker2s2]`: some gsoci images are Docker manifest lists, which
  zot's sync refuses without it.
- `accessControl`: read-only for anonymous clients. Pods can reach containers
  on the `kind` network by name, and nothing there may push a tag into a
  repository every node trusts.
- Retention: a tag not pulled for 14 days (`keepTags: pulledWithin 336h`)
  is removed, after a 24-hour delay, and GC removes what only it used. zot
  has no size cap, so `agentlab status` shows the cache's size.
- No search extension: no CVE scanning and no Trivy downloads.

**Removing it:** `agentlab down` leaves the mirror and its cache, because the
other labs use them. `agentlab down --mirror` removes the lab and then the
mirror container and its volume. Other labs keep their `hosts.toml` and pull
from gsoci until their next `up` or `platform` creates the mirror again.

**What the mirror does not cover:**

- **The host's preload.** `up` and `platform` pull a boot's images into the
  host docker cache and side-load them into the node (`preload.go`). The host
  cache is shared by every lab already, but a new tag is still pulled from
  gsoci on the host. docker's `registry-mirrors` setting applies to Docker Hub
  only.
- **atelet's Harness images.** atelet pulls them into its own layer cache,
  not through containerd.
- **Flux's chart pulls.** source-controller fetches the charts' OCI
  artifacts itself.
- **The routing to gsoci.** The mirror reaches gsoci the way the nodes do,
  through docker's DNS and the host's resolver and routes. A host whose path
  to gsoci stalls in bursts still stalls, but once per image instead of once
  per lab.
- **Recreating it.** A lab that recreates the mirror for a new zot release
  interrupts other labs' pulls in flight, which fall back to gsoci.

## zot follows its releases

Neither container pins zot. `up` and `platform` resolve zot's release once
per run and use it for both containers:

- **The pick** is the highest `v2.x.y` release among the tags of
  `gsoci.azurecr.io/giantswarm/zot`. gsoci also carries prereleases and test
  builds (`v2.0.2-rc1`, `v2.1.11-test`), which are skipped.
- **The floor**, v2.1.23, is the oldest release with what the mirror's
  configuration uses: sparse multi-arch sync (v2.1.22) and streaming
  (v2.1.23). It is a minimum, not a pin. It moves only when agentlab's zot
  configurations start using something newer. A newer major waits for a
  deliberate change, since it may change the configuration format.
- **Offline:** when gsoci's tags cannot be listed, a container that exists
  keeps its release and the run says so. A container that has to be created
  takes the newest release at or above the floor in the host docker cache.
  With none there, the run fails, as the pull would.
- **Reporting:** when the run creates or upgrades a container, it names the
  release (`recreated the lab registry agentlab-registry on zot v2.1.23
  (zot v2.1.21 → v2.1.23, agentlab's configuration, its storage into the
  volume agentlab-registry), its volume kept`).

### Holding the lab registry back

Every new zot release reaches every lab on its next run, untested. A broken
mirror falls back to gsoci, but the lab registry has no fallback. If a
release breaks the dev-image swap, hold the lab registry at a release that
works:

```bash
agentlab configure --zot-version v2.1.23
agentlab platform
```

`zotVersion` in `agentlab.yaml` takes a `v2` release tag at or above the
floor. `agentlab configure --zot-version ""` follows zot's releases again.
The shared mirror always follows the newest release, whatever a lab holds. Two
labs holding it at different releases would recreate it in turn, and gsoci
covers a broken one.

## Status

`agentlab status` names both containers:

```text
    registry    agentlab-registry running (zot v2.1.23)
    mirror      agentlab-mirror running (zot v2.1.23, 1.83GB cached)
```

The mirror's line also says when this lab is off it, and what brings a
missing or stopped mirror back. In JSON these are `registry`,
`registryRelease` and `mirror` (`on`, `state`, `release`, `size`). The size
comes from `docker system df -v`, which sizes every volume on the host and
can take a few seconds on a busy one.

To see what the mirror did, read its log: `docker logs agentlab-mirror`. A
`syncing image` line is a download from gsoci. A pull it served from its
cache has none: a request by tag logs only `successfully synced image` after
its check against gsoci.
