#!/usr/bin/env bash
# Builds a kubeconfig that authenticates ONLY with the given bearer token.
# Needed because the kind kubeconfig ships an admin client certificate, and a
# client cert always wins over --token / a token-based user.
#   usage: mk-kubeconfig.sh <id_token> <output-path>
set -euo pipefail
TOKEN="$1"; OUT="$2"

kind get kubeconfig --name dexlab > "$OUT"
export KUBECONFIG="$OUT"
kubectl config unset users >/dev/null
kubectl config unset contexts >/dev/null
kubectl config set-credentials oidc --token="$TOKEN" >/dev/null
kubectl config set-context oidc --cluster=kind-dexlab --user=oidc >/dev/null
kubectl config use-context oidc >/dev/null
echo "$OUT"
