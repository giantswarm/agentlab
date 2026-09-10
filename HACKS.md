# Hack audit

Inventory of every hack/workaround in the lab's scripts, what it papers over,
and what happened to it. One commit per resolved item. Statuses:

> **2026-08-27, Go port:** the lab was rewritten as a single Go binary
> (`agentlab`) with form-driven configuration; scripts/ and manifests/ are gone.
> Every fix below carries over — the checksum stamping (H2/H3), the digest
> gate (H9), the exact-tag image check (H1), key permissions (H10, now 0600 at
> creation) and the post-render patches (U1-U3, now `agentlab post-render`) all
> live in `internal/lab/`. Two NEW items surfaced during the port: H11, H12.
>
> **2026-09-08, the agent-platform meta chart in the lab shape:** the lab
> installs `giantswarm/agent-platform` (bundled Flux engine on,
> self-management off) instead of the git-vendored agent-platform-standalone
> umbrella. `fluxUp`, the vendored chart (`.vendor/`), the post-renderer
> binary and its generated Helm plugin (`state/helm-plugins/`) are gone; the
> post-render patches (U2, U9, U13) are per-component `postRenderers` values
> the chart forwards to the component HelmReleases (`postrenderers.go`), the
> image preload resolves the component charts from the rendered
> OCIRepositories (`fluxreleases.go`), and mcp-prometheus is a lab-rendered
> HelmRelease through the same engine. H12, H13, U3 and U4 are retired with
> the mechanisms they patched.

- **FIXED** — replaced with a proper solution (commit referenced).
- **BLOCKED UPSTREAM** — cannot be fixed in this repo; the exact upstream
  change that unblocks it is named.
- **ACCEPTED** — deliberate lab trade-off, not worth "fixing"; reasoning given.

## Script hacks (fixable here)

### H1. `backstage.sh`: image-presence check ignores the tag — FIXED
`docker exec … crictl images | grep -q giantswarm/backstage` matches *any*
backstage image. Bumping `IMAGE` to a new tag would silently keep running the
old one already loaded into the node.
**Fix:** exact `repo:tag` match against `crictl images -o json` repoTags.

### H2. `backstage.sh`: unconditional `rollout restart` + secret applied after the Deployment — FIXED
The `dex-ca` secret was created *after* `kubectl apply -f manifests/backstage.yaml`,
so on first boot the pod sat in ContainerCreating waiting for a volume that did
not exist yet; an unconditional `rollout restart` then bounced the (2.4 GB,
Rosetta-emulated) pod on *every* re-run to compensate for possible cert changes.
**Fix:** namespace + secret are applied before the Deployment, and a
`checksum/lab-inputs` annotation (sha256 over the manifest + `certs/ca.crt`)
is stamped into the pod template — same pattern `dex.yaml` already uses — so
the pod rolls exactly when the config or CA changes and no-op re-runs stay
no-ops. The blanket `rollout restart` is gone.

### H3. `up.sh` + `Makefile reload`: duplicated sed/checksum stamping — FIXED
The `sed "s/REPLACED_BY_UP_SH/$SUM/" manifests/dex.yaml | kubectl apply` logic
lived in two places (up.sh and the `reload` target) and would drift.
**Fix:** extracted `scripts/apply-dex.sh` (stamp + apply + rollout wait);
up.sh and `make reload` both call it. The checksum now also covers
`certs/tls.crt`, so a cert rotation rolls the pod too. Placeholder renamed to
`REPLACED_AT_APPLY` to match its new owner.

### H4. `up.sh`: dex-tls secret created after the Dex Deployment — FIXED
Same ordering smell as H2: on first boot the Dex pod waited on a secret that
was applied a step later.
**Fix:** namespace + dex-tls secret are applied before the Deployment.

### H5. `restart-apiserver.sh`: blind `sleep 5` between manifest removal and restore — FIXED, then superseded by H6
The static-pod bounce moved the manifest away, slept 5 seconds, and moved it
back. If the kubelet had not yet noticed the removal, the restore was a no-op
and the apiserver never restarted — a silent false success.
**Fix:** poll `crictl ps` inside the node until the kube-apiserver container
is actually gone before restoring the manifest, fail loudly on timeout, and
restore the manifest via an EXIT trap on every path so the cluster is never
left without an apiserver.

### H6. Is the apiserver bounce needed at all on Kubernetes 1.35? — FIXED (script deleted)
The whole reason for `restart-apiserver.sh` was the claim that the OIDC
authenticator gives up discovery after ~40 s and never retries. Verified
false on a cold boot (kind v0.31, Kubernetes 1.35, 2026-08-27): the apiserver
logs `oidc.go:433 … initializing plugin: … connection refused` every 10
seconds indefinitely, and the first admin token was accepted immediately once
Dex answered — zero bounces. The 40s/4-retries behavior belonged to older
Kubernetes.
**Fix:** `restart-apiserver.sh` deleted, the call removed from up.sh, the
`restart-apiserver` Make target removed, README gotcha rewritten. up.sh's
existing 60 s verification loop comfortably covers the ≤10 s window until the
authenticator's next retry tick.

### H7. `platform-up.sh`: MCPServer "Connected" wait loop cannot fail — FIXED
The `for … seq 1 40` loop printed nothing and fell through silently when the
MCPServer never reached `Connected`; the script then declared the platform up.
**Fix:** the loop now reports the last observed state and exits non-zero on
timeout, pointing at `make platform-logs`.

