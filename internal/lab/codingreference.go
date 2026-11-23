package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// `agentlab coding-test --part reference`: the coding use case's reference
// scenario (giantswarm/giantswarm#38890), on the real GitHub as the person.
// One agent writes, tests and builds a hello-world program and opens a draft
// pull request; a different agent reviews it and posts its findings. Both
// are created through agent-manager as the person with an explicit toolset
// and work in Sessions of one workspace, each started with the person's
// grant.
//
//  1. Preconditions: the workspace-manager, Session admission, the admin
//     holding the github instance's grant (a grant held before the run is
//     kept), the proof repository cloned on this host and an interrupted
//     run's branches deleted.
//  2. Agents: the builder on the claude Harness and the reviewer on the
//     kagent (Go ADK) Harness, each with its own toolset, both Ready.
//  3. A workspace over the proof repository, its first cycle.
//  4. Build: a Session of the builder with the go and github toolchains.
//     Its turn is a task, not a script: write the program and its test, run
//     go test and go build, run the binary, push a branch and open a draft
//     pull request. The verdict is read from GitHub and from this host, not
//     from the agent's word: the pull request is a draft by the person,
//     carries the program and its test, every commit is the person's, the
//     turn's tools ran the tests and the binary, and this host's go test and
//     go build pass on the pushed head.
//  5. Review: a Session of the reviewer with the github toolchain reads the
//     pull request and posts one review with concrete findings. GitHub holds
//     one COMMENTED review by the person whose body carries the reviewer's
//     Session and names a file of the pull request, no approval, and the
//     pull request is still open, unmerged and awaiting a review.
//  6. Boundaries: the reviewer's attempt to approve is refused by GitHub
//     (the same person cannot approve their own pull request); the
//     reviewer's token carries the person's repository permissions, which
//     the part reports as unenforced by the platform
//     (giantswarm/giantswarm#39105); a Session of the reviewer without a
//     grant is refused by GitHub.
//  7. Audit: each Session names its own Agent and the person as creator,
//     the two Sessions and Agents differ, and the pull request and the
//     review each carry the Session that made them.
//
// The Sessions, the Agents, the workspace, the pull request and its branch
// are removed on every path; grants the run created are dropped.

// CodingPartReference is the reference scenario part of coding-test.
const CodingPartReference = "reference"

const (
	referenceBuilderAgent  = "agentlab-ref-builder"
	referenceReviewerAgent = "agentlab-ref-reviewer"
	referenceWorkspace     = "agentlab-ref"
	// referenceBranchPrefix starts every branch the part's builder pushes,
	// deleted at the end of every run and at the start of the next.
	referenceBranchPrefix = "agentlab-ref-"
	// referenceAppRoot holds each run's program, apps/agentlab-hello-<run>.
	referenceAppRoot = "apps"
	// referenceTurnTimeout bounds each agent's task turn: writing, testing,
	// building and pushing take longer than a scripted probe.
	referenceTurnTimeout = 20 * time.Minute
	// referenceSessionMarker opens the line naming a Session in the pull
	// request's and the review's body.
	referenceSessionMarker = "agentlab-session:"
	// The marker lines the agents reply with.
	referenceProbePR         = "pr"
	referenceProbeApprove    = "approve"
	referenceProbeReviewed   = "reviewed"
	referenceProbePerms      = "permissions"
	referenceProbeNoGrant    = "no-grant-user"
	referenceReviewCommented = "COMMENTED"
	referenceReviewApproved  = "APPROVED"
	referenceDecisionPending = "REVIEW_REQUIRED"
	referenceBuilderPrompt   = "You are the builder agent of this test lab's reference coding scenario. You write small, correct Go programs with tests, verify them with the tools you have, and publish them as draft pull requests as the person who asked. Never merge, approve or close pull requests."
	referenceReviewerPrompt  = "You are the reviewer agent of this test lab's reference coding scenario. You read pull requests and post review comments with concrete, actionable findings, each naming a file and a line. You never approve, merge, close, push to or edit a pull request."
)

// referenceProof is the part's state.
type referenceProof struct {
	sp        *syncProof
	pr        *proofRepos
	timeout   time.Duration
	turn      time.Duration
	admin     *workspacePerson
	email     string
	api       *kagentAPI
	set       *sessionSet
	harness   string
	run       string
	branch    string
	appDir    string
	greeting  string
	login     string
	number    int
	builder   *apiv1alpha1.Session
	reviewer  *apiv1alpha1.Session
	agentUIDs map[string]string
	gaps      []string
}

