package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/client-go/rest"

	"github.com/giantswarm/agentlab/internal/config"
)

// Test is the end-to-end RBAC proof: every configured user gets a real token
// from Dex and a set of `auth can-i` assertions (SelfSubjectAccessReviews as
// that user, token only) driven purely by the `groups` claim in the id_token.
func Test(cfg *config.Config) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	pass, fail := 0, 0

	check := func(user *rest.Config, desc, expect string, canIArgs ...string) {
		got := canIAnswer(user, canIArgs)
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
		// The token alone: the kind kubeconfig's admin client certificate
		// would win over it (kubeconfig.go).
		user, err := tokenConfig(token)
		if err != nil {
			return err
		}

		fmt.Printf("  identity: %s\n", identityLine(user))

		// Assertions per effective role. Group membership is configurable in
		// agentlab.yaml but the group -> role mapping is fixed by the lab RBAC,
		// so the strongest group decides what to assert.
		switch {
		case u.HasGroup("platform-admins"):
			check(user, "cluster-admin: list nodes", "yes", "get", "nodes")
			check(user, "cluster-admin: create ns", "yes", "create", "namespaces")
			check(user, "cluster-admin: write anywhere", "yes", "create", "deployments", "-n", "kube-system")
		case u.HasGroup("developers"):
			check(user, "not a cluster admin", "no", "get", "nodes")
			check(user, "cannot create namespaces", "no", "create", "namespaces")
			check(user, "can write in ns/demo", "yes", "create", "deployments", "-n", "demo")
			check(user, "cannot write in ns/default", "no", "create", "deployments", "-n", "default")
		case u.HasGroup("viewers"):
			check(user, "read-only: list pods cluster-wide", "yes", "list", "pods", "--all-namespaces")
			check(user, "read-only: cannot create", "no", "create", "deployments", "-n", "demo")
			check(user, "read-only: cannot delete", "no", "delete", "pods", "-n", "kube-system")
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

// canIQuestion maps the arguments of `kubectl auth can-i <verb> <resource>
// [-n <namespace> | --all-namespaces]` — the shape the assertions above are
// written in — to what canI asks: without a namespace flag the question is
// asked in the kubeconfig's namespace (defaultNamespace, as kubectl does),
// --all-namespaces / -A in every namespace ("").
func canIQuestion(args []string) (verb, resource, ns string, err error) {
	if len(args) < 2 {
		return "", "", "", fmt.Errorf("can-i needs a verb and a resource, got %q", args)
	}
	verb, resource, ns = args[0], args[1], defaultNamespace
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "-n", "--namespace":
			if i+1 >= len(args) {
				return "", "", "", fmt.Errorf("can-i %s: %s without a namespace", strings.Join(args, " "), args[i])
			}
			i++
			ns = args[i]
		case "--all-namespaces", "-A":
			ns = ""
		default:
			return "", "", "", fmt.Errorf("can-i %s: unsupported argument %q", strings.Join(args, " "), args[i])
		}
	}
	return verb, resource, ns, nil
}

// canIAnswer answers one can-i question the way the CLI prints it — "yes" or
// "no" — as the identity user authenticates as; a question that could not be
// asked reads as the error, which no expectation matches.
func canIAnswer(user *rest.Config, canIArgs []string) string {
	verb, resource, ns, err := canIQuestion(canIArgs)
	if err != nil {
		return "error: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	allowed, err := canI(ctx, user, verb, resource, ns)
	if err != nil {
		return "error: " + err.Error()
	}
	if allowed {
		return "yes"
	}
	return "no"
}

// identityLine is what the apiserver makes of the token: the username and the
// groups (as a JSON list, the way kubectl's jsonpath printed them), or the
// probe's failure.
func identityLine(user *rest.Config) string {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	username, groups, err := whoAmI(ctx, user)
	if err != nil {
		return fmt.Sprintf("(whoami failed: %v)", err)
	}
	raw, _ := json.Marshal(groups)
	return username + "  " + string(raw)
}
