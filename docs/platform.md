# The agent platform (muster + Kubernetes MCP)

The lab's centerpiece: Giant Swarm's
[agent-platform](https://github.com/giantswarm/agent-platform) chart — the
same chart every Giant Swarm management cluster runs — wired to the lab Dex,
so Claude Code can drive a Kubernetes MCP server living inside the kind
cluster. The chart is a meta-package: it renders one Flux `OCIRepository` +
`HelmRelease` per component (muster, valkey, agentgateway, the MCP
registrations, kagent, agent-manager, model-manager, mcp-kubernetes,
Backstage, and the connectivity chart that wires them together), and its
bundled Flux engine turns those into running workloads.

The lab installs it in its **lab shape**:

- **The bundled engine is on** (`components.flux.enabled: true`, the chart's
  default): the chart brings the Flux Operator and one `FluxInstance` running
  source-controller + helm-controller under the multi-tenancy lockdown, plus
  the tenant identity `agent-platform-flux` every platform `HelmRelease` runs
  as. Nothing else in the lab installs Flux — the same engine also delivers
  the agents the Backstage create flow and agent-manager write as
  `HelmRelease`s.
- **Self-management is off** (`gitops.self.enabled: false`). With the engine
  on, the chart would by default render its own `OCIRepository` +
  `HelmRelease`, adopt the release and follow the *published* chart's version
  range — and refuse every later `helm upgrade` from the CLI. The lab exists
  to install what is **not** released yet (a local chart checkout, a dev
  image), which that `HelmRelease` would replace with whatever the registry
  holds. So Helm keeps owning the release: `agentlab platform` is one
  idempotent upgrade-or-install with the kstatus wait through the **embedded
  Helm** — Helm 4's SDK in the binary, no `helm` on the machine required; the
  same `helm upgrade --install … --wait`, no post-renderer, no
  `--force-conflicts` — and the release it writes is a regular one, so `helm
  upgrade` from a shell (`KUBECONFIG=state/kubeconfig`) stays the day-2 tool.
  A re-run that would install the same chart version with the same values
  writes no revision (`chart agent-platform <version> already installed with
  these values — nothing to do`) and only re-reads every component's health;
  a changed value, version or a chart directory upgrades.
  Never drop the value on a lab: the first upgrade without it makes the
  release self-managed.
- The chart is **pinned** to an exact release, `platform.chartVersion` in
  `agentlab.yaml` (the default is the release this agentlab was verified
  with). The lab never floats; bump the pin deliberately, with a lab run.
  The one exception is deliberate too: the [dev channel](#dev-channel),
  where `platform.chartBranch` follows a branch's newest dev build — and
  still installs an exact version, written into `chartVersion`.
- A **released 3.x meta chart** (`chartVersion` below `4.0.0`, no
  `chartBranch`, no `chartPath`) renders the **3.x lab shape**: that line's
  root schema is closed and the kagent 0.10 wrapper it resolves refuses the
  current line's keys, so the lab values carry no platform Postgres
  (`postgres`, `components.cloudnative-pg`), no `substrate`, and a `kagent`
  block without `harness`, `database`, the JWT policy on the controller route
  or the CNPG DSN mount — kagent 0.10 on its bundled Postgres, the controller
  in its local-dev auth mode, the controller ServiceMonitor following
  observability. This is what a migration rehearsal seeds before upgrading
  in place to the 4.x line; the switch is `config.LegacyChart`. The
  rehearsal — the four fleet shapes, the in-place upgrade, the migrate Job's
  three phases, the timings — is [The migration rehearsal](migration-rehearsal.md). The dev
  channel and a chart directory always render the current line's shape,
  whatever version they carry.

The platform installs as part of `agentlab up` (it is enabled in the default
configuration); on an already-running cluster the steps are also standalone:

```bash
./agentlab platform       # upgrade-or-install of the chart (embedded Helm), then waits for every component
./agentlab platform-test  # headless proof of the whole chain
```

The lab's patches on the component charts — `hostNetwork` on muster and
Backstage, the `dex-localhost` sidecar on the MCP servers, the kagent UI
NodePort, and the dev images below — are per-component **`postRenderers`
values** (`components.<name>.postRenderers` in
`state/agent-platform-values.yaml`): Kustomize patches the chart forwards to
that component's `HelmRelease` and the bundled helm-controller applies over
the component chart's render — the same mechanism the fleet has for chart
fixes.

## Installing an unreleased chart

Point `platform.chartPath` at a local checkout's chart directory and re-run
`agentlab platform` — nothing needs a release or a push:

```yaml
platform:
  chartPath: /path/to/agent-platform/helm/agent-platform
```

(or `agentlab configure --defaults --chart-path /path/to/agent-platform/helm/agent-platform`;
`--chart-path ""` clears it). `chartVersion` is ignored while it is set, and
the boot says which chart it installed. The directory is read, never written.

## Dev images

To run a component from a build of your own, build the image, and name it
under `platform.devImages`. The targets are the chart's component names for
the Deployments the lab can swap — `muster`, `backstage`, `kagent` (the
controller), `mcp-kubernetes`, `model-manager`, `agent-manager` — and
`harness`, the platform Harness's runtime image (the Go ADK every agent runs
on under kagent API v2):

```yaml
platform:
  devImages:
    muster: muster:dev-1a2b3c
    backstage: backstage-dev:my-feature-4d5e6f
    kagent: kagent-controller:dev-7e8f9a      # the line's controller, built from go/ of giantswarm/kagent-upstream
    harness: golang-adk:dev-7e8f9a            # the Go ADK, same checkout, BUILD_PACKAGE=adk/cmd/main.go
```

Either way the swap is part of the release: `agentlab platform` renders it
into the values it installs, so the same plain `helm upgrade` applies it, and
removing the entry restores the chart's image on the next run. Use a
distinctive tag per build — kind's containerd keeps running the old bits
under a reused tag, and a Harness digest that did not change recompiles
nothing.

### Deployment targets

`agentlab platform` side-loads the image from the host docker cache into the
node, checks the node lists it before anything installs (a missing image
fails right there with its fix, instead of five minutes later as an
`ImagePullBackOff`, a helm-controller timeout and a rollback to the chart's
image), and renders it into that component's `postRenderers` as a Kustomize
image override with `imagePullPolicy: IfNotPresent`; `kubectl get deploy -o
jsonpath` shows the new image. Make it a build of its own, not a re-tag of a
registry image the kubelet has pulled (a chart that pulls `Always`, like
Backstage's, makes the kubelet pull): since Kubernetes 1.33 the kubelet
remembers which image IDs it pulled and re-pulls a pod's ref that maps to one
of them (`KubeletEnsureSecretPulledImages`), so such a re-tag ends in
`ImagePullBackOff` against Docker Hub while a real build — a new image ID,
side-loaded, never pulled by the kubelet — is used as is. (A `kubectl patch`
on the Deployment still works for a quick look, but only until
helm-controller's next release of that component overwrites it — the values
are the durable path.)

The image name the override replaces is read off the component chart's
render, the one the boot renders anyway to side-load the platform images —
not from a table, because it differs between lines: the `kagent` target
replaces `gsoci.azurecr.io/giantswarm/kagent-controller` under the 3.x meta
chart (the wrapper chart) and `ghcr.io/giantswarm/kagent/controller` under
4.x (the kagent line's own chart), whatever Deployment `kagent-controller`'s
`controller` container names. A Kustomize image override whose name is in no
rendered object matches nothing, and kustomize drops it without a word — so
a target whose chart rendered without the Deployment and container the lab
patches is **refused before the install**, naming both (`platform.devImages.kagent:
the kagent chart's render has no Deployment kagent-controller with a container
controller …`); a chart that did not render at all (the preload reports why)
falls back to the table's name with a note that it is unverified.

### The `harness` target

Under kagent API v2 an agent runs on the platform Harness's workload image,
not on a Deployment the lab could patch: the connectivity chart renders the
image **by digest** into the `Harness` object `kagent` (the CRD accepts no
tag), and Substrate's atelet fetches it from a registry into its own layer
cache — the actors' overlay lowerdirs — never through the node's containerd,
so a side-load is invisible to it. The lab therefore runs a **registry**
for this target: a `registry` container named `<clusterName>-registry` on
the kind docker network, published on the host's loopback
(`platform.devRegistryPort`, default 5001, kind's documented local-registry
port; created on demand, removed by `agentlab down`). `agentlab platform`
tags the build into it (`localhost:<port>/<path of your ref>:<tag>`), pushes,
reads the manifest digest the registry computed, and forwards
`kagent.harness.image: localhost:<port>/<path>@sha256:<digest>` through the
meta chart to the connectivity chart. atelet's side of the pattern,
`--localhost-registry-replacement=<clusterName>-registry:5000` (a `localhost`
registry in an image ref is rewritten to that endpoint and pulled over plain
HTTP), is part of the lab shape whenever the agents are on — through the
meta chart's `substrate.atelet.extraArgs`, forwarded to its substrate
component — not only while a dev image is configured: the flag is inert for every other
ref, and having it in place before a swap means the Harness's new digest
(the connectivity release) can never race ahead of the atelet roll (the
substrate release) that would carry it. The boot then asserts the Harness
pins the dev digest, and — because a **digest-pinned Harness recompiles
every template it admits** (a new golden snapshot per revision) — lists the
admitted templates as they come back Ready on it
(`Harness kagent runs localhost:5001/golang-adk@sha256:…; 1 admitted templates
recompiled on it: my-agent 88c11e… -> ebfce6…`). Removing the entry restores
the chart's digest on the next run — again one `helm upgrade`, again a
recompile. A `platform.valuesFiles` overlay that sets `kagent.harness.image`
itself (the way a lab pins a published build) or replaces
`substrate.atelet.extraArgs` (lists replace in a Helm merge) is refused while
the target is configured.

Because atelet keys its cache by digest, the kubelet re-pull trap above does
not apply here, and a pushed image is pulled once per node. The mechanism
needs the platform Harness, so the target is for the 4.x meta chart with
the agents on; the 3.x line has no Harness.

muster runs with `hostNetwork`, so it binds `:8090` on the node, and the
rendered kind config publishes that onto the Mac (host port `platform.musterPort`,
default 8090) — no port-forward. Port mappings are fixed at node-creation time,
so changing the port means `agentlab down && agentlab up`; the stopgap on an old
cluster is `kubectl -n agent-platform port-forward svc/muster 8090:8090`.

## Dev channel

The stable channel is the pin above: `platform.chartVersion`, an exact
release. The **dev channel** follows a branch of agent-platform instead —
the dev builds gitsemver publishes for every commit of a branch that has
branch publishing on (`gen.ci.branchPublish` in giantswarm/github): charts
tagged `X.Y.Z-dev.<branch>.<YYYY-MM-DD>.<HH-MM-SS>.h<sha>` next to the
releases in `oci://gsoci.azurecr.io/charts/giantswarm/agent-platform`, each
coupled to the images its commit built. That is how a chart change is
verified before it is released: every lab installs the branch's newest build
from published artifacts only — no checkout, no dev images, no local
registry.

```bash
agentlab configure --defaults --chart-branch my-feature
export ANTHROPIC_API_KEY=sk-ant-...
agentlab up
agentlab platform-test && agentlab test && agentlab backstage-test
```

`configure` resolves the branch right away and says what it picked:

```
  chart      agent-platform 4.7.12-dev.my-feature.2026-09-11.08-12-33.h7f841be (branch my-feature, dev channel)
```

and `agentlab.yaml` carries both:

```yaml
platform:
  chartBranch: my-feature   # the dev channel: chartVersion follows this branch's newest dev build
  chartVersion: 4.7.12-dev.my-feature.2026-09-11.08-12-33.h7f841be   # written by the resolver
```

How the resolution works: the branch is spelled the way gitsemver embeds it
(lowercased, anything outside `[a-z0-9]` collapsed to one hyphen —
`my/feature` is `my-feature`; a long name is shortened around a
`--` marker, which the filter accepts too), the registry's tags are listed
through the embedded Helm's registry client (anonymously, as pulls are), the
tags whose prerelease is `dev.<that name>.…` are the branch's builds, and the
highest semver among them is the newest — the pick a Flux `OCIRepository`
with `semver: "*-*"` and a `semverFilter` on the branch makes. `up` and
`platform` re-resolve on every run, so the lab follows the branch like Flux
would: a newer build is a new revision of the release on the next
`agentlab platform`, the build the lab already runs is a no-op (`chart
agent-platform <tag> already installed with these values — nothing to do`).
Everything downstream — `render`, the image preload,
the install, `helm -n agent-platform history` — sees the exact version
written to `chartVersion`, as on the stable channel; `render` and the
proofs never resolve.

- **Freezing a build**: `agentlab platform --pin` writes
  `platform.chartPinned: true` and stops re-resolving, so the lab keeps the
  build under test; `--pin=false` follows the branch again. A new
  `--chart-branch` (or `""`, back to the stable channel) always starts
  unpinned.
- **No build yet**: a branch without dev builds is refused with the two ways
  out — the branch's publish has not run (agent-platform needs
  `gen.ci.branchPublish` and a commit on the branch), or pin a tag by hand.
- **The fallback that needs no resolver**: `platform.chartVersion` accepts a
  full dev tag as it is — `agentlab configure --chart-version
  4.7.12-dev.my-feature.2026-09-11.08-12-33.h7f841be` validates and
  Helm pulls that exact tag (look it up with `crane ls
  gsoci.azurecr.io/charts/giantswarm/agent-platform | grep -- -dev.my-feature`).
  The lab then does not follow the branch.
- `chartBranch` and `chartPath` are mutually exclusive: a local chart has no
  builds to follow.
- The dev-tag schema lives in one place, `devTagFilter` in
  `internal/lab/chartbranch.go`. When gitsemver ships the RFC's successor
  schema (`X.Y.Z-b<crc32 of the branch>t<timestamp>c<sha>`), that function
  switches and nothing else changes.

Component channels: agentlab sets nothing per component. A meta chart's dev
build may carry a sibling's channel in its own values (the branch's
`components.<name>.semverFilter`), so selecting the meta chart's dev channel
selects the whole line. The image preload follows the same filters: a
component whose `OCIRepository` carries a `semverFilter` is rendered at the
tag Flux will pull (the filter's regexp over the repository's tags, within
the range, the highest) rather than at Helm's own resolution of the range,
which knows no filter and would land on the stable release; the boot log
names the picks (`rendered 12 of 12 component charts (6 through a
semverFilter: agent-platform-connectivity 3.22.1-dev…, …)`).

### Agent Substrate and the platform Postgres — from the chart

kagent API v2 (the 4.x line: `kagent.dev/v1alpha3`, kagent `0.11.0-gs.N`
from the Giant Swarm kagent line) runs every agent as an actor on
[Agent Substrate](https://github.com/kagent-dev/substrate) — sandboxed
(gVisor) worker pods of a `WorkerPool`, an API server, a per-node agent
(`atelet`) and the actors' ingress/egress data plane (`atenet`) — from the
Giant Swarm line [giantswarm/substrate](https://github.com/giantswarm/substrate)
(its `FORK.md` records the pin, the carried patches and the published
versions). **The chart ships it**: `components.substrate-crds` and
`components.substrate` follow `components.kagent`, both land in `ate-system`
as component releases of the chart's engine at the version the chart pins
(`>=0.0.27-gs.5 <0.0.28-0`, the range the `WorkerPool`'s worker image
`kagent.substrateWorkerPool.workerImage` names too), the connectivity
release's `pre-install,pre-upgrade` hook Job mints what the substrate chart
mounts but does not render (the CA/JWT pools, the actor-identity trust
anchor, ate-api-server's authentication config; a pool that exists is never
touched), the kagent chart creates the `WorkerPool kagent-default` and the
connectivity chart renders the one platform `Harness kagent` (the Go ADK
image by digest, `KAGENT_PROPAGATE_TOKEN`, the WorkerPool, the snapshot
location; admission by the label `agent-platform.giantswarm.io/harness:
kagent`). The lab installs **nothing** of it — one Helm owner, the objects an
installation has (the [agent-platform README](https://github.com/giantswarm/agent-platform#agent-substrate)).
What the lab brings is what the chart cannot: the kind cluster carries the
apiserver gates Substrate needs (`ClusterTrustBundle`,
`ClusterTrustBundleProjection`, `PodCertificateRequest`,
`certificates.k8s.io/v1beta1` — `kind-config.yaml.tmpl`, fixed at `kind
create`), and `agentlab up`/`platform` refuse a cluster that lacks them
before the install, whenever the chart's rendered roster carries the
`substrate` release (`agentlab down && agentlab up` is the fix). A lab that
ran the retired POC channel, where agentlab installed Substrate itself,
upgrades in place: the chart's `substrate` component adopts the
`substrate`/`substrate-crds` releases in `ate-system` by name.

The lab's Substrate values are two: the actor snapshot store is the chart's
bundled in-cluster RustFS (`substrate.rustfs.enabled: true`,
`kagent.harness.snapshotLocation: s3://ate-snapshots/kagent` — an
installation names its own bucket), and the control-plane database follows
the platform Postgres (the chart's `substrate.postgres.enabled: auto`).

**The platform Postgres** is the fleet's shape too: `components.cloudnative-pg`
(the upstream CloudNativePG operator, the chart's optional component — a
management cluster runs it as its own app) and `postgres.enabled` render one
CNPG `Cluster kagent-pg` in the kagent namespace with kagent's `kagent_v2`
database (`Database kagent-pg-kagent-v2`, the `vector` extension for the
memory API) and Substrate's database (`Database kagent-pg-substrate`) on it;
the connectivity release's `post-install,post-upgrade` hook derives one
connection Secret per database from CNPG's `kagent-pg-app` —
`kagent-pg-kagent-v2-app` in `kagent`, mounted by the controller as
`database.postgres.urlFile`, and `kagent-pg-substrate-app` copied into
`ate-system` for ate-api-server. Sized for one kind node: one instance, a
2Gi claim on the local-path storage class, the platform's Postgres image
(`gsoci.azurecr.io/giantswarm/postgresql-cnpg:18.3`) with pgvector as a CNPG
ImageVolume extension (`gsoci.azurecr.io/giantswarm/pgvector`, the kind
node's kubelet serves image volumes) — the fleet runs three instances on
20Gi with backups. Both bundled databases (kagent's, Substrate's) stay off.

**Authentication on the controller route** is the fleet's too: the kagent
controller's gRPC API is a `GRPCRoute` on the edge Gateway with the chart's
JWT policy (`kagent.controllerRoute.jwtAuthentication`, mode `Strict`)
verifying every bearer against the lab Dex (its JWKS over TLS at
`dex.dex.svc.cluster.local:5556/dex/keys`) and setting `x-user-id` from the
verified email claim — replacing whatever the client sent — and the
controller runs `auth.mode: trusted-proxy`, re-deriving the caller from the
same bearer. `agentlab platform-test` proves both halves: a call without a
token is refused at the edge, a call with a valid token and a forged
`x-user-id` is attributed to the token's subject by the controller, and the
same forged header changes nothing at muster and agent-manager, which read
the bearer alone.

**Proofs on the 4.x line.** `platform-test`, `test` and the sign-in half of
`backstage-test` are the platform's (platform-test expects a kagent scrape
target only while a controller ServiceMonitor exists — the kagent line's
controller serves no metrics listener, and the lab renders none for it). The
agent proofs — `agents-test`, `toolsets-test`, `models-test`'s agent turn,
`backstage-test`'s agents pages, `skills-test`, `a2a-test` — drive kagent API
v2: every
agent a HelmRelease of the Generic agent chart (1.x) whose render is an
`AgentTemplate` admitted by the platform `Harness` and run as a Substrate
actor, a turn an `AgentInstance` driven over native gRPC through the edge as
the signed-in user (`a2a-test` asserts that path as the surfaces drive it), the
toolset on the agent's own `RemoteMCPServer` the template binds; see
[Agents](agents.md).

### The skills proof (the golden boot)

`agentlab skills-test` asks the question every skill-carrying agent of the
fleet hangs on: does a Go ADK `AgentTemplate` with a git skill boot under
Substrate? The controller compiles a template into an ActorTemplate;
Substrate boots one actor from it — the golden boot — waits for its readyz
and takes the golden snapshot every turn resumes from; the Go ADK
materialises the template's skills when it starts (a git skill is
`git fetch --depth 1 origin <commit>` of a full commit into `/plugins`,
copied to `/skills`), before it serves readyz; and atenet refuses outbound
connections from an actor that is not `RUNNING`. The proof creates an
`AgentTemplate` on the platform's Go ADK Harness (`kagent`) — labelled as
that Harness's `allowedAgentTemplates` selector admits, read from the
Harness itself — with one skill
pinned to a full commit of a public repository — `agent-self-awareness` of
giantswarm/agent-skills, the repository most of the fleet's skills come
from — and the shared muster server as its tools, waits for `Ready` on the
Harness (`--ready-timeout`, 10 min by default) and, on success, drives one
turn through the edge as the signed-in user that names the skill and
answers a fact only its `SKILL.md` has (the Harness re-emits the person's
bearer on tool calls, `KAGENT_PROPAGATE_TOKEN`, which the proof asserts).
On a failed boot it prints the evidence — the Harness's conditions and
warnings; Substrate's ActorTemplate, actor and pinned worker as the
controller's `GetSubstrateStatus` reports them; the controller's, atenet's
(every container) and the pool's worker pods' log lines about the
template's actors — boots the same template without the skill as the
control, and exits non-zero. Either way it leaves nothing behind: the
templates go, Substrate lets go of their ActorTemplates and actors through
kagent's revision garbage collector, and a worker still pinned to an actor
of a deleted template is freed the documented way (its pod deleted, the
WorkerPool replaces it). A negative outcome is a finding about the line,
not about the lab: the proof stays red until the line carries a fix, and
is that fix's acceptance test.

Another fixture takes the public one's place: `--skill-repo`,
`--skill-commit`, `--skill-path` (the skill's directory within the
repository, its last element the skill's name), `--skill-question` and
`--skill-expect` (the answer only the skill's text has, matched
case-insensitively in the reply together with the skill's name). A private
repository adds `--skill-secret`: a Secret in the kagent namespace whose
`token` key holds a read token for the repository's host, rendered as the
source's `skills[].source.git.credentialRef` (`{name, key: token}`) — the
proof refuses to run it against a kagent whose served `AgentTemplate` CRD
lacks the field, since that kagent would prune it and fetch anonymously. The
Secret is yours to create; the proof never reads its value. A Secret that
does not exist shows as `ResolvedRefs=False` on the Harness before any
actor boots; a wrong token as a failed golden boot whose worker log carries
git's authentication error.

**Outcome (2026-09-11): positive — once every gate on the actor's egress
admits a resuming actor.** Three gates stood between a booting actor and
the network, found by reading the line's code and by the proof's evidence:

1. **ateom** (the worker runtime, `cmd/ateom-gvisor`): the actor
   certificate was minted before the workload started, but atunnel's
   egress was armed only after readyz, together with ingress — until then
   an intercepted connection was closed without a word, which is the
   `Send failure: Broken pipe` of the first runs (measured 2026-09-10 on
   Substrate `0.0.27-dev.giantswarm.2026-09-10.19-33-37.h734ec53`: the
   template stayed `Ready=False ActorTemplatePending` for the whole
   timeout, one worker pinned, nothing logged at any hop; the control
   without the skill Ready in 10 s).
2. **atenet's egress `ext_proc` handler** refused any actor that is not
   `RUNNING`, a state Substrate commits only after readyz.
3. **the egress dataplane itself**: the agentgateway build the Substrate
   chart deploys (`ghcr.io/kagent-dev/substrate/agentgateway:c0f5597c7cb8`,
   a pre-merge build of upstream agentgateway #3237) authorizes every
   CONNECT against ate-api by itself — actor UID, then `RUNNING` — and is
   the check in the request path (the chart's egress config carries the
   `substrateEgress` policy and no `ext_proc`).

The Substrate line carries the fix for the first two (giantswarm/substrate#4,
in `0.0.27-dev.giantswarm.2026-09-10.22-37-39.h1817627`: egress armed before
the first container starts, `RESUMING` admitted next to `RUNNING`, both hops
log a refusal). Under it the proof's evidence changed shape — the worker logs
`atunnel failed to open egress tunnel … egress gateway rejected CONNECT with
403 Forbidden: actor is not running` and ate-api logs the dataplane's
`GetActor` — and the third gate remained: the dataplane's `RUNNING` check.
With an egress dataplane that does not gate on `RUNNING` — upstream
agentgateway `v1.5.0`, whose `substrateEgress` derives the actor from the
SPIFFE id and does no UID or state check, swapped into `atenet-egress` for
the run — the proof passed on both halves on 2026-09-11 (`Ready after 20s`,
the turn answered `Skills: agent-self-awareness … klaus-gateway`), which
proved the Substrate half but gave the authorization up.

The agentgateway line closes the third gate without giving it up: its release
`v1.5.1-gs.2` (giantswarm/agentgateway-upstream#4) carries upstream
agentgateway #3237 — every egress CONNECT authorized against ate-api, the
actor's UID and then its state — and admits `RESUMING` next to `RUNNING`
(SUSPENDED, PAUSED, CRASHED and DELETING stay refused). giantswarm/substrate#7
pins it for `atenet-router` and `atenet-egress`, and giantswarm/substrate#9
declares the actor authorization where that dataplane reads it — #3237 made
it a frontend policy on the CONNECT, no longer a route policy on the inner
listener (with the old shape the gs.2 dataplane refused its config and the
first roll never became ready). The lab's Substrate pin follows that build
(`0.0.27-dev.giantswarm.2026-09-11.02-07-06.h213d76b`), and the WorkerPool's
worker image with it. On it the proof passed on both halves with the check in
place: Ready after 15s on Harness kagent, the turn as admin@lab.local answered "klaus-gateway" from the skill, nothing left behind (revision a8949e0f2c79); platform-test 6/6 and agents-test 5/5 green on the same lab. Every skill-carrying agent of the fleet needs all three
gates open; the upstream exits of the two patches are tracked on
giantswarm/giantswarm#37742 (row 8).

### The Swarmgeist proof (klaus-gateway on kagent API v2)

`agentlab klaus-gateway-test` is the lab carrier of
[klaus-gateway](https://github.com/giantswarm/klaus-gateway) (Swarmgeist,
the fleet's Slack bridge) on kagent API v2: A2A v1 over gRPC through the
agentgateway edge, the roster from `ListAgentTemplates`, one `AgentInstance`
per channel thread kept in the gateway's routing store, human-in-the-loop as
kagent's HITL extension, a stop as `CancelTask` — every call made as the
person behind the turn, whose Dex id_token the gateway forwards and validates
nowhere itself. Slack cannot be driven headlessly; the gateway's web channel
(`/web/*`) is the same facade one adapter down, so the proof drives that.

**Deployment shape.** The gateway runs **out of cluster, on the host** —
the released image on the host network by default
(`--gateway-image`, `gsoci.azurecr.io/giantswarm/klaus-gateway:1.0.2`), or a
local build (`--gateway-binary`, the proof of a branch) — with `a2a.url` =
the lab's public gRPC target `grpcs://agentgateway.<domain>:<gatewayPort>`
(TLS with the lab CA from `certs/ca.crt`; the JWT `Strict` policy of the
controller `GRPCRoute` validates the forwarded token at the edge),
`a2a.namespace: kagent`, a bolt store in a run directory (`--run-dir` keeps
it, together with the gateway's log), the web channel on `127.0.0.1:18090`
(`--gateway-port`; the admin endpoints take the next port), the static
lifecycle driver (no Klaus instances). This is the leg the meta chart's
in-cluster component (`components.klaus-gateway`, `klausGateway.a2a.url` =
the in-cluster `grpc://agentgateway.agent-platform.svc.cluster.local:8080`)
does not exercise: the public route with TLS and the JWT policy. The
component itself stays off in the lab; enabling it is a values change under
the lab lock and is not part of this proof. The gateway forwards the
person's token and talks to no Dex, so it needs no `dex-localhost` bridge.

**Fixtures** (in `kagent`, deleted by the same run, leftovers removed first):
`AgentTemplate agentlab-klaus-gateway-test` in the Generic chart 1.x shape —
the Harness's admission label read from the Harness itself, the
`ui.giantswarm.io/display-name` and `ui.giantswarm.io/icon-url` annotations,
`default-model-config` (`--model-config`), and its own muster carrier
`RemoteMCPServer agentlab-klaus-gateway-test` (muster's in-cluster URL,
`X-Muster-Toolset: preset:read-only`, discovery off, never an Authorization
header) bound with **`requireApproval: true`**, so every muster call pauses
for a decision (the Generic chart renders the same binding from
`muster.requireApproval` since agent 1.1.0; the proof applies the pair
directly so it depends on no chart release resolving in the lab); and
`AgentTemplate agentlab-klaus-gateway-test-unadmitted`, which carries no
admission label.

**Assertions** — the five of giantswarm/agentlab#146 and the attribution:

1. **Discovery**: `GET /web/agents` as the user lists the fixture with the
   display name and icon from its annotations; the unadmitted template is
   not listed, and a turn naming it (`agentRef`) is refused synchronously
   (HTTP 502) with the reason `no Harness admits this AgentTemplate`.
2. **One turn**: the thread's first message streams to `done`; the gateway's
   `instance_bound` record names the `AgentInstance`, and the controller
   (`ListAgentInstances` narrowed to the template, as the user) lists
   exactly that one. A cold worker's first resume may hit Substrate's
   ResumeActor deadline once; the first turn is retried once, visibly.
3. **HITL**: a tool-using question pauses on a `prompt` event (`filter_tools`,
   then `call_tool` on the same task); each pause is `TASK_STATE_INPUT_REQUIRED`
   at the controller (`GetTask`), each approve decision on the web channel
   carries the task id, the resumed task ends `TASK_STATE_COMPLETED` and
   `ListTasks` shows nothing of the instance left at input-required.
   **Attribution**: muster's log since the turn began carries the
   `forwarded_id_token_accepted` audit record with the user's email and
   `tools/call request` lines under the token's subject.
4. **Stop**: a long turn's stream is closed by the client after 6 s; the
   gateway cancels the task at the controller (`ListTasks` shows the new
   task `TASK_STATE_CANCELED`) and the thread takes a following turn.
5. **Restart**: the gateway is stopped and started again on the same bolt
   store; the next turn recalls the first turn's word, no new
   `instance_bound` record is written, and the controller still lists the
   one `AgentInstance`.

The controller reads go over gRPC-Web through the same edge
(`kagentapi.go`), as the portal's backend does. The thread key is
`web|agentlab||thread-<run>|agentlab-klaus-gateway-test`, the user the
signed-in email (`admin@lab.local` by default).

**The negative.** A gateway built without the transport — the `release-v0.x`
line, e.g. `--gateway-image gsoci.azurecr.io/giantswarm/klaus-gateway:0.39.4`
— must fail the proof at start-up or discovery: its `a2a.url` is an HTTP
base URL for kagent's retired JSON-RPC/REST surface and it cannot speak A2A
v1 over gRPC. Run it once when the gateway line changes; it is not a default
step. Not provable here: Slack itself (the `@`-mention, thread reply, `/agent`,
the Block Kit surface) — verified on the first installation that runs
Swarmgeist on klaus-gateway 1.x, per the agentlab-first exception.

## The request path

```
Claude Code ──https://muster.127.0.0.1.nip.io/mcp──> agentgateway ──> muster ──> mcp-kubernetes ──> kube-apiserver
                     │                                 (edge, TLS)     │
                     │ 401 + WWW-Authenticate                          │ OIDC discovery + token exchange
                     └──── browser ────────────────────────────────────┴──> https://localhost:32000/dex
```

muster is the OAuth **server** towards Claude Code (DCR, `/oauth/authorize`,
`/oauth/token`) and an OAuth **client** of Dex. The `muster` staticClient in
the rendered Dex config closes that loop; its `redirectURIs` must equal
`<oauth.server.baseUrl>/oauth/callback`.

`mcp-kubernetes` is deliberately unauthenticated on the cluster network
(the bundled `MCPServer` CR carries no auth block) and talks to the apiserver
with its own ServiceAccount. muster is the single enforcement point.

## Signing in to a downstream server (muster as OAuth client)

muster plays two OAuth roles. Towards Claude Code and Backstage it is the
**server** (above). Towards a downstream MCP server that declares
`auth.type: oauth` it is the **client**: `core_auth_login` (what the portal's
per-server **Sign in** button calls) answers a challenge — a
`https://muster.<domain>/oauth/proxy/start?state=…` URL — the browser follows
it through the downstream's authorization server, and muster keeps the token
per session. The chart leaves that role off (`oauth.mcpClient`), so the
lab's values turn it on with the edge URL as `publicUrl` (the chart derives
the callback `/oauth/proxy/callback` and serves muster's client ID metadata
document at `/.well-known/oauth-client.json`), the way real installations run
it.

Nothing the lab aggregates needs a per-user login, though — mcp-kubernetes,
mcp-prometheus and model-manager are unauthenticated in-cluster — so
`agentlab platform` ships one downstream that does: the MCPServer
`lab-oauth-fixture` (`oauth-fixture.yaml.tmpl`, annotated as a fixture). It
points muster at its **own** protected `/mcp` endpoint (a 401 with RFC 9728
metadata at zero cost), so the CR sits at `Auth Required` until a session
signs in and every `core_auth_login` yields a fresh challenge. The
authorization server the sign-in walks through is **pinned to the lab Dex**
(`spec.auth.authorizationServer`, the shape an operator uses for a GitHub App:
Dex's authorization and token endpoints, the client from the Secret
`lab-oauth-fixture-client`, the scopes — and, as the `issuer`, muster's own
public URL, the identity the grant is filed under: it must be the server the
endpoint's RFC 9728 metadata names, or no call finds the token) rather than
discovered. Discovery would name muster's own OAuth 2.1 server,
which identifies clients by Client ID Metadata Document — and that server's
SSRF guards refuse every lab hostname, for the metadata URL and for a
registered redirect URI alike (`muster.127.0.0.1.nip.io` resolves to the
edge's cluster IP in-cluster and to loopback outside; muster exposes no knob
for mcp-oauth's `AllowPrivateIPClientMetadata`, HACKS.md U18), so no sign-in
could ever complete there. Dex matches redirect URIs exactly: the platform
client lists muster's proxy callback `/oauth/proxy/callback` next to its
server-role callback (`dex.yaml.tmpl`), and the token Dex issues carries the
platform client's audience, which the endpoint trusts. Completing the sign-in
therefore connects muster to itself and surfaces its own tools under
`x_lab-oauth-fixture_` — harmless, per session, and exactly what the toolset
proof needs (a toolset naming the server resolves to its tools only for the
session that signed in). What the self-aggregation costs: right after a muster
restart the CR reads `Failed` (muster dials itself before its listener is up)
until the reconnect backoff flips it to `Auth Required`, about a minute —
`agentlab platform` waits for that.

`agentlab platform-test` proves the path headlessly on a fresh MCP session
(the shape of one portal user's session): `list_tools` flags the fixture under
`servers_requiring_auth`; `core_auth_login` answers a challenge on
`/oauth/proxy/start` with a state; a second call answers a **fresh** state (a
re-clicked Sign in gets its own challenge, giantswarm/backstage#2203); and the
URL redeems — GET-ing it redirects to the pinned authorization server (Dex,
client `agent-platform`) rather than rejecting the state. `agentlab
backstage-test` proves the portal hop for every user: the fixture is listed
`Auth Required` and `POST /api/muster/auth/login` answers `status:
auth_required` with the same URL shape, on that user's own forwarded token.
`agentlab toolsets-test` completes the sign-in headlessly (the Dex login form,
then muster's proxy callback) and proves what it unlocks — see
[Toolsets](#toolsets-declared-tool-access).

To drive the UI: **Agent Platform → MCP Servers**, expand `lab-oauth-fixture`,
**Sign in** — the popup lands on Dex directly. After a muster pod roll the row
reads `Failed` for about a minute (Reconnect, or wait).

## The fake fleet and the tool-group label

A real installation federates many management clusters: agent-platform-mcps
renders one MCPServer per cluster and per infrastructure family — `kubernetes`,
`capi`, `prometheus` — each declaring `spec.family` (so muster exposes the
family once, `x_kubernetes_<tool>` with a `management_cluster` argument) and
labelled `muster.giantswarm.io/management-cluster: <cluster>`. The lab has one
cluster and its bundled `mcp-kubernetes` declares no family, so nothing here
looks like a fleet — yet the portal's MCP servers page, its dashboard's fleet
coverage and the agent create flow's Tools step group servers by family and by
tier. For proofs of those views the lab can ship a **fake fleet**
(`fleet-fixture.yaml.tmpl`, `fleetfixture.go`): three families × two fake
management clusters (`lab-01`, `lab-02`), six MCPServers named
`<family>-<cluster>` in `agent-platform`, each with the family block, the
management-cluster label, the `muster.giantswarm.io/type` the fleet charts
stamp, and the tier label:

```yaml
agent-platform.giantswarm.io/tool-group: infrastructure
```

**Opt-in.** The fixture is off by default and follows one key in `agentlab.yaml`
(`configure` asks nothing about it):

```yaml
platform:
  fakeFleet: true    # default false: a default lab registers only its own MCP servers
```

A default lab lists only the MCP servers of the lab that runs it — the bundled
`mcp-kubernetes`, `mcp-prometheus`, the managers, all `Connected` through the
person's Dex session, plus `lab-oauth-fixture` as the one server requiring
sign-in. The fake fleet's members stay `Auth Required` by design (below), so
with it on the portal's Tool explorer greets a person with seven servers
asking for sign-in, three of them families named after clusters that do not
exist; that is a proof's shape, not a default lab's. Flip the key and re-run
`agentlab platform`: on, it creates the six members and waits for
`Auth Required`; off, it removes every MCPServer carrying
`agentlab.giantswarm.io/fixture=fake-fleet` in `agent-platform` (a lab created
while the key was on loses them; nothing to remove is silence). The proofs read
the same key: `platform-test`, `backstage-test` and `toolsets-test` assert the
fleet shape while it is on and the single-cluster shape while it is off (the
tool-group step and the servers page grouping below). The lasting answer — the
lab's own cluster registered as the infrastructure families, the way an
installation is, which retires the fixture — is agentlab#168.

**What the label is for.** It is the Agent Platform's tiering of MCP servers —
three groups the portal's MCP servers page, the Tools step, muster's toolset
presets and the docs all use under the same names: **Agent Platform**
(`agent-platform`: agent-manager, model-manager, muster's core tools),
**Infrastructure** (`infrastructure`: the kubernetes/capi/prometheus families)
and **Registered servers** (no label: whatever an installation or a user
registers, including everything the portal's Register server flow creates).
The chart that ships a server stamps it; every consumer only reads it, e.g.
`kubectl get mcpservers.muster.giantswarm.io -A -l agent-platform.giantswarm.io/tool-group=infrastructure`
lists the fleet families. Orientation and preset membership, not
authorization — that stays with the servers' OAuth and the clusters' RBAC.
Of the lab's own CRs only the fake fleet carries the label: `lab-oauth-fixture`
and anything you register by hand stay unlabelled on purpose, so the Registered
servers group has members; the vendored agent-manager / model-manager /
component charts bring their own labels (`agent-platform`; `infrastructure` on
the bundled `mcp-kubernetes`) once bumped to the releases that stamp them — on
a default lab (fake fleet off) those are the only labelled servers.

The members point at muster's own protected `/mcp` with `auth.type: oauth`,
like the OAuth fixture, and so read `Auth Required` — what an unconnected
forwardToken fleet member shows on a real installation — at zero cost.
Pointing them at the lab's single mcp-kubernetes instead makes muster open one
connection per member per user session; a dozen of those rate-limited
mcp-kubernetes (429) and took the session's real `mcp-kubernetes` connection
down with them. The fixture exists to be grouped, listed and selected, not
called. `agentlab platform-test` asserts the label: with the fake fleet on, the
`infrastructure` selector lists every member and, of the lab's own CRs,
nothing else; off, no member exists and no lab-created server carries the
label at all; either way every value in the cluster is one of the two the
contract knows and `lab-oauth-fixture` is unlabelled. Servers the vendored
charts label are reported, not judged.

## Toolsets (declared tool access)

A **toolset** is the selector list an agent declares — the `agent` chart's
`toolset` value on the agent's HelmRelease, rendered as the `X-Muster-Toolset`
header on the agent's own `RemoteMCPServer` (`spec.headersFrom`, the one its
`AgentTemplate` binds), agent-manager's `toolset` argument, the portal's
Tools step — that bounds which of the
gateway's tools the agent's meta-tools can see and call. Selectors:
`preset:<name>`, `server:<name>`, `workflow:<name>`, `tool:<name>`; built-in
presets `read-only`, `none`, `full`; the platform chart ships `infrastructure`
and `agent-platform`, defined by the tool-group label above. muster evaluates
the header **per request** on the caller's own catalogue, so the invoking
human's identity and the backends' authorization stay the boundary: a toolset
is composition, not authorization, and it never widens what the person could
reach.

`agentlab toolsets-test` proves the feature end to end against the released
components, as the admin, and leaves nothing behind (its agents are named
`agentlab-toolset-*`, its workflows likewise):

1. **agent-manager's contract** through muster: `create_agent` without a
   toolset is refused naming the shipped presets and `preset:none`; the removed
   `toolNames` argument is refused with the explaining error; four agents are
   created with `preset:read-only`, `preset:none`, `preset:full` and
   `server:lab-oauth-fixture` — a HelmRelease of the agent chart each.
2. **What was rendered**: `values.toolset` on each HelmRelease with the
   declared list, the `AgentTemplate` on the platform Harness binding the
   agent's own `RemoteMCPServer` that carries the header with the joined
   selectors, `get_agent` reporting the same; the `preset:none` agent has
   **no** RemoteMCPServer and no binding at all; a HelmRelease applied
   without the value (every agent that predates toolsets) renders the server
   without the header and `list_agents` reports `implicitFullAccess: true`.
3. **muster's resolution**, in sessions of the same user carrying the header
   an agent's runtime sends: two workflows are created (`core_workflow_create`
   as the caller), one query-only and one with a destructive step, and
   `describe_tool` shows the derived `readOnlyHint` on the first only;
   `preset:read-only` resolves to exactly the tools annotated `readOnlyHint`,
   whatever their kind — the query-only workflows included (the lab's
   `lab-cluster-overview` among them), the mutating one excluded, muster's
   core tools by their own annotation: since muster 5.13.0 the reads such as
   `core_config_get` and `core_workflow_list` are in, the writes such as
   `core_workflow_delete` never are (a muster whose core tools carry no
   annotations puts none in the preset); the read-only Kubernetes call succeeds
   and the destructive call (agent-manager's `delete_agent` — the lab's
   mcp-kubernetes runs non-destructive and registers no writers) and the
   mutating workflow are refused with `tool "…" is outside the toolset
   [preset:read-only]`; `preset:none` sees and gets
   nothing; `preset:full` equals the unscoped catalogue; header errors (an
   unknown preset, the reserved `toolset:`, an inline `label:`) are error
   results, never a fall-back; two toolsets on one token — request by request
   in one session and from two sessions in parallel — each see their own
   tools; when the platform chart shipped `infrastructure` / `agent-platform`,
   each resolves to the tools of the servers carrying that label (the latter
   plus `core_*`).
4. **The runtime path** (skip with `--skip-chat`): an `AgentInstance` and
   one A2A turn through the edge, as the user, the read-only agent lists nothing
   outside `preset:read-only` — read-only core tools may appear, no writer
   does (so kagent sends the header and the user's token) — and the
   `preset:none` agent answers a chat turn. The agents run on
   `default-model-config` (`--model-config` to pick another).
5. **The sign-in claim (ground-truth G6)**: before any sign-in,
   `server:lab-oauth-fixture` resolves to nothing for everyone
   (`toolset_unmatched` names the selector); the portal's Sign in (`POST
   /api/muster/auth/login` with the portal's own forwarded Dex id_token) yields
   the challenge, which the proof completes as the browser would; an
   agent-shaped session on the **same** id_token then resolves the fixture's
   tools and calls one; the real agent, driven through kagent with that token,
   reports them too. A second user, and the same user under a **fresh**
   id_token, still resolve nothing: muster keys a forwarded bearer's session by
   the token (`ext-<hash>`), the grant belongs to that session
   (`grantScope: session`), and the portal forwards one and the same
   id_token to muster and to kagent — which is why the claim holds in the
   portal and only there.
6. **The portal** (skip the create with `--skip-portal`): the Tools step's
   backend calls (`/api/muster/tools/filter` with `include_presets`, a
   `toolset=` resolution, an unmatched selector, an unknown preset relayed as
   muster's error), and the create path the Dev Portal's wizard takes —
   `x_agent-manager_create_agent` through the portal's muster backend
   (`POST /api/muster/call`) with the person's forwarded token — landing the
   HelmRelease as agent-manager's write with `values.toolset`, rendered and
   Ready on the platform Harness with the header on the agent's
   RemoteMCPServer.

`agentlab agents-test` declares `preset:read-only` for its agent and asserts
the refusal without one (agent-manager requires a toolset), and
`agentlab backstage-test` proves the MCP servers page's three groups from the
data the page reads — see [The muster plugin](backstage.md#the-muster-plugin).

## Lab-specific deviations from a real management cluster

| What | Why |
|---|---|
| `gatewayApi.gateway.create: true` — the chart-owned agentgateway Gateway **is** the public edge | A real MC fronts the platform with the cluster's shared Envoy Gateway; kind has none, so the data-plane Gateway itself terminates TLS for `*.127.0.0.1.nip.io` with the lab's wildcard cert (the chart's own standalone/kind mode, `ingress.mode: agentgateway-muster`). The Gateway API CRDs are embedded in the binary and applied before the install; a lab-owned NodePort Service pins the edge onto the kind port mapping (HACKS.md U10), and a CoreDNS rewrite points `*.127.0.0.1.nip.io` at it inside pods (outside, nip.io answers 127.0.0.1 by itself). |
| One Dex client, `agent-platform` | The chart's `global.identity` convention: muster and Backstage share the client, so a Backstage-forwarded token natively carries an audience muster trusts. The extra `dex-k8s-authenticator` client exists only as the cross-client audience target Backstage's GS auth provider requests by default. |
| `networkPolicy.enabled: false`, `kyvernoPolicies.enabled: false` | The chart's own policy objects. No Cilium and no Kyverno in kind, so both would render CRs whose API groups the cluster does not serve. |
| The muster/kagent ServiceMonitors, valkey PodMonitor and muster PrometheusRule follow `platform.observability` | Without it there is no Prometheus Operator, so none of those CRDs exist and the releases fail to render. With it they are scraped by the lab Prometheus — whose selectors are opened up (`*NilUsesHelmValues: false`) because upstream's default selects only monitors carrying the kps release label, and the platform's monitors come from other releases. Flipping observability rolls the muster pod once (the toggle changes its metrics-exporter env). |
| `platform.observability`: the GS kube-prometheus-stack constituent installed directly, Prometheus server re-enabled, instead of the observability-bundle | The bundle is MC-shaped (Flux HelmReleases with a hardcoded remote kubeconfig, Alloy → Mimir, no local PromQL endpoint). See [Observability](observability.md). |
| `muster.muster.oauth.mcpClient.enabled: true` + the `lab-oauth-fixture` MCPServer | The chart leaves muster's OAuth *client* role — the proxy behind `core_auth_login` and the portal's Sign in — off; real installations turn it on, and without it no per-server sign-in can be exercised. The fixture is the one `Auth Required` downstream to sign in to (muster's own protected `/mcp`); see [Signing in to a downstream server](#signing-in-to-a-downstream-server-muster-as-oauth-client). |
| The fake-fleet MCPServers (`<family>-lab-01`, `<family>-lab-02`) with `agent-platform.giantswarm.io/tool-group: infrastructure`, opt-in via `platform.fakeFleet` | One cluster and a family-less bundled `mcp-kubernetes` look nothing like the federated fleet the portal's server groups, fleet coverage and Tools step are built for. With the key on, six `Auth Required` family members fake it, carrying the tier label the fleet charts stamp; off (the default) a lab registers only its own servers and the proofs assert that shape. See [The fake fleet and the tool-group label](#the-fake-fleet-and-the-tool-group-label). |
| `muster.rbac.{mcpServerEditor,workflowEditor}.subjects` → `oidc:platform-admins` | The chart binds muster's editor Roles to Giant Swarm's admin groups, which do not exist here. Rebound to the lab's own admin group (`--oidc-groups-prefix=oidc:`, same spelling as the lab RBAC). Lists replace, so the GS groups are dropped. |
| muster patched to `hostNetwork` + `maxSurge: 0` | Same issuer trick as the apiserver and Backstage. `maxSurge: 0` because two hostNetwork pods cannot both bind `:8090` on a one-node cluster. A Kustomize strategic-merge patch in `components.muster.postRenderers`, which the chart forwards to muster's `HelmRelease` and the bundled helm-controller applies over the muster chart's render. |
| The `dex-localhost` sidecar on mcp-kubernetes, model-manager, agent-manager and mcp-prometheus | Those servers validate the forwarded Dex token themselves and must reach the issuer URL `https://localhost:32000/dex`, but all listen on `:8080` and cannot share the host network. A socat sidecar on the pod's own loopback forwards `:32000` to the Dex Service (HACKS.md U13) — a `postRenderers` patch on each component, and on the lab's own mcp-prometheus `HelmRelease`. |
| `components.kagent.enabled` from `platform.agents`; kagent ServiceMonitor + OTel off | Agents are part of what the lab tests, so kagent is on by default (the chart defaults it off) but optional — `platform.agents: false` skips the runtime. The controller's authentication is the fleet's (`trusted-proxy` behind the JWT `Strict` policy on the controller route); the kagent line serves no /metrics and there is no OTLP gateway in kind. See [Agents (kagent)](agents.md). |
| `kagent.controllerRoute.jwtAuthentication.jwks` = the lab Dex (`dex.dex.svc.cluster.local:5556/dex/keys` over TLS) | The chart's default JWKS source is a Giant Swarm Dex; the policy itself (`Strict`, the identity transformation) is the chart's default, unchanged. |
| `kagent.harness.snapshotLocation: s3://ate-snapshots/kagent` + `substrate.rustfs.enabled: true` | An installation names its own snapshot bucket (S3 with IRSA on CAPA); the lab's store is the substrate chart's bundled in-cluster RustFS. |
| `postgres`: one instance, a 2Gi claim, no backups | The fleet's CNPG `Cluster` runs three instances on 20Gi with Barman backups; the lab keeps the shape (the same operand and pgvector images, the same `Database` CRs and derived Secrets) on one kind node. |
| `kagent.ui.service.type: NodePort`, nodePort 30880 pinned by the kagent `postRenderers` patch | On a real MC the UI sits behind the agentgateway edge; this lab publishes it through the kind port mapping instead (host side `platform.agentsPort`, default 8081). The chart's Service template renders no `nodePort` field, so the fixed node port is a patch (HACKS.md U9). |
| `components.flux.enabled: true`, `gitops.self.enabled: false` | The lab shape (see [The agent platform](#the-agent-platform-muster--kubernetes-mcp)): a management cluster runs its own Flux and installs the chart through it; the lab has none, so the chart brings the engine — and must not adopt its own release, because the lab installs charts and images that are not released. |
| The chart pinned to an exact release (`platform.chartVersion`) | Component versions are the chart's own ranges, resolved by its Flux at reconcile time (the fleet's dogfooding track). The chart itself never floats in the lab: two runs install the same thing, and a bump is a deliberate edit with a lab run behind it. |
| The kind config turns on the `ClusterTrustBundle`, `ClusterTrustBundleProjection` and `PodCertificateRequest` gates | Agent Substrate (from the chart) needs them on the apiserver, controller-manager and kubelet; a Giant Swarm cluster sets them through its cluster chart. Fixed at `kind create` — the boot refuses a cluster that predates them. See [Agent Substrate and the platform Postgres](#agent-substrate-and-the-platform-postgres--from-the-chart). |
| `mcp-prometheus` as a lab-rendered Flux `HelmRelease` | The one release the lab installs outside the chart rides the same engine, as the same tenant identity, so its lab-only sidecar is a `postRenderers` patch like the others and there is exactly one Helm writer (the embedded Helm, for the chart) and one Flux engine on the cluster. |

## Platform gotchas

- **A cluster built by an earlier agentlab needs a clean slate.** Before the
  chart brought its own engine, the lab installed the agent-platform-standalone
  umbrella under the same release name and its own Flux controllers in
  `flux-system`; the chart refuses a second Flux, and there is no in-place
  migration on purpose — the kind cluster is throwaway. `agentlab platform`
  names what it found and asks for `agentlab down && agentlab up`. It also
  removes the leftovers of those versions in the working directory
  (`.vendor/`, `state/helm-plugins/`, `state/flux-values.yaml`,
  `state/mcp-prometheus-values.yaml`). To keep the cluster and its Dex
  instead, `agentlab platform-down` then `agentlab platform`: platform-down
  uninstalls the umbrella and those Flux controllers too, and the component
  releases replace the umbrella's CRDs (`crds: CreateReplace`).
- **`agentlab platform-down` is the chart's ordered teardown.** It deletes
  the lab's mcp-prometheus `HelmRelease` while the engine still runs, then
  the embedded Helm's waited uninstall runs the chart's pre-delete hooks — delete the
  component `HelmRelease`s and wait for their releases, then delete the
  `FluxInstance` and wait for the operator to remove Flux with its CRDs,
  which takes every remaining `HelmRelease` (the agents' too) with it — and
  only then deletes the namespaces. Nothing is left with a finalizer nobody
  processes. The four Flux Operator CRDs and the prometheus-operator CRDs
  stay (Helm never removes a chart's `crds/`); a reinstall is clean. On a
  cluster an earlier agentlab built it also uninstalls the Flux controllers
  that lab installed itself (release `flux` in `flux-system`).
- **A WorkerPool outlives a Substrate database it never knew.** When
  Agent Substrate's control-plane database is replaced under a running
  `WorkerPool` — a lab moving from the substrate chart's bundled Postgres to
  the platform's CNPG `Database`, which `agentlab platform` does once on a
  lab that ran an earlier shape — ate-api-server restarts against an empty
  database, ate-controller re-registers the existing worker pods, and every
  golden boot then fails inside ate-api-server with `AssignWorker:
  ResourceExhausted: no free workers available` (the `AgentTemplate` stays
  `Ready=False ActorTemplatePending`, `skills-test` red after its timeout):
  the re-registered workers still hold the actors the old database knew and
  never report free. Free them the documented way — `kubectl -n kagent delete
  pod -l ate.dev/worker-pool=kagent-default`; the `WorkerPool` recreates
  them, the pending golden actor gets a worker within seconds. A fresh
  install and `platform-down` → `platform` recreate the pods anyway.
- **The first turn after idling hangs on a full laptop disk — the lab
  renders an atelet image-cache policy against it.** Substrate's atelet
  evicts cached images on every GC pass (`--image-cache-gc-period`, 5 min)
  while its cache volume is at or above `--image-cache-high-percent` (85 %),
  sparing only records younger than `--image-cache-min-age` (2 min). In the
  lab that volume is the kind node's root overlay, i.e. the host's disk: on a
  laptop above 85 % the Harness image is gone five minutes after every turn
  (atelet log `Image cache evicting image record`), the next turn pulls and
  unpacks it cold (`Restore timing breakdown` with `oci_unpack` ≈ 5 s), the
  atenet router's 5 s parked-request budget cancels the restore (ate-api-server
  `CallAteletRestore … context canceled`) and the agent stays "Working…" —
  while every turn within five minutes of the previous one works. The lab
  takes the host disk out of the policy through the chart's
  `substrate.atelet.extraArgs`: `--image-cache-high-percent=100` and
  `--image-cache-low-percent=99` (eviction only below 1 % free, where nothing
  runs anyway) with `--image-cache-max-bytes=4294967296` as the bound instead
  (4 GiB, some twenty Harness digests, evicted oldest-first past that);
  period and min-age keep the chart's defaults. `agentlab platform` rolls the
  atelet DaemonSet to it on an existing lab and `agentlab platform-test`
  asserts the flags on the rolled DaemonSet. A `platform.valuesFiles` overlay
  that sets `substrate.atelet.extraArgs` replaces the whole list (Helm merges
  maps, not lists) — keep the policy and the registry flag in it, or drop the
  key. The Substrate line's fix (eviction that spares live images, a restore
  budget that covers a cold start) retires the policy once the meta chart
  pins that release.
- **`allowPublicClientRegistration` must be on for Claude Code's login.**
  Claude Code registers over DCR as a public client on a random loopback port,
  so none of the other registration gates can be opened for it: it cannot send
  a registration token, `trustedPublicRegistrationRedirectURIs` cannot match a
  random port, and `http`/`https` are deliberately stripped from
  `trustedPublicRegistrationSchemes` by mcp-oauth's config validation. The
  muster chart renders the key since 5.7.2 (muster#1118); before that,
  `agentlab post-render` had to edit it into the rendered ConfigMap
  (HACKS.md U1, now fixed upstream).
- **A `Recreate` strategy cannot be patched onto an existing Deployment.** The API
  server has already defaulted `spec.strategy.rollingUpdate`, and a patch that
  flips the type without also deleting that field fails with
  `rollingUpdate: Forbidden`. `maxSurge: 0` achieves the same thing without the
  conflict.
- **The `kagent` namespace follows the kagent component.** While
  `components.kagent.enabled` is true the chart's pre-install/pre-upgrade hook
  creates it ahead of the kagent `HelmRelease`, the connectivity component
  adopts it, and the uninstall (the ordered teardown) removes it with the
  connectivity release. The lab creates no namespace of its own for kagent.
- **Kubernetes tools carry the server-name prefix.** The chart's bundled
  `mcp-kubernetes` MCPServer declares no muster *family*, so its tools use
  per-server prefixing: `call_tool(name=x_mcp-kubernetes_list, arguments={...})`. A default lab has no family server at all, and the fake fleet's
  members (`platform.fakeFleet`) stay `Auth Required`, so no `x_kubernetes_*` family tools appear in a session either way.
  (Real fleet installations register per-cluster servers with a `kubernetes`
  family and a `management_cluster` instance argument instead.)
- **Tool results are double-wrapped.** `result.content[0].text` is JSON whose
  `content[0].text` is the actual payload — two decode hops.
