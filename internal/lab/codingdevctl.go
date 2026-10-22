package lab

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// `agentlab coding-test --part devctl`: a coding agent waits for a pull
// request, merges it and waits for its release through devctl's MCP server
// (platform.devctl, devctl.go), headless, on the real GitHub as the person.
// No stand-in for GitHub or CircleCI: the repository's own CI and Auto
// Release decide every verdict, and the merge record is read back from
// GitHub as the person.
//
//  1. Preconditions: devctl's server reachable through muster, the admin
//     holding the github instance's grant (the one muster attaches to every
//     devctl call), the proof repository cloned on this host and the
//     branches an interrupted run left deleted.
//  2. The tools: the admin's muster lists x_devctl_pr_wait, _pr_merge and
//     _release_wait.
//  3. A claude agent created through agent-manager as the admin, its
//     toolset those three tools, Ready on the workspace-admitting claude
//     Harness; a workspace over the proof repository, its first cycle.
//  4. The pull request: a Session started with the admin's grant; one turn
//     pushes a branch and opens a pull request with gh as the person.
//  5. The waits: while the repository's CI runs on the head, a second turn
//     has the agent call pr_wait, then pr_merge (no release wait), then
//     release_wait, each once and each only after the verdict before it,
//     and read the merge record back as the person. Beside it this proof
//     calls pr_wait on the same head through muster as the person with a
//     progressToken: one call, its progress notifications delivered, its
//     green verdict past muster's per-call timeout.
//  6. The verdict: one call per tool in the agent's turn, in order, each
//     exit 0; the pull request merged by the person, on GitHub and in
//     pr_merge's answer.
//
// The Session, the agent, the workspace, the host clone and every branch of
// the run are removed on every path. On the codex and omp Harnesses the part
// is codingdevctlharness.go's.

// CodingPartDevctl is the devctl part of coding-test.
const CodingPartDevctl = "devctl"

// CodingParts are the parts `coding-test --part` takes.
var CodingParts = []string{CodingPartDevctl}

// DefaultDevctlProofRepository is the repository the part opens and merges
// pull requests on: generated CI with a job that takes ten minutes on a pull
// request's head, Auto Release, the agents App installed. Test use only.
const DefaultDevctlProofRepository = "giantswarm/devctl-mcp-proof"

const (
	devctlProofAgent     = "agentlab-devctl"
	devctlProofWorkspace = "agentlab-devctl"
	// devctlBranchPrefix starts every branch the part pushes; pr_merge
	// deletes it, the cleanup deletes what a failed run left.
	devctlBranchPrefix = "agentlab-devctl-"
	devctlProofFile    = "agentlab-devctl-proof.txt"
	// devctlWaitTimeout is every wait's timeout argument: above the proof
	// repository's ten-minute CI and its tag pipeline, below devctl's 55m
	// bound.
	devctlWaitTimeout = "40m"
	// devctlTurnTimeout bounds the waits' turn: the CI, the merge and the
	// tag pipeline in one turn.
	devctlTurnTimeout = 60 * time.Minute
	// The marker lines of the turns.
	devctlProbeUser     = "gh-user"
	devctlProbePush     = "push"
	devctlProbePRCreate = "pr-create"
	devctlProbePR       = "pr"
	devctlProbeMerged   = "merged"
	devctlSystemPrompt  = "You are a coding agent in a test lab. Run exactly the commands and tool calls you are given, in the order given, each once, and reply with the lines you are asked for, verbatim."
)

// The arguments of devctl's tools the part passes.
const (
	devctlRepositoryArg = "repository"
	devctlNumberArg     = "number"
	devctlTimeoutArg    = "timeout"
)

// The devctl commands the agent's turn calls, as devctl's envelope names
// them, in the order the turn calls them.
const (
	devctlCommandPRWait      = "pr wait"
	devctlCommandPRMerge     = "pr merge"
	devctlCommandReleaseWait = "release wait"
)

// devctlProofTools are the tools the agent's toolset selects, by the
// envelope command they answer.
var devctlProofTools = map[string]string{
	devctlCommandPRWait:      devctlToolPrefix + "pr_wait",
	devctlCommandPRMerge:     devctlToolPrefix + "pr_merge",
	devctlCommandReleaseWait: devctlToolPrefix + "release_wait",
}

var devctlCommandOrder = []string{devctlCommandPRWait, devctlCommandPRMerge, devctlCommandReleaseWait}

