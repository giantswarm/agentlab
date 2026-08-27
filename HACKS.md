# Hack audit

Inventory of every hack/workaround in the lab's scripts, what it papers over,
and what happened to it. One commit per resolved item. Statuses:

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

### H2. `backstage.sh`: unconditional `rollout restart` + secret applied after the Deployment — PENDING
The `dex-ca` secret was created *after* `kubectl apply -f manifests/backstage.yaml`,
so on first boot the pod sat in ContainerCreating waiting for a volume that did
not exist yet; an unconditional `rollout restart` then bounced the (2.4 GB,
Rosetta-emulated) pod on *every* re-run to compensate for possible cert changes.
**Fix:** create the secret before applying the Deployment, and stamp a
`checksum/dex-ca` annotation into the pod template (same pattern `dex.yaml`
already uses for its config) so the pod rolls exactly when the CA changes and
no-op re-runs stay no-ops.

### H3. `up.sh` + `Makefile reload`: duplicated sed/checksum stamping — PENDING
The `sed "s/REPLACED_BY_UP_SH/$SUM/" manifests/dex.yaml | kubectl apply` logic
lived in two places (up.sh and the `reload` target) and would drift.
**Fix:** extracted `scripts/apply-dex.sh`; both callers use it.

### H4. `up.sh`: dex-tls secret created after the Dex Deployment — PENDING
Same ordering smell as H2: on first boot the Dex pod waited on a secret that
was applied a step later.
**Fix:** namespace + dex-tls secret are applied before the Deployment.

### H5. `restart-apiserver.sh`: blind `sleep 5` between manifest removal and restore — PENDING
The static-pod bounce moved the manifest away, slept 5 seconds, and moved it
back. If the kubelet had not yet noticed the removal, the restore was a no-op
and the apiserver never restarted — a silent false success.
**Fix:** poll `crictl ps` inside the node until the kube-apiserver container is
actually gone before restoring the manifest, and fail loudly on timeout.

### H6. Is the apiserver bounce needed at all on Kubernetes 1.35? — TO VERIFY during boot
The whole reason for `restart-apiserver.sh` is the claim that the OIDC
authenticator gives up discovery after ~40 s and never retries. To be verified
empirically on the next cold boot: create the cluster, wait for Dex, do NOT
bounce the apiserver, and watch whether tokens start being accepted. If the
1.35 authenticator recovers on its own, the script gets deleted; if not, the
(hardened, see H5) bounce stays and this entry records the evidence.

### H7. `platform-up.sh`: MCPServer "Connected" wait loop cannot fail — PENDING
The `for … seq 1 40` loop printed nothing and fell through silently when the
MCPServer never reached `Connected`; the script then declared the platform up.
**Fix:** the loop now reports the last observed state and exits non-zero on
timeout, pointing at `make platform-logs`.

### H8. `platform-test.sh`: fixed temp paths under /tmp — PENDING
Response headers/bodies went to `/tmp/.mcp-h` / `/tmp/.mcp-b` — collision-prone
between concurrent runs and left behind afterwards.
**Fix:** `mktemp` + `trap … EXIT` cleanup.

### H9. `platform-up.sh`: mtime-based chart-dependency freshness check — PENDING
`[[ Chart.lock -nt charts ]]` plus a `touch charts` stamp. mtimes lie (git
checkout order, copies, clock skew) and the failure mode is a stale charts/
dir silently installed.
**Fix:** store the sha256 of Chart.lock in `charts/.lock-digest` after a
successful `helm dependency build` and rebuild whenever it differs.

### H10. `gen-certs.sh`: private keys chmod 644 — PENDING
`chmod 644 certs/*.key` made the CA and server keys world-readable. Nothing
needs that: the kind mount is read by root in the node container regardless,
and the secrets are created from file by the invoking user.
**Fix:** 600 on keys, 644 on certs.

## Blocked upstream (documented, not fixable in this repo)

