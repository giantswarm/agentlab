package lab

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
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

// `agentlab workspaces-test --part sessions`: Sessions started in parallel
// on one workspace, each in its own session directory on the workspace's
// volume, on the claude and the kagent (Go ADK) Harness, and the session
// directories' cleanup. On the sync part's filter workspace, after a push to
// one repository since its last sync:
//
//   - the Sessions (ten by default, half on each Harness) started at once
//     through agent-manager's start_session as the admin, each with the
//     admin's grant from authorize_workspace, which names the pushed
//     repository changed since the sync;
//   - each Session's first turn lists its working directory and the head of
//     every clone at <owner>/<repository>, the pushed repository at its new
//     head, and writes a file only it knows; the lab GitHub's request log
//     shows exactly one fetch of the pushed repository per Session and none
//     of the others; the volume holds one directory per Session, each set up
//     once (.kagent/workspace.json, the setup's own record);
//   - isolation: a second turn on each Session finds only its own file
//     anywhere it can look, and a write into the mirrors refused; on the
//     volume each file is in its own Session's directory alone;
//   - a Session deleted: its directory gone with no cycle requested (a
//     cycle with the trigger sessionDeleted; at the next cycle on a manager
//     before the Session poll), the mirrors' heads and every other directory
//     untouched;
//   - a Session started without a workspace: an empty working directory, no
//     mirrors, a turn as before;
//   - the cleanup, with the lab's short window
//     (platform.workspaces.sessionsCleanupAfter): the Sessions suspended,
//     the window waited out, a cycle removes every directory and the
//     Workspace reads SessionsCleanedUp=True; one Session per Harness resumed
//     works in a fresh directory, and its first turn says so.
//
// Per Session the report records when start_session was called and answered,
// when the first turn was sent, when the setup recorded the directory set up
// and when the first turn ended: the measurements read them from the report
// file (sessionsReportFile) or the `timing` lines.

// WorkspacesPartSessions is the sessions part of workspaces-test.
const WorkspacesPartSessions = "sessions"

// DefaultWorkspacesSessions is the number of Sessions the part starts.
const DefaultWorkspacesSessions = 10

const (
	// sessionsKagentAgent is the part's Agent on the Go ADK Harness; its
	// claude Agent is the harness stage's (workspacesHarnessAgent).
	sessionsKagentAgent = "agentlab-workspaces-kagent"
	// sessionsOwnFilePrefix names the file each Session writes into its own
	// directory, followed by a token only it knows.
	sessionsOwnFilePrefix = "agentlab-only-"
	// SessionsMaxCleanupWindow bounds the cleanup window the part waits out;
	// SessionsMinCleanupWindow keeps the live Sessions' directories through
	// the cycles before the cleanup stage. The part's precondition and the
	// help of `configure --workspaces-cleanup-after` both read them.
	SessionsMaxCleanupWindow = 15 * time.Minute
	SessionsMinCleanupWindow = 5 * time.Minute
	// SessionsExampleCleanupWindow is the window the part's messages and the
	// flag's help suggest; it lies inside the range.
	SessionsExampleCleanupWindow = 10 * time.Minute
	// sessionsWindowMargin is waited past the window, for the clocks.
	sessionsWindowMargin = 20 * time.Second
	// sessionsReportFile is the part's report, in the state directory.
	sessionsReportFile = "workspaces-sessions-report.json"
	// sessionsFreshNotice is the words of the runtime's notice that a
	// returning Session's directory was set up again.
	sessionsFreshNotice = "set up again"
	// sessionsCleanupArg is the workspace-manager's flag of the window.
	sessionsCleanupArg = "--sessions-cleanup-after="
	// sessionsKagentArg is the workspace-manager's flag of the kagent
	// controller its cleanup lists Sessions from.
	sessionsKagentArg = "--kagent-address="
	// sessionsPollArg is the workspace-manager's flag of how often it lists
	// kagent's Sessions for a deleted one, whose directory then goes at
	// once (workspace-manager 0.6.0 and newer).
	sessionsPollArg = "--sessions-poll="
	// sessionsDeleteWait is how long a deleted Session's directory may stay
	// on the volume with no cycle requested.
	sessionsDeleteWait = 2 * time.Minute
	// triggerSessionDeleted is the cycle the manager runs for a deleted
	// Session.
	triggerSessionDeleted = "sessionDeleted"
	// workspaceResource is the workspace-manager's Workspace.
	workspaceResource          = "workspaces.workspace-manager.giantswarm.io"
	conditionSessionsCleanedUp = "SessionsCleanedUp"
	// workspaceStateFile is the setup's record in a session directory.
	workspaceStateFile = ".kagent/workspace.json"
	// startSessionRequestIDArg is start_session's idempotency key.
	startSessionRequestIDArg = "requestId"
	// The probes' words: a write refused, a directory absent, a mirror's
	// head.
	probeRefused = "refused"
	probeAbsent  = "absent"
	probeHeadKey = "head"
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
	// SetupSeconds is the setup's time from the first turn sent to the
	// directory set up.
	SetupSeconds float64 `json:"setupSeconds"`
	// What the first turn relayed: the working directory and each clone's
	// head (origin/HEAD), and the setup's record.
	Directory string            `json:"directory"`
	Heads     map[string]string `json:"heads"`
	Cloned    []string          `json:"cloned"`
	Fetched   []string          `json:"fetched"`
	// Isolation: the files of the prefix the second turn found, and its
	// write into the mirrors.
	Seen         []string `json:"seen"`
	MirrorsWrite string   `json:"mirrorsWrite"`
	// Deleted is the Session the part deleted: its timings stay in the
	// report, the cleanup cycle no longer waits on it.
	Deleted bool `json:"deleted,omitempty"`
	// Notices are the first turn's status messages: the setup's word on
	// what it could not clone or fetch.
	Notices []string `json:"notices,omitempty"`
	err     error
}

