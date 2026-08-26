#!/usr/bin/env bash
# Prints the payload of a JWT as JSON. Reads from $1 or stdin.
set -euo pipefail
TOKEN="${1:-$(cat)}"
python3 -c '
import base64, sys
seg = sys.argv[1].split(".")[1]
print(base64.urlsafe_b64decode(seg + "=" * (-len(seg) % 4)).decode())
' "$TOKEN"