// codingReferencePart runs the part as the admin.
func codingReferencePart(cfg *config.Config, opts CodingTestOptions) error {
	defer releaseWorkspaceGrants()
	run := randomHex(4)
	rp := &referenceProof{
		sp: newSyncProof(cfg, opts.ReadyTimeout, referenceWorkspace), timeout: opts.ReadyTimeout,
		turn: max(opts.TurnTimeout, referenceTurnTimeout), run: run,
		branch: referenceBranchPrefix + run, appDir: referenceAppRoot + "/agentlab-hello-" + run,
		greeting: "Hello from agentlab reference run " + run, agentUIDs: map[string]string{},
	}
	p := newProofStages()
	err := rp.runStages(p)
	rp.cleanup()
	if err == nil {
		fmt.Printf("\ncoding-test --part reference passed on GitHub: agent %s (Session %s, Harness %s) wrote, tested and built %s and opened draft pull request #%d on %s as %s; agent %s (Session %s, Harness %s) reviewed it with one COMMENTED review as %s; GitHub refused the reviewer's approval, the pull request still awaits a human review, and a reviewer Session without a grant was refused.\n",
			referenceBuilderAgent, rp.builder.GetId(), rp.harness, rp.appDir, rp.number, rp.pr.pushed, rp.login,
			referenceReviewerAgent, rp.reviewer.GetId(), kagentHarness, rp.login)
		for _, gap := range rp.gaps {
			fmt.Printf("not enforced by the platform: %s\n", gap)
		}
	}
	return p.close(err)
}

func (rp *referenceProof) runStages(p *proofStages) error {
	cfg := rp.sp.cfg
	instance := config.WorkspaceProviderGitHub
	p.begin("reference: preconditions", "The workspace-manager, Session admission, the admin holding the %s instance's grant, the proof repository cloned and an interrupted run's branches deleted", instance)
	if skip := proofReposSkip(cfg, instance); skip != "" {
		return fmt.Errorf("the reference part needs the %s instance's proof repositories: %s", instance, skip)
	}
	if err := rp.preconditions(instance); err != nil {
		return err
	}

	p.begin("reference: agents", "%s on %s and %s on %s through agent-manager as the admin, each with its own toolset, both Ready", referenceBuilderAgent, rp.harness, referenceReviewerAgent, kagentHarness)
	if err := rp.agents(); err != nil {
		return err
	}

	p.begin("reference: workspace", "create_workspace %s over %s, its first cycle", referenceWorkspace, rp.pr.pushed)
	if err := rp.workspace(); err != nil {
		return err
	}

	p.begin("reference: build", "The builder writes, tests and builds %s, pushes %s and opens a draft pull request as the person; verified on GitHub and on this host", rp.appDir, rp.branch)
	if err := rp.build(); err != nil {
		return err
	}

	p.begin("reference: review", "The reviewer reads #%d and posts one review with concrete findings as the person; verified on GitHub", rp.number)
	if err := rp.review(); err != nil {
		return err
	}

	p.begin("reference: boundaries", "GitHub refuses the reviewer's approval; the reviewer's repository permissions recorded; a reviewer Session without a grant refused")
	if err := rp.boundaries(); err != nil {
		return err
	}

	p.begin("reference: audit", "Each Session names its own Agent and the person as creator; the pull request and the review carry their Sessions")
	return rp.audit()
}

