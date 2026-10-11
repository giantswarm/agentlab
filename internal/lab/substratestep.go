package lab

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The Substrate step proof: agents keep their conversation and their durable
// volume across a Substrate release that changes the snapshot layout.
//
// A Session's actor is suspended to a snapshot between turns, and every new
// Session starts from its template's golden snapshot. A Substrate release
// that lays a snapshot out differently (1.7 to 1.8: one tar per durable
// volume instead of one for all) must read what the release before it wrote:
// the suspended conversation is restored from the old layout, and the golden
// is taken again in the new one. The proof drives exactly that on the lab, in
// one run: an agent on the Substrate release the chart installs, a
// conversation of a few turns that writes a known file to the actor's durable
// directory, suspended; the step under it (the substrate and substrate-crds
// components pinned to the target release through a values overlay, dev
// images of the step's own, the meta chart upgraded in place); then the
// suspended conversation resumed — it must still know the earlier turns and
// read the file back — and a new conversation on the same agent, which must
// start from a golden taken again on the new release. The lab returns to the
// chart's Substrate afterwards. docs/platform.md "The Substrate step proof".

// What the proof creates and checks; all of it is removed by the same run.
const (
	substrateStepAgent = "agentlab-substrate-step"
	// substrateStepDir is the proof's directory in the actor's durable
	// directory (/data): what the snapshot carries as a durable volume.
	substrateStepDir  = "/data/agentlab-substrate-step"
	substrateStepFile = substrateStepDir + "/marker"
	// substrateStepValues is the step's values overlay, beside the lab's
	// rendered values.
	substrateStepValues = "substrate-step-values.yaml"
	// substrateCRDsRelease is the component that installs Substrate's CRDs,
	// pinned with the control plane.
	substrateCRDsRelease   = "substrate-crds"
	substrateStepSystemMsg = "You are a terse assistant of the agentlab Substrate step proof. Use your bash tool when asked to run a command. Answer in one short line."
)

// The waits of the proof: the golden boot and the re-take are a golden boot
// each (minutes on a cold worker), the workers' roll onto the new image a
// pod start per worker.
const (
	SubstrateStepReadyTimeout   = 10 * time.Minute
	SubstrateStepRetakeTimeout  = 10 * time.Minute
	substrateStepWorkersTimeout = 5 * time.Minute
	substrateStepPoll           = 5 * time.Second
)

// SubstrateStepOptions tunes the proof.
type SubstrateStepOptions struct {
	// To is the Substrate release the lab steps to: an exact version of the
	// Substrate line (1.8.0, 1.8.1-rc.4).
	To string
	// DevImages are platform.devImages entries for the step alone (the
	// ate-api-server build of a change not yet in a release); the lab before
	// the step runs the chart's images.
	DevImages map[string]string
	// ModelConfig is the kagent ModelConfig the agent runs on (default:
	// default-model-config).
	ModelConfig string
	// ReadyTimeout bounds the golden boot before the step, RetakeTimeout the
	// golden's re-take after it.
	ReadyTimeout, RetakeTimeout time.Duration
	// Stay leaves the lab on To instead of returning it to the chart's
	// Substrate.
	Stay bool
}

// SubstrateStepTest is `agentlab substrate-step-test`. The verdict is one
// PASS or FAIL line per step with its cause, under the summary of the
// stages; a failure returns the stage that failed.
func SubstrateStepTest(cfg *config.Config, email string, opts SubstrateStepOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled || !cfg.Platform.Agents || cfg.LegacyChart() {
		return fmt.Errorf("platform.agents is off in %s (or the chart is a 3.x one): the agents of the 4.x line run on Agent Substrate — enable them and run `agentlab platform` first", config.File)
	}
	if cfg.Platform.ChartBranch != "" && !cfg.Platform.ChartPinned {
		return fmt.Errorf("the lab follows the dev channel of %s, which a platform run in the middle of the proof would move: `agentlab platform --pin` first", cfg.Platform.ChartBranch)
	}
	to, err := semver.StrictNewVersion(strings.TrimPrefix(opts.To, "v"))
	if err != nil {
		return fmt.Errorf("--to %q: an exact Substrate release (1.8.0, 1.8.1-rc.4): %v", opts.To, err)
	}
	stepped, err := substrateStepConfig(cfg, to.String(), filepath.Join(StateDir, substrateStepValues), opts.DevImages)
	if err != nil {
		return err
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}
	if opts.ModelConfig == "" {
		opts.ModelConfig = defaultModelConfig
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = SubstrateStepReadyTimeout
	}
	if opts.RetakeTimeout <= 0 {
		opts.RetakeTimeout = SubstrateStepRetakeTimeout
	}

	run := &substrateStepRun{cfg: cfg, stepped: stepped, to: to, opts: opts, stages: newProofStages()}
	err = run.prove(user)
	// The way back runs on every exit once the step was taken, after the
	// proof's agent is gone: no snapshot of the proof is left to read.
	if run.stepTaken && !opts.Stay {
		if backErr := run.stepBack(); backErr != nil {
			err = errors.Join(err, backErr)
		}
	}
	err = run.stages.close(err)
	run.verdict(err)
	return err
}

