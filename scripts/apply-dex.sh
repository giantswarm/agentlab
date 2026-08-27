#!/usr/bin/env bash
# Applies manifests/dex.yaml with the input checksum stamped into the pod
# template, then waits for the rollout. The checksum covers the manifest AND
# the served cert, so editing the config or regenerating certs rolls the pod,
# while an unchanged re-apply is a pure no-op (no throwaway ReplicaSet).
# Shared by up.sh and `make reload` — keep the stamping logic only here.
set -euo pipefail
cd "$(dirname "$0")/.."

SUM=$(cat manifests/dex.yaml certs/tls.crt | shasum -a 256 | cut -d" " -f1)
sed "s/REPLACED_AT_APPLY/$SUM/" manifests/dex.yaml | kubectl apply -f -

echo "==> Waiting for Dex to become ready"
kubectl -n dex rollout status deployment/dex --timeout=120s
