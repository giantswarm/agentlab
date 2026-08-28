package lab

import (
	"fmt"
	"slices"
	"time"

	"agentlab/internal/config"
)

// BackstageUp deploys Giant Swarm's Backstage into the lab and points it at
// Dex as its only identity provider. This is the GS build (not upstream, not
// RHDH) because it carries the first-party `muster` plugin that drives the
// agent platform.
func BackstageUp(cfg *config.Config) error {
	if !kindClusterExists(cfg.ClusterName) {
		return fmt.Errorf("the %s cluster is not running -- run `agentlab up` first", cfg.ClusterName)
	}

	image := cfg.Backstage.Image

	// Exact repo:tag match — a substring check would accept any already-loaded
	// tag and silently keep running an old image after an image bump.
	if !nodeHasImage(cfg.ControlPlaneNode(), image) {
		step("Loading %s into the kind node (2.4GB, takes a few minutes)", image)
		// gsoci serves this anonymously -- no registry credentials needed. The
		// image is linux/amd64 only, so an arm64 Mac must ask for that platform
		// explicitly and the kind node runs it through its Rosetta binfmt
		// handler.
		if _, err := outputQuiet("docker", "image", "inspect", image); err != nil {
			if err := run("docker", "pull", "--platform", "linux/amd64", image); err != nil {
				return err
			}
		}
		if err := run("kind", "load", "docker-image", image, "--name", cfg.ClusterName); err != nil {
			return err
		}
	}

	step("Deploying Backstage")
	// Namespace and CA secret land before the Deployment so the pod never
	// waits on a missing volume. The checksum stamped into the pod template
	// covers the lab CA and the whole rendered manifest (ConfigMaps included),
	// so a config or cert change rolls the pod while an unchanged re-run stays
	// a no-op — no blanket restart.
	if err := ensureNamespace("backstage"); err != nil {
		return err
	}
	if err := ensureSecretFromFiles("backstage", "dex-ca", map[string]string{
		"ca.crt": "certs/ca.crt",
	}); err != nil {
		return err
	}
	// The ai-chat plugin's Anthropic key. The Deployment references it as an
	// optional env secret, so Backstage boots without it — but env vars are
	// read at container start, so a key that arrives on a re-run (manifest
	// unchanged, checksum no-op) needs the one conditional roll below.
	keyCreated, err := ensureAnthropicSecret("backstage", "backstage-anthropic")
	if err != nil {
		return err
	}
	_, getDeployErr := outputQuiet("kubectl", "-n", "backstage", "get", "deployment", "backstage")
	deployExisted := getDeployErr == nil
	stamped, _, err := renderManifest(cfg, "backstage.yaml.tmpl")
	if err != nil {
		return err
	}
	if err := pipeInto(stamped, "kubectl", "apply", "-f", "-"); err != nil {
		return err
	}
	if keyCreated && deployExisted {
		step("Rolling Backstage to pick up the new Anthropic key")
		if err := runQuiet("kubectl", "-n", "backstage", "rollout", "restart", "deployment/backstage"); err != nil {
			return err
		}
	}

	step("Waiting for Backstage to start (first boot takes a minute under emulation)")
	if err := run("kubectl", "-n", "backstage", "rollout", "status", "deployment/backstage", "--timeout=300s"); err != nil {
		return err
	}

	step("Waiting for %s", cfg.BackstageBaseURL())
	client, err := labHTTPClient(5 * time.Second)
	if err != nil {
		return err
	}
	if !waitFor(120, 2*time.Second, func() bool { return httpUp(client, cfg.BackstageBaseURL()) }) {
		return fmt.Errorf("backstage never answered on %s", cfg.BackstageBaseURL())
	}

	fmt.Printf(`
Backstage is up: %s

  Click "Sign In" -> you land on the Dex login page.
  Log in with any user from %s.

  The muster plugin lives under "Agent Platform" -> "MCP Servers".

  Logs:  agentlab logs backstage
`, cfg.BackstageBaseURL(), config.File)
	return nil
}

// prefetchBackstageImage pulls the Backstage image into the local docker
// cache in the background, so Up can overlap the multi-minute pull with the
// platform install; BackstageUp then finds the image cached and only loads it
// into the node. The buffered channel yields the pull result.
func prefetchBackstageImage(cfg *config.Config) <-chan error {
	image := cfg.Backstage.Image
	done := make(chan error, 1)
	if nodeHasImage(cfg.ControlPlaneNode(), image) {
		done <- nil
		return done
	}
	if _, err := outputQuiet("docker", "image", "inspect", image); err == nil {
		done <- nil
		return done
	}
	note("pulling %s in the background", image)
	// -q: progress bars would garble the platform install's output.
	go func() { done <- runQuiet("docker", "pull", "--platform", "linux/amd64", "-q", image) }()
	return done
}

// nodeHasImage checks the kind node's containerd for an exact repo:tag match.
func nodeHasImage(node, image string) bool {
	tags, err := nodeImageTags(node)
	if err != nil {
		return false
	}
	return slices.Contains(tags, image)
}