### H8. `platform-test.sh`: fixed temp paths under /tmp — FIXED
Response headers/bodies went to `/tmp/.mcp-h` / `/tmp/.mcp-b` — collision-prone
between concurrent runs and left behind afterwards.
**Fix:** `mktemp` + `trap … EXIT` cleanup.

### H9. `platform-up.sh`: mtime-based chart-dependency freshness check — FIXED
`[[ Chart.lock -nt charts ]]` plus a `touch charts` stamp. mtimes lie (git
checkout order, copies, clock skew) and the failure mode is a stale charts/
dir silently installed.
**Fix:** store the sha256 of Chart.lock in `charts/.lock-digest` after a
successful `helm dependency build` and rebuild whenever it differs.

### H10. `gen-certs.sh`: private keys chmod 644 — FIXED
`chmod 644 certs/*.key` made the CA and server keys world-readable. Nothing
needs that: the kind mount is read by root in the node container regardless,
and the secrets are created from file by the invoking user.
**Fix:** 600 on keys, 644 on certs; re-runs also tighten keys left
world-readable by older versions of the script.

### H11. Dex `storage: memory` rotates signing keys on every pod roll — FIXED
Latent in the shell lab and exposed by the Go port's user-editing flow: a
config change rolls the Dex pod by design (checksum annotation), and with
in-memory storage the new pod mints new signing keys — the apiserver then
rejects **every** token with `failed to verify id token signature` until its
JWKS cache refreshes (observed: minutes). The old verification never noticed
because `make reload` was only ever tested as a no-op; edit a user and reload,
and all logins broke.
**Fix:** Dex now uses its CRD-backed `kubernetes` storage (ServiceAccount +
ClusterRole for the `dex.coreos.com` API group). Keys persist across rolls, an
immediate post-reload login verifies, and the state still dies with the
cluster.

### H12. Chart vendored into `vendor/` collides with the Go toolchain — RETIRED
The agent-platform-standalone checkout lived in `vendor/`, which flips a Go
module into vendored-build mode: after the first `agentlab platform`, `go build`
failed with "inconsistent vendoring".
**Fix:** the chart was vendored into `.vendor/` instead. **Retired 2026-09-08:**
the lab installs the released agent-platform chart from the registry (or a
local chart directory, `platform.chartPath`); nothing is vendored, and
`agentlab platform` removes a leftover `.vendor/`.

### H13. `agentlab platform` failed on Helm 4 server-side-apply conflicts after a dev-image swap — RETIRED
The documented way back from a dev-image swap (`kubectl set image` /
`kubectl patch` on a platform Deployment, see the agentlab skill) was "re-run
`agentlab platform`, which reconciles every Deployment to the vendored chart".
Helm 4 applies server-side, so the swapped image field belongs to the
`kubectl` field manager afterwards and the upgrade died on the conflict
(`Apply failed with 1 conflict: conflict with "kubectl-set" ... image`); the
workaround was deleting the Deployment by hand and re-running.
**Fix:** the umbrella upgrade passed `--force-conflicts`: Helm took the
fields it renders back from any other manager, so a re-run really did
reconcile the lab to the chart. **Retired 2026-09-08:** the workloads are the
component HelmReleases' now (helm-controller applies them), and the dev-image
loop goes through the values — `platform.devImages` renders a kustomize image
override into the component's `postRenderers`, so the swap and its restore are
plain `helm upgrade`s with no second writer and no flag. A hand `kubectl patch`
lasts until helm-controller's next release of that component.

## Blocked upstream (documented, not fixable in this repo)

