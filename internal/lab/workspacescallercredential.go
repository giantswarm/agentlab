package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// `agentlab workspaces-test --part caller-credential`: a Session's caller
// credential on its Bearer provider's API host, end to end. On the provider
// instance fake-bearer (the lab's GitHub shaped like github.com: the git host
// github-bearer.<domain>, Basic; the REST API at api.github-bearer.<domain>,
// Bearer), as the admin:
//
//   - the admin connected to fake-bearer, a workspace of it with one
//     repository and its first cycle;
//   - a Session started through agent-manager's start_session with the
//     admin's grant, which names the API host as a Bearer provider of
//     audience fake-bearer (the providers kagent records on the Session are
//     noted);
//   - from the sandbox, a request to the API host with a placeholder
//     Authorization: the lab's GitHub logs a JWT bearer whose aud is the
//     provider's audience and whose sub names the Session's Actor, injected
//     by Substrate's egress gateway (the Session Actor's own egress policy,
//     which kagent compiles from the grant); no JWT in the sandbox's
//     environment, /data or /workspace;
//   - a Session of the same Agent without a workspace sends the same request:
//     no JWT reaches the lab's GitHub.
//
// The lab's GitHub verifies no JWT it did not sign: it records the bearer's
// claims and answers GitHub's 401, so the proof reads its request log, never
// an answer.

// WorkspacesPartCallerCredential is the caller-credential part of
// workspaces-test.
const WorkspacesPartCallerCredential = "caller-credential" // #nosec G101 -- a part's name, not a credential

const (
	// callerCredentialWorkspace is the part's workspace on fake-bearer.
	callerCredentialWorkspace = "agentlab-caller-credential"
	// callerCredentialAgent is the part's Agent on the claude Harness.
	callerCredentialAgent = "agentlab-workspaces-bearer"
	// callerCredentialPath is where the sandbox's request goes on the API
	// host, followed by a token only that Session knows: a path the log
	// tells apart and no API of GitHub's.
	callerCredentialPath = "/agentlab/caller-credential/"
	// callerCredentialPlaceholder is the Authorization the sandbox sends,
	// for the egress gateway to replace.
	callerCredentialPlaceholder = "Bearer agentlab-placeholder"
	// providerSchemeBearer is a grant provider's scheme for an API host.
	providerSchemeBearer = "Bearer"
)

// callerCredentialRepo is the one repository of the part's workspace.
var callerCredentialRepo = syncMovingRepo

// callerCredentialProof is the part's state.
type callerCredentialProof struct {
	sp      *syncProof
	timeout time.Duration
	admin   *workspacePerson
	api     *kagentAPI
	set     *sessionSet
	apiHost string
	// connected says the admin connected to fake-bearer, which the cleanup
	// disconnects.
	connected bool
	// with is the Session in the workspace, without the one without a
	// workspace; jwt is the request the API host logged from with.
	with, without string
	jwt           githubRequest
}

// workspacesCallerCredentialPart runs the part in stages, as the admin.
func workspacesCallerCredentialPart(cfg *config.Config, timeout time.Duration) error {
	p := newProofStages()
	cp := &callerCredentialProof{sp: newSyncProof(cfg, timeout, callerCredentialWorkspace), timeout: timeout,
		apiHost: config.GitHubFakeBearerAPIHost(cfg.Platform.Domain)}
	err := cp.run(p)
	cp.cleanup()
	if err == nil {
		fmt.Printf("\nWorkspaces caller-credential part passed: a Session started in workspace %s on the provider instance %s sent a request from its sandbox to the Bearer API host %s, and the lab's GitHub received a JWT bearer with aud %s and sub %s in place of the sandbox's placeholder; no JWT in the sandbox's environment, /data or /workspace; a Session of the same Agent without a workspace got none on that host.\n",
			callerCredentialWorkspace, config.WorkspaceProviderFakeBearer, cp.apiHost, strings.Join(cp.jwt.Audience, ","), cp.jwt.Subject)
	}
	return p.close(err)
}