// substrateStepRun is one run's state: the configs before and for the step,
// the versions read on either side, what the conversation holds.
type substrateStepRun struct {
	cfg, stepped *config.Config
	to           *semver.Version
	opts         SubstrateStepOptions
	stages       *proofStages

	before, after substrateRunning
	api           *kagentAPI
	session       string
	codeword      string
	marker        string
	goldenBefore  string
	stepTaken     bool
	lines         []string // the PASS lines, in step order
}

// substrateRunning is the Substrate a lab runs, as the proof names it: the
// atelet's, the workers' and ate-api-server's images and the release the
// atelet's tag is.
type substrateRunning struct {
	version, atelet, worker, api string
}

func (r substrateRunning) String() string {
	return fmt.Sprintf("Substrate %s (atelet %s, workers %s, ate-api-server %s)", r.version, r.atelet, r.worker, r.api)
}

func (r *substrateStepRun) pass(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	note("%s", line)
	r.lines = append(r.lines, line)
}

// prove runs the four steps up to the first failure.
func (r *substrateStepRun) prove(user *config.User) error {
	r.stages.begin("before", "Step 1: the Substrate the lab runs before the step")
	before, err := readSubstrateRelease(r.cfg)
	if err != nil {
		return err
	}
	r.before = before
	if err := stepFrom(before.version, r.to); err != nil {
		return err
	}
	r.pass("before the step: %s", before)

	token, err := passwordGrant(r.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	if r.api, err = dialKagentAPI(r.cfg, token); err != nil {
		return err
	}
	defer r.api.close()
	cleanup := func() {
		r.removeSession()
		for _, left := range substrateStepCleanup(r.api) {
			note("cleanup: still in Substrate: %s", left)
		}
	}
	cleanup()
	defer cleanup()

	r.stages.begin("agent", "Step 2: an agent on Substrate %s with a conversation of three turns and a known file on its durable volume, suspended", before.version)
	if err := r.seed(user.Email); err != nil {
		return err
	}

	r.stages.begin("step", "Step 3: the step to Substrate %s under the suspended conversation", r.to)
	if err := r.step(); err != nil {
		return err
	}

	r.stages.begin("resume", "Step 4a: the suspended conversation resumes on Substrate %s with its earlier turns and the file", r.after.version)
	if err := r.resume(); err != nil {
		return err
	}

	r.stages.begin("golden", "Step 4b: a new conversation on the same agent starts from a golden taken again on Substrate %s", r.after.version)
	if err := r.newConversation(); err != nil {
		return err
	}

	r.stages.begin("cleanup", "Removing %s", substrateStepAgent)
	r.removeSession()
	if left := substrateStepCleanup(r.api); len(left) > 0 {
		return fmt.Errorf("the deleted agent left this in Substrate: %s", strings.Join(left, "; "))
	}
	return nil
}

// seed boots the agent on the Substrate before the step, has three turns
// with it — a codeword, a file on the durable volume, the codeword asked
// back — and suspends the conversation, so its snapshot and the template's
// golden are in the layout of the release before the step.
func (r *substrateStepRun) seed(email string) error {
	if _, err := applyManifests(context.Background(), []byte(substrateStepTemplate(r.opts.ModelConfig))); err != nil {
		return err
	}
	boot, err := waitAgentReady(substrateStepAgent, r.opts.ReadyTimeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return fmt.Errorf("agent %s did not become Ready on Substrate %s within %s: %s", substrateStepAgent, r.before.version, r.opts.ReadyTimeout, boot.reason)
	}
	revision := boot.agent.Status.LatestSuccessfulRevision
	note("Ready after %s: revision %.12s", boot.elapsed.Round(time.Second), revision)

	ctx, cancel := context.WithTimeout(context.Background(), 3*kagentTurnTimeout)
	defer cancel()
	session, err := r.api.createSession(ctx, substrateStepAgent, uuid.NewString())
	if err != nil {
		return err
	}
	r.session = session.GetId()
	note("Session %s of %s as %s", r.session, substrateStepAgent, email)
	r.codeword = "step-" + randHex(4)
	r.marker = "substrate-step " + randHex(8)

	if _, err := r.turn(ctx, session, codewordPrompt(r.codeword)); err != nil {
		return fmt.Errorf("turn 1 (the codeword): %w", err)
	}
	reply, err := r.turn(ctx, session, writeMarkerPrompt(r.marker))
	if err != nil {
		return fmt.Errorf("turn 2 (the file): %w", err)
	}
	if !strings.Contains(reply, r.marker) {
		return fmt.Errorf("turn 2: the agent did not write %s (did it run its bash tool?): %s", substrateStepFile, excerpt(reply, 200))
	}
	reply, err = r.turn(ctx, session, recallPrompt)
	if err != nil {
		return fmt.Errorf("turn 3 (the codeword asked back): %w", err)
	}
	if !strings.Contains(reply, r.codeword) {
		return fmt.Errorf("turn 3: the agent did not answer the codeword %s of turn 1 before the step: %s", r.codeword, excerpt(reply, 200))
	}

	suspended, err := r.api.suspendSession(ctx, r.session)
	if err != nil {
		return err
	}
	if suspended, err = r.awaitSuspended(ctx, suspended); err != nil {
		return err
	}
	golden, err := r.golden(revision)
	if err != nil {
		return err
	}
	r.goldenBefore = golden
	r.pass("on %s: three turns (codeword %s, %s holding %q on the durable volume, the codeword answered back), Session %s %s, the template's %s",
		r.before.version, r.codeword, substrateStepFile, r.marker, r.session, sessionState(suspended), golden)
	return nil
}

// turn is one turn on the conversation, its answer or the error.
func (r *substrateStepRun) turn(ctx context.Context, session *apiv1alpha1.Session, prompt string) (string, error) {
	t, err := r.api.turn(ctx, session, userMessage(prompt))
	if err != nil {
		return "", err
	}
	reply, err := t.completedText()
	if err != nil {
		return "", err
	}
	note("answered: %s", excerpt(reply, 160))
	return reply, nil
}

// awaitSuspended polls the session until the controller reports it
// SUSPENDED: the snapshot is written.
func (r *substrateStepRun) awaitSuspended(ctx context.Context, session *apiv1alpha1.Session) (*apiv1alpha1.Session, error) {
	var err error
	suspended := waitFor(int(sessionReadyTimeout/pollInterval), pollInterval, func() bool {
		if session.GetState() == apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED {
			return true
		}
		session, err = r.api.getSession(ctx, r.session)
		return err != nil
	})
	if err != nil {
		return session, err
	}
	if !suspended {
		return session, fmt.Errorf("session %s is still %s %s after the suspend", r.session, sessionState(session), sessionReadyTimeout)
	}
	return session, nil
}

// golden is the golden snapshot of the agent's ActorTemplate of the given
// revision, worded as the controller reports it.
func (r *substrateStepRun) golden(revision string) (string, error) {
	f := skillsFootprint(r.api, substrateStepAgent)
	if f.err != nil {
		return "", f.err
	}
	t, err := templateOfRevision(f.templates, substrateStepAgent, revision)
	if err != nil {
		return "", err
	}
	if t.golden == "" {
		return "", fmt.Errorf("ActorTemplate %s/%s has no golden snapshot (phase %s%s)", t.namespace, t.name, t.phase, failureNote(t.failure))
	}
	return t.golden, nil
}

// step pins the substrate components to the target release (and the step's
// dev images) and upgrades the platform in place, then waits for the
// workers to run the new worker image.
func (r *substrateStepRun) step() error {
	values, err := substrateStepOverlay(r.to.String())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(StateDir, substrateStepValues), values, 0o600); err != nil {
		return err
	}
	note("the overlay %s pins %s and %s to %s%s", filepath.Join(StateDir, substrateStepValues), substrateCRDsRelease, substrateRelease, r.to, devImagesNote(r.opts.DevImages))
	r.stepTaken = true
	if err := PlatformUp(r.stepped, Offers{}); err != nil {
		return fmt.Errorf("the platform upgrade to Substrate %s: %w", r.to, err)
	}
	after, err := readSubstrateRelease(r.stepped)
	if err != nil {
		return err
	}
	if after.version != r.to.String() {
		return fmt.Errorf("the platform upgrade finished, but the atelet runs %s, not %s: %s", after.version, r.to, after)
	}
	if err := waitWorkersOn(after.worker, substrateStepWorkersTimeout); err != nil {
		return err
	}
	r.after = after
	r.pass("after the step: %s", after)
	return nil
}

