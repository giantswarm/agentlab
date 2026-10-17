package lab

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// `agentlab workspaces-test --part gitlab`: the workspace-manager's provider
// instance gitlab against a real GitLab (platform.workspaces.gitlab), with
// muster pinning the manager's registration to the GitLab OAuth
// application (platform.workspaces.grantProvider: gitlab), as the lab's
// admin, whose GitLab account is the configured person, and the second user,
// who has not granted it. Without the instance or the pin the part skips
// every stage, naming why, and passes. The steps:
//
//   - the second user, who has not granted GitLab's application in muster:
//     the manager's tools answer muster's sign-in link;
//   - the admin grants it: muster's link walked through GitLab's sign-in and
//     the application's consent to muster's callback, list_providers showing
//     gitlab connected;
//   - the editor's picker: list_repositories lists the group's projects;
//   - a workspace over the project: its first cycle mirrors it, the mirror on
//     the volume at the manifest's head;
//   - authorize_workspace, checking the project with the admin's GitLab token
//     muster attaches, grants the admin a grant naming the GitLab hosts;
//     start_session starts a Session with it on the volume, and its turn
//     finds its directory with the clone of the project; its push to GitLab
//     is reported as it stands (gitlabBlockedPush);
//   - a Session without a workspace holds no GitLab credential: no
//     credential in its environment, and git refused at GitLab;
//   - the admin drops the grant in muster (core_auth_logout): the manager's
//     tools answer muster's sign-in link again.

// WorkspacesPartGitLab is the GitLab part of workspaces-test.
const WorkspacesPartGitLab = "gitlab"

const (
	gitlabWorkspace = "agentlab-gitlab"
	// gitlabProofBranch is the branch the person's API write and the
	// Session's push name, with a suffix of the run.
	gitlabProofBranch = "agentlab-proof-"
	// The probe's lines the GitLab turns relay.
	gitlabProbeCredentialEnv = "credential-env" // #nosec G101 -- a probe line's name, not a credential
	gitlabProbePush          = "push"
	gitlabProbeLsRemote      = "ls-remote"
	gitlabProbeSetupClone    = "setup-clone"
)

// gitlabBlockedPush words why a Session's push does not reach GitLab as the
// person yet: the grant names the hosts, and the person's token reaches the
// Session's egress only from muster, for a Session bound to its person.
const gitlabBlockedPush = "a Session reaches GitLab as the person only through Substrate's egress gateway, " +
	"which fetches the person's token from muster for a Session bound to them; the lab's GitLab instance is not wired to it, " +
	"so the push from the Session is reported, not judged"

// gitlabProof is the part's state.
type gitlabProof struct {
	sp            *syncProof
	cfg           *config.Config
	gl            config.WorkspaceGitLab
	timeout       time.Duration
	repo          string // <group>/<project>
	admin, second *workspacePerson
	adminUser     *config.User
	secondUser    *config.User
	adminAPI      *kagentAPI
	sessions      *sessionSet
	login         *providerLogin
	head          string // the project's head the workspace's first cycle mirrored
	branch        string
	volume        workspaceVolume
	gitlabHost    string
}

// workspacesGitLabPart runs the part in stages, as the admin and the second
// user email (default dev@lab.local).
func workspacesGitLabPart(cfg *config.Config, email string, timeout time.Duration) error {
	p := newProofStages()
	gp := &gitlabProof{sp: newSyncProof(cfg, timeout, gitlabWorkspace), cfg: cfg, gl: cfg.Platform.Workspaces.GitLab, timeout: timeout}
	err := gp.run(cmp.Or(email, accessDefaultSecond), p)
	gp.cleanup()
	if err == nil && gp.admin != nil {
		fmt.Printf("\nWorkspaces GitLab part passed: muster answered a person who had not granted GitLab's application with its sign-in link, the admin granted it through GitLab's form and consent, list_repositories listed %s's projects, the workspace mirrored %s, a Session started with the grant found its clone, a Session without the workspace held no GitLab credential, and dropping the grant in muster closed the workspace-manager to the admin again.\n", gp.gl.Group, gp.repo)
	}
	return p.close(err)
}

