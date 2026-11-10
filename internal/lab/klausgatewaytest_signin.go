package lab

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The sign-in surfaces proof (`klaus-gateway-test --part signin`,
// docs/klaus-gateway.md "The sign-in surfaces"): a turn of an agent whose
// toolset is the OAuth fixture pauses at input-required on muster's sign-in
// link (kagent's url_elicitation_request), and klaus-gateway's Slack prompt
// and the portal's answer route decide it. Every decision resumes the paused
// task; an answer to a request the task no longer waits on — the other
// surface answered first — is refused instead of deciding a request its
// person never saw.

// KlausGatewayPartSignIn is `klaus-gateway-test --part signin`.
const KlausGatewayPartSignIn = "signin"

const (
	klausGatewaySignInAgent   = "klaus-gateway-signin"
	klausGatewaySignInDisplay = "Swarmgeist sign-in fixture"
	// klausGatewaySignInRetryPrompt makes a declined call pause the same
	// task again: muster asks for the sign-in a second time, in a new status
	// message.
	klausGatewaySignInRetryPrompt = signInTestPromptLead + " and arguments " + signInTestToolArguments +
		". If the call is declined, call it again exactly once. Then reply with the single word DONE."

	slackActionConnect       = "connector_connect"
	slackActionSignInDecline = "sign_in_decline"
	gatewayContinuing        = "Opened the sign-in link"
	gatewaySignedInLine      = " signed in."
	gatewayNotSignedInLine   = " did not sign in."
	gatewayNotSignedIn       = "Not signed in"
	gatewayAnsweredElsewhere = "This was already answered elsewhere, so this answer was not sent to the agent."
	outcomePromptAnswered    = "prompt_answered"
	// portalAnsweredByAnother is the portal's 409 for an answer to a request
	// the task no longer asks (giantswarm/backstage#3331).
	portalAnsweredByAnother = "already answered"
)

// The Accepted stage's hold: how long the runtime's retry stays parked in
// muster's sign-in wait before the sign-in completes. A hold longer than the
// MCP call timeout the Harness applies to the toolset (on claude the
// RemoteMCPServer's timeout, 30s by default) proves the retry survives it;
// the hold stays a minute inside muster's wait so the sign-in still lands.
const (
	signInDefaultHold = 10 * time.Second
	musterSignInWait  = 5 * time.Minute
	signInMaxHold     = musterSignInWait - time.Minute
)

// gatewayCallbackBase is the host-mode gateway's OBO callback base and its
// public base.
func gatewayCallbackBase(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }

// checkSignInHold refuses a hold outside the signin part, a negative one
// and one that outlasts muster's sign-in wait.
func checkSignInHold(opts KlausGatewayTestOptions) error {
	switch {
	case opts.Hold == 0:
		return nil
	case opts.Part != KlausGatewayPartSignIn:
		return fmt.Errorf("--hold applies to --part %s alone", KlausGatewayPartSignIn)
	case opts.Hold < 0:
		return fmt.Errorf("--hold %s: want a positive duration", opts.Hold)
	case opts.Hold > signInMaxHold:
		return fmt.Errorf("--hold %s outlasts muster's %s sign-in wait less the minute the sign-in takes; at most %s", opts.Hold, musterSignInWait, signInMaxHold)
	}
	return nil
}

// signInSurfaces is one run of the part.
type signInSurfaces struct {
	cfg      *config.Config
	user     *config.User
	token    string
	subject  string
	api      *kagentAPI
	slack    *slackProof
	portal   *portalSession
	harness  string
	hold     time.Duration
	verdicts []string
}

// klausGatewaySignInTest is the part: the fixture, the gateway on the host,
// a portal session as the same person, then the stages.
func klausGatewaySignInTest(cfg *config.Config, user *config.User, opts KlausGatewayTestOptions) error {
	if !cfg.Backstage.Enabled {
		return fmt.Errorf("--part %s answers through the portal too; enable backstage in %s and run `agentlab platform` first", KlausGatewayPartSignIn, config.File)
	}
	runDir, cleanRunDir, err := klausGatewayRunDir(opts.RunDir)
	if err != nil {
		return err
	}
	defer cleanRunDir()

	step("Logging in to Dex as %s", user.Email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	identity, err := tokenIdentity(token, time.Now())
	if err != nil {
		return fmt.Errorf("the id_token of %s: %w", user.Email, err)
	}
	api, err := dialKagentAPI(cfg, token)
	if err != nil {
		return err
	}
	defer api.close()
	s := &signInSurfaces{cfg: cfg, user: user, token: token, subject: identity.subject, api: api, harness: opts.Harness, hold: opts.Hold}
	s.cleanup()
	defer s.cleanup()

	run := strings.ToUpper(randomSuffix())
	person := slackUserPrefix + run + "P"
	fake, err := startFakeSlack(net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port+2)), map[string]string{person: user.Email}, nil)
	if err != nil {
		return err
	}
	defer fake.close()

	muster, err := openMusterSession(cfg, token, "klaus-gateway-test")
	if err != nil {
		return err
	}
	if opts.ModelConfig == "" {
		if opts.ModelConfig, err = agentManagerModelConfig(muster); err != nil {
			return err
		}
	}
	step("Creating Agent %s on Harness %s through agent-manager's create_agent as %s, its toolset %s (the OAuth fixture %s, Auth Required)",
		klausGatewaySignInAgent, opts.Harness, user.Email, toolsetFixtureSelector, oauthFixtureServer)
	spec := agentSpec{
		Name: klausGatewaySignInAgent, ModelConfig: opts.ModelConfig, DisplayName: klausGatewaySignInDisplay,
		SystemMessage: signInTestSystemPrompt, Toolset: []string{toolsetFixtureSelector},
		Description: "Throwaway agent of `agentlab klaus-gateway-test --part signin`; deleted by the same run.",
	}
	if opts.Harness != kagentHarness {
		spec.Harness = opts.Harness
	}
	if _, err := (agentManagerWriter{muster}).createAgent(spec); err != nil {
		return err
	}
	boot, err := waitAgentReadyOn(klausGatewaySignInAgent, opts.Harness, opts.ReadyTimeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return boot.failure(klausGatewaySignInAgent, opts.ReadyTimeout)
	}
	note("Ready after %s", boot.elapsed.Round(time.Second))

	keys, err := writeGatewayFiles(runDir)
	if err != nil {
		return err
	}
	if err := seedBoltLink(filepath.Join(runDir, gatewayLinksFile), keys.store, person, identity.link(user.Email, token)); err != nil {
		return err
	}
	edge, err := edgeHostPort(cfg.AgentgatewayBaseURL())
	if err != nil {
		return err
	}
	gate, err := startTCPGate(net.JoinHostPort(cfg.GatewayHostAddress(), strconv.Itoa(opts.Port+3)), edge)
	if err != nil {
		return fmt.Errorf("the gate in front of the edge: %w", err)
	}
	defer gate.stop()
	target := gatedTarget(edge, opts.Port+3)
	caFile, err := filepath.Abs(trustBundleFile(cfg))
	if err != nil {
		return err
	}
	gw := newGatewayProcess(opts, runDir, caFile, target, fake.baseURL(), cfg.MusterBaseURL())
	gw.container = gatewayContainerName(cfg)
	gw.trustLabCA = true
	gw.defaultAgent = klausGatewaySignInAgent
	gw.extraArgs = []string{"--obo-connectors-enabled=true"}
	defer func() { _ = gw.stop() }()
	step("Starting klaus-gateway %s on the host with its connector prompts on, its Web API the fake at %s, a2a target %s",
		gw.describe(), fake.baseURL(), target)
	if err := gw.start(); err != nil {
		return err
	}
	note("ready: %s", gw.version())
	channel := "CAGENTLAB" + run
	s.slack = &slackProof{
		driver: newSlackDriver(gw.baseURL(), keys.signing, fake, channel),
		fake:   fake, channel: channel, agent: klausGatewaySignInAgent,
		logs: func() (string, error) { return gw.logs(), nil },
	}
	step("Signing %s in to the portal: its answer route is the portal's half of every decision", user.Email)
	if s.portal, err = backstageSignIn(cfg, user); err != nil {
		return err
	}
	s.signOut()

	for _, stage := range []struct {
		name string
		run  func(*slackThread) error
	}{
		{"Not now", s.notNow},
		{"a typed refusal", s.typedRefusal},
		{"stop on the paused turn", s.stopped},
		{"the portal first, then Slack", s.portalFirst},
		{"Slack first, then the portal", s.slackFirst},
		{"accepted", s.accepted},
	} {
		if err := stage.run(&slackThread{user: person}); err != nil {
			return fmt.Errorf("%s: %w", stage.name, err)
		}
	}

	step("Deleting the fixture, the Sessions and the gateway")
	if err := gw.stop(); err != nil {
		return err
	}
	s.cleanup()
	fmt.Println()
	fmt.Println(strings.Join(s.verdicts, "\n"))
	return nil
}