// preconditions signs the admin in, has them hold the instance's grant,
// and clones the pushed repository, deleting an interrupted run's branches.
func (rp *referenceProof) preconditions(instance string) error {
	s, cfg := rp.sp, rp.sp.cfg
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
		return fmt.Errorf("no Session admission on this lab (the chart takes no workspaces values, or platform.agents is off): the reference part starts Sessions on a workspace volume")
	}
	rp.harness = rig.ClaudeHarness
	adminUser := cfg.AdminUser()
	if adminUser == nil {
		return fmt.Errorf("the reference part needs an admin user in %s (`agentlab configure --defaults` writes one)", config.File)
	}
	rp.email = adminUser.Email
	var err error
	if rp.admin, err = openWorkspacePerson(cfg, adminUser); err != nil {
		return err
	}
	s.member, s.memberEmail = rp.admin.session, adminUser.Email
	if err := grantProofProvider(cfg, rp.admin, instance, rp.timeout); err != nil {
		return err
	}
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, adminUser.Email, adminUser.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	if rp.api, err = dialKagentAPI(cfg, token); err != nil {
		return err
	}
	rp.set = &sessionSet{api: rp.api, manager: rp.admin.session}
	if err := s.removeLeftovers(); err != nil {
		return err
	}
	for _, agent := range []string{referenceBuilderAgent, referenceReviewerAgent} {
		if agentExists(agent) {
			if err := removeAgent(agent); err != nil {
				return err
			}
		}
	}
	if rp.pr, err = newProofRepos(cfg, instance); err != nil {
		return err
	}
	if _, err := rp.pr.clone(); err != nil {
		return err
	}
	left, err := deleteBranchesWithPrefix(rp.pr, referenceBranchPrefix)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		note("deleted the branches an interrupted run left on %s: %s", rp.pr.pushed, strings.Join(left, ", "))
	}
	if rp.login, err = personGH("api", "user", "--jq", ".login"); err != nil {
		return fmt.Errorf("this host's gh, which verifies GitHub independently of the agents: %w", err)
	}
	note("%s, the repository both agents work on; this host verifies as %s", rp.pr.pushed, rp.login)
	return nil
}

// agents creates both agents through agent-manager as the admin and waits
// for them: the builder with the read-only preset, the reviewer with one
// read tool, so neither holds a muster write tool.
func (rp *referenceProof) agents() error {
	ctx := context.Background()
	modelConfig, err := agentManagerModelConfig(rp.admin.session)
	if err != nil {
		return err
	}
	specs := []agentSpec{
		{Name: referenceBuilderAgent, ModelConfig: modelConfig, Harness: rp.harness, SystemMessage: referenceBuilderPrompt,
			Toolset:     []string{codingTestToolset},
			Description: "Builder of `agentlab coding-test --part reference`; deleted by the same run."},
		{Name: referenceReviewerAgent, ModelConfig: modelConfig, Harness: kagentHarness, SystemMessage: referenceReviewerPrompt,
			Toolset:     []string{"tool:" + codingTestReadTool},
			Description: "Reviewer of `agentlab coding-test --part reference`; deleted by the same run."},
	}
	for _, spec := range specs {
		if err := waitHarnessReady(ctx, spec.Harness, rp.timeout); err != nil {
			return err
		}
		written, err := (agentManagerWriter{rp.admin.session}).createAgent(spec)
		if err != nil {
			return err
		}
		boot, err := waitAgentReadyOn(spec.Name, spec.Harness, rp.timeout)
		if err != nil {
			return err
		}
		if !boot.ready {
			return fmt.Errorf("agent %s is not Ready on Harness %s after %s: %s", spec.Name, spec.Harness, boot.elapsed.Round(time.Second), boot.reason)
		}
		obj, err := getAgentObject(kagentNamespace, spec.Name)
		if err != nil {
			return err
		}
		uid := string(obj.GetUID())
		rp.agentUIDs[spec.Name] = uid
		note("%s Ready on %s in %s (requestedBy %s, uid %s), toolset %s", spec.Name, spec.Harness, boot.elapsed.Round(time.Second), written.RequestedBy, uid, strings.Join(spec.Toolset, ","))
	}
	if rp.agentUIDs[referenceBuilderAgent] == rp.agentUIDs[referenceReviewerAgent] {
		return fmt.Errorf("the builder and the reviewer are one Agent (uid %s)", rp.agentUIDs[referenceBuilderAgent])
	}
	return nil
}

// workspace defines the workspace over the pushed repository as the admin
// and waits for its first cycle.
func (rp *referenceProof) workspace() error {
	s := rp.sp
	var created syncWorkspace
	if err := s.member.createLabWorkspace(map[string]any{
		nameKey: referenceWorkspace, wmOrganizationArg: config.WorkspaceOrganization, wmSourcesArg: rp.pr.sources(rp.pr.pushed),
	}, &created); err != nil {
		return err
	}
	d, err := s.waitCycle(referenceWorkspace, nil)
	if err != nil {
		return err
	}
	if r := d.Workspace.LastSync; r.Result != syncResultSucceed {
		return fmt.Errorf("the first cycle of %s ended %s: %s", referenceWorkspace, r.Result, r.Message)
	}
	note("created %s; first cycle %s", referenceWorkspace, d.Workspace.LastSync.Result)
	return nil
}

