package lab

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// `agentlab workspaces-test --part git`: git and gh as the person from a
// workspace Session, on the claude and the kagent (Go ADK) Harness, on the
// real GitHub through the github instance's App, as the admin who granted it
// in muster. No stand-in for GitHub: what happened at GitHub is read back
// from GitHub's own API by the Session, as the person.
//
//   - a workspace over the github instance's pushed proof repository
//     (platform.workspaces.github.proofRepositories, the first), its first
//     cycle;
//   - per Harness, a Session started through start_session with the admin's
//     grant: in the Session's clone a branch, a commit, git push, gh api user
//     (a person, not an App's bot), gh pr create, the pull request's author
//     and the push's actor (GitHub's repository activity) both the person,
//     the commit's committer the turn's person, the pull request closed and
//     its branch deleted; GH_TOKEN the Harness's inert placeholder and no
//     GitHub token in the environment, the Harness's data directory, the
//     session directory or the home directory;
//   - per Harness, the same Session asks the person a question with an
//     uncommitted file on a new branch, is suspended while it waits, and the
//     answer after the resume finds the same session directory (the volume,
//     the path, the setup record not set up again), the branch and the
//     uncommitted file;
//   - per Harness, a Session started without a workspace: gh api user and a
//     git push to the repository refused, no GitHub token anywhere it looks.
//
// The Sessions, the Agents, the workspace and every branch of the run are
// removed on every path; a branch an interrupted run left is deleted first.

// WorkspacesPartGit is the git part of workspaces-test.
const WorkspacesPartGit = "git"

const (
	gitWorkspace = "agentlab-git"
	// gitBranchPrefix starts every branch the part pushes, deleted at the
	// end of every run and at the start of the next.
	gitBranchPrefix = "agentlab-workspaces-git-"
	// gitProofFile is the file the push commits; gitPauseFile the one the
	// paused Session leaves uncommitted.
	gitProofFile = "agentlab-git-proof.txt"
	gitPauseFile = "agentlab-git-pause.txt"
	// The probe's lines.
	gitProbeClone        = "clone"
	gitProbePush         = "push"
	gitProbeCommitter    = "committer"
	gitProbeStart        = "start"
	gitProbeParent       = "parent"
	gitProbeGHUser       = "gh-user"
	gitProbePRCreate     = "pr-create"
	gitProbePRAuthor     = "pr-author"
	gitProbePushActor    = "push-actor"
	gitProbePRClose      = "pr-close"
	gitProbeGHToken      = "gh-token" // #nosec G101 -- a probe line's name, not a credential
	gitProbeTokenEnv     = "token-env"
	gitProbeTokenFiles   = "token-files"
	gitProbeTokenScanned = "token-scanned"
	gitProbeBranch       = "branch"
	gitProbePauseFile    = "pause-file"
	gitProbePauseStatus  = "pause-status"
	// What the gh-token line says of the Session's GH_TOKEN.
	gitTokenPlaceholder = "placeholder"
	gitTokenUnset       = "unset"
	gitTokenForeign     = "foreign"
	// gitUserType is the type GitHub gives a person's account; an App's
	// installation token answers /user with 403, its bot is type Bot.
	gitUserType = "User"
	// gitHubTokenPattern finds a GitHub token: a user's, an App's user or
	// installation token, OAuth, a fine-grained personal access token.
	gitHubTokenPattern = `gh[opsur]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,}` // #nosec G101 -- a pattern that finds tokens, not one
	// gitPurpose says what the part's commands do, for the model that runs
	// them.
	gitPurpose = "the command works in a throwaway branch of the lab's proof repository with git and gh as the person (a commit, a push, a pull request it closes again and the branch deleted), and prints only exit codes, logins and the names, never the values, of variables and files that hold a GitHub token."
)

// gitHarnessRun is one Harness's Session of the part and what it relayed.
type gitHarnessRun struct {
	harness, agent, session string
	branch                  string
	login                   string
	pr                      string
	directory               string
	setUp                   time.Time
	token                   string
}

// gitProof is the part's state.
type gitProof struct {
	sp      *syncProof
	pr      *proofRepos
	timeout time.Duration
	admin   *workspacePerson
	email   string
	api     *kagentAPI
	set     *sessionSet
	volume  workspaceVolume
	run     string
	runs    []*gitHarnessRun
}

