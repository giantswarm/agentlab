#!/usr/bin/env bash
# Deploys Giant Swarm's Backstage into the lab and points it at Dex as its only
# identity provider. This is the GS build (not upstream, not RHDH) because it
# carries the first-party `muster` plugin that drives the agent platform.
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE=gsoci.azurecr.io/giantswarm/backstage:0.192.0

if ! kind get clusters 2>/dev/null | grep -qx dexlab; then
  echo "the dexlab cluster is not running -- run 'make up' first"; exit 1
fi

if ! docker exec dexlab-control-plane crictl images 2>/dev/null | grep -q giantswarm/backstage; then
  echo "==> Loading $IMAGE into the kind node (2.4GB, takes a few minutes)"
  # gsoci serves this anonymously -- no registry credentials needed. The image
  # is linux/amd64 only, so an arm64 Mac must ask for that platform explicitly
  # and the kind node runs it through its Rosetta binfmt handler.
  docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull --platform linux/amd64 "$IMAGE"
  kind load docker-image "$IMAGE" --name dexlab
fi

echo "==> Deploying Backstage"
kubectl apply -f manifests/backstage.yaml

# Backstage talks to Dex over TLS signed by the lab CA.
kubectl -n backstage create secret generic dex-ca \
  --from-file=ca.crt=certs/ca.crt \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n backstage rollout restart deployment/backstage >/dev/null 2>&1 || true
echo "==> Waiting for Backstage to start (first boot takes a minute under emulation)"
kubectl -n backstage rollout status deployment/backstage --timeout=300s

echo -n "==> Waiting for http://localhost:7007 "
for i in $(seq 1 120); do
  if curl -sf -o /dev/null http://localhost:7007; then echo " up"; break; fi
  [[ $i -eq 120 ]] && { echo " TIMEOUT"; exit 1; }
  echo -n "."; sleep 2
done

cat <<'MSG'

Backstage is up: http://localhost:7007

  Click "Sign In" -> you land on the Dex login page.
  Log in as admin@lab.local / dev@lab.local / viewer@lab.local (password: password).

  The muster plugin lives under "Agent Platform" -> "MCP Servers".

  Logs:  kubectl -n backstage logs -f deploy/backstage
MSG
