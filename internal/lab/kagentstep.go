package lab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The kagent step proof: an agent and its conversation survive a step of the
// kagent line under them. An agent on the kagent release the lab runs, a
// conversation with one turn (a codeword), suspended; the step (the kagent
// and kagent-crds components pinned to a kagent release through a values
// overlay, the meta chart moved to a candidate release, or both, the platform
// upgraded in place); then the conversation from before the step resumed, a
// turn on it that must answer the codeword, a suspend and a resume more, and
// the conversation and the agent deleted. The lab returns to its own
// configuration afterwards. docs/platform.md "The kagent step proof".

// What the proof creates; all of it is removed by the same run.
const (
	kagentStepAgent = "agentlab-kagent-step"
	// kagentStepValues is the step's values overlay, beside the lab's
	// rendered values.
	kagentStepValues    = "kagent-step-values.yaml"
	kagentCRDsRelease   = componentKagent + "-crds"
	kagentStepSystemMsg = "You are a terse assistant of the agentlab kagent step proof. Answer in one short line."
)

// The waits of the proof: the golden boot before the step, the controller's
// roll onto the stepped release.
const (
	KagentStepReadyTimeout     = 10 * time.Minute
	kagentStepControllerTimout = 5 * time.Minute
)

// KagentStepOptions tunes the proof. To, Chart or both name the step.
type KagentStepOptions struct {
	// To is the kagent release the lab steps to: an exact version of the
	// kagent line (1.14.0, 1.15.1-rc.2), pinned on the kagent and kagent-crds
	// components.
	To string
	// Chart is the agent-platform release the lab steps to (4.145.1-rc.6): a
	// candidate whose kagent range moves.
	Chart string
	// DevImages are platform.devImages entries for the step alone (a kagent
	// controller build of a change not yet in a release).
	DevImages map[string]string
	// ModelConfig is the kagent ModelConfig the agent runs on (default:
	// default-model-config).
	ModelConfig string
	// ReadyTimeout bounds the agent's golden boot before the step.
	ReadyTimeout time.Duration
	// Stay leaves the lab on the step instead of returning it to its own
	// configuration.
	Stay bool
}

// KagentStepTest is `agentlab kagent-step-test`. The verdict is one PASS line
// per step and, on a failure, one FAIL line naming what failed — the step,
// the resume, the turn, the suspend or the delete — with its cause.
func KagentStepTest(cfg *config.Config, email string, opts KagentStepOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled || !cfg.Platform.Agents || cfg.LegacyChart() {
		return fmt.Errorf("platform.agents is off in %s (or the chart is a 3.x one): the proof steps the kagent line of the 4.x chart — enable them and run `agentlab platform` first", config.File)
	}
	stepped, err := kagentStepConfig(cfg, &opts)
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
		opts.ReadyTimeout = KagentStepReadyTimeout
	}

	run := &kagentStepRun{cfg: cfg, stepped: stepped, opts: opts, stages: newProofStages()}
	err = run.prove(user)
	if run.stepTaken && !opts.Stay {
		if backErr := run.stepBack(); backErr != nil {
			err = errors.Join(err, backErr)
		}
	}
	err = run.stages.close(err)
	run.verdict(err)
	return err
}

