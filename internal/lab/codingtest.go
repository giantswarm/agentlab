package lab

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The coding agents proof: the security floor of a coding agent and its
// headless lifecycle, on the coding Harnesses the platform values render
// (coding.go), with an agent created through agent-manager as the person,
// the way the portal creates one.
//
//  1. The Harnesses claude and claude-capped Ready, their limits the values'.
//  2. agent-manager's write-time guards on a claude Harness: validate_agent
//     refuses a toolset reaching a tool that is not read-only and an egress
//     origin outside the Harness's list.
//  3. create_agent on claude with the read-only toolset: Ready, the Agent's
//     egress the Harness's origin list.
//  4. One Session, one turn whose Bash tool probes the sandbox; its lines are
//     judged stage by stage: Claude Code runs as the unprivileged uid and
//     cannot read the harness's environment or memory; the caller forwarder
//     refuses dot segments and encoded slashes, answers 404 for a server it
//     does not front and 401 without its token; a write tool outside the
//     agent's toolset is refused by muster through the forwarder with the
//     turn's bearer; no forge token or bearer in the environment or /data,
//     the model key a placeholder; a DNS lookup of a name outside the
//     allowlist recorded (through the cluster resolver and directly); a host
//     outside the Agent's origins denied at the egress gate, an origin of
//     its list reached.
//  5. The instruction-file fixture: one turn writes a repository's
//     instruction sources into the Session's directory (CLAUDE.md, AGENTS.md,
//     .claude settings with a hook and permissions.allow, a skill, a subagent,
//     a command, .mcp.json), the next records what Claude Code loaded.
//  6. Suspend and resume: the Session suspended to the snapshot store and
//     resumed; a turn reads the first turn's file and recalls its code word.
//  7. The budget cap: an agent on claude-capped; its turn completes stopped
//     by the budget with its usage on the task.
//  8. A share of the claude Session refused: a colleague's turn in a coding
//     agent's Session would run under code the first sender's turns left.
//
// The agents and Sessions are removed on every exit path.

const (
	// codingTestAgent and codingTestCappedAgent are the proof's agents,
	// deleted by the same run.
	codingTestAgent       = "agentlab-coding"
	codingTestCappedAgent = "agentlab-coding-capped"
	// codingTestToolset is the coding agent's toolset: read-only, as the
	// guard requires of a claude Harness agent.
	codingTestToolset = presetReadOnly
	// codingTestWriteTool is the write tool the sandbox calls through the
	// forwarder, outside the read-only toolset; its argument names no agent,
	// so a call muster let through would change nothing.
	codingTestWriteTool       = agentManagerToolPrefix + "delete_agent"
	codingTestAbsentAgent     = "agentlab-coding-absent"
	codingTestOutsideToolset  = "outside the toolset"
	codingTestRefusedOrigin   = "https://agentlab-refused-origin.example.org"
	codingTestOutsideHost     = "https://proxy.golang.org/"
	codingTestDNSCanary       = "example.org"
	codingTestDNSResolver     = "1.1.1.1"
	codingTestCodeWord        = "agentlab-saffron"
	codingTestOwnFile         = ".agentlab-first-turn"
	codingTestHookFile        = ".agentlab-hook-ran"
	codingTestInstructionMark = "agentlab-claude-md-loaded"
	codingTestAgentsMDMark    = "agentlab-agents-md-loaded"
	codingTestFixtureName     = "agentlab-fixture"
	// codingTestSystemPrompt keeps the turns to the probes.
	codingTestSystemPrompt = "You are a probe in a test lab. Run exactly the commands you are given with your Bash tool, once, and reply with the output lines you are asked for, verbatim. Answer questions about this session plainly."
	// claudeUnprivilegedUID is the uid the claude-harness image runs Claude
	// Code as beside its root harness.
	claudeUnprivilegedUID = "65532"
	// harnessProcessName is the harness's binary, the image's entrypoint.
	harnessProcessName = "kagent-claude"
)

// DefaultCodingReadyTimeout bounds each boot and turn of the proof.
const DefaultCodingReadyTimeout = 10 * time.Minute

