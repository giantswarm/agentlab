#!/usr/bin/env bash
# Stage 2b of the migration rehearsal (agentlab#143): the operator's diff PR
# applied, the releases on the 1.x agent chart, the migrate Job's contract and
# no-op runs asserted, one turn per migrated agent through the edge.
#
# Usage: contract.sh <evidence-dir> [expand-run-report.yaml]
#   The second argument is the expand run's report (the ConfigMap holds only
#   the latest run): its pinned skill commit is asserted on the template.
#
# Inputs (environment, all optional):
#   GITOPS_MANIFEST  the manifest applied first (default: seed-gitops-1x.yaml
#                    next to this script; empty skips the apply)
#   AGENTLAB         the agentlab binary (default: agentlab on PATH)
#   LAB_DIR          the lab directory whose lock owner file gets this stage
#                    appended (default: $PWD)
#   START_STEP       resume at this step (1-6, default 1) with the evidence of
#                    the earlier steps already in the evidence dir
#
# The recipe is docs/migration-rehearsal.md. Every wait is bounded (timeout); a
# failed assertion stops the script. The functions are sourceable:
# `source contract.sh <evidence-dir>` defines them without running main.
set -euo pipefail

EVIDENCE=${1:?usage: contract.sh <evidence-dir> [expand-run-report.yaml]}
RUN1_REPORT=${2:-}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
GITOPS_MANIFEST=${GITOPS_MANIFEST-$HERE/seed-gitops-1x.yaml}
AGENTLAB=${AGENTLAB:-agentlab}
LAB_DIR=${LAB_DIR:-$PWD}
START_STEP=${START_STEP:-1}
NS=kagent
GITOPS_NS=flux-giantswarm
HARNESS=kagent
REPORT_CM=agent-manager-migrate-report
JOB_LABEL=app.kubernetes.io/component=agent-manager-migrate
LOG=$EVIDENCE/contract.log
TIMINGS=$EVIDENCE/timings.md
export EVIDENCE RUN1_REPORT NS GITOPS_NS HARNESS REPORT_CM JOB_LABEL LOG TIMINGS AGENTLAB LAB_DIR

# shellcheck source-path=SCRIPTDIR
# shellcheck source=lib.sh
source "$HERE/lib.sh"
fail() { die "ASSERTION FAILED: $*"; }
timing() { printf '| %s | %s |\n' "$1" "$2" >>"$TIMINGS"; log "timing: $1: $2"; }
due() { ((START_STEP <= $1)); }
# bounded runs a function under timeout, in a subshell that inherits the
# exported functions and variables; a timeout is a failed assertion.
bounded() {
  local secs=$1 rc=0
  shift
  timeout --foreground "$secs" bash -c 'set -euo pipefail; "$@"' _ "$@" || rc=$?
  ((rc == 124)) && fail "'$*' did not finish within ${secs}s"
  ((rc == 0)) || exit "$rc"
}

save_report() { # <basename>: the latest report as YAML and JSON
  kubectl -n "$NS" get cm "$REPORT_CM" -o jsonpath='{.data.report\.yaml}' >|"$EVIDENCE/$1.yaml"
  yq . "$EVIDENCE/$1.yaml" >|"$EVIDENCE/$1.json"
}
snapshot() { # <prefix>: the releases, sources, templates, v1alpha2 Agents, RemoteMCPServers, CRDs and the report
  {
    kubectl -n "$NS" get helmreleases,ocirepositories,agenttemplates -o wide
    kubectl -n "$NS" get agents.kagent.dev -o wide 2>&1 || true
    kubectl -n "$NS" get remotemcpservers 2>&1 || true
  } >|"$EVIDENCE/$1-kagent.txt" 2>&1
  kubectl -n "$GITOPS_NS" get helmreleases,ocirepositories -o wide >|"$EVIDENCE/$1-flux-giantswarm.txt" 2>&1
  kubectl get crd -o name | /usr/bin/grep '\.kagent\.dev$' >|"$EVIDENCE/$1-crds.txt"
  save_report "$1-report"
}
apply_gitops() {
  [[ -n $GITOPS_MANIFEST ]] || { log "GITOPS_MANIFEST empty: not applying anything"; return 0; }
  kubectl apply -f "$GITOPS_MANIFEST" | tee "$EVIDENCE/gitops-apply.txt" | sed 's/^/  /' | tee -a "$LOG"
  date -u +%FT%TZ >|"$EVIDENCE/gitops-applied-at.txt"
  log "the operator's pull request applied at $(cat "$EVIDENCE/gitops-applied-at.txt")"
}

