package lab

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/agentlab/internal/config"
)

// `agentlab workspaces-test --part access`: the access check at a Session's
// start, as two lab users of the workspace's Organization on the lab's
// GitHub (the provider instance fake), on a workspace the part defines the
// way the sync part does. The steps:
//
//   - a workspace over agentlab-oss/docs and, where the App is installed on
//     its owner, the private agentlab-org/platform-infra, synced;
//   - the second user, not connected to the provider: authorize_workspace
//     answers connect_required with the provider's sign-in link, and a start
//     on the workspace's volume without a grant is refused by kagent's
//     admission (start_session cannot name a workspace without a grant);
//   - connect_provider as that user: the link walked through the lab
//     GitHub's consent and the workspace-manager's callback, list_providers
//     showing the provider connected;
//   - the connected second user, who cannot read the private repository:
//     authorize_workspace refused naming it;
//   - the admin, connected, after a push to agentlab-oss/docs: a grant
//     naming the admin, the workspace, its volume and the repository changed
//     since the sync; start_session with it creates the Session on the
//     volume. The same grant used by the second user, an expired grant, a
//     grant for another workspace, one for another namespace's Agents and one
//     naming no Agent namespace are refused by kagent's admission (and the
//     first two by agent-manager before kagent);
//   - disconnect_provider as the admin: the provider's log shows the
//     revocation, list_providers no sign-in, and authorize_workspace grants
//     no more.
//
// Every refused start is followed by the evidence that nothing of it exists:
// no new Session of the Agent in kagent for the person, none in
// agent-manager's list_sessions, and no new Substrate actor. The latency of
// every authorize_workspace call is recorded.

// WorkspacesPartAccess is the access part of workspaces-test.
const WorkspacesPartAccess = "access"

const (
	accessWorkspace = "agentlab-access"
	// accessDefaultSecond is the part's second user, a developer of the
	// Organization who does not read the private repository.
	accessDefaultSecond = "dev@lab.local"
	// accessOtherWorkspace names the workspace of the grant for another
	// workspace.
	accessOtherWorkspace = "agentlab-access-other"
	// accessOtherAgentNamespace is the namespace a re-signed grant names in
	// place of the lab Agents' own.
	accessOtherAgentNamespace = "agentlab-access-other"
	// accessOtherNamespaceRefusal is kagent's refusal of a grant naming
	// another Agent namespace or none.
	accessOtherNamespaceRefusal = "admission grant is for another namespace"
	// The outcomes of authorize_workspace.
	authorizeGranted         = "granted"
	authorizeConnectRequired = "connect_required"
	authorizeRefused         = "refused"
	// accessTraceSettle is how long a refused start is given to leave a
	// trace before the part looks for one.
	accessTraceSettle = 3 * time.Second
	// accessFirstMessage is start_session's first message on the admitted
	// Session: one short turn.
	accessFirstMessage = "Reply with the single word ready."
)

// accessRevocationPath is the GitHub API's revocation of a person's token or
// grant by the App.
var accessRevocationPath = regexp.MustCompile(`^` + githubFakeAPIPath + `/applications/[^/]+/(grant|token)$`)

// accessProof is the part's state.
type accessProof struct {
	sp             *syncProof
	timeout        time.Duration
	admin, second  *workspacePerson
	adminAPI       *kagentAPI
	secondAPI      *kagentAPI
	key            *ecdsa.PrivateKey
	kid            string
	volume         workspaceVolume
	latencies      []time.Duration
	adminSessions  *sessionSet
	secondSessions *sessionSet
}

// workspacesAccessPart runs the part in stages, as the admin and the second
// user email (default dev@lab.local).
func workspacesAccessPart(cfg *config.Config, email string, timeout time.Duration) error {
	p := newProofStages()
	ap := &accessProof{sp: newSyncProof(cfg, timeout, accessWorkspace), timeout: timeout}
	err := ap.run(cmp.Or(email, accessDefaultSecond), p)
	ap.cleanup()
	if err == nil {
		fmt.Printf("\nWorkspaces access part passed: authorize_workspace answered connect_required to a person not signed in to the provider, refused naming the repository a connected person cannot read and granted the admin a grant naming the person, the Agent namespace, the workspace, its volume and the changed repository; start_session started the Session on the volume with it; every refused start left no Session in kagent or agent-manager and no Substrate actor; disconnect_provider revoked the sign-in at the provider. authorize_workspace latency: %s.\n", latencySummary(ap.latencies))
	}
	return p.close(err)
}