// devctlProof is the part's state.
type devctlProof struct {
	cfg        *config.Config
	repository string
	timeout    time.Duration
	sp         *syncProof
	pr         *proofRepos
	admin      *workspacePerson
	api        *kagentAPI
	set        *sessionSet
	harness    string
	branch     string
	// login is the person's GitHub login as gh reads it in the Session;
	// number the pull request the agent opened.
	login  string
	number int
	// musterTimeout is the devctl MCPServer's per-call timeout.
	musterTimeout time.Duration
}

// codingDevctlPart runs the part as the admin.
func codingDevctlPart(cfg *config.Config, opts CodingTestOptions) error {
	if !cfg.DevctlEnabled() {
		return fmt.Errorf("platform.devctl is off in %s — `agentlab configure --devctl` turns it on, then `agentlab platform`", config.File)
	}
	if rt := codingRuntimes[cmp.Or(opts.Harness, config.CodingRuntimeClaude)]; !rt.claude {
		return codingDevctlHarnessPart(cfg, opts, rt)
	}
	dp := newDevctlProof(cfg, opts)
	p := newProofStages()
	err := dp.run(p)
	dp.cleanup()
	if err == nil {
		fmt.Printf("\ncoding-test --part devctl passed on GitHub: a claude agent opened pull request #%d on %s as %s, waited for its CI with one x_devctl_pr_wait call, merged it with x_devctl_pr_merge and waited for its release with x_devctl_release_wait, each through muster as the person; the merge record names %s.\n",
			dp.number, dp.repository, dp.login, dp.login)
	}
	return p.close(err)
}

// newDevctlProof is the part's state before its run: the repository, a
// fresh branch.
func newDevctlProof(cfg *config.Config, opts CodingTestOptions) *devctlProof {
	return &devctlProof{
		cfg: cfg, repository: cmp.Or(opts.Repository, DefaultDevctlProofRepository), timeout: opts.ReadyTimeout,
		sp: newSyncProof(cfg, opts.ReadyTimeout, devctlProofWorkspace), branch: devctlBranchPrefix + randomHex(4),
	}
}

func (dp *devctlProof) run(p *proofStages) error {
	p.begin("devctl: preconditions", "devctl's server reachable through muster, the admin holding the %s instance's grant, %s cloned and an interrupted run's branches deleted", devctlGrantApp, dp.repository)
	if err := dp.preconditions(); err != nil {
		return err
	}

	p.begin("devctl: tools", "The admin's muster lists %s", strings.Join(slices.Sorted(maps.Values(devctlProofTools)), ", "))
	if err := dp.tools(); err != nil {
		return err
	}

	p.begin("devctl: agent and workspace", "A claude agent through agent-manager with the devctl tools as its toolset, Ready on %s; workspace %s over %s, its first cycle", dp.harness, devctlProofWorkspace, dp.repository)
	if err := dp.agent(); err != nil {
		return err
	}
	if err := dp.workspace(); err != nil {
		return err
	}

	p.begin("devctl: pull request", "A Session with the admin's grant: the agent pushes %s and opens a pull request on %s with gh as the person", dp.branch, dp.repository)
	session, err := dp.openPullRequest()
	if err != nil {
		return err
	}

	p.begin("devctl: waits", "The agent calls pr_wait, pr_merge and release_wait on #%d, each once after the verdict before it; beside it one pr_wait through muster with a progressToken", dp.number)
	return dp.waits(session)
}

