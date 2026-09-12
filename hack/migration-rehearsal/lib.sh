# shellcheck shell=bash
# Helpers shared by the rehearsal's stage scripts (seed.sh, upgrade.sh,
# contract.sh): stage-prefixed logging, the append-only status log, the
# failure paths that leave the lab as is, the lab lock, the Anthropic key,
# a mikefarah yq and HelmRelease readers. Sourced, never run.
#
# Inputs (variables of the sourcing script):
#   STAGE          log prefix (default: the sourcing script's name)
#   EVIDENCE       evidence directory named in BLOCKED status lines
#   LOG            when set, every log line is appended to this file too
#   STATUS_FILE    append-only status log; no status lines when unset
#   STATUS_PREFIX  prefix of every status line (default: $STAGE)
#   LAB_DIR        the lab directory whose state/lab-lock the lock helpers use

STAGE="${STAGE:-$(basename "${BASH_SOURCE[1]:-$0}" .sh)}"
CURRENT_STEP="${CURRENT_STEP:-preflight}"
LOCK_TAG='agentlab#143'
export STAGE

log() { printf '%s %s: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$STAGE" "$*" | tee -a "${LOG:-/dev/null}" >&2; }
now() { date +%s; }
utc() { date -u +%Y-%m-%dT%H:%MZ; }
step() { CURRENT_STEP="$1"; log "== $1"; }

status() {
  [ -n "${STATUS_FILE:-}" ] || return 0
  printf '%s %s %s\n' "$(utc)" "${STATUS_PREFIX:-$STAGE}" "$*" >>"$STATUS_FILE"
}

# A failed assertion or precondition: say so, leave the lab exactly as is.
die() {
  log "FATAL: $*"
  status "BLOCKED step=$CURRENT_STEP — ${1%%$'\n'*} — lab left as is (lock HELD); see ${EVIDENCE:-?}"
  trap - ERR
  exit 1
}
# The ERR trap of seed.sh and upgrade.sh (`trap on_error ERR` under set -E).
on_error() {
  local rc=$?
  log "step '$CURRENT_STEP' failed (exit $rc)"
  status "BLOCKED step=$CURRENT_STEP exit=$rc — lab left as is (lock HELD); see ${EVIDENCE:-?}"
  exit "$rc"
}

# ------------------------------------------------------------------- lock ---
# state/lab-lock/owner under $LAB_DIR (the cwd once a stage's preflight ran).

lock_take() { # <owner text>: take the lock, or accept one this rehearsal already holds
  if /usr/bin/mkdir state/lab-lock 2>/dev/null; then
    echo "$1" >| state/lab-lock/owner
    log "lab lock taken"
  elif lock_is_ours; then
    log "lab lock already held by this rehearsal: $(cat state/lab-lock/owner)"
  else
    cat state/lab-lock/owner 2>/dev/null || true
    die "lab lock held by someone else"
  fi
}
lock_is_ours() { [ -f state/lab-lock/owner ] && /usr/bin/grep -q "$LOCK_TAG" state/lab-lock/owner; }
lock_require() { # the lock must be this rehearsal's
  lock_is_ours || die "the lab lock is not held by the $LOCK_TAG rehearsal: $(cat state/lab-lock/owner 2>&1)"
}
lock_note() { # <text>: append a stage note to the owner file, once
  local owner="${LAB_DIR:-.}/state/lab-lock/owner"
  [ -f "$owner" ] || return 0
  /usr/bin/grep -qF -- "$1" "$owner" || printf '; %s since %s' "$1" "$(date -Is)" >>"$owner"
  log "lock owner: $(cat "$owner")"
}

# -------------------------------------------------------------------- key ---

# export_anthropic_key <min-bytes>: from the Secret kagent/kagent-anthropic
# unless the environment carries one; only its length is ever logged.
export_anthropic_key() {
  if [ -z "${ANTHROPIC_API_KEY:-}" ]; then
    local key
    key=$(kubectl -n kagent get secret kagent-anthropic -o jsonpath='{.data.ANTHROPIC_API_KEY}' | base64 -d)
    export ANTHROPIC_API_KEY="$key"
  fi
  [ "${#ANTHROPIC_API_KEY}" -ge "$1" ] || die "ANTHROPIC_API_KEY has ${#ANTHROPIC_API_KEY} bytes, want at least $1"
  log "ANTHROPIC_API_KEY exported (${#ANTHROPIC_API_KEY} bytes)"
}

# Never let the key reach an evidence file: redact it if a tool echoed it.
scrub_key() {
  local f="$1"
  [ -f "$f" ] || return 0
  if /usr/bin/grep -qF -- "$ANTHROPIC_API_KEY" "$f"; then
    log "WARNING: the Anthropic key appeared in $f — redacted"
    sed -i "s|$ANTHROPIC_API_KEY|<redacted>|g" "$f"
  fi
}

# ----------------------------------------------------------------- github ---

# github_window <what>: print GitHub's core API window before a step that
# resolves skills through GitHub (the migrate Job, agent-manager), the way
# `agentlab backstage-test` and `agents-test` do. Authenticated with
# GITHUB_TOKEN from the environment, else with the lab's own Secret
# agent-platform/agentlab-github-token when `agentlab platform` created one;
# the token reaches curl through a header file, never argv, and is never
# logged. An exhausted window is waited out once (bounded to an hour and a
# bit) rather than failing later on a truncated resolution; an endpoint that
# cannot be read is a log line, never a failure.
github_window() {
  local what="$1" token="${GITHUB_TOKEN:-}" who json limit remaining reset wait
  if [ -z "$token" ]; then
    token=$(kubectl -n agent-platform get secret agentlab-github-token -o jsonpath='{.data.GITHUB_TOKEN}' 2>/dev/null | base64 -d 2>/dev/null || true)
  fi
  who="unauthenticated: this machine's shared window"
  if [ -n "$token" ]; then
    who="authenticated with GITHUB_TOKEN"
    json=$(curl -sS --max-time 15 -H 'Accept: application/vnd.github+json' \
      -H @<(printf 'Authorization: Bearer %s\n' "$token") https://api.github.com/rate_limit 2>/dev/null) || json=''
  else
    json=$(curl -sS --max-time 15 -H 'Accept: application/vnd.github+json' https://api.github.com/rate_limit 2>/dev/null) || json=''
  fi
  limit=$(jq -r '.resources.core.limit // empty' <<<"$json" 2>/dev/null || true)
  remaining=$(jq -r '.resources.core.remaining // empty' <<<"$json" 2>/dev/null || true)
  reset=$(jq -r '.resources.core.reset // empty' <<<"$json" 2>/dev/null || true)
  if ! [[ $remaining =~ ^[0-9]+$ && $reset =~ ^[0-9]+$ ]]; then
    log "GitHub API window not read; $what proceeds without it"
    return 0
  fi
  log "GitHub API window ($who): $remaining of $limit requests remaining, resets $(date -d "@$reset" +%H:%M:%S)"
  if (( remaining == 0 )); then
    wait=$(( reset - $(now) + 5 ))
    (( wait < 0 )) && wait=0
    (( wait > 3900 )) && wait=3900
    log "the window is exhausted — waiting ${wait}s for it to reset at $(date -d "@$reset" +%H:%M:%S) before $what (one bounded wait; export GITHUB_TOKEN to lift it to 5000 an hour)"
    sleep "$wait"
  fi
}

# ------------------------------------------------------------------ tools ---

pick_yq() { # prints the first mikefarah yq v4 found (YQ, yq, ~/.go/bin/yq, go-yq)
  local c
  for c in "${YQ:-}" yq "$HOME/.go/bin/yq" go-yq; do
    [ -n "$c" ] || continue
    if command -v "$c" >/dev/null 2>&1 && "$c" --version 2>&1 | /usr/bin/grep -q mikefarah; then
      echo "$c"; return 0
    fi
  done
  return 1
}

hr_chart_version() { # <ns> <name>: the chart version of the last deployed revision
  kubectl -n "$1" get helmrelease "$2" -o jsonpath='{.status.history[0].chartVersion}' 2>/dev/null
}
