package lab

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/giantswarm/agentplatform-kind/internal/config"
)

// Login performs the headless password-grant login, writes the raw id_token
// to .token and a token-only kubeconfig to kubeconfig.oidc, and prints the
// token claims.
func Login(cfg *config.Config, email, password string) error {
	token, err := passwordGrant(cfg, config.KubernetesClientID, config.KubernetesClientSecret,
		email, password, "openid email profile groups offline_access")
	if err != nil {
		return err
	}
	return saveLoginArtifacts(cfg, token)
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
	// A kubeconfig that carries ONLY this token. A plain `kubectl --token=...`
	// is not enough: the kind kubeconfig ships an admin client certificate,
	// and a client cert always wins over a bearer token.
	if err := writeTokenKubeconfig(cfg, token, "kubeconfig.oidc"); err != nil {
		return err
	}

	cwd, _ := os.Getwd()
	fmt.Printf(`
Logged in as %v.
  raw token   -> .token
  kubeconfig  -> kubeconfig.oidc

  export KUBECONFIG=%s/kubeconfig.oidc
  kubectl auth whoami
`, claims["email"], cwd)
	return nil
}