// gitlabStages are the part's stages, skipped together without the instance.
var gitlabStages = []string{"gitlab: not connected", "gitlab: connect", "gitlab: picker", "gitlab: workspace", "gitlab: start", "gitlab: push from the Session", "gitlab: no workspace", "gitlab: disconnect", "gitlab: restore"}

// gitlabSkip says why the lab runs no GitLab part, "" when it does.
func gitlabSkip(cfg *config.Config) string {
	gl := cfg.Platform.Workspaces.GitLab
	var missing []string
	for _, f := range []struct{ key, value string }{
		{"url", gl.URL}, {"clientId", gl.ClientID}, {"group", gl.Group}, {"project", gl.Project}, {"person", gl.Person},
	} {
		if f.value == "" {
			missing = append(missing, "platform.workspaces.gitlab."+f.key)
		}
	}
	switch {
	case !cfg.WorkspaceGitLabProvider():
		return fmt.Sprintf("platform.workspaces.provider %q configures no instance %s (`agentlab configure --workspaces-provider fake,%s` and platform.workspaces.gitlab, docs/workspaces.md)", cfg.Platform.Workspaces.Provider, config.WorkspaceProviderGitLab, config.WorkspaceProviderGitLab)
	case len(missing) > 0:
		return "the instance gitlab lacks " + strings.Join(missing, ", ")
	case cfg.WorkspaceGrantProvider() != config.WorkspaceProviderGitLab:
		return fmt.Sprintf("muster pins the workspace-manager to the App of %s: the part grants GitLab's application (platform.workspaces.grantProvider: %s)", cfg.WorkspaceGrantProvider(), config.WorkspaceProviderGitLab)
	}
	return ""
}

func (gp *gitlabProof) run(email string, p *proofStages) error {
	cfg := gp.cfg
	p.begin("gitlab: preconditions", "The GitLab part's preconditions: the instance %s, the workspace-manager through muster pinned to its application, the Session admission, the admin and %s signed in", config.WorkspaceProviderGitLab, email)
	if skip := gitlabSkip(cfg); skip != "" {
		p.skip("%s", skip)
		p.leaveOut(skip, gitlabStages...)
		return nil
	}
	if err := gp.preconditions(email); err != nil {
		return err
	}

	p.begin("gitlab: not connected", "%s, who has not granted GitLab's application in muster: the workspace-manager's tools answer muster's sign-in link", email)
	if err := gp.notConnected(); err != nil {
		return err
	}

	p.begin("gitlab: connect", "%s grants GitLab's application: GitLab's sign-in as %s and its consent, to muster's callback", gp.adminUser.Email, gp.gl.Person)
	if err := gp.connect(); err != nil {
		return err
	}

	p.begin("gitlab: picker", "list_repositories for %s on %s as %s: the workspace editor's picker", gp.gl.Group, config.WorkspaceProviderGitLab, gp.adminUser.Email)
	if err := gp.picker(); err != nil {
		return err
	}

	p.begin("gitlab: workspace", "create_workspace %s over %s as %s, and its first cycle: the mirror and the manifest at GitLab's head", gitlabWorkspace, gp.repo, gp.adminUser.Email)
	if err := gp.workspace(); err != nil {
		return err
	}

	p.begin("gitlab: start", "authorize_workspace and start_session as %s: the grant names the GitLab hosts, the Session works in its directory with the clone of %s", gp.adminUser.Email, gp.repo)
	probe, err := gp.start()
	if err != nil {
		return err
	}

	p.begin("gitlab: push from the Session", "The Session pushes to %s and opens a merge request as the person, with no token in the sandbox", gp.repo)
	if pushed := gp.judgePush(probe); !pushed {
		p.skip("%s; the Session's push answered %q", gitlabBlockedPush, probe[gitlabProbePush])
	}

	p.begin("gitlab: no workspace", "A Session started without a workspace as %s holds no GitLab credential", gp.adminUser.Email)
	if err := gp.noWorkspace(); err != nil {
		return err
	}

	p.begin("gitlab: disconnect", "%s drops the grant in muster (core_auth_logout): the workspace-manager's tools answer muster's sign-in link again", gp.adminUser.Email)
	if err := gp.disconnect(); err != nil {
		return err
	}

	p.begin("gitlab: restore", "The Sessions, the Agent, the workspace, the proof branch and merge request removed")
	return gp.cleanupNow()
}

