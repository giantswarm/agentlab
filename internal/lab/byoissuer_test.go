package lab

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	"github.com/giantswarm/agentlab/internal/config"
)

// byoDomain is the test domain an externally provisioned pair is minted for.
const byoDomain = "lab.example.test"

// byoConfig is the default lab with an externally provisioned wildcard pair
// for byoDomain (self-signed, the shape a private BYO pair has), in a
// working directory that holds a minted lab CA and the issuer files.
func byoConfig(t *testing.T, dns ...string) *config.Config {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := GenCerts(byoDomain, false); err != nil {
		t.Fatal(err)
	}
	if dns == nil {
		dns = []string{"*." + byoDomain, byoDomain}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dns[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dns,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(t.TempDir(), "fullchain.pem"), filepath.Join(t.TempDir(), "privkey.pem")
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeKey, Bytes: keyDER})
	// The key in the cert file too (a combined PEM): the bundle must carry
	// its certificates only.
	if err := os.WriteFile(certFile, append(pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: der}), keyPEM...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Platform.Domain = byoDomain
	cfg.Platform.TLS = config.PlatformTLS{CertFile: certFile, KeyFile: keyFile}
	return cfg
}

// The issuer moves under the platform domain exactly when a pair is
// configured, and stays a name, never an IP literal.
func TestIssuerUnderTheDomainWithAnExternalPair(t *testing.T) {
	cfg := config.Default()
	if got, want := cfg.Issuer(), "https://localhost:32000/dex"; got != want {
		t.Errorf("default issuer = %s, want %s", got, want)
	}
	cfg.Platform.Domain = byoDomain
	cfg.Platform.TLS = config.PlatformTLS{CertFile: "c", KeyFile: "k"}
	if got, want := cfg.Issuer(), "https://dex."+byoDomain+":32000/dex"; got != want {
		t.Errorf("BYO issuer = %s, want %s", got, want)
	}
}

// writeIssuerFiles: the bundle is the lab CA plus the pair's certificates
// (and nothing else of the file), it verifies the pair for the issuer's
// host, and the apiserver's hosts file puts that host on loopback. Without
// a pair it writes nothing and the lab CA stays the trust file.
func TestWriteIssuerFiles(t *testing.T) {
	cfg := byoConfig(t)
	if err := writeIssuerFiles(cfg); err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(trustBundleFile(cfg))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(caCertPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(bundle, ca) || bytes.Contains(bundle, []byte("PRIVATE KEY")) {
		t.Errorf("bundle is not the lab CA followed by certificates only:\n%s", bundle)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(bundle)
	leaf := readCertFile(cfg.Platform.TLS.CertFile)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: cfg.DexHost()}); err != nil {
		t.Errorf("the bundle does not verify the pair for %s: %v", cfg.DexHost(), err)
	}
	hosts, err := os.ReadFile(apiserverHostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hosts), "127.0.0.1 localhost dex."+byoDomain+"\n") {
		t.Errorf("apiserver hosts = %q, want dex.%s on loopback", hosts, byoDomain)
	}

	plain := config.Default()
	t.Chdir(t.TempDir())
	if err := writeIssuerFiles(plain); err != nil {
		t.Fatal(err)
	}
	if trustBundleFile(plain) != caCertPath {
		t.Errorf("default trust file = %s, want %s", trustBundleFile(plain), caCertPath)
	}
	if _, err := os.Stat(trustBundlePath); !os.IsNotExist(err) {
		t.Errorf("a bundle written without a pair: %v", err)
	}
}

// A pair that does not cover dex.<domain> is refused at validation, naming
// the host and what the certificate covers.
func TestValidateRefusesAPairWithoutTheIssuerHost(t *testing.T) {
	cfg := byoConfig(t, "muster."+byoDomain)
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "dex."+byoDomain) || !strings.Contains(err.Error(), "muster."+byoDomain) {
		t.Errorf("Validate() = %v, want a refusal naming dex.%s and the SAN", err, byoDomain)
	}
	if err := byoConfig(t).Validate(); err != nil {
		t.Errorf("a wildcard pair: %v", err)
	}
}