// signInPause is a turn paused on the sign-in, as Slack and the controller
// show it.
type signInPause struct {
	turn    *slackTurn
	prompt  slackMessage
	link    string
	session *apiv1alpha1.Session
	task    *a2a.Task
	before  int
}

// asked is the status message the paused task asks in.
func (p *signInPause) asked() string {
	if p.task.Status.Message == nil {
		return ""
	}
	return p.task.Status.Message.ID
}

// pause asks for the fixture's tool in the thread: the turn must pause at
// input-required, the controller hold the task there on a
// url_elicitation_request, and the person get the sign-in prompt.
func (s *signInSurfaces) pause(t *slackThread, text string) (*signInPause, error) {
	before := len(s.slack.fake.thread(s.slack.channel, t.ts))
	var turn *slackTurn
	var err error
	if t.ts == "" {
		turn, err = s.slack.firstTurn(t, text)
	} else {
		turn, err = s.slack.say(t, text)
	}
	if err != nil {
		return nil, err
	}
	if turn.record.Outcome != outcomeInputReq {
		return nil, fmt.Errorf("the turn did not pause on the sign-in: %s", turnFailure(turn))
	}
	return s.paused(t, turn, before)
}

// paused reads a turn that ended at input-required: its sign-in prompt in
// the thread and its task at the controller.
func (s *signInSurfaces) paused(t *slackThread, turn *slackTurn, before int) (*signInPause, error) {
	msgs, ok := waitThread(s.slack.fake, s.slack.channel, t.ts, klausGatewayReplyWait, func(msgs []slackMessage) bool {
		_, found := signInPromptIn(msgs, before, t.user)
		return found
	})
	if !ok {
		return nil, fmt.Errorf("task %s paused but no sign-in prompt (%s and %s) reached %s within %s: %s",
			turn.record.TaskID, slackActionConnect, slackActionSignInDecline, t.user, klausGatewayReplyWait, threadLine(msgs))
	}
	prompt, _ := signInPromptIn(msgs, before, t.user)
	button, _ := prompt.action(slackActionConnect)
	link, _ := button[slackKeyURL].(string)
	session, err := s.slack.boundSession(s.api, t)
	if err != nil {
		return nil, err
	}
	task, err := s.task(session, turn.record.TaskID)
	if err != nil {
		return nil, err
	}
	elicitation := parseURLElicitationRequest(task.Status.Message)
	if task.Status.State != a2a.TaskStateInputRequired || elicitation == nil {
		return nil, fmt.Errorf("the thread shows a sign-in prompt for task %s but the controller has it %s without a url_elicitation_request", task.ID, task.Status.State)
	}
	if link != elicitation.URL {
		return nil, fmt.Errorf("the sign-in button opens %s, not the url_elicitation_request's %s: the gateway must open the server's link unchanged", linkPath(link), linkPath(elicitation.URL))
	}
	if shown := signInLinkShown(link); !strings.Contains(prompt.Text, shown) {
		return nil, fmt.Errorf("the sign-in prompt does not show %q before the link is opened: %s", shown, excerpt(prompt.Text, 300))
	}
	note("task %s of Session %s paused at input-required (status message %s); the prompt went to %s alone, showing its link %s whole and opening it unchanged",
		task.ID, session.GetId(), task.Status.Message.ID, prompt.Recipient, linkPath(link))
	return &signInPause{turn: turn, prompt: prompt, link: link, session: session, task: task, before: len(msgs)}, nil
}

