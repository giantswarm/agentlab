package lab

import (
	"encoding/json"
	"fmt"
	"os"

	"dexlab/internal/config"
)

// Login performs the headless password-grant login, writes the raw id_token
// to .token and a token-only kubeconfig to kubeconfig.oidc, and prints the
// token claims.
func Login(cfg *config.Config, email, password string) error {
	token, err := passwordGrant(cfg, "kubernetes", config.KubernetesClientSecret,
		email, password, "openid email profile groups offline_access")
	if err != nil {
		return err
	}
	if err := os.WriteFile(".token", []byte(token), 0o600); err != nil {
		return err
	}

	claims, err := decodeJWTClaims(token)
	if err != nil {
		return err
	}
	pretty, _ := json.MarshalIndent(claims, "", "  ")
	fmt.Println("id_token claims:")
	fmt.Println(string(pretty))

	// A kubeconfig that carries ONLY this token. A plain `kubectl --token=...`
	// is not enough: the kind kubeconfig ships an admin client certificate,
	// and a client cert always wins over a bearer token.
	if err := writeTokenKubeconfig(cfg, token, "kubeconfig.oidc"); err != nil {
		return err
	}

	cwd, _ := os.Getwd()
	fmt.Printf(`
Logged in as %s.
  raw token   -> .token
  kubeconfig  -> kubeconfig.oidc

  export KUBECONFIG=%s/kubeconfig.oidc
  kubectl auth whoami
`, email, cwd)
	return nil
}
