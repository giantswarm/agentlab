package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// `agentlab coding-test --part devctl --harness codex|omp`: the devctl chain
// of the claude run (codingdevctl.go) on another coding Harness, which
// reaches muster through Substrate's egress gateway with the Session's actor
// JWT. The agent's wait is the proof: one pr_wait through muster as the
// person, on a pull request whose CI outlasts muster's per-call timeout.
//
//  1. Preconditions: devctl's server reachable through muster, the admin
//     holding the github instance's grant, the Harness rendered and Ready,
//     the proof repository cloned on this host, an interrupted run's
//     branches deleted.
//  2. The tools: the admin's muster lists x_devctl_pr_wait.
//  3. The pull request: this proof pushes a branch and opens a pull request
//     on the proof repository from this host as the person; its CI starts.
//  4. The agent: created through agent-manager as the admin on the Harness,
//     its toolset x_devctl_pr_wait (codex on the in-cluster mockllm unless
//     --model-config names a model; omp on agent-manager's default).
//  5. The sandbox: a Session on a workspace over the proof repository, with
//     the admin's grant that binds its actor to the person; its first turn
//     probes its sandbox: no forge token
//     or bearer in its environment, /data or /workspace, the model key and
//     MCP credentials kagent's placeholders.
//  6. The wait: the next turn calls x_devctl_pr_wait once; its document
//     answers exit 0, green, and devctl's server logs the call by the
//     person's GitHub login, the pull request's author.
//
// The Session, the agent, the workspace, the mockllm, the pull request, its
// branch and the host clone are removed on every path.

// devctlHarnessProof is the part's state on a Harness other than claude.
type devctlHarnessProof struct {
	*devctlProof
	rt          codingRuntime
	modelConfig string
	mockBinary  string
	mock        *mockLLM
	// opened is when this proof opened the pull request; the run's
	// leftovers are closed by their branch.
	opened time.Time
}

// devctlHarnessPrompt is the prompt of the wait's turn; the mockllm's
// devctl entry matches its first words.
const devctlHarnessPrompt = "Wait for the CI of pull request"

// devctlPRWaitTool is the devctl server's own name of the tool, which its
// call log names (muster adds devctlToolPrefix).
const devctlPRWaitTool = "pr_wait"

func codingDevctlHarnessPart(cfg *config.Config, opts CodingTestOptions, rt codingRuntime) error {
	if !cfg.CodingHarnessRendered(rt.name) {
		return fmt.Errorf("the lab renders no Harness %s: add it to platform.coding.harnesses in %s (`agentlab configure --coding-harnesses %s`), then `agentlab platform`", rt.name, config.File, strings.Join(append(cfg.CodingHarnesses(), rt.name), ","))
	}
	hp := &devctlHarnessProof{
		devctlProof: newDevctlProof(cfg, opts),
		rt:          rt, modelConfig: opts.ModelConfig,
	}
	if opts.ModelConfig == "" && rt.name == config.CodingRuntimeCodex {
		binary, err := mockLLMBinary(opts.MockLLMBinary)
		if err != nil {
			return err
		}
		hp.mockBinary = binary
	}
	p := newProofStages()
	err := hp.run(p)
	hp.cleanup()
	if err == nil {
		fmt.Printf("\ncoding-test --part devctl --harness %s passed on GitHub: an agent on the %s Harness waited for the CI of pull request #%d on %s with one x_devctl_pr_wait call through muster, devctl acting as %s, with no muster or GitHub token in its sandbox.\n",
			rt.name, rt.name, hp.number, hp.repository, hp.login)
	}
	return p.close(err)
}

