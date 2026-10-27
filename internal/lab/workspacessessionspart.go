package lab

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// `agentlab workspaces-test --part sessions [--provider github|gitlab]
// [--sessions n]`: Sessions started together on one workspace over the
// provider instance's two proof repositories on the real provider
// (workspacesproofrepos.go), on the claude and the kagent (Go ADK) Harness,
// with a sync cycle requested at the same moment, and the session
// directories' cleanup. After a push to the scratch branch of the first
// repository since the workspace's first cycle:
//
//   - one grant per Session from authorize_workspace as the admin, each
//     naming the pushed repository changed since the sync;
//   - sync_workspace and every Session's start_session with its grant
//     released at once (ten Sessions by default, alternating the Harnesses),
//     so the Sessions' setups clone from the mirrors while the requested
//     cycle fetches into them;
//   - each Session's first turn lists its working directory and every
//     clone's heads: the default branches at the provider's, the scratch
//     branch of the pushed repository at the pushed commit; it writes a file
//     only it knows;
//   - the volume holds one directory per Session, each set up once, whose
//     setup record (.kagent/workspace.json) names the pushed repository
//     fetched from the provider exactly once and nothing else; the requested
//     cycle fetched the pushed repository into its mirror;
//   - isolation: a second turn on each Session finds only its own file
//     anywhere it can look, and a write into the mirrors refused;
//   - a Session deleted: its directory gone with no cycle requested, the
//     mirrors' heads and every other directory untouched;
//   - a Session started without a workspace: an empty working directory, no
//     mirrors;
//   - the cleanup, with the lab's short window
//     (platform.workspaces.sessionsCleanupAfter): the Sessions suspended,
//     the window waited out, a cycle removes every directory and the
//     Workspace reads SessionsCleanedUp=True; one Session per Harness resumed
//     works in a fresh directory.
//
// The report (timings per Session) goes to the state directory. The
// Sessions, the Agents, the workspace, the scratch branch and the host's
// clone are removed on every path.

// WorkspacesPartSessions is the sessions part of workspaces-test.
const WorkspacesPartSessions = "sessions"

// DefaultWorkspacesSessions is the number of Sessions the part starts.
const DefaultWorkspacesSessions = 10

// workspacesMinSessions is the fewest Sessions the part starts: one per
// Harness is left after the delete stage.
const workspacesMinSessions = 3

const (
	sessionsWorkspace = "agentlab-sessions"
	// sessionsKagentAgent is the part's Agent on the Go ADK Harness; its
	// claude Agent is the harness stage's (workspacesHarnessAgent).
	sessionsKagentAgent = "agentlab-workspaces-kagent"
	// SessionsMaxCleanupWindow bounds the cleanup window the part waits out;
	// SessionsMinCleanupWindow keeps the live Sessions' directories through
	// the cycles before the cleanup stage.
	SessionsMaxCleanupWindow = 15 * time.Minute
	SessionsMinCleanupWindow = 5 * time.Minute
	// sessionsExampleCleanupWindow is the window the part's messages
	// suggest.
	sessionsExampleCleanupWindow = 10 * time.Minute
	// sessionsWindowMargin is waited past the window, for the clocks.
	sessionsWindowMargin = 20 * time.Second
	// sessionsReportFile is the part's report, in the state directory.
	sessionsReportFile = "workspaces-sessions-report.json"
	// sessionsFreshNotice is the words of the runtime's notice that a
	// returning Session's directory was set up again.
	sessionsFreshNotice = "set up again"
	// sessionsPollArg is the workspace-manager's flag of how often it lists
	// kagent's Sessions for a deleted one, whose directory then goes at
	// once.
	sessionsPollArg = "--sessions-poll="
	// sessionsDeleteWait is how long a deleted Session's directory may stay
	// on the volume with no cycle requested.
	sessionsDeleteWait = 2 * time.Minute
	// triggerSessionDeleted is the cycle the manager runs for a deleted
	// Session.
	triggerSessionDeleted = "sessionDeleted"
	// sessionsProbePurpose says what the part's probe commands do.
	sessionsProbePurpose = "the command only lists your working directory and its git clones and writes a marker file."
)

// sessionsHarnesses are the Harnesses the Sessions alternate over, each with
// its Agent.
var sessionsHarnesses = []struct{ harness, agent string }{
	{claudeHarness, workspacesHarnessAgent},
	{kagentHarness, sessionsKagentAgent},
}

// sessionRun is one Session of the part: who it is, what it said, and its
// timings.
type sessionRun struct {
	Harness string `json:"harness"`
	Agent   string `json:"agent"`
	ID      string `json:"session"`
	// Token is the name suffix of the file only this Session knows.
	Token string `json:"token"`
	// The timings: start_session called and answered, the first turn sent,
	// the setup's record of the directory set up, the first turn ended.
	StartCalled    time.Time `json:"startCalled"`
	StartAnswered  time.Time `json:"startAnswered"`
	FirstTurnSent  time.Time `json:"firstTurnSent"`
	DirectorySetUp time.Time `json:"directorySetUp"`
	FirstTurnEnded time.Time `json:"firstTurnEnded"`
	SetupSeconds   float64   `json:"setupSeconds"`
	// What the first turn relayed: the working directory, each clone's
	// default head (origin/HEAD) and scratch branch, and the setup's record.
	Directory string            `json:"directory"`
	Heads     map[string]string `json:"heads"`
	Branches  map[string]string `json:"branches"`
	Cloned    []string          `json:"cloned"`
	Fetched   []string          `json:"fetched"`
	// Isolation: the files of the prefix the second turn found, and its
	// write into the mirrors.
	Seen         []string `json:"seen"`
	MirrorsWrite string   `json:"mirrorsWrite"`
	Deleted      bool     `json:"deleted,omitempty"`
	Notices      []string `json:"notices,omitempty"`
	grant        string
	err          error
}

// sessionsReport is the part's report file.
type sessionsReport struct {
	Provider      string            `json:"provider"`
	Workspace     string            `json:"workspace"`
	Volume        string            `json:"volume"`
	Pushed        string            `json:"pushed"`
	PushedCommit  string            `json:"pushedCommit"`
	SyncCycle     *syncLastSync     `json:"syncCycle"`
	Sessions      []*sessionRun     `json:"sessions"`
	CleanupWindow string            `json:"cleanupWindow"`
	Condition     templateCondition `json:"sessionsCleanedUp"`
}

// sessionsProof is the part's state.
type sessionsProof struct {
	sp       *syncProof
	pr       *proofRepos
	timeout  time.Duration
	sessions int
	admin    *workspacePerson
	token    string
	api      *kagentAPI
	set      *sessionSet
	volume   workspaceVolume
	window   time.Duration
	poll     time.Duration
	// heads are the default branches' heads at the provider.
	heads  map[string]string
	report sessionsReport
}