// preconditions signs both people in to muster, reads the person's GitLab
// password from the instance's Secret, checks muster pins the manager to the
// instance and the claude Agent is Ready.
func (gp *gitlabProof) preconditions(email string) error {
	cfg, sp := gp.cfg, gp.sp
	if reason := workspaceManagerSkip(cfg); reason != "" {
		return fmt.Errorf("no workspace-manager: %s", reason)
	}
	if err := waitMCPServerReachable(workspaceManagerMCPServer); err != nil {
		return fmt.Errorf("the workspace-manager is not reachable through muster: %w", err)
	}
	if _, err := workspaceManagerPinnedProvider(cfg); err != nil {
		return err
	}
	gp.adminUser, gp.secondUser = cfg.AdminUser(), cfg.FindUser(email)
	switch {
	case gp.adminUser == nil:
		return fmt.Errorf("the GitLab part needs an admin user in %s (`agentlab configure --defaults` writes one)", config.File)
	case gp.secondUser == nil:
		return fmt.Errorf("no user %s in %s", email, config.File)
	case strings.EqualFold(gp.secondUser.Email, gp.adminUser.Email):
		return fmt.Errorf("the second user must be another person than the admin %s", gp.adminUser.Email)
	}
	var err error
	if gp.admin, err = openWorkspacePerson(cfg, gp.adminUser); err != nil {
		return err
	}
	if gp.second, err = openWorkspacePerson(cfg, gp.secondUser); err != nil {
		return err
	}
	sp.member, sp.memberEmail = gp.admin.session, gp.adminUser.Email

	u, err := url.Parse(strings.TrimSuffix(gp.gl.URL, "/"))
	if err != nil {
		return err
	}
	gp.gitlabHost, gp.repo = u.Host, gp.gl.Group+"/"+gp.gl.Project
	gp.branch = gitlabProofBranch + time.Now().UTC().Format("20060102150405")
	password, err := gp.personPassword()
	if err != nil {
		return err
	}
	gp.login = &providerLogin{login: gp.gl.Person, password: password}

	rig := workspaceAdmissionValuesFor(cfg, workspacesChartCarries(cfg))
	if rig == nil {
		return fmt.Errorf("no Session admission on this lab (the chart takes no workspaces values, or platform.agents is off): the GitLab part starts Sessions on a workspace volume")
	}
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, gp.adminUser.Email, gp.adminUser.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	if gp.adminAPI, err = dialKagentAPI(cfg, token); err != nil {
		return err
	}
	gp.sessions = &sessionSet{api: gp.adminAPI}
	if err := waitHarnessReady(context.Background(), rig.ClaudeHarness, gp.timeout); err != nil {
		return err
	}
	if agentExists(workspacesHarnessAgent) {
		if err := removeAgent(workspacesHarnessAgent); err != nil {
			return err
		}
	}
	if _, err := applyManifests(context.Background(), []byte(workspacesHarnessAgentManifest(rig.ClaudeHarness))); err != nil {
		return fmt.Errorf("applying the claude Agent: %w", err)
	}
	boot, err := waitAgentReadyOn(workspacesHarnessAgent, rig.ClaudeHarness, gp.timeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return fmt.Errorf("the claude Agent %s is not Ready on Harness %s after %s: %s", workspacesHarnessAgent, rig.ClaudeHarness, boot.elapsed.Round(time.Second), boot.reason)
	}
	note("GitLab %s, project %s; signed in to muster as %s (GitLab person %s) and %s; the claude Agent %s Ready", gp.gl.URL, gp.repo, gp.adminUser.Email, gp.gl.Person, email, workspacesHarnessAgent)
	return nil
}

