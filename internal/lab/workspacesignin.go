package lab

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// A person's grant of the provider App muster pins the workspace-manager's
// registration to (workspacemustergrant.go), or, with no pin, of one
// instance's provider App muster keeps beside the others, walked headlessly
// the way a browser walks it, so a proof reaches what only a person who
// granted it sees (the manager's tools, authorize_workspace's granted and
// refused):
//
//  1. core_auth_login for the workspace-manager (server=workspace-manager),
//     or for the instance's App (app=<instance>) with no pin, called through
//     muster as the person, answers with muster's sign-in link
//     (/oauth/proxy/start).
//  2. muster's proxy sends the browser to the provider's authorization
//     endpoint (PKCE). A GitLab instance answers with GitLab's sign-in form,
//     which takes the provider person's GitLab login and password
//     (providerLogin), then with the OAuth application's consent form,
//     whose Authorize the walk submits.
//  3. The provider redirects to muster's /oauth/proxy/callback, which
//     redeems the code and files the person's token under the person
//     (grantScope subject): every later MCP session of theirs has it, and
//     list_providers shows the instance connected.
//
// From this host every hop is a name the lab dials as a browser does: the
// platform's names on the edge's loopback port (dialLab), Dex on its issuer
// URL. A GitHub App's grant takes github.com's sign-in in a person's browser,
// which no headless walk makes: a proof finds it held or answers muster's
// sign-in link (openGrantedWorkspacePerson).

// The workspace-manager tools' argument names the walk and the proof pass.
const (
	workspaceArg         = "workspace"
	workspaceProviderArg = "provider"
	workspaceOwnerArg    = "owner"
)

// workspaceSignInMaxHops bounds the walk: muster's proxy start, the
// provider (a GitLab's authorization, sign-in form, its post and consent
// included) and muster's callback, with room for a provider that adds hops.
const workspaceSignInMaxHops = 16

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

// gitlabRedirectRe is the link of GitLab's "Redirecting" page, which an
// authorization the person already granted answers with a 200 instead of a
// redirect: the browser follows it by script.
var gitlabRedirectRe = regexp.MustCompile(`(?is)<a\b[^>]*\bhref\s*=\s*"([^"]*)"[^>]*>\s*Click here to redirect to`)

// gitlabRedirect is the target of GitLab's "Redirecting" page when it is the
// callback path, "" for any other page.
func gitlabRedirect(page []byte, callback string) string {
	m := gitlabRedirectRe.FindSubmatch(page)
	if m == nil {
		return ""
	}
	target := html.UnescapeString(string(m[1]))
	if u, err := url.Parse(target); err != nil || u.Path != callback {
		return ""
	}
	return target
}

var (
	htmlFormRe  = regexp.MustCompile(`(?is)<form\b([^>]*)>(.*?)</form>`)
	htmlInputRe = regexp.MustCompile(`(?is)<input\b([^>]*)>`)
	htmlAttrRe  = regexp.MustCompile(`(?is)([a-z_:-]+)\s*=\s*"([^"]*)"`)
)

// htmlPost is a form's method as htmlForms reads it for a POST.
const htmlPost = "post"

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
// login form a headless walk fills (a GitLab).
type providerLogin struct {
	login, password string
}

