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

// localhostIssuerConfig is the default lab as a cluster created before
// platform.issuerOnDomain records it: the issuer on localhost, bridged into
// pods by the dex-localhost sidecar.
func localhostIssuerConfig() *config.Config {
	cfg := config.Default()
	cfg.Platform.IssuerOnDomain = false
	return cfg
}

// The issuer lives under the platform domain on a new lab and with a pair,
// on localhost only on a lab whose cluster was created before, and stays a
// name, never an IP literal.
func TestIssuerUnderTheDomain(t *testing.T) {
	cfg := config.Default()
	if got, want := cfg.Issuer(), "https://dex.127.0.0.1.nip.io:32000/dex"; got != want {
		t.Errorf("default issuer = %s, want %s", got, want)
	}
	if got, want := localhostIssuerConfig().Issuer(), "https://localhost:32000/dex"; got != want {
		t.Errorf("localhost issuer = %s, want %s", got, want)
	}
	for _, issuerOnDomain := range []bool{true, false} {
		cfg := config.Default()
		cfg.Platform.IssuerOnDomain = issuerOnDomain
		cfg.Platform.Domain = byoDomain
		cfg.Platform.TLS = config.PlatformTLS{CertFile: "c", KeyFile: "k"}
		if got, want := cfg.Issuer(), "https://dex."+byoDomain+":32000/dex"; got != want {
			t.Errorf("BYO issuer (issuerOnDomain %v) = %s, want %s", issuerOnDomain, got, want)
		}
	}
}

// The Dex leaf carries the issuer's name under the domain next to the fixed
// names, so one leaf serves either spelling, and a leaf minted without it is
// re-minted from the unchanged CA.
func TestDexLeafCarriesTheIssuerUnderTheDomain(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := GenCerts("127.0.0.1.nip.io", false); err != nil {
		t.Fatal(err)
	}
	leaf, ca := readCertFile(tlsCertPath), readCertFile(caCertPath)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	for _, host := range []string{"dex.127.0.0.1.nip.io", localhostName, dexServiceHost} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: host}); err != nil {
			t.Errorf("the Dex leaf does not verify for %s: %v", host, err)
		}
	}
	if got := leafRemintReason(leaf, ca, dexSANNames); got != "" {
		t.Errorf("leafRemintReason(fixed names) = %q", got)
	}
	if got, want := leafRemintReason(leaf, ca, dexLeafNames("lab2.127.0.0.2.nip.io")), "SAN missing dex.lab2.127.0.0.2.nip.io"; got != want {
		t.Errorf("leafRemintReason(another domain) = %q, want %q", got, want)
	}
}

