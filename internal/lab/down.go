package lab

import (
	"fmt"
	"os"

	"github.com/giantswarm/agentlab/internal/config"
)

// Down destroys the kind cluster. Certs are kept: the CA is only worth
// regenerating deliberately (agentlab certs --force), and keeping it means an
// immediate `agentlab up` reuses the same trust chain.
func Down(cfg *config.Config) error {
	if err := run("kind", "delete", "cluster", "--name", cfg.ClusterName); err != nil {
		return err
	}
	_ = os.Remove(".token")
	// The exported kubeconfig described a cluster that no longer exists.
	_ = os.Remove(labKubeconfigPath)
	fmt.Println("Lab destroyed. certs/ kept (agentlab certs --force to regenerate).")
	return nil
}

// PlatformDown removes the agent platform releases and namespace, leaving Dex
// and the cluster alone. The observability releases go too (they only exist
// to serve the platform's MCP tools); their prometheus-operator CRDs stay —
// helm never removes a chart's crds/, and re-installs are unaffected.
func PlatformDown(cfg *config.Config) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	_ = runQuiet("helm", "-n", observabilityNamespace, "uninstall", mcpPrometheusRelease)
	_ = runQuiet("helm", "-n", observabilityNamespace, "uninstall", kpsRelease)
	_ = runQuiet("kubectl", "delete", "namespace", observabilityNamespace, "--ignore-not-found")
	_ = runQuiet("helm", "-n", platformNamespace, "uninstall", "agent-platform")
	return run("kubectl", "delete", "namespace", platformNamespace, "--ignore-not-found")
}