// resume resumes the suspended conversation and asks it for the codeword of
// its first turn and the file it wrote.
func (r *substrateStepRun) resume() error {
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout+sessionReadyTimeout)
	defer cancel()
	session, err := r.api.resumeSession(ctx, r.session)
	if err != nil {
		return fmt.Errorf("%s: %w", r.restoreCause(session), err)
	}
	note("Session %s resumed: %s", r.session, sessionState(session))
	reply, err := r.turn(ctx, session, resumePrompt)
	if err != nil {
		return fmt.Errorf("the turn after the resume: %w (%s)", err, r.restoreCause(nil))
	}
	conversation, volume := resumedReplyProves(reply, r.codeword, r.marker)
	if !conversation {
		return fmt.Errorf("the resumed conversation lost its earlier turns: the agent did not answer the codeword %s of turn 1: %s", r.codeword, excerpt(reply, 200))
	}
	if !volume {
		return fmt.Errorf("the durable volume lost its file: %s does not hold %q after the resume: %s", substrateStepFile, r.marker, excerpt(reply, 200))
	}
	r.pass("resumed conversation: Session %s answered the codeword %s of its first turn on %s", r.session, r.codeword, r.after.version)
	r.pass("volume check: %s holds %q, written on %s, read on %s", substrateStepFile, r.marker, r.before.version, r.after.version)
	return nil
}