func (cp *callerCredentialProof) run(p *proofStages) error {
	cfg := cp.sp.cfg
	adminUser := cfg.AdminUser()
	if adminUser == nil {
		return fmt.Errorf("the caller-credential part needs an admin user in %s (`agentlab configure --defaults` writes one)", config.File)
	}
	p.begin("caller-credential: preconditions", "The part's preconditions: the provider instance %s, the Session admission, the Agent %s on the claude Harness, %s signed in", config.WorkspaceProviderFakeBearer, callerCredentialAgent, adminUser.Email)
	if !cfg.WorkspaceFakeBearerProvider() {
		return fmt.Errorf("the caller-credential part proves a Bearer provider, the instance %s, and platform.workspaces.provider %q does not configure it: `agentlab configure --workspaces-provider %s,%s`, then `agentlab platform`",
			config.WorkspaceProviderFakeBearer, cfg.Platform.Workspaces.Provider, config.WorkspaceProviderFake, config.WorkspaceProviderFakeBearer)
	}
	if err := cp.sp.preconditions(adminUser.Email); err != nil {
		return err
	}
	if err := cp.preconditions(adminUser); err != nil {
		return err
	}

	p.begin("caller-credential: workspace", "%s connected to %s, create_workspace %s with %s, its first cycle", adminUser.Email, config.WorkspaceProviderFakeBearer, callerCredentialWorkspace, callerCredentialRepo)
	if err := cp.createWorkspace(); err != nil {
		return err
	}

	p.begin("caller-credential: session", "A Session started through start_session with the admin's grant, which names the API host %s as a %s provider of audience %s", cp.apiHost, providerSchemeBearer, config.WorkspaceProviderFakeBearer)
	session, err := cp.startSession(true)
	if err != nil {
		return err
	}
	recorded := toGrantProviders(session.GetProviders())
	if judgeGrantProviders(recorded, cp.apiHost, config.WorkspaceProviderFakeBearer) == nil {
		note("kagent records the providers %s on %s", grantProvidersLine(recorded), session.GetId())
	} else {
		note("kagent records the providers %s on %s, not the grant's Bearer API host: it compiles no caller credential for it", grantProvidersLine(recorded), session.GetId())
	}

	p.begin("caller-credential: the Actor's JWT", "From the sandbox, a request to %s with a placeholder Authorization: the lab's GitHub receives the Session Actor's JWT for %s; no JWT in the sandbox", cp.apiHost, config.WorkspaceProviderFakeBearer)
	if err := cp.probe(session, true); err != nil {
		return err
	}

	p.begin("caller-credential: no workspace", "A Session of the same Agent without a workspace sends the same request: no JWT reaches the lab's GitHub")
	bare, err := cp.startSession(false)
	if err != nil {
		return err
	}
	return cp.probe(bare, false)
}