func (ap *accessProof) run(email string, p *proofStages) error {
	sp, cfg := ap.sp, ap.sp.cfg
	adminUser := cfg.AdminUser()
	if adminUser == nil {
		return fmt.Errorf("the access part needs an admin user in %s (`agentlab configure --defaults` writes one)", config.File)
	}
	p.begin("access: preconditions", "The access part's preconditions: the workspace-manager through muster, the lab's GitHub, the Session admission, %s and %s signed in", adminUser.Email, email)
	if err := sp.preconditions(adminUser.Email); err != nil {
		return err
	}
	if err := ap.preconditions(adminUser, email); err != nil {
		return err
	}

	p.begin("access: private repository", "The private repository %s: the App's installation on %s", syncPrivateRepo, syncOrg)
	token, err := sp.gh.installationToken(syncOrg)
	switch {
	case isNoInstallation(err):
		p.skip("the App of the lab's GitHub is not installed on %s: the workspace holds the public %s alone, and the refusal naming %s is not proven", syncOrg, syncMovingRepo, syncPrivateRepo)
	case err != nil:
		return err
	default:
		sp.private, sp.tokens[syncOrg] = true, token
		note("the App is installed on %s: the workspace holds %s", syncOrg, syncPrivateRepo)
	}
	repos := []string{syncMovingRepo}
	if sp.private {
		repos = append(repos, syncPrivateRepo)
	}

	p.begin("access: workspace", "create_workspace %s over %s as %s, and its first cycle", accessWorkspace, strings.Join(repos, ", "), adminUser.Email)
	if err := ap.createWorkspace(repos); err != nil {
		return err
	}

	p.begin("access: not connected", "%s, not signed in to %s: authorize_workspace answers with the provider's sign-in link, a start on the volume without a grant is refused, and nothing of it exists", email, config.WorkspaceProviderFake)
	if err := ap.notConnected(); err != nil {
		return err
	}

	p.begin("access: connect", "connect_provider as %s: the link walked through the lab GitHub's consent and the workspace-manager's callback, list_providers showing %s connected", email, config.WorkspaceProviderFake)
	hops, err := connectWorkspaceProvider(cfg, ap.second, config.WorkspaceProviderFake)
	if err != nil {
		return err
	}
	note("%s", hops)
	note("list_providers as %s: %s connected", email, config.WorkspaceProviderFake)
	note("the github provider's sign-in, a browser sign-in of a person's account, is not run here")

	p.begin("access: refused", "authorize_workspace as the connected %s, who cannot read %s: refused naming it, and nothing of the attempt exists", email, syncPrivateRepo)
	if !sp.private {
		p.skip("no lab GitHub App installation on %s: the workspace holds no repository %s cannot read", syncOrg, email)
	} else if err := ap.refusedNaming(); err != nil {
		return err
	}

	p.begin("access: grant", "A push to %s, then authorize_workspace as %s: a grant naming the person, the workspace, its volume and %s as changed since the sync", syncMovingRepo, adminUser.Email, syncMovingRepo)
	grant, err := ap.grant()
	if err != nil {
		return err
	}

	p.begin("access: start", "start_session with the grant as %s: the Session on the workspace's volume", adminUser.Email)
	if err := ap.start(grant); err != nil {
		return err
	}

	p.begin("access: admission", "kagent's admission refuses the same grant used by %s, an expired grant, a grant for another workspace, one for another namespace's Agents and one naming none; nothing of them exists", email)
	if err := ap.admissionRefusals(grant); err != nil {
		return err
	}

	p.begin("access: disconnect", "disconnect_provider as %s: the provider's log shows the revocation, and authorize_workspace grants no more", adminUser.Email)
	if err := ap.disconnect(); err != nil {
		return err
	}

	p.begin("access: latency", "The latency of authorize_workspace")
	note("%s", latencySummary(ap.latencies))

	p.begin("access: restore", "The Sessions, the Agent and the workspace removed, %s back to %s", syncMovingRepo, shortSHA(sp.startHead))
	return ap.cleanupNow()
}

// isNoInstallation says whether err is the lab GitHub's answer that the App
// is not installed on an owner.
func isNoInstallation(err error) bool { return errors.Is(err, errNoInstallation) }