// sessionsReport is the part's report file.
type sessionsReport struct {
	Workspace     string            `json:"workspace"`
	Volume        string            `json:"volume"`
	Moved         string            `json:"moved"`
	MovedHead     string            `json:"movedHead"`
	Fetches       map[string]int    `json:"fetches"`
	Sessions      []*sessionRun     `json:"sessions"`
	CleanupWindow string            `json:"cleanupWindow"`
	Condition     templateCondition `json:"sessionsCleanedUp"`
}

// sessionsProof is the part's state.
type sessionsProof struct {
	sp       *syncProof
	timeout  time.Duration
	sessions int
	admin    *workspacePerson
	token    string
	api      *kagentAPI
	set      *sessionSet
	volume   workspaceVolume
	window   time.Duration
	// poll is the manager's Session poll; zero for a manager that removes a
	// deleted Session's directory only at its next cycle.
	poll   time.Duration
	report sessionsReport
}

// workspacesSessionsPart runs the part in stages, as the admin.
func workspacesSessionsPart(cfg *config.Config, sessions int, timeout time.Duration) error {
	p := newProofStages()
	if sessions <= 0 {
		sessions = DefaultWorkspacesSessions
	}
	sp := &sessionsProof{sp: newSyncProof(cfg, timeout, syncFilterWorkspace), timeout: timeout, sessions: sessions}
	err := sp.run(p)
	sp.cleanup()
	if err == nil {
		fmt.Printf("\nWorkspaces sessions part passed: %d Sessions started in parallel through start_session on %s, each in its own directory on volume %s with a clone of every repository, %s fetched once per Session and nothing else; each Session isolated, the mirrors read-only; a deleted Session's directory gone with no cycle requested (at the next cycle before workspace-manager 0.6.0); a Session without a workspace in an empty directory; after the %s cleanup window every directory removed, %s, and a resumed Session in a fresh directory. Report: %s\n",
			len(sp.report.Sessions), syncFilterWorkspace, sp.volume.Handle, syncMovingRepo, sp.window, sp.report.Condition, filepath.Join(StateDir, sessionsReportFile))
	}
	return p.close(err)
}

func (sp *sessionsProof) run(p *proofStages) error {
	s, cfg := sp.sp, sp.sp.cfg
	adminUser := cfg.AdminUser()
	if adminUser == nil {
		return fmt.Errorf("the sessions part needs an admin user in %s (`agentlab configure --defaults` writes one)", config.File)
	}
	p.begin("sessions: preconditions", "The sessions part's preconditions: the workspace-manager with kagent and a short cleanup window, the lab's GitHub, the Session admission, the Agents on the claude and the kagent Harness, %s signed in", adminUser.Email)
	if err := s.preconditions(adminUser.Email); err != nil {
		return err
	}
	if err := sp.preconditions(adminUser); err != nil {
		return err
	}

	p.begin("sessions: workspace", "create_workspace %s with the sync part's filters, its first cycle", syncFilterWorkspace)
	if err := sp.createWorkspace(); err != nil {
		return err
	}

	p.begin("sessions: push", "A push to %s since the sync, the admin connected to %s", syncMovingRepo, config.WorkspaceProviderFake)
	pushed, err := s.push()
	if err != nil {
		return err
	}
	sp.report.MovedHead = pushed
	hops, err := connectWorkspaceProvider(cfg, sp.admin, config.WorkspaceProviderFake)
	if err != nil {
		return err
	}
	note("%s connected to %s: %s", adminUser.Email, config.WorkspaceProviderFake, hops)
	mark, err := s.mark()
	if err != nil {
		return err
	}

	p.begin("sessions: start", "%d Sessions started at once through start_session with the admin's grant, alternating the claude and the kagent Harness, each first turn listing its directory and every clone's head", sp.sessions)
	if err := sp.startAll(); err != nil {
		return err
	}

	p.begin("sessions: directories", "One directory per Session on the volume, %s fetched once per Session and nothing else", syncMovingRepo)
	if err := sp.directories(mark); err != nil {
		return err
	}

	p.begin("sessions: isolation", "A second turn on each Session: only its own file anywhere it looks, the mirrors refusing a write; on the volume each file in its own directory alone")
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

	p.begin("sessions: cleanup", "The Sessions suspended, the %s window waited out: a cycle removes every directory and the Workspace reads %s=True; a resumed Session per Harness works in a fresh directory and is told so", sp.window, conditionSessionsCleanedUp)
	if err := sp.cleanupCycle(); err != nil {
		return err
	}

	p.begin("sessions: report", "The Sessions' timings, written to %s", filepath.Join(StateDir, sessionsReportFile))
	return sp.writeReport()
}

