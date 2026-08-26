#!/usr/bin/env bash
# Deploys Backstage (Red Hat Developer Hub community build) into the lab and
# points it at Dex as its only identity provider.
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE=quay.io/rhdh-community/rhdh:next-1.9

if ! kind get clusters 2>/dev/null | grep -qx dexlab; then
  echo "the dexlab cluster is not running -- run 'make up' first"; exit 1
fi

if ! docker exec dexlab-control-plane crictl images 2>/dev/null | grep -q rhdh; then
  echo "==> Loading $IMAGE into the kind node (4GB, takes a few minutes)"
  docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE"
  kind load docker-image "$IMAGE" --name dexlab
fi

echo "==> Deploying Backstage"
kubectl apply -f manifests/backstage.yaml

# Backstage talks to Dex over TLS signed by the lab CA.
kubectl -n backstage create secret generic dex-ca \
  --from-file=ca.crt=certs/ca.crt \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n backstage rollout restart deployment/backstage >/dev/null 2>&1 || true
echo "==> Waiting for Backstage to start (first boot takes a minute)"
kubectl -n backstage rollout status deployment/backstage --timeout=300s

echo -n "==> Waiting for http://localhost:7007 "
for i in $(seq 1 90); do
  if curl -sf -o /dev/null http://localhost:7007; then echo " up"; break; fi
  [[ $i -eq 90 ]] && { echo " TIMEOUT"; exit 1; }
  echo -n "."; sleep 2
done

cat <<'MSG'

Backstage is up: http://localhost:7007

  Click "Sign In" -> you land on the Dex login page.
  Log in as admin@lab.local / dev@lab.local / viewer@lab.local (password: password).

  Logs:  kubectl -n backstage logs -f deploy/backstage
MSG