// kagentStepConfig validates the step's options and returns the lab's
// configuration for the step: the overlay that pins the kagent release, the
// candidate chart, the step's dev images. opts.To and opts.Chart come back
// normalized.
func kagentStepConfig(cfg *config.Config, opts *KagentStepOptions) (*config.Config, error) {
	if opts.To == "" && opts.Chart == "" {
		return nil, errors.New("name the step: --to <kagent release>, --chart <agent-platform release>, or both")
	}
	overlay := ""
	if opts.To != "" {
		to, err := semver.StrictNewVersion(strings.TrimPrefix(opts.To, "v"))
		if err != nil {
			return nil, fmt.Errorf("--to %q: an exact kagent release (1.14.0, 1.15.1-rc.2): %v", opts.To, err)
		}
		opts.To = to.String()
		overlay = filepath.Join(StateDir, kagentStepValues)
	}
	if opts.Chart != "" {
		chart, err := semver.StrictNewVersion(strings.TrimPrefix(opts.Chart, "v"))
		if err != nil {
			return nil, fmt.Errorf("--chart %q: an exact agent-platform release (4.145.1-rc.6): %v", opts.Chart, err)
		}
		if cfg.Platform.ChartPath != "" || cfg.Platform.ChartBranch != "" {
			return nil, fmt.Errorf("--chart steps a released chart, and the lab installs %s: drop platform.chartPath and platform.chartBranch first (`agentlab configure --released`)", platformChartFor(cfg))
		}
		opts.Chart = chart.String()
	} else if cfg.Platform.ChartBranch != "" && !cfg.Platform.ChartPinned {
		return nil, fmt.Errorf("the lab follows the dev channel of %s, which a platform run in the middle of the proof would move: `agentlab platform --pin` first", cfg.Platform.ChartBranch)
	}
	stepped, err := steppedConfig(cfg, overlay, opts.DevImages)
	if err != nil {
		return nil, fmt.Errorf("the kagent step: %w", err)
	}
	if opts.Chart != "" {
		stepped.Platform.ChartVersion = opts.Chart
	}
	return stepped, nil
}

// kagentStepRun is one run's state.
type kagentStepRun struct {
	cfg, stepped *config.Config
	opts         KagentStepOptions
	stages       *proofStages

	before, after kagentRunning
	api           *kagentAPI
	session       string
	codeword      string
	stepTaken     bool
	lines         []string
}

// kagentRunning is the kagent line a lab runs, as the proof names it: the
// meta chart, the kagent chart its HelmRelease applied and the controller's
// image.
type kagentRunning struct {
	chart, kagent, controller string
}

func (k kagentRunning) String() string {
	return fmt.Sprintf("kagent %s (controller %s) on agent-platform %s", k.kagent, k.controller, k.chart)
}

func (r *kagentStepRun) pass(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	note("%s", line)
	r.lines = append(r.lines, line)
}

