package lab

import (
	"fmt"
	"os"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// Down destroys the kind cluster. Certs are kept: the CA is only worth
// regenerating deliberately (agentlab certs --force), and keeping it means an
// immediate `agentlab up` reuses the same trust chain.
func Down(cfg *config.Config) error {
	if err := run("kind", "delete", "cluster", "--name", cfg.ClusterName); err != nil {
		// kind delete is `docker rm -f` of the node, and docker gives up on
		// a node that does not exit within ten seconds of SIGKILL ("could
		// not kill container: ... did not receive an exit event"), leaving
		// the container in `docker ps -a` and the cluster listed by kind.
		// The node is dying, not surviving (node.go): wait for it, then
		// delete again — now a plain rm of an exited container.
		if werr := waitForNodeExit(cfg.ClusterName, nodeExitTimeout, 2*time.Second); werr != nil {
			return fmt.Errorf("%w (kind delete cluster: %v)", werr, err)
		}
		step("Retrying kind delete cluster")
		if err := run("kind", "delete", "cluster", "--name", cfg.ClusterName); err != nil {
			return err
		}
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
