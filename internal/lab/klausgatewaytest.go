package lab

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The Swarmgeist proof: klaus-gateway on kagent API v2, headless, through
// its Slack adapter.
//
// klaus-gateway (Swarmgeist, the fleet's Slack bridge) runs its
// conversations on the kagent controller: A2A v1 over gRPC through the
// agentgateway edge, the roster from ListAgents, one Session
// per Slack thread kept in the gateway's routing store, human-in-the-loop as
// kagent's HITL extension, a stop as CancelTask — every call made as the
// person behind the turn, whose Dex id_token the gateway forwards and
// validates nowhere itself. Slack is its only channel, and no workspace
// answers here, so the proof stands in for Slack on both sides (slackfake.go):
// the gateway's Web API is a fake that records what the thread would show,
// and the people's messages and button clicks are Events API callbacks and
// Block Kit payloads signed with the run's signing secret.
//
// The gateway runs out of cluster, on the host — the released image or a
// local build — its a2a target the lab's public gRPC hostname with JWT
// validation at the edge. The person behind the turns is linked the way the
// gateway links anyone: a record in its OBO link store, written through the
// gateway's own store package, whose cached id_token is the lab user's Dex
// id_token. The Slack channel forwards only a linked person's token (there is
// no service-account fallback for it), and the link's cached token is what a
// linked person's turn forwards until its refresh is due, so the gateway runs
// unmodified; a real sign-in cannot complete in the lab (docs/klaus-gateway.md).
// The proof: `@bot agents` lists the proof's AgentTemplate and hides one no
// Harness admits, which the agent picker its Select button opens hides too, and
// a selection of it is refused with the reason; a person with no
// link is asked to sign in and reaches no controller; a turn streams under the
// template's display name and icon, attributed to the person at muster; a tool
// call bound with requireApproval pauses the task at input-required, Approve
// resumes it in place and Deny ends another without the call; `stop` cancels
// the task at the controller and the thread goes on; a gateway restart on
// the same stores continues the same Session; and a restart in the
// middle of a turn, the edge out of reach for a while after it, still posts
// the answer in its thread. docs/platform.md "The
// Swarmgeist proof".

// KlausGatewayImageDefault is the gateway the proof runs when no image or
// binary is named: the current release candidate of the 4.1 line, the first
// that serves the command words alone (`agents`, `stop`) and keeps a parked
// message and a paused approval across a restart (steps 5c and 5d).
const KlausGatewayImageDefault = "gsoci.azurecr.io/giantswarm/klaus-gateway:4.1.0-rc.15"

// Names of what the proof creates in the kagent namespace; all are deleted by
// the same run, and a leftover of an aborted run is removed first.
const (
	klausGatewayTestAgent = "agentlab-klaus-gateway-test"
	// klausGatewayTestUnadmitted is the Agent no Harness runs: its harnessRef
	// names a Harness that does not exist, so the roster must hide it and a
	// selection of it must be refused with the reason.
	klausGatewayTestUnadmitted        = klausGatewayTestAgent + "-unadmitted"
	klausGatewayTestDisplay           = "agentlab Swarmgeist proof"
	klausGatewayTestUnadmittedDisplay = "agentlab unadmitted template"
	klausGatewayTestIcon              = "https://icons.agentlab.invalid/swarmgeist.png"
	// klausGatewayTestToolset is the toolset the fixture's muster carrier
	// declares (X-Muster-Toolset), the fleet's read-only preset.
	klausGatewayTestToolset = "preset:read-only"
	klausGatewayTestPrompt  = "You are a terse assistant of the agentlab Swarmgeist proof. Answer in one short line. " +
		"When the user asks you to reply with a word, reply with exactly that word and nothing else. " +
		"When the user asks about namespaces of the cluster, use your tools to list them once and answer with their count."
	// klausGatewayMusterURL is muster's in-cluster MCP endpoint, the
	// RemoteMCPServer's url exactly as the Generic chart 1.x renders it.
	klausGatewayMusterURL = "http://muster.agent-platform.svc.cluster.local:8090/mcp"
)

// The gateway on the host: its files in the run directory, the container's
// mount paths, and the records of its log the proof reads.
const (
	// gatewayCAPath is where the image shape mounts the lab CA (the chart's
	// own mount path for a2a.caSecret).
	gatewayCAPath         = "/etc/klaus-gateway/a2a/ca.crt"
	gatewayDataPath       = "/var/lib/klaus-gateway"
	gatewayKubeconfigPath = "/etc/klaus-gateway/kubeconfig"
	gatewayBoltFile       = "routes.bolt"
	gatewayLinksFile      = "links.bolt"
	gatewayLogFile        = "klaus-gateway.log"
	gatewaySecrets        = "slack-secrets.yaml"
	gatewayStateKey       = "obo-state.key"
	gatewayStoreKey       = "obo-store.key"
	gatewayReadyPath      = "/readyz"
	gatewayContainer      = "agentlab-klaus-gateway-test"
	gatewayLogLevel       = "info"
	recordBound           = "session_bound"
	recordTurnDone        = "turn_complete"
	recordDispatch        = "turn_dispatch"
	recordRefresh         = "token_refresh"
	recordTurnResume      = "turn_resume"
	recordLeftRunning     = "task_left_running"
	// gatewayResubscribeFailed is the warning of a recovery attempt whose
	// resubscription did not reach the controller.
	gatewayResubscribeFailed = "resubscribe to a turn left running failed"
	outcomeCompleted         = "completed"
	outcomeInputReq          = "input_required"
	outcomeCanceled          = "canceled"
	linkRefreshMarker        = "agentlab-placeholder-refresh-token"
)

// The turns and their words: the first turn's word, recalled after the
// restart; the tool-using question that pauses on approval; the long answer
// `stop` interrupts.
const (
	klausGatewayWord         = "pong"
	klausGatewayWordPrompt   = "Reply with exactly the word " + klausGatewayWord + "."
	klausGatewayRecallPrompt = "Which single word did I ask you to reply with earlier in this conversation? Answer with just that word."
	klausGatewayToolPrompt   = "How many namespaces does the cluster have? Use your tools to list them."
	klausGatewayEssayPrompt  = "Write a long essay of at least 1500 words about the history of container orchestration, without using any tools."
	// klausGatewayCountPrompt is step 5b's long turn: plain text that streams
	// from its first line and runs for minutes, on a small local model too.
	klausGatewayCountPrompt = "Without using any tools, write out every number from one to five hundred in English words, one number per line, and nothing else."
	klausGatewayTurnTimeout = 4 * time.Minute
	klausGatewayReplyWait   = 90 * time.Second
	klausGatewayCancelWait  = 90 * time.Second
	klausGatewayStartWait   = 30 * time.Second
	klausGatewayStopWait    = 20 * time.Second
	// klausGatewayGateClosed is how long the controller stays out of the
	// restarted gateway's reach in step 5b: longer than a few quick retries,
	// so only a recovery that keeps trying delivers the turn.
	klausGatewayGateClosed    = 45 * time.Second
	klausGatewayHITLRounds    = 6
	klausGatewayLogSince      = 30 * time.Minute
	klausGatewayReadyTimeout  = 10 * time.Minute
	klausGatewayMusterAttempt = 15
	// klausGatewayTokenBudget is how much lifetime the lab user's id_token
	// must have left when the run starts: the run's length and then some,
	// plus the five minutes before expiry at which the gateway's refresher
	// would spend the link's (placeholder) refresh token at muster.
	klausGatewayTokenBudget = time.Hour
)

// The id_token claims the person's link carries: the Dex subject and the
// expiry.
const (
	claimSubject = "sub"
	claimExpiry  = "exp"
)

// Muster's audit and protocol records the attribution reads.
const (
	musterTokenAccepted = "forwarded_id_token_accepted"
	musterToolCall      = `"msg":"tools/call request"`
)

// KlausGatewayTestOptions tunes the proof.
type KlausGatewayTestOptions struct {
	// GatewayImage is the klaus-gateway image run on the host's network
	// (default KlausGatewayImageDefault); GatewayBinary, a local build, takes
	// precedence — the proof of a branch.
	GatewayImage  string
	GatewayBinary string
	// Port is the host port of the gateway's Slack endpoints; the admin
	// endpoints take Port+1, the fake Slack Web API Port+2 when it runs in
	// this process. Default 18090.
	Port int
	// SlackFakeBinary is the static Linux agentlab the fake Slack Web API
	// container runs when the component's pods call it (default: this
	// binary).
	SlackFakeBinary string
	// ModelConfig is the kagent ModelConfig the fixture runs on (default
	// default-model-config, the Anthropic one the lab renders).
	ModelConfig string
	// Harness is the Harness the fixture's Agent references and whose Ready
	// the golden-boot wait reads (default the platform Harness).
	Harness string
	// ReadyTimeout bounds the fixture's golden boot (default 10 min).
	ReadyTimeout time.Duration
	// RunDir holds the stores, the keys and the gateway's log; empty picks
	// a temporary directory removed at the end.
	RunDir string
}

