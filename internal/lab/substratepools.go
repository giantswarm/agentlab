// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Ported from kagent-dev/substrate v0.0.26: internal/localca/localca.go
// (GenerateCA, Marshal, TLSCertificateChainPEM, TLSPrivateKeyPEM) and
// internal/localjwtauthority/localjwtauthority.go (GenerateECDSAP256Authority,
// Marshal) — the generate + serialise subset behind `kubectl-ate admin
// make-ca-pool` and `make-jwt-pool` (cmd/kubectl-ate/internal/cmd/
// admin_make_ca_pool.go, admin_make_jwt_pool.go). Both packages are
// substrate-internal and cannot be imported. The wire format below is what
// ate-api-server, podcertificate-controller and atenet read off the pool
// Secrets (localca.Unmarshal, localjwtauthority.Unmarshal), so the JSON field
// names are theirs and must not change; a Substrate bump that changes the
// format shows up as ate-api-server never becoming Ready (substrate.go).

package lab

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// caPoolWire is a CA pool as its Secret's `pool` key holds it: the CAs and
// which of them signs. A CA is one root certificate with its signing key.
type caPoolWire struct {
	CAs              []*caWire
	ActiveForSigning string
}

type caWire struct {
	ID                 string
	SigningKeyPKCS8    []byte
	RootCertificateDER []byte
}

// jwtPoolWire is a JWT authority pool as its Secret's `pool` key holds it.
type jwtPoolWire struct {
	Authorities      []*jwtAuthorityWire
	ActiveForSigning string
}

type jwtAuthorityWire struct {
	ID              string
	Algorithm       string
	SigningKeyPKCS8 []byte
}

// caPoolValidity is the root certificate's lifetime make-ca-pool mints.
const caPoolValidity = 365 * 24 * time.Hour

// The PEM block types of a TLS certificate and a PKCS#8 key.
const (
	pemCertificate = "CERTIFICATE"
	pemPrivateKey  = "PRIVATE KEY"
)

// generateCAPoolSecretData is `kubectl-ate admin make-ca-pool --ca-id <id>`
// as Secret data: a pool of one freshly minted ED25519 CA, active for
// signing, under `pool`, and the same root as a TLS pair (`tls.crt` the
// chain, `tls.key` the PKCS#8 key) — the Secret is of type kubernetes.io/tls.
func generateCAPoolSecretData(caID string) (map[string][]byte, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating the root key: %w", err)
	}
	notBefore := time.Now()
	template := &x509.Certificate{
		// Random subject content: some certificate handling assumes equal
		// parent and template subjects mean a self-signing operation
		// (localca.GenerateCA keeps it random for defense in depth).
		Subject:               pkix.Name{CommonName: rand.Text()},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(caPoolValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("generating the root certificate: %w", err)
	}
	keyPKCS8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("serialising the signing key: %w", err)
	}
	pool, err := json.Marshal(&caPoolWire{
		CAs:              []*caWire{{ID: caID, SigningKeyPKCS8: keyPKCS8, RootCertificateDER: der}},
		ActiveForSigning: caID,
	})
	if err != nil {
		return nil, fmt.Errorf("serialising the pool: %w", err)
	}
	return map[string][]byte{
		"pool":                  pool,
		corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: der}),
		corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: pemPrivateKey, Bytes: keyPKCS8}),
	}, nil
}

// generateJWTPoolSecretData is `kubectl-ate admin make-jwt-pool --key-id
// <id>` as Secret data: a pool of one ECDSA P-256 authority signing ES256,
// active, under `pool` (an Opaque Secret).
func generateJWTPoolSecretData(keyID string) (map[string][]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating the signing key: %w", err)
	}
	keyPKCS8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("serialising the signing key: %w", err)
	}
	pool, err := json.Marshal(&jwtPoolWire{
		Authorities:      []*jwtAuthorityWire{{ID: keyID, Algorithm: "ES256", SigningKeyPKCS8: keyPKCS8}},
		ActiveForSigning: keyID,
	})
	if err != nil {
		return nil, fmt.Errorf("serialising the pool: %w", err)
	}
	return map[string][]byte{"pool": pool}, nil
}

// caPoolRootPEM is the signing CA's root certificate out of a CA pool's
// `pool` JSON, as PEM — what the shell bootstrap produced with jq and
// openssl for the actor-id-ca-certs Secret atenet trusts. The active CA, or
// the first when none is designated (localca's own fallback).
func caPoolRootPEM(pool []byte) ([]byte, error) {
	var wire caPoolWire
	if err := json.Unmarshal(pool, &wire); err != nil {
		return nil, fmt.Errorf("parsing the pool: %w", err)
	}
	if len(wire.CAs) == 0 {
		return nil, fmt.Errorf("the pool has no CAs")
	}
	root := wire.CAs[0]
	for _, ca := range wire.CAs {
		if ca.ID == wire.ActiveForSigning {
			root = ca
		}
	}
	if _, err := x509.ParseCertificate(root.RootCertificateDER); err != nil {
		return nil, fmt.Errorf("parsing the root certificate of CA %q: %w", root.ID, err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: root.RootCertificateDER}), nil
}
