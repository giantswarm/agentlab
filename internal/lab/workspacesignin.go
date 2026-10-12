package lab

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// A person's sign-in at a workspace-manager provider instance, walked
// headlessly the way a browser walks it, so a proof reaches what only a
// connected person sees (authorize_workspace's granted and refused):
//
//  1. connect_provider, called through muster as the person, answers with the
//     provider's authorization link; the lab's GitHub consents at once for
//     the fixture user its login parameter names (githubfake_workspaces.go),
//     so the walk adds the person's fixture login.
//  2. The provider redirects to the workspace-manager's /callback/<instance>
//     through the edge. The callback page completes only for a browser
//     signed in to the manager's pages, so it sends a new browser to Dex
//     first.
//  3. Dex's login form takes the person's lab credentials and returns to the
//     manager's /signin, which sets the page session and returns to the
//     callback, which completes the sign-in: list_providers then shows the
//     person connected.
//
// From this host every hop is a name the lab dials: the platform's names on
// the edge's loopback port (dialLab), Dex on its issuer URL, and the lab's
// GitHub, whose name pods resolve to its Service (coredns.yaml.tmpl), on the
// loopback port its container publishes: the edge has no route for it.
//
// A GitLab instance is a real GitLab: its link leads a new browser to
// GitLab's sign-in form, which takes the provider person's GitLab login and
// password (providerLogin), then to the OAuth application's consent form,
// whose Authorize the walk submits; GitLab then redirects to the callback as
// above.

// The workspace-manager tools' argument names the walk and the proof pass.
const (
	workspaceArg         = "workspace"
	workspaceProviderArg = "provider"
	workspaceOwnerArg    = "owner"
)

// workspaceSignInMaxHops bounds the walk: the provider (a GitLab's
// authorization, sign-in form, its post and consent included), the callback,
// Dex's authorize, its connector and login pages, the login's return,
// /signin and the callback again, with room for a Dex that adds a hop.
const workspaceSignInMaxHops = 24

// dexPasswordField is the password input of Dex's local login form, how the
// walk recognizes the form among the pages it is handed.
const dexPasswordField = `name="` + passwordParam + `"`

// dexLocalConnectorRe finds the local connector's link on Dex's connector
// chooser, which a Dex with more than one connector (the lab's github
// sign-in) shows instead of the login form.
var dexLocalConnectorRe = regexp.MustCompile(`href="([^"]*/auth/local[^"]*)"`)

// pageHeadingRe is the heading of the workspace-manager's pages, its verdict
// on a sign-in in a few words.
var pageHeadingRe = regexp.MustCompile(`(?s)<h1>(.*?)</h1>`)

// GitLab's pages the walk recognizes: the sign-in form (its password field
// and action) and the OAuth application's consent (the form posting to the
// authorization endpoint; its Deny is the same action with a hidden
// `_method=delete`).
const (
	gitlabPasswordField = `name="user[password]"`
	gitlabSignInAction  = "/users/sign_in"
	gitlabAuthorizePath = "/oauth/authorize"
	railsMethodField    = "_method"
)

var (
	htmlFormRe  = regexp.MustCompile(`(?is)<form\b([^>]*)>(.*?)</form>`)
	htmlInputRe = regexp.MustCompile(`(?is)<input\b([^>]*)>`)
	htmlAttrRe  = regexp.MustCompile(`(?is)([a-z_:-]+)\s*=\s*"([^"]*)"`)
)

// htmlForm is one form of a page: its action, method and the named inputs'
// values, unescaped.
type htmlForm struct {
	action, method string
	fields         url.Values
}

// htmlForms reads a page's forms, enough for the server-rendered sign-in
// and consent pages the walk submits.
func htmlForms(page []byte) []htmlForm {
	attrs := func(raw string) map[string]string {
		out := map[string]string{}
		for _, m := range htmlAttrRe.FindAllStringSubmatch(raw, -1) {
			out[strings.ToLower(m[1])] = html.UnescapeString(m[2])
		}
		return out
	}
	var forms []htmlForm
	for _, m := range htmlFormRe.FindAllSubmatch(page, -1) {
		a := attrs(string(m[1]))
		f := htmlForm{action: a["action"], method: strings.ToLower(cmp.Or(a["method"], http.MethodGet)), fields: url.Values{}}
		for _, in := range htmlInputRe.FindAllSubmatch(m[2], -1) {
			ia := attrs(string(in[1]))
			if ia["name"] != "" {
				f.fields.Set(ia["name"], ia["value"])
			}
		}
		forms = append(forms, f)
	}
	return forms
}

