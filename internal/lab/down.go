package lab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/labs"
)

// Down destroys the kind cluster. Certs are kept: the CA is only worth
// regenerating deliberately (agentlab certs --force), and keeping it means an
// immediate `agentlab up` reuses the same trust chain.
func Down(cfg *config.Config) error {
	if err := destroyCluster(cfg); err != nil {
		return err
	}
	fmt.Println("Lab destroyed. certs/ kept (agentlab certs --force to regenerate).")
	fmt.Println("`agentlab down --purge` removes the lab for good: its CA from the trust stores, certs/, state/ and its configuration.")
	return nil
}

// Purge removes the lab for good: the kind cluster as Down does, then the
// lab CA from the trust stores, then the lab's directory, its configuration
// and its registry entry, so `agentlab list` no longer shows it. The CA goes
// before its directory, the one place that names it. A lab outside its Home
// is refused before anything changes, and a failed removal from a trust
// store stops before anything is deleted.
func Purge(cfg *config.Config) error {
	home, err := labs.Home(cfg.ClusterName)
	if err != nil {
		return err
	}
	if !sameDir(".", home) {
		cwd, _ := os.Getwd()
		return fmt.Errorf("lab %s lives in %s, outside agentlab's folders, and that directory may hold more than the lab; run `agentlab down` and delete what you no longer need there by hand", cfg.ClusterName, cwd)
	}
	if err := destroyCluster(cfg); err != nil {
		return err
	}
	if err := untrustLab(); err != nil {
		return fmt.Errorf("%w; the lab's files are kept, run `agentlab down --purge` again once that works", err)
	}
	if err := labs.Remove(cfg.ClusterName); err != nil {
		return err
	}
	fmt.Printf("Lab %s removed: its cluster, its CA from the trust stores, %s and its configuration.\n", cfg.ClusterName, home)
	return nil
}

// sameDir reports whether a and b are one directory.
func sameDir(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

// destroyCluster deletes the kind cluster and the files that described it.
func destroyCluster(cfg *config.Config) error {
	// The workspace volumes (teardown.go) released while the NFS server still
	// runs: the cluster's deletion stops the server, and a node still holding
	// a mount of it then cannot be stopped. Best effort on a cluster whose
	// apiserver answers; the delete follows anyway.
	if workspacesMountable(cfg) {
		ctx := context.Background()
		if err := releaseWorkspaceVolumes(ctx, cfg); err != nil {
			note("releasing the workspace volumes: %v", err)
		}
	}
	if err := deleteClusterWithinBound(cfg.ClusterName, cfg.ControlPlaneNode()); err != nil {
		var broken *brokenNodeError
		if errors.As(err, &broken) {
			return err
		}
		// kind's delete is `docker rm -f` of the node, and docker gives up
		// on a node that does not exit within ten seconds of SIGKILL ("could
		// not kill container: ... did not receive an exit event"), leaving
		// the container in `docker ps -a` and the cluster listed by kind.
		// The node is dying, not surviving (node.go): wait for it, then
		// delete again — now a plain rm of an exited container. One that
		// never exits is a broken lab (brokenNodeError).
		if werr := waitForNodeExit(cfg.ClusterName, nodeExitTimeout, 2*time.Second); werr != nil {
			if errors.As(werr, &broken) {
				return werr
			}
			return fmt.Errorf("%w (%v)", werr, err)
		}
		step("Retrying the cluster deletion")
		if err := deleteClusterWithinBound(cfg.ClusterName, cfg.ControlPlaneNode()); err != nil {
			return err
		}
	}
	_ = os.Remove(".token")
	// The exported kubeconfig described a cluster that no longer exists.
	_ = os.Remove(labKubeconfigPath)
	// The lab registry (the `harness` dev image, devimages.go) served this
	// cluster's Harness; its content is a push away on the next lab.
	removeDevRegistry(cfg)
	// The next `up` is a new deployment for usage data (docs/telemetry.md).
	endTelemetryDeployment()
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
	// The workspace storage (workspaces.go), after the platform: Substrate's
	// CSIDriverConfig and its volumes are finalized by the running chart. The
	// volumes are released before the NFS server goes (teardown.go).
	if err := workspacesTeardown(ctx, cfg); err != nil {
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

// deleteClusterWithinBound is kind's delete of the cluster, bounded by
// nodeStopTimeout: a node that never answers docker's kill is a broken lab.
func deleteClusterWithinBound(cluster, node string) error {
	return withinBound(node, nodeStopTimeout, func() error { return kindDeleteCluster(cluster) })
}

// workspacesMountable reports whether the lab's nodes may hold a mount of
// the workspace storage: the switch on, or its namespace still there from a
// run with it on, on a cluster whose apiserver answers — a node that does
// not run holds no mount.
func workspacesMountable(cfg *config.Config) bool {
	if err := useClusterKubeconfig(cfg); err != nil {
		return false
	}
	if cfg.WorkspacesEnabled() {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exists, err := objectExists(ctx, gvrNamespaces, "", workspacesNamespace)
	return err == nil && exists
}