// KlausGatewayTest is the headless Swarmgeist proof: klaus-gateway against
// the lab's kagent API v2 controller through the edge, driven through its
// Slack adapter as the signed-in user.
func KlausGatewayTest(cfg *config.Config, email string, opts KlausGatewayTestOptions) error {
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
	opts = opts.withDefaults()
	if err := portsFree(opts.Port, opts.Port+1, opts.Port+3); err != nil {
		return err
	}
	runDir, cleanRunDir, err := klausGatewayRunDir(opts.RunDir)
	if err != nil {
		return err
	}
	defer cleanRunDir()

	step("Logging in to Dex as %s", user.Email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterLoginScopes)
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

	// Leftovers of an aborted run first, and everything this run creates on
	// every exit path.
	klausGatewayCleanup(api)
	defer klausGatewayCleanup(api)

	// The fake workspace: the linked person, a person with no link, and the
	// component half's linked person (klausgatewaytest_component.go), all
	// answered by users.info. With the component on, its pods call the fake
	// too: it runs on the kind network then, and a pod's fetch through the
	// component's Service proves the path before anything else runs.
	run := strings.ToUpper(randomSuffix())
	people := slackPeople{
		person:    slackUserPrefix + run + "P",
		stranger:  slackUserPrefix + run + "S",
		component: slackUserPrefix + run + "C",
	}
	emails := map[string]string{people.person: user.Email, people.stranger: "stranger@lab.local", people.component: user.Email}
	var fake slackWorkspace
	if cfg.KlausGatewayEnabled() {
		step("Starting the fake Slack Web API as a container on the %s network (%s slack-fake, in %s) and pointing the component's Service %s/%s at it",
			kindDockerNetwork, opts.SlackFakeBinary, probeImage, platformNamespace, klausGatewaySlackAPIService)
		c, err := startSlackFakeContainer(cfg, opts.SlackFakeBinary, emails)
		if err != nil {
			return err
		}
		defer c.close()
		removeService, err := fakeSlackForPods(cfg, c.podIP, fakeContainerPort)
		if err != nil {
			return err
		}
		defer removeService()
		fake = c
	} else {
		f, err := startFakeSlack(net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port+2)), emails)
		if err != nil {
			return err
		}
		defer f.close()
		fake = f
	}

	step("Applying the fixtures in namespace %s: Agent %s on Harness %s (muster carrier %s with %s, requireApproval on the binding) and Agent %s on a Harness that does not exist (%s)",
		kagentNamespace, klausGatewayTestAgent, opts.Harness, klausGatewayTestAgent, klausGatewayTestToolset, klausGatewayTestUnadmitted, klausGatewayNoHarness)
	if _, err := applyManifests(context.Background(), []byte(klausGatewayFixtures(opts.ModelConfig, opts.Harness))); err != nil {
		return err
	}
	step("Waiting up to %s for %s Ready on Harness %s (the golden boot)", opts.ReadyTimeout, klausGatewayTestAgent, opts.Harness)
	boot, err := waitAgentReadyOn(klausGatewayTestAgent, opts.Harness, opts.ReadyTimeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return boot.failure(klausGatewayTestAgent, opts.ReadyTimeout)
	}
	note("Ready after %s", boot.elapsed.Round(time.Second))

	keys, err := writeGatewayFiles(runDir)
	if err != nil {
		return err
	}
	step("Linking Slack user %s to %s: a record in the gateway's bolt link store (%s), written through pkg/auth/musterlink with the run's store key — the Dex subject, the e-mail, the id_token as the link's cached token (expires %s, %s from now)",
		people.person, user.Email, filepath.Join(runDir, gatewayLinksFile), identity.expiry.UTC().Format(time.RFC3339), time.Until(identity.expiry).Round(time.Minute))
	if err := seedBoltLink(filepath.Join(runDir, gatewayLinksFile), keys.store, people.person, identity.link(user.Email, token)); err != nil {
		return err
	}

	edge, err := edgeHostPort(cfg.AgentgatewayBaseURL())
	if err != nil {
		return err
	}
	gate, err := startTCPGate(net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port+3)), edge)
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
	defer func() { _ = gw.stop() }()
	step("Starting klaus-gateway %s on the host: Slack in events mode at %s, its Web API the fake at %s, a2a target %s (TLS with the lab CA, the person's token forwarded; a loopback gate in front of the edge %s), OBO with the bolt link store, routing store %s",
		gw.describe(), gw.baseURL(), fake.baseURL(), target, edge, filepath.Join(runDir, gatewayBoltFile))
	if err := gw.start(); err != nil {
		return err
	}
	note("ready: %s", gw.version())
	channel := "CAGENTLAB" + run
	p := &slackProof{
		driver: newSlackDriver(gw.baseURL(), keys.signing, fake, channel),
		fake:   fake, channel: channel,
		logs: func() (string, error) { return gw.logs(), nil },
	}

	step("1. Discovery: `@bot %s` as %s lists %q and hides %s; its Select button opens the picker, which hides it too, and a submitted selection of it is refused with the reason and starts nothing; %s, who has no link, is asked to sign in and reaches no controller",
		slackAgentsWord, user.Email, klausGatewayTestDisplay, klausGatewayTestUnadmitted, people.stranger)
	roster, err := p.roster(people.person)
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	names := rosterNames(roster.shown())
	if err := assertSlackRoster(names); err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	refusal, err := p.refusal(people.person, roster, klausGatewayWordPrompt)
	if err != nil {
		return err
	}
	if err := assertNoSessions(api, klausGatewayTestUnadmitted); err != nil {
		return err
	}
	signIn, err := p.signInPrompt(people.stranger, klausGatewayWordPrompt)
	if err != nil {
		return err
	}
	if err := assertNoSessions(api, klausGatewayTestAgent); err != nil {
		return fmt.Errorf("after the unlinked person's message: %w", err)
	}
	note("roster: %s; %s refused: %q; %s asked to sign in (%s), no Session of either Agent",
		strings.Join(names, ", "), klausGatewayTestUnadmitted, excerpt(refusal, 160), people.stranger, excerpt(signIn, 80))

	step("2. One streamed turn as %s: the thread's first turn creates the Session, the answer carries the Agent's display name and icon", user.Email)
	main := &slackThread{user: people.person}
	turn, err := p.firstTurn(main, klausGatewayWordPrompt)
	if err != nil {
		return err
	}
	if err := assertSlackTurnSaid(turn, klausGatewayWord); err != nil {
		return err
	}
	if err := assertBranded(turn); err != nil {
		return err
	}
	session, err := p.boundSession(api, main)
	if err != nil {
		return err
	}
	sessionID := session.GetId()
	if err := assertOnlySessions(api, sessionID); err != nil {
		return err
	}
	dispatch, err := p.dispatch(main, identity.subject, user.Email)
	if err != nil {
		return err
	}
	note("answered %q as %q (icon %s); thread bound to Session %s, the only session of the Agent; turn_dispatch: agent %s (%s), subject %s, sub %.8s…",
		excerpt(turn.answer, 60), turn.stream.Username, turn.stream.IconURL, sessionID, dispatch.Agent, dispatch.AgentSource, dispatch.Subject, dispatch.Sub)

	step("3. Human in the loop: a tool call pauses the task at input-required; Approve on the approval card resumes it in place")
	toolTurnStart := time.Now()
	turn, err = p.say(main, klausGatewayToolPrompt)
	if err != nil {
		return fmt.Errorf("the tool-using turn: %w", err)
	}
	approved, err := p.decideUntilSettled(api, session, main, turn, true)
	if err != nil {
		return err
	}
	if approved.finalState != a2a.TaskStateCompleted {
		return fmt.Errorf("the approved task %s ended %s at the controller, not completed", approved.taskID, approved.finalState)
	}
	if err := assertNothingWaiting(api, session); err != nil {
		return err
	}
	note("%d approval(s) on task %s (%s); GetTask=%s; nothing of the session left at input-required; answered %q",
		approved.rounds, approved.taskID, strings.Join(approved.cards, "; "), approved.finalState, excerpt(approved.answer, 80))

	step("The turn is attributed to %s at muster (the forwarded id_token accepted, the tool calls under that subject)", user.Email)
	if err := assertMusterAttribution(user.Email, identity.subject, toolTurnStart); err != nil {
		return err
	}
	note("muster's audit log: %s email=%s; tools/call requests under subject %.8s…", musterTokenAccepted, user.Email, identity.subject)

	step("3b. Deny in a second thread: the tool call never reaches muster and the task ends")
	denied := &slackThread{user: people.person}
	declineStart := time.Now()
	turn, err = p.firstTurn(denied, klausGatewayToolPrompt)
	if err != nil {
		return fmt.Errorf("the tool-using turn to deny: %w", err)
	}
	deniedSession, err := p.boundSession(api, denied)
	if err != nil {
		return err
	}
	declined, err := p.decideUntilSettled(api, deniedSession, denied, turn, false)
	if err != nil {
		return err
	}
	if !declined.finalState.Terminal() {
		return fmt.Errorf("after %d denial(s) task %s is still %s at the controller", declined.rounds, declined.taskID, declined.finalState)
	}
	calls, err := musterCallsSince(declineStart, user.Email, identity.subject)
	if err != nil {
		return err
	}
	if calls.calls != 0 {
		return fmt.Errorf("muster logged %d tools/call by %s during the denied task — the denied tool ran anyway", calls.calls, user.Email)
	}
	note("%d denial(s) on task %s → %s at the controller (turn outcome %s); no tools/call by %s reached muster; the thread shows %q",
		declined.rounds, declined.taskID, declined.finalState, declined.outcome, user.Email, excerpt(declined.answer, 80))

	step("4. `stop`: a long turn is stopped from the thread; the gateway cancels the task at the controller and the thread takes a following turn")
	tasksBefore, err := api.taskIDs(context.Background(), session)
	if err != nil {
		return err
	}
	stopped, err := p.stop(main, klausGatewayEssayPrompt)
	if err != nil {
		return err
	}
	canceled, err := api.waitCanceledTask(session, tasksBefore, klausGatewayCancelWait)
	if err != nil {
		return err
	}
	turn, err = p.say(main, klausGatewayWordPrompt)
	if err != nil {
		return fmt.Errorf("the turn after stop: %w", err)
	}
	if err := assertSlackTurnSaid(turn, klausGatewayWord); err != nil {
		return fmt.Errorf("the turn after stop: %w", err)
	}
	note("stopped after %d streamed characters with %q; task %s TASK_STATE_CANCELED at the controller; the following turn answered %q",
		len(stopped.answer), slackStopped, canceled, excerpt(turn.answer, 40))

	step("5. Restart on the stores: the thread → Session mapping survives, the next turn continues %s", sessionID)
	boundBefore := len(gatewayRecords(gw.logs(), recordBound))
	if err := gw.stop(); err != nil {
		return err
	}
	if err := gw.start(); err != nil {
		return fmt.Errorf("restarting the gateway: %w", err)
	}
	turn, err = p.say(main, klausGatewayRecallPrompt)
	if err != nil {
		return fmt.Errorf("the turn after the restart: %w", err)
	}
	if err := assertSlackTurnSaid(turn, klausGatewayWord); err != nil {
		return fmt.Errorf("the turn after the restart did not continue the conversation: %w", err)
	}
	if after := len(gatewayRecords(gw.logs(), recordBound)); after != boundBefore {
		return fmt.Errorf("the restarted gateway bound a thread anew (%d session_bound records before, %d after): the bolt store did not carry the mapping", boundBefore, after)
	}
	if err := assertOnlySessions(api, sessionID, deniedSession.GetId()); err != nil {
		return fmt.Errorf("after the restart: %w", err)
	}
	if refreshes := gatewayRecords(gw.logs(), recordRefresh); len(refreshes) > 0 {
		return fmt.Errorf("the gateway refreshed a link %d time(s) (trigger %s): every turn must have forwarded the seeded id_token, and the refresh leg is out of the lab's reach", len(refreshes), refreshes[0].Trigger)
	}
	note("no new binding, still Session %s next to the denied thread's %s; the agent recalled %q; no token_refresh in the run", sessionID, deniedSession.GetId(), excerpt(turn.answer, 40))

	step("5b. Restart mid-turn with the controller out of reach for %s: the turn left running is posted in its thread when it ends, without a reply", klausGatewayGateClosed)
	resumed, err := p.restartMidTurn(gw, gate, main, klausGatewayCountPrompt)
	if err != nil {
		return err
	}
	note("the restart notice promised the post; %d recovery attempt(s) failed while the gate was closed; task %s posted %d characters into the thread %s after the restart, no reply sent",
		resumed.failedAttempts, resumed.taskID, len(resumed.answer), resumed.after.Round(time.Second))

	var signedIn *slackTurn
	var approvedAfter *decisionOutcome
	if gatewayKeepsHeld(gw.version()) {
		step("5c. Restart mid-sign-in: %s, who has no link, asks in a new thread and is prompted to sign in; the gateway stops, the link a completed sign-in leaves is written, the gateway starts — the parked message is answered without being sent again", people.stranger)
		if signedIn, err = p.restartMidSignIn(gw, filepath.Join(runDir, gatewayLinksFile), keys.store, people.stranger, identity.link(user.Email, token)); err != nil {
			return err
		}
		note("the restarted gateway replayed the message parked before the restart: answered %q", excerpt(signedIn.answer, 40))

		step("5d. Restart mid-approval: a tool call pauses a new thread's task on the approval card; after a restart Approve on that card resumes the task in place")
		if approvedAfter, err = p.restartMidApproval(api, gw, people.person); err != nil {
			return err
		}
		note("%d approval(s) on task %s after the restart; GetTask=%s; answered %q",
			approvedAfter.rounds, approvedAfter.taskID, approvedAfter.finalState, excerpt(approvedAfter.answer, 80))
	} else {
		note("%s predates klaus-gateway %s, which keeps a parked message and a paused approval across a restart: steps 5c and 5d skipped", gw.version(), klausGatewayHeldSince)
	}

	// The component half (klausgatewaytest_component.go) while the meta
	// chart's klaus-gateway runs in this lab: its fixtures are the ones
	// above, its Slack Web API the same fake, and the session its turn
	// creates is removed by the cleanup.
	var component *componentOutcome
	if cfg.KlausGatewayEnabled() {
		if component, err = klausGatewayComponentProof(token, identity, user, fake, people.component); err != nil {
			return err
		}
	} else {
		note("the meta chart's klaus-gateway component is off in this lab (platform.klausGateway; `agentlab configure --klaus-gateway` turns it on): the host-mode assertions alone")
	}

	step("Deleting the fixtures, the Sessions and the gateway")
	if err := gw.stop(); err != nil {
		return err
	}
	klausGatewayCleanup(api)
	if left := klausGatewayLeftovers(api); len(left) > 0 {
		return fmt.Errorf("left behind: %s", strings.Join(left, "; "))
	}
	runDirFate := "removed"
	if opts.RunDir != "" {
		runDirFate = "kept at " + runDir
	}
	note("nothing left in the kagent namespace; gateway stopped, run directory %s", runDirFate)

	fmt.Println()
	fmt.Printf("PASS: klaus-gateway %s ran on the host against %s (TLS with the lab CA, JWT validated at the edge) with Slack in events mode on a fake Web API; `@bot agents` as %s listed %q and hid %s, as did the picker its Select button opened, and a selection of it was refused with the reason; a person with no link was asked to sign in and reached no controller\n",
		gw.describe(), target, user.Email, klausGatewayTestDisplay, klausGatewayTestUnadmitted)
	fmt.Printf("PASS: one streamed turn in a Slack thread, answered as %q with the Agent's icon, bound the thread to Session %s, the only session of the Agent; the gateway forwarded %s's linked id_token and muster ran the agent's tool calls under that subject\n", klausGatewayTestDisplay, sessionID, user.Email)
	fmt.Printf("PASS: the requireApproval binding paused task %s at input-required; %d Approve click(s) on the card resumed it in place to %s; in a second thread %d Deny click(s) ended task %s (%s) without the call reaching muster\n",
		approved.taskID, approved.rounds, approved.finalState, declined.rounds, declined.taskID, declined.finalState)
	fmt.Printf("PASS: `stop` in the thread had the gateway cancel task %s at the controller (TASK_STATE_CANCELED); the thread took a following turn\n", canceled)
	fmt.Printf("PASS: a gateway restart on the bolt stores kept the thread → Session mapping: the next turn continued %s and recalled the earlier word; no link refresh in the run\n", sessionID)
	fmt.Printf("PASS: a restart in the middle of a turn, the controller out of reach for %s after it: the restarted gateway kept retrying and posted task %s's answer into its thread %s later, without a reply\n", klausGatewayGateClosed, resumed.taskID, resumed.after.Round(time.Second))
	if signedIn != nil {
		fmt.Printf("PASS: a restart between the sign-in prompt and the sign-in lost nothing: the restarted gateway replayed %s's parked message, answered %q\n", people.stranger, excerpt(signedIn.answer, 40))
		fmt.Printf("PASS: a restart while task %s waited on its approval card lost nothing: %d Approve click(s) after the restart resumed it in place to %s\n", approvedAfter.taskID, approvedAfter.rounds, approvedAfter.finalState)
	}
	if component != nil {
		fmt.Printf("PASS: the meta chart's %s component runs the OBO link store in Secret %s — Role %s grants get/update/patch on that Secret alone, no store volume, RollingUpdate\n", klausGatewayComponent, klausGatewayLinksSecret, klausGatewayLinksSecret)
		fmt.Printf("PASS: two links written through pkg/auth/musterlink with the lab's store-key survived the loss of pod %s: %s was Ready %s after the deletion and read %d links (%d before the proof), both read back unchanged, the proof's records removed\n",
			component.pod, component.replacement, component.elapsed, component.links, component.baseline)
		fmt.Printf("PASS: one Slack turn through the component (%s, the in-cluster target %s) as %s answered %q in the fake thread\n", component.version, klausGatewayInClusterTarget, user.Email, excerpt(component.answer, 40))
	}
	fmt.Printf("PASS: nothing left behind — the Agents, the AgentTemplates, the RemoteMCPServer, the Sessions, the gateway process and its stores are gone\n")
	return nil
}

