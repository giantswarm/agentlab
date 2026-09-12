#!/usr/bin/env bash
# Stage 2a of the migration rehearsal (agentlab#143): the seeded 3.x lab
# upgraded in place to a 4.x meta chart, the migrate Job's expand run asserted.
#
# Usage:  cd <lab dir> && upgrade.sh <evidence-dir> <target-version>
#
# Inputs (environment, all optional):
#   LAB_DIR         the lab directory (default: $PWD; needs agentlab.yaml, state/)
#   WANT_AGENTLAB   the agentlab release the run must use (default: v0.39.1)
#   SCRATCH         where the meta chart is pulled for the pre-flight helm template
#   STATUS_FILE     append-only status log; no status lines when unset
#   STATUS_PREFIX   prefix of every status line (default: agentlab#143-stage2a)
#   AGENTLAB, YQ    binaries (default: agentlab on PATH; a mikefarah yq v4)
#   ANTHROPIC_API_KEY  read from the Secret kagent/kagent-anthropic when unset; never printed
#   START_STEP      resume at this step on a lab already on the target, skipping
#                   the earlier ones: before | config | upgrade | assert-platform
#                   (alias: assert) | migrate-run1 | gitops-pr | record; preflight
#                   always runs, the skipped steps' summary fields read -
#   REPORT_RUN1     a saved copy of the first expand report when the migrate Job
#                   re-ran since (the report ConfigMap holds only the latest run):
#                   the rewrite is asserted from the copy, the live report must
#                   say unchanged for the rewritten releases
#
# The recipe is docs/migration-rehearsal.md. A platform defect stops the run
# with exit 2 and a FOUND-ISSUE.md in the evidence dir; the lab is left as is.
set -Eeuo pipefail

EVIDENCE="${1:?usage: upgrade.sh <evidence-dir> <target-version>}"
TARGET="${2:?usage: upgrade.sh <evidence-dir> <target-version>}"
LAB_DIR="${LAB_DIR:-$PWD}"
WANT_AGENTLAB="${WANT_AGENTLAB:-v0.39.1}"
SCRATCH="${SCRATCH:-${TMPDIR:-/tmp}/agentlab-migration-rehearsal}"
STATUS_FILE="${STATUS_FILE:-}"
STATUS_PREFIX="${STATUS_PREFIX:-agentlab#143-stage2a}"
AGENTLAB="${AGENTLAB:-agentlab}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OVERLAY="$HERE/values-4x-migration.yaml"
GITOPS_MANIFEST="$HERE/seed-gitops.yaml"
GITOPS_1X_MANIFEST="$HERE/seed-gitops-1x.yaml"
MIGRATE_SELECTOR='app.kubernetes.io/component=agent-manager-migrate'
GITOPS_NS=flux-giantswarm
START_STEP="${START_STEP:-before}"
REPORT_RUN1="${REPORT_RUN1:-}"
STEPS=(before config upgrade assert-platform migrate-run1 gitops-pr record)

# The summary fields the steps fill; a resumed run leaves the skipped steps' at -.
KAGENT_CHART=- SUBSTRATE_CHART=- LEFTOVER_AGENTS=- K8S_AGENT_STATE=- K8S_AGENT_REPORT=-
SRE_REMOVED=- NARROW_REMOVED=- NARROW_RENAMED=- SRE_PINNED=- PENDING=- GITOPS_IDENTITY=-

export AGENTLAB_TELEMETRY_TESTMODE=1 AGENTLAB_NO_UPDATE_CHECK=1

# ---------------------------------------------------------------- helpers ---

# shellcheck source-path=SCRIPTDIR
# shellcheck source=lib.sh
source "$HERE/lib.sh"
trap on_error ERR

# A platform defect, not a bug of this script: record it, stop, leave the lab.
found_issue() {
  log "FOUND-ISSUE: $*"
  { echo "## $(utc) step=$CURRENT_STEP"; printf '%s\n\n' "$*"; } >>"$EVIDENCE/FOUND-ISSUE.md"
  status "FOUND-ISSUE $* — lab left exactly as is (lock HELD, no restore, no hand-patching)"
  trap - ERR
  exit 2
}

# Timings survive a resumed run in the evidence dir.
save_t() { printf '%s\n' "$2" >| "$EVIDENCE/t-$1"; }
load_t() { cat "$EVIDENCE/t-$1" 2>/dev/null || echo -; }

hr_table() { # every HelmRelease of the platform namespace with its Ready condition
  kubectl -n agent-platform get helmrelease \
    -o custom-columns='N:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,CHART:.status.history[0].chartVersion,MSG:.status.conditions[?(@.type=="Ready")].message' 2>&1
}
hr_ready() { kubectl -n "$1" get helmrelease "$2" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null; }
hr_conditions() { kubectl -n "$1" get helmrelease "$2" -o jsonpath='{.status.conditions[*].message}' 2>/dev/null; }

# The HelmRelease a not-ready one reports for: a dependency chain
# ("dependency 'ns/name' is not ready") is followed to its root.
hr_root() { # <ns> <name>
  local ns="$1" name="$2" msg dep i=0
  while [ "$i" -lt 10 ]; do
    msg=$(kubectl -n "$ns" get helmrelease "$name" -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}' 2>/dev/null)
    dep=$(sed -nE "s#.*dependency '([^/]+)/([^']+)' is not ready.*#\1 \2#p" <<<"$msg")
    [ -n "$dep" ] || break
    read -r ns name <<<"$dep"; i=$((i + 1))
  done
  echo "$ns $name"
}
HELM_FAILURE_RE='Helm (upgrade|install|rollback)|field is immutable|RetriesExceeded|failed for release'

