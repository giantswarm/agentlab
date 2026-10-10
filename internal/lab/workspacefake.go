package lab

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/giantswarm/agentlab/internal/config"
)

// The lab's GitHub as the workspace-manager's provider instance fake
// (platform.workspaces.provider, the default): `agentlab github-fake
// --workspaces` (githubfake_workspaces.go) in a container on the kind
// network, the way a proof's fake runs (fakecontainer.go), kept with the lab
// rather than with a proof: `agentlab platform` starts it afresh (the fixture
// is its state), `agentlab platform-down` and `agentlab down` remove it. Pods
// reach it as https://github.<domain> (config.GitHubFakeHost): CoreDNS sends
// the name to the selector-less Service agentlab-github in the platform
// namespace (coredns.yaml.tmpl), whose EndpointSlice is the container's address, and
// the fake's TLS leaf (the lab CA's, certs/github-fake) carries the name, so
// a client that trusts the lab CA verifies it; this host reaches it on the
// loopback port the container publishes. The instance the values render is a
// GitHub Enterprise-shaped `github` kind (workspaceprovider.go): the embedded
// fixture's App, whose private key and client secret the lab generated
// itself and places as the Secret agent-platform/workspace-fake, referenced
// by the values and read by nothing else.

const (
	// labGitHubService is the Service pods reach the lab's GitHub through, on
	// 443 like a GitHub, and the CoreDNS rewrite's target (the proofs' own
	// fakes have Services of their own, githubFakeService among them).
	labGitHubService = "agentlab-github"
	// labGitHubContainerSuffix names the container <cluster>-lab-github.
	labGitHubContainerSuffix = "lab-github"
	// labGitHubServicePort is the Service's port: the instance's URL names
	// none, as a GitHub Enterprise Server's does not.
	labGitHubServicePort = 443
	// githubFakeCredentialsMount is where the container reads the
	// credentials directory, githubFakeDataDir where it keeps the bare
	// repositories (the image's /tmp: the container runs as the caller).
	githubFakeCredentialsMount = "/credentials"
	githubFakeDataDir          = "/tmp/github-fake"
)

// githubFakeUp runs the lab's GitHub for the provider instance fake: the
// credentials generated, the container started (a running one replaced: its
// repositories are the fixture's again), the Service pointed at it and a pod
// proven to reach it by name, and the App's key and client secret placed as
// the instance's Secret ahead of the install, which references it.
func githubFakeUp(cfg *config.Config) error {
	step("Starting the lab's GitHub for the workspace-manager's provider instance %s (%s)", config.WorkspaceProviderFake, cfg.GitHubFakeURL())
	dir, err := EnsureGitHubFakeCredentials(cfg.Platform.Domain)
	if err != nil {
		return err
	}
	credentials, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	binary, err := labGitHubBinary()
	if err != nil {
		return err
	}
	c, err := startFakeContainer(cfg, binary, fakeContainerSpec{
		what:       "the lab's GitHub",
		suffix:     labGitHubContainerSuffix,
		command:    githubFakeCommand,
		healthPath: githubFakeHealthPath,
		// The fake serves git's smart HTTP with git itself; the lab's probe
		// image carries none.
		image:  workspacesTestGitImage,
		args:   []string{"--workspaces", "--credentials", githubFakeCredentialsMount, "--data-dir", githubFakeDataDir},
		mounts: []string{credentials + ":" + githubFakeCredentialsMount + ":ro"},
		tls:    true,
	})
	if err != nil {
		return err
	}
	note("the lab's GitHub on %s (this host) and %s (pods); its request log at %s%s", c.hostURL, cfg.GitHubFakeURL(), c.hostURL, githubFakeRequestsPath)
	if _, err := fakeServiceForPods(cfg, labGitHubService, "the lab's GitHub", c.podIP, fakeContainerPort, labGitHubServicePort,
		cfg.GitHubFakeURL()+githubFakeHealthPath); err != nil {
		return err
	}
	return ensureSecretFromFiles(platformNamespace, config.WorkspaceFakeSecretName, map[string]string{
		config.WorkspaceGitHubPrivateKeyKey: filepath.Join(credentials, githubFakeAppKey),
		config.GitHubClientSecretKey:        filepath.Join(credentials, githubFakeClientSecret),
	})
}

// labGitHubBinary is the binary the lab's GitHub container runs: a copy of
// this one under state/, written beside its name and renamed into place.
// The container keeps the executable it mounts busy for as long as it runs,
// and the lab's binary is often the build output in the lab's directory:
// mounted as it is, `make build` would be refused (text file busy) while the
// lab is up. The rename leaves a running container its own inode.
func labGitHubBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating this binary for the lab's GitHub container: %w", err)
	}
	dir := filepath.Join(StateDir, labGitHubContainerSuffix)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(exe) // #nosec G304 -- this binary
	if err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, "agentlab.next")
	if err := os.WriteFile(tmp, raw, 0o750); err != nil { // #nosec G306 G703 -- an executable, to a lab-owned path under state/
		return "", err
	}
	binary := filepath.Join(dir, "agentlab")
	if err := os.Rename(tmp, binary); err != nil {
		return "", err
	}
	return filepath.Abs(binary)
}

// githubFakeDown removes the lab's GitHub container; the Service and the
// Secret go with the platform namespace, or with the chart's next render
// without the instance. Best effort: nothing of it may exist.
func githubFakeDown(cfg *config.Config) {
	(&fakeContainer{name: cfg.ClusterName + "-" + labGitHubContainerSuffix}).close()
}

// githubFakeServiceDown removes the fake's Service from a platform that stays
// up without the instance, so nothing resolves to a container that is gone.
func githubFakeServiceDown(ctx context.Context) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	return removeFakeService(ctx, k, labGitHubService)
}