// workspacesGitPart runs the part in stages, as the admin.
func workspacesGitPart(cfg *config.Config, timeout time.Duration) error {
	p := newProofStages()
	gp := &gitProof{sp: newSyncProof(cfg, timeout, gitWorkspace), timeout: timeout, run: randomHex(4)}
	err := gp.runStages(p)
	gp.cleanup()
	if err == nil && gp.pr != nil {
		var done []string
		for _, r := range gp.runs {
			done = append(done, fmt.Sprintf("%s (pull request %s by %s)", r.harness, r.pr, r.login))
		}
		fmt.Printf("\nWorkspaces git part passed on GitHub: from a Session in workspace %s on %s, git pushed and gh opened and closed a pull request on %s as the person, the push's actor and the pull request's author the person; no GitHub token in the sandbox beyond gh's placeholder; a Session paused on its question resumed in the same directory with its branch and uncommitted file; a Session without a workspace refused (gh api user and git push).\n",
			gitWorkspace, strings.Join(done, " and "), gp.pr.pushed)
	}
	return p.close(err)
}

// gitStages are the part's stages after its preconditions, skipped together
// without the github instance's proof repositories.
var gitStages = []string{"git: workspace", "git: push and pull request", "git: pause and resume", "git: no workspace"}

func (gp *gitProof) runStages(p *proofStages) error {
	cfg := gp.sp.cfg
	instance := config.WorkspaceProviderGitHub
	p.begin("git: preconditions", "The git part's preconditions on %s: its proof repositories, the workspace-manager, the Session admission, the Agents on the claude and the kagent Harness, the admin holding the instance's grant", instance)
	if skip := proofReposSkip(cfg, instance); skip != "" {
		p.skip("%s", skip)
		p.leaveOut(skip, gitStages...)
		return nil
	}
	if err := gp.preconditions(instance); err != nil {
		return err
	}

	p.begin("git: workspace", "create_workspace %s over %s, its first cycle", gitWorkspace, gp.pr.pushed)
	if err := gp.createWorkspace(); err != nil {
		return err
	}

	p.begin("git: push and pull request", "Per Harness, a Session started with the admin's grant: git push and gh pr create on %s as the person, the pull request closed and its branch deleted, no GitHub token in the sandbox", gp.pr.pushed)
	for _, h := range sessionsHarnesses {
		if err := gp.write(h.harness, h.agent); err != nil {
			return fmt.Errorf("on %s: %w", h.harness, err)
		}
	}

	p.begin("git: pause and resume", "Per Harness, the Session asks the person a question with an uncommitted file on a new branch, is suspended while it waits, and resumes for the answer in the same directory")
	for _, r := range gp.runs {
		if err := gp.pauseResume(r); err != nil {
			return fmt.Errorf("on %s: %w", r.harness, err)
		}
	}

	p.begin("git: no workspace", "Per Harness, a Session without a workspace: gh api user and a push to %s refused, no GitHub token", gp.pr.pushed)
	for _, h := range sessionsHarnesses {
		if err := gp.noWorkspace(h.harness, h.agent); err != nil {
			return fmt.Errorf("on %s: %w", h.harness, err)
		}
	}
	return nil
}

