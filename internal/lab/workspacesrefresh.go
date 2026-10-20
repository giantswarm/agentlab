package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/agentlab/internal/config"
)

// `agentlab workspaces-test --part refresh`: the refresh at a Session's
// start, at the workspace-manager's short refresh threshold
// (platform.workspaces.syncRefreshAfter, the manager's sync.refreshAfter).
// On a workspace of its own with one repository, as the admin:
//
//   - within the threshold: a Session started right after the workspace's
//     first cycle, its grant requesting no sync;
//   - nothing started, changed or requested: no cycle runs or is requested
//     until the threshold and a margin have passed;
//   - past the threshold: authorize_workspace answers with the grant at once
//     and says it requested a sync, the Session starts with that grant, and
//     a cycle with the trigger request follows, ending after the answer: the
//     start did not wait for it;
//   - right after that cycle a Session start is within the threshold again
//     and requests none.

// WorkspacesPartRefresh is the refresh part of workspaces-test.
const WorkspacesPartRefresh = "refresh"

const (
	// refreshWorkspace is the part's workspace.
	refreshWorkspace = "agentlab-refresh"
	// refreshAgent is the part's Agent on the Go ADK Harness; its Sessions
	// run no turn.
	refreshAgent = "agentlab-workspaces-refresh"
	// refreshAfterArg is the workspace-manager's flag of the threshold.
	refreshAfterArg = "--sync-refresh-after="
	// RefreshMinThreshold leaves the within stage time to start its Session
	// after the first cycle; RefreshMaxThreshold bounds what the part waits
	// out. The part's precondition and the help of `configure
	// --workspaces-refresh-after` both read them.
	RefreshMinThreshold = 30 * time.Second
	RefreshMaxThreshold = 5 * time.Minute
	// RefreshExampleThreshold is the threshold the messages suggest.
	RefreshExampleThreshold = time.Minute
	// refreshQuietMargin is watched past the threshold with nothing started,
	// changed or requested.
	refreshQuietMargin = 30 * time.Second
	// refreshAnswerLimit is how long authorize_workspace may take to answer
	// past the threshold: at once, never the length of a sync cycle.
	refreshAnswerLimit = 10 * time.Second
	// syncTriggerRequest is the trigger of a requested cycle, a Session
	// start's past the threshold among them.
	syncTriggerRequest = "request"
)

// refreshProof is the part's state.
type refreshProof struct {
	sp        *syncProof
	timeout   time.Duration
	threshold time.Duration
	admin     *workspacePerson
	api       *kagentAPI
	set       *sessionSet
	connected bool
	// The facts the summary names: how long nothing ran, how fast
	// authorize_workspace answered past the threshold, the cycle that
	// followed.
	quiet    time.Duration
	answered time.Duration
	cycle    *syncLastSync
}

// workspacesRefreshPart runs the part in stages, as the admin.
func workspacesRefreshPart(cfg *config.Config, timeout time.Duration) error {
	p := newProofStages()
	rp := &refreshProof{sp: newSyncProof(cfg, timeout, refreshWorkspace), timeout: timeout}
	err := rp.run(p)
	rp.cleanup()
	if err == nil {
		fmt.Printf("\nWorkspaces refresh part passed: at the workspace-manager's refresh threshold %s, a Session started on %s within it requested no sync and no cycle ran for %s with nothing started, changed or requested; past it authorize_workspace answered in %s requesting a sync, and the cycle %s (trigger %s) ended after that answer; a start right after it requested none.\n",
			rp.threshold, refreshWorkspace, rp.quiet.Round(time.Second), rp.answered.Round(time.Millisecond), rp.cycle.Job, rp.cycle.Trigger)
	}
	return p.close(err)
}