func (hp *devctlHarnessProof) run(p *proofStages) error {
	p.begin("devctl: preconditions", "devctl's server reachable through muster, the admin holding the %s instance's grant, the Harness %s Ready, %s cloned and an interrupted run's branches deleted", devctlGrantApp, hp.rt.name, hp.repository)
	if err := hp.preconditions(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), hp.timeout)
	defer cancel()
	if err := waitHarnessReady(ctx, hp.rt.name, hp.timeout); err != nil {
		return err
	}
	hp.harness = hp.rt.name

	p.begin("devctl: tools", "The admin's muster lists %s", devctlProofTools[devctlCommandPRWait])
	if err := hp.tools(); err != nil {
		return err
	}

	p.begin("devctl: pull request", "This proof pushes %s and opens a pull request on %s from this host as the person", hp.branch, hp.repository)
	if err := hp.openPullRequestHere(); err != nil {
		return err
	}

	p.begin("devctl: agent", "A %s agent through agent-manager as the admin, its toolset %s, Ready", hp.rt.name, devctlProofTools[devctlCommandPRWait])
	if err := hp.agent(); err != nil {
		return err
	}

	p.begin("devctl: sandbox", "A Session on workspace %s with the admin's grant; its sandbox holds no muster or GitHub token: no forge token or bearer in its environment, /data or /workspace, the model key and MCP credentials placeholders", devctlProofWorkspace)
	session, err := hp.session()
	if err != nil {
		return err
	}
	if err := hp.sandbox(session); err != nil {
		return err
	}

	p.begin("devctl: wait", "The %s agent calls %s on #%d once: green in one call past muster's per-call timeout, devctl acting as the person", hp.rt.name, devctlProofTools[devctlCommandPRWait], hp.number)
	if err := hp.wait(session); err != nil {
		return err
	}
	if hp.mock == nil {
		return nil
	}
	p.begin("devctl: model", "The %s turns ran on the in-cluster mockllm: every model call answered by its script", hp.rt.name)
	return hp.mockAnswered()
}

// openPullRequestHere pushes a branch from origin/HEAD with one file and
// opens a pull request with the person's own gh on this host, and reads its
// number and author.
func (hp *devctlHarnessProof) openPullRequestHere() error {
	dir := hp.pr.dir
	base, err := hp.pr.git(dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return err
	}
	base = strings.TrimPrefix(base, gitOrigin+"/")
	if _, err := hp.pr.git(dir, "switch", "-q", "-c", hp.branch, gitOrigin+"/HEAD"); err != nil {
		return err
	}
	if err := os.WriteFile(dir+"/"+devctlProofFile, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return err
	}
	if _, err := hp.pr.git(dir, "add", devctlProofFile); err != nil {
		return err
	}
	if _, err := hp.pr.git(dir, "commit", "-qm", "chore: agentlab coding-test --part devctl --harness "+hp.rt.name); err != nil {
		return err
	}
	if _, err := hp.pr.git(dir, gitPush, "-q", gitOrigin, "HEAD:refs/heads/"+hp.branch); err != nil {
		return err
	}
	hp.opened = time.Now()
	if _, err := personGH("pr", "create", "--repo", hp.repository, "--head", hp.branch, "--base", base,
		"--title", "chore: agentlab coding-test --part devctl --harness "+hp.rt.name,
		"--body", "Opened by agentlab coding-test --part devctl as the person; a "+hp.rt.name+" agent waits for its CI through devctl's MCP server, the same run closes it."); err != nil {
		return err
	}
	out, err := personGH("pr", "view", hp.branch, "--repo", hp.repository, "--json", "number,author", "--jq", `(.number|tostring)+" "+.author.login`)
	if err != nil {
		return err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return fmt.Errorf("gh pr view answered %q, want the number and the author", out)
	}
	if hp.number, err = strconv.Atoi(fields[0]); err != nil || hp.number <= 0 {
		return fmt.Errorf("gh pr view answered %q, want the number and the author", out)
	}
	hp.login = fields[1]
	note("pushed %s and opened #%d on %s as %s; its CI runs", hp.branch, hp.number, hp.repository, hp.login)
	return nil
}

