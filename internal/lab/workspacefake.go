package lab

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The lab's GitHub as the workspace-manager's provider instance fake
// (platform.workspaces.provider, the default): `agentlab github-fake
// --workspaces` (githubfake_workspaces.go) in a container on the kind
// network, the way a proof's fake runs (fakecontainer.go), kept with the lab
// rather than with a proof: `agentlab platform` starts it afresh (the fixture
// is its state) but for the users' tokens it issued, which it keeps in
// state/lab-github/tokens across the replacement, so a person's provider
// sign-in outlives the run; `agentlab platform-down` and `agentlab down`
// remove it. Pods
// reach it as https://github.<domain> (config.GitHubFakeHost): CoreDNS sends
// the name to the selector-less Service agentlab-github in the platform
// namespace (coredns.yaml.tmpl), whose EndpointSlice is the container's address, and
// the fake's TLS leaf (the lab CA's, certs/github-fake) carries the name, so
// a client that trusts the lab CA verifies it; this host reaches it on the
// loopback port the container publishes. The instance the values render is a
// GitHub Enterprise-shaped `github` kind (workspaceprovider.go), and with
// fake-bearer configured a github.com-shaped one too, its git host
// github-bearer.<domain> and its API host api.github-bearer.<domain> names of
// the same container (CoreDNS and the leaf carry them): the embedded
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
	// githubFakeTokensMount is where the container keeps the users' tokens
	// (githubFakeTokenStore), a directory of this host's under state/ that
	// outlives the container.
	githubFakeTokensMount = "/tokens"
	githubFakeTokenStore  = githubFakeTokensMount + "/user-tokens.json"
	// labGitHubWhat names the container in the refusal of a binary it cannot
	// run: the instance's name, so a reader finds the knob in agentlab.yaml.
	labGitHubWhat = "the lab's GitHub for the workspace-manager's provider instance " + config.WorkspaceProviderFake
)

// checkLabGitHubBinary refuses this binary ahead of the first install step
// when the lab's GitHub is to run it: startFakeContainer reaches the same
// verdict on the copy labGitHubBinary makes, but only after the storage
// install, and the minutes before it would be lost to a rebuild. Nothing to
// judge without the instance.
func checkLabGitHubBinary(cfg *config.Config) error {
	if !cfg.WorkspaceFakeProvider() {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this binary for the lab's GitHub container: %w", err)
	}
	return linuxStaticBinary(exe, labGitHubWhat, "")
}

