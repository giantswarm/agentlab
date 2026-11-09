package lab

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/agentlab/internal/config"
)

// The commit stage of the portal proof (agent-manager's commit mode,
// agentmanagercommit.go): the portal's create, edit and delete dialogs land
// their change as a pull request opened as the signed-in person in the
// repository that owns the agents, never as a live write. The lab's GitOps
// fixture makes that repository the first of the workspaces' github proof
// repositories: a suspended GitRepository on it and the Kustomization
// agentlab-gitops-fixture (gitopsfixture.go) at gitopsPath, with one agent
// applied from git — the seed, a release agent-manager composed and the
// proof applied with the Kustomization's labels.
//
//   - create: create_agent in mode commit, as the create dialog sends it (no
//     target): agent-manager resolves the namespace's repository from the
//     seed's labels and opens a pull request adding the agent's file under
//     gitopsPath/agent-manager; nothing is applied.
//   - edit: the created agent applied from git as well, its file the
//     GitRepository's branch (the create's pull-request branch): the edit
//     dialog's dry run in mode apply is gitops_owned (the Edit page's
//     GitOps-owned mode), the commit dry run names the pull request it would
//     open, and update_agent in mode commit opens it; the live release
//     untouched.
//   - delete: delete_agent in mode commit opens the pull request removing the
//     agent's file; the release stays.
//
// The admin's grant of the instance's App comes first (grantProofProvider):
// held already, or granted once through muster's sign-in link. Every branch
// the stage opened is deleted at the end, which closes its pull
// request, with the person's own git credentials on this host; the agents,
// the Kustomization and the GitRepository go with them, on every exit path.

const (
	// commitTestAgent is the agent the commit stage creates, edits and
	// deletes through pull requests; commitSeedAgent the agent applied from
	// git that anchors the namespace's repository.
	commitTestAgent = "agentlab-commit"
	commitSeedAgent = "agentlab-commit-seed"
	// commitEditedDescription is the edit the update pull request carries.
	commitEditedDescription = "Edited through a pull request by agentlab backstage-test."
	// commitDirectory is where agent-manager writes an agent's files under
	// the Kustomization's path.
	commitDirectory = "agent-manager"
	// commitBranchPrefix starts every branch agent-manager opens.
	commitBranchPrefix = "agent-manager/"
)

// gitRepositoryGVR is Flux's GitRepository, the version agent-manager reads.
var gitRepositoryGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}

// commitAnswer is a write's answer in mode commit, as the portal reads it.
type commitAnswer struct {
	Mode   string `json:"mode"`
	DryRun bool   `json:"dryRun"`
	Commit struct {
		Repository    string `json:"repository"`
		Base          string `json:"base"`
		Directory     string `json:"directory"`
		Kustomization string `json:"kustomization"`
		Branch        string `json:"branch"`
		PullRequest   string `json:"pullRequest"`
		Number        int    `json:"number"`
		Author        string `json:"author"`
		Files         []struct {
			Path   string `json:"path"`
			Action string `json:"action"`
		} `json:"files"`
	} `json:"commit"`
}

// commitProof is one run of the commit stage.
type commitProof struct {
	primary *portalSession
	spec    agentSpec
	// repo is the fixture repository (owner/name) and base its default
	// branch.
	repo, base string
	// removeKustomization removes the fixture's Kustomization (and the API
	// it brought), nil until applied.
	removeKustomization func()
}