// personPassword is the GitLab person's password, the one value of the
// instance's Secret agentlab reads: it is typed into GitLab's sign-in form,
// as the person would, and never printed.
func (gp *gitlabProof) personPassword() (string, error) {
	k, err := labKube()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	secret, err := k.clientset.CoreV1().Secrets(platformNamespace).Get(ctx, config.WorkspaceGitLabSecretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading the Secret %s/%s: %w", platformNamespace, config.WorkspaceGitLabSecretName, err)
	}
	password := strings.TrimSpace(string(secret.Data[config.WorkspaceGitLabPasswordKey]))
	if password == "" {
		return "", fmt.Errorf("the Secret %s/%s carries no %s: platform.workspaces.gitlab.passwordSource names where the GitLab person's password is", platformNamespace, config.WorkspaceGitLabSecretName, config.WorkspaceGitLabPasswordKey)
	}
	return password, nil
}

// gitlabListed is list_repositories' answer, the fields the part reads.
type gitlabListed struct {
	Total        int `json:"total"`
	OwnerTotal   int `json:"ownerTotal"`
	Repositories []struct {
		Name     string    `json:"name"`
		Language string    `json:"language"`
		LastPush time.Time `json:"lastPush"`
		SizeKiB  int64     `json:"sizeKiB"`
	} `json:"repositories"`
}

// picker lists the group's projects the way the portal's workspace editor
// does: list_repositories, no provider-specific argument.
func (gp *gitlabProof) picker() error {
	var listed gitlabListed
	if err := gp.admin.session.callServerStructured(workspaceManagerToolPrefix+"list_repositories", map[string]any{
		wmOrganizationArg: config.WorkspaceOrganization, wmProviderArg: config.WorkspaceProviderGitLab, wmOwnerArg: gp.gl.Group,
	}, &listed); err != nil {
		return err
	}
	var names []string
	for _, r := range listed.Repositories {
		names = append(names, fmt.Sprintf("%s (%s, pushed %s, %d KiB)", r.Name, cmp.Or(r.Language, "no language"), r.LastPush.UTC().Format(time.RFC3339), r.SizeKiB))
	}
	if !slices.ContainsFunc(listed.Repositories, func(r struct {
		Name     string    `json:"name"`
		Language string    `json:"language"`
		LastPush time.Time `json:"lastPush"`
		SizeKiB  int64     `json:"sizeKiB"`
	}) bool {
		return r.Name == gp.gl.Project
	}) {
		return fmt.Errorf("list_repositories for %s lists %v, not %s", gp.gl.Group, names, gp.gl.Project)
	}
	note("list_repositories: %d of %d projects of %s — %s", listed.Total, listed.OwnerTotal, gp.gl.Group, strings.Join(names, "; "))
	return nil
}

// notConnected proves the second user, who has not granted GitLab's
// application, gets muster's sign-in link from the workspace-manager.
func (gp *gitlabProof) notConnected() error {
	gp.second.disconnect(config.WorkspaceProviderGitLab)
	link, err := gp.second.grantRequired("list_providers", nil)
	if err != nil {
		return err
	}
	note("list_providers as %s: muster's sign-in link %s", gp.secondUser.Email, excerpt(link, 120))
	return nil
}

// connect has the admin grant GitLab's application in muster through
// GitLab's own sign-in and consent, then checks the instance's kind and
// removes what an earlier run left, now that the manager's tools answer.
func (gp *gitlabProof) connect() error {
	hops, err := connectWorkspaceProviderAs(gp.cfg, gp.admin, config.WorkspaceProviderGitLab, gp.login)
	if err != nil {
		return err
	}
	note("%s", hops)
	status, err := gp.admin.providerStatus(config.WorkspaceProviderGitLab)
	if err != nil {
		return err
	}
	if status.Kind != providerKindGitLab {
		return fmt.Errorf("list_providers lists %s with kind %q, want %s", config.WorkspaceProviderGitLab, status.Kind, providerKindGitLab)
	}
	note("list_providers as %s: %s", gp.adminUser.Email, status)
	return gp.sp.removeLeftovers()
}