// preconditions signs both people in to muster and kagent, and checks the
// Session admission's rig and the claude Agent the starts are made on.
func (ap *accessProof) preconditions(adminUser *config.User, email string) error {
	cfg := ap.sp.cfg
	secondUser := cfg.FindUser(email)
	switch {
	case secondUser == nil:
		return fmt.Errorf("no user %s in %s", email, config.File)
	case strings.EqualFold(secondUser.Email, adminUser.Email):
		return fmt.Errorf("the second user must be another person than the admin %s", adminUser.Email)
	case !slices.ContainsFunc(config.WorkspaceOrganizationGroups, secondUser.HasGroup):
		return fmt.Errorf("%s is no member of the Organization %s (groups %s): authorize_workspace answers a member", email, config.WorkspaceOrganization, strings.Join(config.WorkspaceOrganizationGroups, ", "))
	}
	rig := workspaceAdmissionValuesFor(cfg, workspacesChartCarries(cfg))
	if rig == nil {
		return fmt.Errorf("no Session admission on this lab (the chart takes no workspaces values, or platform.agents is off): the access part starts Sessions on a workspace volume")
	}
	var err error
	if ap.key, err = readWorkspaceGrantKey(); err != nil {
		return err
	}
	ctx := context.Background()
	if ap.kid, err = proveWorkspaceManagerPublishesGrantKey(ctx, ap.key); err != nil {
		return err
	}
	if err := proveKagentAdmissionTrusts(ctx, rig); err != nil {
		return err
	}
	note("the manager publishes the lab's grant key as %s; kagent's admission trusts %s (issuer %s)", ap.kid, rig.JWKSURL, rig.Issuer)
	if ap.admin, err = openWorkspacePerson(cfg, adminUser); err != nil {
		return err
	}
	if ap.second, err = openWorkspacePerson(cfg, secondUser); err != nil {
		return err
	}
	for _, person := range []struct {
		user *config.User
		api  **kagentAPI
		set  **sessionSet
	}{{adminUser, &ap.adminAPI, &ap.adminSessions}, {secondUser, &ap.secondAPI, &ap.secondSessions}} {
		token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, person.user.Email, person.user.Password, musterLoginScopes)
		if err != nil {
			return err
		}
		if *person.api, err = dialKagentAPI(cfg, token); err != nil {
			return err
		}
		*person.set = &sessionSet{api: *person.api}
	}
	note("signed in to muster and kagent as %s (fixture login %s) and %s (fixture login %s)", adminUser.Email, ap.admin.login, email, ap.second.login)

	if err := waitHarnessReady(ctx, rig.ClaudeHarness, ap.timeout); err != nil {
		return err
	}
	if agentExists(workspacesHarnessAgent) {
		if err := removeAgent(workspacesHarnessAgent); err != nil {
			return err
		}
	}
	if _, err := applyManifests(ctx, []byte(workspacesHarnessAgentManifest(rig.ClaudeHarness))); err != nil {
		return fmt.Errorf("applying the claude Agent: %w", err)
	}
	boot, err := waitAgentReadyOn(workspacesHarnessAgent, rig.ClaudeHarness, ap.timeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return fmt.Errorf("the claude Agent %s is not Ready on Harness %s after %s: %s", workspacesHarnessAgent, rig.ClaudeHarness, boot.elapsed.Round(time.Second), boot.reason)
	}
	note("the claude Agent %s Ready on Harness %s in %s", workspacesHarnessAgent, rig.ClaudeHarness, boot.elapsed.Round(time.Second))
	return nil
}

// createWorkspace defines the part's workspace as the admin and waits for its
// first cycle; the volume is the one its grants name.
func (ap *accessProof) createWorkspace(repos []string) error {
	v, err := ap.sp.createNamed(accessWorkspace, repos)
	if err != nil {
		return err
	}
	ap.volume = v
	return nil
}

// authorize calls authorize_workspace as the person and records its latency.
func (ap *accessProof) authorize(p *workspacePerson) (workspaceAuthorization, error) {
	started := time.Now()
	a, err := p.authorizeWorkspace(accessWorkspace)
	took := time.Since(started)
	if err == nil {
		ap.latencies = append(ap.latencies, took)
	}
	return a, err
}

