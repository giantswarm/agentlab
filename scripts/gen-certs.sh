#!/usr/bin/env bash
# Generates a self-signed CA and a Dex server certificate.
# The cert must be valid for 127.0.0.1 because that is the issuer host used by
# both the host machine and the kube-apiserver.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p certs

if [[ -f certs/tls.crt && "${FORCE:-0}" != "1" ]]; then
  echo "certs/ already populated (FORCE=1 to regenerate)"
  exit 0
fi

openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout certs/ca.key -out certs/ca.crt \
  -subj "/CN=dex-lab-ca" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null

openssl req -newkey rsa:2048 -nodes \
  -keyout certs/tls.key -out certs/tls.csr \
  -subj "/CN=dex" 2>/dev/null

cat > certs/san.cnf <<'SAN'
subjectAltName = IP:127.0.0.1, DNS:localhost, DNS:dex, DNS:dex.dex.svc, DNS:dex.dex.svc.cluster.local
extendedKeyUsage = serverAuth
SAN

openssl x509 -req -in certs/tls.csr -days 3650 \
  -CA certs/ca.crt -CAkey certs/ca.key -CAcreateserial \
  -extfile certs/san.cnf -out certs/tls.crt 2>/dev/null

rm -f certs/tls.csr certs/san.cnf certs/ca.srl
chmod 644 certs/*.crt certs/*.key

echo "Generated:"
openssl x509 -in certs/tls.crt -noout -subject -ext subjectAltName