// proveCommitPath is the commit stage: skipped with its reason while
// agent-manager offers no commit mode and the lab wires none, a failure when
// the lab wires it and get_info does not report it.
func proveCommitPath(cfg *config.Config, primary *portalSession, spec agentSpec, info *agentManagerInfo) ([]string, error) {
	if !info.Capabilities[modeCommit] {
		wired, err := agentManagerCommitWired()
		if err != nil {
			return nil, err
		}
		if wired {
			return nil, fmt.Errorf("agent-manager runs with githubTokenExchange (Deployment %s/%s) but get_info reports no commit capability", platformNamespace, agentManagerMCPServer)
		}
		why := strings.TrimSpace(agentManagerCommitHint(nil))
		note("commit stage skipped: %s", why)
		return []string{"commit: skipped — " + why}, nil
	}
	repos := cfg.Platform.Workspaces.GitHub.ProofRepositories
	if len(repos) == 0 {
		return nil, fmt.Errorf("agent-manager offers commit mode, but platform.workspaces.github.proofRepositories names no repository for its pull requests (docs/agents.md \"Commit mode\")")
	}
	cp := &commitProof{primary: primary, repo: repos[0]}
	cp.spec = agentSpec{
		Name: commitTestAgent, ModelConfig: spec.ModelConfig, Toolset: spec.Toolset,
		DisplayName: backstageTestDisplayName + " (commit)",
		Description: "Throwaway agent of `agentlab backstage-test`, written only as pull requests that the same run closes.",
	}
	base, err := personGit(StateDir, "ls-remote", "--symref", cp.remote(), "HEAD")
	if err != nil {
		return nil, fmt.Errorf("reading %s's default branch with your git credentials: %w", cp.repo, err)
	}
	cp.base = defaultBranchOf(base)
	if cp.base == "" {
		return nil, fmt.Errorf("%s names no default branch: %q", cp.repo, base)
	}

	// The person's grant of the github instance's App, which muster's broker
	// releases to agent-manager: held already, or granted once in a browser
	// signed in to GitHub through muster's sign-in link, as every proof
	// acting on GitHub as the person takes it.
	admin, err := openWorkspacePerson(cfg, primary.user)
	if err != nil {
		return nil, err
	}
	// grantProofProvider records even a pre-existing grant in the run ledger.
	// Drop it on success and on a failed or timed-out sign-in alike, so the
	// next proof starts without this run's provider credentials.
	defer releaseWorkspaceGrants()
	if err := grantProofProvider(cfg, admin, config.WorkspaceProviderGitHub, 0); err != nil {
		return nil, err
	}

	cp.cleanup()
	defer cp.cleanup()

	var verdicts []string
	if err := cp.seed(); err != nil {
		return verdicts, err
	}
	created, err := cp.create()
	if err != nil {
		return verdicts, err
	}
	verdicts = append(verdicts, fmt.Sprintf("commit: create_agent in mode commit opened %s as %s on %s (base %s), nothing applied", created.Commit.PullRequest, created.Commit.Author, cp.repo, cp.base))
	edited, err := cp.edit(created.Commit.Branch)
	if err != nil {
		return verdicts, err
	}
	verdicts = append(verdicts, fmt.Sprintf("commit: the Edit page's GitOps-owned mode — gitops_owned in mode apply, update_agent in mode commit opened %s (base %s), the live release untouched", edited.Commit.PullRequest, edited.Commit.Base))
	deleted, err := cp.delete(created.Commit.Branch)
	if err != nil {
		return verdicts, err
	}
	verdicts = append(verdicts, fmt.Sprintf("commit: delete_agent in mode commit opened %s removing %s, the release in place", deleted.Commit.PullRequest, cp.agentFile()))
	return verdicts, nil
}

// remote is the fixture repository's git URL.
func (cp *commitProof) remote() string { return measureGitURL + "/" + cp.repo + ".git" }

// agentFile is the created agent's file as agent-manager writes it.
func (cp *commitProof) agentFile() string {
	return gitopsPath + "/" + commitDirectory + "/" + commitTestAgent + ".yaml"
}

// seed applies the fixture: the GitRepository on the repository's default
// branch, the seed agent applied from git, and the Kustomization listing it.
func (cp *commitProof) seed() error {
	step("Commit fixture: GitRepository %s/%s on %s@%s (suspended), agent %s applied from git, Kustomization %s/%s at %s", platformNamespace, gitopsFluxFixture, cp.repo, cp.base, commitSeedAgent, platformNamespace, gitopsFluxFixture, gitopsPath)
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if _, err := applyManifests(ctx, gitRepositoryManifest(cp.repo, cp.base)); err != nil {
		return fmt.Errorf("applying the GitRepository %s/%s: %w", platformNamespace, gitopsFluxFixture, err)
	}
	seed := cp.spec
	seed.Name, seed.DisplayName = commitSeedAgent, backstageTestDisplayName+" (applied from git)"
	report, err := portalValidateAgent(cp.primary, seed)
	if err != nil {
		return err
	}
	if !report.Valid || report.Manifests.HelmRelease == "" {
		return fmt.Errorf("the seed's dry run is not valid: %v", report.Errors)
	}
	if err := applyGitOpsRelease(commitSeedAgent, report.Manifests.HelmRelease); err != nil {
		return err
	}
	release, err := readHelmReleaseObject(commitSeedAgent)
	if err != nil {
		return err
	}
	cp.removeKustomization, err = applyGitOpsFixture(release)
	return err
}