// preconditions signs the admin in, has them hold the grant devctl takes,
// reads the server's per-call timeout and clones the proof repository.
func (dp *devctlProof) preconditions() error {
	cfg := dp.cfg
	if err := waitMCPServerReachable(devctlMCPServer); err != nil {
		return fmt.Errorf("devctl's server is not reachable through muster: %w", err)
	}
	timeout, err := mcpServerTimeout(devctlMCPServer)
	if err != nil {
		return err
	}
	dp.musterTimeout = timeout
	rig := workspaceAdmissionValuesFor(cfg, workspacesChartCarries(cfg))
	if rig == nil {
		return fmt.Errorf("no Session admission on this lab (the chart takes no workspaces values, or platform.agents is off): the part starts its Session on a workspace volume")
	}
	dp.harness = rig.ClaudeHarness
	adminUser := cfg.AdminUser()
	if adminUser == nil {
		return fmt.Errorf("the devctl part needs an admin user in %s (`agentlab configure --defaults` writes one)", config.File)
	}
	if dp.admin, err = openWorkspacePerson(cfg, adminUser); err != nil {
		return err
	}
	dp.sp.member, dp.sp.memberEmail = dp.admin.session, adminUser.Email
	if err := grantProofProvider(cfg, dp.admin, devctlGrantApp, dp.timeout); err != nil {
		return err
	}
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, adminUser.Email, adminUser.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	if dp.api, err = dialKagentAPI(cfg, token); err != nil {
		return err
	}
	dp.set = &sessionSet{api: dp.api, manager: dp.admin.session}
	if err := dp.sp.removeLeftovers(); err != nil {
		return err
	}
	if agentExists(devctlProofAgent) {
		if err := removeAgent(devctlProofAgent); err != nil {
			return err
		}
	}
	dp.pr = &proofRepos{instance: devctlGrantApp, pushed: dp.repository, remote: func(repo string) string { return measureGitURL + "/" + repo + ".git" }, git: personGit}
	if _, err := dp.pr.clone(); err != nil {
		return err
	}
	left, err := deleteBranchesWithPrefix(dp.pr, devctlBranchPrefix)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		note("deleted the branches an interrupted run left on %s: %s", dp.repository, strings.Join(left, ", "))
	}
	note("devctl's MCPServer answers within %s a call, each progress notification restarting it", dp.musterTimeout)
	return nil
}

