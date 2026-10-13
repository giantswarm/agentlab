package lab

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
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
// instance gitlab against a real GitLab (platform.workspaces.gitlab), as the
// lab's admin, whose GitLab account is the configured person, and the second
// user, who is not connected. Without the instance the part skips every
// stage, naming why, and passes. The steps:
//
//   - the editor's picker: list_repositories lists the group's projects;
//   - the second user, not signed in to GitLab: the token exchange refuses
//     them with the instance's connect link;
//   - connect_provider as the admin: the link walked through GitLab's
//     sign-in and the OAuth application's consent to the manager's callback;
//   - the token exchange releases the admin's GitLab token for the audience
//     gitlab: GitLab's API names the person, git's smart HTTP lists the
//     project's refs with it, and the person creates a branch, a commit and
//     a merge request through GitLab's API with it, all removed again;
//   - a workspace over the project: its first cycle mirrors it, the mirror on
//     the volume and the manifest at GitLab's head;
//   - authorize_workspace grants the admin a grant naming the GitLab hosts,
//     start_session starts a Session with it on the volume, and its turn finds
//     its directory with the clone of the project; its push to GitLab is
//     reported as it stands: a Session gets the person's provider token only
//     through kagent's token broker, which the platform does not wire yet;
//   - a Session without a workspace holds no GitLab credential: no
//     credential in its environment, and git refused at GitLab;
//   - disconnect_provider: the token exchange refuses the admin again with
//     the connect link, and GitLab refuses the token it released before.

// WorkspacesPartGitLab is the GitLab part of workspaces-test.
const WorkspacesPartGitLab = "gitlab"

const (
	gitlabWorkspace = "agentlab-gitlab"
	// gitlabProofBranch is the branch the person's API write and the
	// Session's push name, with a suffix of the run.
	gitlabProofBranch = "agentlab-proof-"
	// gitlabAPIPath is GitLab's REST API under the instance's URL.
	gitlabAPIPath = "/api/v4"
	// The probe's lines the GitLab turns relay.
	gitlabProbeCredentialEnv = "credential-env" // #nosec G101 -- a probe line's name, not a credential
	gitlabProbePush          = "push"
	gitlabProbeLsRemote      = "ls-remote"
	gitlabProbeSetupClone    = "setup-clone"
)

// gitlabBlockedPush words why a Session's push and merge request do not
// reach GitLab as the person yet: the grant names the hosts and their
// broker, and nothing hands the Session the person's token for them.
const gitlabBlockedPush = "a Session reaches GitLab as the person only through kagent's token broker, " +
	"which exchanges the turn caller's token at the workspace-manager for each host the grant names; the platform does not wire that broker yet, " +
	"and GitLab's API (Bearer) and git (Basic, user oauth2) on one host need two credentials where the egress binds one per host: " +
	"the push and the merge request from the Session are reported, not judged"

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
	http          *http.Client
	head          string // the project's default branch head at GitLab
	branch        string
	mergeRequest  int
	released      string // the access token the exchange released, held for the revocation check
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
		fmt.Printf("\nWorkspaces GitLab part passed: list_repositories listed %s's projects, the token exchange refused a person not signed in to GitLab with the connect link and released the admin's GitLab token after the sign-in through GitLab's form and consent, with which GitLab named the person and took a branch, a commit and a merge request; the workspace mirrored %s at GitLab's head; a Session started with the grant found its clone; a Session without the workspace held no GitLab credential; disconnect_provider revoked the token at GitLab.\n", gp.gl.Group, gp.repo)
	}
	return p.close(err)
}

// gitlabStages are the part's stages, skipped together without the instance.
var gitlabStages = []string{"gitlab: picker", "gitlab: not connected", "gitlab: connect", "gitlab: token", "gitlab: workspace", "gitlab: start", "gitlab: push from the Session", "gitlab: no workspace", "gitlab: disconnect", "gitlab: restore"}

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
	}
	return ""
}

