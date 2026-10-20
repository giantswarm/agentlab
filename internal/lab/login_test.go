package lab

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

func TestLoginRequest(t *testing.T) {
	cases := []struct {
		name      string
		audiences []string
		client    string
		scope     string
	}{
		{"no flag keeps the kubernetes client's token", nil, config.KubernetesClientID, loginScopes},
		{"the default", DefaultLoginAudiences, config.KubernetesClientID, loginScopes},
		{"kubernetes twice", []string{config.KubernetesClientID, config.KubernetesClientID}, config.KubernetesClientID, loginScopes},
		{"agent-platform is the platform client's own audience", []string{config.AgentPlatformClientID}, config.AgentPlatformClientID, loginScopes},
		{"agent-platform and kubernetes", []string{config.AgentPlatformClientID, config.KubernetesClientID}, config.AgentPlatformClientID,
			loginScopes + " audience:server:client_id:kubernetes"},
		{"every audience, in a stable order", []string{config.DexK8sAuthenticatorClientID, config.AgentPlatformClientID, config.KubernetesClientID}, config.AgentPlatformClientID,
			loginScopes + " audience:server:client_id:kubernetes audience:server:client_id:dex-k8s-authenticator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, secret, scope, err := loginRequest(tc.audiences)
			if err != nil {
				t.Fatal(err)
			}
			if client != tc.client || scope != tc.scope {
				t.Errorf("loginRequest(%v) = %s %q, want %s %q", tc.audiences, client, scope, tc.client, tc.scope)
			}
			wantSecret := config.KubernetesClientSecret
			if tc.client == config.AgentPlatformClientID {
				wantSecret = config.AgentPlatformClientSecret
			}
			if secret != wantSecret {
				t.Errorf("loginRequest(%v) pairs client %s with the wrong secret", tc.audiences, client)
			}
		})
	}
}

func TestLoginRequestRefusesUnknownAudience(t *testing.T) {
	if _, _, _, err := loginRequest([]string{config.AgentPlatformClientID, "portal"}); err == nil {
		t.Fatal("loginRequest accepted an audience the lab Dex has no client for")
	}
}

func TestTokenAudiences(t *testing.T) {
	if got := tokenAudiences(map[string]any{audClaim: config.KubernetesClientID}); len(got) != 1 || got[0] != config.KubernetesClientID {
		t.Errorf("string aud: %v", got)
	}
	if got := tokenAudiences(map[string]any{audClaim: []any{config.KubernetesClientID, config.AgentPlatformClientID}}); len(got) != 2 || got[1] != config.AgentPlatformClientID {
		t.Errorf("list aud: %v", got)
	}
	if got := tokenAudiences(map[string]any{}); got != nil {
		t.Errorf("no aud: %v", got)
	}
}

func TestSaveLoginArtifactsWithoutKubernetesAudience(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("kubeconfig.oidc", []byte("an earlier login's"), 0o600); err != nil {
		t.Fatal(err)
	}
	token := unsignedJWT(map[string]any{emailClaim: testPortalUser, audClaim: config.AgentPlatformClientID})

	if err := saveLoginArtifacts(&config.Config{}, token); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(".token")
	if err != nil || string(got) != token {
		t.Fatalf(".token = %q, %v; want the token", got, err)
	}
	if _, err := os.Stat("kubeconfig.oidc"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("kubeconfig.oidc stays beside a token the apiserver refuses: %v", err)
	}
}

// unsignedJWT is a JWT with the claims and no valid signature.
func unsignedJWT(claims map[string]any) string {
	enc := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return enc(map[string]string{"alg": "ES256", "typ": "JWT"}) + "." + enc(claims) + "." + base64.RawURLEncoding.EncodeToString([]byte("signature"))
}