// withDefaults fills the zero options.
func (o KlausGatewayTestOptions) withDefaults() KlausGatewayTestOptions {
	if o.GatewayImage == "" {
		o.GatewayImage = KlausGatewayImageDefault
	}
	if o.Port == 0 {
		o.Port = 18090
	}
	if o.Harness == "" {
		o.Harness = kagentHarness
	}
	if o.ModelConfig == "" {
		o.ModelConfig = defaultModelConfig
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = klausGatewayReadyTimeout
	}
	if o.SlackFakeBinary == "" {
		if exe, err := os.Executable(); err == nil {
			o.SlackFakeBinary = exe
		}
	}
	return o
}

// portsFree refuses to start on a port something else already listens on.
func portsFree(ports ...int) error {
	for _, port := range ports {
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return fmt.Errorf("host port %d is taken (pick another with --gateway-port): %w", port, err)
		}
		_ = l.Close()
	}
	return nil
}

// klausGatewayRunDir is where the stores, the keys and the gateway's log live
// for one run: the caller's directory (kept), or a temporary one (removed).
// The image shape's container runs as the caller's uid, so the directory
// needs no wider mode.
func klausGatewayRunDir(dir string) (string, func(), error) {
	if dir != "" {
		// Absolute, or `docker run -v` takes it for a named volume the
		// gateway's container cannot write.
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", nil, err
		}
		if err := os.MkdirAll(abs, 0o750); err != nil {
			return "", nil, err
		}
		return abs, func() {}, nil
	}
	tmp, err := os.MkdirTemp("", "agentlab-klaus-gateway-test-")
	if err != nil {
		return "", nil, err
	}
	return tmp, func() { _ = os.RemoveAll(tmp) }, nil
}

// edgeHostPort is the host:port of the edge's https base URL (the GRPCRoute
// shares the public listener), 443 when the URL names no port.
func edgeHostPort(httpsBase string) (string, error) {
	u, err := url.Parse(httpsBase)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("the edge URL %q: not an https URL with a host", httpsBase)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

// gatedTarget is the gateway's grpcs:// target through the gate on port: the
// edge's own hostname, so TLS verifies against the lab CA as usual — the
// lab's domain resolves to loopback, where the gate listens.
func gatedTarget(edge string, port int) string {
	host, _, _ := net.SplitHostPort(edge)
	return "grpcs://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// randomSuffix is a short per-run id.
func randomSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano()%1_000_000, 36)
}

// --- the person's link --------------------------------------------------------

