package lab

import (
	"fmt"
	"os"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

// Test is the end-to-end RBAC proof: every configured user gets a real token
// from Dex and a set of `kubectl auth can-i` assertions driven purely by the
// `groups` claim in the id_token.
func Test(cfg *config.Config) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	pass, fail := 0, 0

	check := func(kubeconfig, desc, expect string, canIArgs ...string) {
		args := append([]string{"--kubeconfig=" + kubeconfig, "auth", "can-i"}, canIArgs...)
		got, _ := outputQuiet("kubectl", args...)
		got = strings.TrimSpace(got)
		if got == expect {
			fmt.Printf("  \033[32mPASS\033[0m  %-46s (can-i %s = %s)\n", desc, strings.Join(canIArgs, " "), got)
			pass++
		} else {
			fmt.Printf("  \033[31mFAIL\033[0m  %-46s (can-i %s = %s, wanted %s)\n", desc, strings.Join(canIArgs, " "), got, expect)
			fail++
		}
	}

	for _, u := range cfg.Users {
		token, err := passwordGrant(cfg, config.KubernetesClientID, config.KubernetesClientSecret,
			u.Email, u.Password, "openid email profile groups")
		if err != nil {
			return fmt.Errorf("could not get a token for %s: %w", u.Email, err)
		}

		fmt.Printf("\n=== %s ===\n", u.Email)
		kubeconfig := ".kubeconfig." + u.Username
		if err := writeTokenKubeconfig(cfg, token, kubeconfig); err != nil {
			return err
		}
		defer func() { _ = os.Remove(kubeconfig) }()

		whoami, _ := outputQuiet("kubectl", "--kubeconfig="+kubeconfig, "auth", "whoami",
			"-o", "jsonpath={.status.userInfo.username}  {.status.userInfo.groups}")
		fmt.Printf("  identity: %s\n", whoami)

		// Assertions per effective role. Group membership is configurable in
		// agentlab.yaml but the group -> role mapping is fixed by the lab RBAC,
		// so the strongest group decides what to assert.
		switch {
		case u.HasGroup("platform-admins"):
			check(kubeconfig, "cluster-admin: list nodes", "yes", "get", "nodes")
			check(kubeconfig, "cluster-admin: create ns", "yes", "create", "namespaces")
			check(kubeconfig, "cluster-admin: write anywhere", "yes", "create", "deployments", "-n", "kube-system")
		case u.HasGroup("developers"):
			check(kubeconfig, "not a cluster admin", "no", "get", "nodes")
			check(kubeconfig, "cannot create namespaces", "no", "create", "namespaces")
			check(kubeconfig, "can write in ns/demo", "yes", "create", "deployments", "-n", "demo")
			check(kubeconfig, "cannot write in ns/default", "no", "create", "deployments", "-n", "default")
		case u.HasGroup("viewers"):
			check(kubeconfig, "read-only: list pods cluster-wide", "yes", "list", "pods", "--all-namespaces")
			check(kubeconfig, "read-only: cannot create", "no", "create", "deployments", "-n", "demo")
			check(kubeconfig, "read-only: cannot delete", "no", "delete", "pods", "-n", "kube-system")
		default:
			fmt.Println("  (no lab groups; nothing to assert)")
		}
	}

	fmt.Println()
	fmt.Println("-------------------------------------------------")
	fmt.Printf("passed: %d   failed: %d\n", pass, fail)
	if fail > 0 {
		return fmt.Errorf("%d RBAC assertions failed", fail)
	}
	return nil
}