// preconditions checks the workspace-manager's cleanup settings, signs the
// admin in to kagent and readies an Agent on each Harness.
func (sp *sessionsProof) preconditions(adminUser *config.User) error {
	cfg := sp.sp.cfg
	ctx := context.Background()
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
		note("the workspace-manager has no %s argument: before 0.6.0 it removes a deleted Session's directory at its next cycle, which the delete stage requests", strings.TrimSuffix(sessionsPollArg, "="))
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
	note("the Agents list the provider's origin %s as their egress: a Session fetches what moved since the sync from it", cfg.GitHubFakeURL())
	if sp.admin, err = openWorkspacePerson(cfg, adminUser); err != nil {
		return err
	}
	if sp.token, err = passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, adminUser.Email, adminUser.Password, musterLoginScopes); err != nil {
		return err
	}
	if sp.api, err = dialKagentAPI(cfg, sp.token); err != nil {
		return err
	}
	sp.set = &sessionSet{api: sp.api}
	for _, h := range sessionsHarnesses {
		if err := waitHarnessReady(ctx, h.harness, sp.timeout); err != nil {
			return err
		}
		if agentExists(h.agent) {
			if err := removeAgent(h.agent); err != nil {
				return err
			}
		}
		if _, err := applyManifests(ctx, []byte(sessionsAgentManifest(h.agent, h.harness, cfg.GitHubFakeURL()))); err != nil {
			return fmt.Errorf("applying the Agent %s: %w", h.agent, err)
		}
		boot, err := waitAgentReadyOn(h.agent, h.harness, sp.timeout)
		if err != nil {
			return err
		}
		if !boot.ready {
			return fmt.Errorf("the Agent %s is not Ready on Harness %s after %s: %s", h.agent, h.harness, boot.elapsed.Round(time.Second), boot.reason)
		}
		note("the Agent %s Ready on Harness %s in %s", h.agent, h.harness, boot.elapsed.Round(time.Second))
	}
	return nil
}

// sessionsAgentManifest is a workspaces proof's Agent that may reach the
// provider's origin: the egress gateway lets an actor reach only the
// origins its Agent lists besides what its revision compiles, and a Session
// fetches the repositories that moved since the sync from the provider
// before its first turn.
func sessionsAgentManifest(name, harness, providerOrigin string) string {
	return workspacesAgentManifest(name, harness) + fmt.Sprintf("  egress:\n    - %q\n", providerOrigin)
}

