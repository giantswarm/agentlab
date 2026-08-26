#!/usr/bin/env bash
# Drives the full Backstage <-> Dex sign-in headlessly and reports the identity
# Backstage resolved. Usage: test-backstage.sh [user@lab.local ...]
set -uo pipefail
cd "$(dirname "$0")/.."

signin() {
  local email="$1" jar body
  jar=$(mktemp)
  local curl_=(curl -sS --cacert certs/ca.crt -c "$jar" -b "$jar")

  # 1. Backstage redirects to Dex and sets its own session cookie.
  local dex_url
  dex_url=$("${curl_[@]}" -o /dev/null -D - \
            "http://localhost:7007/api/auth/oidc/start?env=production" \
            | awk 'tolower($1)=="location:"{print $2}' | tr -d '\r')
  [[ -z "$dex_url" ]] && { echo "  no redirect to Dex"; rm -f "$jar"; return 1; }

  # 2. Follow to Dex's login form, 3. submit lab credentials.
  local login_url
  login_url=$("${curl_[@]}" -L -o /dev/null -w '%{url_effective}' "$dex_url")
  body=$("${curl_[@]}" -L --data-urlencode "login=$email" \
         --data-urlencode "password=password" "$login_url")
  rm -f "$jar"

  python3 - "$body" <<'PY'
import re, json, base64, sys, urllib.parse
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

dex = claims(r['providerInfo']['idToken'])
ident = r['backstageIdentity']['identity']
print(f"  dex asserted    groups={dex.get('groups')} email={dex.get('email')}")
print(f"  backstage user  {ident['userEntityRef']}")
print(f"  ownership refs  {ident['ownershipEntityRefs']}")
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
echo "all sign-ins resolved"