// personGH runs gh on this host with the person's own login, never
// prompting.
func personGH(args ...string) (string, error) {
	cmd := exec.Command("gh", args...) // #nosec G204 -- gh with the proof's own arguments
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, excerpt(strings.TrimSpace(string(out)), 300))
	}
	return strings.TrimSpace(string(out)), nil
}

// agent creates the agent on the Harness with x_devctl_pr_wait as its
// toolset and waits for it; on the mockllm it starts the mockllm for the
// agent's binding, scripted with the wait's call.
func (hp *devctlHarnessProof) agent() error {
	modelConfig := hp.modelConfig
	if hp.mockBinary != "" {
		if err := ensureMockLLMModelConfig(); err != nil {
			return err
		}
		modelConfig = MockLLMModelConfig
	}
	if modelConfig == "" {
		var err error
		if modelConfig, err = agentManagerModelConfig(hp.admin.session); err != nil {
			return err
		}
	}
	toolset := []string{"tool:" + devctlProofTools[devctlCommandPRWait]}
	spec := agentSpec{Name: devctlProofAgent, ModelConfig: modelConfig, Harness: hp.rt.name, Toolset: toolset, SystemMessage: devctlSystemPrompt,
		Description: "Throwaway agent of agentlab coding-test --part devctl; deleted by the same run."}
	written, err := (agentManagerWriter{hp.admin.session}).createAgent(spec)
	if err != nil {
		return err
	}
	boot, err := waitAgentReadyOn(spec.Name, spec.Harness, hp.timeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return fmt.Errorf("agent %s is not Ready on Harness %s after %s: %s", spec.Name, spec.Harness, boot.elapsed.Round(time.Second), boot.reason)
	}
	note("%s Ready on %s in %s (requestedBy %s), toolset %s, model %s", spec.Name, spec.Harness, boot.elapsed.Round(time.Second), written.RequestedBy, strings.Join(toolset, ","), modelConfig)
	if hp.mockBinary == "" {
		return nil
	}
	t, err := readAgentTemplate(devctlProofAgent)
	if err != nil {
		return err
	}
	arguments, err := hp.waitArguments()
	if err != nil {
		return err
	}
	if hp.mock, err = startMockLLM(hp.cfg, hp.mockBinary, t.mcpServer(), arguments, nil); err != nil {
		return err
	}
	note("the mockllm serves %s for the MCP tools of %s, scripted with the wait's call", mockLLMURL(), t.mcpServer())
	return nil
}

// waitArguments are call_tool's arguments of the wait: x_devctl_pr_wait on
// the pull request.
func (hp *devctlHarnessProof) waitArguments() (string, error) {
	b, err := json.Marshal(map[string]any{nameKey: devctlProofTools[devctlCommandPRWait], argumentsKey: map[string]any{
		devctlRepositoryArg: hp.repository, devctlNumberArg: hp.number, devctlTimeoutArg: devctlWaitTimeout,
	}})
	return string(b), err
}

// session defines the workspace over the proof repository and starts a
// Session of the agent on it with the admin's grant: the grant binds the
// Session's actor to the person, whom the egress gateway names to muster.
func (hp *devctlHarnessProof) session() (*apiv1alpha1.Session, error) {
	if err := hp.workspace(); err != nil {
		return nil, err
	}
	session, err := hp.startSession()
	if err != nil {
		return nil, err
	}
	note("Session %s on workspace %s with the admin's grant", session.GetId(), devctlProofWorkspace)
	return session, nil
}

// sandbox runs the coding proof's sandbox check in the Session and judges
// its credentials.
func (hp *devctlHarnessProof) sandbox(session *apiv1alpha1.Session) error {
	ctx, cancel := context.WithTimeout(context.Background(), hp.timeout)
	defer cancel()
	t, err := hp.api.turn(ctx, session, userMessage(codingSandboxPrompt(hp.rt)))
	if err != nil {
		return fmt.Errorf("the sandbox turn on Session %s: %w", session.GetId(), err)
	}
	reply, err := t.completedText()
	if err != nil {
		return fmt.Errorf("the sandbox turn on Session %s: %w", session.GetId(), err)
	}
	verdict, err := parseTurnProbe(reply, t.toolOutputs()).credentials(nil)
	if err != nil {
		return err
	}
	note("%s", verdict)
	return nil
}