// createWorkspace defines the sync part's filter workspace as the admin and
// waits for its first cycle; its volume is the one the grants name.
func (sp *sessionsProof) createWorkspace() error {
	s := sp.sp
	token, err := s.gh.installationToken(syncOrg)
	switch {
	case isNoInstallation(err):
		note("the App of the lab's GitHub is not installed on %s: the workspace selects the public %s alone", syncOrg, syncMovingRepo)
	case err != nil:
		return err
	default:
		s.private, s.tokens[syncOrg] = true, token
	}
	var created syncWorkspace
	if err := s.member.callServerStructured(workspaceManagerToolPrefix+"create_workspace", map[string]any{
		nameKey: syncFilterWorkspace, wmOrganizationArg: config.WorkspaceOrganization, wmSourcesArg: syncFilterSources(s.private),
	}, &created); err != nil {
		return err
	}
	d, err := s.waitCycle(syncFilterWorkspace, nil)
	if err != nil {
		return err
	}
	if r := d.Workspace.LastSync; r.Result != syncResultSucceed {
		return fmt.Errorf("the first cycle of %s ended %s: %s", syncFilterWorkspace, r.Result, r.Message)
	}
	v, err := s.volume(syncFilterWorkspace)
	if err != nil {
		return err
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	pv, err := k.clientset.CoreV1().PersistentVolumes().Get(ctx, v.pv, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the volume %s of workspace %s: %w", v.pv, syncFilterWorkspace, err)
	}
	if pv.Spec.CSI == nil {
		return fmt.Errorf("the volume %s of workspace %s is no CSI volume", v.pv, syncFilterWorkspace)
	}
	sp.volume = workspaceVolume{Driver: pv.Spec.CSI.Driver, Handle: pv.Spec.CSI.VolumeHandle}
	sp.report.Workspace, sp.report.Volume, sp.report.Moved = syncFilterWorkspace, sp.volume.Handle, syncMovingRepo
	note("created %s of Organization %s; first cycle %s; claim %s/%s, volume %s", syncFilterWorkspace, created.Organization, d.Workspace.LastSync.Result, v.namespace, v.claim, sp.volume.Handle)
	s.printManifest(d)
	return nil
}

// startAll starts every Session at once and runs its first turn, then says
// what each relayed; one failed Session fails the stage, naming each.
func (sp *sessionsProof) startAll() error {
	runs := make([]*sessionRun, sp.sessions)
	for i := range runs {
		h := sessionsHarnesses[i%len(sessionsHarnesses)]
		runs[i] = &sessionRun{Harness: h.harness, Agent: h.agent, Token: randomHex(8)}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, r := range runs {
		wg.Go(func() {
			r.err = sp.startOne(r)
			if r.ID != "" {
				mu.Lock()
				sp.set.add(r.ID)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	sp.report.Sessions = runs
	var errs []error
	for _, r := range runs {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("session %s on %s: %w", cmp.Or(r.ID, "(not started)"), r.Harness, r.err))
			continue
		}
		note("%s on %s: works in %s; %s; first turn sent %s after start_session answered, ended in %s",
			r.ID, r.Harness, r.Directory, headsLine(r.Heads), r.FirstTurnSent.Sub(r.StartAnswered).Round(time.Millisecond), r.FirstTurnEnded.Sub(r.FirstTurnSent).Round(time.Second))
		if setup := setupNotices(r.Notices); setup != "" {
			note("  the setup said: %s", setup)
		}
	}
	return errors.Join(errs...)
}

// wantHeads are the heads every clone must name: the provider's for each
// repository the workspace selects, the pushed one's new head.
func (sp *sessionsProof) wantHeads() map[string]string {
	want := map[string]string{syncMovingRepo: sp.report.MovedHead}
	if sp.sp.private {
		if head, err := sp.sp.providerHead(syncPrivateRepo); err == nil {
			want[syncPrivateRepo] = head
		}
	}
	return want
}

// startOne asks for a grant, starts the Session through start_session with
// it and runs the first turn, recording the timings.
func (sp *sessionsProof) startOne(r *sessionRun) error {
	cfg := sp.sp.cfg
	session, err := openMusterSession(cfg, sp.token, "workspaces-test-sessions-"+r.Token)
	if err != nil {
		return err
	}
	person := &workspacePerson{user: sp.admin.user, login: sp.admin.login, session: session}
	a, err := grantWithChange(person, syncMovingRepo, sp.timeout)
	if err != nil {
		return err
	}
	r.StartCalled = time.Now()
	env, err := session.callToolEnvelope(agentManagerToolPrefix+"start_session", map[string]any{
		nameKey: r.Agent, namespaceKey: kagentNamespace, startSessionRequestIDArg: uuid.NewString(), "workspaceGrant": a.Grant,
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
	values, first, sent, err := sp.probeTurn(ctx, ks, sessionsFirstTurnCommand(r.Token))
	r.FirstTurnSent, r.FirstTurnEnded = sent, time.Now()
	r.Notices = firstNotices(first)
	if err != nil {
		return err
	}
	r.Directory, r.Heads = values.pwd, values.heads
	return nil
}

// grantWithChange asks authorize_workspace until the grant names the
// repository changed since the sync: the manager may answer from a listing
// it cached for a minute.
func grantWithChange(p *workspacePerson, changed string, timeout time.Duration) (workspaceAuthorization, error) {
	deadline := time.Now().Add(min(timeout, syncListingFreshness+30*time.Second))
	for {
		a, err := p.authorizeWorkspace(syncFilterWorkspace)
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

// sessionsTurnValues is what a probe turn relayed.
type sessionsTurnValues struct {
	pwd          string
	heads        map[string]string
	entries      string
	ownWrite     string
	mirrorsWrite string
	mirrors      string
	seen         []string
	own          []string
}

var sessionsRepoLine = regexp.MustCompile(probeMarker + ` repo=(\S+) (\S+)`)

// parseSessionsTurn reads the marker lines of a reply.
func parseSessionsTurn(reply string) (sessionsTurnValues, error) {
	values := map[string]string{}
	for _, m := range probeLine.FindAllStringSubmatch(reply, -1) {
		if m[1] != "repo" {
			values[m[1]] = strings.TrimSpace(strings.Trim(strings.TrimSpace(m[2]), "`"))
		}
	}
	if values["pwd"] == "" {
		return sessionsTurnValues{}, fmt.Errorf("the reply carries no %s pwd line (did the agent run its Bash tool?): %s", probeMarker, excerpt(reply, 300))
	}
	v := sessionsTurnValues{pwd: values["pwd"], entries: values["entries"], ownWrite: values["own-write"], mirrorsWrite: values["mirrors-write"],
		mirrors: values["mirrors"], seen: strings.Fields(values["seen"]), own: strings.Fields(values["own"]), heads: map[string]string{}}
	for _, m := range sessionsRepoLine.FindAllStringSubmatch(reply, -1) {
		v.heads[m[1]] = strings.Trim(m[2], "`")
	}
	return v, nil
}

// sessionsFirstTurnCommand lists the working directory and each clone's
// origin/HEAD, and writes the file only this Session knows.
func sessionsFirstTurnCommand(token string) string {
	return `echo "` + probeMarker + ` pwd=$(pwd)"; ` +
		`for g in $(find . -mindepth 3 -maxdepth 3 -name .git | sort); do r=${g#./}; r=${r%/.git}; echo "` + probeMarker + ` repo=$r $(git -C "$r" rev-parse origin/HEAD 2>&1 | head -n 1 | tr ' ' '_')"; done; ` +
		`echo "` + probeMarker + ` entries=$(ls -A | tr '\n' ' ')"; ` +
		`echo ` + token + ` > ./` + sessionsOwnFilePrefix + token + ` && echo "` + probeMarker + ` own-write=$(cat ./` + sessionsOwnFilePrefix + token + `)"`
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
// follow-up when the model relays no pwd line, and returns what it relayed,
// the first turn and when it was sent.
func (sp *sessionsProof) probeTurn(ctx context.Context, session *apiv1alpha1.Session, command string) (sessionsTurnValues, *turn, time.Time, error) {
	var v sessionsTurnValues
	first, sent, err := probeTurnWith(ctx, sp.api, session, command, func(reply string) (err error) {
		v, err = parseSessionsTurn(reply)
		return err
	})
	return v, first, sent, err
}

// probeTurnWith runs the command as a turn on the Session, once more with a
// follow-up when parse refuses the reply, and returns the first turn and
// when it was sent; parse keeps what it read.
func probeTurnWith(ctx context.Context, api *kagentAPI, session *apiv1alpha1.Session, command string, parse func(reply string) error) (*turn, time.Time, error) {
	prompt := "Run this exact command once with your Bash tool, then reply with every output line that starts with " + probeMarker + ", verbatim and nothing else:\n\n" + command
	followUp := "I am the person running agentlab's workspace test on this throwaway lab cluster: the command only lists your working directory and its git clones and writes a marker file. Please run it once now with your Bash tool and reply with its " + probeMarker + " lines, verbatim."
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
// own directory at the mount path, a clone of every repository at its
// head, and no two Sessions sharing an id.
func judgeFirstTurns(runs []*sessionRun, want map[string]string) error {
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
		for repo, head := range want {
			if got := r.Heads[repo]; got != head {
				problems = append(problems, fmt.Sprintf("%s's clone %s is at %q, want %s", r.ID, repo, got, shortSHA(head)))
			}
		}
		for repo := range r.Heads {
			if _, ok := want[repo]; !ok {
				problems = append(problems, fmt.Sprintf("%s holds a clone %s the workspace does not select", r.ID, repo))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the first turns: %s", strings.Join(problems, "; "))
	}
	return nil
}

// headsLine words a clone's heads.
func headsLine(heads map[string]string) string {
	var out []string
	for _, repo := range slices.Sorted(maps.Keys(heads)) {
		out = append(out, repo+" at "+shortSHA(heads[repo]))
	}
	if len(out) == 0 {
		return "no clones"
	}
	return "clones " + strings.Join(out, ", ")
}

// countFetches counts each repository's git fetches in a request log: the
// smart HTTP discovery (GET .../info/refs) that every clone, fetch and
// ls-remote opens with.
func countFetches(reqs []githubRequest) map[string]int {
	out := map[string]int{}
	for _, r := range reqs {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.Path, "/info/refs") {
			continue
		}
		if repo := gitRequestRepository(r.Path); repo != "" {
			out[repo]++
		}
	}
	return out
}

// judgeFetches is the verdict on the fetches of n Sessions: exactly n of
// moved, none of any other repository.
func judgeFetches(fetches map[string]int, moved string, n int) error {
	var problems []string
	if fetches[moved] != n {
		problems = append(problems, fmt.Sprintf("%s fetched %d times, want once per Session (%d)", moved, fetches[moved], n))
	}
	for _, repo := range slices.Sorted(maps.Keys(fetches)) {
		if repo != moved && fetches[repo] > 0 {
			problems = append(problems, fmt.Sprintf("%s fetched %d times, want never: it did not move since the sync", repo, fetches[repo]))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the lab GitHub's request log: %s", strings.Join(problems, "; "))
	}
	return nil
}

// sessionDirectory is a session directory as the volume probe reads it.
type sessionDirectory struct {
	state *workspaceState
	own   []string
}

// workspaceState is the setup's record, .kagent/workspace.json.
type workspaceState struct {
	SetUp    time.Time `json:"setUp"`
	Cloned   []string  `json:"cloned"`
	Fetched  []string  `json:"fetched"`
	Problems []struct {
		Repository string `json:"repository"`
		Reason     string `json:"reason"`
	} `json:"problems"`
}

// sessionsDirectoriesScript lists every session directory on the volume:
// its setup record, base64, and the Session-only files in it.
const sessionsDirectoriesScript = `for d in /workspace/` + workspacesTestSessions + `/*/; do test -d "$d" || continue; id=$(basename "$d"); ` +
	`state=$(base64 -w0 "$d` + workspaceStateFile + `" 2>/dev/null); ` +
	`own=$(ls -A "$d" | grep '^` + sessionsOwnFilePrefix + `' | tr '\n' ','); ` +
	`echo "dir $id state=$state own=$own"; done; true
`

// parseSessionDirectories reads the probe's lines into each directory.
func parseSessionDirectories(out string) (map[string]sessionDirectory, error) {
	dirs := map[string]sessionDirectory{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "dir" {
			continue
		}
		var d sessionDirectory
		for _, f := range fields[2:] {
			k, v, _ := strings.Cut(f, "=")
			switch k {
			case "state":
				if v == "" {
					continue
				}
				raw, err := base64.StdEncoding.DecodeString(v)
				if err != nil {
					return nil, fmt.Errorf("the setup record of %s: %w", fields[1], err)
				}
				var s workspaceState
				if err := json.Unmarshal(raw, &s); err != nil {
					return nil, fmt.Errorf("the setup record of %s: %w", fields[1], err)
				}
				d.state = &s
			case "own":
				d.own = strings.FieldsFunc(v, func(r rune) bool { return r == ',' })
			}
		}
		dirs[fields[1]] = d
	}
	return dirs, nil
}

// directories reads the volume and the request log: one directory per
// Session, set up once with every clone, the moved repository fetched once
// per Session and nothing else.
func (sp *sessionsProof) directories(mark int) error {
	out, err := sp.sp.onVolume(syncFilterWorkspace, "sessions-dirs", sessionsDirectoriesScript)
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
		if !slices.Equal(d.state.Fetched, []string{syncMovingRepo}) {
			problems = append(problems, fmt.Sprintf("%s's setup fetched %v, want %s", r.ID, d.state.Fetched, syncMovingRepo))
		}
	}
	if len(dirs) != len(runs) {
		problems = append(problems, fmt.Sprintf("the volume holds %d session directories, want %d", len(dirs), len(runs)))
	}
	reqs, err := sp.sp.gh.requests()
	if err != nil {
		return err
	}
	sp.report.Fetches = countFetches(reqs[min(mark, len(reqs)):])
	if err := judgeFetches(sp.report.Fetches, syncMovingRepo, len(runs)); err != nil {
		problems = append(problems, err.Error())
	}
	for _, r := range runs {
		note("%s/%s (%s): set up %s, %.1fs after the first turn was sent; cloned %s; fetched %s", workspacesTestSessions, r.ID, r.Harness,
			r.DirectorySetUp.UTC().Format(time.RFC3339), r.SetupSeconds, strings.Join(r.Cloned, ", "), cmp.Or(strings.Join(r.Fetched, ", "), "nothing"))
		if d := dirs[r.ID]; d.state != nil {
			for _, problem := range d.state.Problems {
				note("  the setup's problem: %s: %s", problem.Repository, excerpt(problem.Reason, 300))
			}
		}
	}
	// The heads the first turns relayed, judged with the directories'
	// evidence in view.
	if err := judgeFirstTurns(runs, sp.wantHeads()); err != nil {
		problems = append(problems, err.Error())
	}
	note("%d distinct session directories on volume %s; the request log since the push: %s", len(dirs), sp.volume.Handle, fetchesLine(sp.report.Fetches))
	if len(problems) > 0 {
		return fmt.Errorf("the session directories: %s", strings.Join(problems, "; "))
	}
	return nil
}

func fetchesLine(fetches map[string]int) string {
	if len(fetches) == 0 {
		return "no fetch"
	}
	var out []string
	for _, repo := range slices.Sorted(maps.Keys(fetches)) {
		out = append(out, fmt.Sprintf("%s fetched %d times", repo, fetches[repo]))
	}
	return strings.Join(out, ", ")
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
			v, _, _, err := sp.probeTurn(ctx, ks, sessionsIsolationCommand)
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

// deleteOne deletes the last Session, requests a cycle and checks its
// directory went while the mirrors' heads and the other directories stayed.
func (sp *sessionsProof) deleteOne() error {
	runs := sp.report.Sessions
	victim := runs[len(runs)-1]
	repos := sp.live()
	// A cycle first brings the mirrors up to the push, so the one after the
	// delete has nothing to fetch and its mirrors compare head for head.
	prev, err := sp.sp.get(syncFilterWorkspace)
	if err != nil {
		return err
	}
	if prev, _, err = sp.sp.sync(syncFilterWorkspace, prev, true); err != nil {
		return err
	}
	before, err := sp.sp.onVolume(syncFilterWorkspace, "sessions-mirrors-before", syncMirrorsScript(repos))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sp.timeout)
	defer cancel()
	if err := sp.api.deleteSession(ctx, victim.ID); err != nil {
		return err
	}
	deletedAt := time.Now()
	out, err := sp.sp.onVolume(syncFilterWorkspace, "sessions-deleted", sessionsDirectoriesScript)
	if err != nil {
		return err
	}
	dirs, err := parseSessionDirectories(out)
	if err != nil {
		return err
	}
	var after *syncDetail
	if sp.poll > 0 {
		// No cycle is requested: the manager's Session poll finds the
		// Session gone and removes its directory.
		if after, dirs, err = sp.awaitDeleted(victim.ID, dirs, deletedAt); err != nil {
			return err
		}
	} else {
		if after, _, err = sp.sp.sync(syncFilterWorkspace, prev, false); err != nil {
			return err
		}
		if out, err = sp.sp.onVolume(syncFilterWorkspace, "sessions-after-delete", sessionsDirectoriesScript); err != nil {
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
	mirrorsAfter, err := sp.sp.onVolume(syncFilterWorkspace, "sessions-mirrors-after", syncMirrorsScript(repos))
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
	note("deleted %s (%s): its directory gone %s after DeleteSession, with the cycle %s (trigger %s); the %d other directories and the mirrors' heads untouched, the workspace on volume %s",
		victim.ID, victim.Harness, time.Since(deletedAt).Round(time.Second), after.Workspace.LastSync.Result, after.Workspace.LastSync.Trigger, len(dirs), sp.volume.Handle)
	return nil
}

// awaitDeleted waits, with no cycle requested, for the deleted Session's
// directory to leave the volume and for the workspace's last cycle to be the
// manager's own for it; dirs is the volume's listing right after the delete.
func (sp *sessionsProof) awaitDeleted(id string, dirs map[string]sessionDirectory, deletedAt time.Time) (*syncDetail, map[string]sessionDirectory, error) {
	var err error
	for _, ok := dirs[id]; ok; _, ok = dirs[id] {
		if time.Since(deletedAt) > sessionsDeleteWait {
			return nil, nil, fmt.Errorf("the directory of the deleted Session %s is still on the volume %s after DeleteSession with no cycle requested; the workspace-manager lists kagent's Sessions every %s", id, sessionsDeleteWait, sp.poll)
		}
		time.Sleep(5 * time.Second)
		out, err := sp.sp.onVolume(syncFilterWorkspace, "sessions-after-delete", sessionsDirectoriesScript)
		if err != nil {
			return nil, nil, err
		}
		if dirs, err = parseSessionDirectories(out); err != nil {
			return nil, nil, err
		}
	}
	gone := time.Since(deletedAt)
	var after *syncDetail
	for {
		if after, err = sp.sp.get(syncFilterWorkspace); err != nil {
			return nil, nil, err
		}
		if last := after.Workspace.LastSync; last != nil && last.Trigger == triggerSessionDeleted {
			break
		}
		if time.Since(deletedAt) > sessionsDeleteWait {
			return nil, nil, fmt.Errorf("the deleted Session %s's directory went, but the workspace's last cycle is %+v, not the trigger %s", id, after.Workspace.LastSync, triggerSessionDeleted)
		}
		time.Sleep(5 * time.Second)
	}
	note("the directory %s gone %s after DeleteSession with no cycle requested", id, gone.Round(time.Second))
	return after, dirs, nil
}

// live are the repositories the workspace mirrors.
func (sp *sessionsProof) live() []string {
	repos := []string{syncMovingRepo}
	if sp.sp.private {
		repos = append(repos, syncPrivateRepo)
	}
	slices.Sort(repos)
	return repos
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
		env, err := sp.admin.session.callToolEnvelope(agentManagerToolPrefix+"start_session", map[string]any{
			nameKey: h.agent, namespaceKey: kagentNamespace, startSessionRequestIDArg: uuid.NewString(),
		})
		if err != nil {
			return err
		}
		if env.IsError {
			return fmt.Errorf("start_session without a workspace on %s refused: %.400s", h.agent, env.Content[0].Text)
		}
		var started accessStarted
		if err := json.Unmarshal([]byte(env.Content[0].Text), &started); err != nil || started.Session.ID == "" {
			return fmt.Errorf("start_session answered no session: %.400s", env.Content[0].Text)
		}
		sp.set.add(started.Session.ID)
		ctx, cancel := context.WithTimeout(context.Background(), sp.timeout)
		ks, err := sp.api.getSession(ctx, started.Session.ID)
		if err != nil {
			cancel()
			return err
		}
		if ks.GetVolumeSource() != nil {
			cancel()
			return fmt.Errorf("the Session %s started without a workspace carries volume %q", ks.GetId(), ks.GetVolumeSource().GetVolume().GetVolumeHandle())
		}
		v, _, _, err := sp.probeTurn(ctx, ks, sessionsEmptyCommand)
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
	runs := liveSessions(sp.report.Sessions)
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
	prev, err := sp.sp.get(syncFilterWorkspace)
	if err != nil {
		return err
	}
	after, _, err := sp.sp.sync(syncFilterWorkspace, prev, false)
	if err != nil {
		return err
	}
	cond, err := workspaceCondition(ctx, syncFilterWorkspace, conditionSessionsCleanedUp)
	if err != nil {
		return err
	}
	sp.report.Condition = cond
	if cond.Status != conditionTrue {
		return fmt.Errorf("the Workspace %s reads %s after the cleanup cycle (reason %s): no directory is cleaned up", syncFilterWorkspace, cond, cond.Reason)
	}
	note("cycle %s (trigger %s); the Workspace reads %s", after.Workspace.LastSync.Result, after.Workspace.LastSync.Trigger, cond)
	out, err := sp.sp.onVolume(syncFilterWorkspace, "sessions-cleaned", sessionsDirectoriesScript)
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
		v, first, _, err := sp.probeTurn(ctx, ks, sessionsResumeCommand)
		if err != nil {
			return err
		}
		if err := judgeFresh(v, r.Token, sp.wantHeads()); err != nil {
			return fmt.Errorf("the resumed Session %s on %s: %w", r.ID, h.harness, err)
		}
		told := first != nil && slices.ContainsFunc(first.notices, func(n string) bool { return strings.Contains(n, sessionsFreshNotice) })
		switch {
		case told:
			note("%s (%s) resumed in a fresh directory (%s; its own file of before gone), told so in its first turn: %q", r.ID, h.harness, headsLine(v.heads), excerpt(strings.Join(first.notices, " "), 200))
		case h.harness == claudeHarness:
			note("%s (%s) resumed in a fresh directory (%s; its own file of before gone); KNOWN GAP: the claude Harness does not tell it so (https://github.com/giantswarm/kagent-upstream/issues/399)", r.ID, h.harness, headsLine(v.heads))
		default:
			return fmt.Errorf("the resumed Session %s on %s works in a fresh directory but its first turn does not say so (status messages %q)", r.ID, h.harness, firstNotices(first))
		}
	}
	return nil
}

// liveSessions are the runs the part has not deleted.
func liveSessions(runs []*sessionRun) []*sessionRun {
	return slices.DeleteFunc(slices.Clone(runs), func(r *sessionRun) bool { return r.Deleted })
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
// Session, key=value: what the measurements read.
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
	if sp.admin != nil {
		sp.admin.disconnect(config.WorkspaceProviderFake)
	}
	for _, h := range sessionsHarnesses {
		if agentExists(h.agent) {
			errs = append(errs, removeAgent(h.agent))
		}
	}
	if sp.sp.member != nil {
		if gone, err := sp.sp.gone(syncFilterWorkspace); err == nil && !gone {
			if _, err := sp.sp.volume(syncFilterWorkspace); err == nil {
				out, err := sp.sp.onVolume(syncFilterWorkspace, "sessions-clear", accessClearSessionsScript)
				errs = append(errs, err)
				if err == nil {
					note("%s", indent(out, "  "))
				}
			}
		}
	}
	errs = append(errs, sp.sp.restore())
	if sp.api != nil {
		sp.api.close()
	}
	if err := errors.Join(errs...); err != nil {
		warn("removing what the part left: %v", err)
	}
}

// workspaceManagerFlags are the workspace-manager container's arguments.
func workspaceManagerFlags(ctx context.Context) ([]string, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, kubeReadTimeout)
	defer cancel()
	d, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, workspaceManagerMCPServer, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the Deployment %s/%s: %w", platformNamespace, workspaceManagerMCPServer, err)
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == workspaceManagerMCPServer || len(d.Spec.Template.Spec.Containers) == 1 {
			return append(slices.Clone(c.Command), c.Args...), nil
		}
	}
	return nil, fmt.Errorf("the Deployment %s/%s has no container %s", platformNamespace, workspaceManagerMCPServer, workspaceManagerMCPServer)
}

// flagValue is the value of a --name= argument, "" without one.
func flagValue(args []string, prefix string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			return v
		}
	}
	return ""
}

// judgeKagentAddress is the verdict on the manager's kagent: without its
// address no session directory is ever cleaned up.
func judgeKagentAddress(args []string) error {
	if flagValue(args, sessionsKagentArg) == "" {
		return fmt.Errorf("the workspace-manager runs without kagent (no %s argument): no session directory is ever cleaned up, every Workspace reads %s=False KagentNotConfigured; agent-platform derives workspace-manager.kagent.address from 4.128.0 (the component's range floor at its kagent floor), or the values set it", strings.TrimSuffix(sessionsKagentArg, "="), conditionSessionsCleanedUp)
	}
	return nil
}

// SessionsCleanupWindowRange is the range the sessions part requires of the
// workspace-manager's cleanup window, as a person types durations: "5m to
// 15m".
func SessionsCleanupWindowRange() string {
	return cleanupWindowRange(SessionsMinCleanupWindow, SessionsMaxCleanupWindow)
}

// SessionsCleanupWindowExample is the window to suggest, as a person types
// it: "10m".
func SessionsCleanupWindowExample() string {
	return formatWindow(SessionsExampleCleanupWindow)
}

func cleanupWindowRange(least, limit time.Duration) string {
	return formatWindow(least) + " to " + formatWindow(limit)
}

// formatWindow is a whole number of minutes as a person types it: "10m", not
// "10m0s".
func formatWindow(d time.Duration) string {
	return strings.TrimSuffix(d.String(), "m0s") + "m"
}

// judgeCleanupWindow reads the manager's cleanup window and refuses one
// outside [least, limit].
func judgeCleanupWindow(args []string, least, limit time.Duration) (time.Duration, error) {
	raw := flagValue(args, sessionsCleanupArg)
	if raw == "" {
		return 0, fmt.Errorf("the workspace-manager has no %s argument: a chart before the cleanup; the sessions part needs workspace-manager 0.4.0 or newer", strings.TrimSuffix(sessionsCleanupArg, "="))
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("the workspace-manager's %s%s: %w", sessionsCleanupArg, raw, err)
	}
	if d > limit || d < least {
		return 0, fmt.Errorf("the workspace-manager cleans a session directory up %s after its last turn; the part needs %s (shorter, a live Session's directory goes during the part; longer, the part waits too long): `agentlab configure --workspaces-cleanup-after %s`, then `agentlab platform`", d, cleanupWindowRange(least, limit), formatWindow(SessionsExampleCleanupWindow))
	}
	return d, nil
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
