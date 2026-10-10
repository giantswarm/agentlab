package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// pm-test (docs/platform-manager.md): the released giantswarm-platform-manager
// plans against the lab's platform-manager fixture, an invented installation
// with the agent platform and workspaces on (templates/platform-manager-fixture.yaml).
//
// The registry is the lab's (githubfake_registry.go), a container on the kind
// network the manager's pod reaches through a Service. The manager is its
// published chart and image at platform.platformManager.version, a temporary
// HelmRelease in the platform namespace through the platform's bundled
// engine, pinned to the lab Dex as its authorization server — the shape of
// the manager's own lab values (tests/lab-oauth-values.yaml), the sign-in
// completing headlessly as the commit proofs' does — and to the fixture as
// its GitHub: the bearer muster puts on every call is the person's Dex token,
// which the fixture answers GET /user for. The proof signs the person in
// through muster, runs reconcile_capability's dry run for the installation,
// asserts the workspaces-specific changes, and removes the release and the
// fixture.

// The fixture's names and what its plan must carry.
const (
	pmRelease      = "agentlab-platform-manager"
	pmMCPServer    = "giantswarm-platform-manager"
	pmGitHubSuffix = "pm-github"
	pmGitHubSvc    = "agentlab-pm-github"
	pmGitHubHost   = "http://" + pmGitHubSvc + "." + platformNamespace + ".svc.cluster.local"
	pmRegistryRepo = "agentlab/registry"
	pmInstallation = "ember"
	pmCapability   = "agent-platform"
	// pmSignInURI is the workspace-manager's browser sign-in the plan must
	// register on the platform's Dex client: <base URL>/signin, the base URL
	// the chart derives from the installation's domain.
	pmSignInURI = "https://workspace-manager." + pmInstallation + ".agentlab.test/signin"
	// pmAppIssuer is the pinned App's issuer identity, the key muster files
	// the grant under; nothing fetches it.
	pmAppIssuer    = "https://github.agentlab.invalid/apps/" + pmMCPServer
	pmChartURL     = "oci://gsoci.azurecr.io/charts/giantswarm/giantswarm-platform-manager"
	pmReleaseWait  = 5 * time.Minute
	pmConfigsPatch = "installations/" + pmInstallation + "/apps/"
	pmValuesPatch  = pmConfigsPatch + "agent-platform/configmap-values.yaml.patch"
	pmDexPatch     = pmConfigsPatch + "dex-app/configmap-values.yaml.patch"
)

// PMTestOptions are pm-test's flags.
type PMTestOptions struct {
	// GitHubFakeBinary is the static Linux agentlab the registry's container
	// runs (default: this binary).
	GitHubFakeBinary string
}