// preconditions signs the admin in to muster and kagent, has them hold the
// instance's grant, readies an Agent on each Harness and clones the pushed
// repository on this host, deleting the branches an interrupted run left.
func (gp *gitProof) preconditions(instance string) error {
	s, cfg := gp.sp, gp.sp.cfg
	ctx := context.Background()
	if reason, err := workspaceManagerSkip(cfg); err != nil {
		return err
	} else if reason != "" {
		return fmt.Errorf("no workspace-manager: %s", reason)
	}
	if err := waitMCPServerReachable(workspaceManagerMCPServer); err != nil {
		return fmt.Errorf("the workspace-manager is not reachable through muster: %w", err)
	}
	rig := workspaceAdmissionValuesFor(cfg, workspacesChartCarries(cfg))
	if rig == nil {
		return fmt.Errorf("no Session admission on this lab (the chart takes no workspaces values, or platform.agents is off): the git part starts Sessions on a workspace volume")
	}
	adminUser := cfg.AdminUser()
	if adminUser == nil {
		return fmt.Errorf("the git part needs an admin user in %s (`agentlab configure --defaults` writes one)", config.File)
	}
	gp.email = adminUser.Email
	var err error
	if gp.admin, err = openWorkspacePerson(cfg, adminUser); err != nil {
		return err
	}
	s.member, s.memberEmail = gp.admin.session, adminUser.Email
	if err := grantProofProvider(cfg, gp.admin, instance, gp.timeout); err != nil {
		return err
	}
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, adminUser.Email, adminUser.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	if gp.api, err = dialKagentAPI(cfg, token); err != nil {
		return err
	}
	gp.set = &sessionSet{api: gp.api, manager: gp.admin.session}
	if err := readySessionsAgents(ctx, rig, gp.timeout); err != nil {
		return err
	}
	if res := sideloadImages(cfg, hostPullImages([]string{workspacesTestGitImage})); res.n > 0 {
		note("side-loaded the volume probes' git image (%s)", res.d)
	}
	if err := s.removeLeftovers(); err != nil {
		return err
	}
	if gp.pr, err = newProofRepos(cfg, instance); err != nil {
		return err
	}
	if _, err := gp.pr.clone(); err != nil {
		return err
	}
	left, err := gp.deleteRunBranches()
	if err != nil {
		return err
	}
	if len(left) > 0 {
		note("deleted the branches an interrupted run left on %s: %s", gp.pr.pushed, strings.Join(left, ", "))
	}
	note("%s, the repository the Sessions push to; branches %s<run>-<harness>, deleted at the end", gp.pr.pushed, gitBranchPrefix)
	return nil
}

// createWorkspace defines the workspace over the pushed repository as the
// admin and waits for its first cycle.
func (gp *gitProof) createWorkspace() error {
	s := gp.sp
	var created syncWorkspace
	if err := s.member.createLabWorkspace(map[string]any{
		nameKey: gitWorkspace, wmOrganizationArg: config.WorkspaceOrganization, wmSourcesArg: gp.pr.sources(gp.pr.pushed),
	}, &created); err != nil {
		return err
	}
	d, err := s.waitCycle(gitWorkspace, nil)
	if err != nil {
		return err
	}
	if r := d.Workspace.LastSync; r.Result != syncResultSucceed {
		return fmt.Errorf("the first cycle of %s ended %s: %s", gitWorkspace, r.Result, r.Message)
	}
	if live := d.live(); !slices.Equal(live, []string{gp.pr.pushed}) {
		return fmt.Errorf("the first cycle of %s mirrored %v, want [%s]", gitWorkspace, live, gp.pr.pushed)
	}
	v, err := s.volume(gitWorkspace)
	if err != nil {
		return err
	}
	if gp.volume, err = workspaceVolumeOf(v); err != nil {
		return err
	}
	note("created %s of Organization %s; first cycle %s; volume %s", gitWorkspace, created.Organization, d.Workspace.LastSync.Result, gp.volume.Handle)
	return nil
}