// workspacesSessionsPart runs the part in stages, as the admin.
func workspacesSessionsPart(cfg *config.Config, instance string, sessions int, timeout time.Duration) error {
	p := newProofStages()
	if sessions <= 0 {
		sessions = DefaultWorkspacesSessions
	}
	sp := &sessionsProof{sp: newSyncProof(cfg, timeout, sessionsWorkspace), timeout: timeout, sessions: sessions}
	sp.report.Provider = instance
	err := sp.run(instance, p)
	sp.cleanup()
	if err == nil && sp.pr != nil {
		fmt.Printf("\nWorkspaces sessions part passed on %s: %d Sessions started together with a requested sync cycle on %s, each in its own directory on volume %s with a clone of %s and %s, %s fetched from the provider once per Session and nothing else; each Session isolated, the mirrors read-only; a deleted Session's directory gone with no cycle requested; a Session without a workspace in an empty directory; after the %s cleanup window every directory removed, %s, and a resumed Session in a fresh directory. Report: %s\n",
			instance, len(sp.report.Sessions), sessionsWorkspace, sp.volume.Handle, sp.pr.pushed, sp.pr.still, sp.pr.pushed, sp.window, sp.report.Condition, filepath.Join(StateDir, sessionsReportFile))
	}
	return p.close(err)
}

// sessionsStages are the part's stages after its preconditions, skipped
// together without the instance's proof repositories.
var sessionsStages = []string{"sessions: workspace", "sessions: push", "sessions: start", "sessions: directories", "sessions: isolation", "sessions: delete", "sessions: no workspace", "sessions: cleanup", "sessions: report"}

func (sp *sessionsProof) run(instance string, p *proofStages) error {
	cfg := sp.sp.cfg
	p.begin("sessions: preconditions", "The sessions part's preconditions on %s: its proof repositories, the workspace-manager with kagent and a short cleanup window, the Session admission, the Agents on the claude and the kagent Harness, the admin signed in holding the instance's grant", instance)
	if skip := proofReposSkip(cfg, instance); skip != "" {
		p.skip("%s", skip)
		p.leaveOut(skip, sessionsStages...)
		return nil
	}
	if err := sp.preconditions(instance); err != nil {
		return err
	}
	pr := sp.pr

	p.begin("sessions: workspace", "create_workspace %s over %s and %s, its first cycle", sessionsWorkspace, pr.pushed, pr.still)
	if err := sp.createWorkspace(); err != nil {
		return err
	}

	p.begin("sessions: push", "A push to %s's %s since the sync, and one grant per Session naming it changed", pr.pushed, proofBranch)
	pushed, err := pr.push(pr.start, false)
	if err != nil {
		return err
	}
	sp.report.PushedCommit = pushed
	if err := sp.grants(); err != nil {
		return err
	}

	p.begin("sessions: start", "sync_workspace and %d Sessions' start_session released at once, alternating the claude and the kagent Harness, each first turn listing its directory and every clone's heads", sp.sessions)
	synced, err := sp.sp.get(sessionsWorkspace)
	if err != nil {
		return err
	}
	if err := sp.startAll(); err != nil {
		return err
	}

	p.begin("sessions: directories", "One directory per Session on the volume, %s fetched from the provider once per Session and nothing else; the requested cycle fetched it into the mirror", pr.pushed)
	if err := sp.directories(synced.Workspace.LastSync); err != nil {
		return err
	}

	p.begin("sessions: isolation", "A second turn on each Session: only its own file anywhere it looks, the mirrors refusing a write")
	if err := sp.isolation(); err != nil {
		return err
	}

	p.begin("sessions: delete", "A Session deleted: its directory gone with no cycle requested, the mirrors and every other directory untouched")
	if err := sp.deleteOne(); err != nil {
		return err
	}

	p.begin("sessions: no workspace", "A Session started without a workspace on each Harness: an empty working directory and no mirrors")
	if err := sp.noWorkspace(); err != nil {
		return err
	}

	p.begin("sessions: cleanup", "The Sessions suspended, the %s window waited out: a cycle removes every directory and the Workspace reads %s=True; a resumed Session per Harness works in a fresh directory", sp.window, conditionSessionsCleanedUp)
	if err := sp.cleanupCycle(); err != nil {
		return err
	}

	p.begin("sessions: report", "The Sessions' timings, written to %s", filepath.Join(StateDir, sessionsReportFile))
	return sp.writeReport()
}

// preconditions checks the workspace-manager's cleanup settings, signs the
// admin in to muster and kagent, has them hold the instance's grant, readies
// an Agent on each Harness and clones the pushed repository on this host.
func (sp *sessionsProof) preconditions(instance string) error {
	s, cfg := sp.sp, sp.sp.cfg
	ctx := context.Background()
	if reason, err := workspaceManagerSkip(cfg); err != nil {
		return err
	} else if reason != "" {
		return fmt.Errorf("no workspace-manager: %s", reason)
	}
	if err := waitMCPServerReachable(workspaceManagerMCPServer); err != nil {
		return fmt.Errorf("the workspace-manager is not reachable through muster: %w", err)
	}
	flags, err := workspaceManagerFlags(ctx)
	if err != nil {
		return err
	}
	if err := judgeKagentAddress(flags); err != nil {
		return err
	}
	if sp.window, err = judgeCleanupWindow(flags, SessionsMinCleanupWindow, SessionsMaxCleanupWindow); err != nil {
		return err
	}
	sp.report.CleanupWindow = sp.window.String()
	if sp.poll, err = judgeSessionPoll(flags); err != nil {
		return err
	}
	if sp.poll == 0 {
		note("the workspace-manager has no %s argument: it removes a deleted Session's directory at its next cycle, which the delete stage requests", strings.TrimSuffix(sessionsPollArg, "="))
	}
	note("the workspace-manager lists Sessions from kagent at %s and cleans a directory up %s after its last turn", flagValue(flags, sessionsKagentArg), sp.window)

	rig := workspaceAdmissionValuesFor(cfg, workspacesChartCarries(cfg))
	if rig == nil {
		return fmt.Errorf("no Session admission on this lab (the chart takes no workspaces values, or platform.agents is off): the sessions part starts Sessions on a workspace volume")
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

	adminUser := cfg.AdminUser()
	if adminUser == nil {
		return fmt.Errorf("the sessions part needs an admin user in %s (`agentlab configure --defaults` writes one)", config.File)
	}
	if sp.admin, err = openWorkspacePerson(cfg, adminUser); err != nil {
		return err
	}
	s.member, s.memberEmail = sp.admin.session, adminUser.Email
	if err := grantProofProvider(cfg, sp.admin, instance, sp.timeout); err != nil {
		return err
	}
	if sp.token, err = passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, adminUser.Email, adminUser.Password, musterLoginScopes); err != nil {
		return err
	}
	if sp.api, err = dialKagentAPI(cfg, sp.token); err != nil {
		return err
	}
	sp.set = &sessionSet{api: sp.api, manager: sp.admin.session}
	if err := readySessionsAgents(ctx, rig, sp.timeout); err != nil {
		return err
	}
	if res := sideloadImages(cfg, hostPullImages([]string{workspacesTestGitImage})); res.n > 0 {
		note("side-loaded the volume probes' git image (%s)", res.d)
	}
	if err := s.removeLeftovers(); err != nil {
		return err
	}
	if sp.pr, err = newProofRepos(cfg, instance); err != nil {
		return err
	}
	if _, err := sp.pr.clone(); err != nil {
		return err
	}
	sp.heads = map[string]string{}
	for _, repo := range sp.pr.repos() {
		if sp.heads[repo], err = sp.pr.defaultHead(repo); err != nil {
			return err
		}
	}
	sp.report.Pushed = sp.pr.pushed
	note("%s cloned on this host at %s, to push to its scratch branch %s; %s stays as it is", sp.pr.pushed, shortSHA(sp.pr.start), proofBranch, sp.pr.still)
	return nil
}