// restoreCause words why a resume failed, from the session's failure and the
// crash messages of the agent's actors in ate-api; the restore refusal of a
// snapshot in the layout before the per-volume one is named as such.
func (r *substrateStepRun) restoreCause(session *apiv1alpha1.Session) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if session == nil || session.GetId() == "" {
		if got, err := r.api.getSession(ctx, r.session); err == nil {
			session = got
		}
	}
	var evidence []string
	if f := session.GetFailure(); f.GetReason() != "" || f.GetMessage() != "" {
		evidence = append(evidence, fmt.Sprintf("session %s %s: %s %s", r.session, sessionState(session), f.GetReason(), f.GetMessage()))
	}
	evidence = append(evidence, actorCrashes(ctx, substrateStepAgent)...)
	return restoreVerdict(evidence)
}

// actorCrashes is the state and crash message of each actor of the agent's
// ActorTemplates, read from ate-api; a read that fails says so.
func actorCrashes(ctx context.Context, agent string) []string {
	api, err := dialAteAPI(ctx)
	if err != nil {
		return []string{"ate-api: " + err.Error()}
	}
	defer api.Close()
	actors, err := agentAteActors(ctx, api, agent)
	lines := make([]string, 0, len(actors)+1)
	for _, actor := range actors {
		lines = append(lines, actorLine(actor))
	}
	if err != nil {
		lines = append(lines, "ate-api: "+err.Error())
	}
	return lines
}

// actorLine words an actor's state and crash message.
func actorLine(actor *ateapi.Actor) string {
	line := fmt.Sprintf("actor %s: %s", actor.GetMetadata().GetName(), strings.TrimPrefix(actor.GetStatus().GetState().String(), "ACTOR_STATE_"))
	if msg := actor.GetStatus().GetCrash().GetMessage(); msg != "" {
		line += ", crash: " + msg
	}
	return line
}

