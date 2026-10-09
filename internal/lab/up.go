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
	//
	// The dev channel is resolved first: the render must judge the build the
	// install will use, not the one the last run recorded (platformUp
	// resolves too, for the standalone entry point, and finds it done).
	if cfg.Platform.Enabled {
		if changed, err := ResolveChartVersion(cfg); err != nil {
			return err
		} else if changed {
			if err := cfg.Save(); err != nil {
				return err
			}
		}
	}
	if cfg.Platform.Enabled {
		if err := cfg.CheckValuesFiles(); err != nil {
			return err
		}
	}
	// The certificates first: the pre-boot render reads the CA bundle
	// (trustBundleFile) the platform values carry.
	if err := GenCerts(cfg.Platform.Domain, false); err != nil {
		return err
	}
	if err := writeIssuerFiles(cfg); err != nil {
		return err
	}
	topology, err := platformTopologyFor(cfg)
	if err != nil {
		return err
	}
	if err := preflightRuntimeResources(cfg, topology); err != nil {
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
		warnUnguardedNode(cfg.ControlPlaneNode())
		if err := checkClusterIssuer(cfg); err != nil {
			return err
		}
		if err := checkClusterNodes(cfg); err != nil {
			return err
		}
	} else {
		if err := writeNodeFiles(); err != nil {
			return err
		}
		hostKernel := hostKernelSettings()
		step("Creating kind cluster %q (%s)", cfg.ClusterName, kindNodeImageTag())
		if err := kindCreateCluster(cfg.ClusterName, kindCfg); err != nil {
			return err
		}
		warnHostKernelChanges(hostKernel)
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
	// A kind-<lab> context an earlier kind left in the default kubeconfig
	// carries the previous cluster's CA once the lab is recreated.
	if fresh, err := kindKubeconfig(cfg.ClusterName); err == nil {
		dropStaleKindContext(cfg.ClusterName, fresh)
	}

	step("Deploying Dex")
	// Namespace and TLS secret land before the Deployment so the pod never
	// waits on a missing volume on first boot.
	if err := ensureNamespace(componentDex); err != nil {
		return err
	}
	dexCert, dexKey := dexServingPair(cfg)
	if err := ensureSecretFromFiles(componentDex, "dex-tls", map[string]string{
		"tls.crt": dexCert,
		"tls.key": dexKey,
	}); err != nil {
		return err
	}
	sideloadDexImage(cfg, dexReady)
	// The bulk side-load starts only now, with the Dex image in the node: it
	// runs while Dex rolls out and the OIDC chain is verified, and is joined
	// before the platform install, which is what actually needs it. Started
	// any earlier it is what the Dex image queues behind — an import of
	// everything the last boot ran, minutes long — with the boot log stuck on
	// "Deploying Dex" meanwhile.
	loaded := loadLabImages(cfg, pulled)
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
		snapshotPreloadImages()
		offerTrustAndOpen(cfg, offers, false)
		return nil
	}
	snapshotPreloadImages()
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
	if cfg.VMManagerEnabled() {
		cmds = append(cmds, [2]string{"agentlab vm-manager-test", "vm-manager (the VM provisioner as a pod of the node) as the person: 401 anonymous -> get_host -> create_vm -> ready -> delete_vm, through muster"})
	}
	if cfg.Platform.Enabled && cfg.Platform.Agents {
		cmds = append(cmds, [2]string{"agentlab agents-test", "agent-manager as the caller: create -> ready -> update -> delete, a viewer refused, the ServiceAccount without RBAC"})
	}
	if cfg.WorkspacesEnabled() {
		cmds = append(cmds, [2]string{"agentlab workspaces-test --storage-only", "PVC -> snapshot -> restore, the controller endpoint refused without Substrate's certificate, an actor's volume across pause and resume"})
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
// throwaway ReplicaSet). The GitHub connector renders once its client Secret
// is in place (githubsignin.go), its version in the render, so placing or
// rotating the client secret rolls the pod too. Shared by Up, `agentlab
// platform` and `agentlab reload`.
func ApplyDex(cfg *config.Config) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	signIn, err := gitHubSignInFor(cfg)
	if err != nil {
		return err
	}
	stamped, _, err := renderManifestWith(cfg, "dex.yaml.tmpl", func(d *tmplData) { d.GitHubSignIn = signIn })
	if err != nil {
		return err
	}
	if _, err := applyManifests(context.Background(), stamped); err != nil {
		return err
	}
	if signIn != nil {
		note("GitHub sign-in on: connector %s with client %s; the App's callback URL is %s", signIn.ConnectorID, signIn.ClientID, signIn.RedirectURI)
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

// checkClusterIssuer refuses an existing cluster whose apiserver pins
// another issuer than the configuration's: the OIDC flags are fixed at
// `kind create`, so a platform.tls or dexPort change on a running lab would
// leave every token rejected behind a Dex that answers fine.
func checkClusterIssuer(cfg *config.Config) error {
	node := cfg.ControlPlaneNode()
	manifest, err := outputQuiet(dockerBin, "exec", node, "cat", "/etc/kubernetes/manifests/kube-apiserver.yaml")
	if err != nil {
		return fmt.Errorf("reading the apiserver's OIDC flags off node %s: %w", node, err)
	}
	issuer, ok := apiserverIssuer(manifest)
	if !ok {
		return fmt.Errorf("node %s's apiserver manifest carries no %s flag", node, oidcIssuerFlag)
	}
	if issuer != cfg.Issuer() {
		return fmt.Errorf("cluster %q was created for the issuer %s, the configuration's is %s (platform.tls moves it under platform.domain): the apiserver reads it only at creation, so recreate the lab (`agentlab down`, then `agentlab up`)", cfg.ClusterName, issuer, cfg.Issuer())
	}
	return nil
}

// checkClusterNodes refuses an existing cluster whose nodes are not the
// configuration's — a substrateNodes change on a running lab: kind fixes the
// nodes at `kind create`, and a values render pinning Substrate to workers
// the cluster lacks would leave atelet and every worker Pending.
func checkClusterNodes(cfg *config.Config) error {
	have, err := kindNodeNames(cfg.ClusterName)
	if err != nil {
		return err
	}
	want := append([]string{cfg.ControlPlaneNode()}, cfg.SubstrateNodeNames()...)
	slices.Sort(have)
	slices.Sort(want)
	if !slices.Equal(have, want) {
		return fmt.Errorf("cluster %q has the nodes %s, substrateNodes: %d wants %s: kind fixes the nodes at creation, so recreate the lab (`agentlab down`, then `agentlab up`)",
			cfg.ClusterName, strings.Join(have, ", "), cfg.SubstrateNodes, strings.Join(want, ", "))
	}
	return nil
}

const oidcIssuerFlag = "--oidc-issuer-url="

// apiserverIssuer is the --oidc-issuer-url value of a kube-apiserver
// static pod manifest.
func apiserverIssuer(manifest string) (string, bool) {
	for line := range strings.Lines(manifest) {
		if _, issuer, ok := strings.Cut(strings.TrimSpace(line), oidcIssuerFlag); ok {
			return issuer, true
		}
	}
	return "", false
}