assert_hr_ready() { # <ns> <name> [<chart regex>]
  local ready chart
  ready=$(hr_ready "$1" "$2")
  chart=$(hr_chart_version "$1" "$2")
  if [ "$ready" != True ]; then
    # A failed Helm action of the root release is the platform's defect, not this script's.
    local rns rname rmsg
    read -r rns rname <<<"$(hr_root "$1" "$2")"
    rmsg=$(kubectl -n "$rns" get helmrelease "$rname" -o json 2>/dev/null \
      | jq -r '[.status.conditions[]? | select(.type=="Ready" or .type=="Released" or .type=="Stalled") | .type + "=" + .status + " " + .reason + ": " + .message] | join(" | ")')
    kubectl -n "$rns" get helmrelease "$rname" -o yaml >| "$EVIDENCE/hr-$rname.not-ready.yaml" 2>&1 || true
    helm -n "$rns" history "$rname" >| "$EVIDENCE/helm-history-$rname.txt" 2>&1 || true
    if /usr/bin/grep -Eq "$HELM_FAILURE_RE" <<<"$rmsg"; then
      found_issue "HelmRelease $rns/$rname (the root of $1/$2 Ready=$ready) failed its Helm action: chart $(hr_chart_version "$rns" "$rname"), attempted $(kubectl -n "$rns" get helmrelease "$rname" -o jsonpath='{.status.lastAttemptedRevision}'): $(head -c 900 <<<"$rmsg"); evidence $EVIDENCE/hr-$rname.not-ready.yaml"
    fi
    die "HelmRelease $1/$2 Ready=$ready chart=$chart (root $rns/$rname): $rmsg"
  fi
  if [ -n "${3:-}" ]; then [[ "$chart" =~ ^$3$ ]] || die "HelmRelease $1/$2 chart $chart, want $3"; fi
  log "HelmRelease $1/$2 Ready chart $chart"
}

# The refusal the 4.x kagent-crds release hits on the CRDs the 0.10 wrapper
# installed unowned from its crds/ dir: Helm ownership, or the apiserver storage-version
# rule (status.storedVersions keeps v1alpha2, the 0.11 CRDs carry only v1alpha3).
CRD_REFUSAL_RE='invalid ownership metadata|storedVersions|is invalid:|cannot be imported into the current release|rendered manifests contain a resource that already exists'
kagent_crds_refusal() {
  kubectl -n agent-platform get helmrelease kagent-crds -o json 2>/dev/null \
    | jq -r '.status.conditions[]?.message' | /usr/bin/grep -E -m1 "$CRD_REFUSAL_RE" || true
}

# strip <file>: an object without the fields every reconcile rewrites.
strip() {
  "$YQ" 'del(.status, .metadata.managedFields, .metadata.resourceVersion, .metadata.generation)' "$1"
}

# The GitOps objects (the byte-identity baseline / check).
dump_gitops() { # <suffix>
  kubectl -n "$GITOPS_NS" get helmrelease sre-agent -o yaml >| "$EVIDENCE/gitops-hr-sre-agent.$1.yaml"
  kubectl -n "$GITOPS_NS" get ocirepository agent -o yaml >| "$EVIDENCE/gitops-oci-agent.$1.yaml"
}

# ------------------------------------------------------------------ steps ---

step_preflight() {
  step preflight
  local v
  v=$("$AGENTLAB" --version | awk '{print $3}')
  [ "$v" = "$WANT_AGENTLAB" ] || die "agentlab $v, want $WANT_AGENTLAB"
  YQ=$(pick_yq) || die "no mikefarah yq v4 found (set YQ)"
  local t
  for t in kubectl helm jq git; do command -v "$t" >/dev/null || die "missing tool: $t"; done
  for f in "$OVERLAY" "$GITOPS_MANIFEST"; do [ -f "$f" ] || die "missing $f"; done
  [[ "$TARGET" =~ ^4\.[0-9]+\.[0-9]+$ ]] || die "target $TARGET is not a 4.x version"
  cd "$LAB_DIR"
  [ -f agentlab.yaml ] || die "$LAB_DIR has no agentlab.yaml"
  export KUBECONFIG="$LAB_DIR/state/kubeconfig"
  /usr/bin/mkdir -p "$EVIDENCE" "$SCRATCH"
  CLUSTER=$("$YQ" '.clusterName' agentlab.yaml)
  BEFORE_VERSION=$(helm -n agent-platform history agent-platform -o json | jq -r '.[-1].chart' | sed 's/^agent-platform-//')
  lock_require
  export_anthropic_key 41
  log "agentlab $v, cluster $CLUSTER, on $BEFORE_VERSION → $TARGET, yq=$YQ, key ${#ANTHROPIC_API_KEY} bytes"
}

step_before() {
  step before
  if [ ! -f "$EVIDENCE/before-agents.yaml" ]; then
    kubectl -n kagent get agents.kagent.dev -o yaml >| "$EVIDENCE/before-agents.yaml"
    kubectl -n kagent get agents.kagent.dev -o wide >| "$EVIDENCE/before-agents.txt"
    kubectl get helmrelease,ocirepository -n kagent -o yaml >| "$EVIDENCE/before-hr-oci-kagent.yaml"
    kubectl get helmrelease,ocirepository -n "$GITOPS_NS" -o yaml >| "$EVIDENCE/before-hr-oci-$GITOPS_NS.yaml"
    dump_gitops before
    helm -n agent-platform history agent-platform >| "$EVIDENCE/before-helm-history.txt"
    kubectl get crd -o custom-columns='N:.metadata.name,REL:.metadata.annotations.meta\.helm\.sh/release-name,NS:.metadata.annotations.meta\.helm\.sh/release-namespace,MANAGED:.metadata.labels.app\.kubernetes\.io/managed-by,STORED:.status.storedVersions,VERSIONS:.spec.versions[*].name' \
      | /usr/bin/grep -i 'kagent\|kmcp\|^N ' >| "$EVIDENCE/before-crds.txt"
    hr_table >| "$EVIDENCE/before-helmreleases.txt"
    kubectl -n agent-platform get deploy -o custom-columns='N:.metadata.name,I:.spec.template.spec.containers[*].image' >| "$EVIDENCE/before-deploy-images.txt"
    cp agentlab.yaml "$EVIDENCE/before-agentlab.yaml"
  fi
  lock_note "stage 2a upgrading in place to $TARGET"
  status "STARTED target=$TARGET lab=$BEFORE_VERSION seeded ($(kubectl -n kagent get agents.kagent.dev --no-headers | wc -l) Agents; worktree $(git -C "$HERE" rev-parse --short HEAD))"
}