func (s *signInSurfaces) task(session *apiv1alpha1.Session, id string) (*a2a.Task, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	return s.api.getTask(ctx, session, a2a.TaskID(id))
}

// resumed waits for the turn a decision set off and checks it resumed the
// paused task.
func (s *signInSurfaces) resumed(t *slackThread, paused *signInPause) (*slackTurn, error) {
	turn, err := s.slack.awaitTurn(t, paused.before)
	if err != nil {
		return nil, err
	}
	dispatched := s.slack.records(recordDispatch, t.ts)
	if len(dispatched) == 0 {
		return nil, fmt.Errorf("the gateway's log carries no turn_dispatch record for thread %s", t.ts)
	}
	if last := dispatched[len(dispatched)-1]; !last.Resume || last.TaskID != string(paused.task.ID) {
		return nil, fmt.Errorf("the decision dispatched a turn on task %q (resume %t), not a resume of the paused task %s", last.TaskID, last.Resume, paused.task.ID)
	}
	return turn, nil
}

// declined checks a decline: the task completed without the call, the
// thread says so, and the conversation goes on.
func (s *signInSurfaces) declined(t *slackThread, paused *signInPause, decided time.Time, how string) error {
	turn, err := s.resumed(t, paused)
	if err != nil {
		return err
	}
	if turn.record.Outcome != outcomeCompleted {
		return fmt.Errorf("after %s the turn ended %s", how, turnFailure(turn))
	}
	if ran, err := s.fixtureCallsSince(decided); err != nil {
		return err
	} else if len(ran) > 0 {
		return fmt.Errorf("after %s muster ran %s: %s", how, signInTestTool, strings.Join(ran, ", "))
	}
	if _, ok := findMessage(s.slack.fake.thread(s.slack.channel, t.ts), "<@"+t.user+">"+gatewayNotSignedInLine); !ok {
		return fmt.Errorf("after %s the thread does not say %q: %s", how, "<@"+t.user+">"+gatewayNotSignedInLine, threadLine(turn.msgs))
	}
	return s.followUp(t)
}

