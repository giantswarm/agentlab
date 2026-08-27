#!/usr/bin/env bash
# End-to-end proof: three lab users, three different sets of permissions,
# driven purely by the `groups` claim that Dex puts in the id_token.
set -uo pipefail
cd "$(dirname "$0")/.."

ISSUER="https://localhost:32000/dex"
PASS=0; FAIL=0

token_for() {
  curl -sS --cacert certs/ca.crt "$ISSUER/token" \
    -u "kubernetes:kubernetes-lab-secret" \
    -d grant_type=password -d "username=$1" -d "password=password" \
    -d "scope=openid email profile groups" | jq -r .id_token
}

# check <description> <expected: yes|no> <verb> <resource> [-n namespace]
check() {
  local desc="$1" expect="$2"; shift 2
  local got
  got=$(kubectl --kubeconfig="$KCFG" auth can-i "$@" 2>/dev/null)
  if [[ "$got" == "$expect" ]]; then
    printf '  \033[32mPASS\033[0m  %-46s (can-i %s = %s)\n' "$desc" "$*" "$got"; PASS=$((PASS+1))
  else
    printf '  \033[31mFAIL\033[0m  %-46s (can-i %s = %s, wanted %s)\n' "$desc" "$*" "$got" "$expect"; FAIL=$((FAIL+1))
  fi
}

for u in admin dev viewer; do
  EMAIL="$u@lab.local"
  TOKEN=$(token_for "$EMAIL")
  if [[ -z "$TOKEN" || "$TOKEN" == "null" ]]; then
    echo "could not get a token for $EMAIL"; exit 1
  fi

  echo
  echo "=== $EMAIL ==="
  KCFG=$(./scripts/mk-kubeconfig.sh "$TOKEN" ".kubeconfig.$u")
  echo -n "  identity: "
  kubectl --kubeconfig="$KCFG" auth whoami \
    -o jsonpath='{.status.userInfo.username}  {.status.userInfo.groups}' 2>&1
  echo

  case "$u" in
    admin)
      check "cluster-admin: list nodes"          yes get nodes
      check "cluster-admin: create ns"           yes create namespaces
      check "cluster-admin: write anywhere"      yes create deployments -n kube-system
      ;;
    dev)
      check "not a cluster admin"                no  get nodes
      check "cannot create namespaces"           no  create namespaces
      check "can write in ns/demo"               yes create deployments -n demo
      check "cannot write in ns/default"         no  create deployments -n default
      ;;
    viewer)
      check "read-only: list pods cluster-wide"  yes list pods --all-namespaces
      check "read-only: cannot create"           no  create deployments -n demo
      check "read-only: cannot delete"           no  delete pods -n kube-system
      ;;
  esac
done

echo
rm -f .kubeconfig.admin .kubeconfig.dev .kubeconfig.viewer
echo "-------------------------------------------------"
printf 'passed: %d   failed: %d\n' "$PASS" "$FAIL"
[[ $FAIL -eq 0 ]] || exit 1
