package lab

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
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
//  1. The Harness claude Ready from the values, no limits on it.
//  2. agent-manager's write-time checks on a claude Harness: validate_agent
//     accepts a toolset with a muster write tool and refuses an egress origin
//     outside the Harness's list. An agent-manager below
//     agentManagerAnyToolsetFloor refuses the write tool instead, and the
//     proof expects that refusal on it.
//  3. create_agent on claude with the read-only preset plus that write tool
//     (the read-only preset alone below the floor): Ready, its RemoteMCPServer
//     Accepted and sending the toolset, the Agent's egress the Harness's
//     origin list.
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
//  7. Usage: the resumed turn's task status carries its usage.
//  8. A share of the claude Session refused: a colleague's turn in a coding
//     agent's Session would run under code the first sender's turns left.
//
// The agents and Sessions are removed on every exit path.

const (
	// codingTestAgent is the proof's agent, deleted by the same run.
	codingTestAgent = "agentlab-coding"
	// codingTestToolset is the coding agent's toolset below
	// agentManagerAnyToolsetFloor, where a claude Harness agent is held to
	// read-only tools.
	codingTestToolset = presetReadOnly
	// codingTestGrantedTool is the muster write tool the coding agent's
	// toolset adds from agentManagerAnyToolsetFloor on: which tools a coding
	// agent uses is the person's choice.
	codingTestGrantedTool = "tool:" + agentManagerToolPrefix + "create_agent"
	// agentManagerAnyToolsetFloor is the first agent-manager release that
	// writes a claude Harness agent with any muster toolset; a release
	// candidate of it counts.
	agentManagerAnyToolsetFloor = "1.14.0-0"
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
	// anyToolset is whether the line's agent-manager writes a claude
	// Harness agent with any muster toolset.
	anyToolset bool
}