// PMTest is the headless proof of the platform-manager fixture as the lab
// user email.
func PMTest(cfg *config.Config, email string, opts PMTestOptions) error {
	if !cfg.Platform.Enabled || !cfg.Platform.PlatformManager.Enabled {
		return fmt.Errorf("platform.platformManager is off in %s (needs the platform too): set platform.platformManager.enabled: true", config.File)
	}
	version := cfg.PlatformManagerVersion()
	if _, err := semver.StrictNewVersion(version); err != nil {
		return fmt.Errorf("platform.platformManager.version %q is not a release version: %w", version, err)
	}
	if opts.GitHubFakeBinary == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locating this binary for the registry's container: %w (pass --github-fake-binary)", err)
		}
		opts.GitHubFakeBinary = exe
	}
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}

	step("The platform manager's registry: %s %s %s as a container on the %s network", opts.GitHubFakeBinary, githubFakeCommand, registryFakeFlag, kindDockerNetwork)
	fake, err := startFakeContainer(cfg, opts.GitHubFakeBinary, fakeContainerSpec{
		what: "the platform manager's registry", suffix: pmGitHubSuffix, command: githubFakeCommand,
		binaryFlag: "--github-fake-binary", healthPath: githubFakeHealthPath, args: []string{registryFakeFlag},
	})
	if err != nil {
		return err
	}
	defer fake.close()
	removeService, err := fakeServiceForPods(cfg, pmGitHubSvc, "the platform manager's registry", fake.podIP, fakeContainerPort, fakeServicePort, pmGitHubHost+githubFakeHealthPath)
	if err != nil {
		return err
	}
	defer removeService()

	step("HelmRelease %s: giantswarm-platform-manager %s (%s), pinned to the lab Dex as its App and to %s%s as its GitHub", pmRelease, version, pmChartURL, pmGitHubHost, githubFakeAPIPath)
	uninstall, err := installPlatformManager(cfg, version)
	if err != nil {
		return err
	}
	defer uninstall()
	if err := waitMCPServerState(pmMCPServer, mcpServerStateAuthRequired, mcpServerStateConnected); err != nil {
		return err
	}

	step("Logging in to Dex as %s and signing in to %s through muster", email, pmMCPServer)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	session, err := openMusterSession(cfg, token, "pm-test")
	if err != nil {
		return err
	}
	defer func() { _, _ = session.callToolEnvelope("core_auth_logout", map[string]any{serverKey: pmMCPServer}) }()
	challenge, err := signInChallenge(cfg, session, pmMCPServer)
	if err != nil {
		return err
	}
	if err := completeSignIn(challenge.authURL, user); err != nil {
		return err
	}

	var info struct {
		Version string `json:"version"`
		Caller  struct {
			Login string `json:"login"`
		} `json:"caller"`
	}
	if err := pmCall(session, "get_info", nil, &info); err != nil {
		return err
	}
	if strings.TrimPrefix(info.Version, "v") != version {
		return fmt.Errorf("the manager answers version %q, the lab installed %s", info.Version, version)
	}
	note("%s %s answers as %s", pmMCPServer, info.Version, info.Caller.Login)

	step("reconcile_capability %s on %s, dry run", pmCapability, pmInstallation)
	var plan pmDryRun
	if err := pmCall(session, "reconcile_capability", map[string]any{
		"installations": []string{pmInstallation}, "capability": pmCapability, "dryRun": true, "content": true,
	}, &plan); err != nil {
		return err
	}
	if err := plan.assertWorkspaces(); err != nil {
		return err
	}
	for _, line := range plan.proofLines() {
		note("%s", line)
	}
	if misses, err := fake.registryMisses(); err != nil {
		return err
	} else if writes := slices.DeleteFunc(slices.Clone(misses), func(m string) bool { return strings.HasPrefix(m, "GET ") }); len(writes) > 0 {
		return fmt.Errorf("the dry run wrote to the registry: %s", strings.Join(writes, ", "))
	} else if len(misses) > 0 {
		note("the registry answered 404 for %d read(s) beyond the fixture: %s", len(misses), strings.Join(misses, ", "))
	}
	fmt.Printf("\n✅ pm-test: giantswarm-platform-manager %s plans %s on %s with workspaces on: the Dex client carries %s, the workspace-manager values are kept, nothing written\n",
		version, pmCapability, pmInstallation, pmSignInURI)
	return nil
}

// pmCall runs one of the manager's tools through muster and decodes its JSON
// answer into out.
func pmCall(s *musterSession, tool string, args map[string]any, out any) error {
	text, err := s.callServerTool("x_"+pmMCPServer+"_"+tool, args)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("%s answered no JSON: %w: %.300s", tool, err, text)
	}
	return nil
}

// pmDryRun is the part of reconcile_capability's dry run the proof reads.
type pmDryRun struct {
	DryRun        bool             `json:"dryRun"`
	Order         []string         `json:"order"`
	PullRequests  []any            `json:"pullRequests"`
	Skipped       []map[string]any `json:"skipped"`
	CommitRefused any              `json:"commitRefused"`
	Installations []struct {
		Name   string `json:"name"`
		Inputs struct {
			Installation struct {
				Workspaces *struct {
					Enabled bool `json:"enabled"`
				} `json:"workspaces"`
				DexAppVersion string `json:"dexAppVersion"`
			} `json:"installation"`
		} `json:"inputs"`
		Files []struct {
			Repository string `json:"repository"`
			Path       string `json:"path"`
			Change     string `json:"change"`
			Content    string `json:"content"`
		} `json:"files"`
		DexClients []pmDexClient  `json:"dexClients"`
		Diff       map[string]int `json:"diff"`
	} `json:"installations"`
}

// pmDexClient is a Dex client of the plan: its key and the redirect URIs it
// registers beside the template's.
type pmDexClient struct {
	Client            string   `json:"client"`
	ExtraRedirectURIs []string `json:"extraRedirectURIs"`
}

