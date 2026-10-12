package lab

import (
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// signInLab stands in for the walk's three parties on one test server: the
// provider's consent, the workspace-manager's callback and /signin pages
// (the callback completes only for a browser with the page session), and
// Dex with a connector chooser in front of its local login form.
type signInLab struct {
	server    *httptest.Server
	chooser   bool
	password  string
	completed bool
}

func newSignInLab(t *testing.T, chooser bool) *signInLab {
	t.Helper()
	l := &signInLab{chooser: chooser, password: "password"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("login") == "" {
			http.Error(w, "no lab user", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/callback/fake?code=c&state=s", http.StatusFound)
	})
	mux.HandleFunc("GET /callback/fake", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("session"); err != nil {
			http.SetCookie(w, &http.Cookie{Name: "signin", Value: "nonce", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
			http.Redirect(w, r, "/dex/auth?client_id=agent-platform&state=x", http.StatusSeeOther)
			return
		}
		l.completed = true
		_, _ = w.Write([]byte("<html><body><h1>fake connected</h1></body></html>"))
	})
	mux.HandleFunc("GET /signin", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("signin"); err != nil {
			http.Error(w, "<h1>Sign-in failed</h1>", http.StatusBadRequest)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: testLogin, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		http.Redirect(w, r, "/callback/fake?code=c&state=s", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /dex/auth", func(w http.ResponseWriter, r *http.Request) {
		if l.chooser {
			_, _ = w.Write([]byte(`<a href="/dex/auth/github?client_id=agent-platform&amp;state=x">GitHub</a><a href="/dex/auth/local?client_id=agent-platform&amp;state=x">Email</a>`))
			return
		}
		http.Redirect(w, r, "/dex/auth/local/login?state=x", http.StatusFound)
	})
	mux.HandleFunc("GET /dex/auth/local", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "x" {
			http.Error(w, "the chooser's query was lost", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/dex/auth/local/login?state=x", http.StatusFound)
	})
	mux.HandleFunc("GET /dex/auth/local/login", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<form method="post"><input name="login"><input type="password" name="password"></form>`))
	})
	mux.HandleFunc("POST /dex/auth/local/login", func(w http.ResponseWriter, r *http.Request) {
		if r.PostFormValue("login") != testPortalUser || r.PostFormValue("password") != l.password {
			_, _ = w.Write([]byte(`<p>Invalid Email Address and password.</p><input type="password" name="password">`))
			return
		}
		http.Redirect(w, r, "/signin?code=d&state=x", http.StatusSeeOther)
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
	return l.server.URL + "/login/oauth/authorize?" + url.Values{githubState: {"s"}, githubLogin: {testLogin}}.Encode()
}

func TestWalkProviderSignIn(t *testing.T) {
	for _, chooser := range []bool{false, true} {
		l := newSignInLab(t, chooser)
		hops, err := walkProviderSignIn(l.client(t), l.link(), "fake", testPortalUser, "password")
		if err != nil {
			t.Fatalf("chooser %v: %v", chooser, err)
		}
		if !l.completed {
			t.Fatalf("chooser %v: the callback never completed: %s", chooser, signInHops(hops))
		}
		worded := signInHops(hops)
		for _, want := range []string{"GET " + l.server.URL + "/login/oauth/authorize 302", "POST " + l.server.URL + "/dex/auth/local/login 303", "GET " + l.server.URL + "/signin 303", "GET " + l.server.URL + "/callback/fake 200"} {
			if !strings.Contains(worded, want) {
				t.Errorf("chooser %v: hops %s lack %q", chooser, worded, want)
			}
		}
		if strings.Contains(worded, "code=") || strings.Contains(worded, "state=") {
			t.Errorf("chooser %v: the hops carry a query: %s", chooser, worded)
		}
	}
}

func TestWalkProviderSignInStops(t *testing.T) {
	l := newSignInLab(t, false)
	link := strings.Replace(l.link(), "login=admin", "login=", 1)
	_, err := walkProviderSignIn(l.client(t), link, "fake", testPortalUser, "password")
	if !errors.Is(err, errSignInStopped) || !strings.Contains(err.Error(), "/login/oauth/authorize 400") {
		t.Fatalf("a refused consent: got %v, want the walk stopped at the authorize hop with 400", err)
	}

	l = newSignInLab(t, false)
	l.password = "other"
	_, err = walkProviderSignIn(l.client(t), l.link(), "fake", testPortalUser, "password")
	if !errors.Is(err, errSignInStopped) || !strings.Contains(err.Error(), "refused admin@lab.local's credentials") {
		t.Fatalf("wrong credentials: got %v, want the walk stopped at Dex's login", err)
	}

	l = newSignInLab(t, false)
	l.server.Close()
	_, err = walkProviderSignIn(l.client(t), l.link(), "fake", testPortalUser, "password")
	if !errors.Is(err, errSignInStopped) || !strings.Contains(err.Error(), "/login/oauth/authorize 0") {
		t.Fatalf("an unreachable provider: got %v, want the walk stopped at the authorize hop", err)
	}
}