step_config() {
  step config
  local t0; t0=$(now)
  "$YQ" -I4 -i ".platform.chartVersion = \"$TARGET\" | .platform.valuesFiles = [\"$OVERLAY\"]" agentlab.yaml
  "$AGENTLAB" render >&2
  cp state/agent-platform-values.yaml "$EVIDENCE/rendered-values-$TARGET.yaml"
  if [ ! -d "$SCRATCH/chart/agent-platform" ] || [ "$("$YQ" '.version' "$SCRATCH/chart/agent-platform/Chart.yaml")" != "$TARGET" ]; then
    rm -rf "$SCRATCH/chart"; /usr/bin/mkdir -p "$SCRATCH/chart"
    timeout 300 helm pull "oci://gsoci.azurecr.io/charts/giantswarm/agent-platform" \
      --version "$TARGET" --untar --untardir "$SCRATCH/chart" >&2
  fi
  timeout 300 helm template agent-platform "$SCRATCH/chart/agent-platform" -n agent-platform \
    -f state/agent-platform-values.yaml -f "$OVERLAY" --kube-version 1.33.0 >| /dev/null
  log "helm template of $TARGET with the rendered values: OK"
  # The rendered values carry the 4.x keys, and the overlay the migrate Job's tenant namespaces.
  local merged k
  merged=$("$YQ" ea 'select(fileIndex == 0) * select(fileIndex == 1)' state/agent-platform-values.yaml "$OVERLAY")
  for k in .substrate .postgres .kagent.harness; do
    [ "$("$YQ" "$k | type" <<<"$merged")" = '!!map' ] || die "rendered values lack $k"
  done
  [ "$("$YQ" -o=json -I0 '.agentManager.migration.gitopsNamespaces' <<<"$merged")" = "[\"$GITOPS_NS\"]" ] \
    || die "merged values: agentManager.migration.gitopsNamespaces != [$GITOPS_NS]"
  [ "$("$YQ" '.agentManager.migration.enabled // "unset"' <<<"$merged")" != false ] || die "agentManager.migration.enabled is false"
  save_t config $(( $(now) - t0 ))
  log "rendered values: substrate, postgres, kagent.harness present; agentManager.migration.gitopsNamespaces=[$GITOPS_NS] ($(load_t config)s)"
}

lab_on_target() {
  helm -n agent-platform history agent-platform -o json 2>/dev/null \
    | jq -e --arg c "agent-platform-$TARGET" '.[-1] | .chart == $c and .status == "deployed"' >/dev/null
}

# Every 20 s for the first five minutes: the HelmRelease table; then every
# 30 s the refusal check only, until the upgrade process ends.
watch_upgrade() { # <pid> <log>
  local pid="$1" out="$2" i=0
  while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 60 ]; do
    if [ "$i" -lt 15 ]; then
      { echo "--- $(date -u +%H:%M:%SZ)"; hr_table; } >>"$out"
    fi
    if [ ! -f "$EVIDENCE/TRAP" ]; then
      local m; m=$(kagent_crds_refusal)
      [ -z "$m" ] || printf '%s\n' "$m" >| "$EVIDENCE/TRAP"
    fi
    i=$((i + 1))
    sleep $(( i <= 15 ? 20 : 30 ))
  done
}

# The FOUND-ISSUE evidence of the predicted kagent-crds refusal.
collect_refusal_evidence() { # <suffix>
  local d="$EVIDENCE/refusal-$1"; /usr/bin/mkdir -p "$d"
  hr_table >| "$d/helmreleases.txt"
  kubectl -n agent-platform get helmrelease kagent-crds -o yaml >| "$d/hr-kagent-crds.yaml"
  kubectl describe helmrelease -n agent-platform kagent-crds >| "$d/describe-hr-kagent-crds.txt" 2>&1
  kubectl get crd -o custom-columns='N:.metadata.name,REL:.metadata.annotations.meta\.helm\.sh/release-name,NS:.metadata.annotations.meta\.helm\.sh/release-namespace,MANAGED:.metadata.labels.app\.kubernetes\.io/managed-by,STORED:.status.storedVersions,VERSIONS:.spec.versions[*].name' \
    | /usr/bin/grep -i 'kagent\|kmcp\|^N ' >| "$d/crds.txt"
  local c
  for c in modelconfigs modelproviderconfigs remotemcpservers agents; do
    kubectl get crd "$c.kagent.dev" -o jsonpath='{.metadata.name}{"\n"}{.metadata.annotations}{"\n"}{.metadata.labels}{"\n\n"}' 2>&1
  done >| "$d/crd-metadata.txt"
  helm -n agent-platform history agent-platform >| "$d/helm-history.txt" 2>&1
  for c in agent-platform-connectivity substrate-crds substrate kagent agent-manager; do
    echo "== $c: Ready=$(hr_ready agent-platform "$c") chart=$(hr_chart_version agent-platform "$c")"; hr_conditions agent-platform "$c"; echo
  done >| "$d/component-states.txt" 2>&1
  kubectl -n agent-platform get ocirepository >| "$d/ocirepositories.txt" 2>&1
}

