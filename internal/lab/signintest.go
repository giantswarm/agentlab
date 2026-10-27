package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The sign-in proof (docs/platform.md "The sign-in proof"): an agent that
// calls a tool of a server the person has not signed in to gets muster's
// sign-in link as an MCP URL elicitation, its runtime pauses the turn at
// input-required with the link (kagent's url_elicitation_request), and the
// person's answer resumes the same task: accept once signed in runs the call,
// decline ends it without the call, and the conversation goes on either way.
// The server is the OAuth fixture (oauthfixture.go), whose sign-in the proof
// completes headlessly as the person.

const (
	signInTestAgentPrefix = "signin-test-"
	// signInTestTool is the fixture's tool the proof calls, the fixture
	// being muster: filter_tools for one tool, whose answer is short enough
	// for a model to quote whole.
	signInTestTool          = "x_" + oauthFixtureServer + "_filter_tools"
	signInTestToolArguments = `{"pattern":"core_auth_login"}`
	// signInTestPrompt asks for the fixture's tool through muster's
	// call_tool; the mockllm's sign-in turn answers it for codex, matching
	// signInTestPromptLead, which has no quote for JSON to escape.
	signInTestPromptLead = "Sign-in check: call muster's call_tool tool with name " + signInTestTool
	signInTestPrompt     = signInTestPromptLead + " and arguments " + signInTestToolArguments +
		", then reply with the tool's answer as it came back, nothing else."
	signInTestSystemPrompt = "You are a throwaway agent of `agentlab signin-test`. Do exactly what the user asks with your tools, and never ask questions."
	// signInTestFollowUp is the turn after a decided sign-in: it completes
	// only on a conversation the decision left whole.
	signInTestFollowUp      = "Reply with the single word READY."
	signInTestFollowUpReply = "READY"
)

// SignInTestOptions tune `agentlab signin-test`.
type SignInTestOptions struct {
	// Harnesses are the Harnesses proven, of claude, codex, omp and kagent
	// (the platform Harness); empty proves every one the lab renders.
	Harnesses []string
	// ModelConfig is the model of the claude and kagent agents (default
	// default-model-config). codex runs on the lab's Responses model, else
	// the mockllm.
	ModelConfig   string
	MockLLMBinary string
	Timeout       time.Duration
}

// signInProof is one run's state.
type signInProof struct {
	cfg      *config.Config
	user     *config.User
	token    string
	muster   *musterSession
	api      *kagentAPI
	sessions *sessionSet
	opts     SignInTestOptions
	mock     *mockLLM
	verdicts []string
}

// SignInTest runs the sign-in proof as email on every Harness asked for.
func SignInTest(cfg *config.Config, email string, opts SignInTestOptions) error {
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
	if opts.ModelConfig == "" {
		opts.ModelConfig = defaultModelConfig
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}
	p := &signInProof{cfg: cfg, user: user, opts: opts}
	step("Logging in to Dex as %s", user.Email)
	var err error
	if p.token, err = passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes); err != nil {
		return err
	}
	if p.muster, err = openMusterSession(cfg, p.token, "signin-test"); err != nil {
		return err
	}
	if p.api, err = dialKagentAPI(cfg, p.token); err != nil {
		return err
	}
	p.sessions = &sessionSet{api: p.api, manager: p.muster}
	harnesses, err := p.harnesses()
	if err != nil {
		return err
	}
	defer p.cleanup(harnesses)
	p.signOut()

	for _, harness := range harnesses {
		if err := p.prove(harness); err != nil {
			return fmt.Errorf("on Harness %s: %w", harness, err)
		}
	}
	fmt.Println()
	fmt.Println(strings.Join(p.verdicts, "\n"))
	return nil
}

// harnesses are the Harnesses to prove: those asked for, else kagent and
// the coding Harnesses of claude, codex and omp the namespace has.
func (p *signInProof) harnesses() ([]string, error) {
	rendered, err := namespaceHarnesses(context.Background())
	if err != nil {
		return nil, err
	}
	known := []string{kagentHarness, config.CodingRuntimeClaude, config.CodingRuntimeCodex, config.CodingRuntimeOmp}
	if len(p.opts.Harnesses) == 0 {
		out := []string{kagentHarness}
		for _, h := range known[1:] {
			if slices.Contains(rendered, h) {
				out = append(out, h)
			}
		}
		return out, nil
	}
	for _, h := range p.opts.Harnesses {
		if !slices.Contains(known, h) {
			return nil, fmt.Errorf("--harness %s: the proof knows %s", h, strings.Join(known, ", "))
		}
		if h != kagentHarness && !slices.Contains(rendered, h) {
			return nil, fmt.Errorf("the lab renders no Harness %s: add it to platform.coding.harnesses in %s, then `agentlab platform`", h, config.File)
		}
	}
	return p.opts.Harnesses, nil
}