// followUp is one more turn in the thread, which must complete: a decided
// sign-in left the conversation whole, with no tool call waiting on a
// result.
func (s *signInSurfaces) followUp(t *slackThread) error {
	turn, err := s.slack.say(t, signInTestFollowUp)
	if err != nil {
		return err
	}
	return assertSlackTurnSaid(turn, signInTestFollowUpReply)
}

func (s *signInSurfaces) notNow(t *slackThread) error {
	step("Not now: the turn pauses on muster's sign-in link, the prompt goes to %s alone; Not now resumes the same task with decline, the call never runs, the next turn completes", s.user.Email)
	paused, err := s.pause(t, signInTestPrompt)
	if err != nil {
		return err
	}
	decided := time.Now()
	if err := s.slack.driver.click(t.user, paused.prompt, slackActionSignInDecline); err != nil {
		return err
	}
	if err := s.declined(t, paused, decided, "Not now"); err != nil {
		return err
	}
	if !responded(s.slack.fake, gatewayNotSignedIn) {
		return fmt.Errorf("the prompt was not rewritten to %q", gatewayNotSignedIn)
	}
	s.pass("Not now on the sign-in prompt resumed task %s with decline; the call never reached muster, the prompt read %q, the next turn completed", paused.task.ID, gatewayNotSignedIn)
	return nil
}

