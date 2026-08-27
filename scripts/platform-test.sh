#!/usr/bin/env bash
# Headless end-to-end proof: Dex login -> muster (OAuth-protected) -> the
# Kubernetes MCP -> the kind apiserver.
#
# Uses the Dex password grant with client_id=muster, which yields an id_token
# with aud=muster. muster accepts it directly because `muster` is listed under
# oauth.server.trustedAudiences. Claude Code instead does the full browser
# authorization-code flow; this script is the CI-friendly shortcut.
set -euo pipefail
cd "$(dirname "$0")/.."

MUSTER=${MUSTER:-http://localhost:8090}
USER_EMAIL=${1:-admin@lab.local}

if ! curl -sf -o /dev/null "$MUSTER/.well-known/oauth-authorization-server"; then
  echo "muster is not reachable at $MUSTER — run 'make platform-forward' first" >&2
  exit 1
fi

echo "==> Logging in to Dex as $USER_EMAIL"
TOK=$(curl -sS --cacert certs/ca.crt -X POST https://localhost:32000/dex/token \
  -d grant_type=password -d "username=$USER_EMAIL" -d password=password \
  -d client_id=muster -d client_secret=muster-lab-secret \
  -d "scope=openid email groups profile" | jq -r .id_token)
[[ "$TOK" == "null" || -z "$TOK" ]] && { echo "    Dex refused the login"; exit 1; }
echo "    got an id_token"

hdr=(-H "Authorization: Bearer $TOK" -H 'Content-Type: application/json'
     -H 'Accept: application/json, text/event-stream')

HDRS=$(mktemp); BODY=$(mktemp)
trap 'rm -f "$HDRS" "$BODY"' EXIT

echo "==> MCP initialize"
SID=$(curl -sS -D "$HDRS" -o "$BODY" -X POST "$MUSTER/mcp" "${hdr[@]}" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"platform-test","version":"1"}}}' \
  && grep -i '^mcp-session-id' "$HDRS" | awk '{print $2}' | tr -d '\r')
[[ -z "$SID" ]] && { echo "    no session id — muster rejected the token"; cat "$BODY"; exit 1; }
echo "    session $SID"
curl -sS -o /dev/null -X POST "$MUSTER/mcp" "${hdr[@]}" -H "Mcp-Session-Id: $SID" \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'

call() {
  curl -sS -X POST "$MUSTER/mcp" "${hdr[@]}" -H "Mcp-Session-Id: $SID" -d "$1"
}

echo "==> Kubernetes tools muster is aggregating"
call '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_tools","arguments":{}}}' \
  | jq -r '.result.content[0].text' | jq -r '.tools[].name' | grep '^x_kubernetes_' | head -8 | sed 's/^/    /'

# The `kubernetes` group is rendered as a muster FAMILY (instanceArg
# management_cluster), so every call must name the backing MCPServer CR.
echo "==> Calling x_kubernetes_list namespaces through muster"
call '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"call_tool","arguments":{"name":"x_kubernetes_list","arguments":{"management_cluster":"dexlab-mcp-kubernetes","resourceType":"namespaces"}}}}' \
  | jq -r '.result.content[0].text' | jq -r '.content[0].text' \
  | jq -r '"    namespaces: " + ([.items[].name] | join(", "))'

echo
echo "PASS: Claude Code -> muster (Dex) -> mcp-kubernetes -> kind apiserver"
