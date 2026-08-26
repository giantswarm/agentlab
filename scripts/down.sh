#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
kind delete cluster --name dexlab
rm -f .token
echo "Lab destroyed. certs/ kept (delete manually or FORCE=1 ./scripts/gen-certs.sh)."