func (p *codingProof) run() error {
	ctx := context.Background()
	p.stages.begin("harnesses", "The coding Harness %s Ready from the platform values, no limits on it", claudeHarness)
	for _, h := range codingHarnessesFor(p.cfg, nil) {
		if err := waitHarnessReady(ctx, h.Name, p.timeout); err != nil {
			return err
		}
		limits, err := readHarnessLimits(ctx, h.Name)
		if err != nil {
			return err
		}
		if limits != nil {
			return fmt.Errorf("the Harness %s carries spec.claude.limits %v, which the values no longer render", h.Name, limits)
		}
		note("Harness %s/%s Ready, no limits", kagentNamespace, h.Name)
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
	if !p.anyToolset {
		note("agent-manager below %s holds a claude Harness agent to read-only tools: the write tool's refusal is expected", agentManagerAnyToolsetFloor)
	}
	modelConfig, err := agentManagerModelConfig(p.muster)
	if err != nil {
		return err
	}
	p.removeAgents()

	verdict := "accepted"
	if !p.anyToolset {
		verdict = "refused"
	}
	p.stages.begin("write-time checks", "%svalidate_agent on Harness %s as %s: a toolset with %s %s, an origin outside the Harness's list refused", agentManagerToolPrefix, claudeHarness, p.user.Email, codingTestGrantedTool, verdict)
	spec := agentSpec{
		Name: codingTestAgent, ModelConfig: modelConfig, Harness: claudeHarness, SystemMessage: codingTestSystemPrompt,
		Description: "Throwaway coding agent of `agentlab coding-test`; deleted by the same run.",
	}
	if err := p.proveGuards(spec); err != nil {
		return err
	}

	spec.Toolset = p.toolset()
	p.stages.begin("create", "%screate_agent %s on Harness %s, toolset %v: Ready, its RemoteMCPServer Accepted with the toolset, the Agent's egress the Harness's origins", agentManagerToolPrefix, codingTestAgent, claudeHarness, spec.Toolset)
	egress, err := p.createReady(spec)
	if err != nil {
		return err
	}
	if err := p.proveBinding(spec.Toolset); err != nil {
		return err
	}
	note("Agent %s Ready on %s, egress [%s]", codingTestAgent, claudeHarness, strings.Join(egress, " "))

	// The write tool the sandbox calls must exist for the person: its
	// refusal through the agent's endpoint is then the toolset's.
	tools, err := p.muster.listTools()
	if err != nil {
		return err
	}
	if !slices.Contains(tools, codingTestWriteTool) {
		return fmt.Errorf("%s is not in %s's muster catalogue: the sandbox's write-tool refusal would prove nothing", codingTestWriteTool, p.user.Email)
	}

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
	meta, err := p.suspendResume(ctx, session)
	if err != nil {
		return err
	}

	p.stages.begin("usage", "The resumed turn's task status carries its usage")
	usage, _ := meta[usageMetadataKey].(map[string]any)
	if len(usage) == 0 {
		return fmt.Errorf("the turn on Harness %s completed without %s (status metadata %v; %s)", claudeHarness, usageMetadataKey, meta, p.line)
	}
	note("cost %v USD over %v model turns", usage["costUsd"], usage["numTurns"])

	p.stages.begin("share refused", "A read-write share of the claude Session refused: a collaborator's turn would run under what the first sender's turns left")
	return p.shareRefused(ctx, session)
}

// releases words the line under test: the chart and the versions the
// controller and agent-manager report; it sets anyToolset from the latter.
func (p *codingProof) releases(ctx context.Context) string {
	parts := []string{"agent-platform " + firstNonEmpty(p.cfg.Platform.ChartVersion, p.cfg.Platform.ChartPath, "unread")}
	if v, err := p.api.version(ctx); err == nil {
		parts = append(parts, "kagent "+v.GetKagentVersion())
	}
	if info, err := readAgentManagerInfo(p.muster); err == nil {
		parts = append(parts, "agent-manager "+info.Version)
		p.anyToolset = takesAnyToolset(info.Version)
	}
	return strings.Join(parts, ", ")
}

// takesAnyToolset reports whether an agent-manager version writes a claude
// Harness agent with any muster toolset: a release at or above
// agentManagerAnyToolsetFloor, or a build whose version is no release (a dev
// build is of the current line).
func takesAnyToolset(version string) bool {
	v, err := semver.NewVersion(version)
	if err != nil {
		return true
	}
	return !v.LessThan(semver.MustParse(agentManagerAnyToolsetFloor))
}

// toolset is the coding agent's toolset on this line: the read-only preset
// plus a muster write tool, the read-only preset alone below the floor.
func (p *codingProof) toolset() []string {
	if p.anyToolset {
		return []string{codingTestToolset, codingTestGrantedTool}
	}
	return []string{codingTestToolset}
}

// proveGuards is validate_agent on a claude Harness agent: the toolset with
// a muster write tool accepted (refused below the floor), an origin outside
// the Harness's list refused.
func (p *codingProof) proveGuards(spec agentSpec) error {
	writeToolset := []string{codingTestToolset, codingTestGrantedTool}
	report, err := p.validate(spec, func(args map[string]any) { args[toolsetKey] = writeToolset })
	if err != nil {
		return err
	}
	switch {
	case p.anyToolset && !report.Valid:
		return fmt.Errorf("toolset %v on %s: validate_agent answered valid=false errors=%v, wanted it accepted", writeToolset, p.line, report.Errors)
	case p.anyToolset:
		note("toolset %v accepted", writeToolset)
	case !report.refused("read-only"):
		return fmt.Errorf("toolset %v on %s: validate_agent answered valid=%v errors=%v, wanted the read-only refusal of an agent-manager below %s", writeToolset, p.line, report.Valid, report.Errors, agentManagerAnyToolsetFloor)
	default:
		note("toolset %v refused below %s: %s", writeToolset, agentManagerAnyToolsetFloor, excerpt(strings.Join(report.Errors, "; "), 240))
	}

	origin := "egress [" + codingTestRefusedOrigin + "]"
	report, err = p.validate(spec, func(args map[string]any) {
		args[toolsetKey] = []string{codingTestToolset}
		args[egressKey] = []string{codingTestRefusedOrigin}
	})
	if err != nil {
		return err
	}
	if !report.refused("is not one of Harness") {
		return fmt.Errorf("%s on %s: validate_agent answered valid=%v errors=%v, wanted the refusal %q", origin, p.line, report.Valid, report.Errors, "is not one of Harness")
	}
	note("%s refused: %s", origin, excerpt(strings.Join(report.Errors, "; "), 240))
	return nil
}

// validate is validate_agent on the spec's create arguments as mutate
// changes them.
func (p *codingProof) validate(spec agentSpec, mutate func(args map[string]any)) (validateReport, error) {
	var report validateReport
	args, err := createAgentArgs(spec)
	if err != nil {
		return report, err
	}
	mutate(args)
	err = p.muster.callServerJSON(agentManagerToolPrefix+"validate_agent", args, &report)
	return report, err
}

// refused reports whether validate_agent refused with an error containing want.
func (r validateReport) refused(want string) bool {
	return !r.Valid && slices.ContainsFunc(r.Errors, func(e string) bool { return strings.Contains(e, want) })
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

// proveBinding is the agent's MCP binding: the RemoteMCPServer its template
// binds Accepted, sending the toolset as declared.
func (p *codingProof) proveBinding(toolset []string) error {
	t, err := readAgentTemplate(codingTestAgent)
	if err != nil {
		return err
	}
	header, found, err := toolsetHeaderOf(t)
	if err != nil {
		return fmt.Errorf("agent %s: %w", codingTestAgent, err)
	}
	if want := strings.Join(toolset, ","); !found || header != want {
		return fmt.Errorf("agent %s binds %s %s with %s=%q (present %v), wanted %q", codingTestAgent, remoteMCPServerKind, t.mcpServer(), toolsetHeader, header, found, want)
	}
	var accepted string
	waitFor(int(p.timeout/pollInterval), pollInterval, func() bool {
		rms, err := readKagentObject(remoteMCPServerResource, t.mcpServer())
		accepted = ""
		if err == nil {
			accepted = conditionStatus(rms, conditionAccepted)
		}
		return accepted == conditionTrue
	})
	if accepted != conditionTrue {
		return fmt.Errorf("%s %s of agent %s: Accepted=%q after %s, wanted True (%s)", remoteMCPServerKind, t.mcpServer(), codingTestAgent, accepted, p.timeout, p.line)
	}
	note("binds %s/%s Accepted, %s=%s", remoteMCPServerKind, t.mcpServer(), toolsetHeader, header)
	return nil
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
// first turn's file and recall its code word; it returns that turn's status
// metadata.
func (p *codingProof) suspendResume(ctx context.Context, session *apiv1alpha1.Session) (map[string]any, error) {
	opCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	suspended, err := p.api.suspendSession(opCtx, session.GetId())
	if err != nil {
		return nil, err
	}
	resumed, err := p.api.resumeSession(opCtx, session.GetId())
	if err != nil {
		return nil, err
	}
	note("Session %s: %s, then %s", session.GetId(), sessionState(suspended), sessionState(resumed))
	reply, meta, err := p.turn(ctx, resumed, codingResumePrompt())
	if err != nil {
		return nil, err
	}
	probe := parseCodingProbe(reply)
	if probe["file"] != codingTestCodeWord || !strings.Contains(probe["code-word"], codingTestCodeWord) {
		return nil, fmt.Errorf("after the resume the first turn's file reads %q and the code word recalled is %q, wanted %s for both (%s)", probe["file"], probe["code-word"], codingTestCodeWord, p.line)
	}
	note("the file %s reads %s, the code word recalled from the conversation", codingTestOwnFile, probe["file"])
	return meta, nil
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

// removeAgents removes the proof's agent, a leftover of an earlier run too.
func (p *codingProof) removeAgents() {
	if !agentExists(codingTestAgent) {
		return
	}
	if err := removeAgent(codingTestAgent); err != nil {
		note("cleanup: %v", err)
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

// usageMetadataKey is the status metadata a turn's usage is under.
const usageMetadataKey = "kagent.dev/a2a/usage"

// readHarnessLimits reads a claude Harness's spec.claude.limits, nil where
// it carries none.
func readHarnessLimits(ctx context.Context, name string) (any, error) {
	gvr, err := gvrFor(harnessResource)
	if err != nil {
		return nil, err
	}
	obj, err := getObject(ctx, gvr, kagentNamespace, name)
	if err != nil {
		return nil, err
	}
	limits, _, _ := unstructured.NestedFieldNoCopy(obj.Object, harnessLimitsPath...)
	return limits, nil
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