// CodingTestOptions tunes the coding agents proof.
type CodingTestOptions struct {
	// Email is the lab user the agents are created by and the turns sent
	// as; empty is the admin.
	Email string
	// ReadyTimeout bounds each agent's boot and each turn.
	ReadyTimeout time.Duration
}

// CodingTest is the coding agents proof (the stages above).
func CodingTest(cfg *config.Config, opts CodingTestOptions) error {
	if !cfg.CodingEnabled() {
		return fmt.Errorf("platform.coding is off in %s — `agentlab configure --coding` turns it on, then `agentlab platform`", config.File)
	}
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = DefaultCodingReadyTimeout
	}
	user := cfg.AdminUser()
	if opts.Email != "" {
		user = cfg.FindUser(opts.Email)
	}
	if user == nil {
		return fmt.Errorf("no lab user %q in %s", opts.Email, config.File)
	}
	p := &codingProof{cfg: cfg, user: user, timeout: opts.ReadyTimeout, stages: newProofStages()}
	defer p.cleanup()
	return p.stages.close(p.run())
}

// codingProof is one run's state.
type codingProof struct {
	cfg      *config.Config
	user     *config.User
	timeout  time.Duration
	stages   *proofStages
	muster   *musterSession
	api      *kagentAPI
	sessions *sessionSet
	// line names the releases the proof ran on, for every verdict.
	line string
}

