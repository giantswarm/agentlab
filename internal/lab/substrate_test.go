package lab

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The four pool Secrets are created once — CA pools as kubernetes.io/tls
// with the pool, chain and key, the JWT pool Opaque with the pool — and an
// existing pool is never regenerated: a second run keeps every byte.
func TestEnsureSubstratePools(t *testing.T) {
	newFakeLab(t)
	ctx := context.Background()
	created, err := ensureSubstratePools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if created != len(substratePools) {
		t.Errorf("created %d pools, want %d", created, len(substratePools))
	}
	first := map[string]*unstructured.Unstructured{}
	for _, pool := range substratePools {
		secret, err := getObject(ctx, gvrSecrets, pool.namespace, pool.name)
		if err != nil {
			t.Fatalf("pool %s/%s: %v", pool.namespace, pool.name, err)
		}
		first[pool.name] = secret
		typ, _, _ := unstructured.NestedString(secret.Object, "type")
		data, _, _ := unstructured.NestedMap(secret.Object, "data")
		switch {
		case pool.jwt && (typ != string(corev1.SecretTypeOpaque) || len(data) != 1 || data["pool"] == nil):
			t.Errorf("JWT pool %s: type %q, keys %v; want Opaque with pool", pool.name, typ, data)
		case !pool.jwt && (typ != string(corev1.SecretTypeTLS) || len(data) != 3 || data["pool"] == nil || data[corev1.TLSCertKey] == nil || data[corev1.TLSPrivateKeyKey] == nil):
			t.Errorf("CA pool %s: type %q, keys %v; want kubernetes.io/tls with pool, tls.crt, tls.key", pool.name, typ, data)
		}
	}
	created, err = ensureSubstratePools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Errorf("second run created %d pools, want 0", created)
	}
	for _, pool := range substratePools {
		secret, err := getObject(ctx, gvrSecrets, pool.namespace, pool.name)
		if err != nil {
			t.Fatal(err)
		}
		before, _, _ := unstructured.NestedMap(first[pool.name].Object, "data")
		after, _, _ := unstructured.NestedMap(secret.Object, "data")
		if before["pool"] != after["pool"] {
			t.Errorf("pool %s was regenerated on the second run", pool.name)
		}
	}
}

// actor-id-ca-certs is derived from the actor-id CA pool: its root as PEM
// under ca.crt, identical to the pool Secret's tls.crt.
func TestEnsureActorIDCACerts(t *testing.T) {
	data, err := generateCAPoolSecretData(substratePoolID)
	if err != nil {
		t.Fatal(err)
	}
	newFakeLab(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: actorIDCAPool, Namespace: substrateNamespace},
		Type:       corev1.SecretTypeTLS,
		Data:       data,
	})
	ctx := context.Background()
	if err := ensureActorIDCACerts(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := secretDataKey(ctx, substrateNamespace, actorIDCACertsSecret, caCertKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data[corev1.TLSCertKey]) {
		t.Errorf("ca.crt is not the pool's root:\n%s", got)
	}
	// Idempotent: a second derivation applies the same bytes.
	if err := ensureActorIDCACerts(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := secretDataKey(ctx, substrateNamespace, actorIDCACertsSecret, "missing"); err == nil || !strings.Contains(err.Error(), "no data key missing") {
		t.Errorf("a missing key must be named: %v", err)
	}
}

// ate-api-server's authentication.yaml: the in-cluster issuer in kind's own
// spelling with the projected ServiceAccount CA and token to discover it by
// (the exact document the POC bootstrap wrote); an external issuer without.
func TestATEAPIAuthentication(t *testing.T) {
	want := "actorIdentityJWTProvider: kubernetes\n" +
		"jwtProviders:\n" +
		"- name: kubernetes\n" +
		"  issuer: https://kubernetes.default.svc.cluster.local\n" +
		"  audiences: [api.ate-system.svc]\n" +
		"  certificateAuthorityFile: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt\n" +
		"  discoveryTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token\n"
	if got := ateAPIAuthentication("https://kubernetes.default.svc.cluster.local"); got != want {
		t.Errorf("in-cluster issuer:\n%s\nwant\n%s", got, want)
	}
	if got := ateAPIAuthentication(inClusterIssuer); !strings.Contains(got, "discoveryTokenFile") {
		t.Errorf("the short in-cluster issuer needs the token file too:\n%s", got)
	}
	got := ateAPIAuthentication("https://accounts.google.com")
	if strings.Contains(got, "certificateAuthorityFile") || !strings.Contains(got, "issuer: https://accounts.google.com\n") {
		t.Errorf("external issuer:\n%s", got)
	}
}

// A cluster without the certificates.k8s.io/v1beta1 API is refused before
// anything installs, with the only fix — a new cluster — spelled out; the
// issuer read fails the same way on an apiserver that publishes none.
func TestSubstratePreflightWithoutTheAPI(t *testing.T) {
	newFakeLab(t)
	ctx := context.Background()
	err := preflightPodCertificateAPI(ctx)
	if err == nil || !strings.Contains(err.Error(), "agentlab down && agentlab up") {
		t.Errorf("want the recreate hint, got %v", err)
	}
	if _, err := serviceAccountIssuer(ctx); err == nil {
		t.Error("an apiserver without a discovery document must be an error, not an empty issuer")
	}
}