// create is the create dialog's commit: no target, the namespace's
// repository resolved from the seed.
func (cp *commitProof) create() (*commitAnswer, error) {
	step("C1 create_agent %s in mode commit through the portal as %s: a pull request on %s, nothing applied", commitTestAgent, cp.primary.user.Email, cp.repo)
	args, err := portalAgentArgs(cp.spec)
	if err != nil {
		return nil, err
	}
	args[modeArg] = modeCommit
	var created commitAnswer
	if err := portalToolCall(cp.primary, "create_agent", args, &created); err != nil {
		return nil, err
	}
	if err := cp.assertPullRequest(&created, "create", cp.base); err != nil {
		return nil, err
	}
	if err := created.assertFile(cp.agentFile()); err != nil {
		return nil, err
	}
	if created.Commit.Kustomization != platformNamespace+"/"+gitopsFluxFixture {
		return nil, fmt.Errorf("the create's pull request lands through Kustomization %q, want %s/%s (the seed's)", created.Commit.Kustomization, platformNamespace, gitopsFluxFixture)
	}
	if agentExists(commitTestAgent) {
		return nil, fmt.Errorf("create_agent in mode commit applied %s live", commitTestAgent)
	}
	note("%s by %s: branch %s, files %s; no release applied", created.Commit.PullRequest, created.Commit.Author, created.Commit.Branch, created.files())
	return &created, nil
}

// edit applies the created agent from git, its file on branch, and proves
// the Edit page's GitOps-owned mode.
func (cp *commitProof) edit(branch string) (*commitAnswer, error) {
	step("C2 %s applied from git, its file on %s: the GitRepository follows that branch", commitTestAgent, branch)
	report, err := portalValidateAgent(cp.primary, cp.spec)
	if err != nil {
		return nil, err
	}
	if !report.Valid || report.Manifests.HelmRelease == "" {
		return nil, fmt.Errorf("the dry run of %s is not valid: %v", commitTestAgent, report.Errors)
	}
	if err := applyGitOpsRelease(commitTestAgent, report.Manifests.HelmRelease); err != nil {
		return nil, err
	}
	if err := cp.followBranch(branch); err != nil {
		return nil, err
	}
	var agent struct {
		Managed string `json:"managed"`
	}
	if err := portalToolCall(cp.primary, "get_agent", map[string]any{nameKey: commitTestAgent, namespaceKey: kagentNamespace}, &agent); err != nil {
		return nil, err
	}
	if agent.Managed != gitOpsManaged {
		return nil, fmt.Errorf("get_agent reports %s managed=%q, want %s", commitTestAgent, agent.Managed, gitOpsManaged)
	}
	live, err := readHelmReleaseObject(commitTestAgent)
	if err != nil {
		return nil, err
	}

	step("C3 the Edit page's GitOps-owned mode: validate_agent{update} gitops_owned in mode apply, the commit's dry run, update_agent in mode commit")
	update := map[string]any{nameKey: commitTestAgent, namespaceKey: kagentNamespace, descriptionKey: commitEditedDescription}
	validate := maps.Clone(update)
	validate[validateModeUpdate] = true
	if err := portalToolCall(cp.primary, "validate_agent", validate, nil); err == nil || !strings.Contains(err.Error(), "gitops_owned") {
		return nil, fmt.Errorf("validate_agent{update} of %s in mode apply answered %v, want gitops_owned", commitTestAgent, err)
	}
	dry := maps.Clone(update)
	dry[modeArg], dry[dryRunArg] = modeCommit, true
	var planned commitAnswer
	if err := portalToolCall(cp.primary, "update_agent", dry, &planned); err != nil {
		return nil, err
	}
	if !planned.DryRun || planned.Commit.PullRequest != "" || planned.Commit.Base != branch || planned.Commit.Repository != cp.repo {
		return nil, fmt.Errorf("the edit's commit dry run answered dryRun=%v pullRequest=%q on %s@%s, want a dry run on %s@%s", planned.DryRun, planned.Commit.PullRequest, planned.Commit.Repository, planned.Commit.Base, cp.repo, branch)
	}
	note("dry run: %s@%s, files %s, nothing opened", planned.Commit.Repository, planned.Commit.Base, planned.files())
	commit := maps.Clone(update)
	commit[modeArg] = modeCommit
	var edited commitAnswer
	if err := portalToolCall(cp.primary, "update_agent", commit, &edited); err != nil {
		return nil, err
	}
	if err := cp.assertPullRequest(&edited, "update", branch); err != nil {
		return nil, err
	}
	if err := edited.assertFile(cp.agentFile()); err != nil {
		return nil, err
	}
	after, err := readHelmReleaseObject(commitTestAgent)
	if err != nil {
		return nil, err
	}
	if after.GetGeneration() != live.GetGeneration() {
		return nil, fmt.Errorf("update_agent in mode commit changed the live HelmRelease %s (generation %d -> %d)", commitTestAgent, live.GetGeneration(), after.GetGeneration())
	}
	note("%s by %s: branch %s; the live release at generation %d, as before", edited.Commit.PullRequest, edited.Commit.Author, edited.Commit.Branch, after.GetGeneration())
	return &edited, nil
}

