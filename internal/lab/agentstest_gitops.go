package lab

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	chartutil "helm.sh/helm/v4/pkg/chart/common/util"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/agentlab/internal/config"
)

// agents-gitops-test is the Dev Portal's edit of an agent applied from git,
// headless (docs/agents.md "Editing an agent applied from git"). Such an
// agent's HelmRelease carries the provenance labels a Flux Kustomization
// stamps; agent-manager reports it managed: gitops and refuses every live
// write with gitops_owned. The portal's edit page dry-runs each change with
// validate_agent (update: true) and offers the write only once the dry run
// is valid; for an agent applied from git that write is update_agent in mode
// commit, a pull request opened as the person in the repository that owns
// the release. The proof is that dry run and that write through muster as
// the person, against agent-manager pinned to the fake GitHub
// (githubfake.go) the way models-test pins model-manager
// (modelstest_gitops.go).
//
// The commit target is named (repository, branch, path): the lab's engine
// runs no kustomize-controller, so no Kustomization resolves it from the
// release's labels here; the labels alone make the release GitOps-owned.

const (
	// gitopsAgentManager is the temporary App-pinned copy of the platform's
	// agent-manager release: its fullname, Service and MCPServer, so its
	// tools are x_agent-manager-gitops_*.
	gitopsAgentManager = "agent-manager-gitops"
	// gitopsAgentManagerIssuer is the copy's App issuer, the key muster files
	// the person's grant under (see gitopsAppIssuer).
	gitopsAgentManagerIssuer = "https://github.agentlab.invalid/apps/" + gitopsAgentManager
	// agentsGitOpsAgent is the throwaway agent applied from git: a release
	// agent-manager composed, applied by the proof with the Flux labels.
	agentsGitOpsAgent = "agentlab-agents-gitops"
	// agentsGitOpsDescription is the change the edit carries.
	agentsGitOpsDescription = "edited through a pull request"
	// agentsGitOpsDirectory is agent-manager's directory under gitopsPath,
	// the one gitops-commit's layout names.
	agentsGitOpsDirectory = gitopsPath + "/agent-manager"
	// gitOpsManaged is get_agent's managed for a release applied from git.
	gitOpsManaged = "gitops"
)

// AgentsGitOpsTestOptions are agents-gitops-test's flags.
type AgentsGitOpsTestOptions struct {
	// GitHubFakeBinary is the static Linux agentlab the fake GitHub API
	// container runs (default: this binary).
	GitHubFakeBinary string
}