// linkedIdentity is what the person's link carries of the lab user's Dex
// id_token: its subject and its expiry.
type linkedIdentity struct {
	subject string
	expiry  time.Time
}

// tokenIdentity reads the id_token's subject and expiry and refuses one
// that would not outlive the run: the gateway's refresher spends the link's
// refresh token at muster five minutes before the expiry, and the link's is
// a placeholder.
func tokenIdentity(token string, now time.Time) (linkedIdentity, error) {
	claims, err := decodeJWTClaims(token)
	if err != nil {
		return linkedIdentity{}, err
	}
	subject, _ := claims[claimSubject].(string)
	exp, _ := claims[claimExpiry].(float64)
	if subject == "" || exp == 0 {
		return linkedIdentity{}, fmt.Errorf("the token carries no sub or exp claim")
	}
	id := linkedIdentity{subject: subject, expiry: time.Unix(int64(exp), 0)}
	if left := id.expiry.Sub(now); left < klausGatewayTokenBudget {
		return linkedIdentity{}, fmt.Errorf("it expires in %s, the proof needs %s (Dex's idTokens expiry is too short for the run)", left.Round(time.Second), klausGatewayTokenBudget)
	}
	return id, nil
}

// link is the person's link record: the subject and e-mail as a sign-in
// would store them, the id_token cached with its expiry — what TokenFor
// serves without calling muster — and a refresh token that is plainly a
// placeholder.
func (id linkedIdentity) link(email, token string) *musterlink.Link {
	return &musterlink.Link{
		Sub: id.subject, Email: email, RefreshToken: linkRefreshMarker,
		LinkedAt: time.Now().UTC().Truncate(time.Second), IDToken: token, Expiry: id.expiry,
	}
}

// gatewayKeys are the run's secrets the proof needs back: the signing secret
// it signs the Slack requests with and the store key it seals the link with.
type gatewayKeys struct {
	signing string
	store   []byte
}

// writeGatewayFiles writes the gateway's secrets into the run directory: the
// Slack secrets file (a bot token only the fake reads, the signing secret)
// and the OBO state and store keys, all random, all for this run only.
func writeGatewayFiles(runDir string) (gatewayKeys, error) {
	keys := gatewayKeys{signing: randHex(32), store: []byte(randBase64(32))}
	files := map[string]string{
		gatewaySecrets:  fmt.Sprintf("bot_token: agentlab-fake-bot-%s\nsigning_secret: %s\n", randHex(8), keys.signing),
		gatewayStateKey: randHex(32),
		gatewayStoreKey: string(keys.store),
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := os.WriteFile(filepath.Join(runDir, name), []byte(files[name]), 0o600); err != nil {
			return gatewayKeys{}, err
		}
	}
	return keys, nil
}

// seedBoltLink writes one link into the gateway's bolt link store through the
// gateway's own package, sealed with the store key, before the gateway opens
// the file (bolt holds an exclusive lock while it runs).
func seedBoltLink(path string, key []byte, slackUser string, link *musterlink.Link) error {
	store, err := musterlink.OpenBoltStore(path, key, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return fmt.Errorf("opening the gateway's link store %s: %w", path, err)
	}
	if err := store.Put(slackUser, link); err != nil {
		_ = store.Close()
		return fmt.Errorf("writing the link of %s: %w", slackUser, err)
	}
	return store.Close()
}

// --- the fixtures -------------------------------------------------------------

// klausGatewayFixtures renders the proof's objects: the AgentTemplate with
// the display-name and icon annotations the Generic chart writes, its muster
// carrier (the per-agent RemoteMCPServer the chart renders: muster's
// in-cluster URL, X-Muster-Toolset, discovery off, never an Authorization
// header) bound with requireApproval so every muster call pauses for a
// decision, the Agent pairing the template with the named Harness — and a
// second template and Agent whose Harness does not exist, so no Harness
// runs it.
func klausGatewayFixtures(modelConfig, harness string) string {
	return fmt.Sprintf(`apiVersion: %[1]s
kind: RemoteMCPServer
metadata:
  name: %[2]s
  namespace: %[3]s
  labels:
    %[4]s: %[5]s
    kagent.dev/discovery: disabled
spec:
  description: "muster MCP gateway of agent %[2]s (toolset %[6]s); throwaway of agentlab klaus-gateway-test"
  url: %[7]s
  protocol: STREAMABLE_HTTP
  headersFrom:
    - name: X-Muster-Toolset
      value: %[6]q
---
apiVersion: %[1]s
kind: AgentTemplate
metadata:
  name: %[2]s
  namespace: %[3]s
  labels:
    %[4]s: %[5]s
  annotations:
    ui.giantswarm.io/display-name: %[8]q
    ui.giantswarm.io/icon-url: %[9]q
spec:
  description: "Throwaway agent of agentlab klaus-gateway-test (the Swarmgeist proof); deleted by the same run."
  modelConfig:
    name: %[10]s
  systemPrompt: %[11]q
  tools:
    - mcp:
        server:
          kind: %[12]s
          name: %[2]s
        requireApproval: true
---
%[13]s---
apiVersion: %[1]s
kind: AgentTemplate
metadata:
  name: %[14]s
  namespace: %[3]s
  labels:
    %[4]s: %[5]s
  annotations:
    ui.giantswarm.io/display-name: %[15]q
spec:
  description: "Throwaway agent of agentlab klaus-gateway-test on a Harness that does not exist; deleted by the same run."
  modelConfig:
    name: %[10]s
  systemPrompt: "Never runs."
---
%[16]s`, agentTemplateAPIVersion, klausGatewayTestAgent, kagentNamespace, managedByLabel, managedByAgentlabValue,
		klausGatewayTestToolset, klausGatewayMusterURL, klausGatewayTestDisplay, klausGatewayTestIcon,
		modelConfig, klausGatewayTestPrompt, remoteMCPServerKind,
		agentManifest(klausGatewayTestAgent, klausGatewayTestAgent, harness, map[string]string{displayNameAnnotation: klausGatewayTestDisplay, iconURLAnnotation: klausGatewayTestIcon}),
		klausGatewayTestUnadmitted, klausGatewayTestUnadmittedDisplay,
		agentManifest(klausGatewayTestUnadmitted, klausGatewayTestUnadmitted, klausGatewayNoHarness, map[string]string{displayNameAnnotation: klausGatewayTestUnadmittedDisplay}))
}

// klausGatewayNoHarness is the Harness the unadmitted fixture's Agent
// references: none of that name exists, so the Agent never becomes Ready.
const klausGatewayNoHarness = "agentlab-no-such-harness"

// klausGatewayCleanup removes what the proof creates: the Sessions of the
// fixture (the controller's, listed as the person), the Agents, the
// AgentTemplates and the RemoteMCPServer. Best effort; klausGatewayLeftovers
// reports what stayed.
func klausGatewayCleanup(api *kagentAPI) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if sessions, err := fixtureSessions(ctx, api); err == nil {
		for _, session := range sessions {
			api.removeSession(session.GetId())
		}
	}
	for _, obj := range klausGatewayObjects() {
		gvr, err := gvrFor(obj.resource)
		if err != nil {
			continue
		}
		if err := deleteObject(ctx, gvr, kagentNamespace, obj.name, substrateReleaseWait); err != nil {
			note("cleanup: %s %s: %v", obj.resource, obj.name, err)
		}
	}
}

// klausGatewayObjects are the namespaced objects the proof writes, in the
// order they are deleted.
func klausGatewayObjects() []struct{ resource, name string } {
	return []struct{ resource, name string }{
		{agentResource, klausGatewayTestAgent},
		{agentResource, klausGatewayTestUnadmitted},
		{agentTemplateResource, klausGatewayTestAgent},
		{agentTemplateResource, klausGatewayTestUnadmitted},
		{remoteMCPServerResource, klausGatewayTestAgent},
	}
}

// klausGatewayLeftovers lists what the cleanup did not remove.
func klausGatewayLeftovers(api *kagentAPI) []string {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	var left []string
	if sessions, err := fixtureSessions(ctx, api); err == nil && len(sessions) > 0 {
		left = append(left, fmt.Sprintf("%d Session(s) of %s: %s", len(sessions), klausGatewayTestAgent, sessionIDs(sessions)))
	}
	for _, obj := range klausGatewayObjects() {
		gvr, err := gvrFor(obj.resource)
		if err != nil {
			continue
		}
		if exists, _ := objectExists(ctx, gvr, kagentNamespace, obj.name); exists {
			left = append(left, obj.resource+" "+obj.name)
		}
	}
	return left
}

// instanceIDs joins the ids of instances for a message.
func sessionIDs(sessions []*apiv1alpha1.Session) string {
	ids := make([]string, 0, len(sessions))
	for _, session := range sessions {
		ids = append(ids, session.GetId())
	}
	return strings.Join(ids, ", ")
}

// --- the controller, read as the person through the edge --------------------

// fixtureSessions is the caller's conversations of the fixture:
// ListSessions narrowed to the Agent.
func fixtureSessions(ctx context.Context, api *kagentAPI) ([]*apiv1alpha1.Session, error) {
	return sessionsOf(ctx, api, klausGatewayTestAgent)
}

// instancesOf is the caller's conversations of one template.
func sessionsOf(ctx context.Context, api *kagentAPI, agent string) ([]*apiv1alpha1.Session, error) {
	sessions, err := api.listSessionsOf(ctx, agentRef(agent))
	if err != nil {
		return nil, fmt.Errorf("listing the Sessions of %s: %w", agent, err)
	}
	return sessions, nil
}

// assertNoSessions checks the controller lists no conversation of the
// template: a refused or unauthenticated message started nothing.
func assertNoSessions(api *kagentAPI, agent string) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	sessions, err := sessionsOf(ctx, api, agent)
	if err != nil {
		return err
	}
	if len(sessions) > 0 {
		return fmt.Errorf("the controller lists %d Session(s) of %s (%s), wanted none", len(sessions), agent, sessionIDs(sessions))
	}
	return nil
}