// wait runs the wait's turn and judges the agent's one call and devctl's log
// of it.
func (hp *devctlHarnessProof) wait(session *apiv1alpha1.Session) error {
	note("turn on Session %s, deadline %s", session.GetId(), devctlTurnTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), devctlTurnTimeout)
	defer cancel()
	start := time.Now()
	t, err := hp.api.turn(ctx, session, userMessage(devctlHarnessWaitPrompt(hp.repository, hp.number)))
	if err != nil {
		return fmt.Errorf("the wait's turn on Session %s after %s: %w", session.GetId(), time.Since(start).Round(time.Second), err)
	}
	reply, err := t.completedText()
	if err != nil {
		return fmt.Errorf("the wait's turn on Session %s: %w", session.GetId(), err)
	}
	elapsed := time.Since(start)
	note("the wait's turn ended after %s", elapsed.Round(time.Second))
	calls := devctlCalls(t.tools)
	if len(t.tools) == 0 && hp.mock != nil {
		// codex reports no MCP call's result to its caller; the mockllm's
		// reply is the call's output item as codex returned it.
		calls = devctlCalls([]harnessToolResult{{name: musterCallTool, output: reply}})
	}
	for _, c := range calls {
		note("the agent called %s: exitCode %d, verdict %s", c.command, c.exitCode, c.verdict)
	}
	if err := judgeDevctlHarnessWait(calls, elapsed, hp.musterTimeout); err != nil {
		err = fmt.Errorf("the agent's turn: %w\nits reply: %s\nits tools: %s", err, excerpt(reply, 600), toolResultsExcerpt(t.tools))
		if hp.mock != nil {
			err = fmt.Errorf("%w\nthe mockllm's requests:\n%s", err, hp.mock.logTail())
		}
		return err
	}
	logCtx, logCancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer logCancel()
	logs, err := podLogs(logCtx, platformNamespace, "deploy/"+devctlRelease, "", time.Since(start)+30*time.Second)
	if err != nil {
		return fmt.Errorf("devctl's server log: %w", err)
	}
	logged, err := judgeDevctlCallLog(logs, hp.repository, hp.login)
	if err != nil {
		return err
	}
	note("devctl's server logged the call: %s", logged)
	return nil
}

// toolResultsExcerpt names a turn's tool results and the start of each
// output, for a verdict that found no devctl document among them.
func toolResultsExcerpt(tools []harnessToolResult) string {
	if len(tools) == 0 {
		return "no tool results"
	}
	parts := make([]string, 0, len(tools))
	for _, tool := range tools {
		parts = append(parts, fmt.Sprintf("%s: %s", tool.name, excerpt(tool.output, 300)))
	}
	return strings.Join(parts, "; ")
}

// devctlHarnessWaitPrompt is the wait's turn: one call of x_devctl_pr_wait
// through muster's call_tool, never polled.
func devctlHarnessWaitPrompt(repo string, number int) string {
	return fmt.Sprintf(`%[1]s #%[2]d of %[3]s through the devctl tools on your muster server: call muster's call_tool with name %[4]s and arguments {"repository": %[3]q, "number": %[2]d, "timeout": %[5]q}. The tool blocks until its verdict and reports progress meanwhile: call it exactly once, never poll, never call it again, never sleep, never use a shell.

Reply with one line, "%[4]s exitCode=<n> verdict=<verdict>", from its JSON answer.`,
		devctlHarnessPrompt, number, repo, devctlProofTools[devctlCommandPRWait], devctlWaitTimeout)
}