func (s *signInSurfaces) typedRefusal(t *slackThread) error {
	step("A typed refusal: `no` in the thread resumes the paused task with decline")
	paused, err := s.pause(t, signInTestPrompt)
	if err != nil {
		return err
	}
	decided := time.Now()
	if _, err := s.slack.driver.reply(t.user, t.ts, "no"); err != nil {
		return err
	}
	if err := s.declined(t, paused, decided, "`no`"); err != nil {
		return err
	}
	s.pass("`no` typed in the thread resumed task %s with decline; the call never reached muster, the next turn completed", paused.task.ID)
	return nil
}

func (s *signInSurfaces) stopped(t *slackThread) error {
	step("stop on the paused turn: the task ends without the call, and the next turn completes — no tool call left waiting on a result")
	paused, err := s.pause(t, signInTestPrompt)
	if err != nil {
		return err
	}
	decided := time.Now()
	if _, err := s.slack.driver.reply(t.user, t.ts, "stop"); err != nil {
		return err
	}
	if _, err := s.slack.awaitTurn(t, paused.before); err != nil {
		return err
	}
	task, err := s.task(paused.session, string(paused.task.ID))
	if err != nil {
		return err
	}
	if task.Status.State == a2a.TaskStateInputRequired {
		return fmt.Errorf("after stop task %s still waits at input-required", task.ID)
	}
	if ran, err := s.fixtureCallsSince(decided); err != nil {
		return err
	} else if len(ran) > 0 {
		return fmt.Errorf("after stop muster ran %s: %s", signInTestTool, strings.Join(ran, ", "))
	}
	if err := s.followUp(t); err != nil {
		return fmt.Errorf("the turn after stop: %w", err)
	}
	s.pass("stop on the paused turn left task %s %s without the call; the next turn completed, so no tool call was left without its result", task.ID, task.Status.State)
	return nil
}

func (s *signInSurfaces) portalFirst(t *slackThread) error {
	step("The portal first: the portal's answer route declines the paused task; Not now on the Slack prompt afterwards sends nothing, and the thread says it was answered elsewhere")
	paused, err := s.pause(t, signInTestPrompt)
	if err != nil {
		return err
	}
	status, raw, err := s.portalAnswer(paused, paused.asked())
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return fmt.Errorf("the portal's answer to task %s answered %d: %.300s", paused.task.ID, status, raw)
	}
	if err := s.awaitLeft(paused); err != nil {
		return err
	}
	if err := s.slack.driver.click(t.user, paused.prompt, slackActionSignInDecline); err != nil {
		return err
	}
	turn, err := s.slack.awaitTurn(t, paused.before)
	if err != nil {
		return err
	}
	if turn.record.Outcome != outcomePromptAnswered {
		return fmt.Errorf("the late Not now on Slack ended as %s, not %s: %s", turn.record.Outcome, outcomePromptAnswered, turnFailure(turn))
	}
	if _, ok := findMessage(turn.msgs, gatewayAnsweredElsewhere); !ok {
		return fmt.Errorf("the thread does not say %q: %s", gatewayAnsweredElsewhere, threadLine(turn.msgs))
	}
	if err := s.followUp(t); err != nil {
		return fmt.Errorf("the turn after the late Not now: %w", err)
	}
	s.pass("the portal declined task %s first (%d); Slack's late Not now sent nothing (%s), the thread said %q, and the next turn completed", paused.task.ID, status, outcomePromptAnswered, gatewayAnsweredElsewhere)
	return nil
}