// assertOnlySessions checks the fixture's conversations are exactly the
// given ones: each thread bound once, nothing created behind them.
func assertOnlySessions(api *kagentAPI, ids ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	sessions, err := fixtureSessions(ctx, api)
	if err != nil {
		return err
	}
	got := make([]string, 0, len(sessions))
	for _, session := range sessions {
		got = append(got, session.GetId())
	}
	slices.Sort(got)
	want := slices.Sorted(slices.Values(ids))
	if !slices.Equal(got, want) {
		return fmt.Errorf("the controller lists the Session(s) [%s] of %s, wanted exactly [%s]", strings.Join(got, ", "), klausGatewayTestAgent, strings.Join(want, ", "))
	}
	return nil
}

// assertNothingWaiting checks no task of the session is left at
// input-required.
func assertNothingWaiting(api *kagentAPI, session *apiv1alpha1.Session) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	tasks, err := api.listTasks(ctx, session)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.Status.State == a2a.TaskStateInputRequired {
			return fmt.Errorf("task %s of Session %s is still at input-required after the decisions", t.ID, session.GetId())
		}
	}
	return nil
}

// taskIDs is the set of a session's task ids.
func (a *kagentAPI) taskIDs(ctx context.Context, session *apiv1alpha1.Session) (map[a2a.TaskID]bool, error) {
	tasks, err := a.listTasks(ctx, session)
	if err != nil {
		return nil, err
	}
	ids := make(map[a2a.TaskID]bool, len(tasks))
	for _, t := range tasks {
		ids[t.ID] = true
	}
	return ids, nil
}

// waitCanceledTask polls the session's tasks until one not in before is
// TASK_STATE_CANCELED and returns its id; what the new tasks say otherwise is
// the error.
func (a *kagentAPI) waitCanceledTask(session *apiv1alpha1.Session, before map[a2a.TaskID]bool, timeout time.Duration) (string, error) {
	var canceled string
	var seen []string
	waitFor(int(timeout/pollInterval), pollInterval, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		tasks, err := a.listTasks(ctx, session)
		if err != nil {
			return false
		}
		seen = seen[:0]
		for _, t := range tasks {
			if before[t.ID] {
				continue
			}
			state := t.Status.State
			seen = append(seen, string(t.ID)+" "+string(state))
			if state == a2a.TaskStateCanceled {
				canceled = string(t.ID)
			}
		}
		return canceled != ""
	})
	if canceled == "" {
		return "", fmt.Errorf("no task of Session %s reached TASK_STATE_CANCELED within %s after stop (new tasks: %s)", session, timeout, strings.Join(seen, ", "))
	}
	return canceled, nil
}

// --- the gateway on the host -----------------------------------------------

// gatewayProcess runs klaus-gateway on the host: a local binary, or the
// released image on the host network with the run directory and the lab CA
// mounted. Its log (JSON on stderr) accumulates across restarts in the run
// directory; the stores live there too, so a restart resumes on them.
type gatewayProcess struct {
	opts      KlausGatewayTestOptions
	runDir    string
	caFile    string
	target    string
	slackAPI  string
	musterURL string
	// extraArgs are flags beyond gatewayArgs, and kubeconfig a kubeconfig
	// the gateway reads the API server through (the reviews endpoint's
	// TokenReview), mounted read-only in the image shape; empty, none.
	extraArgs  []string
	kubeconfig string
	// trustLabCA makes the lab CA the gateway's trust store for every TLS
	// call it makes itself (SSL_CERT_FILE): muster, for a proof whose clicks
	// call a tool through it.
	trustLabCA bool
	// network is the container's docker network (default host); host the
	// address the proof reaches its endpoints on (default 127.0.0.1).
	network, host string
	cmd           *exec.Cmd
	logFile       *os.File
	// exited is closed once the process has ended; exitErr is its Wait result.
	// A closed channel satisfies every later wait, so a gateway that died
	// before serving is noticed by the readiness probe and stop() still
	// returns.
	exited  chan struct{}
	exitErr error
}

func newGatewayProcess(opts KlausGatewayTestOptions, runDir, caFile, target, slackAPI, musterURL string) *gatewayProcess {
	return &gatewayProcess{opts: opts, runDir: runDir, caFile: caFile, target: target, slackAPI: slackAPI, musterURL: musterURL}
}

// describe names what runs.
func (g *gatewayProcess) describe() string {
	if g.opts.GatewayBinary != "" {
		return "binary " + g.opts.GatewayBinary
	}
	return "image " + g.opts.GatewayImage
}

func (g *gatewayProcess) baseURL() string {
	return fmt.Sprintf("http://%s:%d", cmp.Or(g.host, "127.0.0.1"), g.opts.Port)
}

func (g *gatewayProcess) adminURL() string {
	return fmt.Sprintf("http://%s:%d", cmp.Or(g.host, "127.0.0.1"), g.opts.Port+1)
}

func (g *gatewayProcess) logPath() string { return filepath.Join(g.runDir, gatewayLogFile) }

// gatewayFlags is where the gateway listens, what it talks to and where its
// files are, for one shape.
type gatewayFlags struct {
	port                        int
	dataDir, caFile             string
	target, slackAPI, musterURL string
}

// gatewayArgs are the gateway's flags for one shape: the Slack endpoints and
// the admin port on loopback, the bolt routing store, the Slack adapter in
// events mode on the fake Web API with the run's secrets, the a2a client at
// the grpcs target with the CA and the fixture as the default agent, OBO
// with the bolt link store the proof seeded. The callback base is the
// gateway's own loopback address — no sign-in completes in the lab, the
// link is seeded — and muster is the lab's, never called while the link's
// token is fresh. dataDir and caFile are the paths as the process sees them
// (the container's mounts in the image shape).
func gatewayArgs(f gatewayFlags) []string {
	data := func(name string) string { return filepath.Join(f.dataDir, name) }
	return []string{
		"--listen-address=" + net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port)),
		"--admin-address=" + net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port+1)),
		"--log-level=" + gatewayLogLevel,
		"--store=bolt",
		"--bolt-path=" + data(gatewayBoltFile),
		"--slack-enabled=true",
		"--slack-mode=events",
		"--slack-secrets-file=" + data(gatewaySecrets),
		"--slack-api-base=" + f.slackAPI,
		"--a2a-enabled=true",
		"--a2a-url=" + f.target,
		"--a2a-ca-file=" + f.caFile,
		"--a2a-namespace=" + kagentNamespace,
		"--a2a-default-agent=" + klausGatewayTestAgent,
		"--obo-enabled=true",
		"--obo-muster-url=" + f.musterURL,
		"--obo-callback-base-url=" + fmt.Sprintf("http://127.0.0.1:%d", f.port),
		"--obo-state-key-file=" + data(gatewayStateKey),
		"--obo-store=bolt",
		"--obo-store-path=" + data(gatewayLinksFile),
		"--obo-store-key-file=" + data(gatewayStoreKey),
	}
}

// dockerRunArgs wraps the gateway's flags into `docker run`: the host network
// (the loopback ports, the fake and the edge's public hostname as the host
// sees them), the caller's uid so the stores are writable in the run
// directory, the run directory and the CA mounted read-write and read-only.
func dockerRunArgs(image, name, network, runDir, caFile string, uid, gid int, args []string) []string {
	return append(dockerRun(name, cmp.Or(network, "host"), "--rm",
		"--user", fmt.Sprintf("%d:%d", uid, gid),
		"-v", runDir+":"+gatewayDataPath,
		"-v", caFile+":"+gatewayCAPath+":ro",
		image,
	), args...)
}

