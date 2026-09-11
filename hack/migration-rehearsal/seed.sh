#!/usr/bin/env bash
# Seed the agentlab with the 3.x meta chart and the four fleet shapes of the
# migration rehearsal (agentlab#143), stage 1:
#
#   shape 4  k8s-agent   bundled example, owned by the meta chart's kagent HelmRelease
#   shape 1  sre         portal-shaped, created by agent-manager through muster
#   shape 2  narrow      hand-applied agent-chart HelmRelease with muster.toolNames
#   shape 3  sre-agent   GitOps-owned: OCIRepository + HelmRelease in flux-giantswarm
#
# Usage:  cd <lab dir> && seed.sh <evidence-dir>
#
# Environment (all optional):
#   LAB_DIR         the lab directory (default: $PWD; needs agentlab.yaml, state/, certs/)
#   CHART_VERSION   the 3.x meta chart to seed (default: 3.23.1)
#   WANT_AGENTLAB   the agentlab release the run must use (default: v0.39.1)
#   SCRATCH         where the meta chart is pulled for the pre-flight helm template
#   STATUS_FILE     append-only status log; no status lines when unset
#   STATUS_PREFIX   prefix of every status line (default: agentlab#143-stage1)
#   LOCK_OWNER      text of state/lab-lock/owner
#   AGENTLAB, YQ    binaries (default: agentlab on PATH; a mikefarah yq v4)
#
# The Anthropic key is read from the Secret kagent/kagent-anthropic into the
# environment (agentlab up consumes $ANTHROPIC_API_KEY) and is never printed;
# only its length is logged. Steps are idempotent where that is cheap, so a
# failed run can be resumed by re-running with the same arguments.
set -Eeuo pipefail

EVIDENCE="${1:?usage: seed.sh <evidence-dir>}"
LAB_DIR="${LAB_DIR:-$PWD}"
CHART_VERSION="${CHART_VERSION:-3.23.1}"
WANT_AGENTLAB="${WANT_AGENTLAB:-v0.39.1}"
SCRATCH="${SCRATCH:-${TMPDIR:-/tmp}/agentlab-migration-rehearsal}"
STATUS_FILE="${STATUS_FILE:-}"
STATUS_PREFIX="${STATUS_PREFIX:-agentlab#143-stage1}"
AGENTLAB="${AGENTLAB:-agentlab}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OVERLAY="$HERE/values-3x-example-agent.yaml"
NARROW_MANIFEST="$HERE/seed-narrow.yaml"
GITOPS_MANIFEST="$HERE/seed-gitops.yaml"
AGENT_SEMVER='>=0.2.1 <1.0.0'
MCP_URL="${MCP_URL:-https://muster.127.0.0.1.nip.io:8445/mcp}"
DEX_TOKEN_URL="${DEX_TOKEN_URL:-https://localhost:32003/dex/token}"

export AGENTLAB_TELEMETRY_TESTMODE=1 AGENTLAB_NO_UPDATE_CHECK=1

# ---------------------------------------------------------------- helpers ---