// The kind config with a pair: the apiserver verifies the issuer against the
// bundle and resolves it through its own hosts file, mounted over
// /etc/hosts; without one, neither.
func TestKindConfigBYOIssuer(t *testing.T) {
	apiServer := func(cfg *config.Config) string {
		out, err := renderTemplate(cfg, "kind-config.yaml.tmpl", nil)
		if err != nil {
			t.Fatal(err)
		}
		var kindCfg v1alpha4.Cluster
		if err := yaml.Unmarshal(out, &kindCfg); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		for _, patch := range kindCfg.KubeadmConfigPatches {
			if strings.Contains(patch, "kind: ClusterConfiguration") {
				return patch
			}
		}
		t.Fatal("no ClusterConfiguration patch rendered")
		return ""
	}
	cfg := byoConfig(t)
	if err := writeIssuerFiles(cfg); err != nil {
		t.Fatal(err)
	}
	byo := apiServer(cfg)
	for _, want := range []string{
		"value: https://dex." + byoDomain + ":32000/dex",
		"value: /etc/kubernetes/pki/dex/trust-bundle.crt",
		"hostPath: /etc/kubernetes/pki/dex/apiserver-hosts\n      mountPath: /etc/hosts",
	} {
		if !strings.Contains(byo, want) {
			t.Errorf("BYO ClusterConfiguration lacks %q:\n%s", want, byo)
		}
	}
	plain := apiServer(config.Default())
	if !strings.Contains(plain, "value: /etc/kubernetes/pki/dex/ca.crt") || strings.Contains(plain, "extraVolumes") {
		t.Errorf("default ClusterConfiguration changed:\n%s", plain)
	}
}

// In-cluster resolution with a pair: CoreDNS answers dex.<domain> with the
// Dex issuer Service before the wildcard sends the domain to the edge, the
// Service serves the issuer's port, the JWKS routes dial the issuer's host,
// and no sidecar is patched — mcp-prometheus's included.
func TestBYOIssuerInCluster(t *testing.T) {
	cfg := byoConfig(t)
	if err := writeIssuerFiles(cfg); err != nil {
		t.Fatal(err)
	}
	coredns, err := renderTemplate(cfg, "coredns.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	rule := `name regex ^dex\.lab\.example\.test\. ` + dexIssuerService + ".dex.svc.cluster.local"
	exact, wildcard := strings.Index(string(coredns), rule), strings.Index(string(coredns), "agentgateway-edge.agent-platform.svc.cluster.local")
	if exact < 0 || exact > wildcard {
		t.Errorf("CoreDNS does not send dex.%s to the issuer Service ahead of the edge wildcard:\n%s", byoDomain, coredns)
	}
	dex, err := renderTemplate(cfg, "dex.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dex), "issuer: https://dex."+byoDomain+":32000/dex") ||
		!strings.Contains(string(dex), "name: "+dexIssuerService+"\n  namespace: dex\nspec:\n  selector: { app: dex }\n  ports:\n    - name: https\n      port: 32000\n      targetPort: 5556") {
		t.Errorf("Dex render lacks the BYO issuer or its Service:\n%s", dex)
	}
	values, err := renderTemplate(cfg, platformValuesTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(values), "host: dex."+byoDomain+"\n        port: 32000\n        path: /dex/keys") {
		t.Error("the controller route's JWKS source is not the issuer's host and port")
	}
	if strings.Contains(string(values), dexServiceHost) {
		t.Errorf("a JWKS source still on %s, a name the pair does not cover", dexServiceHost)
	}
	mcpPrometheus, err := renderTemplate(cfg, mcpPrometheusTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mcpPrometheus), "name: "+dexLocalhostContainer) {
		t.Errorf("mcp-prometheus carries the %s sidecar under the BYO issuer:\n%s", dexLocalhostContainer, mcpPrometheus)
	}
	targets, err := dexLocalhostTargets(cfg, map[string]string{componentMCPKubernetes: "kind: Deployment\nmetadata: {name: x}\nspec:\n  template:\n    spec:\n      containers:\n        - name: x\n          env:\n            - {name: DEX_ISSUER_URL, value: \"" + cfg.Issuer() + "\"}\n"})
	if err != nil || len(targets) != 0 {
		t.Errorf("sidecar targets under the BYO issuer = %v, %v; want none", targets, err)
	}
}

// The apiserver's issuer is read off its static pod manifest, the check an
// existing cluster passes before a changed issuer would strand its tokens.
func TestAPIServerIssuer(t *testing.T) {
	manifest := "spec:\n  containers:\n  - command:\n    - kube-apiserver\n    - --oidc-ca-file=/etc/kubernetes/pki/dex/ca.crt\n    - --oidc-issuer-url=https://localhost:32000/dex\n"
	if got, ok := apiserverIssuer(manifest); !ok || got != "https://localhost:32000/dex" {
		t.Errorf("apiserverIssuer = %q, %v", got, ok)
	}
	if _, ok := apiserverIssuer("spec: {}\n"); ok {
		t.Error("an issuer read off a manifest without the flag")
	}
}