// A new cluster takes the issuer under the domain, recorded in agentlab.yaml;
// an existing cluster keeps the one it was created with, and so does a
// configuration that already records the domain.
func TestMoveIssuerUnderDomain(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := localhostIssuerConfig()
	if err := moveIssuerUnderDomain(cfg, true); err != nil {
		t.Fatal(err)
	}
	if cfg.Platform.IssuerOnDomain {
		t.Fatal("an existing cluster's issuer moved")
	}
	if _, err := os.Stat(config.File); !os.IsNotExist(err) {
		t.Fatalf("agentlab.yaml written for an existing cluster: %v", err)
	}
	if err := moveIssuerUnderDomain(cfg, false); err != nil {
		t.Fatal(err)
	}
	if !cfg.Platform.IssuerOnDomain || cfg.Issuer() != "https://dex.127.0.0.1.nip.io:32000/dex" {
		t.Fatalf("a new cluster's issuer = %s (issuerOnDomain %v)", cfg.Issuer(), cfg.Platform.IssuerOnDomain)
	}
	raw, err := os.ReadFile(config.File)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "issuerOnDomain: true") {
		t.Errorf("agentlab.yaml does not record the issuer under the domain:\n%s", raw)
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

	// The default lab: the issuer under the domain with the lab-CA leaf —
	// the hosts file, no bundle.
	plain := config.Default()
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("certs", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := writeIssuerFiles(plain); err != nil {
		t.Fatal(err)
	}
	if trustBundleFile(plain) != caCertPath {
		t.Errorf("default trust file = %s, want %s", trustBundleFile(plain), caCertPath)
	}
	if _, err := os.Stat(trustBundlePath); !os.IsNotExist(err) {
		t.Errorf("a bundle written without a pair: %v", err)
	}
	hosts, err = os.ReadFile(apiserverHostsPath)
	if err != nil || !strings.Contains(string(hosts), "127.0.0.1 localhost dex.127.0.0.1.nip.io\n") {
		t.Errorf("default apiserver hosts = %q, %v; want dex.127.0.0.1.nip.io on loopback", hosts, err)
	}

	// The localhost issuer: nothing to write.
	t.Chdir(t.TempDir())
	if err := writeIssuerFiles(localhostIssuerConfig()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(apiserverHostsPath); !os.IsNotExist(err) {
		t.Errorf("a hosts file written for the localhost issuer: %v", err)
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
	t.Chdir(t.TempDir())
	plain := apiServer(config.Default())
	for _, want := range []string{
		"value: https://dex.127.0.0.1.nip.io:32000/dex",
		"value: /etc/kubernetes/pki/dex/ca.crt",
		"hostPath: /etc/kubernetes/pki/dex/apiserver-hosts\n      mountPath: /etc/hosts",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("default ClusterConfiguration lacks %q:\n%s", want, plain)
		}
	}
	localhost := apiServer(localhostIssuerConfig())
	if !strings.Contains(localhost, "value: https://localhost:32000/dex") || !strings.Contains(localhost, "value: /etc/kubernetes/pki/dex/ca.crt") || strings.Contains(localhost, "extraVolumes") {
		t.Errorf("localhost-issuer ClusterConfiguration changed:\n%s", localhost)
	}
}

// Dex's port is published where the issuer's host resolves on this machine:
// the edge's address under the domain — a second lab's own loopback address
// — and 127.0.0.1 for localhost.
func TestKindConfigPublishesDexWhereTheIssuerResolves(t *testing.T) {
	t.Chdir(t.TempDir())
	dexListen := func(cfg *config.Config) string {
		out, err := renderTemplate(cfg, "kind-config.yaml.tmpl", nil)
		if err != nil {
			t.Fatal(err)
		}
		var kindCfg v1alpha4.Cluster
		if err := yaml.Unmarshal(out, &kindCfg); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		for _, m := range kindCfg.Nodes[0].ExtraPortMappings {
			if int(m.ContainerPort) == cfg.DexPort {
				return m.ListenAddress
			}
		}
		t.Fatal("no Dex port mapping rendered")
		return ""
	}
	lab2 := config.Default()
	lab2.Platform.Domain, lab2.Platform.GatewayAddress, lab2.DexPort = "lab2.127.0.0.2.nip.io", "127.0.0.2", 32003
	legacy2 := localhostIssuerConfig()
	legacy2.Platform.Domain, legacy2.Platform.GatewayAddress = lab2.Platform.Domain, lab2.Platform.GatewayAddress
	for name, tc := range map[string]struct {
		cfg  *config.Config
		want string
	}{
		"the default lab":         {config.Default(), config.DefaultGatewayAddress},
		"second lab":              {lab2, "127.0.0.2"},
		"second lab on localhost": {legacy2, config.DefaultGatewayAddress},
	} {
		if got := dexListen(tc.cfg); got != tc.want || tc.cfg.DexHostAddress() != tc.want {
			t.Errorf("%s: Dex published on %s (DexHostAddress %s), want %s", name, got, tc.cfg.DexHostAddress(), tc.want)
		}
	}
}

// The default lab in the cluster: CoreDNS answers dex.<domain> with the
// issuer Service ahead of the edge wildcard, the Service serves the issuer's
// port, the JWKS routes keep Dex's in-cluster name (the lab-CA leaf carries
// it), and no sidecar is patched; the localhost issuer keeps the sidecar and
// renders neither.
func TestDefaultIssuerInCluster(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := GenCerts("127.0.0.1.nip.io", false); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	coredns, err := renderTemplate(cfg, "coredns.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	rule := `name regex ^dex\.127\.0\.0\.1\.nip\.io\. ` + dexIssuerService + ".dex.svc.cluster.local"
	exact, wildcard := strings.Index(string(coredns), rule), strings.Index(string(coredns), "agentgateway-edge.agent-platform.svc.cluster.local")
	if exact < 0 || exact > wildcard {
		t.Errorf("CoreDNS does not send dex.127.0.0.1.nip.io to the issuer Service ahead of the edge wildcard:\n%s", coredns)
	}
	dex, err := renderTemplate(cfg, "dex.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dex), "issuer: https://dex.127.0.0.1.nip.io:32000/dex") || !strings.Contains(string(dex), "name: "+dexIssuerService+"\n") {
		t.Errorf("Dex render lacks the issuer under the domain or its Service:\n%s", dex)
	}
	values, err := renderTemplate(cfg, platformValuesTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(values), "host: "+dexServiceHost) {
		t.Errorf("the JWKS routes left Dex's in-cluster name %s", dexServiceHost)
	}
	mcpPrometheus, err := renderTemplate(cfg, mcpPrometheusTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mcpPrometheus), "name: "+dexLocalhostContainer) {
		t.Errorf("mcp-prometheus carries the %s sidecar under the domain:\n%s", dexLocalhostContainer, mcpPrometheus)
	}
	render := func(cfg *config.Config) map[string]string {
		return map[string]string{componentMCPKubernetes: "kind: Deployment\nmetadata: {name: x}\nspec:\n  template:\n    spec:\n      containers:\n        - name: x\n          env:\n            - {name: DEX_ISSUER_URL, value: \"" + cfg.Issuer() + "\"}\n"}
	}
	if targets, err := dexLocalhostTargets(cfg, render(cfg)); err != nil || len(targets) != 0 {
		t.Errorf("sidecar targets under the domain = %v, %v; want none", targets, err)
	}
	if beekeeperDexSidecar(cfg) != "" {
		t.Error("beekeeper serve gets the sidecar under the domain")
	}

	legacy := localhostIssuerConfig()
	if targets, err := dexLocalhostTargets(legacy, render(legacy)); err != nil || len(targets[componentMCPKubernetes]) != 1 {
		t.Errorf("sidecar targets with the localhost issuer = %v, %v; want mcp-kubernetes", targets, err)
	}
	coredns, err = renderTemplate(legacy, "coredns.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	dex, err = renderTemplate(legacy, "dex.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(coredns), dexIssuerService) || strings.Contains(string(dex), "name: "+dexIssuerService+"\n") {
		t.Error("the localhost issuer renders the issuer Service or its CoreDNS rewrite")
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
