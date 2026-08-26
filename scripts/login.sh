#!/usr/bin/env bash
# Headless login via the OAuth2 password grant (enabled by
# `oauth2.passwordConnector: local` in the Dex config).
# Writes the raw id_token to .token and prints its claims.
set -euo pipefail
cd "$(dirname "$0")/.."

USER_EMAIL="${1:-admin@lab.local}"
PASSWORD="${2:-password}"
ISSUER="https://127.0.0.1:32000/dex"

RESP=$(curl -sS --cacert certs/ca.crt "$ISSUER/token" \
  -u "kubernetes:kubernetes-lab-secret" \
  -d grant_type=password \
  -d "username=$USER_EMAIL" \
  -d "password=$PASSWORD" \
  -d "scope=openid email profile groups offline_access")

if ! echo "$RESP" | jq -e '.id_token' >/dev/null 2>&1; then
  echo "login failed:" >&2
  echo "$RESP" | jq . >&2 2>/dev/null || echo "$RESP" >&2
  exit 1
fi

ID_TOKEN=$(echo "$RESP" | jq -r .id_token)
printf '%s' "$ID_TOKEN" > .token

echo "id_token claims:"
./scripts/jwt-decode.sh "$ID_TOKEN" | jq .

# Build a kubeconfig that carries ONLY this token. A plain `kubectl --token=...`
# is not enough: the kind kubeconfig ships an admin client certificate, and a
# client cert always wins over a bearer token.
./scripts/mk-kubeconfig.sh "$ID_TOKEN" kubeconfig.oidc >/dev/null

# Clean up the stale context an older version of this script may have left in
# the default kubeconfig.
kubectl config delete-context dexlab-oidc >/dev/null 2>&1 || true
kubectl config unset users.dexlab-oidc    >/dev/null 2>&1 || true

cat <<MSG

Logged in as $USER_EMAIL.
  raw token   -> .token
  kubeconfig  -> kubeconfig.oidc

  export KUBECONFIG=$PWD/kubeconfig.oidc
  kubectl auth whoami
MSG
