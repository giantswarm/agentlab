package lab

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

// The id_token claims `login` reads.
const (
	audClaim   = "aud"
	emailClaim = "email"
)

// loginScopes are the claims every `login` token carries.
const loginScopes = "openid email profile groups offline_access"

// loginAudiences are the audiences `login --audience` mints a token for: the
// lab Dex's clients. kubernetes is the apiserver's (and mcp-kubernetes'),
// agent-platform the one muster and the agent-platform MCP servers
// (workspace-manager, agent-manager, model-manager, ...) accept,
// dex-k8s-authenticator the one muster's own Kubernetes writes need.
var loginAudiences = []string{config.KubernetesClientID, config.AgentPlatformClientID, config.DexK8sAuthenticatorClientID}

// DefaultLoginAudiences is `login`'s audience without --audience.
var DefaultLoginAudiences = []string{config.KubernetesClientID}

// loginRequest returns the Dex client and the scope that mint a token for
// audiences. The default (kubernetes alone) is the kubernetes client's own
// token. Any other set is requested by the agent-platform client — the one
// client the others list under trustedPeers — with a cross-client scope per
// audience besides its own; Dex adds the requesting client to the audience,
// so such a token always carries agent-platform too.
func loginRequest(audiences []string) (clientID, clientSecret, scope string, err error) {
	if len(audiences) == 0 {
		audiences = DefaultLoginAudiences
	}
	for _, aud := range audiences {
		if !slices.Contains(loginAudiences, aud) {
			return "", "", "", fmt.Errorf("unknown audience %q: the lab Dex mints %s", aud, strings.Join(loginAudiences, ", "))
		}
	}
	if !slices.ContainsFunc(audiences, func(aud string) bool { return aud != config.KubernetesClientID }) {
		return config.KubernetesClientID, config.KubernetesClientSecret, loginScopes, nil
	}
	scopes := []string{loginScopes}
	for _, aud := range loginAudiences {
		if aud != config.AgentPlatformClientID && slices.Contains(audiences, aud) {
			scopes = append(scopes, crossClientScopePrefix+aud)
		}
	}
	return config.AgentPlatformClientID, config.AgentPlatformClientSecret, strings.Join(scopes, " "), nil
}

// Login performs the headless password-grant login for audiences (default
// DefaultLoginAudiences), writes the raw id_token to .token and, when the
// token carries the apiserver's audience, a token-only kubeconfig to
// kubeconfig.oidc, and prints the token claims.
func Login(cfg *config.Config, email, password string, audiences []string) error {
	clientID, clientSecret, scope, err := loginRequest(audiences)
	if err != nil {
		return err
	}
	token, err := passwordGrant(cfg, clientID, clientSecret, email, password, scope)
	if err != nil {
		return err
	}
	return saveLoginArtifacts(cfg, token)
}

// tokenAudiences returns a token's aud claim, a string or a list in a JWT.
func tokenAudiences(claims map[string]any) []string {
	switch aud := claims[audClaim].(type) {
	case string:
		return []string{aud}
	case []any:
		var out []string
		for _, a := range aud {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// saveLoginArtifacts is the shared tail of `login` and `browser`: print the
// token claims, write .token and kubeconfig.oidc, and print how to use them.
func saveLoginArtifacts(cfg *config.Config, token string) error {
	claims, err := decodeJWTClaims(token)
	if err != nil {
		return err
	}
	pretty, _ := json.MarshalIndent(claims, "", "  ")
	fmt.Println("id_token claims:")
	fmt.Println(string(pretty))

	if err := os.WriteFile(".token", []byte(token), 0o600); err != nil {
		return err
	}
	cwd, _ := os.Getwd()

	// The apiserver accepts only the kubernetes audience: a token without it
	// gets no kubeconfig, and an earlier login's goes, so .token and
	// kubeconfig.oidc never name different logins.
	if !slices.Contains(tokenAudiences(claims), config.KubernetesClientID) {
		if err := os.Remove("kubeconfig.oidc"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		fmt.Printf(`
Logged in as %v.
  raw token   -> .token (audience %s)
  kubeconfig  -> none: the apiserver accepts only audience %s (add --audience %s)

  curl -H "Authorization: Bearer $(cat %s/.token)" ...
`, claims[emailClaim], strings.Join(tokenAudiences(claims), ", "), config.KubernetesClientID, config.KubernetesClientID, cwd)
		return nil
	}

	// A kubeconfig that carries ONLY this token. A plain `kubectl --token=...`
	// is not enough: the kind kubeconfig ships an admin client certificate,
	// and a client cert always wins over a bearer token.
	if err := writeTokenKubeconfig(cfg, token, "kubeconfig.oidc"); err != nil {
		return err
	}

	fmt.Printf(`
Logged in as %v.
  raw token   -> .token
  kubeconfig  -> kubeconfig.oidc

  export KUBECONFIG=%s/kubeconfig.oidc
  kubectl auth whoami
`, claims[emailClaim], cwd)
	return nil
}