log() { printf '%s seed: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
die() {
  log "FATAL: $*"
  status "BLOCKED step=$CURRENT_STEP — ${1%%$'\n'*} — lab left as is (lock HELD if taken); see $EVIDENCE"
  trap - ERR
  exit 1
}
now() { date +%s; }

status() {
  [ -n "$STATUS_FILE" ] || return 0
  printf '%s %s %s\n' "$(date -u +%Y-%m-%dT%H:%MZ)" "$STATUS_PREFIX" "$*" >>"$STATUS_FILE"
}

CURRENT_STEP=preflight
on_error() {
  local rc=$?
  log "step '$CURRENT_STEP' failed (exit $rc)"
  status "BLOCKED step=$CURRENT_STEP exit=$rc — lab left as is (lock HELD if taken); see $EVIDENCE"
  exit "$rc"
}
trap on_error ERR

step() { CURRENT_STEP="$1"; log "== $1"; }

# Never let the key reach an evidence file: redact it if a tool echoed it.
scrub_key() {
  local f="$1"
  [ -f "$f" ] || return 0
  if /usr/bin/grep -qF -- "$ANTHROPIC_API_KEY" "$f"; then
    log "WARNING: the Anthropic key appeared in $f — redacted"
    sed -i "s|$ANTHROPIC_API_KEY|<redacted>|g" "$f"
  fi
}

pick_yq() {
  local c
  for c in "${YQ:-}" yq "$HOME/.go/bin/yq" go-yq; do
    [ -n "$c" ] || continue
    if command -v "$c" >/dev/null 2>&1 && "$c" --version 2>&1 | /usr/bin/grep -q mikefarah; then
      echo "$c"; return 0
    fi
  done
  return 1
}

# wait_agent <ns> <name> <timeout-s>: until the Agent's Accepted condition is True.
wait_agent() {
  local ns="$1" name="$2" to="$3"
  timeout "$to" bash -c '
    until [ "$(kubectl -n "$1" get agent "$2" -o jsonpath="{.status.conditions[?(@.type==\"Accepted\")].status}" 2>/dev/null)" = True ]; do
      sleep 5
    done' _ "$ns" "$name" \
    || die "Agent $ns/$name not Accepted within ${to}s: $(kubectl -n "$ns" get agent "$name" -o jsonpath='{.status.conditions}' 2>&1)"
}

# wait_agent_ready <ns> <name> <timeout-s>: Accepted + its Deployment rolled out.
wait_agent_ready() {
  local ns="$1" name="$2" to="$3"
  wait_agent "$ns" "$name" "$to"
  timeout "$to" bash -c 'until kubectl -n "$1" get deploy "$2" >/dev/null 2>&1; do sleep 5; done' _ "$ns" "$name" \
    || die "deploy/$name never appeared in $ns"
  kubectl -n "$ns" rollout status "deploy/$name" --timeout=300s >&2
}

# assert_label <kind> <ns> <name> <label> <want>
assert_label() {
  local got
  got=$(kubectl -n "$2" get "$1" "$3" -o jsonpath="{.metadata.labels['$4']}")
  [ "$got" = "$5" ] || die "$1 $2/$3 label $4 = '$got', want '$5'"
  log "$1 $2/$3 label $4=$got"
}

hr_chart_version() { # <ns> <name>
  kubectl -n "$1" get helmrelease "$2" -o jsonpath='{.status.history[0].chartVersion}'
}

assert_hr_version() { # <ns> <name> <regex>
  local got
  got=$(hr_chart_version "$1" "$2")
  [[ "$got" =~ ^$3$ ]] || die "HelmRelease $1/$2 chart $got, want $3"
  log "HelmRelease $1/$2 chart $got"
}

# mcp_message <file>: the JSON-RPC message of a Streamable-HTTP body (JSON or SSE).
mcp_message() {
  if /usr/bin/grep -q '^data:' "$1"; then
    sed -n 's/^data: //p' "$1" | jq -c 'select(.id != null)' | tail -n1
  else
    jq -c . "$1"
  fi
}

# ------------------------------------------------------------------ steps ---

step_preflight() {
  step preflight
  local v
  v=$("$AGENTLAB" --version | awk '{print $3}')
  [ "$v" = "$WANT_AGENTLAB" ] || die "agentlab $v, want $WANT_AGENTLAB"
  YQ=$(pick_yq) || die "no mikefarah yq v4 found (set YQ)"
  local t
  for t in kubectl helm jq curl kind; do command -v "$t" >/dev/null || die "missing tool: $t"; done
  for f in "$OVERLAY" "$NARROW_MANIFEST" "$GITOPS_MANIFEST"; do [ -f "$f" ] || die "missing $f"; done
  cd "$LAB_DIR"
  [ -f agentlab.yaml ] || die "$LAB_DIR has no agentlab.yaml"
  [ -f certs/ca.crt ] || die "$LAB_DIR has no certs/ca.crt"
  export KUBECONFIG="$LAB_DIR/state/kubeconfig"
  /usr/bin/mkdir -p "$EVIDENCE" "$SCRATCH"
  CLUSTER=$("$YQ" '.clusterName' agentlab.yaml)
  BEFORE_VERSION=$("$YQ" '.platform.chartVersion' agentlab.yaml)
  local wt branch
  wt=$(git -C "$HERE" rev-parse --show-toplevel 2>/dev/null || echo "$HERE")
  branch=$(git -C "$HERE" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')
  status "STARTED worktree=$wt branch=$branch agentlab=$v"
  log "agentlab $v, cluster $CLUSTER, before=$BEFORE_VERSION, yq=$YQ"
}

step_lock_and_before() {
  step lock-and-before
  local owner="${LOCK_OWNER:-agentlab#143 migration rehearsal (stage 1 seed: down && up on $CHART_VERSION, then stages 2a/2b/3) since $(date -Is) — HELD until the rehearsal ends; do not touch}"
  if /usr/bin/mkdir state/lab-lock 2>/dev/null; then
    echo "$owner" >| state/lab-lock/owner
    log "lab lock taken"
  elif [ -f state/lab-lock/owner ] && /usr/bin/grep -q 'agentlab#143' state/lab-lock/owner; then
    log "lab lock already held by this rehearsal: $(cat state/lab-lock/owner)"
  else
    cat state/lab-lock/owner 2>/dev/null || true
    die "lab lock held by someone else"
  fi
  [ -f agentlab.yaml.before-143 ] || cp agentlab.yaml agentlab.yaml.before-143

  if [ ! -f "$EVIDENCE/before-helmreleases.txt" ]; then
    kubectl get helmrelease -A >| "$EVIDENCE/before-helmreleases.txt"
    kubectl -n agent-platform get deploy \
      -o custom-columns='N:.metadata.name,I:.spec.template.spec.containers[*].image' \
      >| "$EVIDENCE/before-deploy-images.txt"
    helm -n agent-platform history agent-platform >| "$EVIDENCE/before-helm-history.txt"
  fi
  BEFORE_READY=$(awk 'NR>1 && $4=="True"' "$EVIDENCE/before-helmreleases.txt" | wc -l)
  BEFORE_TOTAL=$(awk 'NR>1' "$EVIDENCE/before-helmreleases.txt" | wc -l)

  if [ -z "${ANTHROPIC_API_KEY:-}" ]; then
    local key
    key=$(kubectl -n kagent get secret kagent-anthropic -o jsonpath='{.data.ANTHROPIC_API_KEY}' | base64 -d)
    export ANTHROPIC_API_KEY="$key"
  fi
  [ "${#ANTHROPIC_API_KEY}" -eq 108 ] || die "ANTHROPIC_API_KEY has ${#ANTHROPIC_API_KEY} bytes, want 108"
  log "ANTHROPIC_API_KEY exported (${#ANTHROPIC_API_KEY} bytes)"
}

step_config() {
  step config
  "$YQ" -I4 -i ".platform.chartVersion = \"$CHART_VERSION\" | .platform.valuesFiles = [\"$OVERLAY\"]" agentlab.yaml
  "$AGENTLAB" render >&2
  if [ ! -d "$SCRATCH/chart/agent-platform" ]; then
    /usr/bin/mkdir -p "$SCRATCH/chart"
    timeout 300 helm pull "oci://gsoci.azurecr.io/charts/giantswarm/agent-platform" \
      --version "$CHART_VERSION" --untar --untardir "$SCRATCH/chart" >&2
  fi
  timeout 300 helm template agent-platform "$SCRATCH/chart/agent-platform" -n agent-platform \
    -f state/agent-platform-values.yaml -f "$OVERLAY" --kube-version 1.33.0 >| /dev/null
  log "helm template of $CHART_VERSION with the rendered values: OK"
  # The 3.x lab shape carries none of the 4.x keys (comments do not count).
  local hits
  hits=$("$YQ" -o=json '.' state/agent-platform-values.yaml \
    | /usr/bin/grep -nE '"(substrate|postgres|harness|trusted-proxy)"|trusted-proxy' || true)
  [ -z "$hits" ] || die "rendered values carry 4.x keys:"$'\n'"$hits"
  log "rendered values: no substrate/postgres/harness/trusted-proxy"
}

step_down() {
  step down
  status "LAB lock taken — ANNOUNCED: agentlab down && up on $CLUSTER (same name) to seed meta chart $CHART_VERSION; BEFORE=$BEFORE_VERSION $BEFORE_READY/$BEFORE_TOTAL Ready, saved to $EVIDENCE"
  if kind get clusters 2>/dev/null | /usr/bin/grep -qx "$CLUSTER"; then
    local t0; t0=$(now)
    "$AGENTLAB" down >&2
    T_DOWN=$(( $(now) - t0 ))
  else
    log "cluster $CLUSTER already gone"
    T_DOWN=0
  fi
  log "down: ${T_DOWN}s"
}

step_up() {
  step up
  local t0; t0=$(now)
  T_PLATFORM=-
  if timeout 1800 "$AGENTLAB" up --trust=false --open=false 2>&1 | tee "$EVIDENCE/up.log" >&2; then
    T_UP=$(( $(now) - t0 ))
  else
    T_UP=$(( $(now) - t0 ))
    scrub_key "$EVIDENCE/up.log"
    log "agentlab up failed after ${T_UP}s (tail below); re-running agentlab platform once"
    tail -n 15 "$EVIDENCE/up.log" >&2
    t0=$(now)
    if timeout 1500 "$AGENTLAB" platform --trust=false --open=false 2>&1 | tee "$EVIDENCE/platform-rerun.log" >&2; then
      T_PLATFORM=$(( $(now) - t0 ))
    else
      T_PLATFORM=$(( $(now) - t0 ))
      scrub_key "$EVIDENCE/platform-rerun.log"
      die "agentlab platform re-run failed after ${T_PLATFORM}s (up had failed after ${T_UP}s)"
    fi
    scrub_key "$EVIDENCE/platform-rerun.log"
  fi
  scrub_key "$EVIDENCE/up.log"
  T_UP_END=$(now)
  log "up: ${T_UP}s, platform re-run: ${T_PLATFORM}"
}

step_assert_platform() {
  step assert-platform
  kubectl get crd agents.kagent.dev >/dev/null
  if kubectl get crd agenttemplates.kagent.dev >/dev/null 2>&1; then
    die "agenttemplates.kagent.dev exists — this is not the 3.x kagent"
  fi
  log "CRDs: agents.kagent.dev present, agenttemplates.kagent.dev absent"
  helm -n agent-platform history agent-platform >| "$EVIDENCE/after-up-helm-history.txt"
  /usr/bin/grep -q "agent-platform-$CHART_VERSION" "$EVIDENCE/after-up-helm-history.txt" \
    || die "helm history lacks agent-platform-$CHART_VERSION"
  kubectl get helmrelease -A \
    -o custom-columns='NS:.metadata.namespace,N:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,CHART:.status.history[0].chartVersion' \
    | tee "$EVIDENCE/after-up-helmreleases.txt" >&2
  assert_hr_version agent-platform kagent '0\.3\.[0-9]+.*'
  assert_hr_version agent-platform agent-manager '0\.4\.5.*'
  assert_hr_version agent-platform backstage '0\.244\.[0-9]+.*'
  assert_hr_version agent-platform agent-platform-connectivity "${CHART_VERSION//./\\.}.*"
  KAGENT_CHART=$(hr_chart_version agent-platform kagent)
  local n
  for n in substrate substrate-crds kagent-crds cloudnative-pg; do
    if kubectl -n agent-platform get helmrelease "$n" >/dev/null 2>&1; then die "HelmRelease $n exists on the 3.x line"; fi
  done
  kubectl -n kagent get modelconfig default-model-config >&2
  kubectl -n agent-platform get remotemcpserver muster >&2
  status "LAB ON $CHART_VERSION (kagent $KAGENT_CHART/0.10, agent-manager $(hr_chart_version agent-platform agent-manager), backstage $(hr_chart_version agent-platform backstage); up took $T_UP s${T_PLATFORM:+, platform re-run $T_PLATFORM s})"
}

step_platform_test() {
  step platform-test
  if timeout 300 "$AGENTLAB" platform-test 2>&1 | tee "$EVIDENCE/platform-test.txt" >&2; then
    PLATFORM_TEST=PASS
  else
    PLATFORM_TEST=FAIL
  fi
  scrub_key "$EVIDENCE/platform-test.txt"
  log "platform-test: $PLATFORM_TEST"
  [ "$PLATFORM_TEST" = PASS ] || die "agentlab platform-test failed"
}

step_shape4_bundled() {
  step shape4-bundled-k8s-agent
  wait_agent kagent k8s-agent 600
  kubectl -n kagent rollout status deploy/k8s-agent --timeout=300s >&2
  T_SHAPE4=$(( $(now) - T_UP_END ))
  assert_label agent kagent k8s-agent helm.toolkit.fluxcd.io/name kagent
  assert_label agent kagent k8s-agent helm.toolkit.fluxcd.io/namespace agent-platform
  log "shape 4 (k8s-agent) Ready ${T_SHAPE4}s after up"
}

mcp_call() { # <session-id or ""> <json body> <out-file> [<-D headers-file>]
  local sid="$1" body="$2" out="$3" hdrs="${4:-/dev/null}"
  local -a session=()
  [ -z "$sid" ] || session=(-H "Mcp-Session-Id: $sid")
  curl -sS --cacert certs/ca.crt -D "$hdrs" -o "$out" \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    "${session[@]}" \
    -X POST "$MCP_URL" -d "$body"
}

step_shape1_portal() {
  step shape1-portal-sre
  local t0; t0=$(now)
  if kubectl -n kagent get helmrelease sre >/dev/null 2>&1; then
    log "HelmRelease kagent/sre exists — skipping create_agent"
  else
    TOKEN=$(curl -sS --cacert certs/ca.crt -u agent-platform:agent-platform-lab-secret -X POST "$DEX_TOKEN_URL" \
      -d grant_type=password -d username=admin@lab.local -d password=password \
      -d 'scope=openid email groups profile audience:server:client_id:kubernetes' | jq -r .id_token)
    [ -n "$TOKEN" ] && [ "$TOKEN" != null ] || die "no Dex id_token"
    local hdrs="$SCRATCH/mcp-init.headers" sid
    mcp_call "" '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"migration-rehearsal-seed","version":"0.1"}}}' \
      "$SCRATCH/mcp-init.out" "$hdrs"
    sid=$(/usr/bin/grep -i '^mcp-session-id:' "$hdrs" | awk '{print $2}' | tr -d '\r')
    [ -n "$sid" ] || die "initialize returned no Mcp-Session-Id: $(head -c 400 "$SCRATCH/mcp-init.out")"
    mcp_call "$sid" '{"jsonrpc":"2.0","method":"notifications/initialized"}' "$SCRATCH/mcp-initialized.out"
    local args
    args=$(cat <<'JSON'
{"name":"x_agent-manager_create_agent","arguments":{"name":"sre","namespace":"kagent","displayName":"SRE Assistant","description":"Rehearsal agent, portal-shaped","systemMessage":"Be brief.","iconUrl":"https://avatars.example.com/sre.png","runtime":"go","modelConfig":"default-model-config","skills":{"gitRefs":[{"url":"https://github.com/giantswarm/agent-skills","ref":"main","path":"agent-self-awareness","name":"agent-self-awareness"}]},"toolset":["preset:read-only"]}}
JSON
)
    mcp_call "$sid" "$(jq -cn --argjson a "$args" '{jsonrpc:"2.0",id:2,method:"tools/call",params:{name:"call_tool",arguments:$a}}')" \
      "$SCRATCH/mcp-create.out"
    local msg envelope payload
    msg=$(mcp_message "$SCRATCH/mcp-create.out")
    envelope=$(jq -r '.result.content[0].text // empty' <<<"$msg")
    [ -n "$envelope" ] || die "create_agent: no result.content: $msg"
    payload=$(jq -r '.content[0].text // empty' <<<"$envelope")
    printf '%s\n' "$payload" >| "$EVIDENCE/create-agent-sre.json"
    [ "$(jq -r '.isError // false' <<<"$envelope")" = false ] || die "create_agent isError: $payload"
    jq -e '.created.helmRelease == true' <<<"$payload" >/dev/null || die "create_agent: created.helmRelease != true: $payload"
    jq -e '.requestedBy == "admin@lab.local"' <<<"$payload" >/dev/null || die "create_agent: requestedBy: $payload"
    log "create_agent sre: created.helmRelease=true requestedBy=admin@lab.local"
  fi
  local got
  got=$(kubectl -n kagent get ocirepository agent -o jsonpath='{.spec.ref.semver}')
  [ "$got" = "$AGENT_SEMVER" ] || die "kagent/agent OCIRepository semver '$got', want '$AGENT_SEMVER'"
  [ "$(kubectl -n kagent get helmrelease sre -o jsonpath='{.spec.values.skills.gitRefs[0].ref}')" = main ] || die "sre: skills.gitRefs[0].ref != main"
  [ -n "$(kubectl -n kagent get helmrelease sre -o jsonpath='{.spec.values.agent.iconUrl}')" ] || die "sre: no agent.iconUrl"
  [ "$(kubectl -n kagent get helmrelease sre -o jsonpath='{.spec.values.agent.runtime}')" = go ] || die "sre: agent.runtime != go"
  [ -n "$(kubectl -n kagent get helmrelease sre -o jsonpath='{.spec.values.toolset}')" ] || die "sre: no toolset"
  log "HelmRelease kagent/sre values: skill ref main, iconUrl, runtime go, toolset — OK"
  wait_agent_ready kagent sre 600
  T_SHAPE1=$(( $(now) - t0 ))
  log "shape 1 (sre) Ready in ${T_SHAPE1}s"
}

step_shape2_narrow() {
  step shape2-narrow
  local t0; t0=$(now)
  kubectl apply -f "$NARROW_MANIFEST" >&2
  wait_agent_ready kagent narrow 600 >/dev/null
  T_SHAPE2=$(( $(now) - t0 ))
  log "shape 2 (narrow) Ready in ${T_SHAPE2}s"
}

step_shape3_gitops() {
  step shape3-gitops
  local t0; t0=$(now)
  kubectl apply -f "$GITOPS_MANIFEST" >&2
  wait_agent_ready kagent sre-agent 600 >/dev/null
  T_SHAPE3=$(( $(now) - t0 ))
  assert_label agent kagent sre-agent helm.toolkit.fluxcd.io/name sre-agent
  assert_label agent kagent sre-agent helm.toolkit.fluxcd.io/namespace flux-giantswarm
  log "shape 3 (flux-giantswarm/sre-agent -> kagent/sre-agent) Ready in ${T_SHAPE3}s"
}

step_record() {
  step record
  kubectl -n kagent get agents -o wide >| "$EVIDENCE/agents.txt"
  kubectl get helmrelease,ocirepository -n kagent -o yaml >| "$EVIDENCE/hr-oci-kagent.yaml"
  kubectl get helmrelease,ocirepository -n flux-giantswarm -o yaml >| "$EVIDENCE/hr-oci-flux-giantswarm.yaml"
  kubectl -n agent-platform get helmrelease kagent -o jsonpath='{.spec.values}' | "$YQ" -P '.' >| "$EVIDENCE/values-agent-platform-kagent.yaml"
  kubectl -n kagent get helmrelease sre -o jsonpath='{.spec.values}' | "$YQ" -P '.' >| "$EVIDENCE/values-kagent-sre.yaml"
  kubectl -n kagent get helmrelease narrow -o jsonpath='{.spec.values}' | "$YQ" -P '.' >| "$EVIDENCE/values-kagent-narrow.yaml"
  kubectl -n flux-giantswarm get helmrelease sre-agent -o jsonpath='{.spec.values}' | "$YQ" -P '.' >| "$EVIDENCE/values-flux-giantswarm-sre-agent.yaml"
  helm -n agent-platform history agent-platform >| "$EVIDENCE/after-helm-history.txt"
  local agent_chart_kagent agent_chart_gitops
  agent_chart_kagent=$(kubectl -n kagent get ocirepository agent -o jsonpath='{.status.artifact.revision}')
  agent_chart_gitops=$(kubectl -n flux-giantswarm get ocirepository agent -o jsonpath='{.status.artifact.revision}')
  {
    echo "# Stage 1 seed — $CHART_VERSION on $CLUSTER ($(date -u +%Y-%m-%dT%H:%MZ))"
    echo
    echo "| step | seconds |"
    echo "|---|---|"
    echo "| agentlab down | $T_DOWN |"
    echo "| agentlab up | $T_UP |"
    echo "| agentlab platform (re-run) | $T_PLATFORM |"
    echo "| shape 4 k8s-agent (bundled) time-to-Ready after up | $T_SHAPE4 |"
    echo "| shape 1 sre (portal via agent-manager) time-to-Ready | $T_SHAPE1 |"
    echo "| shape 2 narrow (toolNames) time-to-Ready | $T_SHAPE2 |"
    echo "| shape 3 flux-giantswarm/sre-agent (GitOps) time-to-Ready | $T_SHAPE3 |"
    echo
    echo "platform-test: $PLATFORM_TEST"
    echo "kagent chart: $KAGENT_CHART; agent chart resolved: kagent/agent=$agent_chart_kagent flux-giantswarm/agent=$agent_chart_gitops"
  } >| "$EVIDENCE/timings.md"
  status "SEEDED sre (portal, skill main), narrow (toolNames), flux-giantswarm/sre-agent → kagent/sre-agent (GitOps), k8s-agent (bundled) — all Accepted, times shape4=${T_SHAPE4}s shape1=${T_SHAPE1}s shape2=${T_SHAPE2}s shape3=${T_SHAPE3}s"
  status "LAB HELD ($CHART_VERSION seeded — NOT the baseline; stage 2a upgrades in place; lock stays until the rehearsal ends)"
  cat "$EVIDENCE/timings.md"
}

main() {
  step_preflight
  step_lock_and_before
  step_config
  step_down
  step_up
  step_assert_platform
  step_platform_test
  step_shape4_bundled
  step_shape1_portal
  step_shape2_narrow
  step_shape3_gitops
  step_record
}

main "$@"