// write starts the Harness's Session with a grant and runs the write turn.
func (gp *gitProof) write(harness, agent string) error {
	a, err := gp.admin.authorizeWorkspace(gitWorkspace)
	if err != nil {
		return err
	}
	if a.Outcome != authorizeGranted || a.Grant == "" {
		return fmt.Errorf("authorize_workspace answered %s (%q, unreadable %v, connect %v), want granted", a.Outcome, a.Reason, a.Unreadable, a.Connect)
	}
	id, err := startAgentSession(gp.admin.session, agent, a.Grant)
	if err != nil {
		return err
	}
	gp.set.add(id)
	r := &gitHarnessRun{harness: harness, agent: agent, session: id, branch: gitBranchPrefix + gp.run + "-" + harness, token: randomHex(8)}
	gp.runs = append(gp.runs, r)
	ctx, cancel := context.WithTimeout(context.Background(), gp.timeout)
	defer cancel()
	session, err := gp.api.getSession(ctx, id)
	if err != nil {
		return err
	}
	if vs := session.GetVolumeSource().GetVolume(); vs.GetVolumeHandle() != gp.volume.Handle {
		return fmt.Errorf("the Session %s is on volume %q, want the workspace's %s", id, vs.GetVolumeHandle(), gp.volume.Handle)
	}
	v, err := gp.probe(ctx, session, gitWriteCommand(gp.pr.pushed, r.branch), gitProbePRClose)
	if err != nil {
		return err
	}
	note("%s on %s: the clone at start %s; the commit's parent %s", id, harness, v[gitProbeStart], v[gitProbeParent])
	note("%s on %s: clone %s; push %s; committer %s; gh api user %s; gh pr create %s; author %s; push actor %s; close %s; GH_TOKEN %s",
		id, harness, v[gitProbeClone], v[gitProbePush], v[gitProbeCommitter], v[gitProbeGHUser], v[gitProbePRCreate], v[gitProbePRAuthor], v[gitProbePushActor], v[gitProbePRClose], v[gitProbeGHToken])
	login, pr, err := judgeGitWrite(v, gp.pr.pushed, gp.email)
	if err != nil {
		return fmt.Errorf("the Session %s: %w", id, err)
	}
	r.login, r.pr, r.directory = login, pr, v["pwd"]
	note("%s on %s: pushed %s and opened %s as %s, both read back from GitHub as %s, closed and its branch deleted; no GitHub token in the environment or %s files (GH_TOKEN the placeholder)", id, harness, r.branch, pr, login, login, v[gitProbeTokenScanned])
	return nil
}

// probe runs the command as a turn on the Session and returns its lines,
// requiring the last key.
func (gp *gitProof) probe(ctx context.Context, session *apiv1alpha1.Session, command, last string) (map[string]string, error) {
	var values map[string]string
	_, _, err := probeTurnWith(ctx, gp.api, session, command, "", gitPurpose, func(reply string) error {
		values = parseProbeLines(reply)
		if _, ok := values[last]; !ok {
			return fmt.Errorf("the reply carries no %s %s line (did the agent run its Bash tool?): %s", probeMarker, last, excerpt(reply, 300))
		}
		return nil
	})
	return values, err
}

// parseProbeLines reads a reply's marker lines, key to value.
func parseProbeLines(reply string) map[string]string {
	values := map[string]string{}
	for _, m := range probeLine.FindAllStringSubmatch(reply, -1) {
		values[m[1]] = strings.TrimSpace(strings.Trim(strings.TrimSpace(m[2]), "`"))
	}
	return values
}

// gitShell are the shell functions the part's commands share: a marker
// line, an attempt's exit code and its output's last lines, gh against
// github.com that never prompts, and the GitHub token scan: find and grep
// as BusyBox has them too, with the number of files scanned, so a scan
// that read nothing is told from one that found nothing.
const gitShell = `m() { echo "` + probeMarker + ` $1=$(printf '%s' "$2" | tr '\n' ' ' | sed 's/ *$//')"; }; ` +
	`try() { k=$1; shift; o=$("$@" 2>&1); rc=$?; m "$k" "rc$rc $(printf '%s' "$o" | grep -v '^[[:space:]]*$' | tail -n 3)"; }; ` +
	`ghc() { GH_PROMPT_DISABLED=1 GH_NO_UPDATE_NOTIFIER=1 timeout 60 gh "$@"; }; ` +
	`case "${GH_TOKEN-unset}" in unset) t=` + gitTokenUnset + `;; ` + credentialPlaceholder + `) t=` + gitTokenPlaceholder + `;; *) t=` + gitTokenForeign + `;; esac; m ` + gitProbeGHToken + ` "$t"; ` +
	`m ` + gitProbeTokenEnv + ` "$(env | grep -E '` + gitHubTokenPattern + `' | cut -d= -f1 | sort -u)"; ` +
	`f() { find /data /workspace "$PWD" "$HOME" -name .git -prune -o -type f "$@" 2>/dev/null; }; ` +
	`m ` + gitProbeTokenScanned + ` "$(f -print | sort -u | wc -l)"; ` +
	`m ` + gitProbeTokenFiles + ` "$(f -exec grep -lE '` + gitHubTokenPattern + `' {} + | sort -u | head -n 5)"; `