// readySessionsAgents applies an Agent on each of sessionsHarnesses, an
// earlier run's removed first, and waits for each to be Ready.
func readySessionsAgents(ctx context.Context, rig *workspaceAdmissionValues, timeout time.Duration) error {
	harnesses := map[string]string{claudeHarness: rig.ClaudeHarness, kagentHarness: kagentHarness}
	for _, h := range sessionsHarnesses {
		harness := harnesses[h.harness]
		if err := waitHarnessReady(ctx, harness, timeout); err != nil {
			return err
		}
		if agentExists(h.agent) {
			if err := removeAgent(h.agent); err != nil {
				return err
			}
		}
		if _, err := applyManifests(ctx, []byte(workspacesAgentManifest(h.agent, harness, defaultModelConfig, fmt.Sprintf(workspacesHarnessSystemPrompt, "Bash tool")))); err != nil {
			return fmt.Errorf("applying the Agent %s: %w", h.agent, err)
		}
		boot, err := waitAgentReadyOn(h.agent, harness, timeout)
		if err != nil {
			return err
		}
		if !boot.ready {
			return fmt.Errorf("the Agent %s is not Ready on Harness %s after %s: %s", h.agent, harness, boot.elapsed.Round(time.Second), boot.reason)
		}
		note("the Agent %s Ready on Harness %s in %s", h.agent, harness, boot.elapsed.Round(time.Second))
	}
	return nil
}

// removeSessionsAgents removes the Agents readySessionsAgents applied.
func removeSessionsAgents() []error {
	var errs []error
	for _, h := range sessionsHarnesses {
		if agentExists(h.agent) {
			errs = append(errs, removeAgent(h.agent))
		}
	}
	return errs
}

// startAgentSession starts a Session of the Agent through agent-manager's
// start_session as the person ms signs in as, with the grant and the
// toolchains (name[@version] each) when given, and returns its id.
func startAgentSession(ms *musterSession, agent, grant string, toolchains ...string) (string, error) {
	args := map[string]any{nameKey: agent, namespaceKey: kagentNamespace, startSessionRequestIDArg: uuid.NewString()}
	if grant != "" {
		args[startSessionGrantArg] = grant
	}
	if len(toolchains) > 0 {
		args[startSessionToolchainsArg] = toolchains
	}
	env, err := ms.callToolEnvelope(agentManagerToolPrefix+"start_session", args)
	if err != nil {
		return "", err
	}
	if env.IsError {
		return "", fmt.Errorf("start_session on %s refused: %.400s", agent, env.Content[0].Text)
	}
	var started accessStarted
	if err := json.Unmarshal([]byte(env.Content[0].Text), &started); err != nil || started.Session.ID == "" {
		return "", fmt.Errorf("start_session answered no session: %.400s", env.Content[0].Text)
	}
	return started.Session.ID, nil
}

// startBoundSession starts a Session of the Agent without a workspace the
// way Slack and the portal do: through agent-manager's start_session as the
// person ms signs in as, which binds the Session's actor to them in muster.
// It returns the Session once it is ready, and refuses one that lists no
// provider verifying the actor's JWT: its runtime would then reach muster as
// nobody.
func startBoundSession(ctx context.Context, ms *musterSession, api *kagentAPI, sessions *sessionSet, agent string) (*apiv1alpha1.Session, error) {
	id, err := startAgentSession(ms, agent, "")
	if err != nil {
		return nil, err
	}
	sessions.add(id)
	session, err := api.getSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if session, err = api.awaitSessionReady(ctx, session); err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(session.GetProviders(), func(p *apiv1alpha1.SessionProvider) bool { return p.GetAudience() != "" }) {
		return nil, fmt.Errorf("start_session started Session %s of %s with no provider that verifies the actor's JWT (providers %v): agent-manager did not bind it", id, agent, session.GetProviders())
	}
	return session, nil
}

// createWorkspace defines the workspace as the admin and waits for its
// first cycle; its volume is the one the grants name.
func (sp *sessionsProof) createWorkspace() error {
	s, pr := sp.sp, sp.pr
	var created syncWorkspace
	if err := s.member.createLabWorkspace(map[string]any{
		nameKey: sessionsWorkspace, wmOrganizationArg: config.WorkspaceOrganization, wmSourcesArg: pr.sources(pr.repos()...),
	}, &created); err != nil {
		return err
	}
	d, err := s.waitCycle(sessionsWorkspace, nil)
	if err != nil {
		return err
	}
	if r := d.Workspace.LastSync; r.Result != syncResultSucceed {
		return fmt.Errorf("the first cycle of %s ended %s: %s", sessionsWorkspace, r.Result, r.Message)
	}
	if live := d.live(); !slices.Equal(live, pr.repos()) {
		return fmt.Errorf("the first cycle of %s mirrored %v, want %v", sessionsWorkspace, live, pr.repos())
	}
	v, err := s.volume(sessionsWorkspace)
	if err != nil {
		return err
	}
	if sp.volume, err = workspaceVolumeOf(v); err != nil {
		return err
	}
	sp.report.Workspace, sp.report.Volume = sessionsWorkspace, sp.volume.Handle
	note("created %s of Organization %s; first cycle %s; claim %s/%s, volume %s", sessionsWorkspace, created.Organization, d.Workspace.LastSync.Result, v.namespace, v.claim, sp.volume.Handle)
	s.printManifest(d)
	return nil
}