func (gp *gitlabProof) workspace() error {
	sp := gp.sp
	var created syncWorkspace
	if err := sp.member.callServerStructured(workspaceManagerToolPrefix+"create_workspace", map[string]any{
		nameKey: gitlabWorkspace, wmOrganizationArg: config.WorkspaceOrganization,
		wmSourcesArg: []map[string]any{{wmProviderArg: config.WorkspaceProviderGitLab, wmOwnerArg: gp.gl.Group, repositoriesKey: []string{gp.gl.Project}}},
	}, &created); err != nil {
		return err
	}
	d, err := sp.waitCycle(gitlabWorkspace, nil)
	if err != nil {
		return err
	}
	if r := d.Workspace.LastSync; r.Result != syncResultSucceed {
		return fmt.Errorf("the first cycle of %s ended %s: %s", gitlabWorkspace, r.Result, r.Message)
	}
	if live := d.live(); !slices.Equal(live, []string{gp.repo}) {
		return fmt.Errorf("the first cycle of %s mirrored %v, want [%s]", gitlabWorkspace, live, gp.repo)
	}
	sp.printManifest(d)
	m := d.mirror(gp.repo)
	if m == nil || m.Head == "" {
		return fmt.Errorf("the manifest of %s holds no head of %s", gitlabWorkspace, gp.repo)
	}
	gp.head = m.Head
	out, err := sp.onVolume(gitlabWorkspace, "gitlab-mirror", syncMirrorsScript([]string{gp.repo}))
	if err != nil {
		return err
	}
	probe := parseMirrorProbe(out)[gp.repo]
	if bare, _ := strconv.ParseBool(probe["bare"]); !bare || probe["head"] != gp.head {
		return fmt.Errorf("the mirror %s/%s.git on the volume reads %v, want bare at the manifest's head %s", workspacesTestMirrors, gp.repo, probe, shortSHA(gp.head))
	}
	note("the mirror %s/%s.git on the volume: %s", workspacesTestMirrors, gp.repo, strings.TrimSpace(strings.TrimPrefix(out, gp.repo)))
	v, err := sp.volume(gitlabWorkspace)
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
	if err != nil || pv.Spec.CSI == nil {
		return fmt.Errorf("reading the CSI volume %s of workspace %s: %v", v.pv, gitlabWorkspace, err)
	}
	gp.volume = workspaceVolume{Driver: pv.Spec.CSI.Driver, Handle: pv.Spec.CSI.VolumeHandle}
	note("created %s of Organization %s; first cycle %s; volume %s (%s)", gitlabWorkspace, created.Organization, d.Workspace.LastSync.Result, gp.volume.Handle, gp.volume.Driver)
	return nil
}

// gitlabGrantProvider is one host of a grant's providers claim.
type gitlabGrantProvider struct {
	Hostname string `json:"hostname"`
	Audience string `json:"audience"`
	Scheme   string `json:"scheme"`
	Username string `json:"username"`
	Broker   string `json:"broker"`
}

// gitlabProbeCommand is the GitLab turns' Bash command: the directory, the
// clones in it, the setup clone's origin/HEAD, the names of every
// credential-like variable of its environment (names only), and git against
// GitLab: a push of an empty commit from the clone when there is one, else
// a listing of the project's refs.
func (gp *gitlabProof) gitlabProbeCommand() string {
	remote := "https://" + gp.gitlabHost + "/" + gp.repo + ".git"
	return `echo "` + probeMarker + ` pwd=$(pwd)"; ` +
		`echo "` + probeMarker + ` mirrors=$(cd /mirrors 2>/dev/null && find . -mindepth 2 -maxdepth 2 -name '*.git' | sed 's|^\./||' | sort | tr '\n' ' ')"; ` +
		`echo "` + probeMarker + ` clones=$(find . -mindepth 3 -maxdepth 3 -name .git 2>/dev/null | sed 's|^\./||;s|/\.git$||' | sort | tr '\n' ' ')"; ` +
		`echo "` + probeMarker + ` ` + gitlabProbeSetupClone + `=$(git -C ./` + gp.repo + ` rev-parse origin/HEAD 2>&1 | tr '\n' ' ')"; ` +
		`echo "` + probeMarker + ` ` + gitlabProbeCredentialEnv + `=$(env | cut -d= -f1 | grep -iE 'token|gitlab|glab|askpass|credential|password' | sort | tr '\n' ' ')"; ` +
		`if [ -d ./` + gp.repo + ` ]; then ` +
		`out=$(cd ./` + gp.repo + ` && git -c user.name=agentlab -c user.email=agentlab@lab.local commit -q --allow-empty -m agentlab-session-push && GIT_TERMINAL_PROMPT=0 timeout 60 git push ` + remote + ` HEAD:refs/heads/` + gp.branch + `-session 2>&1); rc=$?; ` +
		`echo "` + probeMarker + ` ` + gitlabProbePush + `=rc$rc $(echo "$out" | tail -n 3 | tr '\n' ' ')"; ` +
		`else ` +
		`out=$(GIT_TERMINAL_PROMPT=0 timeout 60 git ls-remote ` + remote + ` 2>&1); rc=$?; ` +
		`echo "` + probeMarker + ` ` + gitlabProbeLsRemote + `=rc$rc $(echo "$out" | tail -n 3 | tr '\n' ' ')"; ` +
		`fi`
}

