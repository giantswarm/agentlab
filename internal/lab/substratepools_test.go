package lab

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// A CA pool Secret round-trips through the shape substrate reads: the `pool`
// JSON names the CA under CAs[0] with its root DER and PKCS#8 key and marks
// it ActiveForSigning; tls.crt is that root as PEM, tls.key its key — the
// three keys kubectl-ate admin make-ca-pool writes.
func TestGenerateCAPoolSecretData(t *testing.T) {
	data, err := generateCAPoolSecretData("1")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pool", corev1.TLSCertKey, corev1.TLSPrivateKeyKey} {
		if len(data[key]) == 0 {
			t.Errorf("Secret key %s missing", key)
		}
	}
	// Read the JSON the way a jq consumer (and localca.Unmarshal) does: the
	// field names are substrate's, the byte slices base64 strings.
	var generic struct {
		CAs []struct {
			ID                 string
			SigningKeyPKCS8    string
			RootCertificateDER string
		}
		ActiveForSigning string
	}
	if err := json.Unmarshal(data["pool"], &generic); err != nil {
		t.Fatalf("pool JSON: %v\n%s", err, data["pool"])
	}
	if generic.ActiveForSigning != "1" || len(generic.CAs) != 1 || generic.CAs[0].ID != "1" {
		t.Fatalf("pool = %+v, want one CA \"1\" active for signing", generic)
	}
	der, err := base64.StdEncoding.DecodeString(generic.CAs[0].RootCertificateDER)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("RootCertificateDER: %v", err)
	}
	if !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("root is not a signing CA: IsCA=%v KeyUsage=%v", cert.IsCA, cert.KeyUsage)
	}
	if cert.NotAfter.Sub(cert.NotBefore) != caPoolValidity {
		t.Errorf("validity %s, want %s", cert.NotAfter.Sub(cert.NotBefore), caPoolValidity)
	}
	keyDER, err := base64.StdEncoding.DecodeString(generic.CAs[0].SigningKeyPKCS8)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		t.Fatalf("SigningKeyPKCS8: %v", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("signing key is %T, want ed25519", key)
	}
	if !priv.Public().(ed25519.PublicKey).Equal(cert.PublicKey) {
		t.Error("the root certificate's public key is not the signing key's")
	}
	// The TLS pair is the same root and key, PEM-encoded.
	block, _ := pem.Decode(data[corev1.TLSCertKey])
	if block == nil || block.Type != pemCertificate || !bytes.Equal(block.Bytes, der) {
		t.Error("tls.crt is not the root certificate as PEM")
	}
	block, _ = pem.Decode(data[corev1.TLSPrivateKeyKey])
	if block == nil || block.Type != pemPrivateKey || !bytes.Equal(block.Bytes, keyDER) {
		t.Error("tls.key is not the signing key as PKCS#8 PEM")
	}
	// The wire struct itself decodes what it encoded.
	var wire caPoolWire
	if err := json.Unmarshal(data["pool"], &wire); err != nil || len(wire.CAs) != 1 || !bytes.Equal(wire.CAs[0].RootCertificateDER, der) {
		t.Errorf("caPoolWire round trip: %v %+v", err, wire)
	}
	// actor-id-ca-certs is the root PEM out of the pool.
	root, err := caPoolRootPEM(data["pool"])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(root, data[corev1.TLSCertKey]) {
		t.Error("caPoolRootPEM must be the tls.crt of a one-CA pool")
	}
	if _, err := caPoolRootPEM([]byte(`{"CAs":[],"ActiveForSigning":""}`)); err == nil {
		t.Error("an empty pool must be refused")
	}
	// Two pools never share a root.
	other, err := generateCAPoolSecretData("1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(other[corev1.TLSCertKey], data[corev1.TLSCertKey]) {
		t.Error("two generated pools carry the same root")
	}
}

// A JWT pool Secret holds one ES256 authority under Authorities[0], active
// for signing — what kubectl-ate admin make-jwt-pool writes and ate-api-server
// signs actor identity tokens with.
func TestGenerateJWTPoolSecretData(t *testing.T) {
	data, err := generateJWTPoolSecretData("1")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 1 || len(data["pool"]) == 0 {
		t.Fatalf("Secret data keys %v, want exactly pool", data)
	}
	var generic struct {
		Authorities []struct {
			ID              string
			Algorithm       string
			SigningKeyPKCS8 string
		}
		ActiveForSigning string
	}
	if err := json.Unmarshal(data["pool"], &generic); err != nil {
		t.Fatalf("pool JSON: %v\n%s", err, data["pool"])
	}
	if generic.ActiveForSigning != "1" || len(generic.Authorities) != 1 || generic.Authorities[0].ID != "1" || generic.Authorities[0].Algorithm != "ES256" {
		t.Fatalf("pool = %+v, want one ES256 authority \"1\" active for signing", generic)
	}
	keyDER, err := base64.StdEncoding.DecodeString(generic.Authorities[0].SigningKeyPKCS8)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		t.Fatalf("SigningKeyPKCS8: %v", err)
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		t.Errorf("signing key is %T on %v, want ECDSA P-256 (ES256)", key, ec)
	}
	var wire jwtPoolWire
	if err := json.Unmarshal(data["pool"], &wire); err != nil || len(wire.Authorities) != 1 || !bytes.Equal(wire.Authorities[0].SigningKeyPKCS8, keyDER) {
		t.Errorf("jwtPoolWire round trip: %v %+v", err, wire)
	}
}