func (s *signInSurfaces) slackFirst(t *slackThread) error {
	step("Slack first: Not now declines, the agent asks again in the same task; the portal's late answer to the first request is refused (409) and the task still waits on the second")
	first, err := s.pause(t, klausGatewaySignInRetryPrompt)
	if err != nil {
		return err
	}
	if err := s.slack.driver.click(t.user, first.prompt, slackActionSignInDecline); err != nil {
		return err
	}
	turn, err := s.resumed(t, first)
	if err != nil {
		return err
	}
	if turn.record.Outcome != outcomeInputReq || turn.record.TaskID != string(first.task.ID) {
		return fmt.Errorf("after Not now the agent did not ask again in task %s (the model's doing, the prompt asks it to retry once): %s", first.task.ID, turnFailure(turn))
	}
	second, err := s.paused(t, turn, first.before)
	if err != nil {
		return err
	}
	if second.asked() == first.asked() {
		return fmt.Errorf("the second sign-in of task %s asks in the same status message %s as the first: the portal cannot tell the two apart", second.task.ID, second.asked())
	}
	status, raw, err := s.portalAnswer(first, first.asked())
	if err != nil {
		return err
	}
	if status != http.StatusConflict || !strings.Contains(string(raw), portalAnsweredByAnother) {
		return fmt.Errorf("the portal's late answer to the first request of task %s answered %d, not a 409 saying %q (giantswarm/backstage#3331): %.300s", first.task.ID, status, portalAnsweredByAnother, raw)
	}
	task, err := s.task(second.session, string(second.task.ID))
	if err != nil {
		return err
	}
	if task.Status.State != a2a.TaskStateInputRequired || task.Status.Message == nil || task.Status.Message.ID != second.asked() {
		return fmt.Errorf("after the refused answer task %s is %s, not still waiting on status message %s", task.ID, task.Status.State, second.asked())
	}
	if err := s.slack.driver.click(t.user, second.prompt, slackActionSignInDecline); err != nil {
		return err
	}
	done, err := s.slack.awaitTurn(t, second.before)
	if err != nil {
		return err
	}
	if done.record.Outcome != outcomeCompleted {
		return fmt.Errorf("after the second Not now: %s", turnFailure(done))
	}
	s.pass("Slack declined task %s first; it asked again in status message %s; the portal's late answer naming the first (%s) was refused with 409 and the task kept waiting on the second, which Slack declined", first.task.ID, second.asked(), first.asked())
	return nil
}

func (s *signInSurfaces) accepted(t *slackThread) error {
	step("Accepted: the click on Sign in is the consent and resumes the same task with accept before the person signed in; the runtime's retry parks in muster's sign-in wait for %s, the sign-in then completes through Dex and muster on muster's own page, and the call runs once in the same task", s.hold)
	paused, err := s.pause(t, signInTestPrompt)
	if err != nil {
		return err
	}
	decided := time.Now()
	if err := s.slack.driver.click(t.user, paused.prompt, slackActionConnect); err != nil {
		return err
	}
	if !waitFor(int(klausGatewayReplyWait/slackPoll), slackPoll, func() bool { return responded(s.slack.fake, gatewayContinuing) }) {
		return fmt.Errorf("the clicked prompt was not rewritten to %q within %s", gatewayContinuing, klausGatewayReplyWait)
	}
	parked, err := s.awaitParked(paused)
	if err != nil {
		return err
	}
	if err := s.holdParked(paused, s.hold); err != nil {
		return err
	}
	if err := completeSignIn(paused.link, s.user); err != nil {
		return err
	}
	turn, err := s.resumed(t, paused)
	if err != nil {
		return err
	}
	if turn.record.Outcome != outcomeCompleted {
		return fmt.Errorf("after the sign-in the turn ended %s", turnFailure(turn))
	}
	if turn.record.TaskID != string(paused.task.ID) {
		return fmt.Errorf("after the sign-in the call ran in task %s, not the paused task %s", turn.record.TaskID, paused.task.ID)
	}
	if !callAnswered(turn.answer) {
		return fmt.Errorf("after the sign-in the answer does not carry the fixture's tools (%s): %s", signInTestToolProof, excerpt(turn.answer, 300))
	}
	if ran, err := s.fixtureCallsSince(decided); err != nil {
		return err
	} else if len(ran) != 1 {
		return fmt.Errorf("after the sign-in muster ran %s %d times, want once: %s", signInTestTool, len(ran), strings.Join(ran, ", "))
	}
	if _, ok := findMessage(turn.msgs, "<@"+t.user+">"+gatewaySignedInLine); !ok {
		return fmt.Errorf("the thread does not say %q: %s", "<@"+t.user+">"+gatewaySignedInLine, threadLine(turn.msgs))
	}
	s.pass("the click on Sign in resumed task %s with accept before the sign-in; the task stayed %s on its retry for %s, muster holding the call until the sign-in; the sign-in from the link, unchanged, ended on muster's page; the call ran once in the same task and the answer carried the fixture's tools", paused.task.ID, parked, s.hold)
	s.signOut()
	restartOAuthFixture()
	return nil
}

