package lab

import (
	"fmt"
	"os"

	"agentlab/internal/config"
)

// Down destroys the kind cluster. Certs are kept: the CA is only worth
// regenerating deliberately (agentlab certs --force), and keeping it means an
// immediate `agentlab up` reuses the same trust chain.
func Down(cfg *config.Config) error {
	if err := run("kind", "delete", "cluster", "--name", cfg.ClusterName); err != nil {
		return err
	}
	os.Remove(".token")
	fmt.Println("Lab destroyed. certs/ kept (agentlab certs --force to regenerate).")
	return nil
}

// PlatformDown removes the agent platform releases and namespace, leaving Dex
// and the cluster alone.
func PlatformDown(cfg *config.Config) error {
	_ = runQuiet("helm", "-n", platformNamespace, "uninstall", "agent-platform")
	_ = runQuiet("helm", "-n", platformNamespace, "uninstall", "mcp-kubernetes")
	return run("kubectl", "delete", "namespace", platformNamespace, "--ignore-not-found")
}