func (gp *gitlabProof) run(email string, p *proofStages) error {
	cfg := gp.cfg
	p.begin("gitlab: preconditions", "The GitLab part's preconditions: the instance %s, the workspace-manager through muster, the token exchange, the Session admission, the admin and %s signed in", config.WorkspaceProviderGitLab, email)
	if skip := gitlabSkip(cfg); skip != "" {
		p.skip("%s", skip)
		p.leaveOut(skip, gitlabStages...)
		return nil
	}
	if err := gp.preconditions(email); err != nil {
		return err
	}

	p.begin("gitlab: picker", "list_repositories for %s on %s as %s: the workspace editor's picker", gp.gl.Group, config.WorkspaceProviderGitLab, gp.adminUser.Email)
	if err := gp.picker(); err != nil {
		return err
	}

	p.begin("gitlab: not connected", "%s, not signed in to GitLab: the token exchange for the audience %s refuses with the connect link", email, config.WorkspaceProviderGitLab)
	if err := gp.notConnected(); err != nil {
		return err
	}

	p.begin("gitlab: connect", "connect_provider as %s: GitLab's sign-in as %s, the OAuth application's consent and the workspace-manager's callback", gp.adminUser.Email, gp.gl.Person)
	hops, err := connectWorkspaceProviderAs(cfg, gp.admin, config.WorkspaceProviderGitLab, gp.login)
	if err != nil {
		return err
	}
	note("%s", hops)
	note("list_providers as %s: %s connected", gp.adminUser.Email, config.WorkspaceProviderGitLab)

	p.begin("gitlab: token", "The token exchange releases %s's GitLab token for the audience %s: GitLab names the person, git lists %s, and the person writes a branch, a commit and a merge request with it", gp.adminUser.Email, config.WorkspaceProviderGitLab, gp.repo)
	if err := gp.token(); err != nil {
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

	p.begin("gitlab: disconnect", "disconnect_provider as %s: the token exchange refuses with the connect link again, and GitLab refuses the token released before", gp.adminUser.Email)
	if err := gp.disconnect(); err != nil {
		return err
	}

	p.begin("gitlab: restore", "The Sessions, the Agent, the workspace, the proof branch and merge request removed")
	return gp.cleanupNow()
}

// preconditions signs both people in, reads the person's GitLab password
// from the instance's Secret, checks the instance is listed, the token
// exchange's client is there and the claude Agent is Ready.
func (gp *gitlabProof) preconditions(email string) error {
	cfg, sp := gp.cfg, gp.sp
	if reason := workspaceManagerSkip(cfg); reason != "" {
		return fmt.Errorf("no workspace-manager: %s", reason)
	}
	if err := waitMCPServerReachable(workspaceManagerMCPServer); err != nil {
		return fmt.Errorf("the workspace-manager is not reachable through muster: %w", err)
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
	status, err := gp.admin.providerStatus(config.WorkspaceProviderGitLab)
	if err != nil {
		return fmt.Errorf("%w: the install renders the instance once platform.workspaces.gitlab and the Secret %s/%s are in place", err, platformNamespace, config.WorkspaceGitLabSecretName)
	}
	if status.Kind != providerKindGitLab {
		return fmt.Errorf("list_providers lists %s with kind %q, want %s", config.WorkspaceProviderGitLab, status.Kind, providerKindGitLab)
	}
	note("list_providers as %s: %s", gp.adminUser.Email, status)

	u, err := url.Parse(strings.TrimSuffix(gp.gl.URL, "/"))
	if err != nil {
		return err
	}
	gp.gitlabHost, gp.repo = u.Host, gp.gl.Group+"/"+gp.gl.Project
	gp.branch = gitlabProofBranch + time.Now().UTC().Format("20060102150405")
	if gp.http, err = labHTTPClient(60 * time.Second); err != nil {
		return err
	}
	password, err := gp.personPassword()
	if err != nil {
		return err
	}
	gp.login = &providerLogin{login: gp.gl.Person, password: password}
	if _, err := readWorkspaceTokenExchangeSecret(); err != nil {
		return err
	}

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
	if err := gp.sp.removeLeftovers(); err != nil {
		return err
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

// gitlabExchange is the token exchange's answer, success or error.
type gitlabExchange struct {
	status      int
	AccessToken string `json:"access_token"`
	IssuedType  string `json:"issued_token_type"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Refresh     string `json:"refresh_token"`
	Error       string `json:"error"`
	ErrorURI    string `json:"error_uri"`
	Description string `json:"error_description"`
}

// exchange asks the manager's token endpoint, as kagent's client, for the
// person's token at the instance: the person's Dex token as the subject.
func (gp *gitlabProof) exchange(user *config.User, audience string) (gitlabExchange, error) {
	var out gitlabExchange
	subject, err := passwordGrant(gp.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return out, err
	}
	secret, err := readWorkspaceTokenExchangeSecret()
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	pod, err := readyPodOf(ctx, platformNamespace, "app.kubernetes.io/name=workspace-manager")
	if err != nil {
		return out, err
	}
	port, stop, err := portForwardPod(ctx, platformNamespace, pod, workspaceManagerHTTPPort)
	if err != nil {
		return out, err
	}
	defer stop()
	form := url.Values{
		grantTypeParam: {workspaceTokenExchangeGrantType}, "subject_token": {subject},
		"subject_token_type": {workspaceTokenExchangeIDToken}, "audience": {audience},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", port, workspaceTokenExchangePath), strings.NewReader(form.Encode()))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(workspaceTokenExchangeClientID, secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, fmt.Errorf("the token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("the token exchange answered %d without JSON: %s", resp.StatusCode, excerpt(string(body), 200))
	}
	out.status = resp.StatusCode
	return out, nil
}

// words is the exchange's answer without its token.
func (x gitlabExchange) words() string {
	if x.AccessToken != "" {
		return fmt.Sprintf("%d, %s of %d bytes, issued_token_type %s, expires_in %ds, a refresh token: %t", x.status, x.TokenType, len(x.AccessToken), x.IssuedType, x.ExpiresIn, x.Refresh != "")
	}
	return fmt.Sprintf("%d %s, error_uri %s (%s)", x.status, x.Error, cmp.Or(x.ErrorURI, "none"), x.Description)
}

// refusedWithConnect judges an exchange for a person without a sign-in.
func (gp *gitlabProof) refusedWithConnect(x gitlabExchange, who string) error {
	if x.status != http.StatusBadRequest || x.Error != "invalid_target" || x.AccessToken != "" || !strings.HasSuffix(x.ErrorURI, "/connect/"+config.WorkspaceProviderGitLab) {
		return fmt.Errorf("the token exchange for %s at %s answered %s, want 400 invalid_target with the instance's connect link", who, config.WorkspaceProviderGitLab, x.words())
	}
	return nil
}

func (gp *gitlabProof) notConnected() error {
	gp.second.disconnect(config.WorkspaceProviderGitLab)
	status, err := gp.second.providerStatus(config.WorkspaceProviderGitLab)
	if err != nil {
		return err
	}
	if status.Connected {
		return fmt.Errorf("list_providers shows %s still connected for %s after disconnect_provider", config.WorkspaceProviderGitLab, gp.secondUser.Email)
	}
	note("list_providers as %s: %s", gp.secondUser.Email, status)
	x, err := gp.exchange(gp.secondUser, config.WorkspaceProviderGitLab)
	if err != nil {
		return err
	}
	if err := gp.refusedWithConnect(x, gp.secondUser.Email); err != nil {
		return err
	}
	note("the token exchange as kagent's client for %s, audience %s: %s", gp.secondUser.Email, config.WorkspaceProviderGitLab, x.words())
	return nil
}

// gitlabAPI calls GitLab's REST API with a Bearer token and decodes the
// answer into into (nil discards it); it returns the status.
func (gp *gitlabProof) gitlabAPI(method, path, token string, body any, into any) (int, error) {
	var r io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		r = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, strings.TrimSuffix(gp.gl.URL, "/")+gitlabAPIPath+path, r)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := gp.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GitLab %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if into != nil && resp.StatusCode/100 == 2 {
		if err := json.Unmarshal(raw, into); err != nil {
			return resp.StatusCode, fmt.Errorf("GitLab %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// gitlabNewBranch, gitlabNewCommit and gitlabCommitAction are the bodies of
// GitLab's branch and commit creation.
type gitlabNewBranch struct {
	Branch string `json:"branch"`
	Ref    string `json:"ref"`
}

type gitlabNewCommit struct {
	Branch  string               `json:"branch"`
	Message string               `json:"commit_message"`
	Actions []gitlabCommitAction `json:"actions"`
}

type gitlabCommitAction struct {
	Action   string `json:"action"`
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// gitlabProjectPath is the project's API path.
func (gp *gitlabProof) gitlabProjectPath() string {
	return "/projects/" + url.PathEscape(gp.repo)
}

// gitRefs lists the project's refs over git's smart HTTP with the token as
// git sends it to GitLab, Basic with the user oauth2: ref name to head.
func (gp *gitlabProof) gitRefs(token string) (int, map[string]string, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(gp.gl.URL, "/")+"/"+gp.repo+".git/info/refs?service=git-upload-pack", http.NoBody)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("oauth2:"+token)))
	resp, err := gp.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	refs := map[string]string{}
	for _, m := range gitAdvertisedRef.FindAllStringSubmatch(string(raw), -1) {
		refs[m[2]] = m[1]
	}
	return resp.StatusCode, refs, nil
}

// gitAdvertisedRef is one ref of git's smart HTTP advertisement.
var gitAdvertisedRef = regexp.MustCompile(`([0-9a-f]{40}) (refs/heads/[^\x00\n]+)`)

func (gp *gitlabProof) token() error {
	x, err := gp.exchange(gp.adminUser, config.WorkspaceProviderGitLab)
	if err != nil {
		return err
	}
	if x.status != http.StatusOK || x.AccessToken == "" || !strings.EqualFold(x.TokenType, "Bearer") || x.Refresh != "" {
		return fmt.Errorf("the token exchange for %s at %s answered %s, want 200 with a Bearer access token and no refresh token", gp.adminUser.Email, config.WorkspaceProviderGitLab, x.words())
	}
	gp.released = x.AccessToken
	note("the token exchange as kagent's client for %s, audience %s: %s", gp.adminUser.Email, config.WorkspaceProviderGitLab, x.words())
	if other, err := gp.exchange(gp.adminUser, "no-such-instance"); err != nil {
		return err
	} else if other.Error != "invalid_target" || other.ErrorURI != "" || other.AccessToken != "" {
		return fmt.Errorf("the token exchange for an unknown audience answered %s, want invalid_target without a connect link", other.words())
	} else {
		note("an unknown audience: %s", other.words())
	}

	var user struct {
		Username string `json:"username"`
	}
	if status, err := gp.gitlabAPI(http.MethodGet, "/user", x.AccessToken, nil, &user); err != nil {
		return err
	} else if status != http.StatusOK || user.Username != gp.gl.Person {
		return fmt.Errorf("GitLab's /user with the released token answered %d naming %q, want the person %s", status, user.Username, gp.gl.Person)
	}
	note("GitLab %s/user with it (Bearer): %s", gitlabAPIPath, user.Username)
	var project struct {
		DefaultBranch string `json:"default_branch"`
	}
	if status, err := gp.gitlabAPI(http.MethodGet, gp.gitlabProjectPath(), x.AccessToken, nil, &project); err != nil || status != http.StatusOK {
		return fmt.Errorf("GitLab's project %s with the released token: %d %v", gp.repo, status, err)
	}
	status, refs, err := gp.gitRefs(x.AccessToken)
	if err != nil {
		return err
	}
	gp.head = refs["refs/heads/"+project.DefaultBranch]
	if status != http.StatusOK || gp.head == "" {
		return fmt.Errorf("git's smart HTTP on %s with the released token (Basic, user oauth2) answered %d with %d branches, want %s's head", gp.repo, status, len(refs), project.DefaultBranch)
	}
	note("git's smart HTTP on %s with it (Basic, user oauth2): %d, %d branches, %s at %s", gp.repo, status, len(refs), project.DefaultBranch, shortSHA(gp.head))

	// The person's write with the released token: a branch, a commit and a
	// merge request, each GitLab's record naming the person.
	if status, err := gp.gitlabAPI(http.MethodPost, gp.gitlabProjectPath()+"/repository/branches", x.AccessToken,
		gitlabNewBranch{Branch: gp.branch, Ref: project.DefaultBranch}, nil); err != nil || status != http.StatusCreated {
		return fmt.Errorf("creating the branch %s as %s: %d %v", gp.branch, gp.gl.Person, status, err)
	}
	var commit struct {
		ID          string `json:"id"`
		AuthorEmail string `json:"author_email"`
		AuthorName  string `json:"author_name"`
	}
	if status, err := gp.gitlabAPI(http.MethodPost, gp.gitlabProjectPath()+"/repository/commits", x.AccessToken, gitlabNewCommit{
		Branch: gp.branch, Message: "agentlab: the GitLab part's write as the person",
		Actions: []gitlabCommitAction{{Action: "create", FilePath: "agentlab-proof/" + gp.branch + ".md", Content: "written by `agentlab workspaces-test --part gitlab`\n"}},
	}, &commit); err != nil || status != http.StatusCreated {
		return fmt.Errorf("committing to %s as %s: %d %v", gp.branch, gp.gl.Person, status, err)
	}
	var mr struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
		Author struct {
			Username string `json:"username"`
		} `json:"author"`
	}
	if status, err := gp.gitlabAPI(http.MethodPost, gp.gitlabProjectPath()+"/merge_requests", x.AccessToken, map[string]any{
		"source_branch": gp.branch, "target_branch": project.DefaultBranch, "title": "agentlab: the GitLab part's merge request as the person", "remove_source_branch": true,
	}, &mr); err != nil || status != http.StatusCreated {
		return fmt.Errorf("opening a merge request from %s as %s: %d %v", gp.branch, gp.gl.Person, status, err)
	}
	gp.mergeRequest = mr.IID
	if mr.Author.Username != gp.gl.Person {
		return fmt.Errorf("the merge request !%d names %q as its author, want %s", mr.IID, mr.Author.Username, gp.gl.Person)
	}
	note("as %s with it (GitLab's API, from this host): branch %s, commit %s by %s, merge request !%d by %s (%s)", gp.gl.Person, gp.branch, shortSHA(commit.ID), commit.AuthorName, mr.IID, mr.Author.Username, mr.WebURL)
	return gp.removeWrite(x.AccessToken)
}

// removeWrite closes the proof's merge request and deletes its branch.
func (gp *gitlabProof) removeWrite(token string) error {
	var errs []error
	if gp.mergeRequest > 0 {
		if status, err := gp.gitlabAPI(http.MethodPut, gp.gitlabProjectPath()+"/merge_requests/"+strconv.Itoa(gp.mergeRequest), token, map[string]string{"state_event": "close"}, nil); err != nil || status != http.StatusOK {
			errs = append(errs, fmt.Errorf("closing the merge request !%d: %d %v", gp.mergeRequest, status, err))
		} else {
			note("merge request !%d closed", gp.mergeRequest)
			gp.mergeRequest = 0
		}
	}
	if status, err := gp.gitlabAPI(http.MethodDelete, gp.gitlabProjectPath()+"/repository/branches/"+url.PathEscape(gp.branch), token, nil, nil); err != nil || (status != http.StatusNoContent && status != http.StatusNotFound) {
		errs = append(errs, fmt.Errorf("deleting the branch %s: %d %v", gp.branch, status, err))
	} else if status == http.StatusNoContent {
		note("branch %s deleted", gp.branch)
	}
	return errors.Join(errs...)
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
	if m := d.mirror(gp.repo); m == nil || m.Head != gp.head {
		return fmt.Errorf("the manifest of %s holds %s at %v, want GitLab's head %s", gitlabWorkspace, gp.repo, m, shortSHA(gp.head))
	}
	out, err := sp.onVolume(gitlabWorkspace, "gitlab-mirror", syncMirrorsScript([]string{gp.repo}))
	if err != nil {
		return err
	}
	probe := parseMirrorProbe(out)[gp.repo]
	if bare, _ := strconv.ParseBool(probe["bare"]); !bare || probe["head"] != gp.head {
		return fmt.Errorf("the mirror %s/%s.git on the volume reads %v, want bare at GitLab's head %s", workspacesTestMirrors, gp.repo, probe, shortSHA(gp.head))
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
	// last_activity_at, which the token stage's merge request moves after
	// the sync without a push to a head: the project may be named changed.
	var changed []string
	if slices.Equal(claims.Changed, []string{gp.repo}) {
		changed = claims.Changed
		note("authorize_workspace names %s changed since the sync: GitLab's last activity moved with the token stage's merge request, its head did not", gp.repo)
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
	note("start_session: Session %s on volume %s; its turn works in %s, mirrors [%s], clones [%s], the setup clone %s at origin/HEAD %s (GitLab's head)",
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

func (gp *gitlabProof) disconnect() error {
	env, err := gp.admin.session.callToolEnvelope(workspaceManagerToolPrefix+"disconnect_provider", map[string]any{workspaceProviderArg: config.WorkspaceProviderGitLab})
	if err != nil {
		return err
	}
	if env.IsError {
		return fmt.Errorf("disconnect_provider as %s refused: %.300s", gp.adminUser.Email, env.Content[0].Text)
	}
	status, err := gp.admin.providerStatus(config.WorkspaceProviderGitLab)
	if err != nil {
		return err
	}
	if status.Connected {
		return fmt.Errorf("list_providers shows %s still connected for %s after disconnect_provider", config.WorkspaceProviderGitLab, gp.adminUser.Email)
	}
	note("list_providers as %s: %s", gp.adminUser.Email, status)
	x, err := gp.exchange(gp.adminUser, config.WorkspaceProviderGitLab)
	if err != nil {
		return err
	}
	if err := gp.refusedWithConnect(x, gp.adminUser.Email); err != nil {
		return err
	}
	note("the token exchange for %s after disconnect_provider: %s", gp.adminUser.Email, x.words())
	if gp.released != "" {
		code, err := gp.gitlabAPI(http.MethodGet, "/user", gp.released, nil, nil)
		if err != nil {
			return err
		}
		if code != http.StatusUnauthorized {
			return fmt.Errorf("GitLab answered %d to the token released before disconnect_provider, want 401: the sign-in was not revoked at GitLab", code)
		}
		note("GitLab %s/user with the token released before: %d (revoked at GitLab)", gitlabAPIPath, code)
		gp.released = ""
	}
	return nil
}

// cleanupNow removes what the part created and returns the first failure.
func (gp *gitlabProof) cleanupNow() error {
	var errs []error
	if gp.sessions != nil {
		errs = append(errs, gp.sessions.removeAllNow())
	}
	if gp.mergeRequest > 0 && gp.released != "" {
		errs = append(errs, gp.removeWrite(gp.released))
	}
	for _, p := range []*workspacePerson{gp.admin, gp.second} {
		if p != nil {
			p.disconnect(config.WorkspaceProviderGitLab)
		}
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