func (rp *refreshProof) run(p *proofStages) error {
	cfg := rp.sp.cfg
	adminUser := cfg.AdminUser()
	if adminUser == nil {
		return fmt.Errorf("the refresh part needs an admin user in %s (`agentlab configure --defaults` writes one)", config.File)
	}
	p.begin("refresh: preconditions", "The refresh part's preconditions: the workspace-manager at a short refresh threshold, the Session admission, the Agent %s on the kagent Harness, %s signed in", refreshAgent, adminUser.Email)
	if err := rp.sp.preconditions(adminUser.Email); err != nil {
		return err
	}
	if err := rp.preconditions(adminUser); err != nil {
		return err
	}

	p.begin("refresh: workspace", "%s connected to %s, create_workspace %s with %s, its first cycle", adminUser.Email, config.WorkspaceProviderFake, refreshWorkspace, syncMovingRepo)
	first, err := rp.createWorkspace()
	if err != nil {
		return err
	}

	p.begin("refresh: within the threshold", "A Session started right after the first cycle, within the %s threshold: its grant requests no sync", rp.threshold)
	a, _, err := rp.startSession(first.Time)
	if err != nil {
		return err
	}
	if a.SyncRequested {
		return fmt.Errorf("authorize_workspace requested a sync %s after the last one, within the threshold %s", time.Since(first.Time).Round(time.Second), rp.threshold)
	}

	p.begin("refresh: nothing requested", "No cycle runs or is requested while nothing starts, changes or is requested, until %s past the threshold", refreshQuietMargin)
	if err := rp.watchQuiet(first, first.Time.Add(rp.threshold+refreshQuietMargin)); err != nil {
		return err
	}

	p.begin("refresh: past the threshold", "A Session started past the threshold: authorize_workspace answers at once and requests a sync, the Session starts, a cycle with the trigger %s follows and ends after the answer", syncTriggerRequest)
	called := time.Now()
	a, rp.answered, err = rp.startSession(time.Time{})
	if err != nil {
		return err
	}
	answeredAt := called.Add(rp.answered)
	if !a.SyncRequested {
		return fmt.Errorf("authorize_workspace requested no sync %s after the last one, past the threshold %s", time.Since(first.Time).Round(time.Second), rp.threshold)
	}
	if rp.answered > refreshAnswerLimit {
		return fmt.Errorf("authorize_workspace answered in %s past the threshold, want at once (within %s): the start waits for the sync", rp.answered.Round(time.Millisecond), refreshAnswerLimit)
	}
	d, err := rp.sp.waitCycle(refreshWorkspace, first)
	if err != nil {
		return err
	}
	if err := judgeRefreshCycle(d.Workspace.LastSync, answeredAt); err != nil {
		return err
	}
	rp.cycle = d.Workspace.LastSync
	note("the cycle %s (trigger %s) ended %s at %s, %s after authorize_workspace answered", rp.cycle.Job, rp.cycle.Trigger, rp.cycle.Result, rp.cycle.Time.UTC().Format(time.RFC3339), rp.cycle.Time.Sub(answeredAt).Round(time.Second))

	p.begin("refresh: within again", "A Session started right after the refresh's cycle: within the threshold again, no sync requested")
	a, _, err = rp.startSession(rp.cycle.Time)
	if err != nil {
		return err
	}
	if a.SyncRequested {
		return fmt.Errorf("authorize_workspace requested a sync %s after the refresh's cycle, within the threshold %s", time.Since(rp.cycle.Time).Round(time.Second), rp.threshold)
	}
	return nil
}

