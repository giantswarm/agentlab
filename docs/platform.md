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
  same `helm upgrade --install … --wait --force-conflicts`, no post-renderer
  (forcing conflicts takes over the fields an earlier install's field manager
  owns, so a lab brought up by an older release keeps upgrading) — and the
  release it writes is a regular one, so `helm
  upgrade` from a shell (`KUBECONFIG=state/kubeconfig`) stays the day-2 tool.
  A re-run that would install the same chart version with the same values
  writes no revision (`chart agent-platform <version> already installed with
  these values — nothing to do`) and only re-reads every component's health;
  a changed value, version or a chart directory upgrades. A chart in place
  that is not the one being installed — a dev chart an earlier run left
  behind, a checkout's build or a branch's dev build — is named before the
  upgrade replaces it (`the cluster runs a dev chart left in place: …`), and
  `agentlab status` names it without installing anything: every revision
  the lab writes carries the chart's channel as the release label
  `agentlab.giantswarm.io/chart-channel` (`stable`, `dev` or `path`) and a
  `helm history` description saying which agentlab installed what from
  where — the only way to tell a checkout's build, whose `Chart.yaml`
  version is a placeholder, from a release.
  Never drop the value on a lab: the first upgrade without it makes the
  release self-managed.
- **The component set is the deployed revision's.** Helm diffs an upgrade
  against the last *deployed* revision, so the `HelmRelease` and
  `OCIRepository` of a component that a failed upgrade added stay in the
  cluster after the next successful one, owned by the release and rendered by
  none of its revisions. After every run that leaves a deployed revision —
  an upgrade or a "nothing to do" — `agentlab platform` deletes every
  `HelmRelease` and `OCIRepository` carrying the release's Helm annotations
  that the deployed revision (manifest and hooks) does not render, the
  `HelmRelease` first and waited for (helm-controller finishes the action it
  is running, then uninstalls the component, each within the release's own
  timeout), and names each one (`removed HelmRelease
  agent-platform/<name>: release agent-platform owns it, its deployed
  revision does not render it`). Objects of other owners — the lab's own
  `mcp-prometheus`, the agents' sources — are never touched; while the
  newest revision is a failed one nothing is removed, the
  next deployed run cleans up.
- **Backstage's app-config is put back on every run.** The portal reads
  two app-config ConfigMaps at start only: the chart's
  (`agent-platform-backstage-app-config`, rendered by the connectivity
  release) and the lab's overlay (`agentlab-backstage-app-config`). Every
  `agentlab platform` (and `up`) server-side applies the overlay as the
  lab renders it (field manager `agentlab`, forced) and restores the
  chart's ConfigMap to its Helm release's manifest, dropping data keys
  neither render carries. Backstage rolls once when a restore changed what
  it reads, and a changed overlay render rolls it through the chart (the
  overlay's `extraAppConfig` `checksum`, the data checksum the ConfigMap also
  carries as `agentlab.giantswarm.io/data-checksum`); a run without drift
  restarts nothing. `agentlab status` names a drifted ConfigMap on its
  `app-config` line.
- The chart is **pinned** to an exact release, `platform.chartVersion` in
  `agentlab.yaml` (the default is the release this agentlab was verified
  with). The lab never floats; bump the pin deliberately, with a lab run.
  The pin and the cluster drift apart when an `agentlab.yaml` is put back
  from a copy without the `agentlab platform` that would install it (a
  holder restoring a shared lab as found): `agentlab status` then names
  both charts and which side is behind, every proof opens with the chart
  in place (`Proving agent-platform <version>`) and warns of the drift
  instead of quoting the pin, and `agentlab configure --adopt-chart`
  writes the chart in place into the pin — or `agentlab platform` installs
  the pin over the cluster, the boot naming the downgrade.