// agentAteActors is every actor in ate-api of the agent's ActorTemplates,
// all pages.
func agentAteActors(ctx context.Context, api *ateAPI, agent string) ([]*ateapi.Actor, error) {
	prefix := actorTemplatePrefix(agent)
	var actors []*ateapi.Actor
	token := ""
	for range substrateMaxPages {
		resp, err := api.ListActors(ctx, &ateapi.ListActorsRequest{Atespace: kagentNamespace, PageToken: token})
		if err != nil {
			return actors, fmt.Errorf("ListActors: %w", err)
		}
		for _, actor := range resp.GetActors() {
			if isActorTemplateOf(prefix, actor.GetActorTemplate().GetName()) {
				actors = append(actors, actor)
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			return actors, nil
		}
	}
	return actors, fmt.Errorf("ListActors: still paging after %d pages", substrateMaxPages)
}

// agentAteTemplates is every ActorTemplate in ate-api of the agent, all
// pages.
func agentAteTemplates(ctx context.Context, api *ateAPI, agent string) ([]*ateapi.ActorTemplate, error) {
	prefix := actorTemplatePrefix(agent)
	var templates []*ateapi.ActorTemplate
	token := ""
	for range substrateMaxPages {
		resp, err := api.ListActorTemplates(ctx, &ateapi.ListActorTemplatesRequest{Atespace: kagentNamespace, PageToken: token})
		if err != nil {
			return templates, fmt.Errorf("ListActorTemplates: %w", err)
		}
		for _, t := range resp.GetActorTemplates() {
			if isActorTemplateOf(prefix, t.GetMetadata().GetName()) {
				templates = append(templates, t)
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			return templates, nil
		}
	}
	return templates, fmt.Errorf("ListActorTemplates: still paging after %d pages", substrateMaxPages)
}

// substrateStepCleanup removes the proof's agent through kagent and, when
// kagent leaves something in Substrate — an actor that crashed on a restore
// and its template, which kagent's revision garbage collector keeps —
// deletes that in ate-api itself and reads the footprint again. Returns what
// is still there, worded.
func substrateStepCleanup(api *kagentAPI) []string {
	left := proofAgentsCleanup(api, substrateStepAgent)
	if len(left) == 0 {
		return nil
	}
	left = removeAteLeftovers(substrateStepAgent)
	if f := skillsFootprint(api, substrateStepAgent); !f.empty() {
		left = append(left, f.lines()...)
	}
	return left
}

// removeAteLeftovers deletes, in ate-api itself, every actor (in any state)
// and ActorTemplate of the agent. Returns what could not be removed, worded.
func removeAteLeftovers(agent string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	api, err := dialAteAPI(ctx)
	if err != nil {
		return []string{"ate-api: " + err.Error()}
	}
	defer api.Close()
	var left []string
	actors, err := agentAteActors(ctx, api, agent)
	if err != nil {
		left = append(left, err.Error())
	}
	for _, actor := range actors {
		note("cleanup: %s is still in ate-api — deleting it there", actorLine(actor))
		ref := &ateapi.ObjectRef{Atespace: kagentNamespace, Name: actor.GetMetadata().GetName()}
		if _, err := api.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref, AnyState: true}); err != nil && status.Code(err) != codes.NotFound {
			left = append(left, fmt.Sprintf("actor %s: %v", ref.GetName(), err))
		}
	}
	templates, err := agentAteTemplates(ctx, api, agent)
	if err != nil {
		left = append(left, err.Error())
	}
	for _, t := range templates {
		ref := &ateapi.ObjectRef{Atespace: kagentNamespace, Name: t.GetMetadata().GetName()}
		note("cleanup: ActorTemplate %s/%s is still in ate-api — deleting it there", ref.GetAtespace(), ref.GetName())
		if _, err := api.DeleteActorTemplate(ctx, &ateapi.DeleteActorTemplateRequest{ActorTemplate: ref}); err != nil && status.Code(err) != codes.NotFound {
			left = append(left, fmt.Sprintf("ActorTemplate %s: %v", ref.GetName(), err))
		}
	}
	return left
}