// judgeDevctlHarnessWait is the verdict on the wait's turn: one pr wait,
// exit 0, after longer than muster's per-call timeout.
func judgeDevctlHarnessWait(calls []devctlCall, elapsed, musterTimeout time.Duration) error {
	if len(calls) != 1 || calls[0].command != devctlCommandPRWait {
		var got []string
		for _, c := range calls {
			got = append(got, c.command)
		}
		return fmt.Errorf("the agent called %v, want %s once", got, devctlCommandPRWait)
	}
	if c := calls[0]; c.exitCode != 0 {
		return fmt.Errorf("%s answered exitCode %d, verdict %s", c.command, c.exitCode, c.verdict)
	}
	if elapsed <= musterTimeout {
		return fmt.Errorf("the wait's turn ended after %s, within muster's per-call timeout %s: the head's CI proves no wait kept alive by progress", elapsed.Round(time.Second), musterTimeout)
	}
	return nil
}

// devctlCallLogMessage is the message of devctl's server's line per tool
// call.
const devctlCallLogMessage = "tool call"

// judgeDevctlCallLog finds devctl's server's log line of the pr_wait call on
// repository and wants it made as login with a green verdict; it returns the
// line's readings.
func judgeDevctlCallLog(logs, repository, login string) (string, error) {
	var seen []string
	for line := range strings.Lines(logs) {
		start := strings.Index(line, "{")
		if start < 0 {
			continue
		}
		var entry struct {
			Msg        string `json:"msg"`
			Tool       string `json:"tool"`
			Login      string `json:"login"`
			Repository string `json:"repository"`
			Verdict    string `json:"verdict"`
			ExitCode   int    `json:"exitCode"`
			Duration   int64  `json:"duration"`
		}
		if json.Unmarshal([]byte(line[start:]), &entry) != nil || entry.Msg != devctlCallLogMessage || entry.Tool != devctlPRWaitTool || entry.Repository != repository {
			continue
		}
		got := fmt.Sprintf("%s on %s as %s: exitCode %d, verdict %s, %s", entry.Tool, entry.Repository, entry.Login, entry.ExitCode, entry.Verdict, time.Duration(entry.Duration).Round(time.Second))
		if entry.Login == login && entry.ExitCode == 0 {
			return got, nil
		}
		seen = append(seen, got)
	}
	if len(seen) > 0 {
		return "", fmt.Errorf("devctl's server logged %s, want one as the person %s with exitCode 0", strings.Join(seen, "; "), login)
	}
	return "", fmt.Errorf("devctl's server logged no %q line of %s on %s since the turn started", devctlCallLogMessage, devctlPRWaitTool, repository)
}

// mockAnswered is the mockllm's count of the model calls: some answered,
// none outside its script.
func (hp *devctlHarnessProof) mockAnswered() error {
	counts, err := hp.mock.requests()
	if err != nil {
		return fmt.Errorf("reading the mockllm's requests: %w", err)
	}
	if counts.Answered == 0 || counts.Unmatched > 0 {
		return fmt.Errorf("the mockllm answered %d model calls of the %s agent and matched no script entry for %d (`docker logs %s-%s`)", counts.Answered, hp.rt.name, counts.Unmatched, hp.cfg.ClusterName, mockLLMContainerSuffix)
	}
	note("the mockllm answered all %d model calls of the %s turns", counts.Answered, hp.rt.name)
	return nil
}

// cleanup closes the pull request, then removes what the claude part's
// cleanup removes, and the mockllm.
func (hp *devctlHarnessProof) cleanup() {
	var errs []error
	if hp.number > 0 {
		if _, err := personGH("pr", "close", strconv.Itoa(hp.number), "--repo", hp.repository, "--delete-branch"); err != nil {
			errs = append(errs, err)
		}
	}
	if hp.mock != nil {
		hp.mock.close()
		removeMockLLMModelConfig()
	}
	if err := errors.Join(errs...); err != nil {
		warn("closing the run's pull request: %v", err)
	}
	hp.devctlProof.cleanup()
}