// gitWriteCommand is the write turn: in the Session's clone of repo a
// branch from origin/HEAD, a commit, the push; gh's user, the pull request opened, its author
// and the push's actor read back from GitHub; the pull request closed with
// its branch deleted.
func gitWriteCommand(repo, branch string) string {
	return gitShell +
		`m pwd "$(pwd)"; ` +
		`if [ ! -d './` + repo + `/.git' ]; then m ` + gitProbeClone + ` ` + probeNone + `; m ` + gitProbePRClose + ` skipped; exit 0; fi; m ` + gitProbeClone + ` './` + repo + `'; ` +
		`cd './` + repo + `'; base=$(git symbolic-ref --short refs/remotes/origin/HEAD | sed 's|^origin/||'); ` +
		`m ` + gitProbeStart + ` "head=$(git rev-parse -q --verify HEAD || echo ` + probeNone + `) origin=$(git rev-parse -q --verify origin/HEAD || echo ` + probeNone + `) base=$base branch=$(git branch --show-current) shallow=$(git rev-parse --is-shallow-repository)"; ` +
		`git switch -q -c '` + branch + `' origin/HEAD && date -u > ` + gitProofFile + ` && git add ` + gitProofFile + ` && git -c commit.gpgsign=false commit -qm 'agentlab workspaces-test --part git'; ` +
		`m ` + gitProbeCommitter + ` "$(git log -1 --format=%ce)"; m ` + gitProbeParent + ` "$(git rev-parse -q --verify HEAD^ || echo ` + probeNone + `)"; ` +
		`try ` + gitProbePush + ` env GIT_TERMINAL_PROMPT=0 timeout 60 git push -q origin 'HEAD:refs/heads/` + branch + `'; ` +
		`try ` + gitProbeGHUser + ` ghc api user --jq '.login+" "+.type'; ` +
		`try ` + gitProbePRCreate + ` ghc pr create --repo '` + repo + `' --head '` + branch + `' --base "$base" --title 'agentlab workspaces-test --part git' --body 'Opened by agentlab workspaces-test --part git as the person; closed at once.'; ` +
		`try ` + gitProbePRAuthor + ` ghc pr view '` + branch + `' --repo '` + repo + `' --json author,url --jq '.author.login+" "+.url'; ` +
		`a=""; for i in 1 2 3 4 5 6; do a=$(ghc api 'repos/` + repo + `/activity?ref=refs/heads/` + branch + `&per_page=1' --jq '.[0].actor.login' 2>/dev/null); [ -n "$a" ] && break; sleep 5; done; m ` + gitProbePushActor + ` "$a"; ` +
		`try ` + gitProbePRClose + ` ghc pr close '` + branch + `' --repo '` + repo + `' --delete-branch`
}

// gitBareCommand is the turn of a Session without a workspace: gh's user
// and a push to repo from a repository of its own, both expected refused.
func gitBareCommand(repo, branch string) string {
	return gitShell +
		`m pwd "$(pwd)"; ` +
		`try ` + gitProbeGHUser + ` ghc api user --jq '.login+" "+.type'; ` +
		`d=$(mktemp -d); git -C "$d" init -q && git -C "$d" -c user.name=agentlab -c user.email=agentlab@lab.local -c commit.gpgsign=false commit -q --allow-empty -m agentlab; ` +
		`try ` + gitProbePush + ` env GIT_TERMINAL_PROMPT=0 timeout 60 git -C "$d" push -q 'https://github.com/` + repo + `.git' 'HEAD:refs/heads/` + branch + `'; rm -rf "$d"`
}

// gitPauseCommand leaves a file uncommitted on a new branch of the clone.
// Like every branch of the part's, it starts at origin/HEAD: the Harness
// clones with no working tree and no local branch, and an agent's first
// switch creates one from origin.
func gitPauseCommand(repo, branch, token string) string {
	return `git -C './` + repo + `' switch -q -c '` + branch + `' origin/HEAD && echo '` + token + `' > './` + repo + `/` + gitPauseFile + `'; ` + gitResumeCommand(repo)
}

// gitResumeCommand reads, from the Session's working directory, its clone's
// branch, the paused file and its status.
func gitResumeCommand(repo string) string {
	return `echo "` + probeMarker + ` pwd=$(pwd)"; ` +
		`echo "` + probeMarker + ` ` + gitProbeBranch + `=$(git -C './` + repo + `' branch --show-current)"; ` +
		`echo "` + probeMarker + ` ` + gitProbePauseFile + `=$(cat './` + repo + `/` + gitPauseFile + `' 2>/dev/null || echo ` + probeNone + `)"; ` +
		`echo "` + probeMarker + ` ` + gitProbePauseStatus + `=$(git -C './` + repo + `' status --porcelain -- ` + gitPauseFile + ` | tr ' ' '_')"`
}