// portalAnswer declines the paused task through the portal's answer route,
// naming the status message the answer is for.
func (s *signInSurfaces) portalAnswer(paused *signInPause, askedIn string) (int, []byte, error) {
	body := portalAgentRef{Namespace: kagentNamespace, Name: klausGatewaySignInAgent}.body()
	body["messageId"] = uuid.NewString()
	body["taskId"] = string(paused.task.ID)
	body["decision"] = hitlDecisionReject
	body["text"] = "Not now."
	body["askedIn"] = askedIn
	return s.portal.kagentRequest(http.MethodPost, kagentSessionsPath+"/"+paused.session.GetId()+"/answer", body)
}

// awaitLeft waits for the paused task to leave the request it asked in.
func (s *signInSurfaces) awaitLeft(paused *signInPause) error {
	var last *a2a.Task
	if waitFor(int(klausGatewayTurnTimeout/slackPoll), slackPoll, func() bool {
		task, err := s.task(paused.session, string(paused.task.ID))
		if err != nil {
			return false
		}
		last = task
		return task.Status.State != a2a.TaskStateInputRequired || task.Status.Message == nil || task.Status.Message.ID != paused.asked()
	}) {
		return nil
	}
	state := a2a.TaskState("unread")
	if last != nil {
		state = last.Status.State
	}
	return fmt.Errorf("task %s still waits on status message %s %s after the portal's answer (%s)", paused.task.ID, paused.asked(), klausGatewayTurnTimeout, state)
}

// awaitParked waits for the accepted task to leave input-required on the
// request it asked in: the runtime's retry is in flight, parked in muster's
// sign-in wait. It returns the state the task waits in.
func (s *signInSurfaces) awaitParked(paused *signInPause) (a2a.TaskState, error) {
	var state a2a.TaskState
	var judged error
	if waitFor(int(klausGatewayTurnTimeout/slackPoll), slackPoll, func() bool {
		task, err := s.task(paused.session, string(paused.task.ID))
		if err != nil {
			return false
		}
		state = task.Status.State
		parked, err := retryParked(task, paused.asked())
		judged = err
		return parked || err != nil
	}) && judged == nil {
		return state, nil
	}
	if judged != nil {
		return "", fmt.Errorf("after the click on Sign in, before the sign-in: %w", judged)
	}
	return "", fmt.Errorf("task %s still waits on status message %s %s after the click on Sign in (%s): the gateway did not resume it", paused.task.ID, paused.asked(), klausGatewayTurnTimeout, state)
}

