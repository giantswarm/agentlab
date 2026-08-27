#!/usr/bin/env bash
# Installs the Giant Swarm agent platform (muster + mcp-kubernetes) into the
# dex lab cluster and wires it to the lab Dex.
#
# The platform ships as agent-platform-standalone: one plain Helm umbrella
# chart with muster, valkey and the MCP registrations as pinned subcharts
# (Chart.lock is the BOM). No GitOps controller involved — unlike the old
# agent-platform meta-package, which rendered Flux HelmReleases and needed a
# helm-controller on the cluster before `helm install` did anything useful.
#
# The chart has no release yet (giantswarm/agent-platform-standalone#11), so it
# is vendored from git at a pinned SHA and installed from the local path.
# Once released: swap the vendor step for the OCI ref.
#
# Order:
#   1. Chart    — vendor at $APS_REF + `helm dependency build` (OCI pulls).
#   2. Secrets  — muster's OAuth credentials and the lab CA.
#   3. mcp-kubernetes — the MCP server muster will aggregate.
#   4. The umbrella chart, through scripts/muster-post-render.sh (hostNetwork,
#      the DCR chart-bug workaround, HTTPRoute strip — see that script).
set -euo pipefail
cd "$(dirname "$0")/.."

NS=agent-platform
MCP_K8S_VERSION=${MCP_K8S_VERSION:-1.0.9}
APS_REPO=${APS_REPO:-https://github.com/giantswarm/agent-platform-standalone}
APS_REF=${APS_REF:-672e08cb67cb210cdcc3bb9c5d11f78a42e92003} # PR #11 head (feat/curate-generator)
APS_DIR=vendor/agent-platform-standalone
CHART_DIR=$APS_DIR/helm/agent-platform-standalone

# A cluster still running the old meta-package has Flux HelmReleases under the
# same Helm release name; upgrading across that boundary races helm-controller
# uninstalls against this install. Start clean instead.
if kubectl -n "$NS" get helmrelease muster >/dev/null 2>&1; then
  echo "ERROR: this cluster runs the old agent-platform meta-package (Flux HelmReleases found)."
  echo "       Run 'make platform-down' first, then re-run 'make platform'."
  exit 1
fi

echo "==> Vendoring agent-platform-standalone @ ${APS_REF:0:12}"
if [[ ! -d "$APS_DIR/.git" ]]; then
  git init -q "$APS_DIR"
  git -C "$APS_DIR" remote add origin "$APS_REPO"
fi
if ! git -C "$APS_DIR" cat-file -e "$APS_REF^{commit}" 2>/dev/null; then
  git -C "$APS_DIR" fetch -q --depth 1 origin "$APS_REF"
fi
git -C "$APS_DIR" -c advice.detachedHead=false checkout -q "$APS_REF"

# Subchart .tgz pulls from gsoci/ghcr (anonymous). Skipped when charts/ is
# already in sync with the checked-out Chart.lock.
if [[ ! -d "$CHART_DIR/charts" || "$CHART_DIR/Chart.lock" -nt "$CHART_DIR/charts" ]]; then
  echo "==> Building chart dependencies"
  helm dependency build "$CHART_DIR" >/dev/null
  touch "$CHART_DIR/charts"
fi

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

echo "==> Deploying mcp-kubernetes ($MCP_K8S_VERSION)"
helm upgrade --install mcp-kubernetes \
  "oci://gsoci.azurecr.io/charts/giantswarm/mcp-kubernetes" \
  --version "$MCP_K8S_VERSION" -n "$NS" \
  -f manifests/agent-platform/mcp-kubernetes-values.yaml \
  --wait --timeout 5m >/dev/null

echo "==> Installing agent-platform-standalone (this waits for every workload)"
# --wait replaces the old HelmRelease polling: Helm itself owns the workloads
# now. The MCPServer/Workflow CRDs ship in the muster subchart's crds/ dir,
# which Helm applies before the manifests on first install.
helm upgrade --install agent-platform "$CHART_DIR" -n "$NS" \
  -f manifests/agent-platform/values.yaml \
  --post-renderer ./scripts/muster-post-render.sh \
  --wait --timeout 10m >/dev/null

echo "==> Waiting for muster to connect to the Kubernetes MCP"
STATE=""
for _ in $(seq 1 40); do
  STATE=$(kubectl -n "$NS" get mcpserver dexlab-mcp-kubernetes -o jsonpath='{.status.state}' 2>/dev/null || true)
  if [[ "$STATE" == "Connected" ]]; then
    echo "    MCPServer dexlab-mcp-kubernetes: Connected"
    break
  fi
  sleep 3
done
if [[ "$STATE" != "Connected" ]]; then
  echo "ERROR: MCPServer dexlab-mcp-kubernetes never reached Connected (last state: '${STATE:-<none>}')." >&2
  echo "       Check 'make platform-logs' and 'kubectl -n $NS describe mcpserver dexlab-mcp-kubernetes'." >&2
  exit 1
fi

# The Workflow CRD ships with muster, so this has to land after the install.
# A muster with no workflows leaves the Backstage muster plugin's main tab
# empty, which reads as "the plugin is broken" rather than "nothing to show".
echo "==> Creating the demo workflow"
kubectl apply -f manifests/agent-platform/demo-workflow.yaml >/dev/null

# muster runs with hostNetwork and kind-config.yaml maps :8090 onto the Mac, so
# it should be reachable directly. A cluster created before that mapping was
# added has no such publish, and the only way to add one is to recreate it.
#
# Retry rather than probing once: helm --wait covers the Deployment, but
# muster's HTTP listener accepts slightly later, so a single-shot check on a
# cold cluster fails and then blames the port mapping, which is the one thing
# that is definitely fine on a freshly created cluster.
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