// startSession starts a Session of the agent, with the workspace's grant
// unless grant is false, tracked for removal.
func (rp *referenceProof) startSession(agent string, grant bool, toolchains ...string) (*apiv1alpha1.Session, error) {
	token := ""
	if grant {
		a, err := rp.admin.authorizeWorkspace(referenceWorkspace)
		if err != nil {
			return nil, err
		}
		if a.Outcome != authorizeGranted || a.Grant == "" {
			return nil, fmt.Errorf("authorize_workspace answered %s (%q), want granted", a.Outcome, a.Reason)
		}
		token = a.Grant
	}
	id, err := startAgentSession(rp.admin.session, agent, token, toolchains...)
	if err != nil {
		return nil, err
	}
	rp.set.add(id)
	ctx, cancel := context.WithTimeout(context.Background(), rp.timeout)
	defer cancel()
	return rp.api.getSession(ctx, id)
}

// build has the builder do the task and verifies its outcome on GitHub and
// on this host.
func (rp *referenceProof) build() error {
	session, err := rp.startSession(referenceBuilderAgent, true, toolchainGo, toolchainGitHub)
	if err != nil {
		return err
	}
	rp.builder = session
	ctx, cancel := context.WithTimeout(context.Background(), rp.turn)
	defer cancel()
	t, err := rp.api.turn(ctx, session, userMessage(referenceBuildTask(rp.pr.pushed, rp.appDir, rp.branch, rp.greeting, session.GetId())))
	if err != nil {
		return fmt.Errorf("the builder's turn on Session %s: %w", session.GetId(), err)
	}
	reply, err := t.completedText()
	if err != nil {
		return fmt.Errorf("the builder's turn on Session %s: %w", session.GetId(), err)
	}
	if rp.number, err = referenceReplyNumber(reply); err != nil {
		return fmt.Errorf("the builder's reply: %w", err)
	}
	if err := judgeReferenceBuildTools(t.toolOutputs(), rp.greeting); err != nil {
		return fmt.Errorf("the builder's tools on Session %s: %w", session.GetId(), err)
	}
	view, err := rp.pullRequest()
	if err != nil {
		return err
	}
	if err := judgeReferencePullRequest(view, rp.login, rp.branch, rp.appDir, session.GetId()); err != nil {
		return fmt.Errorf("pull request #%d: %w", rp.number, err)
	}
	if err := rp.verifyCommits(); err != nil {
		return err
	}
	if err := rp.verifyBuild(); err != nil {
		return err
	}
	note("%s: draft pull request #%d by %s from %s with %d files; every commit the person's; go test and go build pass on this host", session.GetId(), rp.number, view.Author.Login, rp.branch, len(view.Files))
	return nil
}

// referenceBuildTask is the builder's task: the program, its test, the
// checks, the push and the draft pull request, then one line naming it.
func referenceBuildTask(repo, appDir, branch, greeting, sessionID string) string {
	return fmt.Sprintf(`Task: add a hello-world Go program to the repository %[1]s in your working directory.

1. Create the branch %[3]s from origin/HEAD in ./%[1]s.
2. In ./%[1]s/%[2]s write a package main whose main prints exactly %[4]q followed by a newline, through a function that returns that string, and a table-driven test of that function in main_test.go. The directory belongs to the repository's existing Go module; do not create a new module.
3. Run go test ./%[2]s/... and go build -o /tmp/agentlab-hello ./%[2]s from ./%[1]s, then run /tmp/agentlab-hello. Fix the code until all three succeed.
4. Commit only %[2]s with the message "feat: add agentlab hello-world program", push the branch to origin, and open a draft pull request against the default branch with gh pr create --draft, titled "feat: add agentlab hello-world program", whose body describes the program and ends with the line "%[5]s %[6]s".
5. Reply with exactly one line "%[7]s %[8]s=<the pull request number>" and nothing else.`,
		repo, appDir, branch, greeting, referenceSessionMarker, sessionID, probeMarker, referenceProbePR)
}

// referenceReplyNumber reads the pull request number from the builder's
// reply.
func referenceReplyNumber(reply string) (int, error) {
	v := parseProbeLines(reply)
	raw, ok := v[referenceProbePR]
	if !ok {
		return 0, fmt.Errorf("no %s %s line: %s", probeMarker, referenceProbePR, excerpt(reply, 300))
	}
	number, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(raw), "#"))
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("%s %s=%q is no pull request number", probeMarker, referenceProbePR, raw)
	}
	return number, nil
}