// assertWorkspaces asserts the plan is the fixture's with workspaces on: the
// installation planned as a dry run, the switch read from the record,
// the sign-in URI on the platform client in the dex-app patch, the
// workspaces and workspace-manager keys kept in the values patch, and the
// commit not refused.
func (p pmDryRun) assertWorkspaces() error {
	if !p.DryRun {
		return fmt.Errorf("reconcile_capability answered dryRun=false to a dry run")
	}
	if len(p.Installations) != 1 || p.Installations[0].Name != pmInstallation || len(p.Skipped) != 0 {
		return fmt.Errorf("the dry run planned %v (skipped %v), wanted %s alone", p.Order, p.Skipped, pmInstallation)
	}
	if p.CommitRefused != nil {
		return fmt.Errorf("the dry run refuses the commit: %v", p.CommitRefused)
	}
	inst := p.Installations[0]
	if ws := inst.Inputs.Installation.Workspaces; ws == nil || !ws.Enabled {
		return fmt.Errorf("the plan's inputs carry no installation.workspaces.enabled: true read from %s", pmValuesPatch)
	}
	muster := slices.IndexFunc(inst.DexClients, func(c pmDexClient) bool { return c.Client == componentMuster })
	if muster < 0 || !slices.Contains(inst.DexClients[muster].ExtraRedirectURIs, pmSignInURI) {
		return fmt.Errorf("the plan's Dex clients do not register %s on the platform client muster: %+v", pmSignInURI, inst.DexClients)
	}
	files := map[string]string{}
	for _, f := range inst.Files {
		files[f.Path] = f.Content
	}
	var dex struct {
		OIDC struct {
			StaticClients map[string]struct {
				ExtraRedirectURIs []string `yaml:"extraRedirectURIs"`
			} `yaml:"staticClients"`
		} `yaml:"oidc"`
	}
	if err := yaml.Unmarshal([]byte(files[pmDexPatch]), &dex); err != nil {
		return fmt.Errorf("the planned %s: %w", pmDexPatch, err)
	}
	if !slices.Contains(dex.OIDC.StaticClients[componentMuster].ExtraRedirectURIs, pmSignInURI) {
		return fmt.Errorf("the planned %s carries no %s in oidc.staticClients.muster.extraRedirectURIs:\n%s", pmDexPatch, pmSignInURI, files[pmDexPatch])
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(files[pmValuesPatch]), &values); err != nil {
		return fmt.Errorf("the planned %s: %w", pmValuesPatch, err)
	}
	ws, _ := values["workspaces"].(map[string]any)
	if ws["enabled"] != true || values["workspace-manager"] == nil {
		return fmt.Errorf("the planned %s does not keep workspaces.enabled: true and the workspace-manager values:\n%s", pmValuesPatch, files[pmValuesPatch])
	}
	return nil
}

// proofLines are the plan's facts the proof prints.
func (p pmDryRun) proofLines() []string {
	inst := p.Installations[0]
	return []string{
		fmt.Sprintf("planned %s: %d create, %d update, %d unchanged in %d pull requests, none opened; the commit not refused (dex-app %s)",
			inst.Name, inst.Diff["create"], inst.Diff["update"], inst.Diff["unchanged"], len(p.PullRequests), inst.Inputs.Installation.DexAppVersion),
		"installation.workspaces.enabled: true, read from " + pmValuesPatch,
		pmDexPatch + ": oidc.staticClients.muster.extraRedirectURIs carries " + pmSignInURI,
		pmValuesPatch + ": workspaces.enabled: true and the workspace-manager values kept",
	}
}

// registryMisses reads the requests the registry answered 404 for.
func (c *fakeContainer) registryMisses() ([]string, error) {
	resp, err := c.client.Get(c.hostURL + registryFakeMissesPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out []string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("reading the registry's misses: %w", err)
	}
	return out, nil
}