### U1. `muster-post-render.sh` patch: `allowPublicClientRegistration` edited into the rendered ConfigMap — FIXED upstream
The muster chart exposed the key in values.yaml, values.schema.json, README
and unit tests, but `templates/configmap.yaml` never rendered it, so the value
was silently dropped and Claude Code's DCR registration died with
"Registration requires authentication". Neither alternative gate can work for
a public loopback client (no token, random port, http/https stripped from
allowed schemes by mcp-oauth validation), so the key was edited into the
rendered ConfigMap by the post-renderer.
**Fixed:** muster 5.7.2 (giantswarm/muster#1118) renders the key; the chart
BOM carries it since the curation that pinned muster 5.7.2. The post-render
ConfigMap edit is deleted — the values entry alone is effective now.

### U2. hostNetwork + dnsPolicy + maxSurge 0 on the muster (and Backstage) Deployment
muster must resolve `https://localhost:32000/dex` to the Dex NodePort from
inside the pod (the lab's one-issuer-URL trick), which needs hostNetwork; and
with hostNetwork a default rolling update deadlocks on a single node because
both pods want :8090. The muster chart (checked through 5.6.2) exposes no
`hostNetwork`, `dnsPolicy` or `strategy` values; the Backstage chart neither.
Since 2026-09-08 the patch is a Kustomize strategic merge in
`components.muster.postRenderers` / `components.backstage.postRenderers`
(`postrenderers.go`), which the agent-platform chart forwards to the component
HelmRelease and the bundled helm-controller applies — no post-renderer binary.
**Unblocks:** giantswarm/muster — add deployment-level `hostNetwork`/
`dnsPolicy`/`strategy` values. Until then the patch is surgical so chart bumps
need no hand-copying.

### U3. `muster-post-render.sh` patch: umbrella HTTPRoute stripped + placeholder parentRefs — RETIRED
`agent-platform-standalone`'s `_helpers.tpl` hard-failed on empty
`ingress.parentRefs` in **all** modes — even `muster-direct`, where the lab
had no Gateway at all. The values carried a `no-gateway-in-this-lab`
placeholder to pass the guard and the rendered route was stripped.
**Retired:** the lab runs the chart-owned agentgateway edge
(`gatewayApi.gateway.create: true`), so the routes attach to a real Gateway and
nothing is stripped; the post-renderer that did it is gone (2026-09-08).

### U4. `platform-up.sh`: chart vendored from git at a pinned SHA — RETIRED
The umbrella chart had no OCI release: `charts/giantswarm/agent-platform-standalone`
did not exist in gsoci (verified 2026-08-27, `NAME_UNKNOWN`), so the lab
vendored it from git at a pinned SHA (`platform.apsRef`).
**Retired 2026-09-08:** the lab installs the released agent-platform meta
chart — `helm upgrade --install … oci://gsoci.azurecr.io/charts/giantswarm/agent-platform
--version <platform.chartVersion>` — or a local chart directory
(`platform.chartPath`) for unreleased changes; the standalone umbrella retires
with the one-chart delivery story.

### U5. `platform.go`: mcp-kubernetes must be `--wait`ed serially before muster installs — RETIRED (#34)
muster dials its MCPServers ~2s after starting; a failed first dial schedules
a retry "after 30s" but the orchestrator's backoff-expiry sweep only fires
~60s later ("Attempting to reconnect failed MCPServer … (backoff expired)" at
+60s, observed 2026-08-28 on muster 5.5.6). Overlapping the two helm installs
therefore *added* ~30s to boot: muster started before mcp-kubernetes was
ready and ate the fixed penalty. The workaround was ordering — a separate
mcp-kubernetes release `--wait`ed before the umbrella so muster's first dial
always succeeded.
**Retired:** since agent-platform-standalone#44 the umbrella bundles
mcp-kubernetes itself (`components.mcp-kubernetes`, on by default, registering
an MCPServer named `mcp-kubernetes`, no muster family), and the lab adopted it
(#34): the standalone release, its version pin and its values template are
gone, and the tools renamed `x_kubernetes_*` → `x_mcp-kubernetes_*` with no
`management_cluster` argument. The two workloads now start concurrently inside
one release, so a muster pod that wins the race can still eat the reconnect
backoff before the MCPServer shows Connected — the boot's 120s Connected wait
absorbs it. The muster-side fix (sweep at the scheduled retry time) would
remove that residual delay.

### U6. `platform.go`: token-validation probe + one-shot muster bounce after install
A muster pod (5.5.6, mcp-oauth v1.3.1) can come up with a TLS trust pool that
is missing the `--extra-ca-file` CA: every `/mcp` bearer is then rejected with
`invalid_token` — the JWKS fetch AND the userinfo fallback both fail with
`x509: certificate signed by unknown authority` against Dex — while
everything the lab used to check looks healthy (rollout done, MCPServer
Connected, OIDC discovery succeeded, `/.well-known/oauth-authorization-server`
answers 200). Observed 2026-08-28; the same pod's OIDC *discovery* over the
same endpoint succeeded, so only some client constructions miss the CA.
Verified not to be config: the `dex-ca` secret matched `certs/ca.crt` and Dex
served the lab-CA-signed cert at the moment the pod failed x509. Not
deterministic — a pod restart with identical inputs validated fine, and a
forced degraded-start reproduction (valkey down at muster start) also
validated fine, so it is an in-process race in muster/mcp-oauth client
construction, not the degraded-recovery path per se.
**Workaround:** after the install, `up` now probes the real token path
(password grant -> Bearer on `/mcp`, `ensureMusterValidatesTokens`) and, on
rejection, replaces the muster pod once and re-probes.
**Unblocks:** giantswarm/muster — find the client construction that misses
`ExtraCAFile`/RootCAs (candidates: anything cloning `http.DefaultTransport`
before bootstrap swaps it, or an mcp-oauth client built without
`opts.RootCAs`) and thread the pool deterministically; then the probe can
stay but the bounce becomes dead code.

### U7. Backstage agent create flow: `agent-deployment` Template embedded as a copy
The GS Backstage build (0.199.x) hard-defaults the create flow's deploy to
`template:default/agent-deployment`, but ships no such entity — every
installation is expected to load it from the external
giantswarm/backstage-catalogs repo via a `catalog.locations` URL. Without it,
Deploy dies with `404 Template template:default/agent-deployment not found`
(observed 2026-08-28 on 0.199.9). The lab wants a hermetic catalog (no GitHub
fetch at boot), so it embeds a **verbatim copy** of the upstream template
(`internal/lab/templates/static/agent-deployment-template.yaml`, inlined into
the `backstage-catalog` ConfigMap via the `staticFile` template func — the
file bypasses Go templating because its `${{ … }}` scaffolder expressions
contain `{{ … }}`). The copy can drift from upstream; `agentlab
backstage-test` asserts the entity is registered, not that it matches.
**Unblocks:** giantswarm/backstage — bundle the hidden Template with the
backend (or point the default `deployTemplateRef` at an entity the image
registers itself) so an install works without a network catalog location;
then the embedded copy can be deleted.

### U8. `golang-adk:0.9.12` not published to gsoci — agent pods ImagePullBackOff
kagent-controller composes the runtime image for `runtime: go` agents from its
own version tag: `IMAGE_REGISTRY`/golang-adk:`IMAGE_TAG` =
`gsoci.azurecr.io/giantswarm/golang-adk:0.9.12` — with a `-full` suffix when
the agent mounts skills. gsoci has kagent-controller:0.9.12,
kagent-app:0.9.12 and kagent-skills-init:0.9.12, but golang-adk (both
variants) stops at 0.9.11 (verified 2026-08-28 via the tags API), so every
agent the platform creates — including everything deployed through the
Backstage create flow — sits in ImagePullBackOff. Purely a publish/retag gap
for the one repo.
**Workaround (automated):** `healADKImages` (`internal/lab/adk.go`, run by
`agentlab up`/`platform` when agents are enabled) resolves the tag kagent
will reference from the kagent-controller ConfigMap and, per variant
(plain/`-full`), pulls the real image first; only when the registry does not
have it does it retag the newest published older release in its place and
side-load it into the node. Self-converging: the pull-first order means the
moment upstream publishes the real tag, the stand-in is overwritten and
side-loaded on the next `up` — no manual cleanup. Failures downgrade to a
note; the platform install never blocks on this heal.
**Unblocks:** Giant Swarm image retagging — publish golang-adk (plain and
`-full`) at every kagent release tag alongside the other kagent images; the
heal then degenerates to an image preload and can eventually be deleted.

### U9. `components.kagent.postRenderers` patch: fixed nodePort pinned onto the kagent-ui Service
The lab host-publishes the kagent UI through a kind port mapping, which needs
a *stable* node-side port. `ui.service.type: NodePort` is a chart value, but
the upstream kagent chart's `ui-service.yaml` template renders no `nodePort`
field (verified in kagent 0.9.12 via the vendored wrapper chart 0.1.37), so
Kubernetes assigns a random one — useless to kind's fixed `extraPortMappings`.
A Kustomize strategic-merge patch in `components.kagent.postRenderers`
(`postrenderers.go`, since 2026-09-08; `agentlab post-render` before) pins the
Service's `ui` port entry to `config.KagentUINodePort` (30880); the kind config
maps that onto `platform.agentsPort` (default 8081) on the host.
**Unblocks:** kagent-dev/kagent — render `ui.service.ports.nodePort` when set
(the standard chart idiom). The value then moves into
`agent-platform-values.yaml.tmpl` and the patch is deleted.

### U10. Lab-owned NodePort Service selecting the agentgateway data-plane pods
The agentgateway edge (the chart-owned Gateway under
`gatewayApi.gateway.create`) is host-published through a kind port mapping,
which needs a *stable* node-side port. The data-plane Deployment/Service are
created by the agentgateway controller reconciling the Gateway — they are not
part of the Helm release, so neither chart values nor `agentlab post-render`
can pin the controller-created Service's NodePort. The lab applies its own
Service (`gateway-nodeport.yaml.tmpl`, NodePort `config.GatewayNodePort`
30443) selecting the data-plane pods by the standard
`gateway.networking.k8s.io/gateway-name` label the controller stamps on them;
the kind config maps 30443 onto `platform.gatewayPort` (default 443).
**Fragile if:** the controller changes its generated pod labels.
**Unblocks:** an agentgateway/AgentgatewayParameters knob for the generated
Service's nodePort; then this Service is deleted and the kind mapping targets
the controller's own Service.

### U11. Agent CRD patched with `spec.iconUrl` — BLOCKED UPSTREAM
The kagent package (giantswarm/kagent, ex-kagent-app) pins the upstream 0.9.x
CRDs, whose v1alpha2 Agent spec predates the A2A-card metadata fields added on
upstream main for 0.10 (kagent-dev/kagent#2188). The Backstage create flow
composes `agent.iconUrl` (the deterministic `avatars.<baseDomain>` URL)
whenever the installation has a `baseDomain` — this lab always sets one — the
`agent` chart renders it into `Agent.spec.iconUrl`, and server-side apply
rejects the whole HelmRelease: `.spec.iconUrl: field not declared in schema`.
The agents list then stays empty with no visible error, because the scaffolder
task only kube-applies the HelmRelease and reports success. Not lab-specific:
any installation with a configured `baseDomain` fails the same way.
**Workaround (automated):** `patchAgentCRDIconURL` (`internal/lab/kagentcrd.go`,
run by `agentlab up`/`platform` when agents are enabled) adds upstream main's
`iconUrl` property (optional string, stored and ignored by the 0.9.x
controller) to the installed CRD's v1alpha2 schema. Idempotent, and a no-op
once the CRD already carries the field — a kagent bump retires it silently.
**Unblocks:** giantswarm/kagent#55 — ship CRDs that declare the A2A-card
metadata fields (backport or the 0.10 bump); then delete `kagentcrd.go`.

### U12. kube-prometheus-stack: `kyvernoPolicyExceptions.enabled: false` required on Kyverno-less clusters
The GS kube-prometheus-stack wrapper (22.0.0) renders a `kyverno.io/v2alpha1
PolicyException` for node-exporter's host access whenever node-exporter is
enabled. The template's API-version selection has a final fallback that
carries **no** `.Capabilities.APIVersions.Has` guard, so on a cluster without
Kyverno (this lab) the object renders anyway and the install fails on the
missing CRD. The lab needs node-exporter (node CPU/memory is half the point
of `platform.observability`), so the exceptions are switched off wholesale in
`kube-prometheus-stack-values.yaml.tmpl` — correct here regardless (no
Kyverno to except from), but the toggle exists only because of the missing
guard.
**Unblocks:** giantswarm/kube-prometheus-stack-app — guard the PolicyException
fallback with a Capabilities check like the sibling branches; then the value
can be dropped (it would render nothing here either way).

### U13. `postRenderers` patch: `dex-localhost` sidecar on the MCP servers — ACCEPTED
Since 2026-09-03 the bundled mcp-kubernetes, model-manager and agent-manager and
the lab's mcp-prometheus validate the user's forwarded Dex id_token themselves
(mcp-oauth resource servers against `global.identity`), so each of them does
OIDC discovery and JWKS fetches against the issuer URL — `https://localhost:<dexPort>/dex`, the
one URL the browser, the apiserver and every pod must share (H-issuer, above).
muster and Backstage reach it through hostNetwork (U1); these four cannot: all
listen on :8080 and would collide on the single kind node. **Fix:** a
`dex-localhost` sidecar (`alpine/socat`) that listens on the pod's own loopback
:<dexPort> (IPv6 wildcard, dual-stack) and forwards to the Dex ClusterIP
Service, so `localhost` resolves inside the pod exactly as on the host; Dex's
certificate carries `localhost`, TLS verification against the lab CA holds.
Since 2026-09-08 a Kustomize strategic-merge patch in
`components.<server>.postRenderers` (`postrenderers.go`; the post-renderer
binary before), and the same patch on the lab's own mcp-prometheus HelmRelease
(mcp-prometheus.yaml.tmpl). Lab-only by construction: real installations have a
routable issuer.

### U14. `hostmodels.go`: the further host backends are wired as static ModelConfigs — FIXED upstream
`platform.modelManager.backends` lists every model server on the lab host
(`agentlab configure` fills it from what answers: an Ollama, a Lemonade
Server), but model-manager 0.16.0 fronts ONE backend per instance
(`backend: ollama|kserve|lemonade`). Timo's direction (agentlab#60,
2026-09-04) is one multi-backend model-manager, not one release per backend
(the draft that did that, #61, is closed). Until it lands, the lab installs
model-manager with the first backend and, for every further one, renders its
downloaded tool-calling models as lab-labeled ModelConfigs in exactly the
shape model-manager writes for that backend (`lemonade-<model>`: `OpenAI` on
`<endpoint>/api/v1`, placeholder key; `ollama-<model>`: the native keyless
provider), refreshed and pruned on every `agentlab platform` run and proven
by the agent turn `models-test` ends with. Nothing manages those models
(pull/delete stay CLI-on-host) and the portal's Models pages show the first
backend only.
**Fix:** model-manager 0.17.0 fronts several backends in one process
(`--backends`, `GET /api/v1/backends`, `backend` on every object and request,
one ModelConfig per (backend, model) carrying the
`model-manager.giantswarm.io/backend` label), the fleet's connectivity chart
3.7.0 and the umbrella (v0.35.15+) take `model-manager.backends`. The lab now
hands the whole list to `model-manager.backends` (a single entry renders the
`backend:` form), preflights every server, and `hostmodels.go` keeps only the
inventories (discovery summary, the delete check of `models-test`); the static
wiring, its pruning and its agent-turn proof are gone. `models-test --backend
lemonade` proves the second backend through model-manager instead. The
`agentlab.yaml` shape did not change.

### U15. Backstage app-config overlay restates `baseUrl`/`cors.origin` with `platform.gatewayPort` — ACCEPTED
The umbrella's Backstage app-config renders `app.baseUrl`, `backend.baseUrl`
and `backend.cors.origin` as `https://<hostname>` — no port, because
`components.backstage.hostname` is also the HTTPRoute hostname and cannot
carry one. With `platform.gatewayPort != 443` Backstage's OAuth `redirect_uri`
is port-free while the Dex client (`dex.yaml.tmpl`) registers the ported one,
and every login fails with "Unregistered redirect_uri". The lab's app-config
overlay (`backstage-catalog.yaml.tmpl`) restates the three URLs from
`.BackstageBaseURL`; at 443 the values are identical. In-cluster, the lab's
edge Service (`gateway-nodeport.yaml.tmpl`) serves that port as well, so the
ported URLs resolve from pods too (agentlab#67). Lab-only by construction: a
real installation runs its edge on 443, so the umbrella has no reason to
carry a ported public URL; the overlay is the lab's permanent answer, not an
interim.

### U16. Podman: the side-load is one archive per image — OPEN, FIXABLE IN THE LAB
A multi-image `docker save -o <tar> a b c` under Podman's docker-compatible
CLI writes ONE image carrying every name as a tag unless
`--multi-image-archive` is passed, so a batched archive lands one image under
all the tags (the Flux controllers crashlooped running flux-cli). Under podman
(`runtime.go`) the lab therefore saves and imports one archive per image
(`kindLoadImages`); under docker the batch is saved per platform (U21). The
save is the lab's own `docker save` now (the import is the embedded kind's
`nodeutils.LoadImageArchive`), so the lab could pass podman's
`--multi-image-archive` itself and batch there too — not done for lack of a
podman host to verify the flag through the compatible CLI on.

### U17. Rootless Podman: the edge moves off 443 — NOT A BUG, A HOST LIMIT
Rootless Podman publishes ports from the invoking user's network namespace,
where the kernel refuses everything below
`net.ipv4.ip_unprivileged_port_start` (1024). A bind probe alone reads a free
443 as usable, because the lab's own process cannot bind it either way, so
`configure` asks the engine instead (`MinPublishablePort`, `runtime.go`) and
`ChooseFreePorts` moves the edge to 8443. Resolves for a user who runs Podman
as root or lowers the sysctl; nothing to fix in the lab.

### U18. `oauth-fixture.yaml.tmpl`: the sign-in fixture pins Dex as its authorization server — BLOCKED UPSTREAM
The fixture points muster at its own protected `/mcp`, whose RFC 9728 metadata
names muster's own OAuth 2.1 server. That server identifies muster-as-client by
Client ID Metadata Document, and mcp-oauth's SSRF guards refuse the metadata
URL — `muster.127.0.0.1.nip.io` resolves to the edge's cluster IP in-cluster
(the CoreDNS rewrite) and to loopback everywhere else (`client_id metadata URL
resolves to private/internal IP address … (SSRF protection)`); dynamic
registration is refused the same way for the proxy callback's redirect URI
(`hostname resolves to private IP address (DNS rebinding protection)`). muster
exposes none of mcp-oauth's knobs for a lab (`AllowPrivateIPClientMetadata`,
`DisableDNSValidation`, `AllowPrivateIPRedirectURIs`), so no sign-in could
complete against it and the fixture was a challenge generator only. **Fix in
the lab:** the CR pins the lab Dex through `spec.auth.authorizationServer`
(Dex's `authorizationEndpoint`/`tokenEndpoint`, `clientCredentialsSecretRef` →
the platform client's id/secret in the Secret `lab-oauth-fixture-client`,
`scopes` — a pinned server gets no default scope and Dex refuses a request
without `openid` — and as `issuer` muster's own public URL, so the grant's
token-store key stays apart from muster's own login issuer (Dex) and a
`core_auth_logout` clears it; a pin naming Dex's URL as the issuer works since
muster 5.12.1 (muster#1174, muster#1175: the grant key follows the pin and a
changed pin takes effect without a restart) but leaves the grant behind on
logout, which would make the next run's sign-in proof reconnect without a
challenge), and `dex.yaml.tmpl` lists
muster's proxy callback on that client. Dex matches redirect URIs exactly and
the token it issues carries the platform client's audience, which the endpoint
trusts, so `agentlab toolsets-test` completes the sign-in headlessly. Unblocks
when muster exposes `allowPrivateIPClientMetadata` for its OAuth server — then
the pin can go and the challenge chain becomes proxy start → muster
`/oauth/authorize` → Dex again.

### U19. `components.agent-platform-connectivity.postRenderers`: the kagent controller metrics Service selects the wrong instance label — FIXED upstream
The connectivity chart (up to 3.20.1) rendered a Service for the kagent
controller's metrics port and a ServiceMonitor on it, selecting the controller
pods with `app.kubernetes.io/instance: <its own release name>`. Under the
standalone umbrella every subchart shared one release name, so that matched;
under the meta chart kagent is its own release named `kagent`, the Service
selected no pod, and the lab Prometheus scraped nothing of kagent
(`platform-test` "Prometheus scrapes the platform itself" caught it on the
first meta-chart run). The lab carried a Kustomize strategic-merge patch on
the Service's selector in `components.agent-platform-connectivity.postRenderers`.
**Fixed:** agent-platform 3.20.2
([giantswarm/agent-platform#305](https://github.com/giantswarm/agent-platform/issues/305),
PR #308) — the Service selects kagent's own release name. The patch is
deleted; the lab renders no `postRenderers` for the connectivity component,
and the lab's Prometheus scrapes the kagent controller through the chart's
own Service.

### U20. `platform.go`: the `kagent` namespace is created before the chart — FIXED upstream
The kagent chart renders its workloads into `kagent.namespaceOverride`
(`kagent`), but its HelmRelease targets the release namespace like every
component, so helm-controller's `createNamespace` never creates `kagent`; the
one chart object that did — the connectivity chart's Namespace — sits in a
release that `dependsOn` kagent. A first install on a fresh cluster failed
every kagent attempt with `namespaces "kagent" not found` until the retries
were exhausted, and everything behind kagent waited (seen on the first
meta-chart run: 6 attempts, then Stalled). The lab created the namespace in
`platformUp` when agents are on, the way management clusters do in their
bases, and the connectivity release adopted it.
**Fixed:** agent-platform 3.20.2
([giantswarm/agent-platform#306](https://github.com/giantswarm/agent-platform/issues/306),
PR #308) — with the bundled engine and kagent on, a `pre-install,pre-upgrade`
hook Job (`<release>-kagent-namespace`, weight -8) creates the namespace when
it is missing (and waits out a `Terminating` one left by a previous
uninstall) ahead of the kagent HelmRelease; the connectivity release adopts
it on install and deletes it with the release. `ensureNamespace` for kagent
is deleted; a fresh `agentlab up` gets the namespace from the hook.

### U21. `preload.go`: the docker side-load is `docker save --platform` + kind's archive import — BLOCKED UPSTREAM
kind's `load docker-image` runs a plain `docker save` and pipes the archive
into the node's `ctr images import --all-platforms`. Under Docker's containerd
image store — the default on Docker Desktop and on new Docker 29 installs —
that archive carries a pulled image's whole multi-platform index while only
the host platform's blobs were ever pulled, and the import fails on the first
missing digest: `failed to load image: command "docker exec ... ctr
--namespace=k8s.io images import --all-platforms ..." failed with error: exit
status 1` / `content digest sha256:...: not found`, once per side-load lane,
every image degrading to the in-node pull the preload exists to avoid
([kubernetes-sigs/kind#3795](https://github.com/kubernetes-sigs/kind/issues/3795),
open since 2024-11; the maintainers point consumers to `docker save --platform
| kind load image-archive` and will not lock the platform inside `kind load`).
The lab does exactly that, with kind embedded (`kind.go`): the load path of
kind's `load docker-image` command is not used at all — `dockerLoadImages`
asks `docker image inspect` which platform the host holds each ref in, writes
one `docker save --platform <p> -o <tmp.tar>` per platform and imports each
archive with `nodeutils.LoadImageArchive`, the library call behind `kind load
image-archive`. Needs Docker 28 (API 1.48) for `docker save --platform`; an
older client/daemon gets one plain archive of the batch, which is right under
the classic graph driver. Podman keeps its one-archive-per-image load (U16).
Unblocks when kind's `load docker-image` logic (the re-tag of an image ID the
node already has, the per-image save) survives the containerd image store and
is worth reusing over the plain archive import.

### U22. `substratepools.go`: Substrate's CA/JWT pool bootstrap is a Go port of `kubectl-ate` — BLOCKED UPSTREAM
The substrate chart (0.0.26) mounts four pool Secrets, a trust-anchor Secret
and an authentication ConfigMap it does not render. Upstream's install is
`helm install`, then `kubectl-ate admin make-ca-pool` / `make-jwt-pool` plus a
shell step (jq + openssl for the trust anchor, a heredoc for the
authentication config), then a second `helm upgrade --wait` — the first
install's pods restart on missing volumes until then. The lab downloads no
binaries (kind, Helm and client-go are embedded; `kubectl-ate` is unsigned)
and upstream publishes no image with the bootstrap in it (`ate-setup` is not
published), so `substratepools.go` copies the generate + serialise subset of
substrate's internal `localca` and `localjwtauthority` packages (Apache-2.0
header kept, pinned to 0.0.26 in the comment) and `substrate.go` creates every
bootstrap object BEFORE one waited install; the pre-created
`podcertificate-controller-system` namespace the chart also renders is adopted
with Helm's `--take-ownership`. Drift risk: a Substrate bump that changes the
pool wire format shows up as ate-api-server never Ready. The issuer too:
upstream's default authentication config names `https://kubernetes.default.svc`,
which ate-api-server rejects against kind's tokens (their `iss` is
`…svc.cluster.local`); the lab reads the issuer off the apiserver's discovery
document, as upstream's own `ate-setup` does. Unblocks when the substrate
chart renders the bootstrap (a hook Job) or the packages become importable.

### U23. LM Studio has no delete over its API — `models-test` proves the refusal — BLOCKED UPSTREAM
LM Studio's own API (`/api/v1`, 0.4.0+) serves the library, the download that
backs a pull, and load/unload — but no delete. Removing a model is `lms rm`
on the host, a CLI no pod can run, so the `lmstudio` backend of model-manager
reports `delete: false` and the platform answers `501 unsupported`.
**Consequence in the lab:** `agentlab models-test --backend lmstudio` is the
one proof run that leaves something behind — the model it pulls stays
downloaded, and the run's last line says so with the `lms rm` command. Rather
than skip the step, the run asserts the refusal (a stronger check: the
platform must refuse rather than pretend, nothing may be removed by a refused
delete, and the ModelConfig must still come off through `POST
/models/unwire`), and it cross-checks the advertised `delete` capability
against what the server really offers in both directions. **Unblocked by** an
LM Studio release that exposes over its API what `lms rm` does; the lab side
is then one `deleteOverREST: true` in `internal/lab/backends.go` plus the
driver's capability flag.

### U24. LM Studio identity cannot be read from a status code — ACCEPTED
Every other host model server answers a version or health document, so the
discovery could ask "HTTP 200 with a `version` field?". LM Studio has no
version, health or system-info endpoint anywhere, **and it answers HTTP 200
with an `{"error": …}` body for every path outside its own `/api/v1`** —
Ollama's `/api/version`, `/api/tags` and `/api/show` among them (verified
against 0.4.20; paths under `/api/v1` do 404). A probe that trusted the
status code would therefore find an "Ollama" on every LM Studio port.
**Fix:** detection is a per-backend fingerprint over the response *body*
(`backendProbe` in `internal/lab/backends.go`) — LM Studio is recognised by
an `/api/v1/models` document carrying a `models` array whose entries are
keyed by `key`, which also distinguishes it from Lemonade serving the very
same path with a `data` envelope. The discovery prints `api v1` where the
others print a version. Not a workaround to remove: the body is the only
evidence such a server offers. The unit tests carry the 200-on-unknown-path
behaviour in the fake, so a future "simplification" back to a status check
fails them.

## Accepted lab trade-offs (not hacks to fix)

- **Checksum stamping via the `REPLACED_AT_APPLY` placeholder** — the standard
  Helm `checksum/config` pattern, done with sed because there is no templating
  engine here. Deduplicated into one script (H3), otherwise kept.
- **Password grant + static secrets in git** (`kubernetes-lab-secret`, bcrypt
  of "password", the Backstage session key) — the lab's identity is
  self-contained and throwaway by design; nothing here guards anything real.
- **`skipTLSVerify: true`** for the Backstage→apiserver hop — the alternative
  is minting the kind CA into the Backstage trust store; zero value in a lab.
- **`test-backstage.sh` regex-parsing `decodeURIComponent('…')` out of the
  login response** — it drives a browser postMessage flow headlessly; there is
  no API that returns this payload cleanly. Inherent to the test's job.
- **`vendor/agent-platform-standalone/hack/curate.sh`** — upstream's file,
  vendored and gitignored; out of scope here.
- **`login-browser.py` fixed callback port 5555** — must be pre-registered in
  Dex's `redirectURIs`; a random port would break the static client. By design.
- **The OAuth sign-in fixture aggregates muster itself** (`lab-oauth-fixture`
  → muster's own protected `/mcp`, `internal/lab/oauthfixture.go`) — the one
  way to get a downstream that stays `Auth Required` behind an authorization
  server that accepts muster's CIMD client id without adding a workload (Dex
  knows only static clients). Costs: `Failed` for about a minute after a muster
  restart (self-dial before the listener is up; `agentlab platform` waits it
  out), and a completed sign-in connects muster to itself. Both accepted; see
  docs/platform.md "Signing in to a downstream server".
- **A twice-replaced, once-trusted CA lingers only until the next trust op** —
  a `platform.domain` change stashes the outgoing CA under `certs/replaced/`,
  and both `agentlab trust` and `untrust` sweep every stashed CA out of the
  trust stores before doing their own work. The stash (not the store) is the
  source of truth for what to remove, because the stores index roots by
  name+serial and the serial dies with the overwritten `ca.crt` otherwise.

## Full-stack verification (2026-08-27, shell lab)

With every fix above in place, one full cycle on a cold cluster:

| Step | Result |
|---|---|
| `make up` (fresh kind cluster, **no apiserver bounce**) | issuer up, apiserver accepts Dex tokens |
| `make test` | 10/10 RBAC assertions pass for admin/dev/viewer |
| `make platform` | deps rebuilt via digest gate, MCPServer `Connected`, muster live on :8090 |
| `make platform-test` | Dex → muster → mcp-kubernetes → apiserver chain passes |
| `make backstage` | image loaded once (exact-tag check), pod up |
| `make backstage-test` | all three users sign in, reach muster, see workflows/tools |
| `make backstage` re-run | no image reload, same pod, same revision (checksum no-op) |
| `make reload` | no-op apply, Dex stays at revision 1 |
| `make down` | cluster deleted, no leftovers |

## Full-stack verification (2026-08-27, Go binary)

The same cycle through `agentlab`, on a cold cluster, defaults from
`agentlab configure --defaults`:

| Step | Result |
|---|---|
| `agentlab configure --defaults` / `--platform --backstage` | agentlab.yaml written, bcrypt hashes cached |
| `agentlab up` (fresh kind cluster) | issuer up, apiserver accepts Dex tokens |
| `agentlab test` | 10/10 RBAC assertions pass for admin/dev/viewer |
| `agentlab login dev@lab.local` | kubeconfig.oidc works, `kubectl auth whoami` = `oidc:dev@lab.local` |
| `agentlab platform` | deps built via digest gate, MCPServer `Connected`, muster live on :8090 |
| `agentlab platform-test` | Dex → muster → mcp-kubernetes → apiserver chain passes |
| `agentlab backstage` | image loaded once (exact-tag check), pod up |
| `agentlab backstage-test` | all three users sign in, reach muster, see workflows + 29 core tools |
| `agentlab backstage` re-run | no image reload, same pod (checksum no-op) |
| `agentlab reload` (unchanged config) | no-op apply, single ReplicaSet |
| `agentlab up` re-run (components enabled) | idempotent: cluster reused, secrets kept, post-render patches survive the helm upgrade |
| edit a user in agentlab.yaml + `agentlab reload` | pod rolls (checksum), **immediate** login as the new user succeeds (H11) |
| custom config (`agentlab2`, Dex :31000, run from an empty dir) | second cluster up alongside the first, 10/10 RBAC, clean `down` |
| `agentlab down` | cluster deleted, no leftovers |

Form coverage: `go test ./...` drives the real huh form with scripted
keystrokes (accept defaults, edit fields, toggle components) and unit-tests
the post-renderer against a synthetic Helm release.