// rcOK says whether an attempt's line reports exit code 0.
func rcOK(line string) bool {
	return line == rcSuccess || strings.HasPrefix(line, rcSuccess+" ")
}

// rcSuccess opens an attempt's line that exited 0.
const rcSuccess = "rc0"

// judgeGitWrite is the verdict on the write turn: a clone, no GitHub token
// in the sandbox and GH_TOKEN the placeholder, the commit's committer the
// turn's person, the push accepted, gh's user a person, the pull request
// opened by them and their push, closed again. It returns the login and the
// pull request's URL.
func judgeGitWrite(v map[string]string, repo, email string) (login, pr string, err error) {
	if err := judgeGitTokenScan(v); err != nil {
		return "", "", err
	}
	switch {
	case v[gitProbeClone] == "" || v[gitProbeClone] == probeNone:
		return "", "", fmt.Errorf("the Session's directory %s holds no clone of %s", v["pwd"], repo)
	case v[gitProbeGHToken] != gitTokenPlaceholder:
		return "", "", fmt.Errorf("GH_TOKEN is %s, want the Harness's placeholder %s, which the egress gateway replaces", v[gitProbeGHToken], credentialPlaceholder)
	case v[gitProbeCommitter] != email:
		return "", "", fmt.Errorf("the commit's committer is %q, want the turn's person %s", v[gitProbeCommitter], email)
	case !rcOK(v[gitProbePush]):
		return "", "", fmt.Errorf("git push to %s answered %q: the egress gateway set no person's credential on github.com", repo, v[gitProbePush])
	}
	user := strings.Fields(strings.TrimPrefix(v[gitProbeGHUser], rcSuccess))
	if !rcOK(v[gitProbeGHUser]) || len(user) != 2 || user[1] != gitUserType {
		return "", "", fmt.Errorf("gh api user answered %q, want a person's login and type %s: an App's installation token or none reached api.github.com", v[gitProbeGHUser], gitUserType)
	}
	login = user[0]
	if !rcOK(v[gitProbePRCreate]) {
		return "", "", fmt.Errorf("gh pr create answered %q", v[gitProbePRCreate])
	}
	author := strings.Fields(strings.TrimPrefix(v[gitProbePRAuthor], rcSuccess))
	if !rcOK(v[gitProbePRAuthor]) || len(author) != 2 {
		return "", "", fmt.Errorf("gh pr view answered %q, want the pull request's author and URL", v[gitProbePRAuthor])
	}
	pr = author[1]
	switch {
	case author[0] != login:
		return "", "", fmt.Errorf("the pull request %s is by %s, want %s: gh acted with someone else's credential", pr, author[0], login)
	case v[gitProbePushActor] != login:
		return "", "", fmt.Errorf("GitHub's activity names %q as the push's actor, want %s", v[gitProbePushActor], login)
	case !rcOK(v[gitProbePRClose]):
		return "", "", fmt.Errorf("closing the pull request %s answered %q", pr, v[gitProbePRClose])
	}
	return login, pr, nil
}

// judgeGitTokenScan is the verdict on the token scan: files scanned, and
// no GitHub token in the environment or in one of them.
func judgeGitTokenScan(v map[string]string) error {
	if n, err := strconv.Atoi(v[gitProbeTokenScanned]); err != nil || n == 0 {
		return fmt.Errorf("the token scan read no file (%q scanned): it proves nothing", v[gitProbeTokenScanned])
	}
	if v[gitProbeTokenEnv] != "" || v[gitProbeTokenFiles] != "" {
		return fmt.Errorf("a GitHub token in the sandbox: environment variables [%s], files [%s]; the person's token stays in muster and the egress gateway", v[gitProbeTokenEnv], v[gitProbeTokenFiles])
	}
	return nil
}

