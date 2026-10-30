package lab

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"path/filepath"
	"testing"
	"time"
)

// TestMintLeafValidityWithinAppleCap pins the leaf's validity to at most
// 825 days in a zone with a DST change inside that span: AddDate on the
// local clock, plus the one-hour backdate, came out at 825 days and two
// hours, which macOS rejects as "not standards compliant".
func TestMintLeafValidityWithinAppleCap(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	orig := time.Local
	time.Local = berlin
	t.Cleanup(func() { time.Local = orig })

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	if err := mintLeaf(ca, key, "leaf", []string{localhostName}, nil, certPath, filepath.Join(dir, "tls.key")); err != nil {
		t.Fatal(err)
	}
	leaf := readCertFile(certPath)
	if leaf == nil {
		t.Fatal("minted leaf does not parse")
	}
	if got, max := leaf.NotAfter.Sub(leaf.NotBefore), leafValidityDays*24*time.Hour; got > max {
		t.Errorf("leaf validity %v exceeds Apple's cap %v", got, max)
	}
	if reason := leafRemintReason(leaf, ca, []string{localhostName}); reason != "" {
		t.Errorf("fresh leaf flagged for re-mint: %q", reason)
	}
}

// TestLeafRemintReasonOverCap re-mints a leaf even a couple of hours over the
// cap, the shape older labs minted across a DST change.
func TestLeafRemintReasonOverCap(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "self"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(-time.Hour).Add(leafValidityDays*24*time.Hour + 2*time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		DNSNames:              []string{localhostName},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if got := leafRemintReason(cert, cert, []string{localhostName}); got != "validity exceeded Apple's 825-day cap" {
		t.Errorf("leafRemintReason = %q, want the cap reason", got)
	}
}