// start launches the gateway and waits for its readiness.
func (g *gatewayProcess) start() error {
	logFile, err := os.OpenFile(g.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	g.logFile = logFile
	flags := gatewayFlags{port: g.opts.Port, dataDir: g.runDir, caFile: g.caFile, target: g.target, slackAPI: g.slackAPI, musterURL: g.musterURL}
	if g.opts.GatewayBinary != "" {
		g.cmd = command(g.opts.GatewayBinary, append(gatewayArgs(flags), g.extraArgs...)...)
		g.cmd.Env = os.Environ()
		if g.kubeconfig != "" {
			g.cmd.Env = append(g.cmd.Env, "KUBECONFIG="+g.kubeconfig)
		}
		if g.trustLabCA {
			g.cmd.Env = append(g.cmd.Env, "SSL_CERT_FILE="+g.caFile)
		}
	} else {
		_ = command(dockerBin, "rm", "-f", gatewayContainer).Run()
		flags.dataDir, flags.caFile = gatewayDataPath, gatewayCAPath
		args := dockerRunArgs(g.opts.GatewayImage, gatewayContainer, g.network, g.runDir, g.caFile, os.Getuid(), os.Getgid(), append(gatewayArgs(flags), g.extraArgs...))
		// The mounts and the variables go before the image, which
		// dockerRunArgs puts right after its own flags.
		var extra []string
		if g.kubeconfig != "" {
			extra = append(extra, "-v", g.kubeconfig+":"+gatewayKubeconfigPath+":ro", "-e", "KUBECONFIG="+gatewayKubeconfigPath)
		}
		if g.trustLabCA {
			extra = append(extra, "-e", "SSL_CERT_FILE="+gatewayCAPath)
		}
		args = slices.Insert(args, slices.Index(args, g.opts.GatewayImage), extra...)
		g.cmd = command(dockerBin, args...)
	}
	g.cmd.Stdout, g.cmd.Stderr = logFile, logFile
	if err := g.cmd.Start(); err != nil {
		_ = logFile.Close()
		g.cmd, g.logFile = nil, nil
		return fmt.Errorf("starting klaus-gateway (%s): %w", g.describe(), err)
	}
	g.exited = make(chan struct{})
	go func(cmd *exec.Cmd, exited chan<- struct{}) { g.exitErr = cmd.Wait(); close(exited) }(g.cmd, g.exited)

	client := &http.Client{Timeout: 2 * time.Second}
	ready := waitFor(int(klausGatewayStartWait/(500*time.Millisecond)), 500*time.Millisecond, func() bool {
		if g.ended() {
			return true
		}
		resp, err := client.Get(g.adminURL() + gatewayReadyPath)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	if g.ended() || !ready {
		why := fmt.Sprintf("did not become ready at %s%s within %s", g.adminURL(), gatewayReadyPath, klausGatewayStartWait)
		if g.ended() {
			why = fmt.Sprintf("exited before serving %s (%v)", gatewayReadyPath, g.exitErr)
		}
		log := tailLines(g.logs(), 8)
		_ = g.stop()
		return fmt.Errorf("klaus-gateway (%s) %s; its log ends:\n%s", g.describe(), why, log)
	}
	return nil
}

// ended reports whether the gateway process has exited (exitErr says how).
func (g *gatewayProcess) ended() bool {
	select {
	case <-g.exited:
		return true
	default:
		return false
	}
}

// stop ends the gateway: SIGTERM (docker stop for the container) and a
// bounded wait, SIGKILL after it. Idempotent; a gateway that never started
// is nothing to stop.
func (g *gatewayProcess) stop() error {
	if g.cmd == nil {
		return nil
	}
	if g.opts.GatewayBinary == "" {
		_ = command(dockerBin, "stop", "-t", "15", gatewayContainer).Run()
	} else {
		_ = g.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-g.exited:
	case <-time.After(klausGatewayStopWait):
		_ = g.cmd.Process.Kill()
		<-g.exited
	}
	g.cmd, g.exited = nil, nil
	if g.logFile != nil {
		_ = g.logFile.Close()
		g.logFile = nil
	}
	return nil
}

// version is the gateway's own "starting" record, for the notes.
func (g *gatewayProcess) version() string { return gatewayVersion(g.logs()) }

// gatewayVersion reads the gateway's "starting" record off its log: the
// version and commit it runs, whatever shape (a host process, a pod) wrote
// the log. A line may carry a pod prefix before its JSON record.
func gatewayVersion(logs string) string {
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `"klaus-gateway starting"`) {
			continue
		}
		var rec struct {
			Version string `json:"version"`
			GitSHA  string `json:"git_sha"`
		}
		if start := strings.IndexByte(line, '{'); start >= 0 && json.Unmarshal([]byte(line[start:]), &rec) == nil && rec.Version != "" {
			return fmt.Sprintf("klaus-gateway %s (%s)", rec.Version, rec.GitSHA)
		}
	}
	return "klaus-gateway (version not logged)"
}

// logs is everything the gateway wrote so far.
func (g *gatewayProcess) logs() string {
	b, _ := os.ReadFile(g.logPath())
	return string(b)
}

// gatewayRecord is one of the gateway's structured log records the proof
// reads: the thread bound to a Session (session_bound), a turn
// dispatched (turn_dispatch) and completed (turn_complete), a link's
// id_token refreshed (token_refresh).
type gatewayRecord struct {
	Record      string `json:"record"`
	Outcome     string `json:"outcome"`
	Error       string `json:"error"`
	TaskID      string `json:"task_id"`
	ThreadID    string `json:"thread_id"`
	MessageID   string `json:"message_id"`
	Agent       string `json:"agent"`
	AgentSource string `json:"agent_source"`
	SlackUser   string `json:"slack_user"`
	Subject     string `json:"subject"`
	Sub         string `json:"sub"`
	Resume      bool   `json:"resume"`
	// Thread and Session are the session_bound record's.
	Thread  string `json:"thread"`
	Session string `json:"session"`
	Trigger string `json:"trigger"`
}

// thread is the Slack thread a record is about.
func (r gatewayRecord) thread() string {
	if r.ThreadID != "" {
		return r.ThreadID
	}
	return r.Thread
}

// gatewayRecords reads the records of one kind off a log, in order; a line
// may carry a pod prefix before its JSON record.
func gatewayRecords(logs, kind string) []gatewayRecord {
	var out []gatewayRecord
	needle := `"record":"` + kind + `"`
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		start := strings.IndexByte(line, '{')
		var rec gatewayRecord
		if start < 0 || json.Unmarshal([]byte(line[start:]), &rec) != nil || rec.Record != kind {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// threadRecords narrows records to one thread.
func threadRecords(records []gatewayRecord, thread string) []gatewayRecord {
	var out []gatewayRecord
	for _, r := range records {
		if r.thread() == thread {
			out = append(out, r)
		}
	}
	return out
}

// tailLines is the last n non-empty lines of s, each cut short.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, line := range lines {
		lines[i] = excerpt(line, 240)
	}
	return strings.Join(lines, "\n")
}

// --- Slack, driven as the people -----------------------------------------------

// slackPeople are the Slack users of the fake workspace: the linked person,
// one with no link, and the person the component half links in its Secret.
type slackPeople struct {
	person, stranger, component string
}

// slackThread is one conversation of the proof: the thread's ts (empty until
// its first message), who started it, and how many of its turns the gateway
// has completed.
type slackThread struct {
	ts    string
	user  string
	turns int
}

// slackProof drives one gateway through its Slack adapter: the driver posts,
// the fake shows what the thread got, the gateway's log says when a turn is
// over and how it ended.
type slackProof struct {
	driver  *slackDriver
	fake    slackWorkspace
	channel string
	logs    func() (string, error)
}

// slackTurn is one turn as the thread saw it: the gateway's turn_complete
// record, the thread's messages when it ended, the stream the turn wrote last
// and the text of every stream the turn wrote.
type slackTurn struct {
	record gatewayRecord
	msgs   []slackMessage
	stream slackMessage
	answer string
}

// records is the gateway's records of one kind for a thread.
func (p *slackProof) records(kind, thread string) []gatewayRecord {
	logs, err := p.logs()
	if err != nil {
		return nil
	}
	return threadRecords(gatewayRecords(logs, kind), thread)
}

// say posts text as a mention in the thread (its first message opens it)
// and waits for the turn's end.
func (p *slackProof) say(t *slackThread, text string) (*slackTurn, error) {
	before := len(p.fake.thread(p.channel, t.ts))
	ts, err := p.driver.mention(t.user, t.ts, text)
	if err != nil {
		return nil, err
	}
	if t.ts == "" {
		t.ts, before = ts, 0
	}
	return p.awaitTurn(t, before)
}

// slackPoll is how often the proof reads the gateway's log for a turn's end:
// a read of a local file, or of a small pod log.
const slackPoll = 500 * time.Millisecond

// awaitTurn waits for the thread's next turn_complete record and reads what
// the turn left in the thread after its first `before` messages.
func (p *slackProof) awaitTurn(t *slackThread, before int) (*slackTurn, error) {
	var done []gatewayRecord
	if !waitFor(int(klausGatewayTurnTimeout/slackPoll), slackPoll, func() bool {
		done = p.records(recordTurnDone, t.ts)
		return len(done) > t.turns
	}) {
		logs, _ := p.logs()
		return nil, fmt.Errorf("no turn of thread %s completed within %s; the thread shows: %s; the gateway's log ends:\n%s",
			t.ts, klausGatewayTurnTimeout, threadLine(p.fake.thread(p.channel, t.ts)), tailLines(logs, 8))
	}
	rec := done[t.turns]
	t.turns++
	msgs := p.fake.thread(p.channel, t.ts)
	stream, answer := streamedSince(msgs, before)
	return &slackTurn{record: rec, msgs: msgs, stream: stream, answer: answer}, nil
}

// firstTurn is a thread's first message with one visible retry: a cold
// worker's first resume can hit Substrate's ResumeActor deadline once.
func (p *slackProof) firstTurn(t *slackThread, text string) (*slackTurn, error) {
	turn, err := p.say(t, text)
	if err == nil && (turn.record.Outcome == outcomeCompleted || turn.record.Outcome == outcomeInputReq) {
		return turn, nil
	}
	reason := fmt.Sprint(err)
	if err == nil {
		reason = turnFailure(turn)
	}
	note("the thread's first turn failed (%s); retrying once — a cold worker's first resume may hit the ResumeActor deadline", excerpt(reason, 200))
	return p.say(t, text)
}

// turnFailure words a turn that did not complete.
func turnFailure(turn *slackTurn) string {
	failure := "outcome " + turn.record.Outcome
	if turn.record.Error != "" {
		failure += ": " + excerpt(turn.record.Error, 300)
	}
	return failure + "; the thread shows: " + threadLine(turn.msgs)
}

// assertSlackTurnSaid checks a turn completed and its streamed answer
// carries the word.
func assertSlackTurnSaid(turn *slackTurn, word string) error {
	if turn.record.Outcome != outcomeCompleted {
		return fmt.Errorf("the turn did not complete: %s", turnFailure(turn))
	}
	if !strings.Contains(strings.ToLower(turn.answer), strings.ToLower(word)) {
		return fmt.Errorf("the turn completed but its streamed answer %q does not carry %q; the thread shows: %s", excerpt(turn.answer, 200), word, threadLine(turn.msgs))
	}
	return nil
}

// assertBranded checks the answer streamed under the template's display name
// and icon — how Slack shows which agent answers.
func assertBranded(turn *slackTurn) error {
	if turn.stream.Username != klausGatewayTestDisplay || turn.stream.IconURL != klausGatewayTestIcon {
		return fmt.Errorf("the answer streamed as username %q, icon_url %q; wanted %q and %q from the template's annotations",
			turn.stream.Username, turn.stream.IconURL, klausGatewayTestDisplay, klausGatewayTestIcon)
	}
	return nil
}

// roster posts `@bot agents` in a new thread and returns the roster post:
// a row per agent, each with its Select button, its text listing the display
// names.
func (p *slackProof) roster(user string) (slackMessage, error) {
	ts, err := p.driver.mention(user, "", slackAgentsWord)
	if err != nil {
		return slackMessage{}, err
	}
	msgs, ok := waitThread(p.fake, p.channel, ts, klausGatewayReplyWait, func(msgs []slackMessage) bool {
		_, found := findMessage(msgs, slackRosterHeading)
		return found
	})
	if !ok {
		return slackMessage{}, fmt.Errorf("`@bot %s` got no roster within %s; the thread shows: %s", slackAgentsWord, klausGatewayReplyWait, threadLine(msgs))
	}
	post, _ := findMessage(msgs, slackRosterHeading)
	return post, nil
}

// assertSlackRoster checks the roster lists the admitted fixture by its
// display name and not the unadmitted one.
func assertSlackRoster(names []string) error {
	if slices.Contains(names, klausGatewayTestUnadmittedDisplay) {
		return fmt.Errorf("the roster lists %q, which no Harness runs (%s)", klausGatewayTestUnadmittedDisplay, strings.Join(names, ", "))
	}
	if !slices.Contains(names, klausGatewayTestDisplay) {
		return fmt.Errorf("the roster does not list %q (%s)", klausGatewayTestDisplay, strings.Join(names, ", "))
	}
	return nil
}

// unadmittedReason is the reason the gateway gives for an Agent that is not
// Ready (its condition follows: "Agent <name> is not ready: blocked by ...").
const unadmittedReason = "is not ready"

// refusal presses the Select button of the fixture's roster row, which opens
// the agent picker in the roster's thread, checks the picker offers the
// fixture and not the unadmitted template, and submits it with the
// unadmitted template selected — a template that left the roster between
// the listing and the submit. It returns the refusal, which must name the
// reason and say nothing started.
func (p *slackProof) refusal(user string, roster slackMessage, question string) (string, error) {
	var ref string
	for _, b := range roster.actions(slackActionAgentSelect) {
		if v, _ := b[slackKeyValue].(string); strings.HasSuffix(v, klausGatewayTestAgent) {
			ref = v
		}
	}
	if ref == "" {
		return "", fmt.Errorf("the roster carries no %s button for %s: %s", slackActionAgentSelect, klausGatewayTestAgent, threadLine([]slackMessage{roster}))
	}
	before := len(p.fake.openedViews())
	if err := p.driver.press(user, roster, slackActionAgentSelect, ref); err != nil {
		return "", err
	}
	var picker map[string]any
	if !waitFor(int(klausGatewayReplyWait/(250*time.Millisecond)), 250*time.Millisecond, func() bool {
		if views := p.fake.openedViews(); len(views) > before {
			picker = views[len(views)-1]
		}
		return picker != nil
	}) {
		return "", fmt.Errorf("the Select button of %s opened no picker within %s; the thread shows: %s", klausGatewayTestAgent, klausGatewayReplyWait, threadLine(p.fake.thread(p.channel, roster.ThreadTS)))
	}
	if picker["callback_id"] != slackPickerCallback {
		return "", fmt.Errorf("the Select button opened modal %v, not the agent picker %s", picker["callback_id"], slackPickerCallback)
	}
	offered := pickerOptions(picker)
	unadmitted := strings.TrimSuffix(ref, klausGatewayTestAgent) + klausGatewayTestUnadmitted
	if !slices.Contains(offered, ref) || slices.Contains(offered, unadmitted) {
		return "", fmt.Errorf("the picker offers %s; wanted %s and not %s", strings.Join(offered, ", "), ref, unadmitted)
	}
	shown := len(p.fake.thread(p.channel, roster.ThreadTS))
	if err := p.driver.submitView(user, picker, map[string]any{
		slackPickerAgentBlock:    map[string]any{slackPickerAgentAction: map[string]any{fieldTypeKey: "static_select", "selected_option": map[string]any{slackKeyValue: unadmitted}}},
		slackPickerQuestionBlock: map[string]any{slackPickerQuestionAction: map[string]any{fieldTypeKey: "plain_text_input", slackKeyValue: question}},
	}); err != nil {
		return "", err
	}
	msgs, ok := waitThread(p.fake, p.channel, roster.ThreadTS, klausGatewayReplyWait, func(msgs []slackMessage) bool {
		_, found := findMessage(msgs[min(shown, len(msgs)):], slackNotRunnable)
		return found
	})
	if !ok {
		return "", fmt.Errorf("selecting %s in the picker got no refusal within %s; the thread shows: %s", klausGatewayTestUnadmitted, klausGatewayReplyWait, threadLine(msgs))
	}
	msg, _ := findMessage(msgs[min(shown, len(msgs)):], slackNotRunnable)
	text := msg.shown()
	if msg.Recipient != user || !strings.Contains(text, unadmittedReason) || !strings.Contains(text, slackNotStarted) {
		return "", fmt.Errorf("selecting %s was refused without the reason %q or %q, or not to %s alone: %s", klausGatewayTestUnadmitted, unadmittedReason, slackNotStarted, user, excerpt(text, 300))
	}
	first, _, _ := strings.Cut(text, "\n")
	return first, nil
}

// pickerOptions are the agent refs the picker's agent select offers.
func pickerOptions(view map[string]any) []string {
	var refs []string
	blocks, _ := view["blocks"].([]any)
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		if block["block_id"] != slackPickerAgentBlock {
			continue
		}
		element, _ := block["element"].(map[string]any)
		options, _ := element["options"].([]any)
		for _, o := range options {
			if opt, ok := o.(map[string]any); ok {
				if v, ok := opt[slackKeyValue].(string); ok {
					refs = append(refs, v)
				}
			}
		}
	}
	return refs
}