### U1. `muster-post-render.sh` patch: `allowPublicClientRegistration` edited into the rendered ConfigMap
The muster chart exposes the key in values.yaml, values.schema.json, README
and unit tests, but `templates/configmap.yaml` never renders it, so the value
is silently dropped and Claude Code's DCR registration dies with
"Registration requires authentication". **Verified still broken in muster
chart 5.6.2** (latest stable as of 2026-08-27; templates are byte-identical to
5.5.6 — only Chart.yaml differs). Neither alternative gate can work for a
public loopback client (no token, random port, http/https stripped from
allowed schemes by mcp-oauth validation).
**Unblocks:** giantswarm/muster — render the key in
`helm/muster/templates/configmap.yaml` (`aggregator.oauth.server.allowPublicClientRegistration`).
The values entry in `manifests/agent-platform/values.yaml` is kept so the
values file tells the truth; delete the yq patch once the chart renders it.

### U2. `muster-post-render.sh` patch: hostNetwork + dnsPolicy + maxSurge 0 on the muster Deployment
muster must resolve `https://localhost:32000/dex` to the Dex NodePort from
inside the pod (the lab's one-issuer-URL trick), which needs hostNetwork; and
with hostNetwork a default rolling update deadlocks on a single node because
both pods want :8090. The muster chart (checked through 5.6.2) exposes no
`hostNetwork`, `dnsPolicy` or `strategy` values.
**Unblocks:** giantswarm/muster — add deployment-level `hostNetwork`/
`dnsPolicy`/`strategy` values. Until then a post-renderer is the correct
plain-Helm mechanism; the patch is surgical so chart bumps need no hand-copying.

### U3. `muster-post-render.sh` patch: umbrella HTTPRoute stripped + placeholder parentRefs
`agent-platform-standalone`'s `_helpers.tpl` hard-fails on empty
`ingress.parentRefs` in **all** modes — even `muster-direct`, where this lab
has no Gateway (and no Gateway API CRDs) at all. The values carry a
`no-gateway-in-this-lab` placeholder to pass the guard and the rendered route
is stripped.
**Unblocks:** giantswarm/agent-platform-standalone — require `parentRefs` only
in `agentgateway-*` modes, or add an `ingress.enabled: false` escape hatch for
gateway-less clusters.

### U4. `platform-up.sh`: chart vendored from git at a pinned SHA
The umbrella chart has no OCI release: `charts/giantswarm/agent-platform-standalone`
does not exist in gsoci (verified 2026-08-27, `NAME_UNKNOWN`) and PR
[giantswarm/agent-platform-standalone#11](https://github.com/giantswarm/agent-platform-standalone/pull/11)
is still open (its head is exactly the pinned `APS_REF`).
**Unblocks:** merge #11 and publish the chart; then the vendor block becomes
`helm upgrade --install … oci://gsoci.azurecr.io/charts/giantswarm/agent-platform-standalone`.

## Accepted lab trade-offs (not hacks to fix)

- **Checksum stamping via `REPLACED_BY_UP_SH` placeholder** — the standard
  Helm `checksum/config` pattern, done with sed because there is no templating
  engine here. Deduplicated into one script (H3), otherwise kept.
- **Password grant + static secrets in git** (`kubernetes-lab-secret`, bcrypt
  of "password", the Backstage session key) — the lab's entire point is
  self-contained throwaway identity; nothing here guards anything real.
- **`skipTLSVerify: true`** for the Backstage→apiserver hop — the alternative
  is minting the kind CA into the Backstage trust store; zero value in a lab.
- **`test-backstage.sh` regex-parsing `decodeURIComponent('…')` out of the
  login response** — it drives a browser postMessage flow headlessly; there is
  no API that returns this payload cleanly. Inherent to the test's job.
- **`vendor/agent-platform-standalone/hack/curate.sh`** — upstream's file,
  vendored and gitignored; out of scope here.
- **`login-browser.py` fixed callback port 5555** — must be pre-registered in
  Dex's `redirectURIs`; a random port would break the static client. By design.