// providerLogin is the person's own login at a provider whose sign-in is a
// real login form (a GitLab), nil for the lab's GitHub, which consents for
// the login its link names.
type providerLogin struct {
	login, password string
}

// gitlabFormStep answers the walk's next request on a GitLab page: the
// sign-in form filled with the person's GitLab login, or the consent's
// Authorize. ok is false on any other page.
func gitlabFormStep(page []byte, login *providerLogin, posted bool) (action string, form url.Values, ok bool, err error) {
	for _, f := range htmlForms(page) {
		switch {
		case f.method == "post" && strings.HasSuffix(f.action, gitlabSignInAction) && strings.Contains(string(page), gitlabPasswordField):
			if login == nil {
				return "", nil, true, fmt.Errorf("GitLab asks for a sign-in, and the walk has no GitLab login for the person")
			}
			if posted {
				return "", nil, true, fmt.Errorf("GitLab answered the sign-in with its form again: it refused %s's credentials", login.login)
			}
			f.fields.Set("user[login]", login.login)
			f.fields.Set("user[password]", login.password)
			f.fields.Set("user[remember_me]", "0")
			return f.action, f.fields, true, nil
		case f.method == "post" && strings.HasSuffix(f.action, gitlabAuthorizePath) && f.fields.Get(railsMethodField) == "":
			return f.action, f.fields, true, nil
		}
	}
	return "", nil, false, nil
}

// signInHop is one request of the walk: the method, where it went without
// the query (a code and a state are nobody's to read in a log) and the
// answer's status.
type signInHop struct {
	Method string
	URL    string
	Status int
}

func (h signInHop) String() string { return fmt.Sprintf("%s %s %d", h.Method, h.URL, h.Status) }

// signInHops words the walk as one line, hop by hop.
func signInHops(hops []signInHop) string {
	words := make([]string, len(hops))
	for i, h := range hops {
		words[i] = h.String()
	}
	return strings.Join(words, " -> ")
}

// errSignInStopped is a walk that ended before the callback completed; its
// message names the hop and the status it stopped at.
var errSignInStopped = errors.New("the provider sign-in stopped")

// walkProviderSignIn walks a provider's authorization link to the
// workspace-manager's callback for instance, signing in to Dex with the
// person's credentials when the callback sends the browser there. client
// follows no redirect itself and keeps the cookies. It returns the hops;
// an error names the hop the walk stopped at, its status and the page's
// heading.
func walkProviderSignIn(client *http.Client, link, instance, email, password string, login *providerLogin) ([]signInHop, error) {
	var hops []signInHop
	stopped := func(h signInHop, why string) error {
		return fmt.Errorf("%w at %s: %s\n  hops: %s", errSignInStopped, h, why, signInHops(hops))
	}
	next, err := url.Parse(link)
	if err != nil {
		return nil, fmt.Errorf("the authorization link: %w", err)
	}
	callback := "/callback/" + url.PathEscape(instance)
	var form url.Values
	for len(hops) < workspaceSignInMaxHops {
		method := http.MethodGet
		var body io.Reader = http.NoBody
		if form != nil {
			method, body = http.MethodPost, strings.NewReader(form.Encode())
		}
		req, err := http.NewRequest(method, next.String(), body)
		if err != nil {
			return hops, err
		}
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		form = nil
		hop := signInHop{Method: method, URL: next.Scheme + "://" + next.Host + next.Path}
		resp, err := client.Do(req)
		if err != nil {
			hops = append(hops, hop)
			return hops, stopped(hop, err.Error())
		}
		page, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		hop.Status = resp.StatusCode
		hops = append(hops, hop)
		// A GitLab page answers with its sign-in or its consent form.
		var glAction string
		var glFields url.Values
		var glForm bool
		var glErr error
		if login != nil && resp.StatusCode == http.StatusOK && next.Path != callback {
			glAction, glFields, glForm, glErr = gitlabFormStep(page, login, method == http.MethodPost && strings.HasSuffix(next.Path, gitlabSignInAction))
		}
		switch {
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			location, err := next.Parse(resp.Header.Get("Location"))
			if err != nil || resp.Header.Get("Location") == "" {
				return hops, stopped(hop, "a redirect without a usable Location")
			}
			next = location
		case resp.StatusCode != http.StatusOK:
			return hops, stopped(hop, pageHeading(page))
		case next.Path == callback:
			return hops, nil
		case glErr != nil:
			return hops, stopped(hop, glErr.Error())
		case glForm:
			location, err := next.Parse(glAction)
			if err != nil {
				return hops, stopped(hop, "the form's action: "+err.Error())
			}
			next, form = location, glFields
		case strings.Contains(string(page), dexPasswordField) && method == http.MethodPost:
			return hops, stopped(hop, "Dex answered the login with its form again: it refused "+email+"'s credentials")
		case strings.Contains(string(page), dexPasswordField):
			form = url.Values{githubLogin: {email}, passwordParam: {password}}
		case dexLocalConnectorRe.Match(page):
			location, err := next.Parse(html.UnescapeString(string(dexLocalConnectorRe.FindSubmatch(page)[1])))
			if err != nil {
				return hops, stopped(hop, "the local connector's link: "+err.Error())
			}
			next = location
		default:
			return hops, stopped(hop, "a page that is neither the callback, Dex's login form nor its connector chooser: "+pageHeading(page))
		}
	}
	return hops, fmt.Errorf("%w after %d hops without reaching the callback %s\n  hops: %s", errSignInStopped, len(hops), callback, signInHops(hops))
}