// signInPrompt posts a message as a person with no link and returns the
// sign-in link: the Sign in button (obo_sign_in) the gateway shows that
// person alone, pointing at its link route. The prose around it is the
// gateway's to word.
func (p *slackProof) signInPrompt(user, text string) (string, error) {
	ts, err := p.driver.mention(user, "", text)
	if err != nil {
		return "", err
	}
	var button map[string]any
	msgs, ok := waitThread(p.fake, p.channel, ts, klausGatewayReplyWait, func(msgs []slackMessage) bool {
		for _, m := range msgs {
			if b, found := m.action(slackActionSignIn); found && (m.Recipient == user || m.Method == slackPostMessage) {
				button = b
				return true
			}
		}
		return false
	})
	if !ok {
		return "", fmt.Errorf("the unlinked %s got no sign-in prompt (a %s button shown to them) within %s; the thread shows: %s", user, slackActionSignIn, klausGatewayReplyWait, threadLine(msgs))
	}
	link, _ := button[slackKeyURL].(string)
	if !strings.Contains(link, musterlink.LinkPath) {
		return "", fmt.Errorf("the sign-in button points at %q, not the gateway's %s route", link, musterlink.LinkPath)
	}
	return link, nil
}

// boundSession is the Session the gateway bound the thread to (its
// session_bound record), read back from the controller.
func (p *slackProof) boundSession(api *kagentAPI, t *slackThread) (*apiv1alpha1.Session, error) {
	bound := p.records(recordBound, t.ts)
	if len(bound) == 0 || bound[len(bound)-1].Session == "" {
		logs, _ := p.logs()
		return nil, fmt.Errorf("the gateway's log carries no session_bound record for thread %s (the turn ran without binding the thread to a Session); its log ends:\n%s", t.ts, tailLines(logs, 8))
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	return api.getSession(ctx, bound[len(bound)-1].Session)
}

// dispatch checks the thread's first turn_dispatch record: the fixture as
// the default agent, the person as the Slack user, the lab user's e-mail and
// the link's Dex subject as the identity the turn ran under.
func (p *slackProof) dispatch(t *slackThread, subject, email string) (gatewayRecord, error) {
	records := p.records(recordDispatch, t.ts)
	if len(records) == 0 {
		return gatewayRecord{}, fmt.Errorf("the gateway's log carries no turn_dispatch record for thread %s", t.ts)
	}
	d := records[0]
	if !strings.HasSuffix(d.Agent, klausGatewayTestAgent) || d.SlackUser != t.user || d.Subject != email || d.Sub != subject {
		return d, fmt.Errorf("turn_dispatch of thread %s: agent %q, slack_user %q, subject %q, sub %q; wanted %s, %s, %s and the token's subject %.8s…",
			t.ts, d.Agent, d.SlackUser, d.Subject, d.Sub, klausGatewayTestAgent, t.user, email, subject)
	}
	return d, nil
}

// decisionOutcome is how an approval round trip ended.
type decisionOutcome struct {
	taskID     string
	rounds     int
	cards      []string
	finalState a2a.TaskState
	outcome    string
	answer     string
}

// decideUntilSettled answers the tool-using turn's approval card with
// Approve (or Deny) and repeats while the resumed task pauses again (a
// meta-tool call follows the first; a denied model may try once more), up to
// klausGatewayHITLRounds: each pause must be the same task, at
// input-required at the controller, and each decision must rewrite its card
// to name the person. The task's state at the controller once it settles is
// the outcome.
func (p *slackProof) decideUntilSettled(api *kagentAPI, session *apiv1alpha1.Session, t *slackThread, turn *slackTurn, approve bool) (*decisionOutcome, error) {
	action, verdict := slackActionDeny, slackDeniedBy
	if approve {
		action, verdict = slackActionApprove, slackApprovedBy
	}
	if turn.record.Outcome != outcomeInputReq {
		return nil, fmt.Errorf("the tool-using turn did not pause for approval (%s): the requireApproval binding did not gate the tool call", turnFailure(turn))
	}
	out := &decisionOutcome{taskID: turn.record.TaskID}
	for turn.record.Outcome == outcomeInputReq {
		if out.rounds == klausGatewayHITLRounds {
			return nil, fmt.Errorf("task %s still paused after %d decisions", out.taskID, out.rounds)
		}
		card, ok := openCard(turn.msgs)
		if !ok {
			return nil, fmt.Errorf("%w (turn paused on task %s): %s", errNoCard, turn.record.TaskID, threadLine(turn.msgs))
		}
		if card.taskID != out.taskID || turn.record.TaskID != out.taskID {
			return nil, fmt.Errorf("the resumed turn paused on task %s (card for %s), not the one it resumed (%s): the decision did not resume in place", turn.record.TaskID, card.taskID, out.taskID)
		}
		out.rounds++
		out.cards = append(out.cards, excerpt(strings.ReplaceAll(card.msg.shown(), "\n", " "), 80))
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		task, err := api.getTask(ctx, session, a2a.TaskID(out.taskID))
		cancel()
		if err != nil {
			return nil, err
		}
		if state := task.Status.State; state != a2a.TaskStateInputRequired {
			return nil, fmt.Errorf("the thread shows an approval card for task %s but the controller has it %s, not TASK_STATE_INPUT_REQUIRED", out.taskID, state)
		}
		before := len(turn.msgs)
		if err := p.driver.click(t.user, card.msg, action); err != nil {
			return nil, fmt.Errorf("decision %d: %w", out.rounds, err)
		}
		if _, ok := waitThread(p.fake, p.channel, t.ts, klausGatewayReplyWait, func(msgs []slackMessage) bool {
			for _, m := range msgs {
				if m.TS == card.msg.TS {
					return strings.Contains(m.shown(), verdict+t.user+">")
				}
			}
			return false
		}); !ok {
			return nil, fmt.Errorf("decision %d: the card was not rewritten to %q within %s: %s", out.rounds, verdict+t.user+">", klausGatewayReplyWait, threadLine(p.fake.thread(p.channel, t.ts)))
		}
		if turn, err = p.awaitTurn(t, before); err != nil {
			return nil, fmt.Errorf("the turn after decision %d: %w", out.rounds, err)
		}
	}
	out.outcome, out.answer = turn.record.Outcome, turn.answer
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	task, err := api.getTask(ctx, session, a2a.TaskID(out.taskID))
	if err != nil {
		return nil, err
	}
	out.finalState = task.Status.State
	return out, nil
}

// stop posts a long question in the thread, and once its answer streams,
// `stop` as a reply: the turn must end canceled with the thread told so.
func (p *slackProof) stop(t *slackThread, text string) (*slackTurn, error) {
	before := len(p.fake.thread(p.channel, t.ts))
	if _, err := p.driver.mention(t.user, t.ts, text); err != nil {
		return nil, err
	}
	msgs, streaming := waitThread(p.fake, p.channel, t.ts, klausGatewayTurnTimeout, func(msgs []slackMessage) bool {
		if len(p.records(recordTurnDone, t.ts)) > t.turns {
			return true // over already; reported below
		}
		_, answer := streamedSince(msgs, before)
		return answer != ""
	})
	if !streaming {
		return nil, fmt.Errorf("the turn to stop streamed nothing within %s: %s", klausGatewayTurnTimeout, threadLine(msgs))
	}
	if _, err := p.driver.reply(t.user, t.ts, slackStopWord); err != nil {
		return nil, err
	}
	turn, err := p.awaitTurn(t, before)
	if err != nil {
		return nil, fmt.Errorf("the turn after %s: %w", slackStopWord, err)
	}
	if turn.record.Outcome != outcomeCanceled {
		return nil, fmt.Errorf("the turn to stop ended %s, not canceled — it finished before %s arrived, or the stop did not reach it: %s", turn.record.Outcome, slackStopWord, turnFailure(turn))
	}
	if _, ok := waitThread(p.fake, p.channel, t.ts, klausGatewayReplyWait, func(msgs []slackMessage) bool {
		_, found := findMessage(msgs[min(before, len(msgs)):], slackStopped)
		return found
	}); !ok {
		return nil, fmt.Errorf("the thread was not told %q: %s", slackStopped, threadLine(p.fake.thread(p.channel, t.ts)))
	}
	return turn, nil
}

// resumedTurn is what step 5b saw: the task the restarted gateway delivered,
// how many recovery attempts failed while the gate was closed, the answer the
// thread got after the restart and how long after the restart it landed.
type resumedTurn struct {
	taskID         string
	failedAttempts int
	answer         string
	after          time.Duration
}

// restartMidTurn asks a long question in the thread (one whose Session
// is warm, so the answer streams soon) and, once its answer streams, restarts
// the gateway with the gate closed: the stopping process
// leaves the task running and promises the post, the new one cannot reach the
// controller for klausGatewayGateClosed. Once the gate opens again, the
// thread must get the rest of the answer with no message from the person.
func (p *slackProof) restartMidTurn(gw *gatewayProcess, gate *tcpGate, t *slackThread, text string) (*resumedTurn, error) {
	asked := len(p.fake.thread(p.channel, t.ts))
	if _, err := p.driver.mention(t.user, t.ts, text); err != nil {
		return nil, err
	}
	msgs, streaming := waitThread(p.fake, p.channel, t.ts, klausGatewayTurnTimeout, func(msgs []slackMessage) bool {
		_, answer := streamedSince(msgs, asked)
		return answer != ""
	})
	if !streaming {
		return nil, fmt.Errorf("the turn to restart under streamed nothing within %s: %s", klausGatewayTurnTimeout, threadLine(msgs))
	}
	if err := gw.stop(); err != nil {
		return nil, err
	}
	if len(p.records(recordLeftRunning, t.ts)) == 0 {
		return nil, fmt.Errorf("the stopping gateway left no task running for thread %s (no %s record)", t.ts, recordLeftRunning)
	}
	msgs = p.fake.thread(p.channel, t.ts)
	if _, ok := findMessage(msgs[asked:], slackRestartPromise); !ok {
		return nil, fmt.Errorf("the stopping gateway did not promise the post (%q): %s", slackRestartPromise, threadLine(msgs))
	}
	before := len(msgs)
	gate.setOpen(false)
	restarted := time.Now()
	if err := gw.start(); err != nil {
		gate.setOpen(true)
		return nil, fmt.Errorf("restarting the gateway: %w", err)
	}
	time.Sleep(klausGatewayGateClosed)
	failed := len(p.recoveryFailures(t.ts))
	gate.setOpen(true)
	if failed == 0 {
		return nil, fmt.Errorf("no recovery attempt failed while the gate was closed: the proof did not take the controller out of reach")
	}
	var resume []gatewayRecord
	msgs, delivered := waitThread(p.fake, p.channel, t.ts, klausGatewayTurnTimeout, func(msgs []slackMessage) bool {
		resume = p.records(recordTurnResume, t.ts)
		_, answer := streamedSince(msgs, before)
		return len(resume) > 0 && answer != ""
	})
	if !delivered {
		logs, _ := p.logs()
		return nil, fmt.Errorf("the turn left running was not posted within %s of the gate opening: %s; the gateway's log ends:\n%s",
			klausGatewayTurnTimeout, threadLine(msgs), tailLines(logs, 8))
	}
	if _, gaveUp := findMessage(msgs[before:], slackReplyToGetIt); gaveUp {
		return nil, fmt.Errorf("the restarted gateway gave up on the turn (%q) before delivering it: %s", slackReplyToGetIt, threadLine(msgs))
	}
	_, answer := streamedSince(msgs, before)
	return &resumedTurn{taskID: resume[0].TaskID, failedAttempts: failed, answer: answer, after: time.Since(restarted)}, nil
}

// recoveryFailures is the restarted gateway's warnings about a turn of thread
// it could not resubscribe to yet.
func (p *slackProof) recoveryFailures(thread string) []string {
	logs, err := p.logs()
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, gatewayResubscribeFailed) && strings.Contains(line, thread) {
			out = append(out, line)
		}
	}
	return out
}

