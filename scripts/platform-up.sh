#!/usr/bin/env bash
# Installs the Giant Swarm agent platform (muster + mcp-kubernetes) into the
# dex lab cluster and wires it to the lab Dex.
#
# Order matters:
#   1. Flux   — the agent-platform chart is a meta-package: it renders Flux
#               OCIRepository/HelmRelease objects, so a controller must exist
#               before `helm install` of it does anything useful.
#   2. Gateway API CRDs — documented prerequisite of the chart.
#   3. Secrets — muster's OAuth credentials and the lab CA.
#   4. mcp-kubernetes — the MCP server muster will aggregate.
#   5. agent-platform — the meta-package itself.
set -euo pipefail
cd "$(dirname "$0")/.."

NS=agent-platform
FLUX_VERSION=${FLUX_VERSION:-v2.9.4}
GATEWAY_API_VERSION=${GATEWAY_API_VERSION:-v1.2.1}
CHART_VERSION=${CHART_VERSION:-2.12.0}

echo "==> Installing Flux $FLUX_VERSION"
# Pinned deliberately: the meta-package renders helm.toolkit.fluxcd.io/v2 and
# source.toolkit.fluxcd.io/v1. Flux < 2.3 only serves v2beta2 / v1beta2 and the
# HelmReleases will not even be accepted by the API server.
kubectl apply --server-side --force-conflicts \
  -f "https://github.com/fluxcd/flux2/releases/download/$FLUX_VERSION/install.yaml" >/dev/null
kubectl -n flux-system rollout status deploy/source-controller --timeout=180s
kubectl -n flux-system rollout status deploy/helm-controller --timeout=180s

echo "==> Installing Gateway API $GATEWAY_API_VERSION CRDs"
kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/$GATEWAY_API_VERSION/standard-install.yaml" >/dev/null

echo "==> Creating namespace and secrets"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# muster appends this to its system trust pool so it can talk to the lab's
# self-signed Dex over TLS (values: muster.muster.extraCaFile).
kubectl -n "$NS" create secret generic dex-ca \
  --from-file=ca.crt=certs/ca.crt --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# Created once and then left alone: regenerating the encryption key on every run
# would invalidate every issued token. dex-client-secret must match the `muster`
# staticClient in manifests/dex.yaml.
if ! kubectl -n "$NS" get secret agent-platform-secrets >/dev/null 2>&1; then
  kubectl -n "$NS" create secret generic agent-platform-secrets \
    --from-literal=dex-client-secret=muster-lab-secret \
    --from-literal=registration-token="$(openssl rand -hex 32)" \
    --from-literal=oauth-encryption-key="$(openssl rand -base64 32)" \
    --from-literal=valkey-password="$(openssl rand -hex 16)" >/dev/null
  echo "    created agent-platform-secrets"
else
  echo "    agent-platform-secrets already exists, leaving it alone"
fi

echo "==> Deploying mcp-kubernetes"
kubectl apply -f manifests/agent-platform/mcp-kubernetes.yaml >/dev/null

echo "==> Installing the agent-platform meta-package ($CHART_VERSION)"
helm upgrade --install agent-platform \
  "oci://gsoci.azurecr.io/charts/giantswarm/agent-platform" \
  --version "$CHART_VERSION" -n "$NS" \
  -f manifests/agent-platform/values.yaml >/dev/null

echo "==> Waiting for every HelmRelease to become Ready"
for hr in mcp-kubernetes valkey muster agent-platform-mcps; do
  printf "    %-24s" "$hr"
  for _ in $(seq 1 60); do
    if [[ "$(kubectl -n "$NS" get hr "$hr" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == "True" ]]; then
      echo "Ready"; break
    fi
    printf "."; sleep 5
  done
done

echo "==> Waiting for muster to connect to the Kubernetes MCP"
for _ in $(seq 1 40); do
  if [[ "$(kubectl -n "$NS" get mcpserver dexlab-mcp-kubernetes -o jsonpath='{.status.state}' 2>/dev/null)" == "Connected" ]]; then
    echo "    MCPServer dexlab-mcp-kubernetes: Connected"; break
  fi
  sleep 3
done

# The Workflow CRD ships with muster, so this has to land after the HelmRelease
# is Ready. A muster with no workflows leaves the Backstage muster plugin's main
# tab empty, which reads as "the plugin is broken" rather than "nothing to show".
echo "==> Creating the demo workflow"
kubectl apply -f manifests/agent-platform/demo-workflow.yaml >/dev/null

# muster runs with hostNetwork and kind-config.yaml maps :8090 onto the Mac, so
# it should be reachable directly. A cluster created before that mapping was
# added has no such publish, and the only way to add one is to recreate it.
#
# Retry rather than probing once: the HelmRelease going Ready and the MCPServer
# reporting Connected both happen before muster's HTTP listener accepts, so a
# single-shot check on a cold cluster fails and then blames the port mapping,
# which is the one thing that is definitely fine on a freshly created cluster.
echo "==> Waiting for muster to serve on http://localhost:8090"
REACH=""
for _ in $(seq 1 40); do
  if curl -sf -o /dev/null --max-time 3 http://localhost:8090/.well-known/oauth-authorization-server; then
    REACH="muster is live on http://localhost:8090 (no port-forward needed)"
    break
  fi
  sleep 3
done
if [[ -z "$REACH" ]]; then
  REACH="muster is NOT reachable on http://localhost:8090 after 2 minutes.
  If 'docker port dexlab-control-plane' shows no 8090 line, this cluster
  predates the mapping in kind-config.yaml and must be recreated
  (make down && make up && make platform). Otherwise check 'make platform-logs'.
  Stopgap either way: make platform-forward"
fi

cat <<MSG

Platform is up.
  $REACH

  Point Claude Code at it (browser login through Dex):
    claude mcp add --transport http muster http://localhost:8090/mcp
    # then in Claude Code: /mcp -> authenticate

  Smoke-test it headlessly instead:
    ./scripts/platform-test.sh
MSG