// goTestPassLine is a line go test prints for a package whose tests passed.
var goTestPassLine = regexp.MustCompile(`(?m)^ok\s+\S+`)

// judgeReferenceBuildTools is the verdict on what the builder's tools
// returned: go test passed a package and the built binary printed the
// greeting. The agent's own words are not evidence.
func judgeReferenceBuildTools(outputs, greeting string) error {
	var problems []string
	if !goTestPassLine.MatchString(outputs) {
		problems = append(problems, "no tool output shows go test passing a package (an \"ok <package>\" line)")
	}
	if !strings.Contains(outputs, greeting) {
		problems = append(problems, fmt.Sprintf("no tool output carries the binary's greeting %q", greeting))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// referencePullRequestView is gh pr view's JSON for the fields the part
// judges.
type referencePullRequestView struct {
	Number         int    `json:"number"`
	State          string `json:"state"`
	IsDraft        bool   `json:"isDraft"`
	HeadRefName    string `json:"headRefName"`
	HeadRefOid     string `json:"headRefOid"`
	Body           string `json:"body"`
	ReviewDecision string `json:"reviewDecision"`
	Author         struct {
		Login string `json:"login"`
		IsBot bool   `json:"is_bot"`
	} `json:"author"`
	Files   []referenceFile   `json:"files"`
	Reviews []referenceReview `json:"reviews"`
}

// referenceFile is one file a pull request changes.
type referenceFile struct {
	Path string `json:"path"`
}

// referenceReview is one review as gh pr view lists it.
type referenceReview struct {
	State  string `json:"state"`
	Body   string `json:"body"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
}

// pullRequest reads the pull request from GitHub on this host.
func (rp *referenceProof) pullRequest() (*referencePullRequestView, error) {
	out, err := personGH("pr", "view", strconv.Itoa(rp.number), "--repo", rp.pr.pushed,
		"--json", "number,state,isDraft,headRefName,headRefOid,body,reviewDecision,author,files,reviews")
	if err != nil {
		return nil, err
	}
	var view referencePullRequestView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		return nil, fmt.Errorf("gh pr view #%d: %w", rp.number, err)
	}
	return &view, nil
}

// judgeReferencePullRequest is the verdict on the builder's pull request:
// open, a draft, by the person from the run's branch, carrying the program
// and its test and nothing outside the program's directory, its body naming
// the builder's Session.
func judgeReferencePullRequest(v *referencePullRequestView, login, branch, appDir, sessionID string) error {
	var problems []string
	if v.State != "OPEN" {
		problems = append(problems, "state "+v.State+", want OPEN")
	}
	if !v.IsDraft {
		problems = append(problems, "not a draft")
	}
	if v.Author.Login != login || v.Author.IsBot {
		problems = append(problems, fmt.Sprintf("author %s (bot %v), want the person %s", v.Author.Login, v.Author.IsBot, login))
	}
	if v.HeadRefName != branch {
		problems = append(problems, fmt.Sprintf("head %s, want %s", v.HeadRefName, branch))
	}
	var paths []string
	for _, f := range v.Files {
		paths = append(paths, f.Path)
		if !strings.HasPrefix(f.Path, appDir+"/") {
			problems = append(problems, fmt.Sprintf("changes %s outside %s", f.Path, appDir))
		}
	}
	for _, want := range []string{appDir + "/main.go", appDir + "/main_test.go"} {
		if !slices.Contains(paths, want) {
			problems = append(problems, "no "+want)
		}
	}
	if !strings.Contains(v.Body, referenceSessionMarker+" "+sessionID) {
		problems = append(problems, fmt.Sprintf("body does not name the builder's Session (%s %s)", referenceSessionMarker, sessionID))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// verifyCommits fetches the branch on this host and checks every commit
// of it beyond the default branch is the person's.
func (rp *referenceProof) verifyCommits() error {
	if _, err := rp.pr.git(rp.pr.dir, "fetch", "-q", gitOrigin, "+refs/heads/"+rp.branch+":refs/remotes/origin/"+rp.branch); err != nil {
		return err
	}
	out, err := rp.pr.git(rp.pr.dir, "log", "--format=%H %ae %ce", "origin/HEAD..origin/"+rp.branch)
	if err != nil {
		return err
	}
	return judgeReferenceCommits(out, rp.email)
}

// judgeReferenceCommits is the verdict on the branch's commits, one
// "<sha> <author email> <committer email>" line each: at least one, all
// authored and committed by the person.
func judgeReferenceCommits(log, email string) error {
	var problems []string
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		count++
		if fields[1] != email || fields[2] != email {
			problems = append(problems, fmt.Sprintf("%s authored by %s and committed by %s, want %s", shortSHA(fields[0]), fields[1], fields[2], email))
		}
	}
	if count == 0 {
		return errors.New("the branch carries no commit of its own")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// verifyBuild runs go test and go build of the program on this host, at
// the pushed head, in a worktree removed afterwards.
func (rp *referenceProof) verifyBuild() error {
	dir, err := os.MkdirTemp(StateDir, "reference-build-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	tree := filepath.Join(dir, "tree")
	if _, err := rp.pr.git(rp.pr.dir, "worktree", "add", "-q", "--detach", tree, "origin/"+rp.branch); err != nil {
		return err
	}
	defer func() { _, _ = rp.pr.git(rp.pr.dir, "worktree", "remove", "--force", tree) }()
	binary := filepath.Join(dir, "hello")
	for _, args := range [][]string{{"test", "./" + rp.appDir + "/..."}, {"build", "-o", binary, "./" + rp.appDir}} {
		cmd := exec.Command("go", args...) // #nosec G204 -- go with the proof's own arguments
		cmd.Dir = tree
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go %s on this host at %s: %w: %s", strings.Join(args, " "), rp.branch, err, excerpt(string(out), 400))
		}
	}
	out, err := exec.Command(binary).Output() // #nosec G204 -- the binary this proof just built
	if err != nil {
		return fmt.Errorf("running the program built on this host: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != rp.greeting {
		return fmt.Errorf("the program built on this host prints %q, want %q", got, rp.greeting)
	}
	return nil
}

// review has the reviewer review the pull request and verifies the review
// on GitHub.
func (rp *referenceProof) review() error {
	session, err := rp.startSession(referenceReviewerAgent, true, toolchainGitHub)
	if err != nil {
		return err
	}
	rp.reviewer = session
	ctx, cancel := context.WithTimeout(context.Background(), rp.turn)
	defer cancel()
	t, err := rp.api.turn(ctx, session, userMessage(referenceReviewTask(rp.pr.pushed, rp.number, session.GetId())))
	if err != nil {
		return fmt.Errorf("the reviewer's turn on Session %s: %w", session.GetId(), err)
	}
	reply, err := t.completedText()
	if err != nil {
		return fmt.Errorf("the reviewer's turn on Session %s: %w", session.GetId(), err)
	}
	if _, ok := parseProbeLines(reply)[referenceProbeReviewed]; !ok {
		return fmt.Errorf("the reviewer's reply carries no %s %s line: %s", probeMarker, referenceProbeReviewed, excerpt(reply, 300))
	}
	view, err := rp.pullRequest()
	if err != nil {
		return err
	}
	if err := judgeReferenceReview(view, rp.login, session.GetId()); err != nil {
		return fmt.Errorf("pull request #%d after the review: %w", rp.number, err)
	}
	note("%s: one COMMENTED review on #%d by %s naming a changed file; no approval; the pull request open, unmerged, %s", session.GetId(), rp.number, rp.login, view.ReviewDecision)
	return nil
}

// referenceReviewTask is the reviewer's task: read the pull request, post
// one comment review with concrete findings, then one line.
func referenceReviewTask(repo string, number int, sessionID string) string {
	return fmt.Sprintf(`Task: review pull request #%[2]d of %[1]s.

1. Read it with gh: gh pr view %[2]d --repo %[1]s and gh pr diff %[2]d --repo %[1]s.
2. Post exactly one review with gh pr review %[2]d --repo %[1]s --comment --body-file <file>. The body lists at least one concrete finding, each naming the changed file's path and a line number and saying what to change and why (correctness, tests, naming, or documentation). It ends with the line "%[3]s %[4]s".
3. Do not approve, request changes, merge, close, push to or edit the pull request.
4. Reply with exactly one line "%[5]s %[6]s=<the number of findings>" and nothing else.`,
		repo, number, referenceSessionMarker, sessionID, probeMarker, referenceProbeReviewed)
}

// judgeReferenceReview is the verdict on the pull request after the review:
// exactly one review, COMMENTED, by the person, its body naming the
// reviewer's Session and a file the pull request changes; no approval; the
// pull request open and still awaiting a review.
func judgeReferenceReview(v *referencePullRequestView, login, sessionID string) error {
	var problems []string
	var ours []referenceReview
	for _, r := range v.Reviews {
		if r.State == referenceReviewApproved {
			problems = append(problems, "approved by "+r.Author.Login)
		}
		if strings.Contains(r.Body, referenceSessionMarker+" "+sessionID) {
			ours = append(ours, r)
		}
	}
	switch {
	case len(ours) != 1:
		problems = append(problems, fmt.Sprintf("%d reviews name the reviewer's Session, want 1", len(ours)))
	default:
		r := ours[0]
		if r.State != referenceReviewCommented {
			problems = append(problems, "the review is "+r.State+", want "+referenceReviewCommented)
		}
		if r.Author.Login != login {
			problems = append(problems, fmt.Sprintf("the review is by %s, want the person %s", r.Author.Login, login))
		}
		if !slices.ContainsFunc(v.Files, func(f referenceFile) bool {
			return strings.Contains(r.Body, f.Path) || strings.Contains(r.Body, filepath.Base(f.Path)+":")
		}) {
			problems = append(problems, "the review names no file the pull request changes")
		}
	}
	if v.State != "OPEN" {
		problems = append(problems, "state "+v.State+", want OPEN")
	}
	if v.ReviewDecision != referenceDecisionPending {
		problems = append(problems, fmt.Sprintf("review decision %q, want %s: a human approval must still be required", v.ReviewDecision, referenceDecisionPending))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// boundaries runs the reviewer's approval attempt and its permission read
// in its Session, then a reviewer Session without a grant.
func (rp *referenceProof) boundaries() error {
	ctx, cancel := context.WithTimeout(context.Background(), rp.timeout)
	defer cancel()
	var v map[string]string
	if _, _, err := probeTurnWith(ctx, rp.api, rp.reviewer, referenceBoundaryCommand(rp.pr.pushed, rp.number), "", referenceBoundaryPurpose, func(reply string) error {
		v = parseProbeLines(reply)
		if _, ok := v[referenceProbePerms]; !ok {
			return fmt.Errorf("the reply carries no %s %s line: %s", probeMarker, referenceProbePerms, excerpt(reply, 300))
		}
		return nil
	}); err != nil {
		return err
	}
	gap, err := judgeReferenceBoundary(v)
	if err != nil {
		return fmt.Errorf("the reviewer's Session %s: %w", rp.reviewer.GetId(), err)
	}
	if gap != "" {
		rp.gaps = append(rp.gaps, gap)
		note("%s", gap)
	}
	view, err := rp.pullRequest()
	if err != nil {
		return err
	}
	for _, r := range view.Reviews {
		if r.State == referenceReviewApproved {
			return fmt.Errorf("pull request #%d carries an approval by %s after the refused attempt", rp.number, r.Author.Login)
		}
	}

	session, err := rp.startSession(referenceReviewerAgent, false, toolchainGitHub)
	if err != nil {
		return err
	}
	var w map[string]string
	if _, _, err := probeTurnWith(ctx, rp.api, session, referenceNoGrantCommand, "", referenceBoundaryPurpose, func(reply string) error {
		w = parseProbeLines(reply)
		if _, ok := w[referenceProbeNoGrant]; !ok {
			return fmt.Errorf("the reply carries no %s %s line: %s", probeMarker, referenceProbeNoGrant, excerpt(reply, 300))
		}
		return nil
	}); err != nil {
		return err
	}
	if rcOK(w[referenceProbeNoGrant]) {
		return fmt.Errorf("a reviewer Session without a grant (%s) read GitHub's user: %s", session.GetId(), w[referenceProbeNoGrant])
	}
	note("%s without a grant: gh api user refused (%s)", session.GetId(), w[referenceProbeNoGrant])
	return nil
}

// referenceBoundaryPurpose says what the boundary commands do, for the
// model that runs them.
const referenceBoundaryPurpose = "the command checks what GitHub lets this Session do on the lab's throwaway pull request: it attempts an approval GitHub is expected to refuse and reads the repository permissions; it prints only exit codes and the permissions."

// referenceBoundaryCommand attempts the approval and reads the person's
// permissions on the repository.
func referenceBoundaryCommand(repo string, number int) string {
	return gitShell +
		`try ` + referenceProbeApprove + ` ghc pr review ` + strconv.Itoa(number) + ` --repo '` + repo + `' --approve --body 'agentlab reference: approval attempt the proof expects GitHub to refuse'; ` +
		`m ` + referenceProbePerms + ` "$(ghc api 'repos/` + repo + `' --jq '.permissions|to_entries|map(select(.value)|.key)|join(",")' 2>&1)"`
}

// referenceNoGrantCommand reads GitHub's user from a Session without a
// grant.
const referenceNoGrantCommand = gitShell + `try ` + referenceProbeNoGrant + ` ghc api user --jq .login`

// judgeReferenceBoundary is the verdict on the reviewer's boundary
// commands: GitHub refused the approval. It returns, as a gap, the
// repository permissions the reviewer holds beyond reading and commenting,
// which the platform does not narrow.
func judgeReferenceBoundary(v map[string]string) (string, error) {
	if rcOK(v[referenceProbeApprove]) {
		return "", fmt.Errorf("GitHub accepted the reviewer's approval: %s", v[referenceProbeApprove])
	}
	perms := strings.Split(strings.TrimSpace(v[referenceProbePerms]), ",")
	var beyond []string
	for _, p := range perms {
		if p = strings.TrimSpace(p); p != "" && p != "pull" && p != "triage" {
			beyond = append(beyond, p)
		}
	}
	if len(beyond) == 0 {
		return "", nil
	}
	return fmt.Sprintf("the reviewer's Session holds the person's %s permission on the repository: the agent's toolset does not limit forge writes made with gh (giantswarm/giantswarm#39105); GitHub's own rules, not the platform, refused the approval", strings.Join(beyond, ",")), nil
}

// audit checks each Session against its Agent and the person, and that the
// two differ.
func (rp *referenceProof) audit() error {
	ctx, cancel := context.WithTimeout(context.Background(), rp.timeout)
	defer cancel()
	var problems []string
	for _, s := range []struct {
		session *apiv1alpha1.Session
		agent   string
	}{{rp.builder, referenceBuilderAgent}, {rp.reviewer, referenceReviewerAgent}} {
		got, err := rp.api.getSession(ctx, s.session.GetId())
		if err != nil {
			return err
		}
		if got.GetAgent().GetName() != s.agent {
			problems = append(problems, fmt.Sprintf("Session %s runs Agent %q, want %s", got.GetId(), got.GetAgent().GetName(), s.agent))
		}
		if got.GetCreator() != rp.email {
			problems = append(problems, fmt.Sprintf("Session %s's creator is %q, want the person %s", got.GetId(), got.GetCreator(), rp.email))
		}
		note("Session %s: Agent %s (uid %s), creator %s, %d provider bindings", got.GetId(), got.GetAgent().GetName(), rp.agentUIDs[s.agent], got.GetCreator(), len(got.GetProviders()))
	}
	if rp.builder.GetId() == rp.reviewer.GetId() {
		problems = append(problems, "the builder and the reviewer ran in one Session")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	rp.gaps = append(rp.gaps, "GitHub marks the pull request and the review as the person \"via\" the agents' App, but the commits carry no mark and nothing on GitHub names which agent or Session acted; the platform records that (Session, Agent, creator) and the agents write a Session line (giantswarm/giantswarm#39421)")
	return nil
}

// cleanup removes what the run made: its Sessions, its Agents, the
// workspace, the pull request and its branch.
func (rp *referenceProof) cleanup() {
	var errs []error
	if rp.set != nil {
		errs = append(errs, rp.set.removeAllNow())
	}
	for _, agent := range []string{referenceBuilderAgent, referenceReviewerAgent} {
		if agentExists(agent) {
			errs = append(errs, removeAgent(agent))
		}
	}
	errs = append(errs, rp.sp.restore())
	if rp.pr != nil && rp.pr.dir != "" {
		if rp.number > 0 {
			if _, err := personGH("pr", "close", strconv.Itoa(rp.number), "--repo", rp.pr.pushed, "--comment", "Closed by agentlab coding-test --part reference at the end of its run."); err != nil {
				errs = append(errs, err)
			}
		}
		if left, err := deleteBranchesWithPrefix(rp.pr, referenceBranchPrefix); err != nil {
			errs = append(errs, err)
		} else if len(left) > 0 {
			note("deleted the run's branches on %s: %s", rp.pr.pushed, strings.Join(left, ", "))
		}
		errs = append(errs, rp.pr.restore())
	}
	if rp.api != nil {
		rp.api.close()
	}
	if err := errors.Join(errs...); err != nil {
		warn("removing what the part left: %v", err)
	}
}