// assertMusterAttribution reads muster's log since the tool-using turn began:
// the forwarded id_token of the person accepted (the audit record with the
// email), and tools/call requests under the token's subject — the agent's
// muster calls ran as the person, not as the gateway or the Harness.
func assertMusterAttribution(email, subject string, since time.Time) error {
	var accepted, calls int
	var lines []string
	waitFor(klausGatewayMusterAttempt, pollInterval, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		logs, err := podLogs(ctx, platformNamespace, "deploy/"+componentMuster, componentMuster, klausGatewayLogSince)
		if err != nil {
			return false
		}
		accepted, calls, lines = musterAttribution(logs, email, subject, since)
		return accepted > 0 && calls > 0
	})
	if accepted == 0 {
		return fmt.Errorf("muster's log since %s carries no %s record for %s: the agent's tool calls did not ride the person's forwarded id_token", since.UTC().Format(time.RFC3339), musterTokenAccepted, email)
	}
	if calls == 0 {
		return fmt.Errorf("muster's log since %s carries %d %s record(s) for %s but no tools/call request under subject %.8s…: %s", since.UTC().Format(time.RFC3339), accepted, musterTokenAccepted, email, subject, strings.Join(lines, " | "))
	}
	return nil
}

// musterAttribution counts, in muster's log lines stamped after since, the
// accepted-token audit records naming the email and the tools/call requests
// whose (truncated) subject is a prefix of the token's subject.
func musterAttribution(logs, email, subject string, since time.Time) (accepted, calls int, subjects []string) {
	for _, line := range strings.Split(logs, "\n") {
		// A line may carry a pod prefix before its JSON record.
		start := strings.IndexByte(line, '{')
		if start < 0 {
			continue
		}
		record := []byte(line[start:])
		var rec struct {
			Time    string `json:"time"`
			Subject string `json:"subject"`
		}
		if json.Unmarshal(record, &rec) != nil {
			continue
		}
		stamp, err := time.Parse(time.RFC3339Nano, rec.Time)
		if err != nil || stamp.Before(since) {
			continue
		}
		switch {
		case strings.Contains(line, musterTokenAccepted) && strings.Contains(line, `"email":"`+email+`"`):
			accepted++
		case strings.Contains(line, musterToolCall):
			call := rec
			logged := strings.TrimSuffix(call.Subject, "...")
			subjects = append(subjects, call.Subject)
			if logged != "" && strings.HasPrefix(subject, logged) {
				calls++
			}
		}
	}
	return accepted, calls, subjects
}
