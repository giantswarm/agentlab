package lab

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// secondEdgeAddress is a second lab's loopback address.
const secondEdgeAddress = "127.0.0.2"

// The lab's own clients dial its hostnames on the address the edge is
// published on; anything else is dialed as asked.
func TestDialLabAddr(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Domain = "lab2.127.0.0.2.nip.io"
	cfg.Platform.GatewayAddress = secondEdgeAddress
	SetPlatform(cfg)
	t.Cleanup(func() { SetPlatform(&config.Config{}) })
	for in, want := range map[string]string{
		"muster.lab2.127.0.0.2.nip.io:443": secondEdgeAddress + ":443",
		"lab2.127.0.0.2.nip.io:443":        secondEdgeAddress + ":443",
		"muster.127.0.0.1.nip.io:443":      "muster.127.0.0.1.nip.io:443",
		"localhost:32003":                  "localhost:32003",
		"localhost:32000":                  "127.0.0.1:32000",
		"[::1]:32000":                      "[::1]:32000",
		"localhost.example.org:32000":      "localhost.example.org:32000",
		"malformed":                        "malformed",
	} {
		if got := dialLabAddr(in); got != want {
			t.Errorf("dialLabAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

// The dial rewrite must keep hostname verification and the HTTP Host intact.
// A certificate for localhost alone deliberately cannot verify 127.0.0.1.
func TestLabDexTLSOnIPv4(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		DNSNames:     []string{localhostName},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	var wantHost string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.ServerName != localhostName || r.Host != wantHost {
			t.Errorf("lost localhost identity: SNI %q, Host %q", r.TLS.ServerName, r.Host)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	wantHost = net.JoinHostPort(localhostName, strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	defer server.Close()
	cfg := config.Default()
	cfg.DexPort = server.Listener.Addr().(*net.TCPAddr).Port
	SetPlatform(cfg)
	t.Cleanup(func() { SetPlatform(&config.Config{}) })
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	tr := newLabTransport(pool)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get(cfg.Issuer())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("Dex status = %d", resp.StatusCode)
	}
}

func TestDialLabDexPort(t *testing.T) {
	cfg := config.Default()
	cfg.DexPort = 32003
	SetPlatform(cfg)
	t.Cleanup(func() { SetPlatform(&config.Config{}) })
	if got := dialLabAddr("localhost:32003"); got != "127.0.0.1:32003" {
		t.Errorf("custom Dex port dialed as %q", got)
	}
	if got := dialLabAddr("localhost:32000"); got != "localhost:32000" {
		t.Errorf("another localhost port rewritten to %q", got)
	}
	SetPlatform(&config.Config{})
	if got := dialLabAddr("localhost:32003"); got != "localhost:32003" {
		t.Errorf("outside a lab, localhost rewritten to %q", got)
	}
}