// workspaceVolumeOf is the CSI volume behind a workspace's claim, the one
// its grants name.
func workspaceVolumeOf(v syncVolume) (workspaceVolume, error) {
	k, err := labKube()
	if err != nil {
		return workspaceVolume{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	pv, err := k.clientset.CoreV1().PersistentVolumes().Get(ctx, v.pv, metav1.GetOptions{})
	if err != nil {
		return workspaceVolume{}, fmt.Errorf("reading the volume %s: %w", v.pv, err)
	}
	if pv.Spec.CSI == nil {
		return workspaceVolume{}, fmt.Errorf("the volume %s is no CSI volume", v.pv)
	}
	return workspaceVolume{Driver: pv.Spec.CSI.Driver, Handle: pv.Spec.CSI.VolumeHandle}, nil
}

// grants asks one grant per Session, each naming the pushed repository
// changed since the sync.
func (sp *sessionsProof) grants() error {
	runs := make([]*sessionRun, sp.sessions)
	for i := range runs {
		h := sessionsHarnesses[i%len(sessionsHarnesses)]
		runs[i] = &sessionRun{Harness: h.harness, Agent: h.agent, Token: randomHex(8)}
	}
	sp.report.Sessions = runs
	for _, r := range runs {
		a, err := grantWithChange(sp.admin, sessionsWorkspace, sp.pr.pushed, sp.timeout)
		if err != nil {
			return err
		}
		r.grant = a.Grant
	}
	note("%d grants on %s, each naming %s changed since the sync", len(runs), sessionsWorkspace, sp.pr.pushed)
	return nil
}

// grantWithChange asks authorize_workspace until the grant names the
// repository changed since the sync: the provider's listing may report the
// push a little after git accepted it.
func grantWithChange(p *workspacePerson, workspace, changed string, timeout time.Duration) (workspaceAuthorization, error) {
	deadline := time.Now().Add(min(timeout, syncListingFreshness+30*time.Second))
	for {
		a, err := p.authorizeWorkspace(workspace)
		if err != nil {
			return a, err
		}
		if a.Outcome != authorizeGranted || a.Grant == "" {
			return a, fmt.Errorf("authorize_workspace answered %s (%q, unreadable %v, connect %v), want granted", a.Outcome, a.Reason, a.Unreadable, a.Connect)
		}
		if slices.Contains(a.Changed, changed) {
			return a, nil
		}
		if time.Now().After(deadline) {
			return a, fmt.Errorf("the grant names %v changed since the sync, want %s, which was pushed after it", a.Changed, changed)
		}
		time.Sleep(5 * time.Second)
	}
}

// startAll releases the sync request and every Session's start at once,
// runs each first turn, then says what each relayed; one failed Session
// fails the stage, naming each.
func (sp *sessionsProof) startAll() error {
	runs := sp.report.Sessions
	release := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var syncErr error
	wg.Go(func() {
		<-release
		syncErr = requestSync(sp.sp.member, sessionsWorkspace)
	})
	for _, r := range runs {
		wg.Go(func() {
			<-release
			r.err = sp.startOne(r)
			if r.ID != "" {
				mu.Lock()
				sp.set.add(r.ID)
				mu.Unlock()
			}
		})
	}
	close(release)
	wg.Wait()
	errs := []error{syncErr}
	for _, r := range runs {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("session %s on %s: %w", cmp.Or(r.ID, "(not started)"), r.Harness, r.err))
			continue
		}
		note("%s on %s: works in %s; %s; %s at %s; first turn sent %s after start_session answered, ended in %s",
			r.ID, r.Harness, r.Directory, headsLine(r.Heads), proofBranch, headsLine(r.Branches), r.FirstTurnSent.Sub(r.StartAnswered).Round(time.Millisecond), r.FirstTurnEnded.Sub(r.FirstTurnSent).Round(time.Second))
		if setup := setupNotices(r.Notices); setup != "" {
			note("  the setup said: %s", setup)
		}
	}
	return errors.Join(errs...)
}

// startOne starts the Session through start_session with its grant and
// runs the first turn, recording the timings.
func (sp *sessionsProof) startOne(r *sessionRun) error {
	session, err := openMusterSession(sp.sp.cfg, sp.token, "workspaces-test-sessions-"+r.Token)
	if err != nil {
		return err
	}
	r.StartCalled = time.Now()
	env, err := session.callToolEnvelope(agentManagerToolPrefix+"start_session", map[string]any{
		nameKey: r.Agent, namespaceKey: kagentNamespace, startSessionRequestIDArg: uuid.NewString(), startSessionGrantArg: r.grant,
	})
	r.StartAnswered = time.Now()
	if err != nil {
		return err
	}
	if env.IsError {
		return fmt.Errorf("start_session refused: %.400s", env.Content[0].Text)
	}
	var started accessStarted
	if err := json.Unmarshal([]byte(env.Content[0].Text), &started); err != nil || started.Session.ID == "" {
		return fmt.Errorf("start_session answered no session: %.400s", env.Content[0].Text)
	}
	r.ID = started.Session.ID
	ctx, cancel := context.WithTimeout(context.Background(), sp.timeout)
	defer cancel()
	ks, err := sp.api.getSession(ctx, r.ID)
	if err != nil {
		return err
	}
	if vs := ks.GetVolumeSource().GetVolume(); vs.GetVolumeHandle() != sp.volume.Handle {
		return fmt.Errorf("on volume %q, want the workspace's %s", vs.GetVolumeHandle(), sp.volume.Handle)
	}
	var values sessionsTurnValues
	var branches map[string]string
	first, sent, err := probeTurnWith(ctx, sp.api, ks, sessionsProofFirstTurnCommand(r.Token), "", sessionsProbePurpose, func(reply string) (err error) {
		values, err = parseSessionsTurn(reply)
		branches = parseSessionsBranches(reply)
		return err
	})
	r.FirstTurnSent, r.FirstTurnEnded = sent, time.Now()
	r.Notices = firstNotices(first)
	if err != nil {
		return err
	}
	r.Directory, r.Heads, r.Branches = values.pwd, values.heads, branches
	return nil
}

// sessionsBranchLine is a first turn's line of a clone's scratch branch.
var sessionsBranchLine = regexp.MustCompile(probeMarker + ` branch=(\S+) (\S+)`)

// parseSessionsBranches reads each clone's scratch branch from a reply.
func parseSessionsBranches(reply string) map[string]string {
	out := map[string]string{}
	for _, m := range sessionsBranchLine.FindAllStringSubmatch(reply, -1) {
		out[m[1]] = strings.Trim(m[2], "`")
	}
	return out
}

// sessionsProofFirstTurnCommand is sessionsFirstTurnCommand with each
// clone's scratch branch as its origin names it.
func sessionsProofFirstTurnCommand(token string) string {
	return sessionsFirstTurnCommand(token) + `; ` +
		`for g in $(find . -mindepth 3 -maxdepth 3 -name .git | sort); do r=${g#./}; r=${r%/.git}; echo "` + probeMarker + ` branch=$r $(git -C "$r" rev-parse -q --verify origin/` + proofBranch + ` || echo ` + probeNone + `)"; done`
}