step_upgrade() {
  step upgrade
  if lab_on_target; then
    log "cluster $CLUSTER already runs meta chart $TARGET deployed — resuming, no upgrade"
    return
  fi
  status "LAB UPGRADING in place $BEFORE_VERSION → $TARGET (agentlab platform, lock held)"
  rm -f "$EVIDENCE/TRAP"
  local t0 pid rc=0 trap_seen='' waited=0
  t0=$(now)
  timeout 1500 "$AGENTLAB" platform --trust=false --open=false >"$EVIDENCE/platform.log" 2>&1 &
  pid=$!
  watch_upgrade "$pid" "$EVIDENCE/platform-watch.txt" &
  local wpid=$!
  while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt 1600 ]; do
    if [ -z "$trap_seen" ] && [ -f "$EVIDENCE/TRAP" ]; then
      trap_seen=1
      log "kagent-crds ownership refusal seen after $(( $(now) - t0 ))s: $(cat "$EVIDENCE/TRAP")"
      collect_refusal_evidence at-detection
      status "FOUND-ISSUE (early, $(( $(now) - t0 ))s in) agent-platform: $BEFORE_VERSION→$TARGET upgrade — the kagent-crds release cannot replace the CRDs the giantswarm/kagent 0.3.x wrapper installed unowned from crds/ ($(cat "$EVIDENCE/TRAP")); letting agentlab platform run out its --wait for a terminal Helm state; evidence $EVIDENCE/refusal-at-detection"
    fi
    sleep 10; waited=$((waited + 10))
  done
  wait "$pid" || rc=$?
  kill "$wpid" 2>/dev/null || true
  wait "$wpid" 2>/dev/null || true
  save_t platform $(( $(now) - t0 ))
  scrub_key "$EVIDENCE/platform.log"
  log "agentlab platform exit $rc after $(load_t platform)s"
  { echo "--- final $(date -u +%H:%M:%SZ) (exit $rc)"; hr_table; } >>"$EVIDENCE/platform-watch.txt"
  helm -n agent-platform history agent-platform >| "$EVIDENCE/after-platform-helm-history.txt" 2>&1

  local msg; msg=$(kagent_crds_refusal)
  if [ -n "$msg" ] || [ -n "$trap_seen" ]; then
    collect_refusal_evidence final
    found_issue "agent-platform: $BEFORE_VERSION→$TARGET upgrade — the kagent-crds release cannot replace the CRDs the giantswarm/kagent 0.3.x wrapper installed unowned from crds/ (${msg:-$(cat "$EVIDENCE/TRAP")}); agentlab platform exit $rc after $(load_t platform)s; evidence $EVIDENCE/refusal-final"
  fi
  if [ "$rc" -ne 0 ]; then
    tail -n 20 "$EVIDENCE/platform.log" >&2
    local failed
    failed=$(kubectl -n agent-platform get helmrelease -o json | jq -r '.items[] | select((.status.conditions[]? | select(.type=="Ready") | .status) != "True") | "\(.metadata.name): \((.status.conditions[]? | select(.type=="Ready") | .message))"')
    printf '%s\n' "$failed" >| "$EVIDENCE/platform-failed-helmreleases.txt"
    if /usr/bin/grep -qiE 'schema|hook|ownership|already exists|invalid|forbidden|failed to' "$EVIDENCE/platform-failed-helmreleases.txt"; then
      found_issue "agent-platform: $BEFORE_VERSION→$TARGET upgrade failed (agentlab platform exit $rc after $(load_t platform)s): $(head -c 600 "$EVIDENCE/platform-failed-helmreleases.txt" | tr '\n' ' '); evidence $EVIDENCE"
    fi
    log "first revision did not converge in time (not-Ready: $(echo "$failed" | cut -d: -f1 | tr '\n' ' ')) — re-running agentlab platform once"
    t0=$(now)
    if timeout 1500 "$AGENTLAB" platform --trust=false --open=false >"$EVIDENCE/platform-rerun.log" 2>&1; then
      save_t platform-rerun $(( $(now) - t0 ))
    else
      rc=$?; save_t platform-rerun $(( $(now) - t0 ))
      scrub_key "$EVIDENCE/platform-rerun.log"; tail -n 20 "$EVIDENCE/platform-rerun.log" >&2
      hr_table >| "$EVIDENCE/platform-rerun-helmreleases.txt"
      die "agentlab platform re-run failed too (exit $rc after $(load_t platform-rerun)s); see $EVIDENCE/platform-rerun-helmreleases.txt"
    fi
    scrub_key "$EVIDENCE/platform-rerun.log"
    { echo "--- after re-run $(date -u +%H:%M:%SZ)"; hr_table; } >>"$EVIDENCE/platform-watch.txt"
  fi
}

# READY = DESIRED of a WorkerPool, read from its own printer columns.
workerpool_ready() { # <ns> <name>
  local hdr row ri di
  hdr=$(kubectl -n "$1" get workerpool "$2" | head -n1)
  row=$(kubectl -n "$1" get workerpool "$2" --no-headers)
  ri=$(awk '{for(i=1;i<=NF;i++) if($i=="READY") print i}' <<<"$hdr")
  di=$(awk '{for(i=1;i<=NF;i++) if($i=="DESIRED") print i}' <<<"$hdr")
  [ -n "$ri" ] && [ -n "$di" ] || { echo "$hdr"$'\n'"$row"; return 1; }
  local r d
  r=$(awk -v i="$ri" '{print $i}' <<<"$row"); d=$(awk -v i="$di" '{print $i}' <<<"$row")
  echo "READY=$r DESIRED=$d"
  [ "$r" = "$d" ] && [ "$d" != 0 ]
}

