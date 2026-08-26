#!/usr/bin/env bash
# The apiserver runs OIDC discovery against the issuer at startup and retries
# only ~4 times over 40s. On a cold `kind create`, Dex does not exist yet, so
# the authenticator gives up with "oidc: authenticator not initialized" and
# never recovers. Bouncing the static pod once Dex is serving fixes it for good.
set -euo pipefail
NODE=dexlab-control-plane

echo "==> Restarting kube-apiserver so it re-runs OIDC discovery"
docker exec "$NODE" sh -c '
  mkdir -p /tmp/holdmanifest
  mv /etc/kubernetes/manifests/kube-apiserver.yaml /tmp/holdmanifest/
  sleep 5
  mv /tmp/holdmanifest/kube-apiserver.yaml /etc/kubernetes/manifests/
'

echo -n "    waiting for the apiserver to come back "
for _ in $(seq 1 90); do
  if kubectl --context=kind-dexlab get --raw /healthz >/dev/null 2>&1; then
    echo " ok"; exit 0
  fi
  echo -n "."; sleep 2
done
echo " TIMEOUT"; exit 1