// prove runs the steps up to the first failure. Every stage's error is the
// cause; proofStages.close prefixes the stage's name, which is what failed.
func (r *kagentStepRun) prove(user *config.User) error {
	r.stages.begin("before", "Step 1: the kagent line the lab runs before the step")
	before, err := readKagentRunning()
	if err != nil {
		return err
	}
	r.before = before
	if err := r.isStep(); err != nil {
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
	for _, left := range proofAgentsCleanup(r.api, kagentStepAgent) {
		note("cleanup: an earlier run left this in Substrate, the proof goes on beside it: %s", left)
	}
	defer func() {
		r.removeSession()
		for _, left := range proofAgentsCleanup(r.api, kagentStepAgent) {
			note("cleanup: still in Substrate: %s", left)
		}
	}()

	r.stages.begin("agent", "Step 2: an agent on kagent %s with a conversation of one turn, suspended", before.kagent)
	if err := r.seed(user.Email); err != nil {
		return err
	}

	r.stages.begin("step", "Step 3: the step to %s under the suspended conversation", r.target())
	if err := r.step(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*(kagentTurnTimeout+sessionReadyTimeout))
	defer cancel()

	r.stages.begin("resume", "Step 4: the conversation from before the step resumes on kagent %s", r.after.kagent)
	session, err := r.api.resumeSession(ctx, r.session)
	if err != nil {
		return fmt.Errorf("the session %s did not resume on kagent %s: %w", r.session, r.after.kagent, err)
	}
	r.pass("resume: Session %s from kagent %s is %s on kagent %s", r.session, r.before.kagent, sessionState(session), r.after.kagent)

	r.stages.begin("turn", "Step 5: a turn on the resumed conversation answers the codeword of its first turn")
	reply, err := r.turn(ctx, session, recallPrompt)
	if err != nil {
		return fmt.Errorf("the turn after the resume: %w", err)
	}
	if !strings.Contains(reply, r.codeword) {
		return fmt.Errorf("the resumed conversation lost its earlier turn: the agent did not answer the codeword %s: %s", r.codeword, excerpt(reply, 200))
	}
	r.pass("turn: Session %s answered the codeword %s of its turn on kagent %s", r.session, r.codeword, r.before.kagent)

	r.stages.begin("suspend", "Step 6: the conversation suspends and resumes again on kagent %s", r.after.kagent)
	if err := r.suspendResume(ctx); err != nil {
		return err
	}

	r.stages.begin("delete", "Step 7: the conversation and the agent are deleted")
	return r.delete(ctx)
}

// isStep refuses a run that steps nothing: a --to that is not newer than the
// kagent the lab runs, a --chart that is the chart in place.
func (r *kagentStepRun) isStep() error {
	if r.opts.To != "" {
		from, err := semver.NewVersion(r.before.kagent)
		if err != nil {
			return fmt.Errorf("the lab's kagent chart %q is not a version: %v", r.before.kagent, err)
		}
		if to := semver.MustParse(r.opts.To); !from.LessThan(to) {
			return fmt.Errorf("the lab runs kagent %s, and %s is no step to a newer release: the proof starts on the release before the step — the chart's kagent range as released (drop any platform.valuesFiles overlay that pins kagent, then `agentlab platform`)", r.before.kagent, to)
		}
	}
	if r.opts.Chart != "" && r.opts.Chart == r.before.chart {
		return fmt.Errorf("the lab runs agent-platform %s already: --chart names the candidate to step to", r.opts.Chart)
	}
	return nil
}

// target words what the lab steps to.
func (r *kagentStepRun) target() string {
	var parts []string
	if r.opts.Chart != "" {
		parts = append(parts, "agent-platform "+r.opts.Chart)
	}
	if r.opts.To != "" {
		parts = append(parts, "kagent "+r.opts.To)
	}
	return strings.Join(parts, " with ") + devImagesNote(r.opts.DevImages)
}

// seed boots the agent, has one turn with it — a codeword — and suspends the
// conversation.
func (r *kagentStepRun) seed(email string) error {
	if _, err := applyManifests(context.Background(), []byte(kagentStepTemplate(r.opts.ModelConfig))); err != nil {
		return err
	}
	boot, err := waitAgentReady(kagentStepAgent, r.opts.ReadyTimeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return fmt.Errorf("agent %s did not become Ready on kagent %s within %s: %s", kagentStepAgent, r.before.kagent, r.opts.ReadyTimeout, boot.reason)
	}
	note("Ready after %s: revision %.12s", boot.elapsed.Round(time.Second), boot.agent.Status.LatestSuccessfulRevision)

	ctx, cancel := context.WithTimeout(context.Background(), 2*kagentTurnTimeout)
	defer cancel()
	session, err := r.api.createSession(ctx, kagentStepAgent, uuid.NewString())
	if err != nil {
		return err
	}
	r.session = session.GetId()
	note("Session %s of %s as %s", r.session, kagentStepAgent, email)
	r.codeword = "kstep-" + randHex(4)
	if _, err := r.turn(ctx, session, codewordPrompt(r.codeword)); err != nil {
		return fmt.Errorf("the turn before the step: %w", err)
	}
	suspended, err := r.api.suspendSession(ctx, r.session)
	if err != nil {
		return err
	}
	if suspended, err = awaitSessionSuspended(ctx, r.api, suspended); err != nil {
		return err
	}
	r.pass("on kagent %s: Session %s took a turn (codeword %s) and is %s", r.before.kagent, r.session, r.codeword, sessionState(suspended))
	return nil
}

// turn is one turn on the conversation, its answer or the error.
func (r *kagentStepRun) turn(ctx context.Context, session *apiv1alpha1.Session, prompt string) (string, error) {
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

// step writes the overlay (with --to), upgrades the platform onto the step's
// configuration and waits for the controller to run the stepped release.
func (r *kagentStepRun) step() error {
	if r.opts.To != "" {
		values, err := kagentStepOverlay(r.opts.To)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(StateDir, 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(StateDir, kagentStepValues), values, 0o600); err != nil {
			return err
		}
		note("the overlay %s pins %s and %s to %s", filepath.Join(StateDir, kagentStepValues), kagentCRDsRelease, componentKagent, r.opts.To)
	}
	r.stepTaken = true
	if err := PlatformUp(r.stepped, Offers{}); err != nil {
		return fmt.Errorf("the platform upgrade to %s: %w", r.target(), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), kagentStepControllerTimout+time.Minute)
	defer cancel()
	if err := waitDeploymentRolledOut(ctx, kagentNamespace, kagentControllerName, kagentStepControllerTimout); err != nil {
		return err
	}
	after, err := readKagentRunning()
	if err != nil {
		return err
	}
	r.after = after
	if err := kagentStepped(r.before, after, r.opts); err != nil {
		return err
	}
	if r.chartOnly() {
		r.pass("after the step: %s, a chart-only step that keeps kagent", after)
		return nil
	}
	r.pass("after the step: %s", after)
	return nil
}

// kagentStepped judges the lab after the upgrade: the kagent chart is the
// one --to names, without one never older than before (a candidate that keeps
// kagent is a chart-only step, its conversations proven all the same); the
// meta chart the one --chart names.
func kagentStepped(before, after kagentRunning, opts KagentStepOptions) error {
	if opts.Chart != "" && after.chart != opts.Chart {
		return fmt.Errorf("the platform upgrade finished, but the lab runs agent-platform %s, not %s", after.chart, opts.Chart)
	}
	if opts.To != "" {
		// The HelmRelease applies the chart with its build metadata
		// (1.14.0+3177c6907334), which a version comparison ignores.
		if got, err := semver.NewVersion(after.kagent); err != nil || !got.Equal(semver.MustParse(opts.To)) {
			return fmt.Errorf("the platform upgrade finished, but the kagent HelmRelease applied %s, not %s: %s", after.kagent, opts.To, after)
		}
		return nil
	}
	from, errFrom := semver.NewVersion(before.kagent)
	to, errTo := semver.NewVersion(after.kagent)
	if errFrom != nil || errTo != nil {
		return fmt.Errorf("the kagent HelmRelease's chart is not a version: %s before, %s after", before.kagent, after.kagent)
	}
	if to.LessThan(from) {
		return fmt.Errorf("agent-platform %s steps kagent back: kagent %s before, %s after", after.chart, before.kagent, after.kagent)
	}
	return nil
}

// chartOnly reports a step that moved the meta chart and kept kagent.
func (r *kagentStepRun) chartOnly() bool {
	if r.opts.To != "" {
		return false
	}
	from, errFrom := semver.NewVersion(r.before.kagent)
	to, errTo := semver.NewVersion(r.after.kagent)
	return errFrom == nil && errTo == nil && from.Equal(to)
}

// suspendResume suspends the resumed conversation and resumes it once more:
// the stepped controller's own suspend and restore.
func (r *kagentStepRun) suspendResume(ctx context.Context) error {
	suspended, err := r.api.suspendSession(ctx, r.session)
	if err != nil {
		return fmt.Errorf("the suspend on kagent %s: %w", r.after.kagent, err)
	}
	if suspended, err = awaitSessionSuspended(ctx, r.api, suspended); err != nil {
		return fmt.Errorf("the suspend on kagent %s: %w", r.after.kagent, err)
	}
	resumed, err := r.api.resumeSession(ctx, r.session)
	if err != nil {
		return fmt.Errorf("the resume after the suspend on kagent %s: %w", r.after.kagent, err)
	}
	r.pass("suspend: Session %s %s and %s again on kagent %s", r.session, sessionState(suspended), sessionState(resumed), r.after.kagent)
	return nil
}

// delete deletes the conversation, which must then be gone, and the agent,
// which must leave nothing in Substrate.
func (r *kagentStepRun) delete(ctx context.Context) error {
	id := r.session
	if err := r.api.deleteSession(ctx, id); err != nil {
		return fmt.Errorf("deleting Session %s on kagent %s: %w", id, r.after.kagent, err)
	}
	r.session = ""
	if _, err := r.api.getSession(ctx, id); err == nil {
		return fmt.Errorf("the session %s is still there after its delete on kagent %s", id, r.after.kagent)
	}
	if left := proofAgentsCleanup(r.api, kagentStepAgent); len(left) > 0 {
		return fmt.Errorf("the deleted agent left this in Substrate on kagent %s: %s", r.after.kagent, strings.Join(left, "; "))
	}
	r.pass("delete: Session %s and agent %s gone on kagent %s", id, kagentStepAgent, r.after.kagent)
	return nil
}

// removeSession deletes the proof's conversation, once.
func (r *kagentStepRun) removeSession() {
	if r.session != "" {
		r.api.removeSession(r.session)
		r.session = ""
	}
}

// stepBack returns the lab to its own configuration: the overlay removed, the
// platform upgraded onto agentlab.yaml's chart.
func (r *kagentStepRun) stepBack() error {
	step("Returning the lab to its own configuration (%s before the proof)", r.before)
	_ = os.Remove(filepath.Join(StateDir, kagentStepValues))
	if err := PlatformUp(r.cfg, Offers{}); err != nil {
		return fmt.Errorf("returning from the kagent step: %w (the lab still runs the step's: `agentlab platform`)", err)
	}
	back, err := readKagentRunning()
	if err != nil {
		return err
	}
	note("back on %s", back)
	return nil
}

// verdict prints the PASS lines of the steps that passed and the FAIL line
// with the stage that failed and the cause.
func (r *kagentStepRun) verdict(err error) {
	fmt.Println()
	for _, line := range r.lines {
		fmt.Printf("PASS: %s\n", line)
	}
	if err != nil {
		fmt.Printf("FAIL: %v\n", err)
	}
}

// readKagentRunning reads the kagent line the lab runs.
func readKagentRunning() (kagentRunning, error) {
	installed, err := installedPlatformChart()
	if err != nil {
		return kagentRunning{}, err
	}
	if installed == nil {
		return kagentRunning{}, errors.New("no agent-platform release in the lab: `agentlab platform` first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return kagentRunning{}, err
	}
	hr, err := getObject(ctx, gvr, platformNamespace, componentKagent)
	if err != nil {
		return kagentRunning{}, fmt.Errorf("the kagent HelmRelease: %w", err)
	}
	version := appliedChartVersion(hr)
	if version == "" {
		return kagentRunning{}, errors.New("the kagent HelmRelease has applied no chart yet")
	}
	deploy, err := getObject(ctx, gvrDeployments, kagentNamespace, kagentControllerName)
	if err != nil {
		return kagentRunning{}, fmt.Errorf("the kagent controller: %w", err)
	}
	return kagentRunning{chart: installed.Version, kagent: version, controller: firstContainerImage(deploy)}, nil
}

// kagentStepOverlay is the step's values overlay: the kagent and kagent-crds
// components pinned to the exact release, which the chart moves together.
func kagentStepOverlay(to string) ([]byte, error) {
	pin := map[string]any{"versionRange": to}
	return yaml.Marshal(map[string]any{"components": map[string]any{
		kagentCRDsRelease: pin,
		componentKagent:   pin,
	}})
}

// kagentStepTemplate is the proof's AgentTemplate on the platform Harness and
// its Agent.
func kagentStepTemplate(modelConfig string) string {
	return fmt.Sprintf(`apiVersion: %[1]s
kind: AgentTemplate
metadata:
  name: %[2]s
  namespace: %[3]s
  labels:
    %[4]s: %[5]s
spec:
  description: "Throwaway agent of agentlab kagent-step-test; deleted by the same run."
  modelConfig:
    name: %[6]s
  systemPrompt: %[7]q
---
%[8]s`, agentTemplateAPIVersion, kagentStepAgent, kagentNamespace, managedByLabel, managedByAgentlabValue, modelConfig, kagentStepSystemMsg,
		agentManifest(kagentStepAgent, kagentStepAgent, kagentHarness, nil))
}