// delete is the delete dialog's commit.
func (cp *commitProof) delete(branch string) (*commitAnswer, error) {
	step("C4 delete_agent %s in mode commit through the portal: a pull request removing %s, the release in place", commitTestAgent, cp.agentFile())
	var deleted commitAnswer
	if err := portalToolCall(cp.primary, "delete_agent", map[string]any{nameKey: commitTestAgent, namespaceKey: kagentNamespace, modeArg: modeCommit}, &deleted); err != nil {
		return nil, err
	}
	if err := cp.assertPullRequest(&deleted, "delete", branch); err != nil {
		return nil, err
	}
	if err := deleted.assertFile(cp.agentFile()); err != nil {
		return nil, err
	}
	if !agentExists(commitTestAgent) {
		return nil, fmt.Errorf("delete_agent in mode commit removed %s live", commitTestAgent)
	}
	note("%s by %s: branch %s, files %s; the release in place", deleted.Commit.PullRequest, deleted.Commit.Author, deleted.Commit.Branch, deleted.files())
	return &deleted, nil
}

// assertPullRequest checks a commit's answer: a pull request on the
// fixture repository against base, from agent-manager's branch for verb,
// opened as a GitHub login, its branch on the repository.
func (cp *commitProof) assertPullRequest(a *commitAnswer, verb, base string) error {
	c := a.Commit
	wantBranch := commitBranchPrefix + verb + "-" + kagentNamespace + "-" + commitTestAgent
	switch {
	case a.DryRun:
		return fmt.Errorf("%s in mode commit answered a dry run", verb)
	case c.Repository != cp.repo || c.Base != base:
		return fmt.Errorf("the %s's pull request is on %s@%s, want %s@%s", verb, c.Repository, c.Base, cp.repo, base)
	case c.Branch != wantBranch:
		return fmt.Errorf("the %s's pull request comes from branch %q, want %s", verb, c.Branch, wantBranch)
	case !strings.HasPrefix(c.PullRequest, measureGitURL+"/"+cp.repo+"/pull/") || c.Number <= 0:
		return fmt.Errorf("the %s returned no pull-request link on %s: %q (number %d)", verb, cp.repo, c.PullRequest, c.Number)
	case c.Author == "":
		return fmt.Errorf("the %s's pull request names no author", verb)
	}
	out, err := personGit(StateDir, "ls-remote", cp.remote(), "refs/heads/"+c.Branch)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(out, "refs/heads/"+c.Branch) {
		return fmt.Errorf("branch %s of the %s's pull request is not on %s", c.Branch, verb, cp.repo)
	}
	return nil
}

// assertFile checks the commit carries file.
func (a *commitAnswer) assertFile(file string) error {
	for _, f := range a.Commit.Files {
		if f.Path == file {
			return nil
		}
	}
	return fmt.Errorf("the commit's files %s do not carry %s", a.files(), file)
}