// gitlabTurn runs the probe on the Session and returns its marker lines.
func (gp *gitlabProof) gitlabTurn(id string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gp.timeout)
	defer cancel()
	session, err := gp.adminAPI.getSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := gp.firstTurnDone(ctx, session); err != nil {
		return nil, err
	}
	prompt := "Run this exact command once with your Bash tool, then reply with every output line that starts with " + probeMarker + ", verbatim and nothing else:\n\n" + gp.gitlabProbeCommand()
	// The model now and then declines the command as text it was not asked
	// for by its person; the follow-up turn says who asks and why.
	followUp := "I am the person running agentlab's workspace test on this throwaway lab cluster: the command only prints where you work, lists the clones and pushes an empty commit to a proof branch. Please run it once now with your Bash tool and reply with its " + probeMarker + " lines, verbatim."
	var reply string
	for _, msg := range []string{prompt, followUp} {
		t, err := gp.adminAPI.turn(ctx, session, userMessage(msg))
		if err != nil {
			return nil, fmt.Errorf("the probe turn on Session %s: %w", id, err)
		}
		if reply, err = t.completedText(); err != nil {
			return nil, fmt.Errorf("the probe turn on Session %s: %w", id, err)
		}
		values := map[string]string{}
		for _, m := range probeLine.FindAllStringSubmatch(reply, -1) {
			values[m[1]] = strings.TrimSpace(strings.Trim(strings.TrimSpace(m[2]), "`"))
		}
		if values["pwd"] != "" {
			return values, nil
		}
	}
	return nil, fmt.Errorf("the probe turn on Session %s relayed no %s pwd line (did the agent run its Bash tool?): %s", id, probeMarker, excerpt(reply, 300))
}

