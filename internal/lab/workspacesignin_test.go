package lab

import (
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// signInLab stands in for the walk's parties on one test server: muster's
// proxy start, which sends the browser to the provider's authorization
// endpoint, the lab GitHub's own consent page and its post, and muster's
// proxy callback, which completes the grant once.
type signInLab struct {
	server    *httptest.Server
	fails     bool
	completed bool
}

func newSignInLab(t *testing.T) *signInLab {
	t.Helper()
	fixture, err := loadGitHubFixture("")
	if err != nil {
		t.Fatal(err)
	}
	github := &workspaceGitHub{fixture: fixture, now: time.Now, codes: map[string]*oauthCode{}}
	l := &signInLab{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+oauthProxyStartPath, func(w http.ResponseWriter, r *http.Request) {
		q := url.Values{githubState: {r.URL.Query().Get(githubState)}, "client_id": {fixture.App.ClientID},
			"redirect_uri": {l.server.URL + oauthProxyCallbackPath}, "code_challenge": {"c"}, "code_challenge_method": {"S256"}}
		http.Redirect(w, r, githubAuthorizePath+"?"+q.Encode(), http.StatusFound)
	})
	mux.HandleFunc("GET "+githubAuthorizePath, github.authorize)
	mux.HandleFunc("POST "+githubAuthorizePath, github.authorize)
	mux.HandleFunc("GET "+oauthProxyCallbackPath, func(w http.ResponseWriter, _ *http.Request) {
		if l.fails {
			_, _ = w.Write([]byte("<html><body><h1>Authentication Failed</h1></body></html>"))
			return
		}
		l.completed = true
		_, _ = w.Write([]byte("<html><body><h1>" + musterSignInSucceeded[0] + "</h1></body></html>"))
	})
	l.server = httptest.NewTLSServer(mux)
	t.Cleanup(l.server.Close)
	return l
}

func (l *signInLab) client(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Transport: l.server.Client().Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (l *signInLab) link() string {
	return l.server.URL + oauthProxyStartPath + "?" + url.Values{githubState: {"s"}}.Encode()
}

func TestWalkProviderSignIn(t *testing.T) {
	l := newSignInLab(t)
	hops, err := walkProviderSignIn(l.client(t), l.link(), testLogin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !l.completed {
		t.Fatalf("muster's callback never completed: %s", signInHops(hops))
	}
	worded := signInHops(hops)
	for _, want := range []string{"GET " + l.server.URL + oauthProxyStartPath + " 302", "GET " + l.server.URL + githubAuthorizePath + " 200", "POST " + l.server.URL + githubAuthorizePath + " 302", "GET " + l.server.URL + oauthProxyCallbackPath + " 200"} {
		if !strings.Contains(worded, want) {
			t.Errorf("hops %s lack %q", worded, want)
		}
	}
	if strings.Contains(worded, "code=") || strings.Contains(worded, "state=") {
		t.Errorf("the hops carry a query: %s", worded)
	}
}

func TestWalkProviderSignInStops(t *testing.T) {
	l := newSignInLab(t)
	_, err := walkProviderSignIn(l.client(t), l.link(), "", nil)
	if !errors.Is(err, errSignInStopped) || !strings.Contains(err.Error(), githubAuthorizePath+" 200") || !strings.Contains(err.Error(), "no Authorize") {
		t.Fatalf("a consent without the person: got %v, want the walk stopped at the consent page", err)
	}

	l = newSignInLab(t)
	l.fails = true
	_, err = walkProviderSignIn(l.client(t), l.link(), testLogin, nil)
	if !errors.Is(err, errSignInStopped) || !strings.Contains(err.Error(), "did not complete the sign-in") || !strings.Contains(err.Error(), "Authentication Failed") {
		t.Fatalf("a failed callback: got %v, want the walk stopped at muster's callback naming its heading", err)
	}

	l = newSignInLab(t)
	l.server.Close()
	_, err = walkProviderSignIn(l.client(t), l.link(), testLogin, nil)
	if !errors.Is(err, errSignInStopped) || !strings.Contains(err.Error(), oauthProxyStartPath+" 0") {
		t.Fatalf("an unreachable muster: got %v, want the walk stopped at the proxy start hop", err)
	}
}

// muster's auth_required answer carries its sign-in link, which grantRequired
// finds in the prose or a JSON body alike.
func TestMusterSignInLinkRe(t *testing.T) {
	for _, text := range []string{
		`Authentication required for workspace-manager: sign in at https://muster.127.0.0.1.nip.io:8443/oauth/proxy/start?state=abc then retry.`,
		`{"status":"auth_required","auth_url":"https://muster.127.0.0.1.nip.io:8443/oauth/proxy/start?state=abc"}`,
	} {
		if got := musterSignInLinkRe.FindString(text); got != "https://muster.127.0.0.1.nip.io:8443/oauth/proxy/start?state=abc" {
			t.Errorf("%s: found %q", text, got)
		}
	}
	if got := musterSignInLinkRe.FindString(`refused: the workspace is unknown`); got != "" {
		t.Errorf("found %q in a refusal without a link", got)
	}
}

// The walk's GitLab steps: the sign-in form filled with the person's login,
// a second sign-in form a refusal, the consent's Authorize (not its Deny,
// which Rails posts as a DELETE), and any other page not GitLab's.
func TestGitLabFormStep(t *testing.T) {
	signIn := []byte(`<form class="new_user" action="/users/sign_in" accept-charset="UTF-8" method="post"><input type="hidden" name="authenticity_token" value="tok&amp;en" /><input name="user[login]" /><input type="password" name="user[password]" /></form>`)
	consent := []byte(`<form action="/oauth/authorize" method="post"><input type="hidden" name="_method" value="delete" /><input name="state" value="s" /></form>` +
		`<form action="/oauth/authorize" method="post"><input name="authenticity_token" value="t" /><input name="state" value="s" /></form>`)
	login := &providerLogin{login: testGitLabPerson, password: "pw"}

	action, form, ok, err := gitlabFormStep(signIn, login, false)
	if err != nil || !ok || action != "/users/sign_in" || form.Get("user[login]") != testGitLabPerson || form.Get("user[password]") != "pw" || form.Get("authenticity_token") != "tok&en" {
		t.Errorf("sign-in: %q %v %v %v", action, form, ok, err)
	}
	if _, _, ok, err := gitlabFormStep(signIn, login, true); !ok || err == nil {
		t.Errorf("the sign-in form again after posting: ok %v, err %v, want a refusal", ok, err)
	}
	if _, _, ok, err := gitlabFormStep(signIn, nil, false); !ok || err == nil {
		t.Errorf("a sign-in without a login: ok %v, err %v, want an error", ok, err)
	}
	action, form, ok, err = gitlabFormStep(consent, login, true)
	if err != nil || !ok || action != "/oauth/authorize" || form.Get(railsMethodField) != "" || form.Get("authenticity_token") != "t" {
		t.Errorf("consent: %q %v %v %v", action, form, ok, err)
	}
	if _, _, ok, err := gitlabFormStep([]byte(`<html><form action="/search" method="get"></form></html>`), login, false); ok || err != nil {
		t.Errorf("another page: ok %v, err %v", ok, err)
	}
}

// GitLab answers an authorization the person granted before with its
// "Redirecting" page; the walk follows its link only to the callback.
func TestGitLabRedirect(t *testing.T) {
	const callback = "/oauth/proxy/callback"
	page := func(href string) []byte {
		return []byte(`<h1>Redirecting</h1><div><a href="` + href + `">Click here to redirect to ` + href + `</a></div><script>window.location = "x";</script>`)
	}
	for href, want := range map[string]string{
		"https://muster.example.com/oauth/proxy/callback?code=c&amp;state=s": "https://muster.example.com/oauth/proxy/callback?code=c&state=s",
		"https://elsewhere.example.com/steal?code=c":                         "",
	} {
		if got := gitlabRedirect(page(href), callback); got != want {
			t.Errorf("gitlabRedirect(%q) = %q, want %q", href, got, want)
		}
	}
	if got := gitlabRedirect([]byte(`<a href="/oauth/proxy/callback">elsewhere</a>`), callback); got != "" {
		t.Errorf("a page without GitLab's redirect link: %q", got)
	}
}

func TestGrantHeld(t *testing.T) {
	for text, want := range map[string]bool{
		"Successfully connected to 'workspace-manager'!": true,
		"Already authenticated to workspace-manager":     true,
		"Authentication required: open the link":         false,
		"":                                               false,
	} {
		if got := grantHeld(text); got != want {
			t.Errorf("grantHeld(%q) = %t, want %t", text, got, want)
		}
	}
}

func TestSignInCompleted(t *testing.T) {
	for page, want := range map[string]bool{
		"<h1>Signed in</h1><p>workspace-manager is now connected</p>": true,
		"<h1>Authentication Successful</h1>":                          true,
		"<h1>Authentication Failed</h1>":                              false,
		"<h1>Sign-in failed</h1>":                                     false,
	} {
		if got := signInCompleted([]byte(page)); got != want {
			t.Errorf("signInCompleted(%q) = %t, want %t", page, got, want)
		}
	}
}