// preconditions checks the Session admission, signs the admin in and
// readies the part's Agent, which lists both of fake-bearer's hosts as its
// egress.
func (cp *callerCredentialProof) preconditions(adminUser *config.User) error {
	cfg := cp.sp.cfg
	ctx := context.Background()
	rig := workspaceAdmissionValuesFor(cfg, workspacesChartCarries(cfg))
	if rig == nil {
		return fmt.Errorf("no Session admission on this lab (the chart takes no workspaces values, or platform.agents is off): the caller-credential part starts a Session on a workspace volume")
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
	if cp.admin, err = openWorkspacePerson(cfg, adminUser); err != nil {
		return err
	}
	if _, err := cp.admin.providerStatus(config.WorkspaceProviderFakeBearer); err != nil {
		return fmt.Errorf("the workspace-manager has no provider instance %s: %w (`agentlab platform` renders it)", config.WorkspaceProviderFakeBearer, err)
	}
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, adminUser.Email, adminUser.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	if cp.api, err = dialKagentAPI(cfg, token); err != nil {
		return err
	}
	cp.set = &sessionSet{api: cp.api}
	if err := waitHarnessReady(ctx, claudeHarness, cp.timeout); err != nil {
		return err
	}
	if agentExists(callerCredentialAgent) {
		if err := removeAgent(callerCredentialAgent); err != nil {
			return err
		}
	}
	manifest := workspacesAgentManifest(callerCredentialAgent, claudeHarness) +
		fmt.Sprintf("  egress:\n    - %q\n    - %q\n", cfg.GitHubFakeBearerURL(), cfg.GitHubFakeBearerAPIURL())
	if _, err := applyManifests(ctx, []byte(manifest)); err != nil {
		return fmt.Errorf("applying the Agent %s: %w", callerCredentialAgent, err)
	}
	boot, err := waitAgentReadyOn(callerCredentialAgent, claudeHarness, cp.timeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return fmt.Errorf("the Agent %s is not Ready on Harness %s after %s: %s", callerCredentialAgent, claudeHarness, boot.elapsed.Round(time.Second), boot.reason)
	}
	note("the Agent %s Ready on Harness %s in %s, its egress %s and %s", callerCredentialAgent, claudeHarness, boot.elapsed.Round(time.Second), cfg.GitHubFakeBearerURL(), cfg.GitHubFakeBearerAPIURL())
	return nil
}

// createWorkspace connects the admin to fake-bearer and defines the part's
// workspace with the one repository, waiting for its first cycle.
func (cp *callerCredentialProof) createWorkspace() error {
	s, cfg := cp.sp, cp.sp.cfg
	hops, err := connectWorkspaceProvider(cfg, cp.admin, config.WorkspaceProviderFakeBearer)
	if err != nil {
		return err
	}
	cp.connected = true
	note("%s connected to %s: %s", cp.admin.user.Email, config.WorkspaceProviderFakeBearer, hops)
	owner, name, _ := strings.Cut(callerCredentialRepo, "/")
	var created syncWorkspace
	if err := s.member.callServerStructured(workspaceManagerToolPrefix+"create_workspace", map[string]any{
		nameKey: callerCredentialWorkspace, wmOrganizationArg: config.WorkspaceOrganization,
		wmSourcesArg: []map[string]any{{wmProviderArg: config.WorkspaceProviderFakeBearer, wmOwnerArg: owner, repositoriesKey: []string{name}}},
	}, &created); err != nil {
		return err
	}
	d, err := s.waitCycle(callerCredentialWorkspace, nil)
	if err != nil {
		return err
	}
	if r := d.Workspace.LastSync; r.Result != syncResultSucceed {
		return fmt.Errorf("the first cycle of %s ended %s: %s", callerCredentialWorkspace, r.Result, r.Message)
	}
	note("created %s of Organization %s on %s; first cycle %s", callerCredentialWorkspace, created.Organization, config.WorkspaceProviderFakeBearer, d.Workspace.LastSync.Result)
	return nil
}

// startSession starts a Session of the part's Agent through start_session,
// with the admin's grant for the workspace or without a workspace, and
// returns it as kagent records it.
func (cp *callerCredentialProof) startSession(inWorkspace bool) (*apiv1alpha1.Session, error) {
	args := map[string]any{nameKey: callerCredentialAgent, namespaceKey: kagentNamespace, startSessionRequestIDArg: uuid.NewString()}
	if inWorkspace {
		a, err := cp.admin.authorizeWorkspace(callerCredentialWorkspace)
		if err != nil {
			return nil, err
		}
		if a.Outcome != authorizeGranted || a.Grant == "" {
			return nil, fmt.Errorf("authorize_workspace %s answered %s (%q, unreadable %v, connect %v), want granted", callerCredentialWorkspace, a.Outcome, a.Reason, a.Unreadable, a.Connect)
		}
		var claims struct {
			Providers []grantProvider `json:"providers"`
		}
		parts := strings.Split(a.Grant, ".")
		if len(parts) != 3 || decodeJWTPart(parts[1], &claims) != nil {
			return nil, fmt.Errorf("the grant of authorize_workspace %s is no JWT", callerCredentialWorkspace)
		}
		if err := judgeGrantProviders(claims.Providers, cp.apiHost, config.WorkspaceProviderFakeBearer); err != nil {
			return nil, fmt.Errorf("the grant for %s: %w", callerCredentialWorkspace, err)
		}
		note("the grant names the providers %s", grantProvidersLine(claims.Providers))
		args["workspaceGrant"] = a.Grant
	}
	env, err := cp.admin.session.callToolEnvelope(agentManagerToolPrefix+"start_session", args)
	if err != nil {
		return nil, err
	}
	if env.IsError {
		return nil, fmt.Errorf("start_session refused: %.400s", env.Content[0].Text)
	}
	var started accessStarted
	if err := json.Unmarshal([]byte(env.Content[0].Text), &started); err != nil || started.Session.ID == "" {
		return nil, fmt.Errorf("start_session answered no session: %.400s", env.Content[0].Text)
	}
	cp.set.add(started.Session.ID)
	ctx, cancel := context.WithTimeout(context.Background(), cp.timeout)
	defer cancel()
	session, err := cp.api.getSession(ctx, started.Session.ID)
	if err != nil {
		return nil, err
	}
	if !inWorkspace && session.GetVolumeSource() != nil {
		return nil, fmt.Errorf("the Session %s started without a workspace carries volume %q", session.GetId(), session.GetVolumeSource().GetVolume().GetVolumeHandle())
	}
	if inWorkspace {
		cp.with = session.GetId()
	} else {
		cp.without = session.GetId()
	}
	return session, nil
}

// grantProvider is one provider of a workspace grant's providers claim, as
// kagent records it on the Session.
type grantProvider struct {
	Hostname string `json:"hostname"`
	Audience string `json:"audience"`
	Scheme   string `json:"scheme"`
}

// toGrantProviders are the providers kagent recorded on a Session.
func toGrantProviders(providers []*apiv1alpha1.SessionProvider) []grantProvider {
	out := make([]grantProvider, 0, len(providers))
	for _, p := range providers {
		out = append(out, grantProvider{Hostname: p.GetHostname(), Audience: p.GetAudience(), Scheme: p.GetScheme()})
	}
	return out
}

// judgeGrantProviders is the verdict on a grant's providers: the API host
// among them as a Bearer provider of the audience.
func judgeGrantProviders(providers []grantProvider, apiHost, audience string) error {
	for _, p := range providers {
		if p.Hostname == apiHost {
			if !strings.EqualFold(p.Scheme, providerSchemeBearer) || p.Audience != audience {
				return fmt.Errorf("names %s as a %s provider of audience %q, want %s of audience %s", apiHost, p.Scheme, p.Audience, providerSchemeBearer, audience)
			}
			return nil
		}
	}
	return fmt.Errorf("names the providers %s, none on the API host %s: the workspace-manager names no Bearer API host for %s (one without apiURL, or the instance rendered without it)",
		grantProvidersLine(providers), apiHost, audience)
}

// grantProvidersLine words providers: host scheme audience each.
func grantProvidersLine(providers []grantProvider) string {
	if len(providers) == 0 {
		return "(none)"
	}
	words := make([]string, 0, len(providers))
	for _, p := range providers {
		words = append(words, fmt.Sprintf("%s %s aud=%s", p.Hostname, p.Scheme, p.Audience))
	}
	return strings.Join(words, ", ")
}

// callerCredentialValues is what the probe turn relayed: the API host's
// answer to the sandbox's request, and the environment variables and files
// holding a JWT (names only, never a value).
type callerCredentialValues struct {
	status string
	env    []string
	files  []string
}

// parseCallerCredentialTurn reads the marker lines of a reply.
func parseCallerCredentialTurn(reply string) (callerCredentialValues, error) {
	values := map[string]string{}
	for _, m := range probeLine.FindAllStringSubmatch(reply, -1) {
		values[m[1]] = strings.TrimSpace(strings.Trim(strings.TrimSpace(m[2]), "`"))
	}
	if _, ok := values["api-status"]; !ok {
		return callerCredentialValues{}, fmt.Errorf("the reply carries no %s api-status line (did the agent run its Bash tool?): %s", probeMarker, excerpt(reply, 300))
	}
	return callerCredentialValues{status: values["api-status"], env: strings.Fields(values["jwt-env"]), files: strings.Fields(values["jwt-files"])}, nil
}

// jwtPattern finds a JWT in the sandbox: three base64url parts, the first
// two JSON objects.
const jwtPattern = `eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+`

// callerCredentialCommand sends the request to the API host with the
// placeholder (curl, else wget) and names the environment variables and the
// files under /data, /workspace and the working directory holding a JWT.
func callerCredentialCommand(apiURL, token string) string {
	target := apiURL + callerCredentialPath + token
	return `echo "` + probeMarker + ` pwd=$(pwd)"; ` +
		`if command -v curl >/dev/null; then s=$(curl -sS -o /dev/null -w '%{http_code}' -H 'Authorization: ` + callerCredentialPlaceholder + `' '` + target + `' 2>&1 | tail -n 1); ` +
		`elif command -v wget >/dev/null; then s=$(wget -S -O /dev/null --header='Authorization: ` + callerCredentialPlaceholder + `' '` + target + `' 2>&1 | awk '/HTTP\//{c=$2} END{print c}'); ` +
		`else s=no-curl-or-wget; fi; echo "` + probeMarker + ` api-status=$(echo "$s" | tr ' ' '_')"; ` +
		`echo "` + probeMarker + ` jwt-env=$(env | grep -E '` + jwtPattern + `' | cut -d= -f1 | sort -u | tr '\n' ' ')"; ` +
		`echo "` + probeMarker + ` jwt-files=$(grep -rlE '` + jwtPattern + `' /data /workspace "$PWD" 2>/dev/null | sort -u | tr '\n' ' ')"`
}

// probe runs the request from the Session's sandbox and judges what the
// lab's GitHub logged for it: the Actor's JWT for a Session in the
// workspace, none for one without.
func (cp *callerCredentialProof) probe(session *apiv1alpha1.Session, inWorkspace bool) error {
	token := randomHex(8)
	ctx, cancel := context.WithTimeout(context.Background(), cp.timeout)
	defer cancel()
	var v callerCredentialValues
	if _, _, err := probeTurnWith(ctx, cp.api, session, callerCredentialCommand(cp.sp.cfg.GitHubFakeBearerAPIURL(), token), func(reply string) (err error) {
		v, err = parseCallerCredentialTurn(reply)
		return err
	}); err != nil {
		return err
	}
	reqs, err := cp.sp.gh.requests()
	if err != nil {
		return err
	}
	logged := requestsTo(reqs, cp.apiHost, callerCredentialPath+token)
	note("%s: the sandbox's request answered %s; the lab's GitHub logged %s", session.GetId(), v.status, requestsLine(logged))
	if !inWorkspace {
		return judgeNoCallerCredential(logged, session.GetId())
	}
	jwt, err := judgeCallerCredential(logged, v.status, config.WorkspaceProviderFakeBearer, session.GetId())
	if err != nil {
		return err
	}
	cp.jwt = jwt
	if len(v.env) > 0 || len(v.files) > 0 {
		return fmt.Errorf("a JWT in the sandbox of Session %s: environment variables %v, files %v; the Actor's JWT stays in the egress gateway", session.GetId(), v.env, v.files)
	}
	note("%s: the Actor's JWT on %s, aud %s, sub %s; no JWT in the sandbox's environment, /data or /workspace", session.GetId(), cp.apiHost, strings.Join(jwt.Audience, ","), jwt.Subject)
	return nil
}

// requestsTo are the logged requests to path on host.
func requestsTo(reqs []githubRequest, host, path string) []githubRequest {
	var out []githubRequest
	for _, r := range reqs {
		if r.Host == host && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// requestsLine words logged requests: the status and the credential kind,
// with a JWT's claims.
func requestsLine(reqs []githubRequest) string {
	if len(reqs) == 0 {
		return "no request"
	}
	words := make([]string, 0, len(reqs))
	for _, r := range reqs {
		w := fmt.Sprintf("%s %d credential=%s", r.Method, r.Status, r.Credential)
		if r.Credential == credJWT {
			w += fmt.Sprintf(" aud=%s sub=%s", strings.Join(r.Audience, ","), r.Subject)
		}
		words = append(words, w)
	}
	return strings.Join(words, "; ")
}

// actorSubject matches the sub of a Session Actor's JWT for the Session.
func actorSubject(session string) *regexp.Regexp {
	return regexp.MustCompile(`^atespaces:[^:]+:actors:` + regexp.QuoteMeta(substrateActorName(session)) + `$`)
}

// judgeCallerCredential is the verdict on what the API host logged from a
// Session in the workspace: a JWT bearer of the audience whose sub names the
// Session's Actor. status is what the sandbox's request answered.
func judgeCallerCredential(logged []githubRequest, status, audience, session string) (githubRequest, error) {
	if len(logged) == 0 {
		return githubRequest{}, fmt.Errorf("the sandbox's request answered %s and never reached the lab's GitHub: the egress gateway did not pass it to the API host (a 403 is the gateway's: the host is not in the Session Actor's egress)", status)
	}
	var kinds []string
	for _, r := range logged {
		if r.Credential != credJWT {
			kinds = append(kinds, r.Credential)
			continue
		}
		if !slices.Contains(r.Audience, audience) || !actorSubject(session).MatchString(r.Subject) {
			return r, fmt.Errorf("the API host received a JWT with aud %v and sub %q, want aud %s and the sub of the Session's Actor (atespaces:<atespace>:actors:%s)",
				r.Audience, r.Subject, audience, substrateActorName(session))
		}
		return r, nil
	}
	return githubRequest{}, fmt.Errorf("the API host received the credential %s, not the Session Actor's JWT: no caller credential reached the Bearer provider %s, the sandbox's placeholder went through unreplaced (this kagent compiles no actor-JWT credential header onto the Session's Actor for its Bearer providers)",
		strings.Join(slices.Compact(kinds), ", "), audience)
}

// judgeNoCallerCredential is the verdict on a Session without a workspace:
// no JWT on the API host, the request refused at the gateway or carrying
// the placeholder.
func judgeNoCallerCredential(logged []githubRequest, session string) error {
	for _, r := range logged {
		if r.Credential == credJWT {
			return fmt.Errorf("the Session %s without a workspace sent a JWT (aud %v, sub %q) to the API host: a Session without a grant must get no caller credential", session, r.Audience, r.Subject)
		}
	}
	note("%s, no workspace: no JWT reached the lab's GitHub", session)
	return nil
}

// cleanup removes the Sessions, the Agent, the admin's connection and the
// workspace.
func (cp *callerCredentialProof) cleanup() {
	if cp.sp.restored {
		return
	}
	var errs []error
	if cp.set != nil {
		errs = append(errs, cp.set.removeAllNow())
	}
	if cp.admin != nil && cp.connected {
		cp.admin.disconnect(config.WorkspaceProviderFakeBearer)
	}
	if agentExists(callerCredentialAgent) {
		errs = append(errs, removeAgent(callerCredentialAgent))
	}
	if cp.sp.member != nil {
		if gone, err := cp.sp.gone(callerCredentialWorkspace); err == nil && !gone {
			if _, err := cp.sp.volume(callerCredentialWorkspace); err == nil {
				out, err := cp.sp.onVolume(callerCredentialWorkspace, "sessions-clear", accessClearSessionsScript)
				errs = append(errs, err)
				if err == nil {
					note("%s", indent(out, "  "))
				}
			}
		}
	}
	errs = append(errs, cp.sp.restore())
	if cp.api != nil {
		cp.api.close()
	}
	if err := errors.Join(errs...); err != nil {
		warn("removing what the part left: %v", err)
	}
}