// notConnected proves the second user, signed in nowhere, is asked to
// connect, and that a start on the volume without a grant is refused
// leaving nothing behind.
func (ap *accessProof) notConnected() error {
	p := ap.second
	p.disconnect(config.WorkspaceProviderFake)
	status, err := p.providerStatus(config.WorkspaceProviderFake)
	if err != nil {
		return err
	}
	if status.Connected {
		return fmt.Errorf("list_providers shows %s still connected for %s after disconnect_provider", config.WorkspaceProviderFake, p.user.Email)
	}
	note("list_providers as %s: %s", p.user.Email, status)
	before, err := ap.traces(p)
	if err != nil {
		return err
	}
	a, err := ap.authorize(p)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(a.Connect, func(c struct {
		Provider string `json:"provider"`
		URL      string `json:"url"`
	}) bool {
		return c.Provider == config.WorkspaceProviderFake
	})
	if a.Outcome != authorizeConnectRequired || a.Grant != "" || i < 0 || a.Connect[i].URL == "" {
		return fmt.Errorf("authorize_workspace as %s, not connected, answered %s (%s, connect %v, a grant: %t), want %s with %s's sign-in link and no grant", p.user.Email, a.Outcome, a.Reason, a.Connect, a.Grant != "", authorizeConnectRequired, config.WorkspaceProviderFake)
	}
	note("authorize_workspace: %s in %s — %q; sign-in link %s", a.Outcome, ap.latencies[len(ap.latencies)-1].Round(time.Millisecond), a.Reason, a.Connect[i].URL)

	reason, err := workspacesHarnessRefused(context.Background(), ap.secondAPI, workspacesHarnessVolumeSource(ap.volume), "")
	if err != nil {
		return fmt.Errorf("a start on %s's volume without a grant as %s: %w", accessWorkspace, p.user.Email, err)
	}
	note("a start on the volume without a grant (kagent CreateSession as %s; this step of the default workspaces-test proves the same for the admin): refused — %s", p.user.Email, reason)
	note("start_session takes a workspace only as a grant: without one it starts a Session with no volume, as before workspaces")
	return ap.nothingSince(p, before)
}

// refusedNaming proves the connected second user is refused, naming the
// private repository, and that nothing of the attempt exists.
func (ap *accessProof) refusedNaming() error {
	p := ap.second
	before, err := ap.traces(p)
	if err != nil {
		return err
	}
	a, err := ap.authorize(p)
	if err != nil {
		return err
	}
	if a.Outcome != authorizeRefused || a.Grant != "" || !slices.Contains(a.Unreadable, syncPrivateRepo) || !strings.Contains(a.Reason, syncPrivateRepo) {
		return fmt.Errorf("authorize_workspace as %s answered %s (%q, unreadable %v, a grant: %t), want %s naming %s", p.user.Email, a.Outcome, a.Reason, a.Unreadable, a.Grant != "", authorizeRefused, syncPrivateRepo)
	}
	note("authorize_workspace: %s in %s — %q (unreadable %s)", a.Outcome, ap.latencies[len(ap.latencies)-1].Round(time.Millisecond), a.Reason, strings.Join(a.Unreadable, ", "))
	return ap.nothingSince(p, before)
}

// accessGrantClaims are the claims of a manager's grant the part judges.
type accessGrantClaims struct {
	Subject        string            `json:"sub"`
	Expiry         int64             `json:"exp"`
	AgentNamespace string            `json:"agent_namespace"`
	Workspace      map[string]string `json:"workspace"`
	Volume         workspaceVolume   `json:"volume"`
	Mounts         []workspaceMount  `json:"mounts"`
	Changed        []string          `json:"changed"`
}

// grantPayload decodes a grant's claims without verifying it, generically
// (to sign them again with a claim changed) and as the claims judged.
func grantPayload(grant string) (map[string]any, accessGrantClaims, error) {
	parts := strings.Split(grant, ".")
	if len(parts) != 3 {
		return nil, accessGrantClaims{}, fmt.Errorf("the grant is no compact JWS (%d parts)", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, accessGrantClaims{}, fmt.Errorf("decoding the grant's payload: %w", err)
	}
	var generic map[string]any
	var claims accessGrantClaims
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, accessGrantClaims{}, fmt.Errorf("the grant's payload: %w", err)
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, accessGrantClaims{}, fmt.Errorf("the grant's claims: %w", err)
	}
	return generic, claims, nil
}

