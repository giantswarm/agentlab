package lab

import (
	"encoding/base64"
	"errors"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// currentKID is the key Dex signs with in TestJudgeApiserverOIDC.
const currentKID = "kid-current"

// TestJudgeApiserverOIDC: only an Unauthorized for a key Dex publishes is the
// apiserver's stale JWKS cache, which platform-test waits out; an unpublished
// key is Dex's state and any other error is no authentication answer.
func TestJudgeApiserverOIDC(t *testing.T) {
	published := []string{currentKID, "kid-retired"}
	unauthorized := apierrors.NewUnauthorized("Unauthorized")
	for name, tc := range map[string]struct {
		kid  string
		err  error
		want oidcVerdict
	}{
		"the token accepted":                {kid: currentKID, want: oidcAccepted},
		"refused, key published":            {kid: currentKID, err: unauthorized, want: oidcStaleKeys},
		"refused, key not published":        {kid: "kid-unpublished", err: unauthorized, want: oidcRefused},
		"refused, token without a key ID":   {err: unauthorized, want: oidcRefused},
		"forbidden is an answer, not stale": {kid: currentKID, err: apierrors.NewForbidden(gvrPods.GroupResource(), "x", errors.New("no")), want: oidcRefused},
		"apiserver unreachable":             {kid: currentKID, err: errors.New("connection refused"), want: oidcRefused},
	} {
		t.Run(name, func(t *testing.T) {
			if got := judgeApiserverOIDC(tc.kid, published, tc.err); got != tc.want {
				t.Fatalf("judgeApiserverOIDC(%q, %v, %v) = %d, want %d", tc.kid, published, tc.err, got, tc.want)
			}
		})
	}
}

// TestJWTKeyID: the kid comes off the header; a token that is no compact JWT
// is refused.
func TestJWTKeyID(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"kid-header"}`))
	kid, err := jwtKeyID(header + ".e30.c2ln")
	if err != nil || kid != "kid-header" {
		t.Fatalf("jwtKeyID = %q, %v; want kid-header", kid, err)
	}
	if _, err := jwtKeyID("opaque"); err == nil {
		t.Fatal("jwtKeyID accepted a token that is no JWT")
	}
}

// TestParseJWKSKeyIDs: every key's kid, in the document's order.
func TestParseJWKSKeyIDs(t *testing.T) {
	ids, err := parseJWKSKeyIDs([]byte(`{"keys":[{"kty":"RSA","kid":"kid-a"},{"kty":"RSA","kid":"kid-b"}]}`))
	if err != nil || !slices.Equal(ids, []string{"kid-a", "kid-b"}) {
		t.Fatalf("parseJWKSKeyIDs = %v, %v", ids, err)
	}
	if _, err := parseJWKSKeyIDs([]byte("<html>")); err == nil {
		t.Fatal("parseJWKSKeyIDs accepted a page that is no JWKS")
	}
}

// TestDexTemplateRotatesKeysMonthly: the rendered Dex config names the local
// signer with a 30-day rotation, so a lab that lives for days does not meet
// the apiserver's JWKS window every 6 hours.
func TestDexTemplateRotatesKeysMonthly(t *testing.T) {
	raw, err := renderTemplate(config.Default(), "dex.yaml.tmpl", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	objs, err := decodeManifests(raw)
	if err != nil {
		t.Fatalf("the rendered manifests do not decode: %v", err)
	}
	var dexConfig string
	for _, obj := range objs {
		if obj.GetKind() == kindSecret && obj.GetName() == "dex-config" {
			dexConfig, _, _ = unstructured.NestedString(obj.Object, "stringData", "config.yaml")
		}
	}
	var parsed struct {
		Signer struct {
			Type   string
			Config struct {
				KeysRotationPeriod string `yaml:"keysRotationPeriod"`
			}
		}
		Expiry map[string]any
	}
	if err := yaml.Unmarshal([]byte(dexConfig), &parsed); err != nil {
		t.Fatalf("the Dex config does not parse: %v\n%s", err, dexConfig)
	}
	if parsed.Signer.Type != "local" || parsed.Signer.Config.KeysRotationPeriod != "720h" {
		t.Fatalf("signer %+v, want local with keysRotationPeriod 720h", parsed.Signer)
	}
	if _, ok := parsed.Expiry["signingKeys"]; ok {
		t.Fatal("expiry.signingKeys next to the signer: Dex warns and ignores it")
	}
}