step_assert_platform() {
  step assert-platform
  local d="$EVIDENCE/after-platform"; /usr/bin/mkdir -p "$d"
  helm -n agent-platform history agent-platform | tee "$d/helm-history.txt" | tail -n 3 >&2
  lab_on_target || die "helm history: the last revision is not agent-platform-$TARGET deployed"
  hr_table | tee "$d/helmreleases.txt" >&2
  assert_hr_ready agent-platform substrate-crds
  assert_hr_ready agent-platform substrate
  [ "$(kubectl -n agent-platform get helmrelease substrate -o jsonpath='{.spec.targetNamespace}')" = ate-system ] || die "substrate targetNamespace != ate-system"
  assert_hr_ready agent-platform kagent-crds
  assert_hr_ready agent-platform kagent '0\.11\.0-gs\.3.*'
  assert_hr_ready agent-platform agent-platform-connectivity "${TARGET//./\\.}.*"
  assert_hr_ready agent-platform agent-manager '1\.[0-9]+\.[0-9]+.*'
  assert_hr_ready agent-platform backstage '2\.[0-9]+\.[0-9]+.*'
  assert_hr_ready agent-platform cloudnative-pg
  assert_hr_ready agent-platform model-manager
  SUBSTRATE_CHART=$(hr_chart_version agent-platform substrate)
  KAGENT_CHART=$(hr_chart_version agent-platform kagent)

  kubectl -n kagent get harness kagent -o wide | tee "$d/harness.txt" >&2
  local wp; wp=$(workerpool_ready kagent kagent-default) || die "WorkerPool kagent/kagent-default not READY=DESIRED: $wp"
  kubectl -n kagent get workerpool kagent-default -o wide | tee "$d/workerpool.txt" >&2
  log "WorkerPool kagent-default $wp"
  kubectl -n kagent get cluster.postgresql.cnpg.io kagent-pg | tee "$d/cnpg-cluster.txt" >&2
  kubectl -n kagent get database.postgresql.cnpg.io kagent-pg-kagent-v2 | tee "$d/cnpg-database.txt" >&2
  [ "$(kubectl -n kagent get database.postgresql.cnpg.io kagent-pg-kagent-v2 -o jsonpath='{.status.applied}')" = true ] || die "Database kagent-pg-kagent-v2 not Applied"
  kubectl -n kagent get secret kagent-pg-kagent-v2-app -o custom-columns='N:.metadata.name,KEYS:.data' | cut -c1-120 | tee "$d/cnpg-secret.txt" >&2

  local c rel
  kubectl get crd -o custom-columns='N:.metadata.name,REL:.metadata.annotations.meta\.helm\.sh/release-name,NS:.metadata.annotations.meta\.helm\.sh/release-namespace,MANAGED:.metadata.labels.app\.kubernetes\.io/managed-by,STORED:.status.storedVersions,VERSIONS:.spec.versions[*].name' \
    | /usr/bin/grep -i 'kagent\|kmcp\|^N ' | tee "$d/crds.txt" >&2
  for c in agenttemplates harnesses modelconfigs remotemcpservers; do
    rel=$(kubectl get crd "$c.kagent.dev" -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}')
    [ "$rel" = kagent-crds ] || die "CRD $c.kagent.dev meta.helm.sh/release-name='$rel', want kagent-crds"
  done
  kubectl get crd agents.kagent.dev >/dev/null || die "agents.kagent.dev is gone"
  log "CRDs: agenttemplates/harnesses/modelconfigs/remotemcpservers owned by kagent-crds; agents.kagent.dev still present"
  kubectl -n kagent get agents.kagent.dev -o wide 2>&1 | tee "$d/agents.txt" >&2
  LEFTOVER_AGENTS=$(kubectl -n kagent get agents.kagent.dev -o name 2>/dev/null | sed 's|.*/||' | tr '\n' ' ')
  if kubectl -n kagent get agents.kagent.dev k8s-agent >/dev/null 2>&1; then K8S_AGENT_STATE=present; else K8S_AGENT_STATE="gone (with the 0.10 chart's upgrade)"; fi
  log "leftover v1alpha2 Agents: $LEFTOVER_AGENTS; k8s-agent $K8S_AGENT_STATE"
  status "LAB ON $TARGET (platform took $(load_t platform) s$([ "$(load_t platform-rerun)" = - ] || echo ", re-run $(load_t platform-rerun) s"); kagent $KAGENT_CHART, substrate $SUBSTRATE_CHART, Harness kagent, WorkerPool kagent-default $wp, kagent_v2 database Applied; v1alpha3 CRDs owned by kagent-crds; leftover Agents: $LEFTOVER_AGENTS)"
}

# entry <array> <ns/name>: the report entry of an object, by namespace/name
# fields or by any string field spelling ns/name.
entry() {
  jq -c --arg k "$2" ".$1[]? | select((((.namespace // \"\") + \"/\" + (.name // \"\")) == \$k) or ([.. | strings] | index(\$k) != null))" "$REPORT_JSON" | head -n1
}
field() { jq -r "$2 // empty" <<<"$1"; }
assert_eq() { [ "$1" = "$2" ] || die "$3: got '$1', want '$2'"; log "$3 = $1"; }
assert_has() { [[ "$1" == *"$2"* ]] || die "$3 lacks '$2': $1"; log "$3 has $2"; }