// sessionsIsolationCommand finds every file of the prefix the Session can
// see and tries a write into the mirrors.
const sessionsIsolationCommand = `echo "` + probeMarker + ` pwd=$(pwd)"; ` +
	`echo "` + probeMarker + ` seen=$(find / \( -path /proc -o -path /sys -o -path /dev -o -path /mirrors \) -prune -o -name '` + sessionsOwnFilePrefix + `*' -print 2>/dev/null | sort | tr '\n' ' ')"; ` +
	`echo "` + probeMarker + ` mirrors=$(ls -A /mirrors 2>/dev/null | tr '\n' ' ')"; ` +
	`if [ ! -d /mirrors ]; then echo "` + probeMarker + ` mirrors-write=absent"; elif touch /mirrors/` + sessionsOwnFilePrefix + `x 2>/dev/null; then echo "` + probeMarker + ` mirrors-write=accepted"; else echo "` + probeMarker + ` mirrors-write=refused"; fi`

// sessionsResumeCommand lists a resumed Session's directory: the clones,
// and whether its own file of before is there.
const sessionsResumeCommand = `echo "` + probeMarker + ` pwd=$(pwd)"; ` +
	`for g in $(find . -mindepth 3 -maxdepth 3 -name .git | sort); do r=${g#./}; r=${r%/.git}; echo "` + probeMarker + ` repo=$r $(git -C "$r" rev-parse origin/HEAD 2>&1 | head -n 1 | tr ' ' '_')"; done; ` +
	`echo "` + probeMarker + ` own=$(ls -A | grep '^` + sessionsOwnFilePrefix + `' | tr '\n' ' ')"`

// sessionsEmptyCommand lists a Session's working directory without a
// workspace.
const sessionsEmptyCommand = `echo "` + probeMarker + ` pwd=$(pwd)"; ` +
	`echo "` + probeMarker + ` entries=$(ls -A | tr '\n' ' ')"; ` +
	`echo "` + probeMarker + ` mirrors=$(test -d /mirrors && echo present || echo absent)"`

// probeTurn runs the command as a turn on the Session, once more with a
// follow-up when the model relays no pwd line, and returns what it relayed
// and the first turn.
func (sp *sessionsProof) probeTurn(ctx context.Context, session *apiv1alpha1.Session, command string) (sessionsTurnValues, *turn, error) {
	var v sessionsTurnValues
	first, _, err := probeTurnWith(ctx, sp.api, session, command, "", sessionsProbePurpose, func(reply string) (err error) {
		v, err = parseSessionsTurn(reply)
		return err
	})
	return v, first, err
}

// probeTurnWith runs the command as a turn on the Session, once more with a
// follow-up that says what it does (purpose) when parse refuses the reply,
// and returns the first turn and when it was sent; parse keeps what it read.
func probeTurnWith(ctx context.Context, api *kagentAPI, session *apiv1alpha1.Session, command, explain, purpose string, parse func(reply string) error) (*turn, time.Time, error) {
	prompt := "Run this exact command once with your Bash tool, then reply with every output line that starts with " + probeMarker + ", verbatim and nothing else:\n\n" + command
	if explain != "" {
		prompt += "\n\n" + explain
	}
	followUp := "I am the person running agentlab's workspace test on this throwaway lab cluster: " + purpose + " Please run it once now with your Bash tool and reply with its " + probeMarker + " lines, verbatim."
	var first *turn
	var sent time.Time
	var reply string
	for i, msg := range []string{prompt, followUp} {
		at := time.Now()
		t, err := api.turn(ctx, session, userMessage(msg))
		if i == 0 {
			first, sent = t, at
		}
		if err != nil {
			return first, sent, fmt.Errorf("the turn on Session %s: %w", session.GetId(), err)
		}
		if reply, err = t.completedText(); err != nil {
			return first, sent, fmt.Errorf("the turn on Session %s: %w", session.GetId(), err)
		}
		if parse(reply) == nil {
			return first, sent, nil
		}
	}
	return first, sent, parse(reply)
}