// files names the commit's files with their actions.
func (a *commitAnswer) files() string {
	names := make([]string, 0, len(a.Commit.Files))
	for _, f := range a.Commit.Files {
		names = append(names, f.Action+" "+f.Path)
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// followBranch points the GitRepository at branch and records both agents
// in the Kustomization's inventory, as kustomize-controller would after
// applying that branch.
func (cp *commitProof) followBranch(branch string) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	body := fmt.Appendf(nil, `{"spec":{"ref":{"branch":%q}}}`, branch)
	if err := patchObject(ctx, gitRepositoryGVR, platformNamespace, gitopsFluxFixture, types.MergePatchType, body); err != nil {
		return fmt.Errorf("pointing the GitRepository %s/%s at %s: %w", platformNamespace, gitopsFluxFixture, branch, err)
	}
	var objs []*unstructured.Unstructured
	for _, name := range []string{commitSeedAgent, commitTestAgent} {
		obj, err := readHelmReleaseObject(name)
		if err != nil {
			return err
		}
		objs = append(objs, obj)
	}
	status, err := inventoryStatus(objs...)
	if err != nil {
		return err
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	if _, err := k.resource(kustomizationGVR, platformNamespace).Patch(ctx, gitopsFluxFixture, types.MergePatchType, status,
		metav1.PatchOptions{FieldManager: applyFieldManager}, crStatus); err != nil {
		return fmt.Errorf("recording the inventory of %s: %w", describe(kustomizationGVR, platformNamespace, gitopsFluxFixture), err)
	}
	note("GitRepository %s/%s at %s; Kustomization %s/%s lists %s and %s", platformNamespace, gitopsFluxFixture, branch, platformNamespace, gitopsFluxFixture, commitSeedAgent, commitTestAgent)
	return nil
}

// cleanup deletes the stage's branches on the repository (closing their
// pull requests), its agents, the Kustomization and the GitRepository.
// Leftovers of an aborted run included; every failure is a note.
func (cp *commitProof) cleanup() {
	if left, err := cp.deleteBranches(); err != nil {
		note("cleanup: deleting the commit stage's branches on %s: %v", cp.repo, err)
	} else if len(left) > 0 {
		note("cleanup: deleted %s on %s, closing their pull requests", strings.Join(left, ", "), cp.repo)
	}
	for _, name := range []string{commitTestAgent, commitSeedAgent} {
		if !agentExists(name) {
			continue
		}
		if err := removeAgent(name); err != nil {
			note("cleanup: %v", err)
		}
	}
	if cp.removeKustomization != nil {
		cp.removeKustomization()
		cp.removeKustomization = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*gitopsFixtureWait)
	defer cancel()
	if err := deleteObject(ctx, gitRepositoryGVR, platformNamespace, gitopsFluxFixture, gitopsFixtureWait); err != nil {
		note("cleanup: deleting the GitRepository %s/%s: %v", platformNamespace, gitopsFluxFixture, err)
	}
}

// deleteBranches deletes every branch agent-manager opened for the stage's
// agent on the repository and names them.
func (cp *commitProof) deleteBranches() ([]string, error) {
	out, err := personGit(StateDir, "ls-remote", cp.remote(), "refs/heads/"+commitBranchPrefix+"*")
	if err != nil {
		return nil, err
	}
	branches := commitStageBranches(out)
	if len(branches) == 0 {
		return nil, nil
	}
	// git push needs a repository to run in, even for a deletion: an empty
	// one of its own, removed again.
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(StateDir, "commit-proof-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if _, err := personGit(dir, "init", "-q"); err != nil {
		return nil, err
	}
	args := []string{gitPush, "-q", cp.remote(), "--delete"}
	for _, b := range branches {
		args = append(args, "refs/heads/"+b)
	}
	if _, err := personGit(dir, args...); err != nil {
		return nil, err
	}
	return branches, nil
}

// commitStageBranches are the branches of an ls-remote listing that
// agent-manager opens for the stage's agent, any verb.
func commitStageBranches(lsRemote string) []string {
	suffix := "-" + kagentNamespace + "-" + commitTestAgent
	var branches []string
	for line := range strings.Lines(lsRemote) {
		_, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		b := strings.TrimPrefix(ref, "refs/heads/")
		if strings.HasPrefix(b, commitBranchPrefix) && strings.HasSuffix(b, suffix) {
			branches = append(branches, b)
		}
	}
	slices.Sort(branches)
	return branches
}

// defaultBranchOf reads `git ls-remote --symref <remote> HEAD`'s symbolic
// ref line: the remote's default branch.
func defaultBranchOf(lsRemote string) string {
	for line := range strings.Lines(lsRemote) {
		if ref, ok := strings.CutPrefix(strings.TrimSpace(line), "ref: refs/heads/"); ok {
			branch, _, _ := strings.Cut(ref, "\t")
			return branch
		}
	}
	return ""
}

// gitRepositoryManifest is the fixture's GitRepository: the repository at
// branch, suspended (source-controller fetches nothing; agent-manager reads
// only its URL and branch), labelled agentlab's.
func gitRepositoryManifest(repo, branch string) []byte {
	return fmt.Appendf(nil, `apiVersion: %s
kind: GitRepository
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
spec:
  interval: 1h
  suspend: true
  url: %s/%s
  ref:
    branch: %s
`, gitRepositoryGVR.GroupVersion(), gitopsFluxFixture, platformNamespace, managedByLabel, managedByAgentlabValue, measureGitURL, repo, branch)
}

// agentManagerCommitWired says whether the lab's agent-manager Deployment
// runs with githubTokenExchange (the flag the chart renders from it).
func agentManagerCommitWired() (bool, error) {
	k, err := labKube()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	d, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, agentManagerMCPServer, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("reading Deployment %s/%s: %w", platformNamespace, agentManagerMCPServer, err)
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		for _, a := range c.Args {
			if strings.HasPrefix(a, "--github-token-exchange-endpoint=") {
				return true, nil
			}
		}
	}
	return false, nil
}
