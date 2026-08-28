package lab

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentlab/internal/config"
)

const platformNamespace = "agent-platform"

// apsDir is where the umbrella chart is vendored from git. Deliberately NOT
// `vendor/`: the repo is a Go module, and a top-level vendor/ directory would
// flip the Go toolchain into vendored-build mode and break `go build`.
const apsDir = ".vendor/agent-platform-standalone"

// PlatformUp installs the Giant Swarm agent platform (muster + mcp-kubernetes)
// into the lab cluster and wires it to the lab Dex.
//
// The platform ships as agent-platform-standalone: one plain Helm umbrella
// chart with muster, valkey and the MCP registrations as pinned subcharts
// (Chart.lock is the BOM). No GitOps controller involved — unlike the old
// agent-platform meta-package, which rendered Flux HelmReleases and needed a
// helm-controller on the cluster before `helm install` did anything useful.
//
// The chart has no release yet (giantswarm/agent-platform-standalone#11), so
// it is vendored from git at a pinned SHA and installed from the local path.
// Once released: swap the vendor step for the OCI ref.
func PlatformUp(cfg *config.Config) error {
	chartDir := filepath.Join(apsDir, "helm", "agent-platform-standalone")

	// A cluster still running the old meta-package has Flux HelmReleases under
	// the same Helm release name; upgrading across that boundary races
	// helm-controller uninstalls against this install. Start clean instead.
	if _, err := outputQuiet("kubectl", "-n", platformNamespace, "get", "helmrelease", "muster"); err == nil {
		return fmt.Errorf("this cluster runs the old agent-platform meta-package (Flux HelmReleases found);\n" +
			"run `agentlab platform-down` first, then re-run `agentlab platform`")
	}

	step("Vendoring agent-platform-standalone @ %.12s", cfg.Platform.APSRef)
	if _, err := os.Stat(filepath.Join(apsDir, ".git")); os.IsNotExist(err) {
		if err := runQuiet("git", "init", "-q", apsDir); err != nil {
			return err
		}
		if err := runQuiet("git", "-C", apsDir, "remote", "add", "origin", cfg.Platform.APSRepo); err != nil {
			return err
		}
	}
	// Probe with stderr swallowed: a missing commit is the expected trigger
	// for the fetch, not an error worth showing.
	if _, err := outputQuiet("git", "-C", apsDir, "cat-file", "-e", cfg.Platform.APSRef+"^{commit}"); err != nil {
		if err := runQuiet("git", "-C", apsDir, "fetch", "-q", "--depth", "1", "origin", cfg.Platform.APSRef); err != nil {
			return err
		}
	}
	if err := runQuiet("git", "-C", apsDir, "-c", "advice.detachedHead=false", "checkout", "-q", cfg.Platform.APSRef); err != nil {
		return err
	}

	// Subchart .tgz pulls from gsoci/ghcr (anonymous). Skipped when charts/
	// was last built from exactly this Chart.lock — compared by content digest,
	// not mtime: mtimes change on checkout and prove nothing about the last
	// build.
	lockRaw, err := os.ReadFile(filepath.Join(chartDir, "Chart.lock"))
	if err != nil {
		return fmt.Errorf("reading Chart.lock: %w", err)
	}
	lockDigest := hex.EncodeToString(sha256sum(lockRaw))
	digestFile := filepath.Join(chartDir, "charts", ".lock-digest")
	if prev, err := os.ReadFile(digestFile); err != nil || string(prev) != lockDigest {
		step("Building chart dependencies")
		if err := runQuiet("helm", "dependency", "build", chartDir); err != nil {
			return err
		}
		if err := os.WriteFile(digestFile, []byte(lockDigest), 0o644); err != nil {
			return err
		}
	}

	step("Creating namespace and secrets")
	if err := ensureNamespace(platformNamespace); err != nil {
		return err
	}
	// muster appends this to its system trust pool so it can talk to the lab's
	// self-signed Dex over TLS (values: muster.muster.extraCaFile).
	if err := ensureSecretFromFiles(platformNamespace, "dex-ca", map[string]string{
		"ca.crt": "certs/ca.crt",
	}); err != nil {
		return err
	}
	// Created once and then left alone: regenerating the encryption key on
	// every run would invalidate every issued token. dex-client-secret must
	// match the `muster` staticClient in the Dex config.
	if _, err := outputQuiet("kubectl", "-n", platformNamespace, "get", "secret", "agent-platform-secrets"); err != nil {
		if err := runQuiet("kubectl", "-n", platformNamespace, "create", "secret", "generic", "agent-platform-secrets",
			"--from-literal=dex-client-secret="+config.MusterClientSecret,
			"--from-literal=registration-token="+randHex(32),
			"--from-literal=oauth-encryption-key="+randBase64(32),
			"--from-literal=valkey-password="+randHex(16)); err != nil {
			return err
		}
		note("created agent-platform-secrets")
	} else {
		note("agent-platform-secrets already exists, leaving it alone")
	}

	if _, _, err := renderManifest(cfg, "mcp-kubernetes-values.yaml.tmpl"); err != nil {
		return err
	}
	if _, _, err := renderManifest(cfg, "agent-platform-values.yaml.tmpl"); err != nil {
		return err
	}
	if _, _, err := renderManifest(cfg, "demo-workflow.yaml.tmpl"); err != nil {
		return err
	}

	step("Deploying mcp-kubernetes (%s)", cfg.Platform.MCPKubernetesVersion)
	if err := runQuiet("helm", "upgrade", "--install", "mcp-kubernetes",
		"oci://gsoci.azurecr.io/charts/giantswarm/mcp-kubernetes",
		"--version", cfg.Platform.MCPKubernetesVersion, "-n", platformNamespace,
		"-f", StateDir+"/mcp-kubernetes-values.yaml",
		"--wait", "--timeout", "5m"); err != nil {
		return err
	}

	step("Installing agent-platform-standalone (this waits for every workload)")
	// --wait replaces the old HelmRelease polling: Helm itself owns the
	// workloads now. The MCPServer/Workflow CRDs ship in the muster subchart's
	// crds/ dir, which Helm applies before the manifests on first install.
	// The post-renderer is this very binary (see PostRender): hostNetwork,
	// the DCR chart-bug workaround, HTTPRoute strip.
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := runQuiet("helm", "upgrade", "--install", "agent-platform", chartDir,
		"-n", platformNamespace,
		"-f", StateDir+"/agent-platform-values.yaml",
		"--post-renderer", self, "--post-renderer-args", "post-render",
		"--wait", "--timeout", "10m"); err != nil {
		return err
	}

	step("Waiting for muster to connect to the Kubernetes MCP")
	mcpName := cfg.MCPServerName()
	state := ""
	connected := waitFor(40, 3*time.Second, func() bool {
		state, _ = outputQuiet("kubectl", "-n", platformNamespace, "get", "mcpserver", mcpName,
			"-o", "jsonpath={.status.state}")
		return state == "Connected"
	})
	if !connected {
		return fmt.Errorf("MCPServer %s never reached Connected (last state: %q);\n"+
			"check `agentlab logs muster` and `kubectl -n %s describe mcpserver %s`",
			mcpName, state, platformNamespace, mcpName)
	}
	note("MCPServer %s: Connected", mcpName)

	// The Workflow CRD ships with muster, so this has to land after the
	// install. A muster with no workflows leaves the Backstage muster plugin's
	// main tab empty, which reads as "the plugin is broken" rather than
	// "nothing to show".
	step("Creating the demo workflow")
	if err := runQuiet("kubectl", "apply", "-f", StateDir+"/demo-workflow.yaml"); err != nil {
		return err
	}

	// muster runs with hostNetwork and the kind config maps its port onto the
	// host, so it should be reachable directly. Retry rather than probing
	// once: helm --wait covers the Deployment, but muster's HTTP listener
	// accepts slightly later, so a single-shot check on a cold cluster fails
	// and then blames the port mapping, which is the one thing that is
	// definitely fine on a freshly created cluster.
	step("Waiting for muster to serve on %s", cfg.MusterBaseURL())
	client, err := labHTTPClient(3 * time.Second)
	if err != nil {
		return err
	}
	reachable := waitFor(40, 3*time.Second, func() bool {
		return httpUp(client, cfg.MusterBaseURL()+"/.well-known/oauth-authorization-server")
	})

	reach := fmt.Sprintf("muster is live on %s (no port-forward needed)", cfg.MusterBaseURL())
	if !reachable {
		reach = fmt.Sprintf(`muster is NOT reachable on %s after 2 minutes.
  If 'docker port %s' shows no %d line, this cluster
  predates the port mapping and must be recreated (agentlab down && agentlab up).
  Otherwise check 'agentlab logs muster'. Stopgap either way:
  kubectl -n %s port-forward svc/muster %d:%d`,
			cfg.MusterBaseURL(), cfg.ControlPlaneNode(), cfg.Platform.MusterPort,
			platformNamespace, cfg.Platform.MusterPort, config.MusterNodePort)
	}

	fmt.Printf(`
Platform is up.
  %s

  Point Claude Code at it (browser login through Dex):
    claude mcp add --transport http muster %s/mcp
    # then in Claude Code: /mcp -> authenticate

  Smoke-test it headlessly instead:
    agentlab platform-test
`, reach, cfg.MusterBaseURL())
	return nil
}

func sha256sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

func randBase64(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(buf)
}