// judgeGitBare is the verdict on a Session without a workspace: gh's user
// and the push refused, no GitHub token anywhere it looked.
func judgeGitBare(v map[string]string) error {
	if err := judgeGitTokenScan(v); err != nil {
		return err
	}
	switch {
	case rcOK(v[gitProbeGHUser]):
		return fmt.Errorf("gh api user answered %q: a Session without a workspace reached GitHub as someone", v[gitProbeGHUser])
	case rcOK(v[gitProbePush]):
		return fmt.Errorf("the push answered %q: a Session without a workspace pushed", v[gitProbePush])
	}
	return nil
}

// pauseResume has the Session leave a file uncommitted on a new branch and
// ask the person, suspends it while it waits, and resumes it for the
// answer: the same volume, directory and setup, the branch and the file.
func (gp *gitProof) pauseResume(r *gitHarnessRun) error {
	ctx, cancel := context.WithTimeout(context.Background(), gp.timeout)
	defer cancel()
	session, err := gp.api.getSession(ctx, r.session)
	if err != nil {
		return err
	}
	branch := r.branch + "-pause"
	explain := "Then end your reply by asking me, in plain words and without a tool, whether to commit " + gitPauseFile + "; do not commit it."
	var before map[string]string
	if _, _, err := probeTurnWith(ctx, gp.api, session, gitPauseCommand(gp.pr.pushed, branch, r.token), explain, gitPurpose, func(reply string) error {
		before = parseProbeLines(reply)
		return judgeGitPaused(before, branch, r.token)
	}); err != nil {
		return err
	}
	if r.setUp, err = gp.setUp(r.session); err != nil {
		return err
	}
	suspended, err := gp.api.suspendSession(ctx, r.session)
	if err != nil {
		return err
	}
	if suspended.GetState() != apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED {
		return fmt.Errorf("the Session %s is %s after SuspendSession, want suspended", r.session, sessionState(suspended))
	}
	note("%s on %s: %s uncommitted on %s in %s, the question asked; suspended while the person answers", r.session, r.harness, gitPauseFile, branch, before["pwd"])
	resumed, err := gp.api.resumeSession(ctx, r.session)
	if err != nil {
		return err
	}
	if h := resumed.GetVolumeSource().GetVolume().GetVolumeHandle(); h != gp.volume.Handle {
		return fmt.Errorf("the resumed Session %s is on volume %q, want %s", r.session, h, gp.volume.Handle)
	}
	var after map[string]string
	first, _, err := probeTurnWith(ctx, gp.api, resumed, gitResumeCommand(gp.pr.pushed), "This is my answer: leave "+gitPauseFile+" uncommitted.", gitPurpose, func(reply string) error {
		after = parseProbeLines(reply)
		return judgeGitPaused(after, branch, r.token)
	})
	if err != nil {
		return err
	}
	if after["pwd"] != before["pwd"] {
		return fmt.Errorf("the resumed Session %s works in %s, before the pause in %s", r.session, after["pwd"], before["pwd"])
	}
	if first != nil && slices.ContainsFunc(first.notices, func(n string) bool { return strings.Contains(n, sessionsFreshNotice) }) {
		return fmt.Errorf("the resumed Session %s was told its directory was %s: %q", r.session, sessionsFreshNotice, strings.Join(first.notices, " "))
	}
	setUp, err := gp.setUp(r.session)
	if err != nil {
		return err
	}
	if !setUp.Equal(r.setUp) {
		return fmt.Errorf("the resumed Session %s's directory was set up again at %s, first at %s", r.session, setUp.UTC().Format(time.RFC3339), r.setUp.UTC().Format(time.RFC3339))
	}
	note("%s on %s resumed for the answer: volume %s, %s, set up once at %s; %s on %s, %s uncommitted (%s)", r.session, r.harness, gp.volume.Handle, after["pwd"], setUp.UTC().Format(time.RFC3339), after[gitProbeBranch], gitProbeBranch, gitPauseFile, after[gitProbePauseStatus])
	return nil
}

// judgeGitPaused checks the paused clone: the branch, the file with the
// token, untracked.
func judgeGitPaused(v map[string]string, branch, token string) error {
	switch {
	case v["pwd"] == "":
		return fmt.Errorf("the reply carries no %s pwd line", probeMarker)
	case v[gitProbeBranch] != branch:
		return fmt.Errorf("the clone is on %q, want %s", v[gitProbeBranch], branch)
	case v[gitProbePauseFile] != token:
		return fmt.Errorf("%s reads %q, want %s", gitPauseFile, v[gitProbePauseFile], token)
	case v[gitProbePauseStatus] != "??_"+gitPauseFile:
		return fmt.Errorf("git status of %s reads %q, want it untracked", gitPauseFile, v[gitProbePauseStatus])
	}
	return nil
}

