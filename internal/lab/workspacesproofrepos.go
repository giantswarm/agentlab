package lab

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The provider side of the sync and sessions parts: an instance's two proof
// repositories on the real provider (platform.workspaces.<instance>
// .proofRepositories), the first pushed and force-pushed to on a scratch
// branch from a clone on this host, the second left as it is, and the
// person's grant of the instance's App in muster. No fake stands in for the
// provider: a fetch is read from the volume (a mirror's FETCH_HEAD, which
// only git fetch writes) and from the Sessions' setup records, never from
// the provider's side.

const (
	// proofBranch is the scratch branch the parts push to on the pushed
	// repository, deleted at the end of every run and at the start of the
	// next: the default branch stays untouched, whatever protects it.
	proofBranch = "agentlab-workspaces-proof"
	// proofGrantWait bounds the wait for a person's grant of a GitHub App,
	// which only a browser signed in to GitHub gives.
	proofGrantWait = 15 * time.Minute
	// proofGrantPoll is how often the wait asks list_providers: each call
	// before the grant is an authentication attempt muster rate-limits.
	proofGrantPoll = 30 * time.Second
	// proofGrantFile is where a part writes muster's sign-in link it waits
	// on, in the state directory, for a browser to open.
	proofGrantFile = "workspaces-grant-%s.url"
	// gitPush is git's push command.
	gitPush = "push"
)

// proofRepos is the instance and its proof repositories, and the host's
// clone of the pushed one.
type proofRepos struct {
	instance string
	// pushed is the repository the part pushes to, still the one it leaves.
	pushed, still string
	// remote is a repository's git URL at the provider.
	remote func(repo string) string
	// git runs git on this host with the person's credentials at the
	// provider, never prompting.
	git func(dir string, args ...string) (string, error)
	// dir is the clone of pushed, start its default branch's head.
	dir, start string
	// pushes are the commits the run pushed to proofBranch, in order.
	pushes []string
}

// proofReposSkip says why the lab runs no sync, sessions or git part on the
// instance, "" when it does.
func proofReposSkip(cfg *config.Config, instance string) string {
	ws := cfg.Platform.Workspaces
	switch instance {
	case config.WorkspaceProviderGitHub:
		switch {
		case !cfg.WorkspaceGitHubProvider():
			return fmt.Sprintf("platform.workspaces.provider %q configures no instance %s", ws.Provider, instance)
		case len(ws.GitHub.ProofRepositories) == 0:
			return "platform.workspaces.github.proofRepositories names no repositories: two of the App's installation, the first one the person may push a scratch branch to (docs/workspaces.md)"
		}
	case config.WorkspaceProviderGitLab:
		if skip := gitlabSkip(cfg); skip != "" {
			return skip
		}
		if len(ws.GitLab.ProofRepositories) == 0 {
			return "platform.workspaces.gitlab.proofRepositories names no projects: two of the group, the first one the person pushes a scratch branch to (docs/workspaces.md)"
		}
	default:
		return fmt.Sprintf("no provider instance %q: --provider takes %s or %s", instance, config.WorkspaceProviderGitHub, config.WorkspaceProviderGitLab)
	}
	return ""
}

// newProofRepos is the instance's proof repositories with the git that
// reaches them from this host: GitHub through the person's own git
// credentials (their credential helper), GitLab as the instance's proof
// person, whose password the GitLab part reads from the instance's Secret
// and hands git through a credential helper of its environment alone.
func newProofRepos(cfg *config.Config, instance string) (*proofRepos, error) {
	ws := cfg.Platform.Workspaces
	pr := &proofRepos{instance: instance}
	switch instance {
	case config.WorkspaceProviderGitHub:
		pr.pushed, pr.still = ws.GitHub.ProofRepositories[0], ws.GitHub.ProofRepositories[1]
		pr.remote = func(repo string) string { return measureGitURL + "/" + repo + ".git" }
		pr.git = personGit
	case config.WorkspaceProviderGitLab:
		gl := ws.GitLab
		pr.pushed, pr.still = gl.ProofRepositories[0], gl.ProofRepositories[1]
		base := strings.TrimSuffix(gl.URL, "/")
		pr.remote = func(repo string) string { return base + "/" + repo + ".git" }
		password, err := (&gitlabProof{cfg: cfg}).personPassword()
		if err != nil {
			return nil, err
		}
		pr.git = func(dir string, args ...string) (string, error) {
			return loginGit(dir, gl.Person, password, args...)
		}
	}
	return pr, nil
}

