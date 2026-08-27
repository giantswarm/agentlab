#!/usr/bin/env bash
# The apiserver runs OIDC discovery against the issuer at startup and retries
# only ~4 times over 40s. On a cold `kind create`, Dex does not exist yet, so
# the authenticator gives up with "oidc: authenticator not initialized" and
# never recovers. Bouncing the static pod once Dex is serving fixes it for good.
set -euo pipefail
NODE=dexlab-control-plane

echo "==> Restarting kube-apiserver so it re-runs OIDC discovery"
# Move the static-pod manifest away and wait until the kubelet has actually
# torn the container down before restoring it. A blind sleep could restore the
# file before the kubelet noticed the removal — a silent non-restart. The trap
# guarantees the manifest comes back even if the wait fails, so the cluster is
# never left without an apiserver.
docker exec "$NODE" sh -c '
  set -e
  mv /etc/kubernetes/manifests/kube-apiserver.yaml /tmp/kube-apiserver.yaml.hold
  trap "mv /tmp/kube-apiserver.yaml.hold /etc/kubernetes/manifests/" EXIT
  i=0
  while [ -n "$(crictl ps --name kube-apiserver -q 2>/dev/null)" ]; do
    i=$((i+1))
    if [ "$i" -gt 60 ]; then
      echo "kube-apiserver container did not stop within 60s" >&2
      exit 1
    fi
    sleep 1
  done
'

echo -n "    waiting for the apiserver to come back "
for _ in $(seq 1 90); do
  if kubectl --context=kind-dexlab get --raw /healthz >/dev/null 2>&1; then
    echo " ok"; exit 0
  fi
  echo -n "."; sleep 2
done
echo " TIMEOUT"; exit 1
