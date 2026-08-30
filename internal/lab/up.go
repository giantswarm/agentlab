package lab

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"agentlab/internal/config"
)

// Up brings up the whole lab: certs, kind cluster, Dex, RBAC, an end-to-end
// OIDC verification, and then the components the configuration enables — the
// agent platform (the default; it is what the lab tests) and Backstage.
func Up(cfg *config.Config) error {
	// Fail on Helm 3 before any real work: the platform install at the end of
	// this boot needs Helm 4 (see ensureHelmSupportsPlatform), and finding
	// that out after a five-minute cluster boot is the wrong moment.
	if cfg.Platform.Enabled {
		if err := ensureHelmSupportsPlatform(); err != nil {
			return err
		}
	}

	if err := GenCerts(false); err != nil {
		return err
	}

	// Pure network work that needs no cluster starts first, so it overlaps
	// with cluster creation: vendoring the platform chart, and pulling the
	// Dex image plus the last boot's images into the host docker cache
	// (which survives `down`).
	var chartReady <-chan error
	if cfg.Platform.Enabled {
		chartReady = vendorPlatformChart(cfg)
	}
	pulled := pullLabImages(cfg)
	dexReady := pullDexImage(cfg)

	_, kindCfgPath, err := renderManifest(cfg, "kind-config.yaml.tmpl")
	if err != nil {
		return err
	}
	_, rbacPath, err := renderManifest(cfg, "rbac.yaml.tmpl")
	if err != nil {
		return err
	}

	if kindClusterExists(cfg.ClusterName) {
		step("kind cluster %q already exists", cfg.ClusterName)
	} else {
		step("Creating kind cluster %q", cfg.ClusterName)
		if err := run("kind", "create", "cluster", "--config", kindCfgPath, "--wait", "120s"); err != nil {
			return err
		}
	}
	if err := runQuiet("kubectl", "config", "use-context", cfg.KubeContext()); err != nil {
		return err
	}

	// Side-load the cached images while Dex and the OIDC verification run;
	// joined before the platform install, which is what actually needs them.
	loaded := loadLabImages(cfg, pulled)

	step("Deploying Dex")
	// Namespace and TLS secret land before the Deployment so the pod never
	// waits on a missing volume on first boot.
	if err := ensureNamespace(componentDex); err != nil {
		return err
	}
	if err := ensureSecretFromFiles(componentDex, "dex-tls", map[string]string{
		"tls.crt": tlsCertPath,
		"tls.key": "certs/tls.key",
	}); err != nil {
		return err
	}
	sideloadDexImage(cfg, dexReady)
	if err := ApplyDex(cfg); err != nil {
		return err
	}

	step("Applying RBAC bound to OIDC groups")
	if err := runQuiet("kubectl", "apply", "-f", rbacPath); err != nil {
		return err
	}

	step("Waiting for the issuer to answer on %s", cfg.Issuer())
	client, err := labHTTPClient(10 * time.Second)
	if err != nil {
		return err
	}
	if !waitFor(60, 2*time.Second, func() bool {
		return httpUp(client, cfg.Issuer()+"/.well-known/openid-configuration")
	}) {
		return fmt.Errorf("issuer never answered on %s", cfg.Issuer())
	}
	note("issuer is up")

	// No apiserver bounce needed: since (at least) Kubernetes 1.35 the OIDC
	// authenticator retries discovery every 10s forever (oidc.go "initializing
	// plugin" errors until Dex answers), so this loop just waits out the next
	// retry tick. The token is fetched once — the issuer already answers, so
	// only the apiserver side needs polling.
	step("Verifying the end-to-end OIDC chain")
	admin := cfg.AdminUser()
	tok, err := passwordGrant(cfg, config.KubernetesClientID, config.KubernetesClientSecret,
		admin.Email, admin.Password, "openid email groups")
	if err != nil {
		return err
	}
	const probeKubeconfig = ".kubeconfig.probe"
	if err := writeTokenKubeconfig(cfg, tok, probeKubeconfig); err != nil {
		return err
	}
	verified := waitFor(30, 2*time.Second, func() bool {
		_, err := outputQuiet("kubectl", "--kubeconfig="+probeKubeconfig, "auth", "whoami")
		return err == nil
	})
	_ = os.Remove(probeKubeconfig)
	if !verified {
		return fmt.Errorf("apiserver still rejects Dex tokens; check the apiserver log for oidc.go lines")
	}
	note("apiserver accepts Dex tokens")

	fmt.Println()
	fmt.Println("Lab is up.")
	fmt.Println()
	fmt.Println("  Users:")
	for _, u := range cfg.Users {
		fmt.Printf("    %-22s password: %-12s groups: %s\n",
			u.Email, u.Password, strings.Join(u.Groups, ", "))
	}
	fmt.Println()
	fmt.Println("  Try it:")
	fmt.Printf("    agentlab login %s     # headless, prints the token claims\n", admin.Email)
	fmt.Println("    agentlab browser      # real browser login screen")
	fmt.Println("    agentlab test         # full RBAC assertion run")

	reportPreload(loaded)
	if cfg.Platform.Enabled {
		fmt.Println()
		// Backstage deploys as part of the platform (the umbrella chart's
		// backstage component), through the same agentgateway edge.
		if err := platformUp(cfg, chartReady); err != nil {
			return err
		}
	}
	snapshotPreloadImages(cfg)
	return nil
}

// ApplyDex renders manifests with the input checksum stamped into the pod
// template, applies them, and waits for the rollout. The checksum covers the
// rendered manifest AND the served cert, so editing the config or regenerating
// certs rolls the pod, while an unchanged re-apply is a pure no-op (no
// throwaway ReplicaSet). Shared by Up and `agentlab reload`.
func ApplyDex(cfg *config.Config) error {
	stamped, _, err := renderManifest(cfg, "dex.yaml.tmpl")
	if err != nil {
		return err
	}
	if err := pipeInto(stamped, "kubectl", "apply", "-f", "-"); err != nil {
		return err
	}
	step("Waiting for Dex to become ready")
	return run("kubectl", "-n", componentDex, "rollout", "status", "deployment/dex", "--timeout=120s")
}

func kindClusterExists(name string) bool {
	out, err := outputQuiet("kind", "get", "clusters")
	if err != nil {
		return false
	}
	return slices.Contains(strings.Split(strings.TrimSpace(out), "\n"), name)
}