// githubFakeUp runs the lab's GitHub for the provider instance fake: the
// credentials generated, the container started (a running one replaced: its
// repositories are the fixture's again, the users' tokens it issued kept),
// the Service pointed at it and a pod
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
	tokens, err := filepath.Abs(filepath.Join(StateDir, labGitHubContainerSuffix, "tokens"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(tokens, 0o700); err != nil {
		return err
	}
	c, err := startFakeContainer(cfg, binary, fakeContainerSpec{
		what:       labGitHubWhat,
		suffix:     labGitHubContainerSuffix,
		command:    githubFakeCommand,
		healthPath: githubFakeHealthPath,
		// The fake serves git's smart HTTP with git itself; the lab's probe
		// image carries none.
		image: workspacesTestGitImage,
		args: []string{"--workspaces", "--credentials", githubFakeCredentialsMount, "--data-dir", githubFakeDataDir,
			"--api-host", config.GitHubFakeBearerAPIHost(cfg.Platform.Domain), "--token-store", githubFakeTokenStore},
		mounts: []string{credentials + ":" + githubFakeCredentialsMount + ":ro", tokens + ":" + githubFakeTokensMount},
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

// labGitHubClient drives the lab's running GitHub from this host the way a
// person or an App would drive GitHub: the request log, the App's
// installation tokens, a repository's topics and git, over the loopback port
// the container publishes, trusting the lab CA.
type labGitHubClient struct {
	url    string
	client *http.Client
	caPath string
	appKey *rsa.PrivateKey
	appID  int64
}

// errNoInstallation is the App not installed on an owner.
var errNoInstallation = errors.New("the App is not installed on the owner")

// openLabGitHub reaches the lab's GitHub container and reads the App's key
// the lab generated for it (certs/github-fake), proving the fake answers.
func openLabGitHub(cfg *config.Config) (*labGitHubClient, error) {
	name := cfg.ClusterName + "-" + labGitHubContainerSuffix
	published, err := outputQuiet(dockerBin, "port", name, strconv.Itoa(fakeContainerPort)+"/tcp")
	hostPort := publishedLoopback(published)
	if err != nil || hostPort == "" {
		return nil, fmt.Errorf("the lab's GitHub, container %s, publishes no port on this host: `agentlab platform` starts it for the provider instance %s", name, config.WorkspaceProviderFake)
	}
	client, err := labHTTPClient(30 * time.Second)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(githubFakeCredentialsDir, githubFakeAppKey))
	if err != nil {
		return nil, fmt.Errorf("reading the App's key of the lab's GitHub: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s holds no PEM block", filepath.Join(githubFakeCredentialsDir, githubFakeAppKey))
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the App's key of the lab's GitHub: %w", err)
	}
	fixture, err := loadGitHubFixture("")
	if err != nil {
		return nil, err
	}
	caPath, err := filepath.Abs(caCertPath)
	if err != nil {
		return nil, err
	}
	g := &labGitHubClient{url: "https://" + hostPort, client: client, caPath: caPath, appKey: key, appID: fixture.App.ID}
	if status, err := g.do(http.MethodGet, githubFakeHealthPath, "", nil, nil); err != nil || status != http.StatusOK {
		return nil, fmt.Errorf("the lab's GitHub on %s does not answer its health (%d): %v", g.url, status, err)
	}
	return g, nil
}

// do sends method path with the credential as a bearer ("" for none) and
// the JSON body, decoding a 2xx answer into out when it is not nil.
func (g *labGitHubClient) do(method, path, credential string, body, out any) (int, error) {
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, g.url+path, reader)
	if err != nil {
		return 0, err
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s on the lab's GitHub: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s on the lab's GitHub: decoding the answer: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// requests is the fake's request log.
func (g *labGitHubClient) requests() ([]githubRequest, error) {
	var out []githubRequest
	if status, err := g.do(http.MethodGet, githubFakeRequestsPath, "", nil, &out); err != nil || status != http.StatusOK {
		return nil, fmt.Errorf("reading the request log of the lab's GitHub (%d): %v", status, err)
	}
	return out, nil
}

// pulls are the pull requests opened on the fake's repositories.
func (g *labGitHubClient) pulls() ([]workspacePull, error) {
	var out []workspacePull
	if status, err := g.do(http.MethodGet, githubFakePullsPath, "", nil, &out); err != nil || status != http.StatusOK {
		return nil, fmt.Errorf("reading the pull requests of the lab's GitHub (%d): %v", status, err)
	}
	return out, nil
}

// appJWT is a fresh JWT of the App, for the calls GitHub admits with it only.
func (g *labGitHubClient) appJWT() (string, error) {
	return githubAppJWT(g.appKey, g.appID, time.Now().Add(-githubClockSkew/2), githubAppJWTMaxTTL-githubClockSkew)
}

// installationID is the id of the App's installation on owner;
// errNoInstallation where the App is not installed.
func (g *labGitHubClient) installationID(owner, jwt string) (int64, error) {
	var installation struct {
		ID int64 `json:"id"`
	}
	status, err := g.do(http.MethodGet, githubFakeAPIPath+"/orgs/"+owner+"/installation", jwt, nil, &installation)
	switch {
	case err != nil:
		return 0, err
	case status == http.StatusNotFound:
		return 0, fmt.Errorf("%w %s", errNoInstallation, owner)
	case status != http.StatusOK:
		return 0, fmt.Errorf("the App's installation on %s answered %d", owner, status)
	}
	return installation.ID, nil
}

// installationToken is a token of the App's installation on owner, which
// writes to the owner's repositories; errNoInstallation where the App is not
// installed.
func (g *labGitHubClient) installationToken(owner string) (string, error) {
	jwt, err := g.appJWT()
	if err != nil {
		return "", err
	}
	id, err := g.installationID(owner, jwt)
	if err != nil {
		return "", err
	}
	var token struct {
		Token string `json:"token"`
	}
	status, err := g.do(http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", githubFakeAPIPath, id), jwt, nil, &token)
	if err != nil || status != http.StatusCreated || token.Token == "" {
		return "", fmt.Errorf("a token of the App's installation on %s (%d): %v", owner, status, err)
	}
	return token.Token, nil
}

// setSuspended suspends (on) or unsuspends the App's installation on owner,
// as GitHub's PUT and DELETE /app/installations/{id}/suspended do.
func (g *labGitHubClient) setSuspended(owner string, on bool) error {
	jwt, err := g.appJWT()
	if err != nil {
		return err
	}
	id, err := g.installationID(owner, jwt)
	if err != nil {
		return err
	}
	method := http.MethodPut
	if !on {
		method = http.MethodDelete
	}
	status, err := g.do(method, fmt.Sprintf("%s/app/installations/%d/suspended", githubFakeAPIPath, id), jwt, nil, nil)
	if err != nil || status != http.StatusNoContent {
		return fmt.Errorf("%s of the App's installation on %s answered %d: %v", method, owner, status, err)
	}
	return nil
}

// labGitHubInstallation is one of the App's installations as GET
// /app/installations answers it.
type labGitHubInstallation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
	SuspendedAt *string `json:"suspended_at"`
	SuspendedBy *struct {
		Login string `json:"login"`
	} `json:"suspended_by"`
}

// installations are the App's installations.
func (g *labGitHubClient) installations() ([]labGitHubInstallation, error) {
	jwt, err := g.appJWT()
	if err != nil {
		return nil, err
	}
	var out []labGitHubInstallation
	if status, err := g.do(http.MethodGet, githubFakeAPIPath+"/app/installations", jwt, nil, &out); err != nil || status != http.StatusOK {
		return nil, fmt.Errorf("the App's installations answered %d: %v", status, err)
	}
	return out, nil
}

// SuspendLabGitHubInstallation suspends (on) or unsuspends the App's
// installation on owner in the lab's GitHub: the lab's stand-in for an
// organization owner's Suspend on the App's installation page, the
// kill-switch runbook's first switch. Afterwards it prints the
// installations, which show it.
func SuspendLabGitHubInstallation(cfg *config.Config, owner string, on bool) error {
	g, err := openLabGitHub(cfg)
	if err != nil {
		return err
	}
	if err := g.setSuspended(owner, on); err != nil {
		return err
	}
	return printLabGitHubInstallations(g)
}

// LabGitHubInstallations prints the App's installations in the lab's
// GitHub: id, owner, and when and by whom each suspended one was.
func LabGitHubInstallations(cfg *config.Config) error {
	g, err := openLabGitHub(cfg)
	if err != nil {
		return err
	}
	return printLabGitHubInstallations(g)
}

func printLabGitHubInstallations(g *labGitHubClient) error {
	installations, err := g.installations()
	if err != nil {
		return err
	}
	for _, i := range installations {
		state := "active"
		if i.SuspendedAt != nil {
			state = "suspended at " + *i.SuspendedAt
			if i.SuspendedBy != nil {
				state += " by " + i.SuspendedBy.Login
			}
		}
		fmt.Printf("%d\t%s\t%s\n", i.ID, i.Account.Login, state)
	}
	return nil
}

// topics are the repository's topics.
func (g *labGitHubClient) topics(fullName, token string) ([]string, error) {
	var out githubTopics
	status, err := g.do(http.MethodGet, githubFakeAPIPath+"/repos/"+fullName+"/topics", token, nil, &out)
	switch {
	case err != nil:
		return nil, err
	case status == http.StatusNotFound:
		return nil, fmt.Errorf("the lab's GitHub answers 404 for the topics of %s: it runs an agentlab that predates the topics endpoint; `agentlab platform` with this agentlab starts it afresh", fullName)
	case status != http.StatusOK:
		return nil, fmt.Errorf("the topics of %s answered %d", fullName, status)
	}
	return out.Names, nil
}

// setTopics replaces the repository's topics, as GitHub's
// PUT /repos/{owner}/{repo}/topics does.
func (g *labGitHubClient) setTopics(fullName, token string, topics []string) error {
	if status, err := g.do(http.MethodPut, githubFakeAPIPath+"/repos/"+fullName+"/topics", token, githubTopics{Names: topics}, nil); err != nil || status != http.StatusOK {
		return fmt.Errorf("replacing the topics of %s with %v (%d): %v", fullName, topics, status, err)
	}
	return nil
}

// addGeneratedRepo adds a public repository of mib generated MiB to the
// lab's GitHub (githubfake_generated.go) and returns its size in KiB, as
// GitHub's `size` reports it. Seeding a large one takes minutes: the call
// waits for it.
func (g *labGitHubClient) addGeneratedRepo(fullName string, mib int, language string) (int64, error) {
	client := *g.client
	client.Timeout = 30 * time.Minute
	slow := *g
	slow.client = &client
	var out struct {
		Size int64 `json:"size"`
	}
	status, err := slow.do(http.MethodPut, githubFakeReposPath+"/"+fullName, "", generatedRepoRequest{GeneratedMiB: mib, Language: language}, &out)
	switch {
	case err != nil:
		return 0, err
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		return 0, fmt.Errorf("the lab's GitHub answers %d for %s: it runs an agentlab that predates generated repositories; `agentlab platform` with this agentlab starts it afresh", status, githubFakeReposPath)
	case status != http.StatusOK && status != http.StatusCreated:
		return 0, fmt.Errorf("adding the generated repository %s of %d MiB answered %d", fullName, mib, status)
	}
	return out.Size, nil
}

// removeGeneratedRepo removes a repository addGeneratedRepo added; one
// already gone is no error.
func (g *labGitHubClient) removeGeneratedRepo(fullName string) error {
	status, err := g.do(http.MethodDelete, githubFakeReposPath+"/"+fullName, "", nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusNotFound {
		return fmt.Errorf("removing the generated repository %s answered %d", fullName, status)
	}
	return nil
}

// remote is the repository's git URL on this host.
func (g *labGitHubClient) remote(fullName string) string { return g.url + "/" + fullName + ".git" }

// git runs git in dir against the lab's GitHub as the token's holder ("" for
// anonymous): the lab CA trusted, none of this host's configuration, and the
// token handed to git by a credential helper from the environment, never on
// a command line.
func (g *labGitHubClient) git(dir, token string, args ...string) (string, error) {
	base := []string{"-c", "http.sslCAInfo=" + g.caPath, "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main",
		"-c", "user.name=agentlab workspaces-test", "-c", "user.email=lab@agentlab.local", "-c", "credential.helper=",
		"-c", `credential.helper=!f() { test "$1" = get || exit 0; echo username=x-access-token; echo "password=$AGENTLAB_GIT_TOKEN"; }; f`}
	cmd := exec.Command("git", append(base, args...)...) // #nosec G204 -- git with the proof's own arguments
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "AGENTLAB_GIT_TOKEN="+token)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, excerpt(strings.TrimSpace(string(out)), 300))
	}
	return strings.TrimSpace(string(out)), nil
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
