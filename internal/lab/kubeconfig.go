package lab

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"dexlab/internal/config"
)

// writeTokenKubeconfig builds a kubeconfig that authenticates ONLY with the
// given bearer token. Needed because the kind kubeconfig ships an admin client
// certificate, and a client cert always wins over --token / a token-based
// user — kubectl would silently keep authenticating as kubernetes-admin.
func writeTokenKubeconfig(cfg *config.Config, token, outPath string) error {
	raw, err := output("kind", "get", "kubeconfig", "--name", cfg.ClusterName)
	if err != nil {
		return fmt.Errorf("kind get kubeconfig: %w", err)
	}
	var kc struct {
		Clusters []struct {
			Name    string         `yaml:"name"`
			Cluster map[string]any `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if err := yaml.Unmarshal([]byte(raw), &kc); err != nil {
		return fmt.Errorf("parsing kind kubeconfig: %w", err)
	}
	if len(kc.Clusters) == 0 {
		return fmt.Errorf("kind kubeconfig has no clusters")
	}

	out := map[string]any{
		"apiVersion": "v1",
		"kind":       "Config",
		"clusters": []map[string]any{
			{"name": kc.Clusters[0].Name, "cluster": kc.Clusters[0].Cluster},
		},
		"users": []map[string]any{
			{"name": "oidc", "user": map[string]any{"token": token}},
		},
		"contexts": []map[string]any{
			{"name": "oidc", "context": map[string]any{
				"cluster": kc.Clusters[0].Name, "user": "oidc",
			}},
		},
		"current-context": "oidc",
	}
	data, err := yaml.Marshal(out)
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, data, 0o600)
}
