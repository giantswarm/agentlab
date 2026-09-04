package lab

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

const (
	// nameKey is the "name" field key, shared by the kubeconfig maps and the
	// manifest/JSON lookups elsewhere in the package.
	nameKey = "name"
	// oidcEntryName names the token kubeconfig's user and context entries
	// (and its current-context).
	oidcEntryName = "oidc"
	// labKubeconfigPath is the lab-owned kubeconfig: the kind cluster's admin
	// kubeconfig exactly as `kind get kubeconfig` emits it (its
	// current-context is the kind-<cluster> context), written by
	// useClusterKubeconfig and set as KUBECONFIG on every kubectl and helm the
	// lab runs (exec.go). Under StateDir like every other generated artifact;
	// `KUBECONFIG=state/kubeconfig kubectl ...` is the same view from a shell.
	labKubeconfigPath = StateDir + "/kubeconfig"
)

// labKubeconfig is labKubeconfigPath made absolute, so a child process reads
// the same file whatever its working directory.
func labKubeconfig() string {
	if abs, err := filepath.Abs(labKubeconfigPath); err == nil {
		return abs
	}
	return labKubeconfigPath
}

// kindKubeconfigCache holds `kind get kubeconfig` per process: a docker round
// trip whose output never changes within a run, while up's verification, test
// and login all want it.
var kindKubeconfigCache struct {
	name string
	raw  []byte
}

// kindKubeconfig returns the cluster's admin kubeconfig from kind, which reads
// it off the control-plane node — independent of any kubeconfig on the host.
// A cluster that is not running fails here, by name, with kind's message.
func kindKubeconfig(clusterName string) ([]byte, error) {
	if kindKubeconfigCache.raw != nil && kindKubeconfigCache.name == clusterName {
		return kindKubeconfigCache.raw, nil
	}
	raw, err := outputQuiet("kind", "get", "kubeconfig", "--name", clusterName)
	if err != nil {
		return nil, fmt.Errorf("no kubeconfig for kind cluster %q (is the lab up? `agentlab up`): %w", clusterName, err)
	}
	kindKubeconfigCache.name, kindKubeconfigCache.raw = clusterName, []byte(raw)
	return kindKubeconfigCache.raw, nil
}

// useClusterKubeconfig exports the kind cluster's kubeconfig to
// labKubeconfigPath. Every command that talks to the cluster calls it first:
// from then on its kubectl and helm are deterministic about the cluster (the
// one agentlab.yaml names), and a lab that is not running fails right here
// instead of as an opaque kubectl error — or, worse, as a command against
// whatever cluster the shell's own kubeconfig happens to point at. The user's
// kubeconfig and current-context are never read or changed.
func useClusterKubeconfig(cfg *config.Config) error {
	raw, err := kindKubeconfig(cfg.ClusterName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return err
	}
	return os.WriteFile(labKubeconfigPath, raw, 0o600)
}

// kindClusterEntry is the cluster entry (name and server/CA) of the kind
// kubeconfig, for the token-only kubeconfigs that reuse the endpoint without
// the admin client certificate.
func kindClusterEntry(clusterName string) (string, map[string]any, error) {
	raw, err := kindKubeconfig(clusterName)
	if err != nil {
		return "", nil, err
	}
	var kc struct {
		Clusters []struct {
			Name    string         `yaml:"name"`
			Cluster map[string]any `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return "", nil, fmt.Errorf("parsing kind kubeconfig: %w", err)
	}
	if len(kc.Clusters) == 0 {
		return "", nil, fmt.Errorf("kind kubeconfig has no clusters")
	}
	return kc.Clusters[0].Name, kc.Clusters[0].Cluster, nil
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