// firstTurnDone waits for the turn start_session's first message began: the
// Session takes the next message only after it, kagent answering a message
// sent during it with a resource conflict.
func (gp *gitlabProof) firstTurnDone(ctx context.Context, session *apiv1alpha1.Session) error {
	for {
		tasks, err := gp.adminAPI.listTasks(ctx, session)
		if err != nil {
			return err
		}
		if len(tasks) > 0 && !slices.ContainsFunc(tasks, func(t *a2a.Task) bool { return !t.Status.State.Terminal() }) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the first turn of Session %s did not end: %w", session.GetId(), ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// startedSession starts a Session through agent-manager as the admin, with
// the grant when one is given, and returns its id.
func (gp *gitlabProof) startedSession(grant string) (string, error) {
	env, err := startSession(gp.admin, grant)
	if err != nil {
		return "", err
	}
	if env.IsError {
		return "", fmt.Errorf("start_session as %s refused: %.400s", gp.adminUser.Email, env.Content[0].Text)
	}
	var started accessStarted
	if err := json.Unmarshal([]byte(env.Content[0].Text), &started); err != nil || started.Session.ID == "" {
		return "", fmt.Errorf("start_session answered no session: %.400s", env.Content[0].Text)
	}
	gp.sessions.add(started.Session.ID)
	return started.Session.ID, nil
}

func (gp *gitlabProof) start() (map[string]string, error) {
	a, err := gp.admin.authorizeWorkspace(gitlabWorkspace)
	if err != nil {
		return nil, err
	}
	if a.Outcome != authorizeGranted || a.Grant == "" {
		return nil, fmt.Errorf("authorize_workspace as %s answered %s (%q, unreadable %v, connect %v), want granted", gp.adminUser.Email, a.Outcome, a.Reason, a.Unreadable, a.Connect)
	}
	generic, claims, err := grantPayload(a.Grant)
	if err != nil {
		return nil, err
	}
	// The gitlab kind reads a project's last change from GitLab's
	// last_activity_at, which activity on the project moves after the sync
	// without a push to a head: the project may be named changed.
	var changed []string
	if slices.Equal(claims.Changed, []string{gp.repo}) {
		changed = claims.Changed
		note("authorize_workspace names %s changed since the sync: GitLab's last activity moved, its head need not have", gp.repo)
	}
	if err := judgeGrant(claims, gp.adminUser.Email, gitlabWorkspace, gp.volume, changed); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(generic["providers"])
	var hosts []gitlabGrantProvider
	_ = json.Unmarshal(raw, &hosts)
	var worded []string
	for _, h := range hosts {
		if h.Audience == config.WorkspaceProviderGitLab {
			worded = append(worded, fmt.Sprintf("%s (%s, user %q, broker %s)", h.Hostname, h.Scheme, h.Username, h.Broker))
		}
	}
	if !slices.ContainsFunc(hosts, func(h gitlabGrantProvider) bool {
		return h.Audience == config.WorkspaceProviderGitLab && h.Hostname == gp.gitlabHost
	}) {
		return nil, fmt.Errorf("the grant names no GitLab host %s for the audience %s: %s", gp.gitlabHost, config.WorkspaceProviderGitLab, string(raw))
	}
	note("authorize_workspace: granted — sub %s, workspace %s, volume %s; GitLab hosts %s; no token in the grant", claims.Subject, claims.Workspace[nameKey], claims.Volume.Handle, strings.Join(worded, ", "))

	id, err := gp.startedSession(a.Grant)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), gp.timeout)
	defer cancel()
	session, err := gp.adminAPI.getSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if vs := session.GetVolumeSource().GetVolume(); vs.GetVolumeHandle() != gp.volume.Handle {
		return nil, fmt.Errorf("the Session %s started with the grant is on volume %q, want the workspace's %s", id, vs.GetVolumeHandle(), gp.volume.Handle)
	}
	probe, err := gp.gitlabTurn(id)
	if err != nil {
		return nil, err
	}
	setup := strings.TrimSpace(probe[gitlabProbeSetupClone])
	if probe["pwd"] != workspacesTestMountPath || !slices.Contains(strings.Fields(probe["clones"]), gp.repo) || setup != gp.head {
		return nil, fmt.Errorf("the Session %s should work in %s with the clone %s at %s: %v", id, workspacesTestMountPath, gp.repo, shortSHA(gp.head), probe)
	}
	note("start_session: Session %s on volume %s; its turn works in %s, mirrors [%s], clones [%s], the setup clone %s at origin/HEAD %s (the mirrored head)",
		id, gp.volume.Handle, probe["pwd"], probe["mirrors"], probe["clones"], gp.repo, shortSHA(setup))
	if env := gitlabCredentialEnv(probe[gitlabProbeCredentialEnv]); len(env) > 0 {
		return nil, fmt.Errorf("the Session %s carries credential-like variables %v beyond the platform's own %v: the GitLab token stays out of the sandbox", id, env, gitlabPlatformEnv)
	}
	note("credential-like variables in its environment: [%s], none beyond the platform's own", probe[gitlabProbeCredentialEnv])
	return probe, nil
}

// judgePush reports the Session's push: true when it reached GitLab as the
// person, which the part then checks at GitLab.
func (gp *gitlabProof) judgePush(probe map[string]string) bool {
	push := probe[gitlabProbePush]
	note("the Session's git push of an empty commit to %s:%s-session: %s", gp.repo, gp.branch, push)
	return strings.HasPrefix(push, "rc0 ")
}

