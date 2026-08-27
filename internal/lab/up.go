package lab

import (
	"fmt"
	"os"
	"strings"
	"time"

	"dexlab/internal/config"
)

// Up brings up the whole lab: certs, kind cluster, Dex, RBAC, an end-to-end
// OIDC verification, and then whichever optional components the configuration
// enables (agent platform, Backstage).
func Up(cfg *config.Config) error {
	if err := GenCerts(false); err != nil {
		return err
	}

	_, kindCfgPath, err := renderToState(cfg, "kind-config.yaml.tmpl", "kind-config.yaml")
	if err != nil {
		return err
	}
	if _, _, err := renderToState(cfg, "rbac.yaml.tmpl", "rbac.yaml"); err != nil {
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

	step("Deploying Dex")
	// Namespace and TLS secret land before the Deployment so the pod never
	// waits on a missing volume on first boot.
	if err := ensureNamespace("dex"); err != nil {
		return err
	}
	if err := ensureSecretFromFiles("dex", "dex-tls", map[string]string{
		"tls.crt": "certs/tls.crt",
		"tls.key": "certs/tls.key",
	}); err != nil {
		return err
	}
	if err := ApplyDex(cfg); err != nil {
		return err
	}

	step("Applying RBAC bound to OIDC groups")
	if err := runQuiet("kubectl", "apply", "-f", StateDir+"/rbac.yaml"); err != nil {
		return err
	}

	step("Waiting for the issuer to answer on %s", cfg.Issuer())
	client, err := labHTTPClient(10 * time.Second)
	if err != nil {
		return err
	}
	issuerUp := false
	for i := 0; i < 60; i++ {
		if httpUp(client, cfg.Issuer()+"/.well-known/openid-configuration") {
			note("issuer is up")
			issuerUp = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !issuerUp {
		return fmt.Errorf("issuer never answered on %s", cfg.Issuer())
	}

	// No apiserver bounce needed: since (at least) Kubernetes 1.35 the OIDC
	// authenticator retries discovery every 10s forever (oidc.go "initializing
	// plugin" errors until Dex answers), so this loop just waits out the next
	// retry tick.
	step("Verifying the end-to-end OIDC chain")
	admin := cfg.AdminUser()
	verified := false
	const probeKubeconfig = ".kubeconfig.probe"
	for i := 0; i < 30; i++ {
		tok, err := passwordGrant(cfg, "kubernetes", config.KubernetesClientSecret,
			admin.Email, admin.Password, "openid email groups")
		if err == nil {
			if err := writeTokenKubeconfig(cfg, tok, probeKubeconfig); err != nil {
				return err
			}
			if _, err := outputQuiet("kubectl", "--kubeconfig="+probeKubeconfig, "auth", "whoami"); err == nil {
				note("apiserver accepts Dex tokens")
				verified = true
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	os.Remove(probeKubeconfig)
	if !verified {
		return fmt.Errorf("apiserver still rejects Dex tokens; check the apiserver log for oidc.go lines")
	}

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
	fmt.Printf("    dexlab login %s     # headless, prints the token claims\n", admin.Email)
	fmt.Println("    dexlab browser      # real browser login screen")
	fmt.Println("    dexlab test         # full RBAC assertion run")

	if cfg.Platform.Enabled {
		fmt.Println()
		if err := PlatformUp(cfg); err != nil {
			return err
		}
	}
	if cfg.Backstage.Enabled {
		fmt.Println()
		if err := BackstageUp(cfg); err != nil {
			return err
		}
	}
	return nil
}

// ApplyDex renders manifests with the input checksum stamped into the pod
// template, applies them, and waits for the rollout. The checksum covers the
// rendered manifest AND the served cert, so editing the config or regenerating
// certs rolls the pod, while an unchanged re-apply is a pure no-op (no
// throwaway ReplicaSet). Shared by Up and `dexlab reload`.
func ApplyDex(cfg *config.Config) error {
	stamped, path, err := renderStamped(cfg, "dex.yaml.tmpl", "dex.yaml", "certs/tls.crt")
	if err != nil {
		return err
	}
	if err := pipeInto(stamped, "kubectl", "apply", "-f", "-"); err != nil {
		return err
	}
	_ = path
	step("Waiting for Dex to become ready")
	return run("kubectl", "-n", "dex", "rollout", "status", "deployment/dex", "--timeout=120s")
}

func kindClusterExists(name string) bool {
	out, err := outputQuiet("kind", "get", "clusters")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == name {
			return true
		}
	}
	return false
}