// judgeGrant is the verdict on the admin's grant: it names the person, the
// namespace of the lab's Agents, the workspace, its volume, the workspace's mounts and exactly the changed
// repositories.
func judgeGrant(c accessGrantClaims, person, workspace string, volume workspaceVolume, changed []string) error {
	var problems []string
	if c.Subject != person {
		problems = append(problems, fmt.Sprintf("names %q, not %s", c.Subject, person))
	}
	if c.AgentNamespace != kagentNamespace {
		problems = append(problems, fmt.Sprintf("names Agent namespace %q, not %s (agent_namespace, workspace-manager 0.9.0 or newer)", c.AgentNamespace, kagentNamespace))
	}
	if c.Workspace[nameKey] != workspace {
		problems = append(problems, fmt.Sprintf("names workspace %q, not %s", c.Workspace[nameKey], workspace))
	}
	if c.Volume != volume {
		problems = append(problems, fmt.Sprintf("names volume %s %s, not the workspace's %s %s", c.Volume.Driver, c.Volume.Handle, volume.Driver, volume.Handle))
	}
	want := []workspaceMount{{SubPath: workspaceGrantSessionDirectory}, {SubPath: workspaceGrantMirrors, ReadOnly: true}}
	if !slices.Equal(c.Mounts, want) {
		problems = append(problems, fmt.Sprintf("allows mounts %v, not the Session's own directory and the mirrors read-only", c.Mounts))
	}
	got := slices.Clone(c.Changed)
	slices.Sort(got)
	if !slices.Equal(got, changed) {
		problems = append(problems, fmt.Sprintf("names %v changed since the sync, want %v", c.Changed, changed))
	}
	if len(problems) > 0 {
		return fmt.Errorf("the grant %s", strings.Join(problems, "; "))
	}
	return nil
}

