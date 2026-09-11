package lab

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// Up brings up the whole lab: certs, kind cluster, Dex, RBAC, an end-to-end
// OIDC verification, and then the components the configuration enables — the
// agent platform (the default; it is what the lab tests) and Backstage.
func Up(cfg *config.Config, offers Offers) error {
	// Before any real work: a docker VM too small for what this
	// configuration schedules leaves pods Pending forever (the scheduler
	// refuses CPU requests that do not fit — resources.go), and the symptom
	// would be an install timing out on agentgateway, minutes from now —
	// after a five-minute cluster boot, the wrong moment. Refused here, with
	// the fix and the numbers — for what the chart about to be installed
	// ships (its rendered roster: Agent Substrate and the platform Postgres
	// come with the agents on the 4.x line), not for a version's folklore.
	if err := preflightRuntimeResources(cfg, platformTopologyFor(cfg)); err != nil {
		return err
	}

	if err := GenCerts(cfg.Platform.Domain, false); err != nil {
		return err
	}

	// Pure network work that needs no cluster starts first, so it overlaps
	// with cluster creation: pulling the Dex image plus the last boot's
	// images into the host docker cache (which survives `down`).
	pulled := pullLabImages(cfg)
	dexReady := pullDexImage(cfg)

	kindCfg, _, err := renderManifest(cfg, "kind-config.yaml.tmpl")
	if err != nil {
		return err
	}
	rbac, _, err := renderManifest(cfg, "rbac.yaml.tmpl")
	if err != nil {
		return err
	}

	if kindClusterExists(cfg.ClusterName) {
		step("kind cluster %q already exists", cfg.ClusterName)
		// Its node may be exited — what a `down` that lost its race with
		// docker leaves behind (node.go); the kubeconfig read below would
		// fail on it with an opaque docker exec error.
		if err := ensureNodeRunning(cfg); err != nil {
			return err
		}
	} else {
		step("Creating kind cluster %q (%s)", cfg.ClusterName, kindNodeImage())
		if err := kindCreateCluster(cfg.ClusterName, kindCfg); err != nil {
			return err
		}
	}
	// From here on the embedded Helm and the Kubernetes client run against
	// the cluster's exported kubeconfig (restclient.go, kube.go), the one
	// file they are built from. The user's own kubeconfig and current-context
	// are never touched: the embedded kind writes the admin kubeconfig to
	// state/kubeconfig (kind.go), and re-reading it off the node here covers a
	// cluster that already existed too.
	if err := useClusterKubeconfig(cfg); err != nil {
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
	if _, err := applyManifests(context.Background(), rbac); err != nil {
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
	// A config that carries ONLY the token: the admin client certificate of
	// the kind kubeconfig would win over it (kube.go, tokenConfig).
	probe, err := tokenConfig(tok)
	if err != nil {
		return err
	}
	var username string
	var groups []string
	verified := waitFor(30, 2*time.Second, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var err error
		username, groups, err = whoAmI(ctx, probe)
		return err == nil
	})
	if !verified {
		return fmt.Errorf("apiserver still rejects Dex tokens; check the apiserver log for oidc.go lines")
	}
	note("apiserver accepts Dex tokens: %s is %s in %s", admin.Email, username, strings.Join(groups, ", "))

	reportPreload(loaded)
	if cfg.Platform.Enabled {
		// Backstage deploys as part of the platform (the chart's backstage
		// component), through the same agentgateway edge. One summary per
		// boot: the platform path prints it — users, URLs and try-it
		// commands together — once everything is actually up.
		if err := platformUp(cfg, "Lab is up.", offers); err != nil {
			return err
		}
	} else {
		fmt.Println()
		fmt.Println("Lab is up.")
		fmt.Print(usersBlock(cfg))
		fmt.Print(tryItBlock(cfg))
		// The platform summary carries its own, more complete trust hint.
		if !SystemTrusted() {
			fmt.Println()
			fmt.Println("  The browser will warn on the Dex login page (lab-CA certificate). One-time")
			fmt.Println("  fix, reverted by `agentlab untrust`:  agentlab trust")
		}
		// Bookkeeping this boot earned goes in before the question: in
		// accessible mode the prompt reads lines, so a Ctrl-C there is a real
		// SIGINT that ends the process (platformUp snapshots first for the
		// same reason). No portal without the platform (config.Validate), so
		// this path can only offer the trust step.
		snapshotPreloadImages(cfg)
		offerTrustAndOpen(cfg, offers, false)
		return nil
	}
	snapshotPreloadImages(cfg)
	return nil
}

// usersBlock and tryItBlock are the "what to do next" tail of every boot
// summary — the configured identities and the commands that exercise them.
// Both start with a blank separator line and end with a newline so callers
// can concatenate them between other summary sections.
func usersBlock(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("\n  Users:\n")
	for _, u := range cfg.Users {
		fmt.Fprintf(&b, "    %-22s password: %-12s groups: %s\n",
			u.Email, u.Password, strings.Join(u.Groups, ", "))
	}
	return b.String()
}

func tryItBlock(cfg *config.Config) string {
	var cmds [][2]string
	// The names come from the open table (open.go), so a renamed target
	// cannot leave a Try it line that refuses when run.
	if cfg.Backstage.Enabled {
		cmds = append(cmds, [2]string{"agentlab open " + openTargetPortal, "the portal in the browser"})
	}
	if cfg.Platform.Enabled && cfg.Platform.Agents {
		cmds = append(cmds, [2]string{"agentlab open " + openTargetAgents, "the kagent UI in the browser"})
	}
	cmds = append(cmds,
		[2]string{"agentlab login " + cfg.AdminUser().Email, "headless, prints the token claims"},
		[2]string{"agentlab login --browser", "real browser login screen"},
		[2]string{"agentlab test", "full RBAC assertion run"},
	)
	if cfg.Platform.Enabled {
		cmds = append(cmds, [2]string{"agentlab platform-test", "headless smoke test of the whole platform"})
	}
	if cfg.ModelManagerEnabled() {
		cmds = append(cmds, [2]string{"agentlab models-test", "pull -> ModelConfig -> agent turn -> unload -> delete, through the platform"})
	}
	if cfg.Platform.Enabled && cfg.Platform.Agents {
		cmds = append(cmds, [2]string{"agentlab agents-test", "agent-manager as the caller: create -> ready -> update -> delete, a viewer refused, the ServiceAccount without RBAC"})
	}
	width := 0
	for _, c := range cmds {
		width = max(width, len(c[0]))
	}
	var b strings.Builder
	b.WriteString("\n  Try it:\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, "    %-*s   # %s\n", width, c[0], c[1])
	}
	return b.String()
}

// ApplyDex renders manifests with the input checksum stamped into the pod
// template, applies them, and waits for the rollout. The checksum covers the
// rendered manifest AND the served cert, so editing the config or regenerating
// certs rolls the pod, while an unchanged re-apply is a pure no-op (no
// throwaway ReplicaSet). Shared by Up and `agentlab reload`.
func ApplyDex(cfg *config.Config) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	stamped, _, err := renderManifest(cfg, "dex.yaml.tmpl")
	if err != nil {
		return err
	}
	if _, err := applyManifests(context.Background(), stamped); err != nil {
		return err
	}
	step("Waiting for Dex to become ready")
	return waitDeploymentRolledOut(context.Background(), componentDex, componentDex, 120*time.Second)
}

// kindClusterExists reports whether kind knows a cluster by that name — its
// node containers exist, running or not (node.go).
func kindClusterExists(name string) bool {
	clusters, err := kindClusters()
	return err == nil && slices.Contains(clusters, name)
}