// loginGitHelper is git's credential helper for a provider login: it
// answers from its environment alone.
const loginGitHelper = `!f() { test "$1" = get && echo "username=$PROOF_GIT_LOGIN" && echo "password=$PROOF_GIT_PASSWORD"; }; f`

// loginGit runs git as a provider login: the credential helper answers the
// login and its password from git's environment, never from its arguments
// or the person's own helpers.
func loginGit(dir, login, password string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "credential.helper=", "-c", "credential.helper=" + loginGitHelper}, args...)...) // #nosec G204 -- git with the part's own arguments
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "PROOF_GIT_LOGIN="+login, "PROOF_GIT_PASSWORD="+password)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, excerpt(strings.TrimSpace(string(out)), 300))
	}
	return strings.TrimSpace(string(out)), nil
}

// repos are both repositories, sorted.
func (pr *proofRepos) repos() []string {
	return slices.Sorted(slices.Values([]string{pr.pushed, pr.still}))
}

// sources select repos by name on the instance, one source per owner.
func (pr *proofRepos) sources(repos ...string) []map[string]any {
	byOwner := map[string][]string{}
	var owners []string
	for _, r := range repos {
		owner, name := splitRepo(r)
		if _, ok := byOwner[owner]; !ok {
			owners = append(owners, owner)
		}
		byOwner[owner] = append(byOwner[owner], name)
	}
	out := make([]map[string]any, 0, len(owners))
	for _, owner := range owners {
		out = append(out, map[string]any{wmProviderArg: pr.instance, wmOwnerArg: owner, repositoriesKey: byOwner[owner]})
	}
	return out
}

// splitRepo cuts a repository at its last slash: a GitLab project's group
// may be nested.
func splitRepo(repo string) (owner, name string) {
	i := strings.LastIndex(repo, "/")
	return repo[:i], repo[i+1:]
}

// clone makes a shallow clone of the pushed repository on this host,
// without a working tree or file contents, and removes the scratch branch an
// interrupted run left; it says whether there was one.
func (pr *proofRepos) clone() (bool, error) {
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return false, err
	}
	dir, err := os.MkdirTemp(StateDir, "workspaces-proof-push-")
	if err != nil {
		return false, err
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return false, err
	}
	pr.dir = filepath.Join(dir, "clone")
	if _, err := pr.git(dir, "clone", "-q", "--depth", "1", "--filter=blob:none", "--no-checkout", "--single-branch", pr.remote(pr.pushed), "clone"); err != nil {
		return false, fmt.Errorf("cloning %s, which the part pushes to with your git credentials: %w", pr.pushed, err)
	}
	if pr.start, err = pr.git(pr.dir, "rev-parse", "HEAD"); err != nil {
		return false, err
	}
	left, err := pr.head(pr.pushed, proofBranch)
	if err != nil || left == "" {
		return false, err
	}
	if err := pr.deleteBranch(); err != nil {
		return false, fmt.Errorf("deleting the scratch branch %s an interrupted run left on %s: %w", proofBranch, pr.pushed, err)
	}
	note("deleted the scratch branch %s an interrupted run left on %s (at %s)", proofBranch, pr.pushed, shortSHA(left))
	return true, nil
}

// push commits on parent, the tree unchanged, and pushes the commit to the
// scratch branch, forced when force; it returns the commit.
func (pr *proofRepos) push(parent string, force bool) (string, error) {
	msg := fmt.Sprintf("agentlab workspaces-test: push %d at %s", len(pr.pushes)+1, time.Now().UTC().Format(time.RFC3339Nano))
	commit, err := pr.git(pr.dir, "commit-tree", parent+"^{tree}", "-p", parent, "-m", msg)
	if err != nil {
		return "", err
	}
	args := []string{gitPush, "-q", gitOrigin, commit + ":refs/heads/" + proofBranch}
	if force {
		args = []string{gitPush, "-q", "--force", gitOrigin, commit + ":refs/heads/" + proofBranch}
	}
	if _, err := pr.git(pr.dir, args...); err != nil {
		return "", err
	}
	pr.pushes = append(pr.pushes, commit)
	verb := "pushed"
	if force {
		verb = "force-pushed"
	}
	note("%s %s to %s's %s", verb, shortSHA(commit), pr.pushed, proofBranch)
	return commit, nil
}