// preconditions reads the manager's threshold, checks the Session admission,
// signs the admin in and readies the part's Agent.
func (rp *refreshProof) preconditions(adminUser *config.User) error {
	cfg := rp.sp.cfg
	ctx := context.Background()
	flags, err := workspaceManagerFlags(ctx)
	if err != nil {
		return err
	}
	if rp.threshold, err = judgeRefreshThreshold(flags); err != nil {
		return err
	}
	note("the workspace-manager's refresh threshold is %s", rp.threshold)
	rig := workspaceAdmissionValuesFor(cfg, workspacesChartCarries(cfg))
	if rig == nil {
		return fmt.Errorf("no Session admission on this lab (the chart takes no workspaces values, or platform.agents is off): the refresh part starts Sessions on a workspace volume")
	}
	key, err := readWorkspaceGrantKey()
	if err != nil {
		return err
	}
	kid, err := proveWorkspaceManagerPublishesGrantKey(ctx, key)
	if err != nil {
		return err
	}
	if err := proveKagentAdmissionTrusts(ctx, rig); err != nil {
		return err
	}
	note("the manager publishes the lab's grant key as %s; kagent's admission trusts %s", kid, rig.JWKSURL)
	if rp.admin, err = openWorkspacePerson(cfg, adminUser); err != nil {
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
	if err := waitHarnessReady(ctx, kagentHarness, rp.timeout); err != nil {
		return err
	}
	if agentExists(refreshAgent) {
		if err := removeAgent(refreshAgent); err != nil {
			return err
		}
	}
	if _, err := applyManifests(ctx, []byte(workspacesAgentManifest(refreshAgent, kagentHarness))); err != nil {
		return fmt.Errorf("applying the Agent %s: %w", refreshAgent, err)
	}
	boot, err := waitAgentReadyOn(refreshAgent, kagentHarness, rp.timeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return fmt.Errorf("the Agent %s is not Ready on Harness %s after %s: %s", refreshAgent, kagentHarness, boot.elapsed.Round(time.Second), boot.reason)
	}
	note("the Agent %s Ready on Harness %s in %s", refreshAgent, kagentHarness, boot.elapsed.Round(time.Second))
	return nil
}

// createWorkspace connects the admin to the lab's GitHub and defines the
// part's workspace, returning its first cycle.
func (rp *refreshProof) createWorkspace() (*syncLastSync, error) {
	s, cfg := rp.sp, rp.sp.cfg
	hops, err := connectWorkspaceProvider(cfg, rp.admin, config.WorkspaceProviderFake)
	if err != nil {
		return nil, err
	}
	rp.connected = true
	note("%s connected to %s: %s", rp.admin.user.Email, config.WorkspaceProviderFake, hops)
	var created syncWorkspace
	if err := s.member.createLabWorkspace(map[string]any{
		nameKey: refreshWorkspace, wmOrganizationArg: config.WorkspaceOrganization, wmSourcesArg: syncNamedSources(syncMovingRepo),
	}, &created); err != nil {
		return nil, err
	}
	d, err := s.waitCycle(refreshWorkspace, nil)
	if err != nil {
		return nil, err
	}
	r := d.Workspace.LastSync
	if r.Result != syncResultSucceed {
		return nil, fmt.Errorf("the first cycle of %s ended %s: %s", refreshWorkspace, r.Result, r.Message)
	}
	note("created %s of Organization %s; first cycle %s (trigger %s) ended %s", refreshWorkspace, created.Organization, r.Job, r.Trigger, r.Time.UTC().Format(time.RFC3339))
	return r, nil
}

// startSession asks authorize_workspace for the admin's grant and starts a
// Session of the part's Agent with it through start_session; it returns the
// grant and how long authorize_workspace took to answer. A non-zero lastSync is the sync the start must be within
// the threshold of: a start that came too late for it is the error.
func (rp *refreshProof) startSession(lastSync time.Time) (workspaceAuthorization, time.Duration, error) {
	called := time.Now()
	a, err := rp.admin.authorizeWorkspace(refreshWorkspace)
	answered := time.Since(called)
	if err != nil {
		return a, answered, err
	}
	if a.Outcome != authorizeGranted || a.Grant == "" {
		return a, answered, fmt.Errorf("authorize_workspace %s answered %s (%q, unreadable %v, connect %v), want granted", refreshWorkspace, a.Outcome, a.Reason, a.Unreadable, a.Connect)
	}
	if !lastSync.IsZero() {
		if since := called.Sub(lastSync); since >= rp.threshold {
			return a, answered, fmt.Errorf("the Session started %s after the last sync, not within the threshold %s: a longer threshold (`agentlab configure --workspaces-refresh-after %s`, then `agentlab platform`)", since.Round(time.Second), rp.threshold, formatThreshold(RefreshExampleThreshold))
		}
	}
	env, err := rp.admin.session.callToolEnvelope(agentManagerToolPrefix+"start_session", map[string]any{
		nameKey: refreshAgent, namespaceKey: kagentNamespace, startSessionRequestIDArg: uuid.NewString(), startSessionGrantArg: a.Grant,
	})
	if err != nil {
		return a, answered, err
	}
	if env.IsError {
		return a, answered, fmt.Errorf("start_session refused: %.400s", env.Content[0].Text)
	}
	var started accessStarted
	if err := json.Unmarshal([]byte(env.Content[0].Text), &started); err != nil || started.Session.ID == "" {
		return a, answered, fmt.Errorf("start_session answered no session: %.400s", env.Content[0].Text)
	}
	rp.set.add(started.Session.ID)
	note("Session %s started; authorize_workspace answered in %s, sync requested: %t", started.Session.ID, answered.Round(time.Millisecond), a.SyncRequested)
	return a, answered, nil
}

// watchQuiet reads the workspace until the deadline: no cycle may run, be
// requested or end after last.
func (rp *refreshProof) watchQuiet(last *syncLastSync, until time.Time) error {
	start := time.Now()
	for {
		d, err := rp.sp.get(refreshWorkspace)
		if err != nil {
			return err
		}
		if err := judgeQuiet(last, d.Workspace); err != nil {
			return fmt.Errorf("%s after the last sync, with nothing started, changed or requested: %w", time.Since(last.Time).Round(time.Second), err)
		}
		if time.Now().After(until) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	rp.quiet = time.Since(start)
	note("no cycle ran or was requested for %s; the last sync is still %s, %s ago", rp.quiet.Round(time.Second), last.Job, time.Since(last.Time).Round(time.Second))
	return nil
}

// judgeQuiet is the verdict on a workspace read while nothing starts,
// changes or is requested: still at the last sync, nothing running or
// requested.
func judgeQuiet(last *syncLastSync, w syncWorkspace) error {
	switch {
	case w.ActiveSync != nil:
		return fmt.Errorf("the cycle %s runs", w.ActiveSync.Job)
	case w.SyncRequest != nil:
		return fmt.Errorf("a sync was requested at %s", w.SyncRequest.RequestedAt.UTC().Format(time.RFC3339))
	case w.LastSync == nil || !w.LastSync.Time.Equal(last.Time) || w.LastSync.Job != last.Job:
		return fmt.Errorf("the last sync is %+v, want %s ended %s", w.LastSync, last.Job, last.Time.UTC().Format(time.RFC3339))
	}
	return nil
}

// judgeRefreshCycle is the verdict on the cycle that followed a Session
// start past the threshold: requested, and ended after authorize_workspace
// answered, so the start did not wait for it.
func judgeRefreshCycle(r *syncLastSync, answered time.Time) error {
	switch {
	case r == nil:
		return errors.New("no cycle followed the Session start past the threshold")
	case r.Trigger != syncTriggerRequest:
		return fmt.Errorf("the cycle %s after the Session start has the trigger %q, want %q", r.Job, r.Trigger, syncTriggerRequest)
	case !r.Time.After(answered):
		return fmt.Errorf("the cycle %s ended at %s, before authorize_workspace answered at %s: the start waited for it", r.Job, r.Time.UTC().Format(time.RFC3339Nano), answered.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// judgeRefreshThreshold reads the manager's refresh threshold and refuses
// one outside [RefreshMinThreshold, RefreshMaxThreshold].
func judgeRefreshThreshold(args []string) (time.Duration, error) {
	raw := flagValue(args, refreshAfterArg)
	if raw == "" {
		return 0, fmt.Errorf("the workspace-manager has no %s argument: a chart before the refresh at a Session's start; the refresh part needs workspace-manager 0.8.2 or newer", strings.TrimSuffix(refreshAfterArg, "="))
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("the workspace-manager's %s%s: %w", refreshAfterArg, raw, err)
	}
	if d < RefreshMinThreshold || d > RefreshMaxThreshold {
		return 0, fmt.Errorf("the workspace-manager's refresh threshold is %s; the refresh part needs %s (shorter, a Session cannot start within it; longer, the part waits too long): `agentlab configure --workspaces-refresh-after %s`, then `agentlab platform`", d, RefreshThresholdRange(), RefreshThresholdExample())
	}
	return d, nil
}

// RefreshThresholdRange is the range the refresh part requires of the
// workspace-manager's refresh threshold, as a person types durations: "30s
// to 5m".
func RefreshThresholdRange() string {
	return formatThreshold(RefreshMinThreshold) + " to " + formatThreshold(RefreshMaxThreshold)
}

// RefreshThresholdExample is the threshold to suggest: "1m".
func RefreshThresholdExample() string {
	return formatThreshold(RefreshExampleThreshold)
}

// formatThreshold is a duration as a person types it: "1m", not "1m0s".
func formatThreshold(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	return s
}

// cleanup removes what the part left: the Sessions, the connection, the
// Agent, the workspace.
func (rp *refreshProof) cleanup() {
	if rp.sp.restored {
		return
	}
	var errs []error
	if rp.set != nil {
		errs = append(errs, rp.set.removeAllNow())
	}
	if rp.admin != nil && rp.connected {
		rp.admin.disconnect(config.WorkspaceProviderFake)
	}
	if agentExists(refreshAgent) {
		errs = append(errs, removeAgent(refreshAgent))
	}
	errs = append(errs, rp.sp.restore())
	if rp.api != nil {
		rp.api.close()
	}
	if err := errors.Join(errs...); err != nil {
		warn("removing what the part left: %v", err)
	}
}
