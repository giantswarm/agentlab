package lab

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// Down destroys the kind cluster. Certs are kept: the CA is only worth
// regenerating deliberately (agentlab certs --force), and keeping it means an
// immediate `agentlab up` reuses the same trust chain.
func Down(cfg *config.Config) error {
	if err := kindDeleteCluster(cfg.ClusterName); err != nil {
		// kind's delete is `docker rm -f` of the node, and docker gives up
		// on a node that does not exit within ten seconds of SIGKILL ("could
		// not kill container: ... did not receive an exit event"), leaving
		// the container in `docker ps -a` and the cluster listed by kind.
		// The node is dying, not surviving (node.go): wait for it, then
		// delete again — now a plain rm of an exited container.
		if werr := waitForNodeExit(cfg.ClusterName, nodeExitTimeout, 2*time.Second); werr != nil {
			return fmt.Errorf("%w (%v)", werr, err)
		}
		step("Retrying the cluster deletion")
		if err := kindDeleteCluster(cfg.ClusterName); err != nil {
			return err
		}
	}
	_ = os.Remove(".token")
	// The exported kubeconfig described a cluster that no longer exists.
	_ = os.Remove(labKubeconfigPath)
	fmt.Println("Lab destroyed. certs/ kept (agentlab certs --force to regenerate).")
	return nil
}

// The uninstall timeouts of platform-down: the platform's ordered teardown
// (its pre-delete hooks wait on the component releases and on the engine
// leaving), the Flux controllers an earlier agentlab installed, and the
// observability chart's hooks (unwaited otherwise).
const (
	platformUninstallTimeout   = 10 * time.Minute
	legacyFluxUninstallTimeout = 5 * time.Minute
	kpsUninstallTimeout        = 5 * time.Minute
	// mcpPrometheusDeleteTimeout bounds the wait for helm-controller to
	// uninstall the lab's mcp-prometheus release behind its HelmRelease.
	mcpPrometheusDeleteTimeout = 3 * time.Minute
	// namespaceDeleteTimeout bounds the wait for a namespace to be gone — its
	// contents drained, their finalizers run — the wait `kubectl delete
	// namespace` performs by default, which the CLI leaves unbounded.
	namespaceDeleteTimeout = 5 * time.Minute
)

// The Flux resources the lab's own mcp-prometheus release consists of, as
// kubectl's resource arguments (fully qualified: the group has dots).
const (
	fluxHelmReleaseResource   = "helmreleases.helm.toolkit.fluxcd.io"
	fluxOCIRepositoryResource = "ocirepositories.source.toolkit.fluxcd.io"
)

// PlatformDown removes the agent platform and the observability releases,
// leaving Dex and the cluster alone. The order is the chart's: the lab's own
// mcp-prometheus HelmRelease goes first while helm-controller still runs (a
// clean uninstall of its release), then the waited uninstall of the platform
// through the embedded Helm runs the chart's pre-delete hooks — delete the
// component HelmReleases and wait for their releases to be uninstalled, then
// delete the FluxInstance and wait for the operator to remove Flux with its
// CRDs, which takes every remaining HelmRelease object (the agents' too) with
// it — before Helm removes the operator and the identities. Nothing is left
// with a finalizer nobody processes, so the namespaces delete promptly. The
// prometheus-operator CRDs and the four Flux Operator CRDs stay — Helm never
// removes a chart's crds/, and re-installs are unaffected.
func PlatformDown(cfg *config.Config) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	ctx := context.Background()
	if mcpPrometheusReleaseExists(ctx) {
		step("Uninstalling mcp-prometheus (its HelmRelease, while the engine still runs)")
		if gvr, err := gvrFor(fluxHelmReleaseResource); err == nil {
			_ = deleteObject(ctx, gvr, platformNamespace, mcpPrometheusRelease, mcpPrometheusDeleteTimeout)
		}
		if gvr, err := gvrFor(fluxOCIRepositoryResource); err == nil {
			_ = deleteObject(ctx, gvr, platformNamespace, mcpPrometheusRelease, 0)
		}
	}
	if helmReleaseExists(platformNamespace, platformRelease) {
		step("Uninstalling the platform (the chart's ordered teardown: releases, then the engine)")
		if err := helmUninstall(platformNamespace, platformRelease, true, platformUninstallTimeout); err != nil {
			return err
		}
	}
	if helmReleaseExists(observabilityNamespace, kpsRelease) {
		// Best-effort, unwaited: the namespace delete below takes the rest.
		_ = helmUninstall(observabilityNamespace, kpsRelease, false, kpsUninstallTimeout)
	}
	_ = deleteNamespace(ctx, observabilityNamespace)
	if err := deleteNamespace(ctx, platformNamespace); err != nil {
		return err
	}
	// Substrate after the platform: the teardown above deleted kagent's
	// WorkerPool and Harnesses, whose finalizers ate-controller processed.
	if err := substrateDown(ctx); err != nil {
		return err
	}
	// What an earlier agentlab installed next to the umbrella: its own Flux
	// controllers for the agent create flow. The chart brings the engine now
	// and refuses a second Flux (refuseOlderLabShape), so they go with the
	// platform — after it, so the umbrella's agent HelmReleases were
	// finalized by a running helm-controller — and `agentlab platform` is a
	// clean reinstall on this cluster.
	if legacyFluxInstalled() {
		step("Uninstalling the Flux controllers an earlier agentlab installed (release %s in %s)", legacyFluxRelease, legacyFluxNamespace)
		if err := helmUninstall(legacyFluxNamespace, legacyFluxRelease, true, legacyFluxUninstallTimeout); err != nil {
			return err
		}
		if err := deleteNamespace(ctx, legacyFluxNamespace); err != nil {
			return err
		}
	}
	return nil
}

// mcpPrometheusReleaseExists reports whether the lab's mcp-prometheus
// HelmRelease is on the cluster. A cluster without the Flux CRDs (no platform
// was ever installed) has none, whatever the read said.
func mcpPrometheusReleaseExists(ctx context.Context) bool {
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return false
	}
	exists, err := objectExists(ctx, gvr, platformNamespace, mcpPrometheusRelease)
	return err == nil && exists
}

// deleteNamespace is `kubectl delete namespace <ns> --ignore-not-found`: a
// namespace that is not there is success, and one that is gets deleted and
// waited for — the CLI's default wait, bounded by namespaceDeleteTimeout —
// so what follows (a reinstall, the next uninstall) meets no Terminating
// namespace.
func deleteNamespace(ctx context.Context, ns string) error {
	exists, err := objectExists(ctx, gvrNamespaces, "", ns)
	if err != nil || !exists {
		return err
	}
	if err := deleteObject(ctx, gvrNamespaces, "", ns, namespaceDeleteTimeout); err != nil {
		return err
	}
	note("namespace %s deleted", ns)
	return nil
}