// gitlabFormStep answers the walk's next request on a GitLab page: the
// sign-in form filled with the person's GitLab login, or the consent's
// Authorize. ok is false on any other page.
func gitlabFormStep(page []byte, login *providerLogin, posted bool) (action string, form url.Values, ok bool, err error) {
	for _, f := range htmlForms(page) {
		switch {
		case f.method == htmlPost && strings.HasSuffix(f.action, gitlabSignInAction) && strings.Contains(string(page), gitlabPasswordField):
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
		case f.method == htmlPost && strings.HasSuffix(f.action, gitlabAuthorizePath) && f.fields.Get(railsMethodField) == "":
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

// musterSignInSucceeded is the heading of muster's proxy callback page on a
// completed sign-in: "Signed in" on its pagekit pages, "Authentication
// Successful" on a muster before them.
var musterSignInSucceeded = []string{"Signed in", "Authentication Successful"}

// signInCompleted says whether muster's callback page reports a completed
// sign-in.
func signInCompleted(page []byte) bool {
	return slices.ContainsFunc(musterSignInSucceeded, func(heading string) bool {
		return strings.Contains(string(page), heading)
	})
}

// errSignInStopped is a walk that ended before the callback completed; its
// message names the hop and the status it stopped at.
var errSignInStopped = errors.New("the provider sign-in stopped")

// walkProviderSignIn walks muster's sign-in link to its proxy callback.
// client follows no redirect itself and keeps the cookies. On a GitLab the
// walk fills its sign-in form with login and submits its consent; without a
// login it follows redirects alone. It returns the hops; an error names the
// hop the walk stopped at, its status and the page's heading.
func walkProviderSignIn(client *http.Client, link string, login *providerLogin) ([]signInHop, error) {
	var hops []signInHop
	stopped := func(h signInHop, why string) error {
		return fmt.Errorf("%w at %s: %s\n  hops: %s", errSignInStopped, h, why, signInHops(hops))
	}
	next, err := url.Parse(link)
	if err != nil {
		return nil, fmt.Errorf("the authorization link: %w", err)
	}
	callback := oauthProxyCallbackPath
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
		switch {
		case login != nil && resp.StatusCode == http.StatusOK && next.Path != callback:
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
		case next.Path == callback && !signInCompleted(page):
			return hops, stopped(hop, "muster's callback did not complete the sign-in: "+pageHeading(page))
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
		case login != nil && gitlabRedirect(page, callback) != "":
			// An authorization the person granted before: GitLab skips the
			// consent and sends the browser on to the callback by script.
			location, err := next.Parse(gitlabRedirect(page, callback))
			if err != nil {
				return hops, stopped(hop, "GitLab's redirect: "+err.Error())
			}
			next = location
		default:
			return hops, stopped(hop, "a page that is neither muster's callback nor a provider's sign-in or consent form: "+pageHeading(page))
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
// own cookie jar and no redirect followed. Every name is dialled as a
// browser on this host dials it.
func workspaceSignInClient() (*http.Client, error) {
	transport, err := labTLSTransport()
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &http.Client{Jar: jar, Transport: transport, Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

// workspacePerson is a lab user signed in to muster for the workspace-manager's
// tools. perApp is a lab whose registration is pinned to no App
// (config.WorkspaceProviderApps): the person grants and drops each
// instance's App by its name.
type workspacePerson struct {
	user    *config.User
	session *musterSession
	perApp  bool
}

// openWorkspacePerson signs user in to muster (Dex password grant, client
// agent-platform).
func openWorkspacePerson(cfg *config.Config, user *config.User) (*workspacePerson, error) {
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return nil, err
	}
	login, _, _ := strings.Cut(user.Email, "@")
	session, err := openMusterSession(cfg, token, "workspaces-"+login)
	if err != nil {
		return nil, err
	}
	return &workspacePerson{user: user, session: session, perApp: cfg.WorkspaceProviderApps()}, nil
}

// connectWorkspaceProvider has the person grant the provider instance's App
// in muster the way a browser does: core_auth_login's link walked through
// the provider's sign-in and consent to muster's callback, then
// list_providers checked to show them connected. The workspace-manager's
// registration must receive instance's grant (workspaceManagerReceives);
// login is the person's account at the provider (a GitLab's). It returns the
// walk's hops, worded ("already granted" for a person who had granted it);
// an error says where the walk stopped.
func connectWorkspaceProvider(cfg *config.Config, p *workspacePerson, instance string, login *providerLogin) (string, error) {
	if err := workspaceManagerReceives(cfg, instance); err != nil {
		return "", err
	}
	link, err := p.grantLink(instance)
	if err != nil {
		return "", err
	}
	hops := "already granted"
	if link != "" {
		client, err := workspaceSignInClient()
		if err != nil {
			return "", err
		}
		walked, err := walkProviderSignIn(client, link, login)
		if err != nil {
			return "", fmt.Errorf("%s granting %s as %s: %w", p.user.Email, instance, login.login, err)
		}
		hops = signInHops(walked)
	}
	status, err := p.providerStatus(instance)
	if err != nil {
		return "", err
	}
	if !status.Connected {
		return "", fmt.Errorf("the grant of %s at %s reached muster's callback, but list_providers still shows %s\n  hops: %s", p.user.Email, instance, status, hops)
	}
	// Held before or granted now, the run drops it at its end.
	recordWorkspaceGrant(p, instance)
	return hops, nil
}

// providerStatus is list_providers' entry for the instance, as the person.
func (p *workspacePerson) providerStatus(instance string) (workspaceProviderStatus, error) {
	listed, err := p.providerStatuses()
	if err != nil {
		return workspaceProviderStatus{}, err
	}
	i := slices.IndexFunc(listed, func(s workspaceProviderStatus) bool { return s.Name == instance })
	if i < 0 {
		return workspaceProviderStatus{}, fmt.Errorf("list_providers does not list %s for %s", instance, p.user.Email)
	}
	return listed[i], nil
}

// musterAppArg is the argument of core_auth_login and core_auth_logout that
// names a provider App muster keeps instead of a server.
const musterAppArg = "app"

// grantArgs are core_auth_login's and core_auth_logout's arguments for the
// instance's grant: its App by name with no pin, else the workspace-manager,
// whose pinned App is the instance's.
func (p *workspacePerson) grantArgs(instance string) map[string]any {
	if p.perApp {
		return map[string]any{musterAppArg: instance}
	}
	return map[string]any{serverKey: workspaceManagerMCPServer}
}

// providerStatuses is list_providers' answer, every instance, as the person.
func (p *workspacePerson) providerStatuses() ([]workspaceProviderStatus, error) {
	var listed struct {
		Providers []workspaceProviderStatus `json:"providers"`
	}
	if err := p.session.callServerStructured(workspaceManagerToolPrefix+"list_providers", nil, &listed); err != nil {
		return nil, err
	}
	return listed.Providers, nil
}

// workspaceProviderStatusesWorded words list_providers' answer, instance by
// instance.
func workspaceProviderStatusesWorded(listed []workspaceProviderStatus) string {
	words := make([]string, len(listed))
	for i, s := range listed {
		words[i] = s.String()
	}
	return strings.Join(words, ", ")
}

// notConnectedTo checks list_providers shows the instance not connected to
// the person.
func (p *workspacePerson) notConnectedTo(instance string) error {
	status, err := p.providerStatus(instance)
	if err != nil {
		return err
	}
	if status.Connected {
		return fmt.Errorf("list_providers as %s shows %s, who holds no grant of its App", p.user.Email, status)
	}
	note("list_providers as %s: %s", p.user.Email, status)
	return nil
}

// othersUnchanged checks, after the person dropped instance's grant, that
// list_providers shows instance not connected and every other instance as
// before: one App's drop touches no other App's grant.
func (p *workspacePerson) othersUnchanged(instance string, before []workspaceProviderStatus) error {
	after, err := p.providerStatuses()
	if err != nil {
		return err
	}
	for _, b := range before {
		i := slices.IndexFunc(after, func(a workspaceProviderStatus) bool { return a.Name == b.Name })
		switch {
		case i < 0:
			return fmt.Errorf("list_providers as %s no longer lists %s after dropping the grant of %s", p.user.Email, b.Name, instance)
		case b.Name == instance && after[i].Connected:
			return fmt.Errorf("list_providers as %s shows %s after core_auth_logout dropped its grant", p.user.Email, after[i])
		case b.Name != instance && after[i].Connected != b.Connected:
			return fmt.Errorf("dropping the grant of %s moved %s from %s to %s: one App's drop touches no other's grant", instance, b.Name, b, after[i])
		}
	}
	note("list_providers as %s after core_auth_logout app=%s: %s (was %s)", p.user.Email, instance, workspaceProviderStatusesWorded(after), workspaceProviderStatusesWorded(before))
	return nil
}

// grantLink is core_auth_login's sign-in link for the instance's App, as
// the person; "" when muster holds their grant already.
func (p *workspacePerson) grantLink(instance string) (string, error) {
	args := p.grantArgs(instance)
	env, err := p.session.callToolEnvelope("core_auth_login", args)
	if err != nil {
		return "", err
	}
	if link, _ := env.StructuredContent["authUrl"].(string); link != "" {
		return link, nil
	}
	text := env.Content[0].Text
	if !env.IsError && grantHeld(text) {
		return "", nil
	}
	return "", fmt.Errorf("core_auth_login %v as %s answered neither a sign-in link nor a held grant: %.300s", args, p.user.Email, text)
}

// grantHeld says whether core_auth_login's answer without a sign-in link
// is a grant the person holds: muster says it was already held, or that it
// connected the server with the person's token at once.
func grantHeld(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "already") || strings.Contains(lower, "successfully connected")
}

// musterSignInLinkRe finds muster's sign-in link in its auth_required answer.
var musterSignInLinkRe = regexp.MustCompile(`https?://[^\s"'\\]*` + regexp.QuoteMeta(oauthProxyStartPath) + `[^\s"'\\]*`)

// grantRequired is the answer of a workspace-manager tool to a person who
// has not granted the pinned App: muster's auth_required with its sign-in
// link, which it returns. Any other answer is an error.
func (p *workspacePerson) grantRequired(tool string, args map[string]any) (string, error) {
	env, err := p.session.callToolEnvelope(workspaceManagerToolPrefix+tool, args)
	link, text := musterSignInLink(env, err)
	if link != "" {
		return link, nil
	}
	return "", fmt.Errorf("%s as %s, who has not granted the workspace-manager's App, answered no muster sign-in link: %.400s", tool, p.user.Email, text)
}

// musterSignInLink is muster's sign-in link in a tool call's answer (its
// authUrl, or the link in its text or error), "" when there is none, and the
// text it was read from.
func musterSignInLink(env *toolEnvelope, err error) (link, text string) {
	switch {
	case err != nil:
		text = err.Error()
	case env.StructuredContent["authUrl"] != nil:
		text, _ = env.StructuredContent["authUrl"].(string)
	default:
		text = env.Content[0].Text
	}
	return musterSignInLinkRe.FindString(text), text
}

// workspaceManagerPinnedProvider is the instance whose App the live
// workspace-manager MCPServer is pinned to, the lab's grantProvider once the
// MCPServer carries an authorization server: the chart renders one only with
// muster.mcpServer.auth.provider, which the lab sets on a workspace-manager
// of 0.10.0 or newer alone.
func workspaceManagerPinnedProvider(cfg *config.Config) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	mcpServers, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return "", err
	}
	obj, err := getObject(ctx, mcpServers, platformNamespace, workspaceManagerMCPServer)
	if err != nil {
		return "", err
	}
	issuer, _, _ := unstructured.NestedString(obj.Object, "spec", "auth", "authorizationServer", "issuer")
	if issuer == "" {
		return "", fmt.Errorf("the MCPServer %s/%s is not pinned to a provider App: the person's sign-in moved to muster in workspace-manager 0.10.0, which the lab pins (components.workspace-manager.versionRange in platform.valuesFiles, then `agentlab platform`)", platformNamespace, workspaceManagerMCPServer)
	}
	return cfg.WorkspaceGrantProvider(), nil
}

// workspaceManagerReceives checks that the live workspace-manager MCPServer
// receives the person's grant of instance's App: named in its
// spec.auth.providerApps with no pin (config.WorkspaceProviderApps), else
// pinned to it (workspaceManagerPinnedProvider).
func workspaceManagerReceives(cfg *config.Config, instance string) error {
	if !cfg.WorkspaceProviderApps() {
		pinned, err := workspaceManagerPinnedProvider(cfg)
		if err == nil && pinned != instance {
			err = fmt.Errorf("muster pins the workspace-manager to the App of %s, not %s: a person grants only that App (`agentlab configure --workspaces-grant-provider %s` or %s, then `agentlab platform`)", pinned, instance, instance, config.WorkspaceGrantProviderApps)
		}
		return err
	}
	apps, err := workspaceManagerProviderApps()
	if err != nil {
		return err
	}
	if !slices.Contains(apps, instance) {
		return fmt.Errorf("the MCPServer %s/%s receives the grants of the provider Apps [%s], not %s: platform.workspaces.grantProvider %s needs a chart that keeps one muster provider App per instance (agent-platform.workspaceManager.providerApps), then `agentlab platform`", platformNamespace, workspaceManagerMCPServer, strings.Join(apps, ", "), instance, config.WorkspaceGrantProviderApps)
	}
	return nil
}

// workspaceManagerProviderApps are the provider Apps whose grants the live
// workspace-manager MCPServer receives (spec.auth.providerApps), none for a
// pinned or unauthenticated registration.
func workspaceManagerProviderApps() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	mcpServers, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return nil, err
	}
	obj, err := getObject(ctx, mcpServers, platformNamespace, workspaceManagerMCPServer)
	if err != nil {
		return nil, err
	}
	apps, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "auth", "providerApps")
	return apps, nil
}

// disconnect drops the person's grant of the instance's App in muster, best
// effort: a later proof starts from a person who has not granted it.
func (p *workspacePerson) disconnect(instance string) {
	if _, err := p.session.callToolEnvelope("core_auth_logout", p.grantArgs(instance)); err != nil {
		warn("dropping %s's grant of the App of %s: %v", p.user.Email, instance, err)
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
	// SyncRequested says the check requested a sync of the workspace, its
	// last one older than the manager's refresh threshold (refreshAfter).
	SyncRequested bool `json:"syncRequested"`
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