// setUp is when the Session's directory was set up, from its setup record
// on the volume.
func (gp *gitProof) setUp(id string) (time.Time, error) {
	out, err := gp.sp.onVolume(gitWorkspace, "git-directory", sessionsDirectoriesScript)
	if err != nil {
		return time.Time{}, err
	}
	dirs, err := parseSessionDirectories(out)
	if err != nil {
		return time.Time{}, err
	}
	d, ok := dirs[id]
	if !ok || d.state == nil {
		return time.Time{}, fmt.Errorf("no directory with a setup record for the Session %s on the volume", id)
	}
	return d.state.SetUp, nil
}

// noWorkspace starts the Harness's Session without a grant and runs the
// bare turn.
func (gp *gitProof) noWorkspace(harness, agent string) error {
	id, err := startAgentSession(gp.admin.session, agent, "")
	if err != nil {
		return err
	}
	gp.set.add(id)
	ctx, cancel := context.WithTimeout(context.Background(), gp.timeout)
	defer cancel()
	session, err := gp.api.getSession(ctx, id)
	if err != nil {
		return err
	}
	if session.GetVolumeSource() != nil {
		return fmt.Errorf("the Session %s started without a workspace carries volume %q", id, session.GetVolumeSource().GetVolume().GetVolumeHandle())
	}
	v, err := gp.probe(ctx, session, gitBareCommand(gp.pr.pushed, gitBranchPrefix+gp.run+"-"+harness+"-bare"), gitProbePush)
	if err != nil {
		return err
	}
	if err := judgeGitBare(v); err != nil {
		return fmt.Errorf("the Session %s: %w", id, err)
	}
	note("%s on %s, no workspace: gh api user %s; push %s; GH_TOKEN %s; no GitHub token in the environment or %s files", id, harness, v[gitProbeGHUser], v[gitProbePush], v[gitProbeGHToken], v[gitProbeTokenScanned])
	return nil
}

// deleteRunBranches deletes every branch of the part's prefix on the pushed
// repository, with the person's own git credentials, and names them.
func (gp *gitProof) deleteRunBranches() ([]string, error) {
	return deleteBranchesWithPrefix(gp.pr, gitBranchPrefix)
}

// deleteBranchesWithPrefix deletes every branch of prefix on the pushed
// repository of pr's clone, with the person's own git credentials, and names
// them.
func deleteBranchesWithPrefix(pr *proofRepos, prefix string) ([]string, error) {
	out, err := pr.git(pr.dir, "ls-remote", gitOrigin, "refs/heads/"+prefix+"*")
	if err != nil {
		return nil, err
	}
	var branches []string
	for line := range strings.Lines(out) {
		if _, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok {
			branches = append(branches, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	if len(branches) == 0 {
		return nil, nil
	}
	args := []string{gitPush, "-q", gitOrigin, "--delete"}
	for _, b := range branches {
		args = append(args, "refs/heads/"+b)
	}
	if _, err := pr.git(pr.dir, args...); err != nil {
		return nil, err
	}
	return branches, nil
}

// cleanup removes what the part created on every path, once.
func (gp *gitProof) cleanup() {
	if gp.sp.restored {
		return
	}
	var errs []error
	if gp.set != nil {
		errs = append(errs, gp.set.removeAllNow())
	}
	errs = append(errs, removeSessionsAgents()...)
	errs = append(errs, gp.sp.restore())
	if gp.pr != nil && gp.pr.dir != "" {
		if left, err := gp.deleteRunBranches(); err != nil {
			errs = append(errs, err)
		} else if len(left) > 0 {
			note("deleted the run's branches left on %s: %s", gp.pr.pushed, strings.Join(left, ", "))
		}
		errs = append(errs, gp.pr.restore())
	}
	if gp.api != nil {
		gp.api.close()
	}
	if err := errors.Join(errs...); err != nil {
		warn("removing what the part left: %v", err)
	}
}