// pageHeading is a page's heading, or its start when it has none.
func pageHeading(page []byte) string {
	if m := pageHeadingRe.FindSubmatch(page); m != nil {
		return strconv.Quote(html.UnescapeString(strings.TrimSpace(string(m[1]))))
	}
	return fmt.Sprintf("%.200q", strings.TrimSpace(string(page)))
}

// workspaceSignInClient is a browser for the walk: the lab's TLS trust, its
// own cookie jar, no redirect followed, and the lab's GitHub dialed on the
// loopback port its container publishes.
func workspaceSignInClient(cfg *config.Config) (*http.Client, error) {
	transport, err := labTLSTransport()
	if err != nil {
		return nil, err
	}
	github, err := labGitHubHostAddr(cfg)
	if err != nil {
		return nil, err
	}
	walk := transport.Clone()
	host := config.GitHubFakeHost(cfg.Platform.Domain)
	walk.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if h, _, err := net.SplitHostPort(addr); err == nil && h == host {
			addr = github
		}
		return dialLab(ctx, network, addr)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &http.Client{Jar: jar, Transport: walk, Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

// labGitHubHostAddr is the loopback address the lab's GitHub container
// publishes its port on.
func labGitHubHostAddr(cfg *config.Config) (string, error) {
	name := cfg.ClusterName + "-" + labGitHubContainerSuffix
	published, err := outputQuiet(dockerBin, "port", name, strconv.Itoa(fakeContainerPort)+"/tcp")
	if addr := publishedLoopback(published); err == nil && addr != "" {
		return addr, nil
	}
	return "", fmt.Errorf("the lab's GitHub, container %s, publishes no port on this host: `agentlab platform` starts it for the provider instance %s", name, config.WorkspaceProviderFake)
}

// workspacePerson is a lab user signed in to muster for the workspace-manager's
// tools, with the fixture login the lab's GitHub knows them by.
type workspacePerson struct {
	user    *config.User
	login   string
	session *musterSession
}

// openWorkspacePerson signs user in to muster (Dex password grant, client
// agent-platform) and finds their login in the lab GitHub's fixture.
func openWorkspacePerson(cfg *config.Config, user *config.User) (*workspacePerson, error) {
	fixture, err := loadGitHubFixture("")
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(fixture.Users, func(u githubFixtureUser) bool { return strings.EqualFold(u.Email, user.Email) })
	if i < 0 {
		return nil, fmt.Errorf("the lab's GitHub has no fixture user with the email %s: nobody to consent as", user.Email)
	}
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return nil, err
	}
	session, err := openMusterSession(cfg, token, "workspaces-signin-"+fixture.Users[i].Login)
	if err != nil {
		return nil, err
	}
	return &workspacePerson{user: user, login: fixture.Users[i].Login, session: session}, nil
}

// connectWorkspaceProvider connects the person to the provider instance the
// way a browser does: connect_provider's link walked to the callback, then
// list_providers checked to show them connected. It returns the walk's hops,
// worded; an error says where the walk stopped. The proofs that need a
// connected person call it (workspaces-signin-test, workspaces-test's
// access part).
func connectWorkspaceProvider(cfg *config.Config, p *workspacePerson, instance string) (string, error) {
	return connectWorkspaceProviderAs(cfg, p, instance, nil)
}

// connectWorkspaceProviderAs is connectWorkspaceProvider at a provider whose
// sign-in is a real login form: login is the person's account there (a
// GitLab's). Without one the walk adds the person's fixture login for the
// lab's GitHub.
func connectWorkspaceProviderAs(cfg *config.Config, p *workspacePerson, instance string, login *providerLogin) (string, error) {
	var started struct {
		AuthorizationURL string `json:"authorizationURL"`
	}
	if err := p.session.callServerStructured(workspaceManagerToolPrefix+"connect_provider", map[string]any{workspaceProviderArg: instance}, &started); err != nil {
		return "", err
	}
	link, err := url.Parse(started.AuthorizationURL)
	if err != nil || link.Host == "" {
		return "", fmt.Errorf("connect_provider for %s answered no authorization link: %q", instance, started.AuthorizationURL)
	}
	// The lab's GitHub consents for the fixture user the login parameter
	// names, as GitHub would for the browser's signed-in account.
	account := p.login
	if login == nil {
		q := link.Query()
		q.Set(githubLogin, p.login)
		link.RawQuery = q.Encode()
	} else {
		account = login.login
	}
	client, err := workspaceSignInClient(cfg)
	if err != nil {
		return "", err
	}
	hops, err := walkProviderSignIn(client, link.String(), instance, p.user.Email, p.user.Password, login)
	if err != nil {
		return "", fmt.Errorf("%s connecting %s as %s: %w", p.user.Email, instance, account, err)
	}
	status, err := p.providerStatus(instance)
	if err != nil {
		return "", err
	}
	if !status.Connected {
		return "", fmt.Errorf("the sign-in of %s at %s reached the callback, but list_providers still shows %s\n  hops: %s", p.user.Email, instance, status, signInHops(hops))
	}
	return signInHops(hops), nil
}

// providerStatus is list_providers' entry for the instance, as the person.
func (p *workspacePerson) providerStatus(instance string) (workspaceProviderStatus, error) {
	var listed struct {
		Providers []workspaceProviderStatus `json:"providers"`
	}
	if err := p.session.callServerStructured(workspaceManagerToolPrefix+"list_providers", nil, &listed); err != nil {
		return workspaceProviderStatus{}, err
	}
	i := slices.IndexFunc(listed.Providers, func(s workspaceProviderStatus) bool { return s.Name == instance })
	if i < 0 {
		return workspaceProviderStatus{}, fmt.Errorf("list_providers does not list %s for %s", instance, p.user.Email)
	}
	return listed.Providers[i], nil
}

// disconnect forgets the person's sign-in at the instance, best effort: a
// later proof starts from a person who is not connected.
func (p *workspacePerson) disconnect(instance string) {
	if _, err := p.session.callToolEnvelope(workspaceManagerToolPrefix+"disconnect_provider", map[string]any{workspaceProviderArg: instance}); err != nil {
		warn("disconnecting %s from %s: %v", p.user.Email, instance, err)
	}
}

// workspaceAuthorization is authorize_workspace's answer, the fields a proof
// judges.
type workspaceAuthorization struct {
	Outcome      string     `json:"outcome"`
	Reason       string     `json:"reason"`
	Grant        string     `json:"grant"`
	ExpiresAt    *time.Time `json:"expiresAt"`
	Repositories []string   `json:"repositories"`
	Changed      []string   `json:"changed"`
	Unreadable   []string   `json:"unreadable"`
	// Connect are the providers the person signs in to first, each with the
	// link that starts it.
	Connect []struct {
		Provider string `json:"provider"`
		URL      string `json:"url"`
	} `json:"connect"`
}

// authorizeWorkspace calls authorize_workspace on the workspace as the person.
func (p *workspacePerson) authorizeWorkspace(workspace string) (workspaceAuthorization, error) {
	var a workspaceAuthorization
	err := p.session.callServerStructured(workspaceManagerToolPrefix+"authorize_workspace", map[string]any{workspaceArg: workspace}, &a)
	return a, err
}

// The workspace the sign-in proof creates: the lab GitHub's agentlab-org
// repositories one fixture member reads both of and the other only one.
const (
	workspaceSignInProofName = "agentlab-signin-proof"
	workspaceSignInOwner     = "agentlab-org"
	workspaceSignInReadable  = "platform-api"
	// workspaceSignInPrivate is the repository only the admin reads
	// (github-fixture.yaml).
	workspaceSignInPrivate = "platform-infra"
)

// WorkspacesSignInTestOptions tune the sign-in proof.
type WorkspacesSignInTestOptions struct {
	// Provider is the provider instance the people connect to.
	Provider string
	// SyncTimeout bounds the wait for the workspace's first sync.
	SyncTimeout time.Duration
}

// DefaultWorkspacesSignInSyncTimeout is the proof's default wait for a sync.
const DefaultWorkspacesSignInSyncTimeout = 5 * time.Minute

// WorkspacesSignInTest is the headless provider sign-in proof: a workspace
// over agentlab-org/platform-api and platform-infra created and synced as the
// admin -> the admin and a developer connected to the provider instance by
// the browser's walk (connectWorkspaceProvider), list_providers showing each
// connected -> authorize_workspace granted for the admin, who reads both,
// and refused naming platform-infra for the developer, who does not ->
// the workspace deleted and both sign-ins forgotten.
func WorkspacesSignInTest(cfg *config.Config, opts WorkspacesSignInTestOptions) error {
	if skip := workspaceManagerSkip(cfg); skip != "" {
		return fmt.Errorf("no workspace-manager to sign in to: %s", skip)
	}
	instance := cmp.Or(opts.Provider, config.WorkspaceProviderFake)
	if instance != config.WorkspaceProviderFake || !cfg.WorkspaceFakeProvider() {
		return fmt.Errorf("the sign-in proof walks the lab's GitHub, the provider instance %s: platform.workspaces.provider in %s must name it", config.WorkspaceProviderFake, config.File)
	}
	if opts.SyncTimeout <= 0 {
		opts.SyncTimeout = DefaultWorkspacesSignInSyncTimeout
	}
	adminUser, memberUser := cfg.FindUserInGroup("platform-admins"), cfg.FindUser("dev@lab.local")
	if adminUser == nil || memberUser == nil {
		return fmt.Errorf("the sign-in proof needs a platform-admins user and dev@lab.local in %s (`agentlab configure --defaults` writes them)", config.File)
	}
	admin, err := openWorkspacePerson(cfg, adminUser)
	if err != nil {
		return err
	}
	member, err := openWorkspacePerson(cfg, memberUser)
	if err != nil {
		return err
	}

	step("A workspace %s of Organization %s over %s/%s and %s/%s, created as %s", workspaceSignInProofName, config.WorkspaceOrganization,
		workspaceSignInOwner, workspaceSignInReadable, workspaceSignInOwner, workspaceSignInPrivate, admin.user.Email)
	deleteWorkspace := func() {
		_, _ = admin.session.callToolEnvelope(workspaceManagerToolPrefix+"delete_workspace", map[string]any{workspaceArg: workspaceSignInProofName})
	}
	deleteWorkspace()
	if err := waitWorkspaceGone(admin, opts.SyncTimeout); err != nil {
		return err
	}
	defer deleteWorkspace()
	create := map[string]any{
		nameKey:        workspaceSignInProofName,
		"organization": config.WorkspaceOrganization,
		"sources": []map[string]any{{
			"provider":        instance,
			workspaceOwnerArg: workspaceSignInOwner,
			repositoriesKey:   []string{workspaceSignInReadable, workspaceSignInPrivate},
		}},
	}
	if env, err := admin.session.callToolEnvelope(workspaceManagerToolPrefix+"create_workspace", create); err != nil {
		return err
	} else if env.IsError {
		return fmt.Errorf("create_workspace refused: %.300s", env.Content[0].Text)
	}
	synced, err := waitWorkspaceSynced(admin, opts.SyncTimeout)
	if err != nil {
		return err
	}
	note("synced: %s", synced)

	for _, p := range []*workspacePerson{admin, member} {
		step("%s connects %s as fixture user %s: the authorization link walked through the callback and Dex", p.user.Email, instance, p.login)
		defer p.disconnect(instance)
		hops, err := connectWorkspaceProvider(cfg, p, instance)
		if err != nil {
			return err
		}
		note("%s", hops)
		note("list_providers as %s: %s connected", p.user.Email, instance)
	}

	step("authorize_workspace %s as %s (reads both) and as %s (not %s)", workspaceSignInProofName, admin.user.Email, member.user.Email, workspaceSignInPrivate)
	granted, err := admin.authorizeWorkspace(workspaceSignInProofName)
	if err != nil {
		return err
	}
	if granted.Outcome != "granted" || granted.Grant == "" {
		return fmt.Errorf("authorize_workspace as %s answered %s (%s), want granted with a grant", admin.user.Email, granted.Outcome, granted.Reason)
	}
	note("%s: granted, a grant of %d bytes", admin.user.Email, len(granted.Grant))
	refused, err := member.authorizeWorkspace(workspaceSignInProofName)
	if err != nil {
		return err
	}
	private := workspaceSignInOwner + "/" + workspaceSignInPrivate
	if refused.Outcome != "refused" || !slices.ContainsFunc(refused.Unreadable, func(r string) bool { return strings.HasSuffix(r, private) }) {
		return fmt.Errorf("authorize_workspace as %s answered %s (%s, unreadable %v), want refused naming %s", member.user.Email, refused.Outcome, refused.Reason, refused.Unreadable, private)
	}
	note("%s: refused, unreadable %s (%s)", member.user.Email, strings.Join(refused.Unreadable, ", "), refused.Reason)

	fmt.Printf("\nWorkspace sign-in proof passed: %s and %s connected to %s headlessly, authorize_workspace granted for the one and refused naming %s for the other.\n",
		admin.user.Email, member.user.Email, instance, private)
	return nil
}

// workspaceView is get_workspace's answer (the workspace-manager's
// WorkspaceDetail), the fields the proof waits on.
type workspaceView struct {
	Workspace struct {
		LastSync *struct {
			Time    time.Time `json:"time"`
			Result  string    `json:"result"`
			Message string    `json:"message"`
		} `json:"lastSync"`
	} `json:"workspace"`
}

// waitWorkspaceSynced waits for the proof workspace's first sync and words it;
// a sync that ended other than succeeded fails the wait.
func waitWorkspaceSynced(p *workspacePerson, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		var w workspaceView
		if err := p.session.callServerStructured(workspaceManagerToolPrefix+"get_workspace", map[string]any{workspaceArg: workspaceSignInProofName}, &w); err != nil {
			return "", err
		}
		if s := w.Workspace.LastSync; s != nil {
			if !strings.EqualFold(s.Result, "succeeded") {
				return "", fmt.Errorf("workspace %s's sync ended %s: %s", workspaceSignInProofName, s.Result, s.Message)
			}
			return fmt.Sprintf("%s at %s", s.Result, s.Time.UTC().Format(time.RFC3339)), nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("workspace %s did not sync within %s: check `kubectl -n %s logs deploy/%s`", workspaceSignInProofName, timeout, platformNamespace, workspaceManagerMCPServer)
		}
		time.Sleep(5 * time.Second)
	}
}

// waitWorkspaceGone waits until the proof workspace an earlier run left is
// gone, so the create starts afresh.
func waitWorkspaceGone(p *workspacePerson, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		env, err := p.session.callToolEnvelope(workspaceManagerToolPrefix+"get_workspace", map[string]any{workspaceArg: workspaceSignInProofName})
		if err != nil {
			return err
		}
		if env.IsError && strings.Contains(env.Content[0].Text, "not found") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("workspace %s of an earlier run is not gone within %s", workspaceSignInProofName, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}