step_migrate_run1() {
  step migrate-run1
  local d="$EVIDENCE/run1"; /usr/bin/mkdir -p "$d"
  if ! kubectl -n kagent get job -l "$MIGRATE_SELECTOR" --no-headers 2>/dev/null | /usr/bin/grep -q .; then
    kubectl -n agent-platform get helmrelease agent-platform-connectivity -o yaml | /usr/bin/grep -A3 migration >| "$d/connectivity-migration-values.txt" || true
    found_issue "agent-platform-connectivity $TARGET rendered no migrate Job in kagent (selector $MIGRATE_SELECTOR); connectivity values: $(tr '\n' ' ' <"$d/connectivity-migration-values.txt" | head -c 300); evidence $d"
  fi
  kubectl -n kagent get job -l "$MIGRATE_SELECTOR" \
    -o custom-columns='N:.metadata.name,CREATED:.metadata.creationTimestamp,COMPLETE:.status.conditions[?(@.type=="Complete")].status,START:.status.startTime,END:.status.completionTime,IMAGE:.spec.template.spec.containers[0].image,CHART:.metadata.labels.helm\.sh/chart' \
    | tee "$d/jobs.txt" >&2
  local job; job=$(kubectl -n kagent get job -l "$MIGRATE_SELECTOR" --sort-by=.metadata.creationTimestamp -o name | tail -n1)
  log "newest migrate Job: $job ($(kubectl -n kagent get job -l "$MIGRATE_SELECTOR" --no-headers | wc -l) rendered so far)"
  # shellcheck disable=SC2016  # the body runs in bash -c with its own positionals
  if ! timeout 600 bash -c '
      until [ "$(kubectl -n kagent get "$1" -o jsonpath="{.status.conditions[?(@.type==\"Complete\")].status}" 2>/dev/null)" = True ]; do
        [ "$(kubectl -n kagent get "$1" -o jsonpath="{.status.conditions[?(@.type==\"Failed\")].status}" 2>/dev/null)" = True ] && exit 3
        sleep 5
      done' _ "$job"; then
    kubectl -n kagent get "$job" -o yaml >| "$d/job.yaml"
    kubectl -n kagent logs -l "$MIGRATE_SELECTOR" --tail=-1 --all-containers >| "$d/job.log" 2>&1 || true
    found_issue "migrate Job $job did not Complete (Failed or >600 s): $(kubectl -n kagent get "$job" -o jsonpath='{.status.conditions}' | head -c 400); log tail: $(tail -n 5 "$d/job.log" | tr '\n' ' ' | head -c 600); evidence $d"
  fi
  kubectl -n kagent get "$job" -o yaml >| "$d/job.yaml"
  kubectl -n kagent get "$job" -o wide | tee "$d/job.txt" >&2
  kubectl -n kagent logs -l "$MIGRATE_SELECTOR" --tail=-1 >| "$d/job.log" 2>&1 || true
  local st ct
  st=$(kubectl -n kagent get "$job" -o jsonpath='{.status.startTime}'); ct=$(kubectl -n kagent get "$job" -o jsonpath='{.status.completionTime}')
  save_t job1 "$(( $(date -d "$ct" +%s) - $(date -d "$st" +%s) )) ($st → $ct)"
  kubectl -n kagent get cm agent-manager-migrate-report -o jsonpath='{.data.report\.yaml}' >| "$d/report.yaml"
  kubectl -n kagent get cm agent-manager-migrate-report -o yaml >| "$d/report-cm.yaml"
  REPORT_JSON="$d/report.json"; "$YQ" -o=json '.' "$d/report.yaml" >| "$REPORT_JSON"
  log "report: $(wc -c <"$d/report.yaml") bytes, cm phase=$(kubectl -n kagent get cm agent-manager-migrate-report -o jsonpath='{.data.phase}')"

  local LIVE_REPORT_JSON="$REPORT_JSON"
  if [ -n "$REPORT_RUN1" ]; then
    # The Job re-ran since the first expand run: the live report describes the
    # re-run (nothing left to rewrite), the rewrite is in the saved copy.
    [ -f "$REPORT_RUN1" ] || die "REPORT_RUN1=$REPORT_RUN1 does not exist"
    cp "$REPORT_RUN1" "$d/report-run1.yaml"
    REPORT_JSON="$d/report-run1.json"; "$YQ" -o=json '.' "$d/report-run1.yaml" >| "$REPORT_JSON"
    local r
    for r in sre narrow; do
      assert_eq "$(jq -r --arg n "$r" '.releases[] | select(.namespace=="kagent" and .name==$n) | .action' "$LIVE_REPORT_JSON")" unchanged "live report releases kagent/$r action"
      assert_eq "$(jq -r --arg n "$r" '.releases[] | select(.namespace=="kagent" and .name==$n) | .reason' "$LIVE_REPORT_JSON")" "already on the 1.x values" "live report releases kagent/$r reason"
    done
    log "rewrite asserted from the first run's report (run.at $(jq -r .run.at "$REPORT_JSON"), $REPORT_RUN1); the live report is the re-run's (run.at $(jq -r .run.at "$LIVE_REPORT_JSON"), $job)"
  fi
  assert_eq "$("$YQ" '.phase' "$d/report.yaml")" expand "report.phase"
  local e
  e=$(entry releases kagent/sre); [ -n "$e" ] || die "report: no releases[] entry for kagent/sre"
  assert_eq "$(field "$e" .action)" rewritten "releases kagent/sre action"
  SRE_REMOVED=$(jq -c '.changes.removed // []' <<<"$e"); assert_has "$SRE_REMOVED" agent.runtime "kagent/sre changes.removed"
  SRE_PINNED=$(jq -r '[.changes.skills[]?.pinned // empty] | .[0] // ""' <<<"$e")
  [[ "$SRE_PINNED" =~ ^[0-9a-f]{40}$ ]] || die "kagent/sre changes.skills[].pinned '$SRE_PINNED' is not 40-hex: $(jq -c .changes <<<"$e")"
  log "kagent/sre skill pinned $SRE_PINNED; changes: $(jq -c .changes <<<"$e")"
  e=$(entry releases kagent/narrow); [ -n "$e" ] || die "report: no releases[] entry for kagent/narrow"
  assert_eq "$(field "$e" .action)" rewritten "releases kagent/narrow action"
  NARROW_REMOVED=$(jq -c '.changes.removed // []' <<<"$e")
  assert_has "$NARROW_REMOVED" agent.runtime "kagent/narrow changes.removed"; assert_has "$NARROW_REMOVED" muster.serverRef "kagent/narrow changes.removed"
  NARROW_RENAMED=$(jq -c '.changes.renamed // []' <<<"$e")
  assert_has "$NARROW_RENAMED" muster.toolNames "kagent/narrow changes.renamed"; assert_has "$NARROW_RENAMED" muster.tools "kagent/narrow changes.renamed"
  log "kagent/narrow changes: $(jq -c .changes <<<"$e")"
  e=$(entry releases "$GITOPS_NS/sre-agent"); [ -n "$e" ] || die "report: no releases[] entry for $GITOPS_NS/sre-agent"
  assert_eq "$(field "$e" .ownership)" external "releases $GITOPS_NS/sre-agent ownership"
  assert_eq "$(field "$e" .action)" diff "releases $GITOPS_NS/sre-agent action"
  field "$e" .diff >| "$d/sre-agent.diff"
  local want
  for want in '^-[[:space:]]+runtime: python' '^-[[:space:]]+toolNames:' '^\+[[:space:]]+tools:' '^-[[:space:]]+- /spec/declarative/deployment/podSecurityContext'; do
    /usr/bin/grep -Eq "$want" "$d/sre-agent.diff" || die "sre-agent diff lacks /$want/:"$'\n'"$(cat "$d/sre-agent.diff")"
  done
  log "sre-agent diff ($(wc -l <"$d/sre-agent.diff") lines) carries runtime/toolNames→tools/driftDetection.ignore"
  e=$(entry sources kagent/agent); [ -n "$e" ] || die "report: no sources[] entry for kagent/agent"
  assert_eq "$(field "$e" .action)" moved "sources kagent/agent action"
  assert_eq "$(field "$e" .from)" '>=0.2.1 <1.0.0' "sources kagent/agent from"; assert_eq "$(field "$e" .to)" 1.x "sources kagent/agent to"
  e=$(entry sources "$GITOPS_NS/agent"); [ -n "$e" ] || die "report: no sources[] entry for $GITOPS_NS/agent"
  assert_eq "$(field "$e" .action)" diff "sources $GITOPS_NS/agent action"
  local a
  for a in sre narrow sre-agent; do
    e=$(entry agents "kagent/$a"); [ -n "$e" ] || die "report: no agents[] entry for kagent/$a"
    assert_has "$e" awaiting-upgrade "agents kagent/$a"
  done
  e=$(entry agents kagent/k8s-agent)
  if [ "$K8S_AGENT_STATE" = present ]; then
    [ -n "$e" ] || die "report: k8s-agent exists but has no agents[] entry"
    assert_has "$e" not-migratable "agents kagent/k8s-agent"
    K8S_AGENT_REPORT=not-migratable
  else
    if [ -n "$e" ]; then assert_has "$e" not-migratable "agents kagent/k8s-agent"; K8S_AGENT_REPORT="not-migratable at run time, the object gone since"; else K8S_AGENT_REPORT="no report entry"; fi
    log "k8s-agent $K8S_AGENT_STATE; report: $K8S_AGENT_REPORT"
  fi
  PENDING=$(jq -c '.pending // []' "$REPORT_JSON"); assert_has "$PENDING" "$GITOPS_NS/sre-agent" "pending[]"

  # Live: the rewritten releases, the moved source, the untouched GitOps objects.
  local v
  v=$(kubectl -n kagent get helmrelease sre -o jsonpath='{.spec.values}'); printf '%s\n' "$v" | "$YQ" -P '.' >| "$d/live-values-sre.yaml"
  [ -z "$(jq -r '.agent.runtime // empty' <<<"$v")" ] || die "live kagent/sre still has agent.runtime"
  [ -n "$(jq -r '.agent.iconUrl // empty' <<<"$v")" ] || die "live kagent/sre lost agent.iconUrl"
  [[ "$(jq -r '.skills[0].git.commit // empty' <<<"$v")" =~ ^[0-9a-f]{40}$ ]] || die "live kagent/sre skills[0].git.commit not 40-hex: $(jq -c .skills <<<"$v")"
  v=$(kubectl -n kagent get helmrelease narrow -o jsonpath='{.spec.values}'); printf '%s\n' "$v" | "$YQ" -P '.' >| "$d/live-values-narrow.yaml"
  [ -n "$(jq -c '.muster.tools // empty' <<<"$v")" ] || die "live kagent/narrow has no muster.tools"
  [ -z "$(jq -c '.muster.toolNames // empty' <<<"$v")" ] || die "live kagent/narrow still has muster.toolNames"
  [ -z "$(jq -c '.muster.serverRef // empty' <<<"$v")" ] || die "live kagent/narrow still has muster.serverRef"
  assert_eq "$(kubectl -n kagent get ocirepository agent -o jsonpath='{.spec.ref.semver}')" 1.x "live OCIRepository kagent/agent spec.ref.semver"
  dump_gitops after-run1
  local f
  for f in gitops-hr-sre-agent gitops-oci-agent; do
    if ! diff <(strip "$EVIDENCE/$f.before.yaml") <(strip "$EVIDENCE/$f.after-run1.yaml") >| "$d/$f.identity.diff"; then
      die "$f changed across the upgrade + run 1 (must be byte-identical): $(head -c 800 "$d/$f.identity.diff")"
    fi
  done
  GITOPS_IDENTITY="byte-identical (HelmRelease $GITOPS_NS/sre-agent, OCIRepository $GITOPS_NS/agent; status/managedFields/resourceVersion/generation stripped)"
  log "GitOps objects $GITOPS_IDENTITY"
  status "EXPANDED run1: phase=expand, sre+narrow rewritten (removed sre=$SRE_REMOVED narrow=$NARROW_REMOVED, renamed narrow=$NARROW_RENAMED, skill pinned ${SRE_PINNED:0:7}), sre-agent external diff, kagent/agent moved →1.x, k8s-agent $K8S_AGENT_STATE ($K8S_AGENT_REPORT); Job $(load_t job1 | cut -d' ' -f1) s; GitOps objects $GITOPS_IDENTITY"
}

# norm <diff>: the changed lines as sign + trimmed, unquoted content.
norm_diff() { /usr/bin/grep -E '^[-+][^-+]' "$1" | sed -E 's/^([-+])[[:space:]]*/\1 /; s/[[:space:]]+$//' | tr -d "'\"" | sort -u; }

step_gitops_pr() {
  step gitops-pr
  local d="$EVIDENCE/run1"
  # The tenant's pull request: the seed manifest with the report's diff applied.
  "$YQ" '
    (select(.kind == "OCIRepository") | .spec.ref.semver) = "1.x" |
    (select(.kind == "HelmRelease") | .spec.driftDetection) |= del(.ignore) |
    (select(.kind == "HelmRelease") | .spec.values.agent) |= del(.runtime) |
    (select(.kind == "HelmRelease") | .spec.values.muster) |= (del(.serverRef) | .tools = .toolNames | del(.toolNames))
  ' "$GITOPS_MANIFEST" >| "$GITOPS_1X_MANIFEST"
  sed -i '1,6{s|^# Fleet shape 3: a GitOps-owned agent\.|# Fleet shape 3 after the migrate Job'"'"'s expand run: the tenant'"'"'s pull request\n# applies the report'"'"'s diff to the GitOps-owned agent — agent chart 1.x, no\n# agent.runtime, muster.tools instead of toolNames/serverRef, no drift ignores.|}' "$GITOPS_1X_MANIFEST"
  diff "$GITOPS_MANIFEST" "$GITOPS_1X_MANIFEST" >| "$d/seed-gitops-1x.vs-seed.diff" || true
  # Line by line against the report's diff, both ways.
  local mine theirs missing extra
  mine=$(norm_diff "$d/seed-gitops-1x.vs-seed.diff"); theirs=$(norm_diff "$d/sre-agent.diff")
  missing=$(comm -13 <(echo "$mine") <(echo "$theirs")); extra=$(comm -23 <(echo "$mine") <(echo "$theirs"))
  { echo "# report diff lines my file does not change:"; echo "$missing"; echo; echo "# lines my file changes the report does not:"; echo "$extra"; } >| "$d/seed-gitops-1x.compare.txt"
  # The OCIRepository is not part of the release diff; its move is the sources[] entry.
  extra=$(/usr/bin/grep -v 'semver' <<<"$extra" || true)
  [ -z "$missing" ] && [ -z "$extra" ] || die "seed-gitops-1x.yaml does not match the report's diff line by line:"$'\n'"$(cat "$d/seed-gitops-1x.compare.txt")"
  log "seed-gitops-1x.yaml matches the report's diff line by line (plus the OCIRepository move)"
  if ! git -C "$HERE" diff --quiet -- "$GITOPS_1X_MANIFEST" || [ -n "$(git -C "$HERE" ls-files --others --exclude-standard -- "$GITOPS_1X_MANIFEST")" ]; then
    git -C "$HERE" add "$GITOPS_1X_MANIFEST"
    git -C "$HERE" commit -q -m "feat(migration-rehearsal): the GitOps agent after the migrate Job's diff" -- "$GITOPS_1X_MANIFEST"
    timeout 120 git -C "$HERE" push -q
    log "committed and pushed $(git -C "$HERE" rev-parse --short HEAD)"
  fi
  local t0; t0=$(now)
  kubectl apply -f "$GITOPS_1X_MANIFEST" 2>&1 | tee "$d/apply-seed-gitops-1x.txt" >&2
  save_t apply "$(( $(now) - t0 )) at $(utc)"
  dump_gitops after-apply
  log "GitOps diff applied: $(load_t apply)"
}

step_record() {
  step record
  {
    echo "# Stage 2a — $BEFORE_VERSION → $TARGET in place on $CLUSTER ($(utc))"
    echo
    echo "| step | seconds |"
    echo "|---|---|"
    echo "| render + helm template pre-flight | $(load_t config) |"
    echo "| agentlab platform (in place) | $(load_t platform) |"
    echo "| agentlab platform (re-run) | $(load_t platform-rerun) |"
    echo "| migrate Job run 1 (start → completion) | $(load_t job1) |"
    echo "| GitOps diff apply (flux-giantswarm/sre-agent) | $(load_t apply) |"
    echo
    echo "kagent chart: $KAGENT_CHART; substrate: $SUBSTRATE_CHART; agent-manager: $(hr_chart_version agent-platform agent-manager); backstage: $(hr_chart_version agent-platform backstage)"
    echo "leftover agents.kagent.dev: $LEFTOVER_AGENTS; k8s-agent: $K8S_AGENT_STATE ($K8S_AGENT_REPORT)"
    echo "sre removed: $SRE_REMOVED; narrow removed: $NARROW_REMOVED; narrow renamed: $NARROW_RENAMED; sre skill pinned: $SRE_PINNED"
    echo "pending: $PENDING"
    echo "GitOps objects: $GITOPS_IDENTITY"
    echo "worktree head: $(git -C "$HERE" rev-parse --short HEAD)"
  } >| "$EVIDENCE/timings.md"
  status "LAB HELD ($TARGET expanded; stage 2b runs the contract; lock stays until the rehearsal ends)"
  cat "$EVIDENCE/timings.md"
}

main() {
  step_preflight
  if [ "$START_STEP" = assert ]; then START_STEP=assert-platform; fi
  [[ " ${STEPS[*]} " == *" $START_STEP "* ]] || die "START_STEP=$START_STEP is none of: ${STEPS[*]}"
  local s run=''
  for s in "${STEPS[@]}"; do
    [ "$s" != "$START_STEP" ] || run=1
    if [ -n "$run" ]; then "step_${s//-/_}"; else log "skipping $s (START_STEP=$START_STEP)"; fi
  done
}

# Sourceable: the helpers and evidence collectors run standalone.
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