// AgentsGitOpsTest is the headless proof that an agent applied from git is
// edited through a pull request, as the portal does it: the platform's
// agent-manager composes the release, the proof applies it with the Flux
// labels and agent-manager reports it managed: gitops; validate_agent of a
// change refuses mode apply with gitops_owned (force or not) and validates
// mode commit, answering the manifests with the change; update_agent in mode
// commit opens the pull request as the person on the fake GitHub, its file
// the dry run's manifest byte for byte, while the live release stays
// untouched and a mode apply write is still refused. Leaves nothing behind.
func AgentsGitOpsTest(cfg *config.Config, email string, opts AgentsGitOpsTestOptions) error {
	if opts.GitHubFakeBinary == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locating this binary for the fake GitHub API container: %w (pass --github-fake-binary)", err)
		}
		opts.GitHubFakeBinary = exe
	}
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled || !cfg.Platform.Agents {
		return fmt.Errorf("platform.agents is off in %s — enable it and run `agentlab platform` first", config.File)
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}
	login, _, _ := strings.Cut(user.Email, "@")

	step("Logging in to Dex as %s", email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	session, err := openMusterSession(cfg, token, "agents-gitops-test")
	if err != nil {
		return err
	}
	platform := &modelManagerTools{session: session, server: agentManagerMCPServer}

	// Leftovers of an aborted run first, and the agent on every exit path.
	if agentExists(agentsGitOpsAgent) {
		note("cleanup: removing %s", agentsGitOpsAgent)
		if err := removeAgent(agentsGitOpsAgent); err != nil {
			return err
		}
	}
	defer func() {
		if err := removeAgent(agentsGitOpsAgent); err != nil {
			note("cleanup: %v", err)
		}
	}()

	step("%s: the release of %s as agent-manager composes it, the file the repository carries", platform.toolName("validate_agent"), agentsGitOpsAgent)
	modelConfig, err := agentManagerModelConfig(session)
	if err != nil {
		return err
	}
	var composed validateReport
	spec := map[string]any{nameKey: agentsGitOpsAgent, "modelConfig": modelConfig, "toolset": []string{agentsTestToolset}, "displayName": "Agentlab GitOps agent", "description": "applied from git"}
	if err := platform.getJSON("validate_agent", spec, &composed); err != nil {
		return err
	}
	if !composed.Valid || composed.Manifests.HelmRelease == "" || composed.Manifests.OCIRepository == "" {
		return fmt.Errorf("the create's dry run is not valid: %v", composed.Errors)
	}
	note("valid: HelmRelease %s/%s on ModelConfig %s, next to the chart source", kagentNamespace, agentsGitOpsAgent, modelConfig)

	step("Applying it with the provenance a Flux Kustomization stamps (%s=%s): an agent applied from git", gitopsFluxName, gitopsFluxFixture)
	if err := applyGitOpsAgent(composed.Manifests.HelmRelease); err != nil {
		return err
	}
	if err := expectManaged(platform, gitOpsManaged); err != nil {
		return err
	}

	edit := map[string]any{nameKey: agentsGitOpsAgent, "update": true, "description": agentsGitOpsDescription}
	withCommit := maps.Clone(edit)
	withCommit[modeArg] = modeCommit
	step("%s of the change on the platform's agent-manager, which offers no commit mode: gitops_owned in mode apply, unsupported in mode commit — as its writes answer", platform.toolName("validate_agent"))
	if err := expectRefusal(platform, "validate_agent", edit, "gitops_owned", "mode commit"); err != nil {
		return err
	}
	if err := expectRefusal(platform, "validate_agent", withCommit, "unsupported", "capabilities.commit"); err != nil {
		return err
	}

	step("Commit mode: the fake GitHub API as a container on the %s network, holding %s@%s with the release's file under %s", kindDockerNetwork, gitopsRepository, gitopsBranch, agentsGitOpsDirectory)
	fake, err := startGitHubFakeContainer(cfg, opts.GitHubFakeBinary, gitopsRepository, gitopsBranch, agentsGitOpsSeed(composed.Manifests))
	if err != nil {
		return err
	}
	defer fake.close()
	removeService, err := fakeServiceForPods(cfg, githubFakeService, "the fake GitHub API", fake.podIP, fakeContainerPort, githubFakeServiceHost+githubFakeHealthPath)
	if err != nil {
		return err
	}
	defer removeService()

	step("Applying HelmRelease %s: a copy of the platform's agent-manager release, pinned to the lab Dex as its GitHub App and to %s%s as its GitHub API", gitopsAgentManager, githubFakeServiceHost, githubFakeAPIPath)
	uninstall, err := gitopsCopy{platform: agentManagerMCPServer, name: gitopsAgentManager, values: func(v map[string]any) map[string]any {
		return gitopsAgentManagerValues(cfg, v, fake.podIP)
	}}.install()
	if err != nil {
		return err
	}
	defer uninstall()
	if err := waitMCPServerState(gitopsAgentManager, mcpServerStateAuthRequired, mcpServerStateConnected); err != nil {
		return err
	}

	step("Signing %s in to %s through muster (core_auth_login, the Dex login form, muster's proxy callback)", user.Email, gitopsAgentManager)
	defer func() {
		_, _ = session.callToolEnvelope("core_auth_logout", map[string]any{serverKey: gitopsAgentManager})
	}()
	challenge, err := signInChallenge(cfg, session, gitopsAgentManager)
	if err != nil {
		return err
	}
	if err := completeSignIn(challenge.authURL, user); err != nil {
		return err
	}
	note("signed in: muster holds the pinned server's token for the session")
	api := &modelManagerTools{session: session, server: gitopsAgentManager}
	var info struct {
		Version      string          `json:"version"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := api.getJSON("get_info", nil, &info); err != nil {
		return err
	}
	if !info.Capabilities[modeCommit] {
		return fmt.Errorf("%s reports capabilities.commit=false although it is pinned: %v", gitopsAgentManager, info.Capabilities)
	}
	note("%s: version %s, capabilities.commit=true", api.toolName("get_info"), info.Version)
	if err := expectManaged(api, gitOpsManaged); err != nil {
		return err
	}

	step("%s of the change, mode apply: gitops_owned, force or not — the live write it stands for is refused", api.toolName("validate_agent"))
	if err := expectRefusal(api, "validate_agent", edit, "gitops_owned", "mode commit"); err != nil {
		return err
	}
	forced := maps.Clone(edit)
	forced["force"] = true
	if err := expectRefusal(api, "validate_agent", forced, "gitops_owned", "mode commit"); err != nil {
		return err
	}

	step("%s of the change, mode commit: the dry run the edit page runs before it offers Commit — valid, the manifests carrying the change, nothing written", api.toolName("validate_agent"))
	var dry validateReport
	if err := api.getJSON("validate_agent", withCommit, &dry); err != nil {
		return fmt.Errorf("the dry run of a change to an agent applied from git is refused in mode commit, so a Commit button can never be enabled for it: %w", err)
	}
	if !dry.Valid || dry.Mode != "update" {
		return fmt.Errorf("the dry run answered valid=%v mode=%q with %v, wanted a valid update", dry.Valid, dry.Mode, dry.Errors)
	}
	if !strings.Contains(dry.Manifests.HelmRelease, "description: "+agentsGitOpsDescription) {
		return fmt.Errorf("the dry run's HelmRelease does not carry the new description:\n%s", dry.Manifests.HelmRelease)
	}
	if pulls, err := fake.pulls(); err != nil || len(pulls) != 0 {
		return fmt.Errorf("the dry run reached the fake GitHub: %d pull requests (%v)", len(pulls), err)
	}
	live, err := readGitOpsAgentRelease()
	if err != nil {
		return err
	}
	note("valid, mode %s, no violation; the HelmRelease carries %q; no pull request, the release at resourceVersion %s", dry.Mode, agentsGitOpsDescription, live.GetResourceVersion())

	step("%s mode commit: the edit's pull request, opened as %s in %s@%s", api.toolName("update_agent"), login, gitopsRepository, gitopsBranch)
	write := map[string]any{nameKey: agentsGitOpsAgent, "description": agentsGitOpsDescription, modeArg: modeCommit,
		repositoryArg: gitopsRepository, "branch": gitopsBranch, "path": gitopsPath}
	var done struct {
		Changed   []string       `json:"changed"`
		Manifests agentManifests `json:"manifests"`
		Commit    *commitAnswer  `json:"commit"`
		Agent     struct {
			Managed string `json:"managed"`
		} `json:"agent"`
	}
	if err := api.getJSON("update_agent", write, &done); err != nil {
		return err
	}
	if done.Commit == nil || done.Commit.PullRequest == "" || done.Commit.Number == 0 || done.Commit.Author != login {
		return fmt.Errorf("update_agent mode commit answered commit %+v, wanted a pull request opened as %q", done.Commit, login)
	}
	if len(done.Changed) != 1 || done.Changed[0] != agentDescriptionValuePath {
		return fmt.Errorf("update_agent reports changed %v, wanted [%s]", done.Changed, agentDescriptionValuePath)
	}
	if done.Manifests.HelmRelease != dry.Manifests.HelmRelease {
		return fmt.Errorf("the write's HelmRelease differs from the dry run's:\n--- dry run\n%s\n--- write\n%s", dry.Manifests.HelmRelease, done.Manifests.HelmRelease)
	}
	pulls, err := fake.pulls()
	if err != nil {
		return err
	}
	if len(pulls) != 1 {
		return fmt.Errorf("the fake GitHub holds %d pull requests, wanted the one", len(pulls))
	}
	pr := pulls[0]
	if pr.Number != done.Commit.Number || pr.Author != login || pr.Base != gitopsBranch || pr.Head != done.Commit.Branch {
		return fmt.Errorf("the fake's pull request is #%d %s -> %s by %q, the tool answered #%d %s -> %s by %q", pr.Number, pr.Head, pr.Base, pr.Author, done.Commit.Number, done.Commit.Branch, gitopsBranch, login)
	}
	file := agentsGitOpsDirectory + "/" + agentsGitOpsAgent + ".yaml"
	if got, ok := pr.Files[file]; !ok || got != dry.Manifests.HelmRelease {
		return fmt.Errorf("the pull request's %s (present %v) is not the dry run's HelmRelease:\n--- dry run\n%s\n--- pull request\n%s", file, ok, dry.Manifests.HelmRelease, got)
	}
	if len(pr.Files) != 1 || len(pr.Removed) != 0 {
		return fmt.Errorf("the pull request writes %d files and removes %d, wanted the release's file alone: %v %v", len(pr.Files), len(pr.Removed), slices.Sorted(maps.Keys(pr.Files)), pr.Removed)
	}
	// The answer lists the directory's kustomization.yaml as unchanged next
	// to the release's file: the update is the one change.
	updated := false
	for _, f := range done.Commit.Files {
		switch {
		case f.Path == file && f.Action == "update":
			updated = true
		case f.Action != "unchanged":
			return fmt.Errorf("update_agent reports %s %s, wanted an update of %s alone", f.Action, f.Path, file)
		}
	}
	if !updated {
		return fmt.Errorf("update_agent reports the commit's files as %+v, wanted an update of %s", done.Commit.Files, file)
	}
	if done.Agent.Managed != gitOpsManaged {
		return fmt.Errorf("update_agent reports the agent managed=%q after the commit, wanted %s", done.Agent.Managed, gitOpsManaged)
	}
	note("#%d %q by %s: %s -> %s, %s is the dry run's manifest byte for byte", pr.Number, pr.Title, pr.Author, pr.Head, pr.Base, file)

	step("The live release is untouched, and a mode apply write is still refused")
	after, err := readGitOpsAgentRelease()
	if err != nil {
		return err
	}
	if after.GetResourceVersion() != live.GetResourceVersion() {
		return fmt.Errorf("the commit changed the live HelmRelease %s (resourceVersion %s -> %s): commit mode writes git only", agentsGitOpsAgent, live.GetResourceVersion(), after.GetResourceVersion())
	}
	if err := expectRefusal(api, "update_agent", map[string]any{nameKey: agentsGitOpsAgent, "description": agentsGitOpsDescription}, "gitops_owned", "mode commit"); err != nil {
		return err
	}
	note("HelmRelease %s at resourceVersion %s, as before", agentsGitOpsAgent, after.GetResourceVersion())

	fmt.Printf("PASS: %s is applied from git (managed %s); validate_agent of a change refuses mode apply with gitops_owned (force or not) and validates mode commit with the manifests; update_agent mode commit as %s -> pull request #%d on the fake GitHub (%s -> %s), its file the dry run's; the live release untouched, mode apply still refused\n",
		agentsGitOpsAgent, gitOpsManaged, login, pr.Number, pr.Head, pr.Base)
	return nil
}

// applyGitOpsAgent applies the composed HelmRelease and stamps the Flux
// provenance on it: what a Kustomization leaves on every object it applies,
// and what makes agent-manager report the agent managed: gitops.
func applyGitOpsAgent(helmRelease string) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if _, err := applyManifests(ctx, []byte(helmRelease)); err != nil {
		return fmt.Errorf("applying the HelmRelease %s/%s: %w", kagentNamespace, agentsGitOpsAgent, err)
	}
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return err
	}
	body := fmt.Appendf(nil, `{"metadata":{"labels":{%q:%q,%q:%q}}}`, gitopsFluxName, gitopsFluxFixture, gitopsFluxNamespace, platformNamespace)
	if err := patchObject(ctx, gvr, kagentNamespace, agentsGitOpsAgent, types.MergePatchType, body); err != nil {
		return fmt.Errorf("labelling the HelmRelease %s/%s: %w", kagentNamespace, agentsGitOpsAgent, err)
	}
	note("HelmRelease %s/%s applied, %s=%s", kagentNamespace, agentsGitOpsAgent, gitopsFluxName, gitopsFluxFixture)
	return nil
}

// readGitOpsAgentRelease reads the proof's HelmRelease back.
func readGitOpsAgentRelease() (*unstructured.Unstructured, error) {
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	return getObject(ctx, gvr, kagentNamespace, agentsGitOpsAgent)
}

// agentsGitOpsSeed is the fake repository's base branch: the directory a
// Kustomization builds, with agent-manager's directory in gitops-commit's
// layout — the namespace's chart source and the agent's release, the files
// agent-manager's commit mode would have written on create.
func agentsGitOpsSeed(m agentManifests) map[string][]byte {
	return map[string][]byte{
		gitopsPath + "/kustomization.yaml":                        []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - agent-manager\n"),
		agentsGitOpsDirectory + "/kustomization.yaml":             []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - agent.yaml\n  - " + agentsGitOpsAgent + ".yaml\n"),
		agentsGitOpsDirectory + "/agent.yaml":                     []byte(m.OCIRepository),
		agentsGitOpsDirectory + "/" + agentsGitOpsAgent + ".yaml": []byte(m.HelmRelease),
	}
}

// gitopsAgentManagerValues are the platform release's values with the copy's
// own name, no route, and the GitHub pin (gitopsAppPin) under
// gitopsAgentManagerIssuer, registered with muster under the copy's name.
func gitopsAgentManagerValues(cfg *config.Config, platformValues map[string]any, fakeIP string) map[string]any {
	overrides := map[string]any{
		"fullnameOverride": gitopsAgentManager,
		"httpRoute":        map[string]any{valuesEnabled: false},
		"github":           gitopsAppPin(cfg, gitopsAgentManagerIssuer),
		musterValues: map[string]any{"mcpServer": map[string]any{valuesEnabled: true, nameKey: gitopsAgentManager,
			"description": "agentlab agents-gitops-test: agent-manager pinned to the fake GitHub, for the commit proof (temporary)"}},
	}
	if egress := gitopsEgress(platformValues, fakeIP); egress != nil {
		overrides["networkPolicy"] = egress
	}
	if platformValues == nil {
		return overrides
	}
	return chartutil.MergeTables(overrides, platformValues)
}

// expectManaged asserts get_agent reports the proof's agent managed as
// wanted.
func expectManaged(api *modelManagerTools, want string) error {
	var agent struct {
		Managed string `json:"managed"`
	}
	if err := api.getJSON("get_agent", map[string]any{nameKey: agentsGitOpsAgent}, &agent); err != nil {
		return err
	}
	if agent.Managed != want {
		return fmt.Errorf("%s reports %s managed=%q, wanted %s", api.toolName("get_agent"), agentsGitOpsAgent, agent.Managed, want)
	}
	note("%s: managed %s", api.toolName("get_agent"), agent.Managed)
	return nil
}

// expectRefusal asserts a tool refuses the call with code, its message
// naming mentions.
func expectRefusal(api *modelManagerTools, tool string, args map[string]any, code, mentions string) error {
	_, err := api.call(tool, args)
	if refusalCode(err) != code {
		return fmt.Errorf("%s with mode %v answered %v, wanted %s", api.toolName(tool), firstNonEmpty(fmt.Sprint(args[modeArg]), "apply"), err, code)
	}
	if !strings.Contains(err.Error(), mentions) {
		return fmt.Errorf("%s's %s refusal does not say %q: %v", api.toolName(tool), code, mentions, err)
	}
	note("%s: %s", api.toolName(tool), excerpt(err.Error(), 200))
	return nil
}