// tools checks the admin's muster lists the part's devctl tools.
func (dp *devctlProof) tools() error {
	names, err := dp.admin.session.listTools()
	if err != nil {
		return err
	}
	var missing []string
	for _, tool := range devctlProofTools {
		if !slices.Contains(names, tool) {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("the admin's muster lists no %s (%d tools listed): is the MCPServer %s/%s Connected?", strings.Join(missing, ", "), len(names), platformNamespace, devctlMCPServer)
	}
	note("listed: %s", strings.Join(slices.Sorted(maps.Values(devctlProofTools)), ", "))
	return nil
}

// agent creates the coding agent through agent-manager as the admin, on
// agent-manager's default ModelConfig, its toolset the part's devctl tools,
// and waits for it.
func (dp *devctlProof) agent() error {
	var toolset []string
	for _, command := range devctlCommandOrder {
		toolset = append(toolset, "tool:"+devctlProofTools[command])
	}
	modelConfig, err := agentManagerModelConfig(dp.admin.session)
	if err != nil {
		return err
	}
	spec := agentSpec{Name: devctlProofAgent, ModelConfig: modelConfig, Harness: dp.harness, Toolset: toolset, SystemMessage: devctlSystemPrompt,
		Description: "Throwaway agent of agentlab coding-test --part devctl; deleted by the same run."}
	written, err := (agentManagerWriter{dp.admin.session}).createAgent(spec)
	if err != nil {
		return err
	}
	boot, err := waitAgentReadyOn(spec.Name, spec.Harness, dp.timeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return fmt.Errorf("agent %s is not Ready on Harness %s after %s: %s", spec.Name, spec.Harness, boot.elapsed.Round(time.Second), boot.reason)
	}
	note("%s Ready on %s in %s (requestedBy %s), toolset %s", spec.Name, spec.Harness, boot.elapsed.Round(time.Second), written.RequestedBy, strings.Join(toolset, ","))
	return nil
}

// workspace defines the workspace over the proof repository as the admin
// and waits for its first cycle.
func (dp *devctlProof) workspace() error {
	s := dp.sp
	var created syncWorkspace
	if err := s.member.createLabWorkspace(map[string]any{
		nameKey: devctlProofWorkspace, wmOrganizationArg: config.WorkspaceOrganization, wmSourcesArg: dp.pr.sources(dp.repository),
	}, &created); err != nil {
		return err
	}
	d, err := s.waitCycle(devctlProofWorkspace, nil)
	if err != nil {
		return err
	}
	if r := d.Workspace.LastSync; r.Result != syncResultSucceed {
		return fmt.Errorf("the first cycle of %s ended %s: %s", devctlProofWorkspace, r.Result, r.Message)
	}
	note("created %s; first cycle %s", devctlProofWorkspace, d.Workspace.LastSync.Result)
	return nil
}

// openPullRequest starts the Session with the admin's grant and has the
// agent push a branch and open a pull request as the person.
func (dp *devctlProof) openPullRequest() (*apiv1alpha1.Session, error) {
	session, err := dp.startSession()
	if err != nil {
		return nil, err
	}
	id := session.GetId()
	ctx, cancel := context.WithTimeout(context.Background(), dp.timeout)
	defer cancel()
	var v map[string]string
	if _, _, err := probeTurnWith(ctx, dp.api, session, devctlPullRequestCommand(dp.repository, dp.branch), "", devctlPurpose, func(reply string) error {
		v = parseProbeLines(reply)
		if _, ok := v[devctlProbePR]; !ok {
			return fmt.Errorf("the reply carries no %s %s line (did the agent run its Bash tool?): %s", probeMarker, devctlProbePR, excerpt(reply, 300))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	login, number, err := judgeDevctlPullRequest(v)
	if err != nil {
		return nil, fmt.Errorf("the Session %s: %w", id, err)
	}
	dp.login, dp.number = login, number
	note("%s: gh api user %s; push %s; gh pr create %s; #%d by %s", id, v[devctlProbeUser], v[devctlProbePush], v[devctlProbePRCreate], number, login)
	return session, nil
}

// startSession starts a Session of the agent on the workspace with the
// admin's grant, tracked for removal: the Session's actor is the person.
func (dp *devctlProof) startSession() (*apiv1alpha1.Session, error) {
	a, err := dp.admin.authorizeWorkspace(devctlProofWorkspace)
	if err != nil {
		return nil, err
	}
	if a.Outcome != authorizeGranted || a.Grant == "" {
		return nil, fmt.Errorf("authorize_workspace answered %s (%q), want granted", a.Outcome, a.Reason)
	}
	id, err := startAgentSession(dp.admin.session, devctlProofAgent, a.Grant)
	if err != nil {
		return nil, err
	}
	dp.set.add(id)
	ctx, cancel := context.WithTimeout(context.Background(), dp.timeout)
	defer cancel()
	return dp.api.getSession(ctx, id)
}

// devctlPurpose says what the part's first command does, for the model
// that runs it.
const devctlPurpose = "the command pushes a throwaway branch of the lab's proof repository with git and opens a pull request with gh as the person, and prints only exit codes, the login and the pull request."

// devctlPullRequestCommand is the first turn: gh's user, a branch from
// origin/HEAD with one file, its push and the pull request.
func devctlPullRequestCommand(repo, branch string) string {
	return gitShell +
		`try ` + devctlProbeUser + ` ghc api user --jq '.login+" "+.type'; ` +
		`cd './` + repo + `' && base=$(git symbolic-ref --short refs/remotes/origin/HEAD | sed 's|^origin/||') && ` +
		`git switch -q -c '` + branch + `' origin/HEAD && date -u > ` + devctlProofFile + ` && git add ` + devctlProofFile + ` && git -c commit.gpgsign=false commit -qm 'chore: agentlab coding-test --part devctl'; ` +
		`try ` + devctlProbePush + ` env GIT_TERMINAL_PROMPT=0 timeout 60 git push -q origin 'HEAD:refs/heads/` + branch + `'; ` +
		`try ` + devctlProbePRCreate + ` ghc pr create --repo '` + repo + `' --head '` + branch + `' --base "$base" --title 'chore: agentlab coding-test --part devctl' --body 'Opened by agentlab coding-test --part devctl as the person; merged by the same run through devctl'"'"'s MCP server.'; ` +
		`m ` + devctlProbePR + ` "$(ghc pr view '` + branch + `' --repo '` + repo + `' --json number,author --jq '(.number|tostring)+" "+.author.login' 2>&1)"`
}

// judgeDevctlPullRequest is the verdict on the first turn: gh's user a
// person, the push and the pull request made, its author that person. It
// returns the login and the pull request's number.
func judgeDevctlPullRequest(v map[string]string) (string, int, error) {
	user := strings.Fields(strings.TrimPrefix(v[devctlProbeUser], rcSuccess))
	if !rcOK(v[devctlProbeUser]) || len(user) != 2 || user[1] != gitUserType {
		return "", 0, fmt.Errorf("gh api user answered %q, want a person's login of type %s", v[devctlProbeUser], gitUserType)
	}
	login := user[0]
	if !rcOK(v[devctlProbePush]) {
		return "", 0, fmt.Errorf("git push answered %q", v[devctlProbePush])
	}
	if !rcOK(v[devctlProbePRCreate]) {
		return "", 0, fmt.Errorf("gh pr create answered %q", v[devctlProbePRCreate])
	}
	fields := strings.Fields(v[devctlProbePR])
	if len(fields) != 2 {
		return "", 0, fmt.Errorf("gh pr view answered %q, want the number and the author", v[devctlProbePR])
	}
	number, err := strconv.Atoi(fields[0])
	if err != nil || number <= 0 {
		return "", 0, fmt.Errorf("gh pr view answered %q, want the number and the author", v[devctlProbePR])
	}
	if fields[1] != login {
		return "", 0, fmt.Errorf("pull request #%d's author is %s, want the person %s", number, fields[1], login)
	}
	return login, number, nil
}

// devctlWaitsPrompt is the second turn: the three tools in order, each once
// and each only after the verdict before it, then the merge record read
// back as the person.
func devctlWaitsPrompt(repo string, number int, shellTool string) string {
	call := func(command string, args string) string {
		return fmt.Sprintf("call muster's call_tool with name %s and arguments %s", devctlProofTools[command], args)
	}
	return fmt.Sprintf(`Work on pull request #%[1]d of %[2]s through the devctl tools on your muster server. Each devctl tool answers a JSON document with exitCode and verdict. A wait blocks until its verdict and reports progress meanwhile: call each tool exactly once, never poll, never call one again, never sleep.

1. %[3]s.
2. Only if that answer's exitCode is 0: %[4]s.
3. Only if that answer's exitCode is 0: %[5]s.
4. Then run this exact command once with your %[6]s:

%[7]s

Reply with one line per tool you called, "<tool> exitCode=<n> verdict=<verdict>", then every output line that starts with %[8]s, verbatim. If an answer's exitCode is not 0, stop there and reply with its reason.`,
		number, repo,
		call(devctlCommandPRWait, fmt.Sprintf(`{"repository": %q, "number": %d, "timeout": %q}`, repo, number, devctlWaitTimeout)),
		call(devctlCommandPRMerge, fmt.Sprintf(`{"repository": %q, "number": %d, "timeout": %q, "noReleaseWait": true}`, repo, number, devctlWaitTimeout)),
		call(devctlCommandReleaseWait, fmt.Sprintf(`{"repository": %q, "pr": %d, "timeout": %q}`, repo, number, devctlWaitTimeout)),
		shellTool, devctlMergedCommand(repo, number), probeMarker)
}

// devctlMergedCommand reads the pull request's merge record back from
// GitHub as the person.
func devctlMergedCommand(repo string, number int) string {
	return gitShell + `m ` + devctlProbeMerged + ` "$(ghc pr view ` + strconv.Itoa(number) + ` --repo '` + repo + `' --json state,mergedBy,mergeCommit --jq '.state+" "+.mergedBy.login+" "+.mergeCommit.oid' 2>&1)"`
}

// devctlProgressCall is the proof's own pr_wait through muster as the
// person, with a progressToken: its answer and its progress.
type devctlProgressCall struct {
	env     *toolEnvelope
	notes   []progressNote
	elapsed time.Duration
	err     error
}

// waits runs the agent's turn and, beside it, the proof's own pr_wait with a
// progressToken, and judges both.
func (dp *devctlProof) waits(session *apiv1alpha1.Session) error {
	progress := make(chan devctlProgressCall, 1)
	go func() { progress <- dp.progressWait() }()

	note("turn on Session %s, deadline %s", session.GetId(), devctlTurnTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), devctlTurnTimeout)
	defer cancel()
	start := time.Now()
	t, err := dp.api.turn(ctx, session, userMessage(devctlWaitsPrompt(dp.repository, dp.number, "Bash tool")))
	if err != nil {
		return fmt.Errorf("the waits' turn on Session %s after %s: %w", session.GetId(), time.Since(start).Round(time.Second), err)
	}
	reply, err := t.completedText()
	if err != nil {
		return fmt.Errorf("the waits' turn on Session %s: %w", session.GetId(), err)
	}
	note("the waits' turn ended after %s", time.Since(start).Round(time.Second))
	calls := devctlCalls(t.tools)
	for _, c := range calls {
		note("the agent called %s: exitCode %d, verdict %s", c.command, c.exitCode, c.verdict)
	}
	merged := parseProbeLines(reply)[devctlProbeMerged]
	agentErr := judgeDevctlWaits(calls, merged, dp.login)

	pc := <-progress
	progressErr := judgeDevctlProgress(pc, dp.musterTimeout)
	if pc.err == nil {
		note("the proof's own %s through muster: verdict in one call after %s, %d progress notifications (last: %q)", devctlProofTools[devctlCommandPRWait], pc.elapsed.Round(time.Second), len(pc.notes), lastProgressMessage(pc.notes))
	}
	if agentErr != nil {
		return fmt.Errorf("the agent's turn: %w\nits reply: %s", agentErr, excerpt(reply, 600))
	}
	return progressErr
}

// progressWait calls pr_wait on the agent's pull request through muster as
// the admin, on a session of its own, with a progressToken.
func (dp *devctlProof) progressWait() devctlProgressCall {
	adminUser := dp.cfg.AdminUser()
	person, err := openWorkspacePerson(dp.cfg, adminUser)
	if err != nil {
		return devctlProgressCall{err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), devctlTurnTimeout)
	defer cancel()
	start := time.Now()
	env, notes, err := person.session.callToolProgress(ctx, devctlProofTools[devctlCommandPRWait],
		map[string]any{devctlRepositoryArg: dp.repository, devctlNumberArg: dp.number, devctlTimeoutArg: devctlWaitTimeout}, "agentlab-devctl-"+randomHex(4))
	return devctlProgressCall{env: env, notes: notes, elapsed: time.Since(start), err: err}
}

// devctlCall is one devctl answer among a turn's tool results.
type devctlCall struct {
	command  string
	exitCode int
	verdict  string
	mergedBy string
}

// devctlEnvelope finds devctl's JSON documents in a tool's output, however
// deep the harness and muster quoted them.
var devctlEnvelope = regexp.MustCompile(`"command":"(pr wait|pr merge|release wait)"`)

// devctlCalls reads the devctl answers among the turn's tool results, in
// order: a result with a devctl document is one call.
func devctlCalls(tools []harnessToolResult) []devctlCall {
	var calls []devctlCall
	for _, tool := range tools {
		doc := unquoteJSON(tool.output)
		m := devctlEnvelope.FindStringIndex(doc)
		if m == nil {
			continue
		}
		calls = append(calls, devctlDocument(doc, m[0]))
	}
	return calls
}

// devctlDocument decodes the devctl document whose "command" key starts at
// key: the object that owns the key, found by walking back over the opening
// braces before it, since a nested object (a check, a release) may open
// between the document's start and its "command".
func devctlDocument(doc string, key int) devctlCall {
	want := devctlEnvelope.FindStringSubmatch(doc[key:])[1]
	for start := strings.LastIndex(doc[:key], "{"); start >= 0; start = strings.LastIndex(doc[:start], "{") {
		var env struct {
			Command  string `json:"command"`
			ExitCode int    `json:"exitCode"`
			Verdict  string `json:"verdict"`
			MergedBy any    `json:"mergedBy"`
		}
		if json.NewDecoder(strings.NewReader(doc[start:])).Decode(&env) == nil && env.Command == want {
			return devctlCall{command: env.Command, exitCode: env.ExitCode, verdict: env.Verdict, mergedBy: loginOf(env.MergedBy)}
		}
	}
	return devctlCall{command: want, exitCode: -1}
}

// unquoteJSON undoes the string quoting a JSON document gets when a tool
// result nests it in another (call_tool's text in the harness's result).
func unquoteJSON(s string) string {
	for strings.Contains(s, `\"`) {
		var inner string
		if json.Unmarshal([]byte(`"`+s+`"`), &inner) != nil {
			return strings.ReplaceAll(s, `\"`, `"`)
		}
		s = inner
	}
	return s
}

// loginOf is a merger's login: devctl names it as a string or as a user
// object.
func loginOf(v any) string {
	switch m := v.(type) {
	case string:
		return m
	case map[string]any:
		login, _ := m["login"].(string)
		return login
	}
	return ""
}

// judgeDevctlWaits is the verdict on the agent's turn: each tool called
// once, in order, each exit 0; pr_merge's merger and GitHub's merge record
// the person.
func judgeDevctlWaits(calls []devctlCall, merged, login string) error {
	var order []string
	byCommand := map[string][]devctlCall{}
	for _, c := range calls {
		order = append(order, c.command)
		byCommand[c.command] = append(byCommand[c.command], c)
	}
	if !slices.Equal(order, devctlCommandOrder) {
		return fmt.Errorf("the agent called %v, want each of %v once, in that order", order, devctlCommandOrder)
	}
	var problems []string
	for _, command := range devctlCommandOrder {
		if c := byCommand[command][0]; c.exitCode != 0 {
			problems = append(problems, fmt.Sprintf("%s answered exitCode %d, verdict %s", command, c.exitCode, c.verdict))
		}
	}
	if by := byCommand[devctlCommandPRMerge][0].mergedBy; by != "" && by != login {
		problems = append(problems, fmt.Sprintf("pr_merge names %s as the merger, want the person %s", by, login))
	}
	fields := strings.Fields(merged)
	switch {
	case len(fields) != 3:
		problems = append(problems, fmt.Sprintf("the merge record read back as %q, want the state, the merger and the merge commit", merged))
	case fields[0] != "MERGED":
		problems = append(problems, fmt.Sprintf("the pull request is %s, want MERGED", fields[0]))
	case fields[1] != login:
		problems = append(problems, fmt.Sprintf("GitHub names %s as the merger, want the person %s", fields[1], login))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// judgeDevctlProgress is the verdict on the proof's own pr_wait: one call
// answering green past the server's per-call timeout, with progress
// notifications whose progress rises.
func judgeDevctlProgress(pc devctlProgressCall, musterTimeout time.Duration) error {
	if pc.err != nil {
		return fmt.Errorf("the proof's own pr_wait through muster: %w", pc.err)
	}
	text := ""
	if len(pc.env.Content) > 0 {
		text = pc.env.Content[0].Text
	}
	var doc struct {
		ExitCode int    `json:"exitCode"`
		Verdict  string `json:"verdict"`
	}
	if pc.env.IsError || json.Unmarshal([]byte(text), &doc) != nil || doc.ExitCode != 0 {
		return fmt.Errorf("the proof's own pr_wait answered %s, want exitCode 0, verdict green", excerpt(text, 300))
	}
	if pc.elapsed <= musterTimeout {
		return fmt.Errorf("the proof's own pr_wait answered after %s, within muster's per-call timeout %s: the head's CI proves no wait kept alive by progress", pc.elapsed.Round(time.Second), musterTimeout)
	}
	if len(pc.notes) == 0 {
		return fmt.Errorf("the proof's own pr_wait answered after %s with no progress notification delivered", pc.elapsed.Round(time.Second))
	}
	for i := 1; i < len(pc.notes); i++ {
		if pc.notes[i].Progress <= pc.notes[i-1].Progress {
			return fmt.Errorf("progress notification %d carries progress %v after %v: it must rise", i+1, pc.notes[i].Progress, pc.notes[i-1].Progress)
		}
	}
	return nil
}

func lastProgressMessage(notes []progressNote) string {
	if len(notes) == 0 {
		return ""
	}
	return notes[len(notes)-1].Message
}

// mcpServerTimeout is a muster MCPServer's per-call timeout (spec.timeout,
// seconds).
func mcpServerTimeout(name string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return 0, err
	}
	obj, err := getObject(ctx, gvr, platformNamespace, name)
	if err != nil {
		return 0, err
	}
	seconds, found, err := unstructured.NestedInt64(obj.Object, "spec", "timeout")
	if err != nil || !found || seconds <= 0 {
		return 0, fmt.Errorf("the MCPServer %s/%s carries no spec.timeout (%v)", platformNamespace, name, err)
	}
	return time.Duration(seconds) * time.Second, nil
}

// cleanup removes what the part created on every path.
func (dp *devctlProof) cleanup() {
	var errs []error
	if dp.set != nil {
		errs = append(errs, dp.set.removeAllNow())
	}
	if agentExists(devctlProofAgent) {
		errs = append(errs, removeAgent(devctlProofAgent))
	}
	errs = append(errs, dp.sp.restore())
	if dp.pr != nil && dp.pr.dir != "" {
		if left, err := deleteBranchesWithPrefix(dp.pr, devctlBranchPrefix); err != nil {
			errs = append(errs, err)
		} else if len(left) > 0 {
			note("deleted the run's branches left on %s: %s", dp.repository, strings.Join(left, ", "))
		}
		errs = append(errs, dp.pr.restore())
	}
	if dp.api != nil {
		dp.api.close()
	}
	if err := errors.Join(errs...); err != nil {
		warn("removing what the part left: %v", err)
	}
}