// installPlatformManager applies the manager's OCIRepository and HelmRelease
// and waits for the release Ready; the returned func deletes both, and
// helm-controller uninstalls the release.
func installPlatformManager(cfg *config.Config, version string) (func(), error) {
	manifests, err := platformManagerManifests(cfg, version)
	if err != nil {
		return nil, err
	}
	hrGVR, err := gvrFor(helmReleaseResource)
	if err != nil {
		return nil, err
	}
	repoGVR, err := gvrFor(ociRepositoryResource)
	if err != nil {
		return nil, err
	}
	remove := func() {
		if err := deleteObject(context.Background(), hrGVR, platformNamespace, pmRelease, pmReleaseWait); err != nil {
			note("cleanup: deleting the HelmRelease %s: %v", pmRelease, err)
		}
		if err := deleteObject(context.Background(), repoGVR, platformNamespace, pmRelease, pmReleaseWait); err != nil {
			note("cleanup: deleting the OCIRepository %s: %v", pmRelease, err)
		}
	}
	remove() // a leftover of an aborted run
	ctx, cancel := context.WithTimeout(context.Background(), pmReleaseWait+time.Minute)
	defer cancel()
	if _, err := applyManifests(ctx, manifests); err != nil {
		remove()
		return nil, err
	}
	if err := waitCondition(ctx, hrGVR, platformNamespace, pmRelease, "Ready", "True", pmReleaseWait); err != nil {
		gitopsPodDiagnosis(pmRelease)
		remove()
		return nil, fmt.Errorf("HelmRelease %s/%s not Ready: %w", platformNamespace, pmRelease, err)
	}
	note("HelmRelease %s Ready", pmRelease)
	return remove, nil
}

// ociRepositoryResource is Flux's OCIRepository, as gvrFor takes it.
const ociRepositoryResource = "ocirepositories.source.toolkit.fluxcd.io"

// platformManagerManifests are the manager's OCIRepository at version and its
// HelmRelease, through the platform's bundled engine as the lab's own
// mcp-prometheus is (the tenant ServiceAccount agent-platform-flux).
func platformManagerManifests(cfg *config.Config, version string) ([]byte, error) {
	values, err := json.Marshal(platformManagerValues(cfg))
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, `apiVersion: %[1]s
kind: OCIRepository
metadata:
  name: %[3]s
  namespace: %[4]s
  labels:
    %[5]s: %[6]s
spec:
  interval: %[7]s
  url: %[8]s
  ref:
    semver: %[9]q
---
apiVersion: %[2]s
kind: HelmRelease
metadata:
  name: %[3]s
  namespace: %[4]s
  labels:
    %[5]s: %[6]s
spec:
  interval: %[7]s
  releaseName: %[3]s
  serviceAccountName: agent-platform-flux
  chartRef:
    kind: OCIRepository
    name: %[3]s
    namespace: %[4]s
  install:
    remediation:
      retries: 3
  values: %[10]s
`, fluxOCIRepositoryAPIVersion, fluxHelmReleaseAPIVersion, pmRelease, platformNamespace, managedByLabel, managedByAgentlabValue,
		helmReleaseInterval, pmChartURL, version, values), nil
}

// The manager chart's values keys the lab sets.
const (
	pmFullnameOverride = "fullnameOverride"
	pmGitHubValues     = "github"
	pmMCPServerValues  = "mcpServer"
)

// platformManagerValues are the manager's values in the lab: the fixture as
// its GitHub and registry, the fixture's installation as its hub, OAuth on
// and pinned to the lab Dex through the OAuth fixture's client (whose
// redirect URIs list muster's proxy callback), no Action records (a dry run
// writes none) and no live tools.
func platformManagerValues(cfg *config.Config) map[string]any {
	return map[string]any{
		pmFullnameOverride: pmMCPServer,
		pmGitHubValues:     map[string]any{"apiURL": pmGitHubHost + githubFakeAPIPath},
		"registry":         map[string]any{repositoryArg: pmRegistryRepo},
		"hub":              pmInstallation,
		"actions":          map[string]any{valuesEnabled: false, "installCRD": false},
		"oauth": map[string]any{valuesEnabled: true,
			"baseURL": "http://" + pmMCPServer + "." + platformNamespace + ".svc.cluster.local:8080"},
		musterValues: map[string]any{pmMCPServerValues: map[string]any{valuesEnabled: true, nameKey: pmMCPServer,
			descriptionKey: "agentlab pm-test: the platform manager on the lab's registry fixture (temporary)",
			"auth": map[string]any{"authorizationServer": map[string]any{
				"issuer":                     pmAppIssuer,
				"expectedIssuer":             "",
				"authorizationEndpoint":      cfg.Issuer() + "/auth",
				"tokenEndpoint":              cfg.Issuer() + "/token",
				"scopes":                     "openid profile email offline_access",
				"clientCredentialsSecretRef": map[string]any{nameKey: oauthFixtureServer + "-client", namespaceKey: platformNamespace},
				"grantScope":                 "subject",
			}},
		}},
	}
}
