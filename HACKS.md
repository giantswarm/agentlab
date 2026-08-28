# Hack audit

Inventory of every hack/workaround in the lab's scripts, what it papers over,
and what happened to it. One commit per resolved item. Statuses:

> **2026-08-27, Go port:** the lab was rewritten as a single Go binary
> (`agentlab`) with form-driven configuration; scripts/ and manifests/ are gone.
> Every fix below carries over — the checksum stamping (H2/H3), the digest
> gate (H9), the exact-tag image check (H1), key permissions (H10, now 0600 at
> creation) and the post-render patches (U1-U3, now `agentlab post-render`) all
> live in `internal/lab/`. Two NEW items surfaced during the port: H11, H12.

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

### H12. Chart vendored into `vendor/` collides with the Go toolchain — FIXED
The agent-platform-standalone checkout lived in `vendor/`, which flips a Go
module into vendored-build mode: after the first `agentlab platform`, `go build`
failed with "inconsistent vendoring".
**Fix:** the chart is vendored into `.vendor/` instead.

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

- **Checksum stamping via the `REPLACED_AT_APPLY` placeholder** — the standard
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

TUI form coverage: `go test ./...` drives the real huh form with scripted
keystrokes (accept defaults, edit fields, toggle components) and unit-tests
the post-renderer against a synthetic Helm release.
