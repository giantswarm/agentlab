package lab

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"agentlab/internal/config"
)

const (
	// nameKey is the "name" field key, shared by the kubeconfig maps and the
	// manifest/JSON lookups elsewhere in the package.
	nameKey = "name"
	// oidcEntryName names the token kubeconfig's user and context entries
	// (and its current-context).
	oidcEntryName = "oidc"
)

// kindCluster is the cluster entry of the kind kubeconfig, cached per process:
// `kind get kubeconfig` spawns a subprocess and its output never changes
// within a run, while Test and the up verification write several kubeconfigs.
var kindCluster struct {
	name    string
	cluster map[string]any
}

func kindClusterEntry(clusterName string) (string, map[string]any, error) {
	if kindCluster.cluster != nil {
		return kindCluster.name, kindCluster.cluster, nil
	}
	raw, err := output("kind", "get", "kubeconfig", "--name", clusterName)
	if err != nil {
		return "", nil, fmt.Errorf("kind get kubeconfig: %w", err)
	}
	var kc struct {
		Clusters []struct {
			Name    string         `yaml:"name"`
			Cluster map[string]any `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if err := yaml.Unmarshal([]byte(raw), &kc); err != nil {
		return "", nil, fmt.Errorf("parsing kind kubeconfig: %w", err)
	}
	if len(kc.Clusters) == 0 {
		return "", nil, fmt.Errorf("kind kubeconfig has no clusters")
	}
	kindCluster.name, kindCluster.cluster = kc.Clusters[0].Name, kc.Clusters[0].Cluster
	return kindCluster.name, kindCluster.cluster, nil
}

// writeTokenKubeconfig builds a kubeconfig that authenticates ONLY with the
// given bearer token. Needed because the kind kubeconfig ships an admin client
// certificate, and a client cert always wins over --token / a token-based
// user — kubectl would silently keep authenticating as kubernetes-admin.
func writeTokenKubeconfig(cfg *config.Config, token, outPath string) error {
	name, cluster, err := kindClusterEntry(cfg.ClusterName)
	if err != nil {
		return err
	}

	out := map[string]any{
		"apiVersion": "v1",
		"kind":       "Config",
		"clusters": []map[string]any{
			{nameKey: name, "cluster": cluster},
		},
		"users": []map[string]any{
			{nameKey: oidcEntryName, "user": map[string]any{"token": token}},
		},
		"contexts": []map[string]any{
			{nameKey: oidcEntryName, "context": map[string]any{
				"cluster": name, "user": oidcEntryName,
			}},
		},
		"current-context": oidcEntryName,
	}
	data, err := yaml.Marshal(out)
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, data, 0o600)
}