// deleteBranch deletes the scratch branch at the provider.
func (pr *proofRepos) deleteBranch() error {
	_, err := pr.git(pr.dir, gitPush, "-q", gitOrigin, "--delete", "refs/heads/"+proofBranch)
	return err
}

// head is a branch's head of the repository at the provider, "" without
// the branch.
func (pr *proofRepos) head(repo, branch string) (string, error) {
	out, err := pr.git(StateDir, "ls-remote", pr.remote(repo), "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	sha, _, _ := strings.Cut(out, "\t")
	return strings.TrimSpace(sha), nil
}

// defaultHead is the head of the repository's default branch at the
// provider.
func (pr *proofRepos) defaultHead(repo string) (string, error) {
	out, err := pr.git(StateDir, "ls-remote", pr.remote(repo), "HEAD")
	if err != nil {
		return "", err
	}
	sha, _, _ := strings.Cut(out, "\t")
	if sha = strings.TrimSpace(sha); sha == "" {
		return "", fmt.Errorf("%s answers no HEAD at the provider", repo)
	}
	return sha, nil
}

// restore deletes the scratch branch and the host's clone.
func (pr *proofRepos) restore() error {
	if pr.dir == "" {
		return nil
	}
	var err error
	if len(pr.pushes) > 0 {
		if err = pr.deleteBranch(); err == nil {
			note("deleted the scratch branch %s of %s", proofBranch, pr.pushed)
			pr.pushes = nil
		}
	}
	if rerr := os.RemoveAll(filepath.Dir(pr.dir)); rerr != nil && err == nil {
		err = rerr
	}
	pr.dir = ""
	return err
}

// grantProofProvider has the person hold the instance's grant in muster:
// a GitLab's walked through its sign-in form (connectWorkspaceProvider); a
// GitHub App's, which only a browser signed in to GitHub gives, waited for
// while muster's sign-in link is open there. The link is printed and
// written to the state directory for the browser step; the grant goes into
// the run's ledger and is dropped at the run's end.
func grantProofProvider(cfg *config.Config, p *workspacePerson, instance string, timeout time.Duration) error {
	if instance == config.WorkspaceProviderGitLab {
		gp := &gitlabProof{cfg: cfg}
		password, err := gp.personPassword()
		if err != nil {
			return err
		}
		hops, err := connectWorkspaceProvider(cfg, p, instance, &providerLogin{login: cfg.Platform.Workspaces.GitLab.Person, password: password})
		if err != nil {
			return err
		}
		note("%s holds the grant of %s: %s", p.user.Email, instance, hops)
		return nil
	}
	if err := workspaceManagerReceives(cfg, instance); err != nil {
		return err
	}
	link, err := p.grantLink(instance)
	if err != nil {
		return err
	}
	recordWorkspaceGrant(p, instance)
	if link == "" {
		note("%s holds the grant of the App of %s in muster already", p.user.Email, instance)
		return nil
	}
	path := filepath.Join(StateDir, fmt.Sprintf(proofGrantFile, instance))
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(link+"\n"), 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()
	wait := max(timeout, proofGrantWait)
	note("%s grants the App of %s in a browser signed in to GitHub: open muster's sign-in link (also in %s); waiting up to %s\n  %s", p.user.Email, instance, path, wait, link)
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	ticker := time.NewTicker(proofGrantPoll)
	defer ticker.Stop()
	for {
		status, err := p.providerStatus(instance)
		if err == nil && status.Connected {
			note("%s granted the App of %s: list_providers shows it connected", p.user.Email, instance)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not grant the App of %s within %s: muster's sign-in link was %s (last list_providers: %v, error %v)", p.user.Email, instance, wait, link, status, err)
		case <-ticker.C:
		}
	}
}