// newConversation waits for the template's golden to be taken again on the
// new release, then has one turn on a new conversation of the same agent.
func (r *substrateStepRun) newConversation() error {
	revision := ""
	if agent, err := readAgent(substrateStepAgent); err == nil {
		revision = agent.Status.LatestSuccessfulRevision
	}
	var golden string
	var readErr error
	retaken := waitFor(int(r.opts.RetakeTimeout/substrateStepPoll), substrateStepPoll, func() bool {
		golden, readErr = r.golden(revision)
		return readErr == nil && golden != r.goldenBefore
	})
	if !retaken {
		cause := fmt.Sprintf("still %s, taken on %s", r.goldenBefore, r.before.version)
		if readErr != nil {
			cause = readErr.Error()
		}
		return fmt.Errorf("the golden was not taken again on Substrate %s within %s (%s): a new conversation would start from the snapshot of the release before", r.after.version, r.opts.RetakeTimeout, cause)
	}
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	defer cancel()
	session, err := r.api.createSession(ctx, substrateStepAgent, uuid.NewString())
	if err != nil {
		return err
	}
	defer r.api.removeSession(session.GetId())
	reply, err := r.turn(ctx, session, newConversationPrompt)
	if err != nil {
		return fmt.Errorf("the first turn of a new conversation: %w (%s)", err, restoreVerdict(actorCrashes(ctx, substrateStepAgent)))
	}
	if !strings.Contains(strings.ToUpper(reply), newConversationAnswer) {
		return fmt.Errorf("the new conversation answered %q, not %s", excerpt(reply, 200), newConversationAnswer)
	}
	r.pass("new conversation: Session %s answered on the golden taken again on %s (%s, was %s)", session.GetId(), r.after.version, golden, r.goldenBefore)
	return nil
}

// removeSession deletes the proof's conversation, once.
func (r *substrateStepRun) removeSession() {
	if r.session != "" {
		r.api.removeSession(r.session)
		r.session = ""
	}
}

// stepBack returns the lab to the Substrate the chart installs.
func (r *substrateStepRun) stepBack() error {
	step("Returning the lab to the chart's Substrate (%s before the step)", r.before.version)
	_ = os.Remove(filepath.Join(StateDir, substrateStepValues))
	if err := PlatformUp(r.cfg, Offers{}); err != nil {
		return fmt.Errorf("returning to the chart's Substrate: %w (the lab still runs the step's: `agentlab platform`)", err)
	}
	back, err := readSubstrateRelease(r.cfg)
	if err != nil {
		return err
	}
	note("back on %s", back)
	return nil
}

// verdict prints the PASS lines of the steps that passed and the FAIL line
// with the cause.
func (r *substrateStepRun) verdict(err error) {
	fmt.Println()
	for _, line := range r.lines {
		fmt.Printf("PASS: %s\n", line)
	}
	if err != nil {
		fmt.Printf("FAIL: %v\n", err)
	}
}

// readSubstrateRelease reads the Substrate the lab runs: the atelet's image
// (whose tag is the release), the worker image of the WorkerPool (which must
// be of the same release, proveSubstrateLine) and ate-api-server's image.
func readSubstrateRelease(cfg *config.Config) (substrateRunning, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	pools, err := proveSubstrateLine(ctx, substrateSkewRemedy(cfg))
	if err != nil {
		return substrateRunning{}, err
	}
	gvr, err := gvrFor("deployments.apps")
	if err != nil {
		return substrateRunning{}, err
	}
	deploy, err := getObject(ctx, gvr, substrateNamespace, substrateAPIDeployment)
	if err != nil {
		return substrateRunning{}, err
	}
	version, err := imageTag(pools[0].atelet)
	if err != nil {
		return substrateRunning{}, err
	}
	return substrateRunning{version: version, atelet: pools[0].atelet, worker: pools[0].worker, api: firstContainerImage(deploy)}, nil
}

// imageTag is an image's tag as a version (the atelet's is its release).
func imageTag(image string) (string, error) {
	ref, _, _ := strings.Cut(image, "@")
	name := ref[strings.LastIndex(ref, "/")+1:]
	_, tag, ok := strings.Cut(name, ":")
	if !ok || tag == "" {
		return "", fmt.Errorf("image %s names no tag", image)
	}
	v, err := semver.NewVersion(tag)
	if err != nil {
		return "", fmt.Errorf("image %s: tag %q is not a version (%v)", image, tag, err)
	}
	return v.String(), nil
}