// prove is the proof on one Harness: an agent there, a declined sign-in in
// one conversation, an accepted one in another, then the person signed out
// again.
func (p *signInProof) prove(harness string) error {
	name := signInTestAgentPrefix + harness
	step("An agent on Harness %s whose toolset names %s, created as %s", harness, toolsetFixtureSelector, p.user.Email)
	model, err := p.modelFor(harness)
	if err != nil {
		return err
	}
	spec := agentSpec{
		Name: name, ModelConfig: model, SystemMessage: signInTestSystemPrompt, Toolset: []string{toolsetFixtureSelector},
		Description: "Throwaway agent of `agentlab signin-test`; deleted by the same run.",
	}
	if harness != kagentHarness {
		spec.Harness = harness
	}
	if agentExists(name) {
		if err := removeAgent(name); err != nil {
			return err
		}
	}
	if _, err := (agentManagerWriter{p.muster}).createAgent(spec); err != nil {
		return err
	}
	boot, err := waitAgentReadyOn(name, harness, p.opts.Timeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return fmt.Errorf("agent %s is not Ready after %s: %s", name, boot.elapsed.Round(time.Second), boot.reason)
	}
	note("%s Ready in %s", name, boot.elapsed.Round(time.Second))
	if harness == config.CodingRuntimeCodex && model == MockLLMModelConfig {
		if err := p.startMock(name); err != nil {
			return err
		}
	}

	step("Declined: the turn pauses at input-required on muster's sign-in link; decline ends it without the call, and the conversation goes on")
	session, paused, err := p.pausedOnSignIn(name)
	if err != nil {
		return err
	}
	decided := time.Now()
	declined, err := p.answer(session, paused, signInDecline)
	if err != nil {
		return err
	}
	if declined.state() != a2a.TaskStateCompleted {
		return fmt.Errorf("after decline the task ended %s (%s): %s", stateName(declined.state()), declined.statesString(), excerpt(declined.text(), 300))
	}
	if ran, err := p.fixtureCallsSince(decided); err != nil {
		return err
	} else if len(ran) > 0 {
		return fmt.Errorf("after decline muster ran %s: %s", signInTestTool, strings.Join(ran, ", "))
	}
	if err := p.followUp(session); err != nil {
		return fmt.Errorf("after decline: %w", err)
	}
	p.pass("%s — declined: paused at input-required on %s's link (%s), the decline completed the task without the call, the next turn completed", harness, paused.signIn.Server, linkPath(paused.signIn.URL))

	step("Accepted: the same pause, the sign-in completed as %s, accept runs the call once", p.user.Email)
	session, paused, err = p.pausedOnSignIn(name)
	if err != nil {
		return err
	}
	if err := completeSignIn(paused.signIn.URL, p.user); err != nil {
		return fmt.Errorf("completing the sign-in at %s: %w", linkPath(paused.signIn.URL), err)
	}
	note("Dex login completed; muster's proxy callback answered Authentication Successful")
	decided = time.Now()
	accepted, err := p.answer(session, paused, signInAccept)
	if err != nil {
		return err
	}
	if accepted.state() != a2a.TaskStateCompleted {
		return fmt.Errorf("after accept the task ended %s (%s): %s", stateName(accepted.state()), accepted.statesString(), excerpt(accepted.text(), 300))
	}
	answered := accepted.text()
	for _, tool := range accepted.tools {
		answered += "\n" + tool.output
	}
	if !callAnswered(answered) {
		return fmt.Errorf("after accept neither the reply nor a tool result carries the fixture's tools (%s): %s", signInTestToolProof, excerpt(answered, 300))
	}
	if ran, err := p.fixtureCallsSince(decided); err != nil {
		return err
	} else if len(ran) != 1 {
		return fmt.Errorf("after accept muster ran %s %d times, want once: %s", signInTestTool, len(ran), strings.Join(ran, ", "))
	}
	if err := p.followUp(session); err != nil {
		return fmt.Errorf("after accept: %w", err)
	}
	p.pass("%s — accepted: the sign-in completed through Dex and muster's callback, accept resumed the same task (%s) and the call answered the fixture's tools, the next turn completed", harness, accepted.taskID)

	// Signed out, the fixture stays Connected; restarted, it is Auth
	// Required again, so the next Harness's call meets the sign-in.
	p.signOut()
	restartOAuthFixture()
	return removeAgent(name)
}

// pausedOnSignIn starts a conversation of agent the way Slack and the
// portal do (startBoundSession) and asks for the fixture's tool: the turn
// must pause at input-required on muster's sign-in link.
func (p *signInProof) pausedOnSignIn(agent string) (*apiv1alpha1.Session, *turn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.opts.Timeout)
	defer cancel()
	session, err := startBoundSession(ctx, p.muster, p.api, p.sessions, agent)
	if err != nil {
		return nil, nil, err
	}
	t, err := p.api.turnOn(session, userMessage(signInTestPrompt))
	if err != nil {
		return nil, nil, err
	}
	if t.state() != a2a.TaskStateInputRequired || t.signIn == nil {
		return nil, nil, fmt.Errorf("the turn ended %s (%s), not paused at input-required on a url_elicitation_request: %s", stateName(t.state()), t.statesString(), excerpt(t.text(), 300))
	}
	if path := linkPath(t.signIn.URL); !strings.HasSuffix(path, oauthProxyStartPath) {
		return nil, nil, fmt.Errorf("the sign-in link is %s, not muster's %s", path, oauthProxyStartPath)
	}
	note("Session %s paused at input-required on %s: %q, link %s", session.GetId(), t.signIn.Server, t.signIn.Message, linkPath(t.signIn.URL))
	return session, t.turn, nil
}