hr_state() { # <ns> <name>: one line — chart, Ready status, Ready message
  kubectl -n "$1" get helmrelease "$2" -o json | jq -r '
    (.status.conditions // [] | map(select(.type=="Ready")) | .[0]) as $r
    | "chart=\(.status.history[0].chartVersion // "-") ready=\($r.status // "-") \($r.message // "" | .[0:200])"'
}
nudge_sources() { # once: ask Flux to reconcile the sources and releases now
  [[ -f $EVIDENCE/.nudged ]] && return 0
  local at
  at=$(date +%s)
  kubectl annotate --overwrite ocirepository -n "$NS" agent reconcile.fluxcd.io/requestedAt="$at" >/dev/null
  kubectl annotate --overwrite ocirepository -n "$GITOPS_NS" agent reconcile.fluxcd.io/requestedAt="$at" >/dev/null
  kubectl annotate --overwrite helmrelease -n "$NS" sre narrow reconcile.fluxcd.io/requestedAt="$at" >/dev/null
  kubectl annotate --overwrite helmrelease -n "$GITOPS_NS" sre-agent reconcile.fluxcd.io/requestedAt="$at" >/dev/null
  touch "$EVIDENCE/.nudged"
  log "  nudged the two OCIRepositories and the three HelmReleases once (reconcile.fluxcd.io/requestedAt=$at)"
}
wait_release_1x() { # <ns> <name>: poll 15 s until Ready on chart 1.x; log each change; time the schema refusal
  local ns=$1 name=$2 start last="" state schema_from="" schema="none seen"
  start=$(now)
  while :; do
    state=$(hr_state "$ns" "$name")
    if [[ $state != "$last" ]]; then
      log "  $ns/$name: $state"
      last=$state
      if [[ $state == *"ready=False"* && $state == *schema* ]]; then
        schema_from=${schema_from:-$(now)}
      elif [[ -n $schema_from ]]; then
        schema="$(($(now) - schema_from)) s (15 s resolution; the events carry the exact stamps)"
        schema_from=""
      fi
    fi
    if [[ $state == chart=1.* && $state == *"ready=True"* ]]; then
      timing "release $ns/$name Ready on ${state%% *}" "$(($(now) - start)) s; schema refusal $schema"
      return 0
    fi
    (($(now) - start > 120)) && nudge_sources
    sleep 15
  done
}
save_events() { # the timestamps of the refusal and the upgrade, for the exact schema interval
  kubectl -n "$GITOPS_NS" get events --field-selector involvedObject.name=sre-agent --sort-by=.lastTimestamp \
    -o custom-columns='LAST:.lastTimestamp,FIRST:.firstTimestamp,COUNT:.count,REASON:.reason,MESSAGE:.message' >|"$EVIDENCE/events-sre-agent.txt" 2>&1 || true
  kubectl -n "$NS" get events --sort-by=.lastTimestamp \
    -o custom-columns='LAST:.lastTimestamp,OBJECT:.involvedObject.kind/.involvedObject.name,REASON:.reason,MESSAGE:.message' >|"$EVIDENCE/events-kagent.txt" 2>&1 || true
  /usr/bin/grep -i 'schema\|upgrade succeeded\|install succeeded' "$EVIDENCE/events-sre-agent.txt" | cut -c1-220 | sed 's/^/  event: /' | tee -a "$LOG" || true
}

tmpl_ready() { # <name>: "<status>|<reason>|<message>" of the Ready condition in the Harness's status.harnesses[] entry
  kubectl -n "$NS" get agenttemplate "$1" -o json 2>/dev/null | jq -r --arg h "$HARNESS" '
    ((.status.harnesses // [])[] | select(.harness==$h) | (.conditions // [])[] | select(.type=="Ready")
      | "\(.status)|\(.reason // "")|\(.message // "" | .[0:200])") // "absent||no entry for Harness \($h) yet"' ||
    echo "missing||the AgentTemplate does not exist"
}
wait_templates() { # <name>...: each Ready on the Harness; one failed golden boot is noted (the Harness retries), a second is a platform defect
  local start t state all
  local -A last=() fails=() ready=()
  start=$(now)
  while :; do
    all=1
    for t in "$@"; do
      [[ -n ${ready[$t]:-} ]] && continue
      state=$(tmpl_ready "$t")
      if [[ $state != "${last[$t]:-}" ]]; then
        log "  agenttemplate $t: $state"
        last[$t]=$state
        if [[ $state == False* && $state == *[Ff]ailed* ]]; then
          fails[$t]=$((${fails[$t]:-0} + 1))
          if ((fails[$t] >= 2)); then
            kubectl -n "$NS" get agenttemplate "$t" -o yaml >|"$EVIDENCE/found-issue-agenttemplate-$t.yaml"
            kubectl -n "$NS" get pod -l ate.dev/worker-pool=kagent-default -o wide >|"$EVIDENCE/found-issue-worker-pods.txt" 2>&1 || true
            fail "FOUND-ISSUE: agenttemplate $t failed its golden boot twice: $state"
          fi
          log "  (the golden boot of $t failed once; waiting for the Harness's retry)"
        fi
      fi
      if [[ $state == True* ]]; then
        ready[$t]=1
        timing "agenttemplate $t Ready on Harness $HARNESS" "$(($(now) - start)) s (boot failures seen: ${fails[$t]:-0})"
      else
        all=0
      fi
    done
    ((all)) && return 0
    sleep 10
  done
}
assert_templates_ready() { # <name>...: still Ready now
  local t state
  for t in "$@"; do
    state=$(tmpl_ready "$t")
    [[ $state == True* ]] || fail "agenttemplate $t is not Ready on Harness $HARNESS: $state"
  done
  log "  agenttemplates $* Ready on Harness $HARNESS"
}
assert_skill_pin() { # the sre template's skill is pinned to a full commit, the one the expand report named
  local commit want=""
  commit=$(kubectl -n "$NS" get agenttemplate sre -o jsonpath='{.spec.skills[0].source.git.commit}')
  [[ $commit =~ ^[0-9a-f]{40}$ ]] || fail "agenttemplate sre: spec.skills[0].source.git.commit is not a 40-hex commit: '$commit'"
  if [[ -n $RUN1_REPORT ]]; then
    want=$(yq -r '[.releases[] | select(.name=="sre") | (.changes.skills // [])[] | .pinned] | first // ""' "$RUN1_REPORT")
    [[ -n $want ]] || fail "the expand report $RUN1_REPORT names no pinned skill commit for sre"
    [[ $commit == "$want" ]] || fail "agenttemplate sre: skill commit $commit differs from the expand report's pinned $want"
  fi
  echo "$commit" >|"$EVIDENCE/sre-skill-commit.txt"
  log "  agenttemplate sre: skill pinned to $commit${want:+ (= the expand report)}"
}
assert_rmcp() { # <name>...: one RemoteMCPServer per agent
  local t
  for t in "$@"; do kubectl -n "$NS" get remotemcpserver "$t" >/dev/null || fail "no RemoteMCPServer $NS/$t"; done
  kubectl -n "$NS" get remotemcpservers >|"$EVIDENCE/remotemcpservers.txt"
  log "  RemoteMCPServers present for $*"
}

clone_job() { # <name>: the chart's UPGRADE.md recipe — the migrate Job cloned under a new name
  local src
  src=$(kubectl -n "$NS" get job -l "$JOB_LABEL" -o name | head -1)
  [[ -n $src ]] || fail "no migrate Job with label $JOB_LABEL to clone"
  kubectl -n "$NS" get "$src" -o json | jq --arg n "$1" '
    del(.status, .metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp, .metadata.managedFields, .spec.selector,
        .spec.template.metadata.labels["batch.kubernetes.io/controller-uid"], .spec.template.metadata.labels["controller-uid"],
        .spec.template.metadata.labels["batch.kubernetes.io/job-name"], .spec.template.metadata.labels["job-name"])
    | .metadata.name = $n' | kubectl create -f - | sed 's/^/  /' | tee -a "$LOG"
  log "  cloned from $src"
}
wait_job() { # <name>: until the Job is Complete; a Failed Job stops the script with its log
  local s
  while :; do
    s=$(kubectl -n "$NS" get job "$1" -o jsonpath='{.status.succeeded}/{.status.failed}')
    [[ $s == 1/* ]] && return 0
    if [[ ${s#*/} =~ ^[1-9] ]]; then
      kubectl -n "$NS" logs "job/$1" --all-containers >|"$EVIDENCE/$1.log" 2>&1 || true
      fail "job $1 failed (succeeded/failed=$s); log in $EVIDENCE/$1.log"
    fi
    sleep 5
  done
}
run_migrate() { # <name>: clone, wait, save the Job, its log and the report; print the phase
  local name=$1 start st ct
  start=$(now)
  clone_job "$name"
  bounded 600 wait_job "$name"
  kubectl -n "$NS" get job "$name" -o yaml >|"$EVIDENCE/$name.yaml"
  kubectl -n "$NS" logs "job/$name" --all-containers >|"$EVIDENCE/$name.log" 2>&1 || true
  save_report "$name-report"
  st=$(kubectl -n "$NS" get job "$name" -o jsonpath='{.status.startTime}')
  ct=$(kubectl -n "$NS" get job "$name" -o jsonpath='{.status.completionTime}')
  timing "migrate Job $name" "$(($(date -d "$ct" +%s) - $(date -d "$st" +%s))) s (start $st, completion $ct; $(($(now) - start)) s wall)"
  jq -r '"  report: phase=\(.phase) changed=\(.changed) at=\(.run.at)\n  summary: \(.summary)"' "$EVIDENCE/$name-report.json" | tee -a "$LOG"
}
assert_contract() { # <report.json> <expected-agents.txt>: the contract phase deleted what was left and retired the five CRDs
  local r=$1 phase deleted expected crds c
  phase=$(jq -r .phase "$r")
  [[ $phase == contract ]] || fail "run 2: phase=$phase, want contract"
  deleted=$(jq -r '[(.contract.agentsDeleted // [])[] | (.name // .)] | sort | join(",")' "$r")
  expected=$(paste -sd, "$2")
  log "  contract.agentsDeleted=[$deleted]; the v1alpha2 Agents that existed before run 2: [$expected]"
  [[ $deleted == "$expected" ]] || fail "run 2: contract.agentsDeleted=[$deleted] but [$expected] existed"
  crds=$(jq -r '[(.contract.crds // [])[] | "\(.name // .crd // .)=\(.action // "?")"] | sort | join(" ")' "$r")
  log "  contract.crds: $crds"
  for c in agents sandboxagents agentharnesses memories toolservers; do
    jq -e --arg c "$c" '(.contract.crds // [])[] | select(((.name // .crd // .) | startswith($c)) and .action=="deleted")' "$r" >/dev/null ||
      fail "run 2: contract.crds has no action=deleted entry for $c"
    ! kubectl get crd "$c.kagent.dev" >/dev/null 2>&1 || fail "run 2: crd $c.kagent.dev is still present"
  done
  for c in modelconfigs remotemcpservers agenttemplates; do
    kubectl get crd "$c.kagent.dev" >/dev/null || fail "run 2: crd $c.kagent.dev is gone"
  done
  kubectl get crd -o name | /usr/bin/grep '\.kagent\.dev$' >|"$EVIDENCE/after-run2-crds.txt"
  log "  live CRDs: the five retired kinds NotFound; modelconfigs, remotemcpservers, agenttemplates present ($(wc -l <"$EVIDENCE/after-run2-crds.txt") kagent.dev CRDs)"
}
assert_complete() { # <report.json>: the no-op — complete, unchanged, every release on 1.x
  local r=$1 phase changed
  phase=$(jq -r .phase "$r")
  changed=$(jq -r .changed "$r")
  [[ $phase == complete ]] || fail "run 3: phase=$phase, want complete"
  [[ $changed == false ]] || fail "run 3: changed=$changed, want false"
  [[ $(jq -r '(.pending // []) | length' "$r") == 0 ]] || fail "run 3: pending is not empty: $(jq -c .pending "$r")"
  jq -r '.releases[] | "  release \(.namespace)/\(.name): action=\(.action) chart=\(.chartVersion // "-") template.verdict=\(.template.verdict // "-")"' "$r" | tee -a "$LOG"
  jq -e '[.releases[] | (.chartVersion // "") | startswith("1.")] | all' "$r" >/dev/null || fail "run 3: not every release is on chart 1.x"
  jq -e '[.releases[] | select(.template != null) | .template.verdict == "ready"] | all' "$r" >/dev/null || fail "run 3: a release's template.verdict is not ready"
  log "  agents left in the report: $(jq -c '(.agents // []) | map(.name)' "$r")"
}

turn() { # <template> <prompt>: one turn as admin@lab.local through the edge; completed, non-empty, the instance deleted
  local t=$1 prompt=$2 out start rc=0 id answer
  out=$EVIDENCE/turn-$t.txt
  start=$(now)
  timeout --foreground 300 "$AGENTLAB" turn --user admin@lab.local --template "$t" "$prompt" >|"$out" 2>&1 || rc=$?
  sed 's/^/  turn> /' "$out" | tee -a "$LOG"
  ((rc == 0)) || fail "agentlab turn on $t exited $rc"
  /usr/bin/grep -q '^state: completed' "$out" || fail "turn on $t did not end completed"
  id=$(sed -n 's/^instance: \([^ ]*\).*/\1/p' "$out")
  answer=$(sed -n '/^answer: /,/^deleted: /p' "$out" | sed '1s/^answer: //; $d')
  [[ -n ${answer//[[:space:]]/} ]] || fail "turn on $t answered nothing"
  /usr/bin/grep -q "^deleted: $id\$" "$out" || fail "turn on $t: AgentInstance $id not confirmed deleted"
  echo "$id" >>"$EVIDENCE/turn-instances.txt"
  timing "turn on $t as admin@lab.local (instance $id)" "$(($(now) - start)) s"
}
dev_roster() { # the roster a developer sees, and the RBAC behind it — recorded, not asserted
  local out=$EVIDENCE/dev-roster.txt rc=0 can
  timeout --foreground 120 "$AGENTLAB" turn --user dev@lab.local --list >|"$out" 2>&1 || rc=$?
  sed 's/^/  roster> /' "$out" | tee -a "$LOG"
  log "  agentlab turn --user dev@lab.local --list exited $rc"
  can=$(kubectl auth can-i list agenttemplates.kagent.dev -n "$NS" --as=oidc:dev@lab.local --as-group=oidc:developers 2>&1 || true)
  echo "$can" >|"$EVIDENCE/dev-can-i.txt"
  log "  kubectl auth can-i list agenttemplates.kagent.dev as oidc:dev@lab.local (oidc:developers): $can (expected: no)"
}
assert_nothing_left() { # no AgentInstance of ours; the templates and their releases stay
  local left
  left=$(kubectl -n "$NS" get agentinstances -o name 2>/dev/null || true)
  [[ -z $left ]] || fail "leftover AgentInstances: $left"
  log "  AgentInstances: none served as a resource; every turn's instance confirmed deleted ($(paste -sd, "$EVIDENCE/turn-instances.txt"))"
  kubectl -n "$NS" get pod -l ate.dev/worker-pool=kagent-default -o wide >|"$EVIDENCE/after-worker-pods.txt" 2>&1 || true
  local n
  n=$(kubectl -n "$NS" get agenttemplates -o name | wc -l)
  ((n >= 3)) || fail "only $n AgentTemplates left"
  if ! kubectl -n "$NS" get helmrelease sre narrow >/dev/null || ! kubectl -n "$GITOPS_NS" get helmrelease sre-agent >/dev/null; then
    fail "a release is gone"
  fi
}

while read -r _ _ fn; do export -f "${fn?}"; done < <(declare -F)

main() {
  local t0 report
  /usr/bin/mkdir -p "$EVIDENCE"
  [[ -s $TIMINGS ]] || printf '| step | timing |\n|---|---|\n' >|"$TIMINGS"
  trap 'rc=$?; ((rc)) && log "stopped with exit $rc; evidence in $EVIDENCE"' EXIT
  t0=$(now)
  log "stage 2b contract from step $START_STEP: evidence $EVIDENCE; $("$AGENTLAB" --version 2>/dev/null | head -1)"

  if due 1; then
    log "step 1: the operator's pull request"
    lock_note "stage 2b contract"
    snapshot before
    apply_gitops
  fi
  if due 2; then
    log "step 2: Flux upgrades the releases to the 1.x agent chart"
    bounded 900 wait_release_1x "$NS" sre
    bounded 900 wait_release_1x "$NS" narrow
    bounded 900 wait_release_1x "$GITOPS_NS" sre-agent
    save_events
    log "step 2: the AgentTemplates on Harness $HARNESS"
    bounded 900 wait_templates sre narrow sre-agent
    assert_skill_pin
    assert_rmcp sre narrow sre-agent
  fi
  if due 3; then
    log "step 3: run 2 — the contract phase"
    # What Helm left behind when the releases upgraded (the 1.x chart renders
    # the AgentTemplate in the Agent's place) is what the contract phase sweeps.
    kubectl -n "$NS" get agents.kagent.dev -o name 2>/dev/null | sed 's#.*/##' | sort >|"$EVIDENCE/before-run2-v1alpha2-agents.txt" || true
    log "  v1alpha2 Agents left for the contract phase: [$(paste -sd, "$EVIDENCE/before-run2-v1alpha2-agents.txt")]"
    run_migrate agent-manager-migrate-2
    report=$EVIDENCE/agent-manager-migrate-2-report.json
    if [[ $(jq -r .phase "$report") == wait ]]; then
      jq -r '.pending[]' "$report" | tee "$EVIDENCE/agent-manager-migrate-2-pending.txt" | sed 's/^/  pending: /' | tee -a "$LOG"
      log "  phase wait: waiting (bounded 10 min) for the releases and templates it names, then cloning once more"
      bounded 600 wait_release_1x "$GITOPS_NS" sre-agent
      bounded 600 wait_templates sre narrow sre-agent
      run_migrate agent-manager-migrate-2b
      report=$EVIDENCE/agent-manager-migrate-2b-report.json
    fi
    assert_contract "$report" "$EVIDENCE/before-run2-v1alpha2-agents.txt"
    assert_templates_ready sre narrow sre-agent
  fi
  if due 4; then
    log "step 4: run 3 — the no-op"
    run_migrate agent-manager-migrate-3
    assert_complete "$EVIDENCE/agent-manager-migrate-3-report.json"
  fi
  if due 5; then
    log "step 5: one turn per migrated agent as admin@lab.local, then the developer's roster"
    : >|"$EVIDENCE/turn-instances.txt"
    turn sre "In one short line: which skills do you have, and what is 2+2?"
    turn narrow "Answer with one word: pong"
    dev_roster
  fi
  if due 6; then
    log "step 6: after"
    snapshot after
    assert_nothing_left
  fi
  timing "stage 2b steps $START_STEP-6" "$(($(now) - t0)) s"
  log "done; timings:"
  cat "$TIMINGS"
}

[[ ${BASH_SOURCE[0]:-} == "$0" ]] && main "$@"