// stepFrom refuses a step that is none: the lab must run an older Substrate
// release (major.minor) than the one it steps to.
func stepFrom(before string, to *semver.Version) error {
	from, err := semver.NewVersion(before)
	if err != nil {
		return fmt.Errorf("the lab's Substrate %q is not a version: %v", before, err)
	}
	if from.Major() > to.Major() || (from.Major() == to.Major() && from.Minor() >= to.Minor()) {
		return fmt.Errorf("the lab runs Substrate %s, and %s is no step to a newer release: the proof starts on the release before the step — the chart's Substrate range as released (drop any platform.valuesFiles overlay that pins substrate, then `agentlab platform`)", before, to)
	}
	return nil
}

// substrateStepOverlay is the step's values overlay: the substrate and
// substrate-crds components pinned to the exact release, which moves the
// control plane, the atelet and the WorkerPool's worker image (derived from
// the range's floor) together.
func substrateStepOverlay(to string) ([]byte, error) {
	pin := map[string]any{"versionRange": to}
	return yaml.Marshal(map[string]any{"components": map[string]any{
		substrateCRDsRelease: pin,
		substrateRelease:     pin,
	}})
}

// substrateStepConfig is the lab's configuration for the step: the overlay
// after the lab's own values files, the step's dev images over the lab's.
// The lab's configuration is left as it is — agentlab.yaml is never written
// with the step in it — and the result is validated like agentlab.yaml.
func substrateStepConfig(cfg *config.Config, to, overlay string, devImages map[string]string) (*config.Config, error) {
	stepped := *cfg
	stepped.Platform.ValuesFiles = append(slices.Clone(cfg.Platform.ValuesFiles), overlay)
	stepped.Platform.DevImages = maps.Clone(cfg.Platform.DevImages)
	if len(devImages) > 0 && stepped.Platform.DevImages == nil {
		stepped.Platform.DevImages = map[string]string{}
	}
	maps.Copy(stepped.Platform.DevImages, devImages)
	if err := stepped.Validate(); err != nil {
		return nil, fmt.Errorf("the step to Substrate %s: %w", to, err)
	}
	return &stepped, nil
}

// devImagesNote words the step's dev images for the step line.
func devImagesNote(devImages map[string]string) string {
	if len(devImages) == 0 {
		return ""
	}
	var pairs []string
	for _, target := range slices.Sorted(maps.Keys(devImages)) {
		pairs = append(pairs, target+"="+devImages[target])
	}
	return ", with the dev images " + strings.Join(pairs, ", ")
}

// waitWorkersOn waits for every worker pod of the pools to run the image and
// be Ready: the WorkerPool rolls its workers onto a new worker image.
func waitWorkersOn(image string, timeout time.Duration) error {
	gvr := gvrPods
	var off []string
	var err error
	rolled := waitFor(int(timeout/substrateStepPoll), substrateStepPoll, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		var pods []unstructured.Unstructured
		if pods, err = listObjects(ctx, gvr, kagentNamespace, substrateWorkerLabel); err != nil {
			return false
		}
		off = workersOff(pods, image)
		return len(off) == 0
	})
	if rolled {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("the workers did not roll onto %s within %s: %s", image, timeout, strings.Join(off, "; "))
}

// podContainersKey is a pod spec's list of containers.
const podContainersKey = "containers"

// workersOff words each worker pod that does not run the image, or is not
// Ready yet; none at all is one entry too.
func workersOff(pods []unstructured.Unstructured, image string) []string {
	if len(pods) == 0 {
		return []string{"no worker pod in " + kagentNamespace}
	}
	var off []string
	for _, pod := range pods {
		containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", podContainersKey)
		var images []string
		on := false
		for _, c := range containers {
			container, _ := c.(map[string]any)
			img, _ := container[imageKey].(string)
			images = append(images, img)
			on = on || img == image
		}
		conditions, _, _ := unstructured.NestedSlice(pod.Object, crStatus, crConditions)
		ready, _ := readyConditionOf(conditions)
		switch {
		case pod.GetDeletionTimestamp() != nil:
			off = append(off, pod.GetName()+" is terminating")
		case !on:
			off = append(off, fmt.Sprintf("%s runs %s", pod.GetName(), strings.Join(images, ", ")))
		case !ready:
			off = append(off, pod.GetName()+" is not Ready")
		}
	}
	return off
}