func (gp *gitlabProof) noWorkspace() error {
	id, err := gp.startedSession("")
	if err != nil {
		return err
	}
	probe, err := gp.gitlabTurn(id)
	if err != nil {
		return err
	}
	if probe["pwd"] != kagentDefaultWorkingDirectory || probe["mirrors"] != "" {
		return fmt.Errorf("the Session %s without a workspace should work in %s with no /mirrors: %v", id, kagentDefaultWorkingDirectory, probe)
	}
	if env := gitlabCredentialEnv(probe[gitlabProbeCredentialEnv]); len(env) > 0 {
		return fmt.Errorf("the Session %s without a workspace carries credential-like variables %v beyond the platform's own %v", id, env, gitlabPlatformEnv)
	}
	if strings.HasPrefix(probe[gitlabProbeLsRemote], "rc0 ") {
		return fmt.Errorf("the Session %s without a workspace listed the private %s at GitLab: %s", id, gp.repo, probe[gitlabProbeLsRemote])
	}
	note("Session %s without a workspace: works in %s, no credential-like variable beyond the platform's own %v, git ls-remote of the private %s at GitLab refused: %s", id, probe["pwd"], gitlabPlatformEnv, gp.repo, probe[gitlabProbeLsRemote])
	return nil
}

// gitlabPlatformEnv are the credential-like variables every Session holds,
// with or without a workspace: the Harness's messaging channel and kagent's
// propagation of the turn caller's token to its own broker; neither is a
// GitLab credential.
var gitlabPlatformEnv = []string{"CLAUDE_CODE_MESSAGING_TOKEN", "KAGENT_PROPAGATE_TOKEN"}

// gitlabCredentialEnv is the probe's credential-like variable names beyond
// gitlabPlatformEnv.
func gitlabCredentialEnv(names string) []string {
	var extra []string
	for _, n := range strings.Fields(names) {
		if !slices.Contains(gitlabPlatformEnv, n) {
			extra = append(extra, n)
		}
	}
	return extra
}

// disconnect drops the admin's grant of GitLab's application in muster and
// proves the workspace-manager's tools answer muster's sign-in link again.
func (gp *gitlabProof) disconnect() error {
	env, err := gp.admin.session.callToolEnvelope("core_auth_logout", map[string]any{serverKey: workspaceManagerMCPServer})
	if err != nil {
		return err
	}
	if env.IsError {
		return fmt.Errorf("core_auth_logout %s as %s refused: %.300s", workspaceManagerMCPServer, gp.adminUser.Email, env.Content[0].Text)
	}
	link, err := gp.admin.grantRequired("list_providers", nil)
	if err != nil {
		return err
	}
	note("list_providers as %s after core_auth_logout: muster's sign-in link %s", gp.adminUser.Email, excerpt(link, 120))
	// The cleanup removes the workspace as the admin, through the manager.
	if _, err := connectWorkspaceProviderAs(gp.cfg, gp.admin, config.WorkspaceProviderGitLab, gp.login); err != nil {
		return fmt.Errorf("granting GitLab's application again for the cleanup: %w", err)
	}
	return nil
}

// cleanupNow removes what the part created and returns the first failure.
func (gp *gitlabProof) cleanupNow() error {
	var errs []error
	if gp.sessions != nil {
		errs = append(errs, gp.sessions.removeAllNow())
	}
	if gp.adminAPI != nil && agentExists(workspacesHarnessAgent) {
		errs = append(errs, removeAgent(workspacesHarnessAgent))
	}
	if gp.sp.member != nil {
		if gone, err := gp.sp.gone(gitlabWorkspace); err == nil && !gone {
			if _, err := gp.sp.volume(gitlabWorkspace); err == nil {
				out, err := gp.sp.onVolume(gitlabWorkspace, "gitlab-clear", accessClearSessionsScript)
				errs = append(errs, err)
				if err == nil {
					note("%s", indent(out, "  "))
				}
			}
		}
		errs = append(errs, gp.sp.restore())
	}
	for _, p := range []*workspacePerson{gp.admin, gp.second} {
		if p != nil {
			p.disconnect(config.WorkspaceProviderGitLab)
		}
	}
	if gp.adminAPI != nil {
		gp.adminAPI.close()
		gp.adminAPI = nil
	}
	gp.sp.restored = true
	return errors.Join(errs...)
}

// cleanup is cleanupNow on every path, once, warning about a failure.
func (gp *gitlabProof) cleanup() {
	if gp.sp.restored {
		return
	}
	if err := gp.cleanupNow(); err != nil {
		warn("removing what the part left: %v", err)
	}
}