// holdParked keeps the sign-in back for hold and fails as soon as the task
// stops waiting: a runtime that does not wait in muster's sign-in wait ends
// the turn or asks again before the sign-in.
func (s *signInSurfaces) holdParked(paused *signInPause, hold time.Duration) error {
	note("task %s parked; holding the sign-in back for %s", paused.task.ID, hold)
	for end := time.Now().Add(hold); time.Now().Before(end); time.Sleep(slackPoll) {
		task, err := s.task(paused.session, string(paused.task.ID))
		if err != nil {
			continue
		}
		if _, err := retryParked(task, paused.asked()); err != nil {
			return fmt.Errorf("while the sign-in was held back: %w", err)
		}
	}
	return nil
}

// retryParked judges a task the person accepted the sign-in of, before the
// sign-in completes: false while it still waits on the request it asked in
// (askedIn), true while it works on the retry, an error once it ended or
// asked again.
func retryParked(task *a2a.Task, askedIn string) (bool, error) {
	switch state := task.Status.State; state {
	case a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired:
		if task.Status.Message != nil && task.Status.Message.ID == askedIn {
			return false, nil
		}
		return false, fmt.Errorf("task %s asks again (%s) instead of its retry waiting in muster's sign-in wait", task.ID, state)
	case a2a.TaskStateSubmitted, a2a.TaskStateWorking:
		return true, nil
	default:
		return false, fmt.Errorf("task %s ended %s before the sign-in: its retry did not wait in muster's sign-in wait", task.ID, state)
	}
}

func (s *signInSurfaces) fixtureCallsSince(since time.Time) ([]string, error) {
	logs, err := musterLogSince(since)
	if err != nil {
		return nil, err
	}
	return fixtureCallsIn(logs, since, s.subject), nil
}

// signOut forgets the person's grant for the fixture, so the next turn
// meets the sign-in.
func (s *signInSurfaces) signOut() {
	if err := signOutOfFixture(s.cfg, s.token); err != nil {
		note("signing out of %s: %v", oauthFixtureServer, err)
	}
}

func (s *signInSurfaces) pass(format string, a ...any) {
	s.verdicts = append(s.verdicts, "PASS: "+s.harness+" — "+fmt.Sprintf(format, a...))
}

// cleanup removes the fixture's Sessions and objects and leaves the person
// signed out of the fixture. Best effort.
func (s *signInSurfaces) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	if sessions, err := sessionsOf(ctx, s.api, klausGatewaySignInAgent); err == nil {
		for _, session := range sessions {
			s.api.removeSession(session.GetId())
		}
	}
	cancel()
	if agentExists(klausGatewaySignInAgent) {
		if err := removeAgent(klausGatewaySignInAgent); err != nil {
			note("cleanup: %v", err)
		}
	}
	if s.token != "" {
		s.signOut()
	}
}

// signInPromptIn is the last sign-in prompt to user after the first
// `before` messages: an ephemeral with the Sign in and Not now buttons.
func signInPromptIn(msgs []slackMessage, before int, user string) (slackMessage, bool) {
	for i := len(msgs) - 1; i >= before && i >= 0; i-- {
		m := msgs[i]
		if m.Method != slackPostEphemeral || m.Recipient != user || m.Deleted {
			continue
		}
		if _, ok := m.action(slackActionConnect); !ok {
			continue
		}
		if _, ok := m.action(slackActionSignInDecline); ok {
			return m, true
		}
	}
	return slackMessage{}, false
}

// signInLinkShown is how the gateway's sign-in prompt shows an ASCII link
// before it is opened: the domain in bold, then the whole URL in code,
// escaped as Slack mrkdwn.
func signInLinkShown(link string) string {
	host := ""
	if u, err := url.Parse(link); err == nil {
		host = u.Hostname()
	}
	escape := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
	return "Opens *" + escape(host) + "*\n`" + escape(link) + "`"
}

// responded reports whether a prompt was rewritten through its response_url
// to text carrying needle.
func responded(fake slackWorkspace, needle string) bool {
	for _, body := range fake.responses() {
		if strings.Contains(body, needle) {
			return true
		}
	}
	return false
}