// templateOfRevision is the agent's ActorTemplate of the revision (kagent
// names it <agent>-<the revision's first 12 characters>); without a revision
// to match, the agent's one template.
func templateOfRevision(templates []substrateTemplate, agent, revision string) (substrateTemplate, error) {
	if len(revision) >= revisionSuffixSize {
		want := actorTemplatePrefix(agent) + strings.ToLower(revision[:revisionSuffixSize])
		for _, t := range templates {
			if t.name == want {
				return t, nil
			}
		}
		return substrateTemplate{}, fmt.Errorf("no ActorTemplate %s in Substrate (%d of the agent)", want, len(templates))
	}
	if len(templates) == 1 {
		return templates[0], nil
	}
	return substrateTemplate{}, fmt.Errorf("agent %s has %d ActorTemplates in Substrate and no revision to tell them apart", agent, len(templates))
}

// failureNote words a template's failure for a line, "" without one.
func failureNote(failure string) string {
	if failure == "" {
		return ""
	}
	return ", error: " + failure
}

// The restore refusal of Substrate 1.8.0: a snapshot in the layout before the
// per-volume one is refused instead of restored (FailedPrecondition, the
// actor crashed with the message).
const legacyLayoutRefusal = "written before the per-volume layout"

// restoreVerdict words the evidence of a failed resume as its cause: the
// restore refusal of the old layout when a line carries it, else the
// evidence as it is.
func restoreVerdict(evidence []string) string {
	if len(evidence) == 0 {
		return "no failure recorded on the session or its actors"
	}
	for _, line := range evidence {
		if strings.Contains(line, legacyLayoutRefusal) {
			return "the restore refusal: Substrate refuses the snapshot written in the layout before the per-volume one instead of converting it (" + line + ")"
		}
	}
	return strings.Join(evidence, "; ")
}

// The turns. The codeword and the marker are random per run, so no earlier
// run's answer can pass.
const (
	recallPrompt          = "What is the codeword I gave you at the start of our conversation? Reply with the codeword only."
	newConversationAnswer = "PONG"
	newConversationPrompt = "Reply with the single word " + newConversationAnswer + "."
)

var resumePrompt = "What is the codeword I gave you at the start of our conversation? Then run this exact command once with your bash tool: cat " + substrateStepFile +
	" — and reply in one line: the codeword, then the command's output verbatim."

func codewordPrompt(codeword string) string {
	return "Remember this codeword for later in our conversation: " + codeword + ". Reply with OK only."
}

func writeMarkerPrompt(marker string) string {
	return fmt.Sprintf("Run this exact command once with your bash tool, then reply with its output verbatim and nothing else:\n\nmkdir -p %s && printf '%%s\\n' '%s' > %s && cat %s",
		substrateStepDir, marker, substrateStepFile, substrateStepFile)
}

// resumedReplyProves judges the turn after the resume: the codeword of the
// first turn (the conversation) and the file's content (the volume).
func resumedReplyProves(reply, codeword, marker string) (conversation, volume bool) {
	return strings.Contains(reply, codeword), strings.Contains(reply, marker)
}

// substrateStepTemplate is the proof's AgentTemplate on the platform
// Harness and its Agent. The template carries the skills proof's public
// skill for its side effect: the Go ADK gives an agent its execution tools —
// bash, which writes and reads the file on the durable volume — only with
// skills.
func substrateStepTemplate(modelConfig string) string {
	return fmt.Sprintf(`apiVersion: %[1]s
kind: AgentTemplate
metadata:
  name: %[2]s
  namespace: %[3]s
  labels:
    %[4]s: %[5]s
spec:
  description: "Throwaway agent of agentlab substrate-step-test; deleted by the same run."
  modelConfig:
    name: %[6]s
  systemPrompt: %[7]q
  skills:
    - name: %[8]s
      source:
        git:
          url: %[9]s
          commit: %[10]s
        path: %[8]s
---
%[11]s`, agentTemplateAPIVersion, substrateStepAgent, kagentNamespace, managedByLabel, managedByAgentlabValue, modelConfig, substrateStepSystemMsg,
		skillsTestSkill, skillsTestRepo, skillsTestCommit,
		agentManifest(substrateStepAgent, substrateStepAgent, kagentHarness, nil))
}