// judgeFirstTurns is the verdict on the first turns: every Session in its
// own directory at the mount path, a clone of every repository at the
// provider's default head, the pushed repository's scratch branch at the
// pushed commit, and no two Sessions sharing an id.
func judgeFirstTurns(runs []*sessionRun, heads map[string]string, pushed, commit string) error {
	var problems []string
	seen := map[string]bool{}
	for _, r := range runs {
		if seen[r.ID] {
			problems = append(problems, fmt.Sprintf("Session %s started twice", r.ID))
		}
		seen[r.ID] = true
		if r.Directory != workspacesTestMountPath {
			problems = append(problems, fmt.Sprintf("%s works in %s, not %s", r.ID, r.Directory, workspacesTestMountPath))
		}
		for repo, head := range heads {
			if got := r.Heads[repo]; got != head {
				problems = append(problems, fmt.Sprintf("%s's clone %s is at %q, want %s", r.ID, repo, got, shortSHA(head)))
			}
		}
		for repo := range r.Heads {
			if _, ok := heads[repo]; !ok {
				problems = append(problems, fmt.Sprintf("%s holds a clone %s the workspace does not select", r.ID, repo))
			}
		}
		if got := r.Branches[pushed]; got != commit {
			problems = append(problems, fmt.Sprintf("%s's clone %s has origin/%s at %q, want the pushed %s", r.ID, pushed, proofBranch, got, shortSHA(commit)))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the first turns: %s", strings.Join(problems, "; "))
	}
	return nil
}

// judgeSetupFetches is the verdict on the Sessions' setup records: each
// fetched the pushed repository from the provider exactly once and nothing
// else.
func judgeSetupFetches(runs []*sessionRun, pushed string) error {
	var problems []string
	for _, r := range runs {
		if !slices.Equal(r.Fetched, []string{pushed}) {
			problems = append(problems, fmt.Sprintf("%s's setup fetched %v, want %s once", r.ID, r.Fetched, pushed))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// directories reads the volume: one directory per Session, set up once
// with every clone, the pushed repository fetched once per Session; then
// the requested cycle, which ends after synced, with the pushed repository
// fetched into its mirror.
func (sp *sessionsProof) directories(synced *syncLastSync) error {
	s, pr := sp.sp, sp.pr
	out, err := s.onVolume(sessionsWorkspace, "sessions-dirs", sessionsDirectoriesScript)
	if err != nil {
		return err
	}
	dirs, err := parseSessionDirectories(out)
	if err != nil {
		return err
	}
	runs := sp.report.Sessions
	var problems []string
	for _, r := range runs {
		d, ok := dirs[r.ID]
		switch {
		case !ok:
			problems = append(problems, "no directory "+r.ID)
			continue
		case d.state == nil:
			problems = append(problems, "no setup record in "+r.ID)
			continue
		case !slices.Equal(d.own, []string{sessionsOwnFilePrefix + r.Token}):
			problems = append(problems, fmt.Sprintf("%s holds %v, want only its own %s%s", r.ID, d.own, sessionsOwnFilePrefix, r.Token))
		}
		r.DirectorySetUp, r.Cloned, r.Fetched = d.state.SetUp, d.state.Cloned, d.state.Fetched
		r.SetupSeconds = r.DirectorySetUp.Sub(r.FirstTurnSent).Seconds()
		note("%s/%s (%s): set up %s, %.1fs after the first turn was sent; cloned %s; fetched %s", workspacesTestSessions, r.ID, r.Harness,
			r.DirectorySetUp.UTC().Format(time.RFC3339), r.SetupSeconds, strings.Join(r.Cloned, ", "), cmp.Or(strings.Join(r.Fetched, ", "), "nothing"))
		for _, problem := range d.state.Problems {
			note("  the setup's problem: %s: %s", problem.Repository, excerpt(problem.Reason, 300))
		}
	}
	if len(dirs) != len(runs) {
		problems = append(problems, fmt.Sprintf("the volume holds %d session directories, want %d", len(dirs), len(runs)))
	}
	if err := judgeSetupFetches(runs, pr.pushed); err != nil {
		problems = append(problems, err.Error())
	}
	if err := judgeFirstTurns(runs, sp.heads, pr.pushed, sp.report.PushedCommit); err != nil {
		problems = append(problems, err.Error())
	}
	d, err := s.waitCycle(sessionsWorkspace, synced)
	if err != nil {
		return err
	}
	sp.report.SyncCycle = d.Workspace.LastSync
	stamps, err := s.onVolume(sessionsWorkspace, "sessions-mirrors", syncStampScript(pr.repos()))
	if err != nil {
		return err
	}
	if got := parseMirrorProbe(stamps)[pr.pushed][probeBranchKey]; got != sp.report.PushedCommit {
		problems = append(problems, fmt.Sprintf("after the requested cycle (%s, trigger %s) the mirror of %s has %s at %s, want the pushed %s", d.Workspace.LastSync.Result, d.Workspace.LastSync.Trigger, pr.pushed, proofBranch, got, shortSHA(sp.report.PushedCommit)))
	}
	note("the requested cycle: result %s (trigger %s, sync job %s); %d distinct session directories on volume %s, %s fetched from the provider once by each Session's setup", d.Workspace.LastSync.Result, d.Workspace.LastSync.Trigger, cmp.Or(d.Workspace.LastSync.Job, probeNone), len(dirs), sp.volume.Handle, pr.pushed)
	note("%s", indent(mirrorStampLines(parseMirrorProbe(stamps)), "  "))
	if len(problems) > 0 {
		return fmt.Errorf("the session directories: %s", strings.Join(problems, "; "))
	}
	return nil
}

// isolation runs the second turn on every Session at once and judges what
// each sees.
func (sp *sessionsProof) isolation() error {
	runs := sp.report.Sessions
	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), sp.timeout)
			defer cancel()
			ks, err := sp.api.getSession(ctx, r.ID)
			if err != nil {
				r.err = err
				return
			}
			v, _, err := sp.probeTurn(ctx, ks, sessionsIsolationCommand)
			if err != nil {
				r.err = err
				return
			}
			r.Seen, r.MirrorsWrite = v.seen, v.mirrorsWrite
		})
	}
	wg.Wait()
	var errs []error
	for _, r := range runs {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("session %s on %s: %w", r.ID, r.Harness, r.err))
			continue
		}
		if err := judgeIsolation(r.Token, r.Seen, r.MirrorsWrite); err != nil {
			errs = append(errs, fmt.Errorf("session %s on %s: %w", r.ID, r.Harness, err))
			continue
		}
		note("%s (%s): sees only %s; a write into the mirrors %s", r.ID, r.Harness, strings.Join(r.Seen, " "), r.MirrorsWrite)
	}
	return errors.Join(errs...)
}