// answer resumes the paused task with action.
func (p *signInProof) answer(session *apiv1alpha1.Session, paused *turn, action string) (*turn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	defer cancel()
	t, err := p.api.answerSignIn(ctx, session, paused, action)
	if err != nil {
		return nil, err
	}
	if t.taskID != paused.taskID {
		return nil, fmt.Errorf("%s resumed task %s, not the paused task %s", action, t.taskID, paused.taskID)
	}
	note("%s: task %s ended %s (%s)", action, t.taskID, stateName(t.state()), excerpt(t.text(), 160))
	return t, nil
}

// followUp is one more turn of the conversation, which must complete.
func (p *signInProof) followUp(session *apiv1alpha1.Session) error {
	t, err := p.api.turnOn(session, userMessage(signInTestFollowUp))
	if err != nil {
		return err
	}
	_, err = t.completedText()
	return err
}

// modelFor is the ModelConfig of the agent on harness.
func (p *signInProof) modelFor(harness string) (string, error) {
	if harness != config.CodingRuntimeCodex {
		return p.opts.ModelConfig, nil
	}
	if model := p.cfg.CodexModelConfig(); model != "" {
		return model, nil
	}
	if err := ensureMockLLMModelConfig(); err != nil {
		return "", err
	}
	return MockLLMModelConfig, nil
}

// startMock runs the mockllm for the codex agent's MCP binding.
func (p *signInProof) startMock(agent string) error {
	binary, err := mockLLMBinary(p.opts.MockLLMBinary)
	if err != nil {
		return err
	}
	t, err := readAgentTemplate(agent)
	if err != nil {
		return err
	}
	if p.mock, err = startMockLLM(p.cfg, binary, t.mcpServer(), "", nil); err != nil {
		return err
	}
	note("codex runs on the mockllm (%s), scripted to call %s through call_tool", mockLLMURL(), signInTestTool)
	return nil
}

// signOut forgets the person's grant for the fixture, so the next agent
// meets the sign-in again.
func (p *signInProof) signOut() {
	if err := signOutOfFixture(p.cfg, p.token); err != nil {
		note("signing out of %s: %v", oauthFixtureServer, err)
	}
}

func (p *signInProof) pass(format string, a ...any) {
	p.verdicts = append(p.verdicts, "PASS: "+fmt.Sprintf(format, a...))
}

func (p *signInProof) cleanup(harnesses []string) {
	p.sessions.removeAll()
	for _, h := range harnesses {
		if name := signInTestAgentPrefix + h; agentExists(name) {
			if err := removeAgent(name); err != nil {
				note("cleanup: %v", err)
			}
		}
	}
	if p.mock != nil {
		p.mock.close()
	}
	p.signOut()
	restartOAuthFixture()
}

// linkPath is a link without its query, which carries the flow's state.
func linkPath(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return "an unparsable link"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// signInTestToolProof matches the count field of the fixture's
// filter_tools answer, the fixture being muster, also inside JSON strings
// nested to any depth; a refusal of the call carries no count.
var signInTestToolProof = regexp.MustCompile(`filtered_count[\\"]*\s*:\s*\d`)

// fixtureCallsSince lists muster's successful dispatches of the fixture's
// tool as the person since the instant.
func (p *signInProof) fixtureCallsSince(since time.Time) ([]string, error) {
	identity, err := tokenIdentity(p.token, time.Now())
	if err != nil {
		return nil, err
	}
	logs, err := musterLogSince(since)
	if err != nil {
		return nil, err
	}
	return fixtureCallsIn(logs, since, identity.subject), nil
}

// fixtureCallsIn lists, from the instant on, the subject's dispatches of the
// fixture's tool that muster ran: a refused or declined call logs none.
func fixtureCallsIn(logs string, since time.Time, subject string) []string {
	var ran []string
	for _, line := range strings.Split(logs, "\n") {
		var entry struct {
			Time    time.Time `json:"time"`
			Msg     string    `json:"msg"`
			Tool    string    `json:"tool"`
			Subject string    `json:"subject"`
			Outcome string    `json:"outcome"`
		}
		start := strings.IndexByte(line, '{')
		if start < 0 || json.Unmarshal([]byte(line[start:]), &entry) != nil || entry.Time.Before(since) {
			continue
		}
		if entry.Msg == "tool dispatched" && entry.Tool == signInTestTool && entry.Outcome == "ok" && subjectMatches(entry.Subject, subject) {
			ran = append(ran, entry.Time.UTC().Format(time.RFC3339Nano))
		}
	}
	return ran
}

// callAnswered reports whether text carries the fixture's filter_tools
// answer.
func callAnswered(text string) bool {
	return signInTestToolProof.MatchString(text)
}