- A `platform.valuesFiles` overlay changes what the lab runs without
  changing the chart version: a proof's `components.<name>.versionRange`
  pins a component the chart's own range does not admit. `agentlab status`
  names every overlay with the values it sets, and the ones the release in
  place was installed with (each platform run records them in its
  revision's description) that `agentlab.yaml` no longer lists. Before a
  shared lab is handed on, `agentlab configure --released` drops the proof
  state (overlays, dev images, a chart directory, the dev channel, a local
  guest image) and `agentlab platform` installs the released platform, so
  the next holder starts there.
- A proof runs against the components in place when it starts. The meta
  chart tracks most components by semver range, and source-controller
  resolves a range again on every interval, so a component release published
  mid-proof would roll its HelmRelease under the proof. Every `*-test`
  therefore holds the ranges for its length: each OCIRepository in
  `agent-platform` whose `spec.ref.semver` is a range (and has resolved it) is
  suspended — it keeps the artifact it has, its HelmRelease keeps the chart
  it runs — with the proof listed in its `agentlab.giantswarm.io/range-hold`
  annotation, and resumed when the proof ends, failed or interrupted. An
  exact version is never held, nor a source someone suspended by hand. Two
  proofs on one lab share the hold (the last one out resumes), and a holder
  whose process is gone is pruned by the next proof. An explicit change wins:
  `agentlab platform` and `up` (and the step proofs' installs) lift every hold
  before they install, so `platform.devImages`, `platform.chartPath`, an
  overlay's version and a new chart apply, and a proof in the same process
  holds again on what the install left running. A release that must reach
  the lab mid-lease comes by `agentlab platform`; otherwise it lands after
  the proof.
- A field a hand wrote on a platform workload — `kubectl patch` of muster's
  `hostNetwork`, a `kubectl set env` on Backstage — is no release's: Helm and
  helm-controller's drift correction own and correct only the fields the
  render carries. The apiserver records each field's writer
  (`managedFields`). `agentlab platform`, the step a lab's restore ends with,
  puts every field one of kubectl's imperative edits wrote (`kubectl-set`,
  `kubectl-patch`, `kubectl-edit`, `kubectl-client-side-apply`,
  `kubectl-label`, `kubectl-annotate`) on a Helm-owned Deployment,
  StatefulSet or DaemonSet back to its release's render — the rendered value
  where the render carries the field, removed where it does not — drops the
  edit's entry, names each (`Put back to its release's render: …`) and waits
  for the roll; a field it cannot put back fails the run, named. A field
  written by any other manager than the releases' (`helm`, `helm-controller`,
  the lab's `agentlab`, the connectivity release's config-reload hook
  `agent-platform-config-reload`, which stamps the Backstage app-config
  checksum into the pod template, `kube-controller-manager`) is named by `agentlab
  status` on its `workloads` line, warned at the end of `agentlab platform`
  and at every proof's opening, and fails platform-test's first stage,
  `workload field managers`. A rollout restart's `restartedAt` annotation is
  none. A lab change belongs in `agentlab.yaml` (`platform.devImages`,
  `platform.chartPath`), never a patch.
  The one exception is deliberate too: the [dev channel](#dev-channel),
  where `platform.chartBranch` follows a branch's newest dev build — and
  still installs an exact version, written into `chartVersion`.
- A **3.x meta chart** renders the **3.x lab shape**: a release or a dev
  build (`chartVersion` below `4.0.0`, on the dev channel the resolved
  build of a `release-v3.x` branch) or a chart directory whose `values.yaml`
  roster has no `components.substrate` (a checkout of the maintenance line;
  its `Chart.yaml` version is a placeholder on both lines). That line's
  root schema is closed and the kagent 0.10 wrapper it resolves refuses the
  current line's keys, so the lab values carry no platform Postgres
  (`postgres`, `components.cloudnative-pg`), no `substrate`, and a `kagent`
  block without `harness`, `database`, the JWT policy on the controller route
  or the CNPG DSN mount — kagent 0.10 on its bundled Postgres, the controller
  in its local-dev auth mode, the controller ServiceMonitor following
  observability. This is what a migration rehearsal seeds before upgrading
  in place to the 4.x line, and what a fix on the 3.x maintenance line is
  proven on; the switch is `config.LegacyChart`. Its connectivity chart
  registers the family-less MCPServer `mcp-kubernetes` (tools
  `x_mcp-kubernetes_<tool>`, no `management_cluster`), which the boot and the
  proofs address on that line. `platform-test` on it skips the controller
  identity proof and the infrastructure families proof and says why (the 3.x
  connectivity chart renders no `GRPCRoute` for the controller and no family
  member; the muster and mcp-kubernetes identity proofs cover the edge). `platform.serving` and
  `platform.vmManager` came with the 4.x line and are refused on it, with
  the switch that turns them off. The
  rehearsal — the four fleet shapes, the in-place upgrade, the migrate Job's
  three phases, the timings — is [The migration rehearsal](migration-rehearsal.md).

The platform installs as part of `agentlab up` (it is enabled in the default
configuration); on an already-running cluster the steps are also standalone:

```bash
agentlab platform       # upgrade-or-install of the chart (embedded Helm), then waits for every component
agentlab platform-test  # headless proof of the whole chain
```

`platform-test` runs in stages — the Dex token, the muster session, the MCP
tool call, then every further proof — and ends with a summary that names
each stage with its outcome and duration before the verdict: on a warm lab
the whole proof takes about a second, which the whole-second stamps on the
step lines cannot show. A stage the lab's live state cannot run never
passes silently: a fixture user missing from `agentlab.yaml`, a release the
chart renders but did not install, a read that failed each fail the proof,
naming the stage (`Error: muster session: muster is not reachable …`). The
one allowance is the configuration: the stages `platform.agents: false` or
`platform.observability: false` leave without a subject, and the ones the
3.x line has no route or family member for, are listed as `SKIP` with that
reason and the proof still passes.

```
==> [00:01] 19 stages in 1.1s
    PASS    78ms  Dex token
    PASS    41ms  muster session
    PASS   102ms  Kubernetes tools
    PASS    95ms  MCP tool call
    …
    SKIP     0ms  controller identity  (the 3.x connectivity chart renders no GRPCRoute …)
```

The lab's patches on the component charts — `hostNetwork` on muster and
Backstage, the `dex-localhost` sidecar on every server told the lab Dex's
address, the kagent UI NodePort, and the dev images below — are per-component
**`postRenderers` values** (`components.<name>.postRenderers` in
`state/agent-platform-values.yaml`): Kustomize patches the chart forwards to
that component's `HelmRelease` and the bundled helm-controller applies over
the component chart's render — the same mechanism the fleet has for chart
fixes. The sidecar's targets are read off the component charts' renders
(every Deployment whose containers are told `localhost:<dexPort>` through a
name they dial it by — see below), so `agentlab platform` renders the values
twice: once for the roster, then with the targets for the install. A plain
`agentlab render` shows the first, without sidecars.

### Which Deployments get the `dex-localhost` sidecar

A Deployment off the host network is a sidecar target when a container is told
the lab Dex's localhost address through a name the platform's OAuth resource
servers **dial** the issuer by — for OIDC discovery and the JWKS:

| Name | As | Who |
|---|---|---|
| `--dex-issuer-url` | argument | the managers (model-manager, agent-manager, vm-manager, cluster-manager) |
| `DEX_ISSUER_URL` | environment variable | the mcp-oauth resource servers (mcp-kubernetes, mcp-prometheus) |
| `OIDC_ISSUER_URL` | environment variable | oauth2-proxy (the kagent UI) |

The sidecar is a native sidecar: an init container that restarts always, with a
startup probe on its listen port, so the kubelet starts the server's own
container only once the forwarder listens and a server that dials the issuer at
start is never refused.

An argument counts in its variable spelling, so `--oidc-issuer-url=…` is
`OIDC_ISSUER_URL`. A variable that carries the address as an **identity** only
is not a dial target and selects nothing, whatever its value: the
authorization server a resource server names in its RFC 9728 metadata and
muster keys grants by (`OAUTH_AUTHORIZATION_SERVER`), an issuer a token is
checked against. Both `agentlab platform` (the `dex-localhost sidecar on …`
line) and `agentlab platform-test` name the argument or variable each target
was selected by.

The annotation `agentlab.giantswarm.io/dex-localhost` on the Deployment or its
pod template decides instead where the rule does not fit: `"false"` keeps the
sidecar off a Deployment the rule would select, `"true"` puts it on one the
rule would skip — a server that dials the issuer through a name the rule does
not know. Any other value fails `agentlab platform` and `platform-test`.

## Installing an unreleased chart

Point `platform.chartPath` at a local checkout's chart directory and re-run
`agentlab platform` — nothing needs a release or a push:

```yaml
platform:
  chartPath: /path/to/agent-platform/helm/agent-platform
```

(or `agentlab configure --defaults --chart-path /path/to/agent-platform/helm/agent-platform`;
`--chart-path ""` clears it). `chartVersion` is ignored while it is set, and
the boot says which chart it installed. The directories are read, never
written. Clearing it and re-running `agentlab platform` puts the pinned
release back in place, the boot saying so (`the cluster runs a dev chart
left in place: agent-platform 1.1.35, a chart directory's build (…);
agent-platform 4.116.0 replaces it`); `agentlab status` says which chart a
lab runs before anything is installed on it.

**Both charts of the checkout are installed.** The meta chart installs its
wiring chart, `agent-platform-connectivity`, at its own exact version
(`components.agent-platform-connectivity.releasedWithChart`: the two charts
are published off one git tag, so an installation's connectivity never lags
or leads the meta chart). A checkout's meta chart carries the placeholder
version of its `Chart.yaml`, which no registry publishes — so the lab
packages the checkout's connectivity chart from the sibling directory
(`helm/agent-platform-connectivity`, refused when it is not there) at that
version, pushes it into the lab registry (the `registry` container on the
kind network the [dev images](#dev-images) go through), and points the meta
chart at it (`components.agent-platform-connectivity.repository`, `insecure`
and `versionRange` in the rendered values — the knobs the meta chart admits
for a chart pushed by hand). An edit to either chart is one `agentlab
platform` away: the connectivity release is asked to fetch the pushed chart
and waited for until it runs it (`status.history[0].ociDigest` names the
pushed manifest). The offline renders of the boot read the connectivity
chart from the directory too, so a value it refuses is refused before the
install. A `platform.valuesFiles` overlay that still points the component at
a branch's dev channel (`versionRange` + `semverFilter`) wins over the lab's
source and must go — it selected a chart from the registry, not the
checkout's.

### An unreleased component chart

Any other component's chart (kagent, Substrate, muster, …) is proven the
same way by hand: push it into the lab registry through its host port and
point the component at it with a `platform.valuesFiles` overlay.

```sh
helm package /path/to/kagent/helm/kagent --version 0.9.0-dev.1
helm push kagent-0.9.0-dev.1.tgz oci://localhost:5001/charts --plain-http
```

```yaml
components:
  kagent:
    repository: oci://agentlab-registry:5000/charts   # <clusterName>-registry, its kind-network name
    insecure: true
    versionRange: "0.9.0-dev.1"
```

The HelmRelease keeps the kind-network name, the one source-controller
reaches. The boot's offline render of the chart, which no resolver on the
host can name that way, goes through the port the registry publishes on the
host loopback (`platform.devRegistryPort`, 5001 by default) over plain HTTP,
and the boot log names both (`read on this host as
oci://localhost:5001/charts/kagent`). A registry that does not answer there
stops the boot before the install, naming the container to check. Every `up`
and `platform` run ensures the lab registry — created when missing, started
when stopped — since atelet and the node's containerd point at it whether or
not a dev image is configured; `agentlab status` names it on its `registry`
line, and it goes with `agentlab down`. Removing the overlay and re-running `agentlab platform`
puts the released chart back.

## Dev images

To run a component from a build of your own, build the image, and name it
under `platform.devImages`. The targets are the chart's component names for
the Deployments the lab can swap — `muster`, `backstage`, `kagent` (the
controller), `mcp-kubernetes`, `model-manager`, `agent-manager`,
`vm-manager` (with `platform.vmManager`), `klaus-gateway` (with
`platform.klausGateway`), `cluster-manager` (which the entry turns on, see
below), `workspace-manager` (with `platform.workspaces`), `ate-api-server` (Agent
Substrate's API server, where the golden snapshots are taken; with the
agents), the agentgateway component's two Deployments under their own names,
`agentgateway` (the data-plane proxy) and `agentgateway-controller` —,
`mcp-prometheus` (the lab's own release outside the meta chart, with
`platform.observability`; the swap rides that HelmRelease's `postRenderers`,
[docs/observability.md](observability.md#alertmanager)),
`harness`, the platform Harness's runtime image (the Go ADK every agent runs
on under kagent API v2), and `claude-harness`, `codex-harness` and `omp-harness`, a coding
Harness's image (with that Harness rendered, [below](#coding-harness-targets)):

```yaml
platform:
  devImages:
    muster: muster:dev-1a2b3c
    backstage: backstage-dev:my-feature-4d5e6f
    kagent: kagent-controller:dev-7e8f9a      # the line's controller, built from go/ of giantswarm/kagent-upstream
    harness: golang-adk:dev-7e8f9a            # the Go ADK, same checkout, BUILD_PACKAGE=adk/cmd/main.go
    cluster-manager: cluster-manager:dev-3e4f  # turns the component on, with the lab's audience for it
    workspace-manager: workspace-manager:dev-5a6b  # with platform.workspaces on
    agentgateway: agentgateway:dev-7c8d       # the proxy, built from giantswarm/agentgateway-upstream
    agentgateway-controller: agentgateway-controller:dev-7c8d
```

Either way the swap is part of the release: `agentlab platform` renders it
into the values it installs, so the same plain `helm upgrade` applies it, and
removing the entry restores the chart's image on the next run. Use a
distinctive tag per build — kind's containerd keeps running the old bits
under a reused tag, and a Harness digest that did not change recompiles
nothing.

A build named without a registry (`muster:dev-1a2b3c`) runs under the lab's
name for local builds, `localhost/muster:dev-1a2b3c`: `agentlab platform`
tags it so, side-loads it under that name and writes that name into the
override, so the pod's ref and the node's copy agree and no pod of the lab
names a registry that does not have its image (podman spells local builds
this way itself). A ref that names a registry — the lab registry, a gsoci
release, a line's dev build on gsoci — is used as written.

The `mcp-prometheus` swap changes the image only; a release candidate whose
chart changed too pins that chart with `platform.mcpPrometheusChartVersion`
(an exact version, a release candidate included; with
`platform.observability`). The lab renders it into its mcp-prometheus
`OCIRepository` in place of the release this agentlab pins
(`internal/lab/observability.go`), so the next `agentlab platform` installs
it, and removing the key returns to the pin:

```yaml
platform:
  mcpPrometheusChartVersion: 0.8.1-rc.1
  devImages:
    mcp-prometheus: gsoci.azurecr.io/giantswarm/mcp-prometheus:0.8.1-rc.1   # optional: the chart's default image follows its version
```

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

The `workspace-manager` target also runs the manager's sync Jobs on the
build: the Jobs are made from a template in a ConfigMap, which the override
does not reach, so the same patch sets the manager's
`WORKSPACE_MANAGER_SYNC_IMAGE` (its `--sync-image`) to the dev image and
`WORKSPACE_MANAGER_SYNC_IMAGE_PULL_POLICY` to `IfNotPresent`. Without the
target neither is set and the Jobs keep the chart's image.

The image name the override replaces is read off the component chart's
render, the one the boot renders anyway to side-load the platform images —
not from a table, because it differs between lines: the `kagent` target
replaces `gsoci.azurecr.io/giantswarm/kagent-controller` under the 3.x meta
chart (the wrapper chart) and `gsoci.azurecr.io/giantswarm/kagent/controller`
under 4.x (the kagent line's own chart, published under the line's own path),
whatever Deployment `kagent-controller`'s `controller` container names. A
Kustomize image override whose name is in no rendered object matches nothing,
and kustomize drops it without a word — so
a target whose chart rendered without the Deployment and container the lab
patches is **refused before the install**, naming both (`platform.devImages.kagent:
the kagent chart's render has no Deployment kagent-controller with a container
controller …`); a chart whose render was skipped (the preload notes why; a
registry that does not answer, or has no such chart, stops the boot instead,
see the gotchas) falls
back to the table's name with a note that it is unverified. A target's patch
lands on the release whose chart renders the Deployment: the
`agentgateway-controller` target (Deployment `agentgateway-controller`,
container `controller`) rides the `agentgateway` component's
`postRenderers`.

### The `cluster-manager` target

The meta chart ships cluster-manager off, and the lab has no other switch
for it: naming its image under `platform.devImages.cluster-manager` turns
the component on (`components.cluster-manager.enabled: true` in the rendered
values) with the lab's patches on it — the `dex-localhost` sidecar the rule
finds on its render, the image override, the pull policy — and the lab's
own values for it: `cluster-manager.muster.mcpServer.auth.requiredAudiences:
[kubernetes]`, the same audience story as mcp-kubernetes, agent-manager and
model-manager. cluster-manager's downstream OAuth presents the forwarded
token to the kind apiserver as the caller, so without that audience a
headless session sees no `x_cluster-manager_*` tool while the MCPServer reads
Connected. The audience block renders whatever turns the component on — the
meta chart forwards it only while the component is enabled — so a
`platform.valuesFiles` overlay that enables `components.cluster-manager`
itself needs no audience of its own either. Removing the entry turns the
component off again on the next `agentlab platform`. A released image works
as a target too (`gsoci.azurecr.io/giantswarm/cluster-manager:0.33.5`): the
lab side-loads it and the chart's Deployment runs it as written.

### The `agentgateway` target

The agentgateway chart renders one Deployment, the controller. The
data-plane proxy — Deployment `agentgateway`, the edge every lab URL and MCP
call passes — is the controller's: it creates it at run time for the lab's
Gateway from the image its chart hands it as three values,
`proxy.image.registry`, `.repository` and `.tag` (the controller's
`AGW_PROXY_IMAGE_*` environment), so no post-renderer on the component's
release can reach that Deployment. `platform.devImages.agentgateway`
therefore travels as those values — the side-loaded name split into
registry, repository and tag (`localhost` / `agentgateway` / `dev-7c8d` for
a build of this host), which is why a digest ref is refused for this target
— side-loaded into the node like every Deployment target; the controller's
proxies pull `IfNotPresent`, so the build is used as is. After the install
the boot waits for the controller to roll the proxy onto the image and
reports it (`the agentgateway proxy (Deployment agentgateway) runs
localhost/agentgateway:dev-7c8d`). A `platform.valuesFiles` overlay that
sets `agentgateway.proxy.image` itself is refused while the target is
configured, like the harness's pin.

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

### Coding Harness targets

`claude-harness`, `codex-harness` and `omp-harness` swap a coding Harness's image for a
build of the kagent line's `go/harness/<runtime>/Dockerfile`, say from a
pull request, so a fork change is proven before its release. They travel the
`harness` target's way: pushed to the lab registry, pinned by digest, and
forwarded as the `image` of that runtime's `kagent.harnesses` entry, which
the kagent chart renders into the Harness instead of its own image. A target
needs its Harness rendered (`platform.coding` with the runtime in
`platform.coding.harnesses`; `claude` also with the workspaces on). The boot
asserts the Harness runs the dev digest and waits for it Ready; an overlay
that replaces `kagent.harnesses` is refused while a target is configured.

A CLI bump needs `platform.devImages.kagent` from the same build:
the kagent controller compiles the `claude` and `codex` Harnesses' pinned CLI
version (`ExpectedClaudeVersion` from `PinnedClaudeVersion`, `ExpectedCodexVersion`
from `PinnedCodexVersion`) into their config with `StrictVersion`, so a
`claude-harness` or `codex-harness` whose CLI differs from the lab
controller's pin refuses to start on the version mismatch and the golden boot
loops. `agentlab` therefore refuses either target without `kagent`; build
both from one checkout of giantswarm/kagent-upstream.

```yaml
platform:
  coding:
    enabled: true
    harnesses: [claude, codex, omp]
    images:
      omp: gsoci.azurecr.io/giantswarm/kagent/omp-harness@sha256:…   # the release's runtimeImages.ompHarness.digest; omp-harness below wins
  devImages:
    codex-harness: codex-harness:pr-123   # docker build -f go/harness/codex/Dockerfile go/ of giantswarm/kagent-upstream
    kagent: kagent-controller:pr-123      # the controller from the same checkout: it pins the harness's CLI version
    omp-harness: omp-harness:pr-456       # docker build -f go/harness/omp/Dockerfile go/
```

muster runs with `hostNetwork`, so it binds `:8090` on the node, and the
rendered kind config publishes that onto the Mac (host port `platform.musterPort`,
default 8090) — no port-forward. Port mappings are fixed at node-creation time,
so changing the port means `agentlab down && agentlab up`; the stopgap on an old
cluster is `kubectl -n agent-platform port-forward svc/muster 8090:8090`.

### muster on the pod network

`platform.musterPodNetwork: true` (`agentlab configure --muster-pod-network`)
runs muster the way an installation does, the shape its availability work is
proven in: on the pod network, at two replicas behind the edge, with a
PodDisruptionBudget of `minAvailable: 1` (the values set `muster.replicaCount`
and `muster.podDisruptionBudget`). The host-network patch is not applied to
muster then; the `dex-localhost` sidecar bridges the issuer
`https://localhost:<dexPort>/dex` into each muster pod as it does for the MCP
servers (under `platform.tls` cluster DNS answers the issuer and no sidecar is
patched). Both replicas share the Valkey stores, and muster checks an MCP
session ID for its format only, so a session needs no affinity: a request on
the other replica is served as is. Backstage stays on the host network. The
direct debug port (`platform.musterPort`) answers nothing in this shape: use
the edge or `kubectl -n agent-platform port-forward svc/muster 8090:8090`.
`agentlab beekeeper-decisions-test` reaches muster on that port in the node's
network and refuses to run with the option on.

`agentlab platform-test` adds its `muster replicas` stage with the option on:
the Deployment's two Ready pods off the host network, each with the sidecar,
the budget's `minAvailable: 1`; then one pod is deleted while the proof's MCP
session calls `tools/list` through the edge every 250 ms, and not one call may
fail until the replacement is Ready and the Deployment has two Ready pods
again. With the option off (the default) the stage is skipped and muster runs
on the host network at one replica.

## Upgrade proofs from an older line

agentlab refuses a released chart below agent-platform 4.93.0, the first
release whose connectivity chart registers the bundled mcp-kubernetes as a
kubernetes family member. A proof that something survives an upgrade from an
older 4.x line (a conversation started on the kagent 1.0 line, agent-platform
4.49 to 4.70, moving to the current one) needs a lab on that line first:
`platform.upgradeSeed` (`agentlab configure --upgrade-seed`) lifts the
refusal. The seed installs the release in the shape of its line, as the 3.x
lab shape does: its connectivity chart registers the family-less MCPServer
`mcp-kubernetes`, which the boot and the proofs address
(`x_mcp-kubernetes_<tool>`), and `platform-test` skips the infrastructure
families proof and says why. Everything else is the current lab: the same
agentlab release, the same `agentlab.yaml`.

```bash
agentlab configure --defaults --upgrade-seed --chart-version 4.66.5
agentlab up                  # or `agentlab platform` on a fresh cluster
agentlab platform-test       # the seed's baseline
# … create the agents and conversations the proof upgrades …
agentlab configure --defaults --upgrade-seed=false --chart-version <the default>
agentlab platform            # helm upgrade in place, then every component's health
agentlab platform-test       # the families proof runs again
```

The upgrade is the lab's ordinary path: `agentlab platform` upgrades the
release to the new pin, the components' HelmReleases follow, and the
family-less `mcp-kubernetes` gives way to `<clusterName>-mcp-kubernetes`. The
switch has no effect on a chart at or above the floor; clearing it returns
the lab to the standard configuration and the refusal to its place. Seed a
fresh cluster: installing an older line over a lab that ran a newer one is a
downgrade, which no proof is about.

## Dev channel

The stable channel is the pin above: `platform.chartVersion`, an exact
release. The **dev channel** follows a branch of agent-platform instead —
the dev builds gitsemver publishes for every commit of a branch that has
branch publishing on (`gen.ci.branchPublish` in giantswarm/github): charts
tagged `X.Y.Z-r<branch-hash>t<YYYYMMDDHHMMSS>h<sha7>` (gitsemver 3's dev
shape, since architect-orb 10.10.0 on 2026-09-23; builds before it carry
`X.Y.Z-dev.<branch>.<YYYY-MM-DD>.<HH-MM-SS>.h<sha>`) next to the
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
  chart      agent-platform 4.66.5-r7b5b4fa7t20260911081233h7f841be (branch my-feature, dev channel)
```

and `agentlab.yaml` carries both:

```yaml
platform:
  chartBranch: my-feature   # the dev channel: chartVersion follows this branch's newest dev build
  chartVersion: 4.66.5-r7b5b4fa7t20260911081233h7f841be   # written by the resolver
```

How the resolution works: the branch is fingerprinted the way gitsemver
embeds it (the CRC32 of the full branch name as eight hex digits —
`gitsemver branch-hash my-feature` prints `7b5b4fa7`), the registry's tags
are listed through the embedded Helm's registry client (anonymously, as
pulls are), the tags whose prerelease is `r7b5b4fa7t<14 digits>h<7 hex>` are
the branch's builds — and so are the superseded
`dev.my-feature.<date>.<time>.h<sha>` tags of builds made before 2026-09-23
(the branch lowercased, anything outside `[a-z0-9]` one hyphen, a long name
shortened around a `--` marker), which the registry keeps and a branch not
pushed since has no others of — and the highest semver among them is the
newest; at one `X.Y.Z` a current tag sorts above a superseded one. That is
the pick a Flux `OCIRepository` with a prerelease-admitting range
(`>=0.0.0-0`) and the branch's `semverFilter`
(`^.*-r7b5b4fa7t[0-9]{14}h[0-9a-f]{7}$`) makes. `up` and
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
  unpinned, and `""` also puts the default release in place of the branch's
  build the resolver wrote into `chartVersion` — unless `--chart-version`
  names one in the same call — so the next `agentlab platform` installs a
  release, never the dev build the branch left behind for the next person.
- **No build yet**: a branch without dev builds is refused with the two ways
  out — the branch's publish has not run (agent-platform needs
  `gen.ci.branchPublish` and a commit on the branch), or pin a tag by hand.
- **The fallback that needs no resolver**: `platform.chartVersion` accepts a
  full dev tag as it is — `agentlab configure --chart-version
  4.66.5-r7b5b4fa7t20260911081233h7f841be` validates and
  Helm pulls that exact tag (look it up with `crane ls
  gsoci.azurecr.io/charts/giantswarm/agent-platform | grep -- "-r$(gitsemver branch-hash my-feature)t"`).
  The lab then does not follow the branch.
- `chartBranch` and `chartPath` are mutually exclusive: a local chart has no
  builds to follow.
- The dev-tag schema lives in one place, `devTagFilter` in
  `internal/lab/chartbranch.go` (the hash in `config.BranchHash`): the
  current shape and the superseded one, which goes when no branch's newest
  build carries it any more.

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

kagent API v2 (the 4.x line: `api.kagent.dev/v1alpha3`, kagent `1.x` from the
Giant Swarm kagent line's own releases) runs every agent as an actor on
[Agent Substrate](https://github.com/kagent-dev/substrate) — sandboxed
(gVisor) worker pods of a `WorkerPool`, an API server, a per-node agent
(`atelet`) and the actors' ingress/egress data plane (`atenet`) — from the
Giant Swarm line [giantswarm/substrate](https://github.com/giantswarm/substrate)
(its `FORK.md` records the pin, the carried patches and the published
versions). **The chart ships it**: `components.substrate-crds` and
`components.substrate` follow `components.kagent`, both land in `ate-system`
as component releases of the chart's engine at the version the chart pins
(`>=1.0.0 <1.1.0` from agent-platform 4.49.0 — the line's own stable semver,
its `FORK.md` naming the upstream release it carries; the range the
`WorkerPool`'s worker image `kagent.substrateWorkerPool.workerImage` names
too, and the release check below holds the two to), the connectivity
release's `pre-install,pre-upgrade` hook Job mints what the substrate chart
mounts but does not render (the CA/JWT pools, the actor-identity trust
anchor, ate-api-server's authentication config; a pool that exists is never
touched), the kagent chart creates the `WorkerPool kagent-default` and the
connectivity chart renders the one platform `Harness kagent` (the Go ADK
image by digest, `KAGENT_PROPAGATE_TOKEN`, the WorkerPool, the snapshot
location; an `Agent` runs on it by naming it in `spec.harnessRef`). The lab
installs **nothing** of it — one Helm owner, the objects an
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

The lab's Substrate values are three: the actor snapshot store is the chart's
bundled in-cluster RustFS (`substrate.rustfs.enabled: true`,
`kagent.harness.snapshotLocation: s3://ate-snapshots/kagent` — an
installation names its own bucket), the control-plane database follows
the platform Postgres (the chart's `substrate.postgres.enabled: auto`), and
the `WorkerPool`'s workers are pinned to the node's architecture.

**The architecture pin** is the one worker-pool value the lab sets. One pool
runs one CPU feature set — an actor's golden snapshot is a gVisor checkpoint,
and gVisor restores it only where the CPU offers every feature it recorded —
so the chart pins the pool with
`kagent.substrateWorkerPool.template.nodeSelector`, defaulting to the fleet's
`kubernetes.io/arch: amd64`. The lab is the arm64 installation the chart's
`UPGRADE.md` describes: on an Apple Silicon host that default matches no node,
every gVisor worker stays `Pending` and every agent turn waits for a worker
that never comes. `agentlab platform` therefore renders the architecture **the
node reports**, not the binary's — the devctl Makefile's `make build`
cross-builds amd64 even on an arm64 host, so `GOARCH` would pin the very thing
this avoids (it stays the fallback for an `agentlab render` without a cluster).
Only the architecture key travels: Helm merges the map, so the worker
resources and the `workerImage` stamp stay the chart's, and the value is
quoted — a `nodeSelector` value is a string, and the chart's
`validateWorkerPool` fails the render otherwise. Before the install
`up`/`platform` read the pin **off the rendered `WorkerPool`** — the object
the chart is about to create, so the chart's own default counts too, not just
what the lab asked for — and refuse a cluster no node of which carries it.
That is the only warning there is: the pool's workers are ate-controller's,
so the Helm release goes Ready while they sit `Pending`.

**Substrate worker nodes (`substrateNodes`)** are for the proofs a single
node cannot run: losing the node that holds a paused actor's local snapshot,
which is what a spot interruption or a node roll does on an installation.
`substrateNodes: N` in `agentlab.yaml` (0 to 3; the default 0 is the
single-node lab, rendered unchanged) adds N kind workers
`<cluster>-worker`, `<cluster>-worker2`, … labelled and tainted
`agentlab.giantswarm.io/substrate-worker=true` (`NoSchedule`). The values pin
the atelet DaemonSet (`substrate.atelet.nodeSelector`/`tolerations`) and the
`WorkerPool`'s workers (`kagent.substrateWorkerPool.template`, beside the
architecture pin) to them, and the control plane keeps no taint, so everything
else stays there. Stopping a worker therefore removes exactly that node's
ateom workers and its atelet. The setting needs the 4.x agents
(`platform.agents`) and is fixed at `kind create`: `up` refuses an existing
cluster whose nodes differ, and `agentlab down && agentlab up` applies a
change. Every worker is another kubelet and containerd on the host's memory.

The node-loss recipe, on a lab with `substrateNodes: 2`:

1. Park a turn on a decision: a Generic-chart agent with
   `muster.requireApproval: true` (the shape `a2a-test` writes) pauses its task
   at `input-required` on its first tool call. ate-api logs `Pause snapshot is
   durable` once the atelet has uploaded the copy. The atelet that logs
   `UploadPausedCheckpoint` names the node.
2. Lose that node: `docker stop <cluster>-worker2 && kubectl delete node
   <cluster>-worker2` (`KUBECONFIG=state/kubeconfig`). Within about a minute
   Substrate deregisters the node's workers and the pool rebuilds them on the
   remaining worker. The platform stays up.
3. Answer the decision. The actor resumes on the other node, and its atelet's
   `Restore` RPC carries `type: 2` with `ExternalConfig.snapshot_uri` naming
   the pause snapshot: the durable copy, restored off the dead node.

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
`AgentTemplate` and the `Agent` pairing it with the platform `Harness`, run
as a Substrate actor, a turn a `Session` driven over native gRPC through the edge as
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
`AgentTemplate` and the `Agent` pairing it with the platform's Go ADK
Harness (`kagent`, by `spec.harnessRef`) with one skill
pinned to a full commit of a public repository — `agent-self-awareness` of
giantswarm/agent-skills, the repository most of the fleet's skills come
from — and the shared muster server as its tools, waits for `Ready` on the
Harness (`--ready-timeout`, 10 min by default) and, on success, drives one
turn through the edge as the signed-in user that names the skill and
answers a fact only its `SKILL.md` has (the Harness re-emits the person's
bearer on tool calls, `KAGENT_PROPAGATE_TOKEN`, which the proof asserts).
On a failed boot it prints the evidence — the Agent's conditions and
warnings; Substrate's ActorTemplate, actor and pinned worker as the
controller's `GetSubstrateSummary` and `ListSubstrateActors` report them; the controller's, atenet's
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
`token` key holds the base64 of `<user>:<token>` for the repository's host
(git's Basic credential, which kagent's egress gateway puts after `Basic `;
`x-access-token` is the user for a GitHub token), rendered as the
source's `skills[].source.git.credentialRef` (`{name, key: token}`) — the
proof refuses to run it against a kagent whose served `AgentTemplate` CRD
lacks the field, since that kagent would prune it and fetch anonymously. The
Secret is yours to create; the proof never reads its value. A Secret that
does not exist shows as `ResolvedRefs=False` on the Harness before any
actor boots; a wrong token as a failed golden boot whose worker log carries
git's authentication error.

#### What a Session sends to the skill's host

kagent binds a credentialed source's Secret to the source's host as
`Authorization`: the golden boot sends the header with an inert placeholder
(`Basic kagent-credential-injected`) and Substrate's egress gateway
replaces the value with the Secret's. The gateway replaces only a header a
request carries, and only where the actor's egress policy holds the
binding. Whether a Session's actor holds it too, so any request its sandbox
sends to that host with an `Authorization` header leaves with the source's
token, is what the proof's second turn shows: after the skill's turn, a new
Session's agent runs, with its `bash` tool, `git ls-remote` of the source
twice from the sandbox — without `Authorization`, as git does before a
challenge, and with the placeholder — and relays the exit codes.

`--skill-fixture` puts the lab's own private git host in the path, so the
private case needs no GitHub token: the proof generates a credential,
creates the Secret `kagent/agentlab-skills-test-credential` with it, and
runs this binary (`agentlab skill-host`, hidden) in a container on the kind
network in the Harness's runtime image, which carries git. The fixture
serves one repository, `agentlab/skill.git`, with a `skills/agentlab-skill-host`
skill whose codeword the first turn asks for, over git's smart HTTP
protocol (`upload-pack` in stateless RPC mode); it answers 401 to every
request without the credential and records each request's method, path and
the kind of its `Authorization` (`none`, `secret`, `placeholder`, `other`)
and the names of any header carrying the Secret value or the placeholder —
never a value. The proof routes `skillhost.<domain>/agentlab/skill.git` to it on the
edge (an HTTPRoute in the platform namespace; pods reach the name through
the CoreDNS rewrite), whose lab-CA certificate the egress gateway trusts
(`substrate.atenetEgress.upstreamTrust`), and reads the record from this
host. The run removes the Secret, the route, the Service and the
container. Two verdict lines come of the record:

- `golden fetch: credential sent` — the requests recorded before the Agent
  was Ready carried the Secret value: the gateway bound the credential for
  the golden boot.
- `session request: no credential` — every request recorded from Ready on
  carried the Secret value in no header, and the sandbox's placeholder
  request arrived with the placeholder, not replaced: the Session's egress
  policy is compiled without the source's credential. On a kagent line that
  binds it to every actor the proof prints `FINDING: session request:
  credential sent` with the first request that carried it and exits
  non-zero; a record without the placeholder request (the gateway denied
  it, or the turn did not run it) fails the run as inconclusive.

```bash
agentlab skills-test --skill-fixture
```

The fixture run is also the `--skill-secret` path with a credential the lab
generates: the template names the proof's Secret as the source's
`credentialRef` exactly as `--skill-secret` names yours, and the run notes
the verdict `--skill-secret` reaches from the exit codes next to the
record's. No person's GitHub token enters the lab for it.

On the public default and with `--skill-secret` the fixture is not in the
path, and the second turn probes the source's own host (GitHub). Its status
line reports both exit codes and git's error line of the placeholder
request. For a public source nothing is bound, so the status is all it
says. With `--skill-secret` an accepted placeholder request is the same
finding, and the host refusing it for its credential (`Authentication
failed`) is `session request: no credential`. Any other failure, such as the
gateway refusing the connection, says nothing about the credential and fails
the run.

This repository ships a second fixture for the loader itself:
`fixtures/skills/frontmatter-fields`, a skill whose frontmatter carries
Claude Code's `user-invocable` and `argument-hint` and a top-level
`version` next to the specification's fields — the shape of a skill written
once for several harnesses, which the Go ADK's strict parser refused until
the kagent line loaded it leniently. Boot it against a full commit of this
repository's `main`:

```bash
agentlab skills-test \
  --skill-repo https://github.com/giantswarm/agentlab --skill-commit <commit> \
  --skill-path fixtures/skills/frontmatter-fields \
  --skill-question 'what is the codeword of the frontmatter fixture?' \
  --skill-expect marzipan-lantern
```

A red boot whose worker log says `field user-invocable not found in type
skill.Frontmatter` is the loader refusing the fields; green is the skill
listed, loaded and its codeword answered.

A third fixture is a plugin rather than a skill:
`fixtures/plugins/selection`, an Agent Plugins package (`plugin.json` at its
root) with two skills, `selected-codeword` (codeword **saffron-viaduct**)
and `unselected-codeword` (codeword **pewter-orchard**). An agent with the
plugin and only `selected-codeword` in its `plugins[].skills` must answer
the first codeword and not know the second: the package materialises only
the skills an agent selects. With this repository among the portal's and
agent-manager's skill repositories (a `platform.valuesFiles` overlay), the
Dev Portal's wizard lists the package on its skills step with both skills
as checkboxes.

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

The agentgateway line closes the third gate without giving it up: it carries
upstream agentgateway #3237 (giantswarm/agentgateway-upstream#4) — every
egress CONNECT authorized against ate-api, the actor's UID and then its state
— and admits `RESUMING` next to `RUNNING` (SUSPENDED, PAUSED, CRASHED and
DELETING stay refused). giantswarm/substrate#7 pins it for `atenet-router`
and `atenet-egress`, and giantswarm/substrate#9 declares the actor
authorization where that dataplane reads it — #3237 made it a frontend policy
on the CONNECT, no longer a route policy on the inner listener (with the old
shape the dataplane refused its config and the first roll never became
ready). With the check in place the proof passes on both halves: Ready after
15s on Harness kagent, the turn as admin@lab.local answers "klaus-gateway"
from the skill, nothing left behind; platform-test and agents-test green on
the same lab. Every skill-carrying agent of the fleet needs all three gates
open; the upstream exits of the two patches are tracked on
giantswarm/giantswarm#37742 (row 8).

The three lines release stable semver of their own, decoupled from the
upstream versions their `FORK.md`s record — agent-platform 4.49.0 pins kagent
and Substrate `>=1.0.0 <1.1.0` and the agentgateway line's `2.0.0`, ranges
without a `-0` bound (Flux's semver evaluates prereleases for the whole range
once a bound carries one, so `<1.1.0-0` would admit the lines' `-dev.`
builds). A patch of a line is carried patches or a rebuild on the same
upstream pin; a re-pin onto another upstream release is at least a minor. The
Substrate line sits on kagent-dev/substrate v0.0.29, whose chart declares the
check as agentgateway#3318's `substrateEgressActorResolution` frontend
policy, and its router and egress gateway on the agentgateway line — upstream
agentgateway `main` past `9f9744cf` (the substrate ingress header of #3409
the v0.0.29 router speaks), still admitting a `RESUMING` actor; the kagent
line on upstream `main` `800015de` (kagent-dev/kagent#2802: the a2a gateway
addresses an actor by the `ate-target-actor` header, which only a router from
v0.0.28 on knows — the three move together).

### The egress proof

An agent's egress on the kagent API v2 line is its compiled destinations —
the model, its MCP servers (muster), its skill sources, telemetry — plus the
HTTP(S) origins its Agent's `spec.egress` declares (`agent.egress` of the
Generic chart, `egress` of agent-manager's `create_agent`); Substrate's
egress gateway (`atenet-egress`) enforces the actor's policy.
`agentlab egress-test [email] [--origin <origin>] [--model-config <name>]`
proves it from inside the actor: one agent on the platform Harness, created
through agent-manager as the person with the read-only toolset and one
declared origin (default `https://example.com`), and one turn whose `bash`
tool runs a probe script. Every stage is judged from Substrate's egress
gateway's own record of the turn's actor (`session-<Session id>`), read from
the `agentgateway` container of `deploy/atenet-egress` and cited in the
verdict: its access-log `request` lines (`endpoint=` when it forwarded the
request, `error=` when it answered itself), its `CONNECT tunnel terminated`
lines and its `upstream tcp connected` lines (debug, the level Substrate runs
the gateway at). The agent's reply is no evidence: its `AGENTLAB_PROBE` lines
are shown beside each verdict and decide nothing, so a reply that claims a
reach the gateway refused, or denies one it made, changes no outcome.

| Stage | What passes |
|---|---|
| create | `create_agent` with `egress: [<origin>]` as the person; Ready; the Agent's `spec.egress` exactly that list |
| probe turn | the turn completes |
| declared | the gateway forwarded the actor's `GET` to the declared origin |
| undeclared | the gateway answered the actor's request to `https://proxy.golang.org/`, outside every list, with its own 403 and the reason `actor egress policy denied`, and forwarded none |
| muster | the gateway forwarded the probe's `GET` to muster's URL from the agent's `RemoteMCPServer`: a compiled destination needs no egress entry |
| raw tcp | the gateway took the actor's tunnel to the address the lab host resolves `github.com` to, port 22, and opened no upstream connection to any port-22 tunnel's target nor forwarded a request to it. No tunnel recorded is no evidence and fails; an upstream connection fails, naming the destination |

`--without-egress` is the control: the agent is created with no egress
entry and the same origin probed; its `declared` stage fails, so the reach is
the declaration's. The agent and its Session are removed on every exit path.

### The coding agents proof

A coding agent is an agent whose Agent names a coding Harness: a coding CLI
run headless in a Substrate actor by the kagent line's image of its runtime
— `claude` (Claude Code, `claude-harness`, calling muster as the person who
sent the turn), `codex` (OpenAI's Codex CLI, `codex-harness`, its MCP
credentials on upstream's Secret-backed path) or `omp` (oh-my-pi, a BYO
Harness on the kagent line's `omp-harness` adapter, the same Secret-backed
path, on the Anthropic ModelConfig claude uses). An installation turns coding
agents on through the meta chart's `kagent.harnesses` alone, and the lab
does the same: with `platform.coding` (`agentlab configure --coding`,
agent-platform 4.110.0 or newer) `agentlab platform` renders one coding
Harness per runtime of `platform.coding.harnesses`
(`--coding-harnesses claude,codex,omp`; default `claude` alone), named after it,
snapshots under `s3://ate-snapshots/<runtime>`. `claude` carries
`KAGENT_PROPAGATE_TOKEN` (the caller's bearer reaches MCP servers through
the harness's loopback forwarder, never the process environment) and no turn
limits (the kagent line dropped `spec.claude.limits`; a lab whose Harnesses
still carry them has them removed by `agentlab platform` before the chart is
applied). `codex` needs a kagent line whose chart takes `runtime: codex`, and
a ModelConfig codex speaks (the OpenAI Responses API, or Bedrock `gpt-*`).
`omp` renders a `byo` entry (display name `oh-my-pi`, the title the Dev
Portal shows), which needs a kagent line whose chart takes
`runtime: byo`, on the `omp-harness` image `platform.coding.images.omp`
names by digest (`agentlab configure --coding-images omp=<image>`; the
release's chart stamps it as `runtimeImages.ompHarness.digest`), a dev
image (`platform.devImages` `omp-harness`) winning over it.

`coding-test --harness codex` runs on the in-cluster mockllm unless
`--model-config` names another model: a mock OpenAI Responses API in the
shape of kagent's mockllm (its matching and its stream's events, served by
agentlab itself, no SDK), scripted for the proof's turns, so the proof
reaches its verdict on a lab with no OpenAI or Bedrock account. It runs
the way a proof's fake runs: `agentlab mockllm` in a container on the kind
network, reached by the cluster as the Service `agentlab-mockllm` in the
platform namespace, which the run's ModelConfig `agentlab-mockllm` (the
`OpenAI` provider, `gpt-5.2-codex`, `apiFormat: responses`, a placeholder
key) names as its base URL; the run removes all three at its end. The script
answers each prompt with the tool call it asks for (the probe script, the
MCP tool through codex's tool search, the heartbeat and its read-back, the
code word's file, its read-back)
and a fixed reply once the tool's output is back. A shell command goes to
the shell tool the runtime offers: `shell_command`, or on codex 0.163 and
later (kagent 1.16) the unified `exec_command`, whose command runs past the
call's 30 s yield; the script then waits for it with an empty `write_stdin`
on its session until it exits, and replies after its final output. The sandbox stages, the
MCP call and the resume's file judge those outputs, what the sandbox
printed and the tool answered, as the harness reports them on the turn's
tool results; the code word the resumed turn recalls is the script's. The
`model` stage closes the run: every model call answered by the script, none
outside it. A failed run quotes the mockllm's last requests: the tools each
offered the model and its last input item, what the script matched or did
not.

A real model is named with `--model-config`: a hosted OpenAI Responses API
model (an `OpenAI` extraModels entry with `apiFormat: responses` and its key
in `apiKeyEnv`, or one on a ChatGPT login, `credentialType: chatgptOAuth`:
[Extra model configs](models.md#extra-model-configs-self-hosted-openrouter-gemini-openai)). On every model the stages judge the probe's lines from the
turn's tool results first, what the commands printed; the reply fills only
the readings no command prints (what the session loaded, the code word), so a
relay that drops or rewords a line changes no verdict. The sandbox prompt asks
for a command timeout of 5 minutes, since codex's default of seconds cuts the
script at its `/data` token search; a reading neither the outputs nor the
reply carry fails its stage as missing. No host model passes every stage
yet:

| Host model (Ollama) | Memory loaded | On the codex proof |
|---|---|---|
| `qwen3-coder:30b` (30b-a3b, Q4_K_M, 18 GB) | 21 GiB | sandbox turn (1–4 min), credentials and DNS canary pass; the egress readings it relays are not the probe's |
| `gpt-oss:20b` (MXFP4, 13 GB) | 16 GiB | Ollama answers some `/v1/responses` calls with 500 and codex fails the turn; otherwise the relay drops lines |
| `qwen3.5:9b` (6.6 GB) | not measured | every turn runs; the relay fails at a different stage each run |

`agentlab configure` adds no host model unasked: each of these needs more
memory than a lab proof's host model gets. With the codex Harness rendered,
`configure --codex-host-model` wires the first of `qwen3-coder:30b` and
`gpt-oss:20b` the host's Ollama has downloaded as the extraModels entry
`codex-host` (the `OpenAI` provider on Ollama's `/v1`, `apiFormat:
responses`, keyless), which `--model-config codex-host` runs the proof on
under the 40m host model turn deadline; a failed sandbox stage quotes the
agent's reply.

```sh
agentlab configure --defaults --coding --coding-harnesses claude,codex
agentlab platform
agentlab coding-test --harness codex                            # the mockllm
agentlab configure --defaults --codex-host-model                # opt in to a host model
agentlab coding-test --harness codex --model-config codex-host  # on it
```

The workspaces proof's claude Session runs on the same `claude` Harness,
which the lab also renders with the workspaces on and coding off.

`agentlab coding-test [email] [--harness claude|codex|omp] [--model-config
<name>] [--muster-tools meta|<tool,…>]` is the proof, and the floor a coding agent rests on, on one Harness
per run (default `claude`). Every stage runs on the lines the lab pins, so
it repeats on every release candidate. The stages marked *claude* prove
Claude Code's own mechanisms and run on `claude` alone; *codex, omp* marks
the one that runs on both of those:

| Stage | What passes |
|---|---|
| harnesses | the Harness Ready; on `claude` no `spec.claude.limits` on it |
| write-time checks | agent-manager's `validate_agent` on the Harness accepts `[preset:read-only, tool:x_agent-manager_create_agent]` (which muster tools a coding agent uses is the person's choice) and refuses an origin outside the Harness's list; an agent-manager below 1.14.0 refuses the write tool, and the proof expects that refusal on it |
| plugins accepted | `validate_agent` on every Harness of the namespace (`kagent` and the coding ones) accepts `plugins[]` with the upstream example package `https://github.com/agentplugins/agent-plugins-example.git` pinned by commit (an Agent Plugins package: a root `plugin.json`, one skill) and the skill `migrate-agent-plugin` selected: no plugin error; errors of other rules, such as a ModelConfig the Harness's runtime does not speak, are quoted only |
| claude-format refused on codex | a public Claude-format package, Anthropic's `explanatory-output-style` (`anthropics/claude-code` `plugins/explanatory-output-style` pinned by commit: a `.claude-plugin/plugin.json`, no root one, a SessionStart hook in shell), on `codex`: `validate_agent` and `create_agent` refuse it as Claude-format, naming `plugin.json` at the plugin root, and no HelmRelease is written. The writes name a Responses API ModelConfig (the run's on `codex`, the lab's codex model, else the mockllm's, placed for the run), so the plugin check is the rule left to answer. SKIP, naming the fix, where agent-manager could not read the manifests (GitHub's anonymous window spent) or the lab renders no `codex` Harness |
| claude-format refused on kagent | SKIP until giantswarm/giantswarm#38689 is released: agent-manager runs no plugin check on `kagent` before it, so a Claude-format git plugin passes the write there and fails at its golden boot |
| create | `create_agent` on the Harness with that toolset (`preset:read-only` alone below agent-manager 1.14.0) and the example package, `migrate-agent-plugin` selected (on `claude` with `explanatory-output-style` beside it), as the person; Ready; its `RemoteMCPServer` Accepted and sending the toolset as `X-Muster-Toolset`; the Agent's `spec.egress` is the Harness's origin list agent-manager writes |
| secret-backed credential (*codex, omp*) | every header the agent's `RemoteMCPServer` takes from a Secret names a Secret key that is there: upstream's Secret-backed path, the controller handing the runtime kagent's placeholder and the egress gateway setting the value; SKIP, naming the headers, on a platform line whose binding takes no header from a Secret (no released component writes one yet) |
| uid split (*claude*) | the agent's commands run as uid 65532; the root harness's `/proc/<pid>/environ` and `mem` refuse it |
| forwarder paths (*claude*) | the forwarder answers 400 to `../..`, `%2e%2e` and `..%2f`, 404 for a server it does not front (401 where the MCP config holds only kagent's placeholder: the probe has no loopback token to send, and the forwarder refuses before it matches a server), 401 without its token |
| write tool refused (*claude*) | `x_agent-manager_delete_agent`, called through the forwarder with the turn's bearer, refused by muster as outside the toolset |
| credentials | no forge token or bearer in the environment, `/data` or `/workspace` (one admitted: Claude Code's signed policy cache `/data/claude/policy-limits.json.signature.json`, while its every match is a JWS of header `typ: cc-signed-cache+jws`, Anthropic's signature over the cached policy that authenticates nothing); the model key kagent's placeholder, or unset where the runtime keeps it out of the shell's environment (omp); on `codex` every `KAGENT_CODEX_MCP_CREDENTIAL_*` the sandbox sees the placeholder too, on `omp` every `KAGENT_CREDENTIAL_*` |
| dns canary | a recorded state, not a gate: `example.org` through the cluster resolver and directly at 1.1.1.1 |
| egress | `proxy.golang.org` denied 403 at the egress gate; `api.github.com`, one of the Agent's origins, reached |
| plugin skills | the agent has the selected plugin skill and no other: on a real model its own listing of the skills it can invoke (`migrate-agent-plugin`); on `codex` on the mockllm the requests the runtime sent the model carry `migrate-agent-plugin` (the mockllm records which watched names each request carries anywhere: what the runtime presented to the model) |
| plugin hook (*claude*) | `explanatory-output-style` loaded whole: the agent quotes the context its SessionStart hook added (the explanatory output style) |
| harness change refused (*claude*) | `update_agent` moving the agent, which binds `explanatory-output-style`, to `codex` (with a Responses API ModelConfig) is refused as Claude-format; the HelmRelease's resourceVersion is unchanged and the Agent still Ready on `claude`; SKIP where no `codex` Harness is rendered |
| harness change to kagent refused (*claude*) | SKIP until giantswarm/giantswarm#38689 is released; the move is not attempted, since before it agent-manager would write a revision that never boots |
| mcp call | the agent calls `x_agent-manager_list_agents` through its MCP binding (muster's `call_tool`, which runs every catalogue tool; on `codex` the mockllm's script calls it so) and finds itself listed; SKIP where the secret-backed credential stage skipped and the call answered nothing else: the binding carries no credential on that line, so muster refuses the runtime's MCP connection (401) and the runtime has none of the binding's tools |
| turn gap | one turn starts a detached heartbeat in the sandbox: every 3 s it appends the time and muster's status for an MCP initialize with a placeholder `Authorization`; after 90 s without a turn the next turn reads it back. It ticked in its own turn, not once from 20 s after that turn's answer (Substrate suspends the actor once a turn settles) to the next turn's send, and again in the next turn. The egress gateway checks no turn: that nothing in the sandbox runs between turns is what keeps a call outside a turn from reaching muster |
| instruction fixture (*claude*) | a record: what Claude Code loads from a repository's `CLAUDE.md`, `AGENTS.md`, `.claude/` settings, hook, skill, subagent and command and `.mcp.json` on the pinned release |
| suspend and resume | the Session's file and conversation survive the snapshot |
| usage | the resumed turn's task status carries its usage |
| share refused | a read-write share of the coding Session is refused: a collaborator's turn would run under what the first sender's turns left behind (git hooks, Makefiles, background processes) |
| empty selection | `update_agent` with the example package's selection `[]`: the AgentTemplate carries it, the Agent is Ready on a new revision, and a new Session's agent has no `migrate-agent-plugin` (its listing on a real model; on the mockllm none of the new Session's requests carries it) |
| model (*codex on the mockllm*) | the mockllm answered every model call of the turns from its script, none outside it |

The sandbox stages read one turn: the agent's shell tool runs a POSIX script
and relays its `AGENTLAB_PROBE` lines, and each stage judges its own. The
script never prints a credential: the forwarder's loopback token stays in a
shell variable, and the token search prints counts. The DNS canary and the
instruction fixture are records because the floor accepts both as known
residuals: DNS leaves through the cluster resolver until a resolver answers
only the allowlist's names, and a workspace is an instruction source for
Claude Code, as on a developer's laptop. Writing to a forge with `git` from a workspace, as the person, is the
workspaces proof's GitLab part (`workspaces-test --part gitlab`), which
pushes from a Session through the egress gateway with the person's
credential.

Each turn has a deadline by the model the agent runs on, which the turn
prints as it starts: 40 minutes on a model a host model server serves (a
ModelConfig model-manager wired, or one whose endpoint is
`host.agentlab.internal`), where a coding runtime's calls take a minute and
more each on the lab machine's hardware, 10 minutes on a hosted model.
`--turn-timeout` sets it by hand; `--ready-timeout` bounds the boots and the
waits outside a turn.

### The Substrate step proof (conversations across a Substrate release)

`agentlab substrate-step-test --to <release>` proves that agents keep their
conversations and their durable volumes when the lab steps from the
Substrate release the chart installs to a newer one under them — the step a
release that changes the snapshot layout has to survive (1.7 to 1.8: one tar
per durable volume instead of one for all), and the step a release that keeps
the layout takes with its goldens kept (1.8.1 to 1.8.2). A Session's actor is
suspended to a snapshot between turns and resumed from it; every new Session
starts from its template's golden snapshot. Both are written by the release
before the step and read by the one after it. One headless run, four steps,
each judged `PASS` or `FAIL` with its cause:

1. **before** — the Substrate the lab runs: the atelet's, the WorkerPool's
   worker and `ate-api-server`'s images, the release the atelet's tag is. A
   lab already on `--to`'s release (or a newer one) is refused: the proof
   starts on the chart's Substrate range as released.
2. **agent** — an `AgentTemplate` on the platform Harness (with the skills proof's public skill: the Go ADK gives an agent its `bash` tool only with skills) and its `Agent`
   boot (a golden in the old layout); a conversation of three turns: a
   random codeword, the agent's `bash` tool writing a random marker to
   `/data/agentlab-substrate-step/marker` (`/data` is the actor's durable
   directory), the codeword asked back; then the Session is suspended and
   the template's golden tag recorded.
3. **step** — `state/substrate-step-values.yaml` pins the `substrate` and
   `substrate-crds` components to `--to` exactly (which moves the control
   plane, the atelet and the WorkerPool's worker image together), added after
   the lab's own `platform.valuesFiles` for this run only, with the
   `--step-dev-image <target>=<image>` entries over the lab's `devImages`
   (an `ate-api-server` build of a change no release carries yet); the
   platform is upgraded in place as `agentlab platform` does, and the proof
   waits for every worker pod to run the new worker image. `agentlab.yaml`
   is never written. **The drain case** (on by default, `--no-drain` skips
   it) forces the roll of a multi-worker pool, which a one-node lab is too
   quick to meet: from the moment an `ate-api-server` pod of the candidate is
   Ready, every worker pod of the WorkerPool is deleted every 8 s for a
   minute, so the golden re-take and the resume land on workers that drain,
   go away or have no ateom socket yet; the suspended Session's resume is
   issued during the drain, as soon as no `ate-api-server` pod of the release
   before is left: the kagent controller holds one connection to the ate-api
   Service for its lifetime, which the rollout moves only when the pod behind
   it goes, and a resume issued while the release before's pods still serve
   restores under the release before. A drain that ends with them still there
   fails, naming them. The resume is issued again, as a client would, while
   kagent answers `Unavailable` (its connection moving, a resume still
   deferred at the controller's own bound) or `Aborted` (the operation still
   pending); `FailedPrecondition`, the runtime lost, ends it. The `drain:`
   lines name the `ate-api-server` pod that began it, its rounds and every
   worker pod it deleted, when the release before's pods were gone and each
   resume attempt's answer.
   **restore** — the candidate's own account, read before the lab steps back:
   the RPC log of every `ate-api-server` pod of the candidate must show a
   `ResumeActor` of the Session's actor that arrived while the drain ran (the
   line's `time` less its `elapsed-time`); the `PASS` line names each one by
   its offset from the drain's start and how it ended (`RUNNING`, or the
   error — a restore the drain cut answers `Unavailable` and comes back as
   another), then the resume's own account (issued when, attempts, answered
   `READY`). None during the drain fails the stage, naming the ones outside
   it or none at all; a resume that reached the candidate and ended in an
   error fails it with the actor's state and crash.
4. **resume** — the suspended Session resumes; its next turn must answer the
   codeword of the first turn (the conversation) and `cat` the marker back
   (the volume). A failed resume names its cause from the Session's failure
   and the crash messages of the agent's actors in ate-api; Substrate 1.8.0's
   refusal of a snapshot in the old layout is named as the restore refusal.
   **golden** — the template's golden must be in the snapshot layout the new
   release writes within `--retake-timeout` (default 10m): taken again on a
   layout change, kept otherwise — Substrate records the layout a golden was
   written in (`GoldenSnapshotStatus.snapshot_layout`, from 1.8.1-rc.4 on)
   and takes a golden again only in an older one, and the proof knows the
   layout each release writes (`currentSnapshotLayout`, a table Substrate
   exposes on no API; a release with a new layout is added there); then a new
   Session of the same agent answers one turn.

In the drain case the stages `restore` and `retake` run between the step and
the resume, so a failed resume still leaves their verdicts in the output;
`retake`: every ActorTemplate
of the lab that had a golden before the step must hold one in that layout
within `--retake-timeout` — its own, kept, or one taken again — and none may
record a terminal `GoldenSnapshotStatus` error — the `GoldenActorCrashed` a
drain leaves on every template of a Substrate without the golden take retry.
Each poll re-reads ate-api's `ListActorTemplates`: a template no longer listed
is a superseded revision kagent deleted during the run, named in the PASS line
and not judged.

The agent is removed — through kagent, then whatever of it ate-api still
holds (its ActorTemplate, an actor that crashed on a restore), deleted in
ate-api itself, an actor still `DELETING` deleted again until ate-api lets go
of it: the next run's agent compiles to the same template name — and
the lab returns to the chart's Substrate (`--stay`
leaves it on `--to`) — on a failure too, so the next run starts where this
one did. The run starts with the same cleanup; what an earlier run left and
it cannot remove is noted and never fails this run, whose own leftovers do.
The return to a release before 1.9 removes the rustfs Deployment's
`serviceAccountName` and its deprecated alias `serviceAccount` first: the
1.8 chart drops the ServiceAccount, and the apiserver would fill the name
back from the alias, stalling the rollout until the HelmRelease rolls back.
The verdict names the Substrate versions before and after, the
resumed conversation and the volume check:

```bash
agentlab substrate-step-test --to 1.8.0         # 1.8.0 as released: FAIL at resume, the restore refusal
agentlab substrate-step-test --to 1.8.1-rc.4    # the conversion and the re-take: PASS
agentlab substrate-step-test --to 1.8.2         # the patch step: the goldens kept, the resume during the drain: PASS
```

The run on the release before a fix is the regression check: it must fail at
step 4 with its cause, or the check would not catch the fix's loss.

**Where it runs.** The proof is the first step of the lab round of every
Substrate and kagent release candidate that moves the Substrate the platform
pins: the candidate becomes `--to` (its kagent counterpart through
`platform.valuesFiles` or `platform.devImages`), on a lab on the platform's
current release. It is not a CircleCI job of this repository: its turns are
model turns (the conversation and the file are the agent's work), which a
pipeline could only run with a model key in a CircleCI context this
repository's pipeline does not have, and its subject is a release of another
line, not a change of agentlab. The unit tests of every piece it adds run in
`make test`.

### The kagent step proof (conversations across a kagent step)

`agentlab kagent-step-test` proves that an agent and its conversation
survive a step of the kagent line under them, the way
`substrate-step-test` proves a Substrate step. It names the step in one of
two shapes, or both:

- `--to <kagent release>` pins the `kagent` and `kagent-crds` components to
  that exact release of the kagent line (`1.14.0`, `1.15.1-rc.2`) through a
  values overlay of the run's own (`state/kagent-step-values.yaml`), on the
  chart the lab runs: a step within the released line, or to a kagent
  release candidate. The lab must run an older kagent before it.
- `--chart <agent-platform release>` moves the meta chart to a candidate
  release (`4.145.1-rc.6`) with the values that candidate carries: the step
  a platform candidate takes when it keeps Substrate, which
  `substrate-step-test --to` cannot express. A candidate whose kagent range
  moves steps kagent under the conversation; one that keeps kagent and
  Substrate is a chart-only step (its `step` PASS line says so), and the
  same resume, turn, suspend and delete prove the conversation survives the
  chart's upgrade. A candidate that steps kagent back fails the step stage.

The run, on the static binary (no scratch driver): the kagent line before
the step read off the meta chart, the kagent HelmRelease's applied chart and
the controller's image; an agent on the platform Harness with a
conversation of one turn (a random codeword), suspended; the platform
upgraded in place onto the step and the kagent controller rolled out; then
the conversation from before the step resumed (`resume`), a turn on it that
must answer the codeword (`turn`), a suspend and a resume more on the new
controller (`suspend`), and the conversation and the agent deleted, the
Session gone and Substrate holding nothing of the agent (`delete`). Each
PASS line names what it proved; a failure's FAIL line starts with the stage
that failed — `step`, `resume`, `turn`, `suspend` or `delete` — and its
cause. The lab returns to its own configuration afterwards (the overlay
removed, `agentlab.yaml`'s chart upgraded back onto); `--stay` leaves it on
the step. `--step-dev-image <target>=<image>` adds dev images for the step
alone (a `kagent` controller build), `--ready-timeout` bounds the golden
boot, `--model-config` names the ModelConfig.

```bash
agentlab kagent-step-test --to 1.14.0              # a step within the released kagent line
agentlab kagent-step-test --chart 4.145.1-rc.6     # a platform candidate that steps kagent 1.13 -> 1.15
agentlab kagent-step-test --chart 4.146.0-rc.1     # a chart-only candidate: kagent and Substrate kept
```

**Where it runs.** The proof is the lab round of every kagent release
candidate and of every agent-platform candidate that does not move the
Substrate it pins (one that does takes `substrate-step-test`), on a lab on
the platform's current release: a promoter's suspended-conversation resume
proof of a chart-only candidate is `kagent-step-test --chart <rc>`, no turn
by hand. Like the Substrate step proof it is
not a CircleCI job of this repository: its turns are model turns. The unit
tests of its pieces run in `make test`.

### The Substrate tunnel-close proof (gRPC to an HTTP/1.1-only actor)

`agentlab substrate-tunnel-test` proves that a gRPC call through
atenet-router to an actor that speaks HTTP/1.1 only fails at once instead of
waiting out its client's timeout. atenet-router reaches an actor through a
CONNECT tunnel to atunnel in the worker (the `ateom-gvisor` image), which
relays it to the actor's port 80. Plain HTTP the router downgrades to
HTTP/1.1 for such an actor; a gRPC-shaped request it carries as HTTP/2, and
the actor closes on the HTTP/2 preface. A relay that ends the tunnel with the
actor's connection lets the router fail the call (gRPC `Unavailable`, or HTTP
502) in milliseconds; one that waits for the router to stop sending leaves
the router waiting for the actor's SETTINGS frame until the client gives up.

The agents' actors all speak h2c, so the proof brings its own, with no local
build: an ActorTemplate `agentlab-substrate-tunnel` in the `kagent` atespace
on the platform Harness's worker pool and snapshot storage, one container on
the lab's alpine (pinned by digest, the image the workspaces proof's actors
run) whose busybox `nc` serves `GET /readyz` over HTTP/1.1 and closes on the
preface without an answer, its input unread. The client reaches atenet-router's
HTTP port through a port-forward and names the actor in the `ate-target-actor`
header. Four stages, each judged `PASS` or `FAIL`:

1. **substrate** — the Substrate the run is on: the atelet's and
   `ate-api-server`'s images, and the image the worker pods run, where atunnel
   relays the tunnel — read off the pods, since the WorkerPool's
   `spec.workerImage` stays at the chart's floor while the controller runs the
   pods on its own release (condition `WorkerImageOverridden`).
   `--substrate <release>` pins the `substrate` and `substrate-crds`
   components to the release for the run through a values overlay
   (`state/substrate-tunnel-values.yaml`), the way the step proof steps, older
   or newer than the lab's, unless the lab and its worker pods run it already;
   the lab returns to the chart's Substrate at the end (`--stay` leaves it).
2. **actor** — the template's golden snapshot, the actor created and resumed.
3. **route** — `GET /readyz` over HTTP/1.1 answers 200 through the router
   (waited for up to `--ready-timeout`, default 5m), then over h2c: HTTP/2.0
   200, downgraded to HTTP/1.1 at the actor.
4. **tunnel-close** — `POST` with `content-type: application/grpc` over h2c,
   the client's timeout `--client-timeout` (30s): it must fail loudly (HTTP
   502, or 200 with `grpc-status: 14`) within `--promptly` (5s). A call that
   ends with the client's timeout is the tunnel the actor's close did not end.

The actor and its template are removed by the same run, and an earlier run's
first. On a lab the case fails on Substrate 1.11.3 (the call waits the
full 30s) and passes on 1.11.4, which carries the relay change.

### The Swarmgeist proof (klaus-gateway on kagent API v2)

`agentlab klaus-gateway-test` is the lab carrier of
[klaus-gateway](https://github.com/giantswarm/klaus-gateway) (Swarmgeist,
the fleet's Slack bridge) on kagent API v2: A2A v1 over gRPC through the
agentgateway edge, the roster from `ListAgents`, one `Session` per Slack
thread kept in the gateway's routing store, human-in-the-loop as
kagent's HITL extension, a stop as `CancelTask` — every call made as the
person behind the turn, whose Dex id_token the gateway forwards and validates
nowhere itself. Slack is the gateway's only channel and no workspace answers
the lab, so the proof drives the Slack adapter headlessly with what the
gateway ships for it: its Web API base (`--slack-api-base`) points at a fake
Slack Web API the proof serves, which answers `auth.test` (the bot user),
`users.info` (the people's e-mails) and `{"ok":true,"ts":…}` for the rest, and
records every `chat.postMessage`, `chat.postEphemeral`, `chat.update`,
`chat.delete` and `chat.startStream`/`appendStream`/`stopStream` per thread —
the assertions read what the thread would show. The people's messages are
Events API callbacks to `POST /channels/slack/events` and their button
clicks Block Kit payloads to `POST /channels/slack/interactions`, signed with
the run's signing secret (`v0=` HMAC-SHA256 over `v0:<ts>:<body>`). When a
turn is over the gateway's log says so (`turn_complete`, with its outcome and
task), next to `session_bound` and `turn_dispatch`.

**The person's identity.** The Slack channel forwards only a linked person's
token — there is no service-account fallback for it — and a real sign-in
cannot complete in the lab ([klaus-gateway](klaus-gateway.md), "What the lab
cannot prove"). The proof links the person the way a sign-in leaves them
linked: before the gateway starts, it writes a record into the gateway's
bolt link store through the gateway's own store package
(`pkg/auth/musterlink`) with the run's store key — the Dex subject and
e-mail, the lab user's Dex id_token as the link's cached token, its `exp` as
the expiry, a placeholder refresh token. `TokenFor` serves a cached,
unexpired id_token without calling muster, so the gateway runs unmodified.
The token must have an hour left at the start, and a `token_refresh` record
anywhere in the run fails the proof.

**Deployment shape.** The gateway runs **out of cluster, on the host** —
the image on the host network by default
(`--gateway-image`; by default the release the lab's meta chart runs as its
`klaus-gateway` component — the version its HelmRelease applied, or with the
component off the newest release the chart's `components.klaus-gateway`
range admits — so one chart bump moves both shapes; the proof prints which
image and why), or a
local build (`--gateway-binary`, the proof of a branch) — with `a2a.url` =
the lab's public gRPC target `grpcs://agentgateway.<domain>:<gatewayPort>`
(TLS with the lab CA from `certs/ca.crt`; the JWT `Strict` policy of the
controller `GRPCRoute` validates the forwarded token at the edge),
`a2a.namespace: kagent`, the fixture as the default agent, Slack in `events`
mode on the fake, OBO on the seeded bolt link store, a bolt routing store —
all in a run directory (`--run-dir` keeps it, together with the keys and the
gateway's log). The Slack endpoints listen on `127.0.0.1:18090` on the
first lab, ten ports higher per port a lab's `apiServerPort` sits above
6443, so two labs never share one (`--gateway-port`; the admin endpoints
take the next port, the fake the one after). The image shape's container is
`<clusterName>-klaus-gateway-test`; an uncached image is pulled before the
30 s readiness wait starts, and the container is removed on every exit,
a readiness timeout included. This is the leg the meta chart's in-cluster component
(`components.klaus-gateway`, `klausGateway.a2a.url` = the in-cluster
`grpc://agentgateway.agent-platform.svc.cluster.local:8080`) does not
exercise: the public route with TLS and the JWT policy. The component is the
other shape, `platform.klausGateway` — on a lab that runs it, the proof
continues on the component (assertions 6–9 below), whose pods call the same
fake through the Service `agent-platform/agentlab-slack-api`: then the fake
runs as a container on the `kind` network (`agentlab slack-fake` in the
lab's probe image; a static Linux agentlab, `--slack-fake-binary`), and a
probe pod fetches it through the Service first. The gateway forwards the
person's token and talks to no Dex, so it needs no `dex-localhost` bridge.

**Fixtures** (in `kagent`, deleted by the same run, leftovers removed first):
`AgentTemplate agentlab-klaus-gateway-test` in the Generic chart shape — the
`ui.giantswarm.io/display-name` and `ui.giantswarm.io/icon-url` annotations,
`default-model-config` (`--model-config`) — with the `Agent` of the same name
pairing it with the Harness (`--harness`, the annotations repeated on the
Agent), and its own muster carrier
`RemoteMCPServer agentlab-klaus-gateway-test` (muster's in-cluster URL,
`X-Muster-Toolset: preset:read-only`, discovery off, never an Authorization
header) bound with **`requireApproval: true`**, so every muster call pauses
for a decision (the Generic chart renders the same binding from
`muster.requireApproval` since agent 1.1.0; the proof applies the pair
directly so it depends on no chart release resolving in the lab); and
`AgentTemplate agentlab-klaus-gateway-test-unadmitted` with an `Agent` whose
`harnessRef` names a Harness that does not exist, so no Harness runs it.

**Assertions**:

1. **Discovery**: `@bot agents` posts the roster, which lists the
   fixture's display name and not the unadmitted template's; the fixture
   row's Select button (`agent_select`) opens the agent picker
   (`views.open`, callback `ask_agent`) in the roster's thread, which offers
   the fixture and not the unadmitted template; the picker submitted with
   the unadmitted template selected (one that left the roster between the
   listing and the submit) is answered, to the submitter alone, with
   `… cannot start a conversation right now: no Harness admits this
   AgentTemplate. Nothing was started.` and the controller lists no
   `Session` of it; a person with no link is shown a Sign in button
   (`obo_sign_in`, ephemeral) to the gateway's `/auth/slack/link` and
   reaches no controller.
2. **One turn**: the thread's first mention streams the answer
   (`chat.startStream` … `stopStream`) under the Agent's display name and
   icon; the gateway's `session_bound` record names the `Session`, and the
   controller (`ListSessions` narrowed to the Agent, as the user) lists
   exactly that one; `turn_dispatch` names the fixture, the
   person's e-mail and the link's subject. A cold worker's first resume may
   hit Substrate's ResumeActor deadline once; a thread's first turn is
   retried once, visibly.
3. **HITL**: a tool-using question pauses (`turn_complete` outcome
   `input_required`) with an approval card; each pause is
   `TASK_STATE_INPUT_REQUIRED` at the controller (`GetTask`) on the task the
   card's buttons name, each Approve click (`hitl_approve`) rewrites the card
   to `Approved by <@…>` and resumes the same task (`filter_tools`, then
   `call_tool`), the task ends `TASK_STATE_COMPLETED` and `ListTasks` shows
   nothing of the session left at input-required. **Attribution**: muster's
   log since the turn began carries the `forwarded_id_token_accepted` audit
   record with the user's email and `tools/call request` lines under the
   token's subject. **Deny**: in a second thread (a fresh session: the first
   one's conversation already holds the count) each Deny click
   (`hitl_deny`) rewrites the card to `Denied by <@…>`; the task ends in a
   terminal state and none of the task's tools reaches muster as the person:
   no `tools/call request` for a meta-tool its `tool_approval_request`s
   named, no `tool dispatched` for a tool its `call_tool` named. The
   person's other muster calls meanwhile (a Dev Portal tab polling as them)
   do not count.
4. **stop**: once a long answer streams, a `stop` reply in the thread ends
   the turn `canceled`, the thread is told `Stopped.`, the new task is
   `TASK_STATE_CANCELED` at the controller, and the thread takes a following
   turn.
5. **Restart**: the gateway is stopped and started again on the same stores;
   the next mention recalls the first turn's word, no new `session_bound`
   record is written, and the controller lists the thread's `Session`
   and the Deny thread's, nothing else. No `token_refresh` record in the run.
   **5b. Restart mid-turn**: in the same thread a long answer (every number
   to five hundred in words) starts streaming; the gateway is stopped (it
   records `task_left_running` and the thread gets the notice that the answer
   is posted when it is done) and started again while a loopback gate in
   front of the edge refuses connections for 45 s. The restarted gateway's
   resubscriptions fail while the gate is closed, and once it opens the
   thread gets the rest of the answer (`turn_resume`) with no reply from the
   person and no "reply in this thread" note. The host gateway always reaches
   the edge through that gate (`--gateway-port` + 3), under the edge's own
   hostname, so TLS verifies as usual.
   **5c. Restart mid-sign-in**: the person with no link asks in a new thread
   and is prompted to sign in; the gateway stops, the link a completed
   sign-in leaves is written to its link store, and the gateway starts again.
   The parked message is answered without being sent again: the restarted
   gateway read it back from its routing store and replayed it.
   **5d. Restart mid-approval**: a tool call pauses a new thread's task on the
   approval card; after a restart, Approve on that card resumes the same task
   in place to `TASK_STATE_COMPLETED`. Steps 5c and 5d run on a gateway that
   keeps held state (klaus-gateway 4.1.0-rc.15 and later, or a dev build) and
   are skipped, with a note, on an earlier release.

With `platform.klausGateway` on, the meta chart's `klaus-gateway` component
follows (the in-cluster shape, the OBO link store in a Secret, its Slack Web
API the same fake through the lab's Service): 6. the render — the Role scoped
to the link Secret, no store volume, the lab's Slack Web API base; 7. two
links written through the gateway's store package, the person's with the
id_token; 8. the pod deleted and its replacement Ready with the same links
within seconds; 9. one Slack turn through the pod over the in-cluster target.
The details, the dev-image loop and what stays out of reach (a real OBO
sign-in, a link's refresh) are in [klaus-gateway](klaus-gateway.md).

The controller reads go over gRPC-Web through the same edge
(`kagentapi.go`), as the portal's backend does. The thread key is
`slack|<channel>|<thread ts>`, the channel `CAGENTLAB<run>`, the people
`UAGENTLABTEST<run>P` (linked, the signed-in email — `admin@lab.local` by
default) and `UAGENTLABTEST<run>S` (no link).

**The negative.** A gateway built without the transport — the `release-v0.x`
line, e.g. `--gateway-image gsoci.azurecr.io/giantswarm/klaus-gateway:0.39.4`
— must fail the proof at start-up or discovery: its `a2a.url` is an HTTP
base URL for kagent's retired JSON-RPC/REST surface and it cannot speak A2A
v1 over gRPC. Run it once when the gateway line changes; it is not a default
step. Not provable here: Slack itself (how it renders a stream, a card or
the native stop button, the app manifest's scopes) — verified on the first
installation that runs the release, per the agentlab-first exception.

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

## GitHub as the person (`platform.github`)

Off by default. On (`agentlab configure --github`), `agentlab platform` registers GitHub's hosted MCP server (`https://api.githubcopilot.com/mcp/`) with muster as the MCPServer `github` (`github-mcp.yaml.tmpl`). An installation declares it the same way: a pinned GitHub authorization server, the grant filed per person (`grantScope: subject`), tools under `x_github_`. Off again, the run deletes the MCPServer; the client Secret is yours and stays.

The OAuth client is yours to register, since GitHub registers no clients dynamically:

- an **OAuth App** (Settings, Developer settings, OAuth Apps), the quickest: its token carries the scopes `repo read:org`, so it reaches every repository the person can;
- or a **GitHub App's** client with user-to-server tokens: rights are the App's permissions on the repositories it is installed on, intersected with the person's. Scopes are ignored.

Either way it is an App of its own, named for this server: the App behind the lab's [GitHub sign-in](github-signin.md) reads a profile and nothing on repositories, and never serves an MCP server — the client here needs repository rights, which a sign-in App must not have.

Either way, the callback URL is muster's proxy callback, `https://muster.<domain>:<gatewayPort>/oauth/proxy/callback` (the run prints it). GitHub only redirects the browser there, so a loopback lab URL works.

The client lives in one Kubernetes Secret in the lab, keys `client-id` and `client-secret`: `agent-platform/github-oauth-client` unless `platform.github.secret` names another (`name`, `namespace`). agentlab never reads, writes or deletes the values; it reads which keys the Secret carries and points the MCPServer at it (`clientCredentialsSecretRef`). The operator's secret tooling places it, one key per call, straight from the vault into the lab's apiserver, so the values pass through no shell, file or agent session:

```sh
beekeeper secret copy op://<vault>/<item>/client-id --to-secret kind-agentlab/agent-platform/github-oauth-client/client-id
beekeeper secret copy op://<vault>/<item>/client-secret --to-secret kind-agentlab/agent-platform/github-oauth-client/client-secret
```

(A person without that tooling creates the Secret with `kubectl --context kind-agentlab -n agent-platform create secret generic github-oauth-client --from-file=client-id=… --from-file=client-secret=…`.) Then `agentlab platform`. Without the Secret, or with a key missing, the run skips the server with a warning naming the Secret, the keys and the command, and goes on.

Each person signs in once (`core_auth_login` with `server: github` through muster, or the portal's Sign in). An agent calling a GitHub tool before that gets muster's sign-in challenge, which it cannot follow itself. muster keeps one pinned client per issuer string, so a second MCPServer pinning `https://github.com/login/oauth` with another client would take over this one's grants.

A personal access token does not fit: muster sends only literal headers, so the token would sit in the MCPServer spec.

## The workspace-manager's provider instances (`platform.workspaces.provider`)

With workspaces on, the workspace-manager gets the provider instances `platform.workspaces.provider` names — `github` (the default while the key is empty), `gitlab`, or both, `github,gitlab` (`agentlab configure --workspaces-provider …`) — `github` of the chart's `github` kind, `gitlab` of its `gitlab` kind, whose App the workspace mirrors sync with. Three pieces follow from the switch alone, whatever the instances:

- `workspace-manager.oauth.baseURL` is `https://workspace-manager.<domain>:<gatewayPort>` (port-free on 443), which the chart's own derivation from `global.domain` would drop the port from. The chart routes that host for the grants' key set (`/.well-known/jwks.json`) and forwards the URL.
- The person's sign-in at a provider is muster's (workspace-manager 0.10.0 and newer): muster pins the manager's registration to one instance's App (`workspace-manager.muster.mcpServer.auth`, the instance `platform.workspaces.grantProvider`, `--workspaces-grant-provider`, empty for the first; `apps` pins none and the chart keeps one muster provider App per instance, each granted with `core_auth_login app=<instance>`), the person grants it once with `core_auth_login`, and muster attaches the person's App token to every call of the manager's tools. The App's callback URL is muster's `/oauth/proxy/callback`; the pin's OAuth client is the instance's Secret with `client-id` beside `client-secret` ([Workspaces](workspaces.md#the-persons-grant-musters)). The manager serves no sign-in of its own: its routes are its MCP endpoint and the grants' key set.
- `agentlab configure` and `agentlab platform` print the callback URL each instance's App names, muster's `/oauth/proxy/callback`.

**`github`** needs a GitHub App of the lab's own on github.com, apart from the [sign-in App](github-signin.md) and from the [`github` MCP server's client](#github-as-the-person-platformgithub); [Workspaces](workspaces.md#the-provider-instance-github) names its callback and permissions. Its App id and client id are public and live in `agentlab.yaml` (`platform.workspaces.github.appId` and `clientId`, or `--workspaces-github-app-id` and `--workspaces-github-client-id`), empty until the App exists. Its private key and client secret live in one Kubernetes Secret, `agent-platform/workspace-github`, keys `private-key` and `client-secret`; the values agentlab renders reference it (`app.privateKey`, `oauth.clientSecret`). agentlab never reads, writes or deletes the values; it reads which keys the Secret carries. The operator's secret tooling places it, one key per call, straight from the vault into the lab's apiserver. Recorded in `agentlab.yaml` (`platform.workspaces.github.privateKeySource` and `clientSecretSource`, or `--workspaces-github-private-key-source` and `--workspaces-github-client-secret-source`: references `beekeeper secret copy` resolves, never a value), every `agentlab up` and `agentlab platform` places both before the install, so a recreated lab carries the instance without a manual step, and a placement that fails fails the run. By hand:

```sh
beekeeper secret copy op://<vault>/<item>/private-key --to-secret kind-agentlab/agent-platform/workspace-github/private-key
beekeeper secret copy op://<vault>/<item>/client-secret --to-secret kind-agentlab/agent-platform/workspace-github/client-secret
```

**`gitlab`** is a GitLab's OAuth application with a group access token ([Workspaces](workspaces.md#the-provider-instance-gitlab) names GitLab's side). Its URL and client id are public and live in `agentlab.yaml` (`platform.workspaces.gitlab.url` and `clientId`); the token and the application's secret are the Secret `agent-platform/workspace-gitlab` (keys `token`, `client-secret`), placed from `tokenSource` and `clientSecretSource` on every `up` and `platform`. The instance renders once all four are in place.

(A person without that tooling creates the Secret with `kubectl --context kind-agentlab -n agent-platform create secret generic workspace-github --from-file=private-key=… --from-file=client-secret=…`.) Then `agentlab platform`. While an id is empty, the Secret is missing or a key lacks a value, the run renders no `github` instance (the workspace-manager refuses one without them at start-up), warns with what is missing, the callback URL and the commands, and goes on with the base URL, the route and the other instance in place. With no instance rendered, muster's credential provider releases the configured instance (github.com's OAuth issuer before the App id).

No secret value reaches `agentlab.yaml`, `state/`, a log line or a process argument for any instance: the values carry Secret references, placed by the operator's secret tooling (`internal/lab/workspaceprovider_test.go` renders them and checks). The golden renders of the block, one per configuration, are `internal/lab/testdata/workspace-providers/`.

## The infrastructure families and the tool-group label

An installation registers the servers for the management clusters the
platform runs on as members of muster's infrastructure families —
`kubernetes`, `prometheus`, `capi`: agent-platform-mcps renders one MCPServer
per cluster and family, `<cluster>-mcp-<family>`, each declaring
`spec.family` (so muster exposes the family once, `x_kubernetes_<tool>` with a
`management_cluster` argument) and labelled
`muster.giantswarm.io/management-cluster: <cluster>`. The lab registers its own
cluster the same way, with `clusterName` (default `agentlab`) as the instance:

| Server | Family | Registered by | Tools |
|---|---|---|---|
| `<clusterName>-mcp-kubernetes` | `kubernetes` | the connectivity chart (`mcp-kubernetes.mcpServer.managementCluster`) | `x_kubernetes_<tool>` |
| `<clusterName>-mcp-prometheus` (with `platform.observability`) | `prometheus` | an `agent-platform-mcps.mcpServers` entry (`cluster`, `group: prometheus`) | `x_prometheus_<tool>` |

Every family tool takes `management_cluster`, the member's MCPServer name
(`<clusterName>-mcp-kubernetes`, `<clusterName>-mcp-prometheus`), the way an
installation's take `<cluster>-mcp-kubernetes`. There is no
family-less registration: the portal's MCP servers page, the agent create
flow's Tools step and muster's `infrastructure` preset see the shape of an
installation with one management cluster. `capi` joins when the lab runs
Cluster API. A lab made by an earlier release loses the six fake-fleet
MCPServers (`<family>-lab-01`, `<family>-lab-02`) on its next `agentlab
platform`; the retired `platform.fakeFleet` key still loads and is dropped from
`agentlab.yaml`. The released chart must be agent-platform 4.93.0 or newer (the
3.x line, a migration rehearsal's seed, and an [upgrade
seed](#upgrade-proofs-from-an-older-line) keep their family-less
`mcp-kubernetes`).

**What the label is for.** `agent-platform.giantswarm.io/tool-group` is the
Agent Platform's tiering of MCP servers — three groups the portal's MCP servers
page, the Tools step, muster's toolset presets and the docs all use under the
same names: **Agent Platform** (`agent-platform`: agent-manager,
model-manager, muster's core tools), **Infrastructure** (`infrastructure`: the
families) and **Registered servers** (no label: whatever an installation or a
user registers, including everything the portal's Register server flow
creates). The chart that ships a server stamps it; every consumer only reads
it, e.g.
`kubectl get mcpservers.muster.giantswarm.io -A -l agent-platform.giantswarm.io/tool-group=infrastructure`
lists the lab's family members. Orientation and preset membership, not
authorization — that stays with the servers' OAuth and the clusters' RBAC.
`lab-oauth-fixture` and anything you register by hand stay unlabelled on
purpose, so the Registered servers group has members.

`agentlab platform-test` asserts the families and the label: the lab's members
exist with the family block, the management-cluster label and
`tool-group: infrastructure`; no family-less `mcp-kubernetes` and no retired
fake-fleet member exists; nothing the lab creates itself carries the label;
every value in the cluster is one of the two the contract knows and
`lab-oauth-fixture` is unlabelled. Servers the vendored charts label are
reported, not judged.

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
   plus `core_*`), and `infrastructure` holds the tools of every family the
   lab's cluster is a member of (`x_kubernetes_*`, and `x_prometheus_*` with
   observability).
4. **The runtime path** (skip with `--skip-chat`): a `Session` and
   one A2A turn through the edge, as the user, the read-only agent lists nothing
   outside `preset:read-only` — read-only core tools may appear, no writer
   does (so kagent sends the header and the user's token) — and the
   `preset:none` agent answers a chat turn. The agents run on
   `default-model-config` (`--model-config` to pick another).
5. **The sign-in claim (ground-truth G6)**: before any sign-in,
   `server:lab-oauth-fixture` resolves to nothing for everyone
   (`toolset_unmatched` names the selector), yet points at the sign-in:
   `filter_tools` lists the fixture under `toolset_requiring_auth`, and a call
   to `x_lab-oauth-fixture_list_core_tools` answers `auth_required` with the
   sign-in link, never `outside the toolset`; the portal's Sign in (`POST
   /api/muster/auth/login` with the portal's own forwarded Dex id_token) yields
   the challenge, which the proof completes as the browser would; an
   agent-shaped session on the **same** id_token then resolves the fixture's
   tools and calls one; the real agent, driven through kagent with that token,
   reports them too. muster keys a forwarded bearer's session by its
   principal (`ext-<hash>` of iss, sub, azp or aud and the act chain), and the
   grant belongs to that session (`grantScope: session`): the same user under
   a **fresh** id_token of the same client resolves the same tools (a
   refreshed token keeps its session), while a second user and the same user
   through **another client** (`kubernetes`) resolve nothing. After
   `core_auth_logout` on the portal's token, the user's fresh id_token
   resolves nothing either. The portal forwards one and the same id_token to
   muster and to kagent, so the agent sees what the person signed in to.
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
| `networkPolicy.enabled: false`, `kyvernoPolicies.enabled: false` | The chart's own policy objects. The Cilium policies would apply (the API is served, next row) and enforce nothing, so the lab does not pretend to have them. The chart's Kyverno objects are PolicyExceptions to the fleet's Pod Security Standards policies and the mutate policies of agent-sandbox and model serving: the lab's Kyverno (two rows down) enforces the fleet's `flux-multi-tenancy` policy and nothing else, so the exceptions would except from policies that are not here and the mutate policies would change pods no lab proof ever ran with. Pinned off because the chart's `auto` would render them once `kyverno.io/v1` is served — a lab with and without the fleet's admission renders the same platform. The exceptions are still judged: `agentlab polex-test` and its `platform-test` stage render them with `kyvernoPolicies.enabled: true` (and `tenants.organizations` on where agents are) and create each as a server-side dry run against the lab's Kyverno, whose exception webhook refuses what the fleet's refuses. |
| The Cilium policy CRDs (`cilium.io/v2`: `CiliumNetworkPolicy`, `CiliumClusterwideNetworkPolicy`) served, with no Cilium behind them | Every Giant Swarm cluster runs Cilium, so component charts render their Cilium policies unconditionally — gpu-operator-app's three `CiliumNetworkPolicy` objects (`components.gpu-operator`) — and without the API the HelmRelease fails on `no matches for kind CiliumNetworkPolicy`. `agentlab up` applies the CRDs of the Cilium release the fleet runs (1.20.1, verbatim upstream, embedded like the Gateway API CRDs), so the objects apply and validate against the schema an installation enforces. **Inert by construction:** no agent reads them — a policy that would isolate a pod on an installation isolates nothing here, so the lab proves that a chart's policies *install*, never that they *work*. `agentlab platform-test` asserts both kinds are served (`kubectl api-resources \| grep cilium` from a shell). |
| The fleet's `flux-multi-tenancy` ClusterPolicy in **Enforce** on the fleet's Kyverno (the charts security-bundle installs, from `gsoci.azurecr.io/charts/giantswarm`: `kyverno-crds` 1.17.0 and the kyverno app 0.24.2, which wraps the upstream chart 3.7.2 = v1.17.2, reduced to its admission controller — one replica, no background, cleanup or reports controllers, the app's policy-reporter beside it, the fleet's images from gsoci, PolicyExceptions on for every namespace as the app ships them), the platform namespace exempt next to the fleet's `flux-giantswarm`, `giantswarm`, `monitoring`; the org namespace `org-lab` with the tenant ServiceAccount `automation` bound to cluster-admin inside it by `write-all-customer-sa` | Every Giant Swarm management cluster enforces this policy on every Flux object outside those three namespaces: a `HelmRelease` or `Kustomization` in an org namespace carries `spec.serviceAccountName` or `spec.kubeConfig.secretRef.name`, and leaves its namespace (`targetNamespace`, `storageNamespace`) only with a kubeconfig. A lab without it admits what an installation refuses — cluster-manager 0.5.2's own-cluster releases passed every lab proof and were denied on an installation. `agentlab up` installs the engine after the platform (whose bundled Flux brings the kinds the policy names) and applies the policy verbatim from management-cluster-bases (`internal/lab/templates/flux-multi-tenancy.yaml`, the ClusterPolicy and the aggregated ClusterRole its rules read with; the file is byte-identical to the source, the header names the commit); the one lab addition is made in code (`admission.go`): the platform namespace joins each rule's exclude list, standing in for `flux-giantswarm` — on an installation the platform's child HelmReleases live there, in the lab in `agent-platform`, and the lab-rendered mcp-prometheus release targets `monitoring` from there, legal only for an exempt namespace. The org fixture (`org-fixture.yaml`) is what rbac-operator gives every `org-<org>` on an installation, so a release naming `automation` (cluster-manager's default `flux.tenantServiceAccount`) is admitted **and** runs. The boot gates on the policy enforcing — a `HelmRelease` in `org-lab` without a tenant denied with the fleet's message — and `agentlab platform-test` proves the matrix as server-side dry runs (nothing is created): no tenant → denied by `serviceAccountNameMustBeSet`; `serviceAccountName: automation` → admitted; `kubeConfig.secretRef` into `kube-system` → admitted; a tenant with `targetNamespace: kube-system` and no kubeconfig → denied by `targetNamespaceMustBeLocal`. What the lab does not carry: the fleet's Pod Security Standards policies (previous row), and the Flux kinds the policy also names — `Kustomization`, `Alert`, `Receiver`, `ImagePolicy` — are not served here (the engine runs source- and helm-controller only), so those rules match nothing; Kyverno accepts the policy with the warning `unable to convert GVK to GVR for kinds Kustomization` in the boot log, and its resource webhook intercepts `helmreleases` (and core `events`, which Kyverno's default resource filter excludes) with `failurePolicy: Fail`, as on an installation. |
| The muster/kagent ServiceMonitors, valkey PodMonitor and muster PrometheusRule follow `platform.observability` | Without it there is no Prometheus Operator, so none of those CRDs exist and the releases fail to render. With it they are scraped by the lab Prometheus — whose selectors are opened up (`*NilUsesHelmValues: false`) because upstream's default selects only monitors carrying the kps release label, and the platform's monitors come from other releases. Flipping observability rolls the muster pod once (the toggle changes its metrics-exporter env). |
| `platform.observability`: the GS kube-prometheus-stack constituent installed directly, Prometheus server re-enabled, instead of the observability-bundle | The bundle is MC-shaped (Flux HelmReleases with a hardcoded remote kubeconfig, Alloy → Mimir, no local PromQL endpoint). See [Observability](observability.md). |
| `global.observability.traces.otlp.endpoint: ""` | The chart's default collector is the fleet's kube-system `otlp-gateway`, which kind does not run, with or without `platform.observability`. Empty, every `auto` exporter turns off (Substrate's signals, kagent's, muster's) instead of logging a resolver error on every export. |
| `muster.muster.oauth.mcpClient.enabled: true` + the `lab-oauth-fixture` MCPServer | The chart leaves muster's OAuth *client* role — the proxy behind `core_auth_login` and the portal's Sign in — off; real installations turn it on, and without it no per-server sign-in can be exercised. The fixture is the one `Auth Required` downstream to sign in to (muster's own protected `/mcp`); see [Signing in to a downstream server](#signing-in-to-a-downstream-server-muster-as-oauth-client). |
| `muster.rbac.{mcpServerEditor,workflowEditor}.subjects` → `oidc:platform-admins` | The chart binds muster's editor Roles to Giant Swarm's admin groups, which do not exist here. Rebound to the lab's own admin group (`--oidc-groups-prefix=oidc:`, same spelling as the lab RBAC). Lists replace, so the GS groups are dropped. |
| muster patched to `hostNetwork` + one replica + `maxSurge: 0` (unless `platform.musterPodNetwork`) | Same issuer trick as the apiserver and Backstage. One replica and `maxSurge: 0` because two hostNetwork pods cannot both bind `:8090` on a one-node cluster. With [muster on the pod network](#muster-on-the-pod-network) it runs two replicas with the `dex-localhost` sidecar instead. A Kustomize strategic-merge patch in `components.muster.postRenderers`, which the chart forwards to muster's `HelmRelease` and the bundled helm-controller applies over the muster chart's render. |
| The `dex-localhost` sidecar on every Deployment told the lab Dex's address — mcp-kubernetes, the managers (model-manager, agent-manager, vm-manager, cluster-manager) and mcp-prometheus | Those servers validate the forwarded Dex token themselves and must reach the issuer URL `https://localhost:32000/dex`, but all listen on `:8080` and cannot share the host network. A socat sidecar on the pod's own loopback forwards `:32000` to the Dex Service (HACKS.md U13) — a `postRenderers` patch on each component, and on the lab's own mcp-prometheus `HelmRelease`. The targets are a rule over the component renders (a Deployment whose containers are told the address through a name they dial it by — `--dex-issuer-url`, `DEX_ISSUER_URL`, `OIDC_ISSUER_URL` — or that the `agentlab.giantswarm.io/dex-localhost` annotation selects; an identity-only variable such as `OAUTH_AUTHORIZATION_SERVER` selects nothing), not a list: a component the chart turns on by default or an overlay turns on is covered without a lab release, and the roster's trailing range renders `components.<name>.postRenderers` for one the lab names no block for. `agentlab platform-test` asserts the sidecar, a clean rollout with no container restarted since the release — a kind node restart, which restarts every container once together, is history, not a crash loop — and the MCPServer reachable (Connected, Awaiting Session or Auth Required) on every such Deployment, naming the key each was selected by; `agentlab platform` rolls such a Deployment once when it crash-looped since its release. |
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

- **A dev build that waits for a component release is refused before
  anything is applied.** A branch of agent-platform may raise a component's
  `versionRange` past every published chart; its dev build records each such
  component in the `Chart.yaml` annotation
  `agent-platform.giantswarm.io/unreleased` (component → `versionRange`,
  `waitsFor`). Flux would find no tag in that range minutes into the
  install, after the components ahead of it are up. `agentlab up` (before
  the certs and the cluster) and `agentlab platform` (before the namespace)
  read the annotation of the chart they install — the pulled build on the
  dev channel or a pin, the checkout under `platform.chartPath` — and stop,
  naming every component and the version it waits for, when the component is
  enabled, its `versionRange` in the values is still the recorded one, it
  sets no `semverFilter`, and the chart is a development build (not `X.Y.Z`
  or `X.Y.Z-rc.N`; a chart directory always counts as one). Point the
  component at a published range or a dev channel in `platform.valuesFiles`,
  or install a build whose ranges are published. A chart without the
  annotation installs as before.
- **A chart that refuses its values stops the install before it starts.** The
  boot renders the meta chart and every component chart offline to side-load
  their images — each at the version its `OCIRepository` resolves to, with
  the values its `HelmRelease` carries: the pair helm-controller validates on
  the cluster. Most render failures there are skips, noted (a chart whose
  `kubeVersion` an offline render cannot satisfy, a tag the registry does not
  have): the node pulls those images itself and the install proceeds. Two are
  not. The first is a registry this host cannot reach — a name the resolver
  does not answer, a refused or timed-out dial, a 5xx or 429 from the
  registry. The renders are more than the image list: the lab's patches for
  a component (the `dex-localhost` sidecar's targets, the `platform.devImages`
  image names) are read off them, so a component without its render would be
  installed unpatched — mcp-kubernetes crash-looping on `connection refused`
  to `https://localhost:<dexPort>/dex`, the install failing minutes later on
  its `HelmRelease`, nothing pointing at the network. A name the resolver
  does not know or a refused dial is tried again (three retries over some
  twenty seconds, each noted with the resolver's or the dialer's words; a
  timeout, a 5xx or a 429 the registry client's transport has already
  retried five times per request); if the registry still does not answer,
  the boot stops before the install, printing each release's render error
  whole and the registry host to check, and `agentlab platform` picks it up
  again once the host resolves. The meta chart's own render is held to the
  same rule — before the certs and the cluster on `agentlab up`. The second
  is a chart that refuses its values. When a
  `values.schema.json` — the chart's own or a subchart's — *refuses* the
  values, the install would carry them to helm-controller and fail after its
  whole wait, so the lab refuses instead, printing Helm's verdict whole (the
  schema paths are its last lines), the release and the chart version it
  came from, and the knob that selects the chart in this lab's mode:
  `platform.chartVersion` for a release, another build or a pin on the dev
  channel, the checkout itself under `platform.chartPath`. The usual cause is
  a meta chart whose component range floats onto a chart from another line —
  `platform.chartVersion` 3.20.2 carries `agent-platform-connectivity
  >=1.0.0`, which resolves to a 4.x connectivity that rejects the 3.x
  `kyvernoPolicies.*` keys the meta chart still forwards. Where the **meta
  chart's own** schema refuses the lab's values, the boot's first render
  already holds that verdict and `agentlab up` stops there, before the certs
  and the cluster (the dev channel is resolved first, so the build judged is
  the one the install would use); a component's verdict comes after the
  cluster boot, when the components are rendered. The lab's own
  mcp-prometheus `HelmRelease` is judged like a component, with the advice
  that fits it (its chart version and its values are agentlab's). With
  `platform.devImages` the values the install carries are rendered once more
  after the swap, and the changed pair is put to the charts again, so a dev
  image a schema refuses is refused here too.
  Three cases stay notes, because a refusal there does not predict the
  install: a chart whose `values.schema.json` cannot be loaded or compiled at
  all (a remote `$ref` this host cannot fetch — helm-controller has cluster
  egress and may render it fine); a `HelmRelease` that turns helm-controller's
  schema validation off (`spec.install.disableSchemaValidation`); and a
  `HelmRelease` with a `valuesFrom` whose verdict the ConfigMap or Secret
  could answer — a missing property may be there — while `additional
  properties … not allowed` on keys the render did see is refused all the
  same, since Flux lets `spec.values` win and a reference cannot take a key
  away.
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
- **The atelet and the WorkerPool's workers must be one Substrate release.**
  The chart installs two halves of Agent Substrate from two pins: the control
  plane and the per-node `atelet` from `components.substrate`, and the
  `WorkerPool`'s worker image (`spec.workerImage`, `ateom-gvisor`) from the
  kagent release `components.kagent` resolves to — kagent's own Substrate pin,
  forwarded by the meta chart. A meta chart whose kagent range admits a kagent
  from a newer Substrate release while its substrate range stays installs
  green and boots no golden actor: a worker looks for the actor bundles laid
  out as its own release writes them (a Substrate release renamed the actor's
  pause bundle, `bundles/pause` → `bundles/_pause`) — every compile the atelet
  asks of the worker fails inside the worker on
  `bundles/_pause/config.json: no such file or directory`, every
  `AgentTemplate` sits at `Ready=False ActorTemplatePending` ("golden snapshot
  compiling"), `agentlab up` and the platform releases stay green,
  `agents-test` and `skills-test` burn their five minutes and only
  `kubectl -n ate-system logs ds/atelet` says why. `agentlab platform`
  reads the atelet's image off its DaemonSet and the worker image off every
  `WorkerPool` in `kagent` after the install and refuses a lab whose halves
  are different Substrate releases (the tag's major.minor: a patch of the
  line never changes the worker/atelet contract, a re-pin onto another
  upstream release is at least a minor), naming both images and the fix — `agentlab configure --defaults
  --chart-version <a release that pins them together> && agentlab platform`
  for a release pin, the chart's own ranges for a checkout or the dev channel;
  `platform-test` asserts the same pair and prints it. The chart-side
  coupling is giantswarm/agent-platform#466.
- **Every worker is `Pending` and no agent turn ever finishes.** The
  `WorkerPool`'s gVisor workers carry the pool's CPU feature-set pin
  (`kagent.substrateWorkerPool.template.nodeSelector`), and a pin the node does
  not match schedules nothing: `kubectl -n kagent get pod -l
  ate.dev/worker-pool=kagent-default` shows `Pending`, the pod's events read
  `node(s) didn't match Pod's node affinity/selector`, and the
  `AgentTemplate` sits `Ready=False ActorTemplatePending` while agents stay
  "Working…". Compare the two with `kubectl get nodes -L kubernetes.io/arch`
  and `kubectl -n kagent get workerpool kagent-default -o
  jsonpath='{.spec.template.nodeSelector}'`. The lab renders the node's own
  architecture, so a mismatch means either a `platform.valuesFiles` overlay
  setting the key, or a chart version that no longer takes it and left its own
  `amd64` default on the pool — then `agentlab platform`. `up`/`platform`
  refuse the install when the `WorkerPool` they are about to create names an
  architecture no node carries, so this only bites a cluster whose values or
  chart changed underneath it. `platform-test` catches every other cause too
  (a taint, a resource request no node fits, a pin changed after the
  install): it waits up to two minutes for every worker of every pool in
  `kagent` to be Running and Ready, at least the pool's `spec.replicas` of
  them, each on a node that carries the pool's node selector, and fails
  naming each worker that is not and why — the scheduler's reason and
  message for a Pending one.
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
- **Kubernetes and Prometheus tools are the families'.** The lab's servers
  are the `kubernetes` and `prometheus` families' members for its cluster, as
  on an installation, so a call names the family's tool and the cluster:
  `call_tool(name=x_kubernetes_list, arguments={management_cluster: agentlab-mcp-kubernetes, resourceType: namespaces})`.
  See [The infrastructure families](#the-infrastructure-families-and-the-tool-group-label).
- **Tool results are double-wrapped.** `result.content[0].text` is JSON whose
  `content[0].text` is the actual payload — two decode hops.