// grant pushes to the public repository, connects the admin and asks for a
// grant until the push is seen as changed since the sync; it returns the
// grant, judged.
func (ap *accessProof) grant() (string, error) {
	sp, p := ap.sp, ap.admin
	pushed, err := sp.push()
	if err != nil {
		return "", err
	}
	hops, err := connectWorkspaceProvider(sp.cfg, p, config.WorkspaceProviderFake)
	if err != nil {
		return "", err
	}
	note("%s connected to %s: %s", p.user.Email, config.WorkspaceProviderFake, hops)
	// The manager reads what the person sees from a listing it may cache for
	// a minute: the push shows once a fresh listing is made.
	deadline := time.Now().Add(syncListingFreshness + 30*time.Second)
	var a workspaceAuthorization
	for {
		if a, err = ap.authorize(p); err != nil {
			return "", err
		}
		if a.Outcome != authorizeGranted || a.Grant == "" {
			return "", fmt.Errorf("authorize_workspace as %s answered %s (%q, unreadable %v, connect %v), want granted", p.user.Email, a.Outcome, a.Reason, a.Unreadable, a.Connect)
		}
		if slices.Contains(a.Changed, syncMovingRepo) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	_, claims, err := grantPayload(a.Grant)
	if err != nil {
		return "", err
	}
	if err := judgeGrant(claims, p.user.Email, accessWorkspace, ap.volume, []string{syncMovingRepo}); err != nil {
		return "", fmt.Errorf("%w (pushed %s to %s after the sync)", err, shortSHA(pushed), syncMovingRepo)
	}
	expires := time.Unix(claims.Expiry, 0).UTC()
	note("authorize_workspace: %s in %s — a grant of %d bytes, sub %s, agent_namespace %s, workspace %s/%s, volume %s, mounts %s and %s read-only, changed %s, expires %s (repositories %s)",
		a.Outcome, ap.latencies[len(ap.latencies)-1].Round(time.Millisecond), len(a.Grant), claims.Subject, claims.AgentNamespace, claims.Workspace[namespaceKey], claims.Workspace[nameKey], claims.Volume.Handle,
		workspaceGrantSessionDirectory, workspaceGrantMirrors, strings.Join(claims.Changed, ", "), expires.Format(time.RFC3339), strings.Join(a.Repositories, ", "))
	return a.Grant, nil
}

// accessStarted is start_session's answer, the fields the part reads.
type accessStarted struct {
	Session struct {
		ID      string `json:"id"`
		Creator string `json:"creator"`
	} `json:"session"`
	Workspace string `json:"workspace"`
}

// startSession calls agent-manager's start_session as the person, with the
// grant when one is given.
func startSession(p *workspacePerson, grant string) (*toolEnvelope, error) {
	args := map[string]any{nameKey: workspacesHarnessAgent, namespaceKey: kagentNamespace, "message": accessFirstMessage, startSessionRequestIDArg: uuid.NewString()}
	if grant != "" {
		args[startSessionGrantArg] = grant
	}
	return p.session.callToolEnvelope(agentManagerToolPrefix+"start_session", args)
}

// start starts the admin's Session with the grant through agent-manager and
// reads it back from kagent: on the workspace's volume.
func (ap *accessProof) start(grant string) error {
	p := ap.admin
	env, err := startSession(p, grant)
	if err != nil {
		return err
	}
	if env.IsError {
		return fmt.Errorf("start_session with the grant as %s refused: %.400s", p.user.Email, env.Content[0].Text)
	}
	var started accessStarted
	if err := json.Unmarshal([]byte(env.Content[0].Text), &started); err != nil || started.Session.ID == "" {
		return fmt.Errorf("start_session answered no session: %.400s", env.Content[0].Text)
	}
	ap.adminSessions.add(started.Session.ID)
	ctx, cancel := context.WithTimeout(context.Background(), ap.timeout)
	defer cancel()
	session, err := ap.adminAPI.getSession(ctx, started.Session.ID)
	if err != nil {
		return err
	}
	vs := session.GetVolumeSource().GetVolume()
	if vs.GetVolumeHandle() != ap.volume.Handle || vs.GetCsiDriver() != ap.volume.Driver {
		return fmt.Errorf("the Session %s started with the grant is on volume %q (%q), want the workspace's %s", started.Session.ID, vs.GetVolumeHandle(), vs.GetCsiDriver(), ap.volume.Handle)
	}
	note("start_session: Session %s of %s (creator %s, workspace %s), state %s, on volume %s", started.Session.ID, workspacesHarnessAgent, session.GetCreator(), started.Workspace, sessionState(session), vs.GetVolumeHandle())
	return nil
}

// admissionRefusals presents the admin's grant as the second user, an
// expired grant and a grant for another workspace, each straight to kagent
// (its admission's verdict) and, where agent-manager decides first, through
// start_session; nothing of any of them may exist after.
func (ap *accessProof) admissionRefusals(grant string) error {
	generic, _, err := grantPayload(grant)
	if err != nil {
		return err
	}
	resign := func(change func(map[string]any)) (string, error) {
		claims := map[string]any{}
		for k, v := range generic {
			claims[k] = v
		}
		claims["jti"] = randomHex(16)
		change(claims)
		return signWorkspaceClaims(ap.key, ap.kid, claims)
	}
	now := time.Now().UTC()
	expired, err := resign(func(c map[string]any) {
		c["iat"], c["nbf"], c["exp"] = now.Add(-10*time.Minute).Unix(), now.Add(-10*time.Minute).Unix(), now.Add(-5*time.Minute).Unix()
	})
	if err != nil {
		return err
	}
	other, err := resign(func(c map[string]any) {
		c["workspace"] = map[string]string{namespaceKey: kagentNamespace, nameKey: accessOtherWorkspace}
		c["volume"] = workspaceVolume{Driver: ap.volume.Driver, Handle: ap.volume.Handle + "-" + accessOtherWorkspace}
	})
	if err != nil {
		return err
	}
	otherNamespace, err := resign(func(c map[string]any) { c["agent_namespace"] = accessOtherAgentNamespace })
	if err != nil {
		return err
	}
	noNamespace, err := resign(func(c map[string]any) { delete(c, "agent_namespace") })
	if err != nil {
		return err
	}
	source := workspacesHarnessVolumeSource(ap.volume)
	for _, r := range []struct {
		what    string
		person  *workspacePerson
		api     *kagentAPI
		grant   string
		manager bool
		// reason is the refusal kagent must name; empty takes any.
		reason string
	}{
		{fmt.Sprintf("the admin's grant used by %s", ap.second.user.Email), ap.second, ap.secondAPI, grant, true, ""},
		{"an expired grant (the manager's, re-signed with the lab's grant key with its expiry 5 minutes past)", ap.admin, ap.adminAPI, expired, true, ""},
		{fmt.Sprintf("a grant for another workspace (%s on another volume, signed with the lab's grant key) presented on %s's volume", accessOtherWorkspace, accessWorkspace), ap.admin, ap.adminAPI, other, false, ""},
		{fmt.Sprintf("a grant for the Agents of namespace %s (the manager's, re-signed with the lab's grant key) presented on Agent %s/%s", accessOtherAgentNamespace, kagentNamespace, workspacesHarnessAgent), ap.admin, ap.adminAPI, otherNamespace, false, accessOtherNamespaceRefusal},
		{"a grant without agent_namespace (the manager's, re-signed with the lab's grant key without the claim)", ap.admin, ap.adminAPI, noNamespace, false, accessOtherNamespaceRefusal},
	} {
		before, err := ap.traces(r.person)
		if err != nil {
			return err
		}
		if r.manager {
			env, err := startSession(r.person, r.grant)
			if err != nil {
				return err
			}
			if !env.IsError {
				var started accessStarted
				_ = json.Unmarshal([]byte(env.Content[0].Text), &started)
				return fmt.Errorf("%s: start_session as %s started Session %s", r.what, r.person.user.Email, started.Session.ID)
			}
			note("%s: start_session as %s refused by agent-manager — %.300s", r.what, r.person.user.Email, env.Content[0].Text)
		}
		reason, err := workspacesHarnessRefused(context.Background(), r.api, source, r.grant)
		if err != nil {
			return fmt.Errorf("%s: kagent CreateSession as %s: %w", r.what, r.person.user.Email, err)
		}
		if !strings.Contains(reason, r.reason) {
			return fmt.Errorf("%s: kagent CreateSession as %s refused for another reason than %q: %s", r.what, r.person.user.Email, r.reason, reason)
		}
		note("%s: kagent CreateSession as %s refused by the admission — %s", r.what, r.person.user.Email, reason)
		if err := ap.nothingSince(r.person, before); err != nil {
			return err
		}
	}
	note("a grant signed by a key the manager does not publish: refused in the default workspaces-test's admission step, not repeated here")
	return nil
}

// disconnect forgets the admin's sign-in and proves the provider revoked it
// and that authorize_workspace grants no more.
func (ap *accessProof) disconnect() error {
	sp, p := ap.sp, ap.admin
	mark, err := sp.mark()
	if err != nil {
		return err
	}
	env, err := p.session.callToolEnvelope(workspaceManagerToolPrefix+"disconnect_provider", map[string]any{workspaceProviderArg: config.WorkspaceProviderFake})
	if err != nil {
		return err
	}
	if env.IsError {
		return fmt.Errorf("disconnect_provider as %s refused: %.300s", p.user.Email, env.Content[0].Text)
	}
	reqs, err := sp.gh.requests()
	if err != nil {
		return err
	}
	revocations := revocationsOf(reqs[min(mark, len(reqs)):])
	if len(revocations) == 0 {
		return fmt.Errorf("the lab GitHub's request log shows no revocation (DELETE %s/applications/<client>/grant or /token answered 2xx) after disconnect_provider as %s", githubFakeAPIPath, p.user.Email)
	}
	note("the provider's log: %s", strings.Join(revocations, "; "))
	status, err := p.providerStatus(config.WorkspaceProviderFake)
	if err != nil {
		return err
	}
	if status.Connected {
		return fmt.Errorf("list_providers shows %s still connected for %s after disconnect_provider", config.WorkspaceProviderFake, p.user.Email)
	}
	note("list_providers as %s: %s", p.user.Email, status)
	a, err := ap.authorize(p)
	if err != nil {
		return err
	}
	if a.Outcome != authorizeConnectRequired || a.Grant != "" {
		return fmt.Errorf("authorize_workspace as %s after disconnect_provider answered %s (%q, a grant: %t), want %s", p.user.Email, a.Outcome, a.Reason, a.Grant != "", authorizeConnectRequired)
	}
	note("authorize_workspace: %s in %s — %q; a grant issued before stays valid until its expiry (a revocation after start is not checked again)", a.Outcome, ap.latencies[len(ap.latencies)-1].Round(time.Millisecond), a.Reason)
	return nil
}

// revocationsOf words the request log's revocations: each DELETE of a token
// or grant by the App that the provider accepted.
func revocationsOf(reqs []githubRequest) []string {
	var out []string
	for _, r := range reqs {
		if r.Method == http.MethodDelete && accessRevocationPath.MatchString(r.Path) && r.Status/100 == 2 {
			out = append(out, fmt.Sprintf("%s %s %s %d", r.Time.UTC().Format(time.RFC3339), r.Method, r.Path, r.Status))
		}
	}
	return out
}

// accessTraces is what a start leaves: the Agent's Sessions in kagent for the
// person, agent-manager's list_sessions for them, and Substrate's actors.
type accessTraces struct {
	kagent, manager []string
	actors          []string
}

// traces reads what exists now for the person.
func (ap *accessProof) traces(p *workspacePerson) (accessTraces, error) {
	api := ap.secondAPI
	if p == ap.admin {
		api = ap.adminAPI
	}
	return sessionTraces(p, api, ap.adminAPI, workspacesHarnessAgent)
}

// sessionTraces reads what exists now of the person's Sessions of agent:
// kagent's as the person (api), agent-manager's list_sessions as them, and
// Substrate's actors, read through state.
func sessionTraces(p *workspacePerson, api, state *kagentAPI, agent string) (accessTraces, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	var t accessTraces
	sessions, err := api.listSessionsOf(ctx, agentRef(agent))
	if err != nil {
		return t, err
	}
	for _, s := range sessions {
		t.kagent = append(t.kagent, s.GetId())
	}
	var listed struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := p.session.callServerJSON(agentManagerToolPrefix+"list_sessions", map[string]any{nameKey: agent, namespaceKey: kagentNamespace}, &listed); err != nil {
		return t, err
	}
	for _, s := range listed.Sessions {
		t.manager = append(t.manager, s.ID)
	}
	actors, err := state.substrateState(ctx, kagentNamespace)
	if err != nil {
		return t, err
	}
	for _, a := range actors.actors {
		t.actors = append(t.actors, a.id)
	}
	return t, nil
}

// nothingSince is the evidence for a refused start: after a moment, no
// Session, list_sessions entry or actor that was not there before.
func (ap *accessProof) nothingSince(p *workspacePerson, before accessTraces) error {
	time.Sleep(accessTraceSettle)
	after, err := ap.traces(p)
	if err != nil {
		return err
	}
	if err := judgeNothingNew(before, after); err != nil {
		return fmt.Errorf("after the refusal as %s: %w", p.user.Email, err)
	}
	note("nothing of it exists: kagent holds %d Sessions of %s for %s, agent-manager's list_sessions %d, Substrate %d actors — none new", len(after.kagent), workspacesHarnessAgent, p.user.Email, len(after.manager), len(after.actors))
	return nil
}

// judgeNothingNew names every Session and actor in after that is not in
// before.
func judgeNothingNew(before, after accessTraces) error {
	var problems []string
	for _, c := range []struct {
		what          string
		before, after []string
	}{
		{"kagent holds a new Session", before.kagent, after.kagent},
		{"agent-manager's list_sessions lists a new Session", before.manager, after.manager},
		{"Substrate holds a new actor", before.actors, after.actors},
	} {
		for _, id := range c.after {
			if !slices.Contains(c.before, id) {
				problems = append(problems, c.what+" "+id)
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("a refused start left %s", strings.Join(problems, ", "))
	}
	return nil
}

// latencySummary words the latencies: the count, the minimum, the median and
// the maximum.
func latencySummary(ds []time.Duration) string {
	if len(ds) == 0 {
		return "no calls"
	}
	sorted := slices.Clone(ds)
	slices.Sort(sorted)
	r := func(d time.Duration) time.Duration { return d.Round(time.Millisecond) }
	return fmt.Sprintf("%d calls, min %s, median %s, max %s", len(sorted), r(sorted[0]), r(sorted[len(sorted)/2]), r(sorted[len(sorted)-1]))
}

// accessClearSessionsScript removes every session directory from the
// workspace's volume, so its delete is not held by one.
const accessClearSessionsScript = `rm -rf /workspace/` + workspacesTestSessions + `/*
echo "sessions: $(ls -A /workspace/` + workspacesTestSessions + ` 2>/dev/null | wc -l) directories left"
`

// cleanupNow removes what the part created and returns the first failure:
// the Sessions, both people's sign-ins, the Agent, the session directories,
// the workspace and the moving repository's head.
func (ap *accessProof) cleanupNow() error {
	var errs []error
	for _, s := range []*sessionSet{ap.adminSessions, ap.secondSessions} {
		if s != nil {
			errs = append(errs, s.removeAllNow())
		}
	}
	for _, p := range []*workspacePerson{ap.admin, ap.second} {
		if p != nil {
			p.disconnect(config.WorkspaceProviderFake)
		}
	}
	if agentExists(workspacesHarnessAgent) {
		errs = append(errs, removeAgent(workspacesHarnessAgent))
	}
	if ap.sp.member != nil {
		if gone, err := ap.sp.gone(accessWorkspace); err == nil && !gone {
			if _, err := ap.sp.volume(accessWorkspace); err == nil {
				out, err := ap.sp.onVolume(accessWorkspace, "access-clear", accessClearSessionsScript)
				errs = append(errs, err)
				if err == nil {
					note("%s", indent(out, "  "))
				}
			}
		}
	}
	errs = append(errs, ap.sp.restore())
	for _, api := range []*kagentAPI{ap.adminAPI, ap.secondAPI} {
		if api != nil {
			api.close()
		}
	}
	ap.adminAPI, ap.secondAPI = nil, nil
	return errors.Join(errs...)
}

// cleanup is cleanupNow on every path, once, warning about a failure.
func (ap *accessProof) cleanup() {
	if ap.sp.restored {
		return
	}
	if err := ap.cleanupNow(); err != nil {
		warn("removing what the part left: %v", err)
	}
}
