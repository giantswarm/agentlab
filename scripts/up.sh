#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=dexlab

./scripts/gen-certs.sh

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "==> kind cluster '$CLUSTER' already exists"
else
  echo "==> Creating kind cluster '$CLUSTER' (Kubernetes v1.35)"
  kind create cluster --config kind-config.yaml --wait 120s
fi

kubectl config use-context "kind-$CLUSTER" >/dev/null

echo "==> Deploying Dex"
# Namespace and TLS secret land before the Deployment so the pod never waits
# on a missing volume on first boot.
kubectl create namespace dex --dry-run=client -o yaml | kubectl apply -f -
kubectl -n dex create secret generic dex-tls \
  --from-file=tls.crt=certs/tls.crt \
  --from-file=tls.key=certs/tls.key \
  --dry-run=client -o yaml | kubectl apply -f -

./scripts/apply-dex.sh

echo "==> Applying RBAC bound to OIDC groups"
kubectl apply -f manifests/rbac.yaml

echo "==> Waiting for the issuer to answer on https://localhost:32000/dex"
for i in $(seq 1 60); do
  if curl -sf --cacert certs/ca.crt \
       https://localhost:32000/dex/.well-known/openid-configuration >/dev/null; then
    echo "    issuer is up"
    break
  fi
  [[ $i -eq 60 ]] && { echo "    TIMEOUT"; exit 1; }
  sleep 2
done

# No apiserver bounce needed: since (at least) Kubernetes 1.35 the OIDC
# authenticator retries discovery every 10s forever (oidc.go "initializing
# plugin" errors until Dex answers), so the loop below just waits out the
# next retry tick.
echo "==> Verifying the end-to-end OIDC chain"
for i in $(seq 1 30); do
  TOK=$(curl -sS --cacert certs/ca.crt "https://localhost:32000/dex/token" \
        -u kubernetes:kubernetes-lab-secret \
        -d grant_type=password -d username=admin@lab.local \
        -d password=password -d "scope=openid email groups" | jq -r .id_token)
  ./scripts/mk-kubeconfig.sh "$TOK" .kubeconfig.probe >/dev/null
  if kubectl --kubeconfig=.kubeconfig.probe auth whoami >/dev/null 2>&1; then
    echo "    apiserver accepts Dex tokens"
    rm -f .kubeconfig.probe
    break
  fi
  [[ $i -eq 30 ]] && { echo "    apiserver still rejects Dex tokens"; rm -f .kubeconfig.probe; exit 1; }
  sleep 2
done

cat <<'MSG'

Lab is up.

  Users (password for all three: "password")
    admin@lab.local    groups: platform-admins, developers  -> cluster-admin
    dev@lab.local      groups: developers                   -> edit in ns/demo
    viewer@lab.local   groups: viewers                      -> read-only cluster wide

  Try it:
    ./scripts/login.sh admin@lab.local password   # headless, prints the token claims
    ./scripts/login-browser.py                    # real browser login screen
    ./scripts/test.sh                             # full RBAC assertion run
MSG
