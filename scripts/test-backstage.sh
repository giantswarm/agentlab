#!/usr/bin/env bash
# Drives the full Backstage <-> Dex sign-in headlessly, reports the identity
# Backstage resolved, and then proves the Giant Swarm muster plugin can reach
# muster with that user's forwarded token.
# Usage: test-backstage.sh [user@lab.local ...]
set -uo pipefail
cd "$(dirname "$0")/.."

signin() {
  local email="$1" jar body
  jar=$(mktemp)
  local curl_=(curl -sS --cacert certs/ca.crt -c "$jar" -b "$jar")

  # 1. Backstage redirects to Dex and sets its own session cookie. The provider
  #    name (oidc-lab) and the auth.environment (development) both come from
  #    manifests/backstage.yaml and must match, or this 404s.
  #    The scope list is passed explicitly because only the BROWSER app applies
  #    plugins/gs/src/apis/auth/scopes.ts (BASE_SCOPES + gs.auth.extraScopes);
  #    hitting /start directly would otherwise get a bare token with no groups
  #    and aud=["backstage"] alone.
  local scope="openid profile email groups offline_access"
  scope+=" audience:server:client_id:kubernetes audience:server:client_id:muster"
  local dex_url
  dex_url=$("${curl_[@]}" -o /dev/null -D - -G --data-urlencode "scope=$scope" \
            "http://localhost:7007/api/auth/oidc-lab/start?env=development" \
            | awk 'tolower($1)=="location:"{print $2}' | tr -d '\r')
  [[ -z "$dex_url" ]] && { echo "  no redirect to Dex"; rm -f "$jar"; return 1; }

  # 2. Follow to Dex's login form, 3. submit lab credentials.
  local login_url
  login_url=$("${curl_[@]}" -L -o /dev/null -w '%{url_effective}' "$dex_url")
  body=$("${curl_[@]}" -L --data-urlencode "login=$email" \
         --data-urlencode "password=password" "$login_url")
  rm -f "$jar"

  python3 - "$body" <<'PY'
import re, json, base64, sys, urllib.parse, urllib.request

raw = sys.argv[1]
m = re.search(r"decodeURIComponent\('([^']+)'\)", raw)
if not m:
    print("  could not parse the handler response"); sys.exit(1)
data = json.loads(urllib.parse.unquote(m.group(1)))
if data.get('type') != 'authorization_response' or 'error' in data.get('response', {}):
    print("  SIGN-IN FAILED:", json.dumps(data.get('response', data))[:400]); sys.exit(1)
r = data['response']

def claims(t):
    p = t.split('.')[1]
    return json.loads(base64.urlsafe_b64decode(p + '=' * (-len(p) % 4)))

dex_id_token = r['providerInfo']['idToken']
dex = claims(dex_id_token)
ident = r['backstageIdentity']['identity']
bs_token = r['backstageIdentity']['token']

print(f"  dex asserted    groups={dex.get('groups')} email={dex.get('email')}")
print(f"  token audience  {dex.get('aud')}")
print(f"  backstage user  {ident['userEntityRef']}")
print(f"  ownership refs  {ident['ownershipEntityRefs']}")

# The muster plugin forwards the Dex id_token in its own header; the backend
# promotes it to Authorization: Bearer on the MCP session to muster.
def muster(path):
    req = urllib.request.Request(
        f"http://localhost:7007/api/muster{path}",
        headers={
            'Authorization': f'Bearer {bs_token}',
            'backstage-muster-authorization': dex_id_token,
        })
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:200]

def unwrap(node, key):
    """muster tool results nest JSON inside content[].text, twice over."""
    stack = [node]
    while stack:
        cur = stack.pop()
        if isinstance(cur, str):
            try:
                cur = json.loads(cur)
            except (ValueError, TypeError):
                continue
        if isinstance(cur, dict):
            if key in cur:
                return cur[key]
            stack.extend(cur.values())
        elif isinstance(cur, list):
            stack.extend(cur)
    return None

status, payload = muster('/servers?installation=dexlab')
if status != 200:
    print(f"  muster /servers FAILED {status}: {payload}"); sys.exit(1)
servers = unwrap(payload, 'mcpServers') or []
print(f"  muster servers  {[(s.get('name'), s.get('state')) for s in servers]}")

status, payload = muster('/workflows?installation=dexlab')
wfs = unwrap(payload, 'workflows') or [] if status == 200 else []
print(f"  muster workflows {[w.get('name') for w in wfs if isinstance(w, dict)]}")

status, tools = muster('/core-tools?installation=dexlab')
if status == 200:
    tl = tools.get('tools') or tools.get('coreTools') or []
    print(f"  muster core tools {len(tl)} exposed")
else:
    print(f"  muster /core-tools -> {status}")
PY
}

if [[ $# -gt 0 ]]; then
  USERS=("$@")
else
  USERS=(admin@lab.local dev@lab.local viewer@lab.local)
fi

for email in "${USERS[@]}"; do
  echo "=== $email ==="
  signin "$email" || exit 1
  echo
done
echo "all sign-ins resolved and reached muster"