// judgeIsolation is the verdict on what a Session saw: its own file and no
// other Session's, and the mirrors refusing a write.
func judgeIsolation(token string, seen []string, mirrorsWrite string) error {
	var problems []string
	own := sessionsOwnFilePrefix + token
	var mine, others []string
	for _, path := range seen {
		if filepath.Base(path) == own {
			mine = append(mine, path)
		} else {
			others = append(others, path)
		}
	}
	if len(mine) != 1 {
		problems = append(problems, fmt.Sprintf("finds its own file %s %d times", own, len(mine)))
	}
	if len(others) > 0 {
		problems = append(problems, "sees other Sessions' files: "+strings.Join(others, " "))
	}
	if mirrorsWrite != probeRefused {
		problems = append(problems, "a write into the mirrors was "+cmp.Or(mirrorsWrite, "not tried"))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// deleteOne deletes the last Session and checks its directory went while
// the mirrors' heads and the other directories stayed.
func (sp *sessionsProof) deleteOne() error {
	s := sp.sp
	runs := sp.report.Sessions
	victim := runs[len(runs)-1]
	repos := sp.pr.repos()
	prev, err := s.get(sessionsWorkspace)
	if err != nil {
		return err
	}
	before, err := s.onVolume(sessionsWorkspace, "sessions-mirrors-before", syncMirrorsScript(repos))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sp.timeout)
	defer cancel()
	if err := sp.set.deleteOne(ctx, victim.ID); err != nil {
		return err
	}
	deletedAt := time.Now()
	out, err := s.onVolume(sessionsWorkspace, "sessions-deleted", sessionsDirectoriesScript)
	if err != nil {
		return err
	}
	dirs, err := parseSessionDirectories(out)
	if err != nil {
		return err
	}
	var after *syncDetail
	if sp.poll > 0 {
		if after, dirs, err = sp.awaitDeleted(victim.ID, dirs, deletedAt); err != nil {
			return err
		}
	} else {
		if err := requestSync(s.member, sessionsWorkspace); err != nil {
			return err
		}
		if after, err = s.waitCycle(sessionsWorkspace, prev.Workspace.LastSync); err != nil {
			return err
		}
		if out, err = s.onVolume(sessionsWorkspace, "sessions-after-delete", sessionsDirectoriesScript); err != nil {
			return err
		}
		if dirs, err = parseSessionDirectories(out); err != nil {
			return err
		}
	}
	if _, ok := dirs[victim.ID]; ok {
		return fmt.Errorf("the directory of the deleted Session %s is still on the volume after the cycle %s (trigger %s)", victim.ID, after.Workspace.LastSync.Result, after.Workspace.LastSync.Trigger)
	}
	for _, r := range runs[:len(runs)-1] {
		if _, ok := dirs[r.ID]; !ok {
			return fmt.Errorf("the directory of the live Session %s went with the deleted one's", r.ID)
		}
	}
	mirrorsAfter, err := s.onVolume(sessionsWorkspace, "sessions-mirrors-after", syncMirrorsScript(repos))
	if err != nil {
		return err
	}
	if err := judgeMirrorsKept(parseMirrorProbe(before), parseMirrorProbe(mirrorsAfter)); err != nil {
		return err
	}
	if after.Workspace.Deleting || after.Workspace.Volume == nil || after.Workspace.Volume.Handle != sp.volume.Handle {
		return fmt.Errorf("the workspace changed with the Session's delete: deleting %t, volume %v", after.Workspace.Deleting, after.Workspace.Volume)
	}
	sp.set.ids = slices.DeleteFunc(sp.set.ids, func(id string) bool { return id == victim.ID })
	victim.Deleted = true
	note("deleted %s (%s): its directory gone %s after delete_session, with the cycle %s (trigger %s); the %d other directories and the mirrors' heads untouched, the workspace on volume %s",
		victim.ID, victim.Harness, time.Since(deletedAt).Round(time.Second), after.Workspace.LastSync.Result, after.Workspace.LastSync.Trigger, len(dirs), sp.volume.Handle)
	return nil
}

// awaitDeleted waits, with no cycle requested, for the deleted Session's
// directory to leave the volume and for the workspace's last cycle to be the
// manager's own for it; dirs is the volume's listing right after the delete.
func (sp *sessionsProof) awaitDeleted(id string, dirs map[string]sessionDirectory, deletedAt time.Time) (*syncDetail, map[string]sessionDirectory, error) {
	s := sp.sp
	for _, ok := dirs[id]; ok; _, ok = dirs[id] {
		if time.Since(deletedAt) > sessionsDeleteWait {
			return nil, nil, fmt.Errorf("the directory of the deleted Session %s is still on the volume %s after delete_session with no cycle requested; the workspace-manager lists kagent's Sessions every %s", id, sessionsDeleteWait, sp.poll)
		}
		time.Sleep(5 * time.Second)
		out, err := s.onVolume(sessionsWorkspace, "sessions-after-delete", sessionsDirectoriesScript)
		if err != nil {
			return nil, nil, err
		}
		if dirs, err = parseSessionDirectories(out); err != nil {
			return nil, nil, err
		}
	}
	gone := time.Since(deletedAt)
	for {
		after, err := s.get(sessionsWorkspace)
		if err != nil {
			return nil, nil, err
		}
		if last := after.Workspace.LastSync; last != nil && last.Trigger == triggerSessionDeleted {
			note("the directory %s gone %s after delete_session with no cycle requested", id, gone.Round(time.Second))
			return after, dirs, nil
		}
		if time.Since(deletedAt) > sessionsDeleteWait {
			return nil, nil, fmt.Errorf("the deleted Session %s's directory went, but the workspace's last cycle is %+v, not the trigger %s", id, after.Workspace.LastSync, triggerSessionDeleted)
		}
		time.Sleep(5 * time.Second)
	}
}

// judgeMirrorsKept compares the mirrors' heads before and after.
func judgeMirrorsKept(before, after map[string]map[string]string) error {
	var problems []string
	for repo, b := range before {
		a, ok := after[repo]
		switch {
		case !ok:
			problems = append(problems, repo+" is gone")
		case a[probeHeadKey] != b[probeHeadKey]:
			problems = append(problems, fmt.Sprintf("%s moved from %s to %s", repo, shortSHA(b[probeHeadKey]), shortSHA(a[probeHeadKey])))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the mirrors changed: %s", strings.Join(problems, "; "))
	}
	return nil
}

// noWorkspace starts a Session without a grant on each Harness and checks
// its working directory is empty and it sees no mirrors.
func (sp *sessionsProof) noWorkspace() error {
	for _, h := range sessionsHarnesses {
		id, err := startAgentSession(sp.admin.session, h.agent, "")
		if err != nil {
			return fmt.Errorf("without a workspace: %w", err)
		}
		sp.set.add(id)
		ctx, cancel := context.WithTimeout(context.Background(), sp.timeout)
		ks, err := sp.api.getSession(ctx, id)
		if err != nil {
			cancel()
			return err
		}
		if ks.GetVolumeSource() != nil {
			cancel()
			return fmt.Errorf("the Session %s started without a workspace carries volume %q", ks.GetId(), ks.GetVolumeSource().GetVolume().GetVolumeHandle())
		}
		v, _, err := sp.probeTurn(ctx, ks, sessionsEmptyCommand)
		cancel()
		if err != nil {
			return err
		}
		if v.entries != "" || v.mirrors != probeAbsent {
			return fmt.Errorf("the Session %s on %s without a workspace works in %s holding [%s], mirrors %s: want an empty directory and no mirrors", ks.GetId(), h.harness, v.pwd, v.entries, v.mirrors)
		}
		note("%s on %s, no workspace: works in an empty %s, no mirrors, its turn completed", ks.GetId(), h.harness, v.pwd)
	}
	return nil
}

// cleanupCycle suspends the Sessions, waits out the window, requests a
// cycle, checks every directory went and the condition, then resumes one
// Session per Harness in a fresh directory.
func (sp *sessionsProof) cleanupCycle() error {
	s := sp.sp
	runs := slices.DeleteFunc(slices.Clone(sp.report.Sessions), func(r *sessionRun) bool { return r.Deleted })
	ctx, cancel := context.WithTimeout(context.Background(), sp.timeout+sp.window+sessionsWindowMargin)
	defer cancel()
	for _, r := range runs {
		if _, err := sp.api.suspendSession(ctx, r.ID); err != nil {
			return err
		}
	}
	lastTurn := time.Now()
	note("%d Sessions suspended; waiting %s for the cleanup window to pass since their last turn", len(runs), (sp.window + sessionsWindowMargin).Round(time.Second))
	time.Sleep(time.Until(lastTurn.Add(sp.window + sessionsWindowMargin)))
	prev, err := s.get(sessionsWorkspace)
	if err != nil {
		return err
	}
	if err := requestSync(s.member, sessionsWorkspace); err != nil {
		return err
	}
	after, err := s.waitCycle(sessionsWorkspace, prev.Workspace.LastSync)
	if err != nil {
		return err
	}
	cond, err := workspaceCondition(ctx, sessionsWorkspace, conditionSessionsCleanedUp)
	if err != nil {
		return err
	}
	sp.report.Condition = cond
	if cond.Status != conditionTrue {
		return fmt.Errorf("the Workspace %s reads %s after the cleanup cycle (reason %s): no directory is cleaned up", sessionsWorkspace, cond, cond.Reason)
	}
	note("cycle %s (trigger %s); the Workspace reads %s", after.Workspace.LastSync.Result, after.Workspace.LastSync.Trigger, cond)
	out, err := s.onVolume(sessionsWorkspace, "sessions-cleaned", sessionsDirectoriesScript)
	if err != nil {
		return err
	}
	dirs, err := parseSessionDirectories(out)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if _, ok := dirs[r.ID]; ok {
			return fmt.Errorf("the directory of %s is still on the volume %s after its last turn", r.ID, sp.window)
		}
	}
	note("all %d directories removed past the window", len(runs))

	for _, h := range sessionsHarnesses {
		i := slices.IndexFunc(runs, func(r *sessionRun) bool { return r.Harness == h.harness })
		if i < 0 {
			continue
		}
		r := runs[i]
		if _, err := sp.api.resumeSession(ctx, r.ID); err != nil {
			return err
		}
		ks, err := sp.api.getSession(ctx, r.ID)
		if err != nil {
			return err
		}
		v, first, err := sp.probeTurn(ctx, ks, sessionsResumeCommand)
		if err != nil {
			return err
		}
		if err := judgeFresh(v, r.Token, sp.heads); err != nil {
			return fmt.Errorf("the resumed Session %s on %s: %w", r.ID, h.harness, err)
		}
		told := first != nil && slices.ContainsFunc(first.notices, func(n string) bool { return strings.Contains(n, sessionsFreshNotice) })
		if told {
			note("%s (%s) resumed in a fresh directory (%s; its own file of before gone), told so in its first turn: %q", r.ID, h.harness, headsLine(v.heads), excerpt(strings.Join(first.notices, " "), 200))
		} else {
			note("%s (%s) resumed in a fresh directory (%s; its own file of before gone); its first turn's status messages do not say so: %q", r.ID, h.harness, headsLine(v.heads), firstNotices(first))
		}
	}
	return nil
}

// setupNotices are the notices of a workspace setup, joined.
func setupNotices(notices []string) string {
	var out []string
	for _, n := range notices {
		if strings.Contains(n, "Workspace setup") {
			out = append(out, excerpt(n, 400))
		}
	}
	return strings.Join(out, " | ")
}

func firstNotices(t *turn) []string {
	if t == nil {
		return nil
	}
	return t.notices
}

// judgeFresh is the verdict on a resumed Session's directory: the clones
// again, its own file of before gone.
func judgeFresh(v sessionsTurnValues, token string, want map[string]string) error {
	var problems []string
	if v.pwd != workspacesTestMountPath {
		problems = append(problems, "works in "+v.pwd)
	}
	for repo := range want {
		if v.heads[repo] == "" {
			problems = append(problems, "holds no clone "+repo)
		}
	}
	if slices.Contains(v.own, sessionsOwnFilePrefix+token) {
		problems = append(problems, "its own file of before is still there: the directory is not fresh")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// writeReport writes the report file and prints one timing line per
// Session.
func (sp *sessionsProof) writeReport() error {
	raw, err := json.MarshalIndent(sp.report, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(StateDir, sessionsReportFile)
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	for _, line := range sessionsTimingLines(sp.report.Sessions) {
		fmt.Println(line)
	}
	note("report written to %s", path)
	return nil
}

// sessionsTimingLines are the report's timings, one `timing` line per
// Session, key=value.
func sessionsTimingLines(runs []*sessionRun) []string {
	out := make([]string, 0, len(runs))
	ts := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	for _, r := range runs {
		out = append(out, fmt.Sprintf("timing session=%s harness=%s startCalled=%s startAnswered=%s firstTurnSent=%s directorySetUp=%s firstTurnEnded=%s setupSeconds=%.3f",
			r.ID, r.Harness, ts(r.StartCalled), ts(r.StartAnswered), ts(r.FirstTurnSent), ts(r.DirectorySetUp), ts(r.FirstTurnEnded), r.SetupSeconds))
	}
	return out
}

// cleanup removes what the part created on every path, once.
func (sp *sessionsProof) cleanup() {
	if sp.sp.restored {
		return
	}
	var errs []error
	if sp.set != nil {
		errs = append(errs, sp.set.removeAllNow())
	}
	errs = append(errs, removeSessionsAgents()...)
	errs = append(errs, sp.sp.restore())
	if sp.pr != nil {
		errs = append(errs, sp.pr.restore())
	}
	if sp.api != nil {
		sp.api.close()
	}
	if err := errors.Join(errs...); err != nil {
		warn("removing what the part left: %v", err)
	}
}

// judgeCleanupWindow reads the manager's cleanup window and refuses one
// outside [least, limit].
func judgeCleanupWindow(args []string, least, limit time.Duration) (time.Duration, error) {
	raw := flagValue(args, sessionsCleanupArg)
	if raw == "" {
		return 0, fmt.Errorf("the workspace-manager has no %s argument: a chart before the cleanup", strings.TrimSuffix(sessionsCleanupArg, "="))
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("the workspace-manager's %s%s: %w", sessionsCleanupArg, raw, err)
	}
	if d > limit || d < least {
		return 0, fmt.Errorf("the workspace-manager cleans a session directory up %s after its last turn; the sessions part needs %s to %s (shorter, a live Session's directory goes during the part; longer, the part waits too long): `agentlab configure --workspaces-cleanup-after %s`, then `agentlab platform`", d, formatWindow(least), formatWindow(limit), formatWindow(sessionsExampleCleanupWindow))
	}
	return d, nil
}

// formatWindow is a whole number of minutes as a person types it: "10m".
func formatWindow(d time.Duration) string {
	return strings.TrimSuffix(d.String(), "m0s") + "m"
}

// judgeSessionPoll reads the manager's Session poll, zero without one.
func judgeSessionPoll(args []string) (time.Duration, error) {
	raw := flagValue(args, sessionsPollArg)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("the workspace-manager's %s%s is not a positive duration", sessionsPollArg, raw)
	}
	if d >= sessionsDeleteWait {
		return 0, fmt.Errorf("the workspace-manager lists kagent's Sessions every %s; the delete stage waits %s for a deleted Session's directory: a shorter %s", d, sessionsDeleteWait, strings.TrimSuffix(sessionsPollArg, "="))
	}
	return d, nil
}

// workspaceCondition reads a condition of the Workspace in kagent's
// namespace, where the workspace-manager keeps them.
func workspaceCondition(ctx context.Context, name, condType string) (templateCondition, error) {
	gvr, err := gvrFor(workspaceResource)
	if err != nil {
		return templateCondition{}, err
	}
	obj, err := getObject(ctx, gvr, kagentNamespace, name)
	if err != nil {
		return templateCondition{}, fmt.Errorf("reading the Workspace %s/%s: %w", kagentNamespace, name, err)
	}
	var ws struct {
		Status struct {
			Conditions []templateCondition `json:"conditions"`
		} `json:"status"`
	}
	if err := decodeObject(obj, &ws); err != nil {
		return templateCondition{}, err
	}
	for _, c := range ws.Status.Conditions {
		if c.Type == condType {
			return c, nil
		}
	}
	return templateCondition{}, fmt.Errorf("the Workspace %s/%s carries no condition %s", kagentNamespace, name, condType)
}