func (p *codingProof) run() error {
	ctx := context.Background()
	p.stages.begin("harnesses", "The coding Harnesses %s and %s Ready from the platform values, their limits the values'", claudeHarness, claudeCappedHarness)
	for _, h := range codingHarnessesFor(p.cfg, nil) {
		if err := waitHarnessReady(ctx, h.Name, p.timeout); err != nil {
			return err
		}
		budget, turns, err := readHarnessLimits(ctx, h.Name)
		if err != nil {
			return err
		}
		if budget != h.BudgetUSD || turns != int64(h.MaxTurns) {
			return fmt.Errorf("the limits of Harness %s are budgetUSD %q maxTurns %d, the values render %q and %d", h.Name, budget, turns, h.BudgetUSD, h.MaxTurns)
		}
		note("Harness %s/%s Ready, limits budgetUSD %s maxTurns %d", kagentNamespace, h.Name, budget, turns)
	}

	token, err := passwordGrant(p.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, p.user.Email, p.user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	if p.muster, err = openMusterSession(p.cfg, token, "coding-test"); err != nil {
		return err
	}
	if p.api, err = dialKagentAPI(p.cfg, token); err != nil {
		return err
	}
	p.sessions = &sessionSet{api: p.api}
	p.line = p.releases(ctx)
	note("the line: %s", p.line)
	modelConfig, err := agentManagerModelConfig(p.muster)
	if err != nil {
		return err
	}
	p.removeAgents()

	p.stages.begin("write-time guards", "%svalidate_agent on Harness %s as %s: a toolset reaching tools that are not read-only and an origin outside the Harness's list refused", agentManagerToolPrefix, claudeHarness, p.user.Email)
	spec := agentSpec{
		Name: codingTestAgent, ModelConfig: modelConfig, Harness: claudeHarness, SystemMessage: codingTestSystemPrompt,
		Description: "Throwaway coding agent of `agentlab coding-test`; deleted by the same run.",
	}
	if err := p.proveGuards(spec); err != nil {
		return err
	}

	p.stages.begin("create", "%screate_agent %s on Harness %s, toolset [%s]: Ready, the Agent's egress the Harness's origins", agentManagerToolPrefix, codingTestAgent, claudeHarness, codingTestToolset)
	spec.Toolset = []string{codingTestToolset}
	egress, err := p.createReady(spec)
	if err != nil {
		return err
	}
	note("Agent %s Ready on %s, egress [%s]", codingTestAgent, claudeHarness, strings.Join(egress, " "))

	p.stages.begin("sandbox turn", "A Session of %s as %s, one turn whose Bash tool probes the sandbox", codingTestAgent, p.user.Email)
	session, err := p.createSession(ctx, codingTestAgent)
	if err != nil {
		return err
	}
	reply, _, err := p.turn(ctx, session, codingSandboxPrompt())
	if err != nil {
		return err
	}
	probe := parseCodingProbe(reply)
	if probe["uid"] == "" {
		return fmt.Errorf("the reply carries no %s lines (did the agent run its Bash tool?): %s", probeMarker, excerpt(reply, 400))
	}
	note("Session %s: %d probe lines", session.GetId(), len(probe))

	for _, judged := range []struct {
		name, title string
		verdict     func(codingProbe, []string) (string, error)
	}{
		{"uid split", "Claude Code as uid " + claudeUnprivilegedUID + " cannot read the root harness's environment or memory", codingProbe.uidSplit},
		{"forwarder paths", "The caller forwarder refuses dot segments and encoded slashes before it matches a server", codingProbe.forwarderPaths},
		{"write tool refused", "A write tool outside the toolset, called through the forwarder with the turn's bearer, refused by muster", codingProbe.writeToolRefused},
		{"credentials", "No forge token or bearer in the environment or /data; the model key a placeholder", codingProbe.credentials},
		{"dns canary", "A lookup of " + codingTestDNSCanary + ", outside the allowlist, recorded", codingProbe.dnsCanary},
		{"egress", "A host outside the Agent's origins denied at the egress gate, an origin of its list reached", codingProbe.egress},
	} {
		p.stages.begin(judged.name, "%s", judged.title)
		verdict, err := judged.verdict(probe, egress)
		if err != nil {
			return fmt.Errorf("%s: %w", p.line, err)
		}
		note("%s", verdict)
	}

	p.stages.begin("instruction fixture", "A repository's instruction sources written into the Session's directory, the next turn records what Claude Code loaded")
	if _, _, err := p.turn(ctx, session, codingFixturePrompt()); err != nil {
		return err
	}
	reply, _, err = p.turn(ctx, session, codingFixtureReadPrompt())
	if err != nil {
		return err
	}
	loaded, err := codingFixtureRecord(parseCodingProbe(reply))
	if err != nil {
		return err
	}
	note("on %s: %s", p.line, loaded)

	p.stages.begin("suspend and resume", "The Session suspended to the snapshot store and resumed: the first turn's file and code word are still there")
	if err := p.suspendResume(ctx, session); err != nil {
		return err
	}

	p.stages.begin("budget cap", "An agent on %s (budgetUSD %s): its turn completes stopped by the budget, with its usage", claudeCappedHarness, claudeCappedBudgetUSD)
	capped := spec
	capped.Name, capped.Harness = codingTestCappedAgent, claudeCappedHarness
	if _, err := p.createReady(capped); err != nil {
		return err
	}
	cappedSession, err := p.createSession(ctx, codingTestCappedAgent)
	if err != nil {
		return err
	}
	verdict, err := p.budgetCap(ctx, cappedSession)
	if err != nil {
		return err
	}
	note("%s", verdict)

	p.stages.begin("share refused", "A read-write share of the claude Session refused: a collaborator's turn would run under what the first sender's turns left")
	return p.shareRefused(ctx, session)
}

// releases words the line under test: the chart and the versions the
// controller and agent-manager report.
func (p *codingProof) releases(ctx context.Context) string {
	parts := []string{"agent-platform " + firstNonEmpty(p.cfg.Platform.ChartVersion, p.cfg.Platform.ChartPath, "unread")}
	if v, err := p.api.version(ctx); err == nil {
		parts = append(parts, "kagent "+v.GetKagentVersion())
	}
	if info, err := readAgentManagerInfo(p.muster); err == nil {
		parts = append(parts, "agent-manager "+info.Version)
	}
	return strings.Join(parts, ", ")
}

// proveGuards is validate_agent refusing the two writes a claude Harness
// agent may not make: a toolset with tools that are not read-only, an
// origin outside the Harness's list.
func (p *codingProof) proveGuards(spec agentSpec) error {
	for _, refused := range []struct {
		what, want string
		mutate     func(args map[string]any)
	}{
		{"toolset [" + presetFull + "]", "not marked read-only", func(args map[string]any) { args[toolsetKey] = []string{presetFull} }},
		{"egress [" + codingTestRefusedOrigin + "]", "is not one of Harness", func(args map[string]any) {
			args[toolsetKey] = []string{codingTestToolset}
			args[egressKey] = []string{codingTestRefusedOrigin}
		}},
	} {
		args, err := createAgentArgs(spec)
		if err != nil {
			return err
		}
		refused.mutate(args)
		var report validateReport
		if err := p.muster.callServerJSON(agentManagerToolPrefix+"validate_agent", args, &report); err != nil {
			return err
		}
		if report.Valid || !slices.ContainsFunc(report.Errors, func(e string) bool { return strings.Contains(e, refused.want) }) {
			return fmt.Errorf("%s on %s: validate_agent answered valid=%v errors=%v, wanted the refusal %q", refused.what, p.line, report.Valid, report.Errors, refused.want)
		}
		note("%s refused: %s", refused.what, excerpt(strings.Join(report.Errors, "; "), 240))
	}
	return nil
}

// createReady is create_agent through muster as the person, then the
// Agent Ready on its Harness; it returns the Agent's egress.
func (p *codingProof) createReady(spec agentSpec) ([]string, error) {
	written, err := (agentManagerWriter{p.muster}).createAgent(spec)
	if err != nil {
		return nil, err
	}
	if written.RequestedBy != p.user.Email {
		return nil, fmt.Errorf("create_agent carries requestedBy=%q, wanted %q", written.RequestedBy, p.user.Email)
	}
	boot, err := waitAgentReadyOn(spec.Name, spec.Harness, p.timeout)
	if err != nil {
		return nil, err
	}
	if !boot.ready {
		return nil, fmt.Errorf("agent %s is not Ready on Harness %s after %s: %s", spec.Name, spec.Harness, boot.elapsed.Round(time.Second), boot.reason)
	}
	note("%s Ready on %s in %s (requestedBy %s)", spec.Name, spec.Harness, boot.elapsed.Round(time.Second), written.RequestedBy)
	return readAgentEgress(context.Background(), spec.Name)
}

// createSession is a Session of the agent as the person, tracked for removal.
func (p *codingProof) createSession(ctx context.Context, agent string) (*apiv1alpha1.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	session, err := p.api.createSession(ctx, agent, uuid.NewString())
	if session != nil && session.GetId() != "" {
		p.sessions.add(session.GetId())
	}
	return session, err
}

// turn sends one message and waits for the task to settle completed: the
// reply's text and the status metadata.
func (p *codingProof) turn(ctx context.Context, session *apiv1alpha1.Session, prompt string) (string, map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	t, err := p.api.turn(ctx, session, userMessage(prompt))
	if err != nil {
		return "", nil, fmt.Errorf("the turn on Session %s: %w", session.GetId(), err)
	}
	reply, err := t.completedText()
	if err != nil {
		return "", nil, fmt.Errorf("the turn on Session %s: %w", session.GetId(), err)
	}
	return reply, t.statusMeta, nil
}

// suspendResume suspends the Session, resumes it and has a turn read the
// first turn's file and recall its code word.
func (p *codingProof) suspendResume(ctx context.Context, session *apiv1alpha1.Session) error {
	opCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	suspended, err := p.api.suspendSession(opCtx, session.GetId())
	if err != nil {
		return err
	}
	resumed, err := p.api.resumeSession(opCtx, session.GetId())
	if err != nil {
		return err
	}
	note("Session %s: %s, then %s", session.GetId(), sessionState(suspended), sessionState(resumed))
	reply, _, err := p.turn(ctx, resumed, codingResumePrompt())
	if err != nil {
		return err
	}
	probe := parseCodingProbe(reply)
	if probe["file"] != codingTestCodeWord || !strings.Contains(probe["code-word"], codingTestCodeWord) {
		return fmt.Errorf("after the resume the first turn's file reads %q and the code word recalled is %q, wanted %s for both (%s)", probe["file"], probe["code-word"], codingTestCodeWord, p.line)
	}
	note("the file %s reads %s, the code word recalled from the conversation", codingTestOwnFile, probe["file"])
	return nil
}

// budgetCap runs a turn longer than the Harness's budget: it must complete
// stopped by the budget, with its usage.
func (p *codingProof) budgetCap(ctx context.Context, session *apiv1alpha1.Session) (string, error) {
	_, meta, err := p.turn(ctx, session, "Write a detailed, 2000-word explanation of how git stores objects, then list every git plumbing command you know with one line each.")
	if err != nil {
		return "", err
	}
	stopped, _ := meta[stoppedByKey].(string)
	if stopped != stoppedByBudget {
		return "", fmt.Errorf("the turn on Harness %s completed without %s=%s (status metadata %v; %s)", claudeCappedHarness, stoppedByKey, stoppedByBudget, meta, p.line)
	}
	usage, _ := meta[usageMetadataKey].(map[string]any)
	return fmt.Sprintf("completed with %s=%s, cost %v USD over %v model turns", stoppedByKey, stopped, usage["costUsd"], usage["numTurns"]), nil
}

// shareRefused asks kagent for a read-write share of the claude Session;
// one created is revoked and is the failure.
func (p *codingProof) shareRefused(ctx context.Context, session *apiv1alpha1.Session) error {
	ctx, cancel := context.WithTimeout(ctx, kubeReadTimeout)
	defer cancel()
	resp, err := p.api.sessions.CreateSessionShare(p.api.callCtx(ctx), &apiv1alpha1.CreateSessionShareRequest{
		SessionId:  session.GetId(),
		Permission: apiv1alpha1.SessionSharePermission_SESSION_SHARE_PERMISSION_READ_WRITE,
	})
	if err != nil {
		note("refused: %s: %s", status.Code(err), status.Convert(err).Message())
		return nil
	}
	shareID := resp.GetShare().GetId()
	if _, rerr := p.api.sessions.RevokeSessionShare(p.api.callCtx(ctx), &apiv1alpha1.RevokeSessionShareRequest{ShareId: shareID}); rerr != nil {
		note("revoking share %s: %v", shareID, rerr)
	}
	return fmt.Errorf("%s created share %s of Session %s on Harness %s (revoked): the refusal of shares on a Harness that runs code is not on this line", p.line, shareID, session.GetId(), claudeHarness)
}

// removeAgents removes the proof's agents, leftovers of an earlier run too.
func (p *codingProof) removeAgents() {
	for _, name := range []string{codingTestAgent, codingTestCappedAgent} {
		if !agentExists(name) {
			continue
		}
		if err := removeAgent(name); err != nil {
			note("cleanup: %v", err)
		}
	}
}

func (p *codingProof) cleanup() {
	if p.sessions != nil {
		p.sessions.removeAll()
	}
	p.removeAgents()
	if p.api != nil {
		p.api.close()
	}
}

// The status metadata of a turn a limit ended: the limit, and the usage.
const (
	stoppedByKey     = "stopped_by"
	stoppedByBudget  = "budget"
	usageMetadataKey = "kagent.dev/a2a/usage"
)

// readHarnessLimits reads a claude Harness's spec.claude.limits.
func readHarnessLimits(ctx context.Context, name string) (string, int64, error) {
	gvr, err := gvrFor(harnessResource)
	if err != nil {
		return "", 0, err
	}
	obj, err := getObject(ctx, gvr, kagentNamespace, name)
	if err != nil {
		return "", 0, err
	}
	budget, _, _ := unstructured.NestedString(obj.Object, "spec", "claude", "limits", "budgetUSD")
	turns, _, _ := unstructured.NestedInt64(obj.Object, "spec", "claude", "limits", "maxTurns")
	return budget, turns, nil
}

// readAgentEgress reads the Agent's spec.egress.
func readAgentEgress(ctx context.Context, name string) ([]string, error) {
	gvr, err := gvrFor(agentResource)
	if err != nil {
		return nil, err
	}
	obj, err := getObject(ctx, gvr, kagentNamespace, name)
	if err != nil {
		return nil, err
	}
	egress, _, err := unstructured.NestedStringSlice(obj.Object, "spec", "egress")
	return egress, err
}
