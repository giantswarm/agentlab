package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub is GitHub's releases API for Repository: every call's
// Authorization header is recorded; limited answers each call with the 403
// GitHub gives once a caller's rate limit is spent, else the releases list
// one release, latestV, with this platform's binary and its bundle.
type fakeGitHub struct {
	limited bool
	reset   time.Time

	mu    sync.Mutex
	auths []string
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.auths = append(g.auths, r.Header.Get("Authorization"))
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if g.limited {
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(g.reset.Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for 192.0.2.1."}`))
		return
	}
	rel := map[string]any{
		"id":           1,
		"tag_name":     latestV,
		"html_url":     "https://github.com/" + Repository + "/releases/tag/" + latestV,
		"published_at": "2026-09-08T10:00:00Z",
		"assets": []map[string]any{
			{"id": bundleID, "name": binaryName() + ".bundle", "size": 3, "browser_download_url": "https://example.test/bundle"},
			{"id": binaryID, "name": binaryName(), "size": 3, "browser_download_url": "https://example.test/binary"},
		},
	}
	var body any = rel
	if !strings.HasSuffix(r.URL.Path, "/latest") {
		body = []any{rel}
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (g *fakeGitHub) authorizations() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.auths...)
}

// githubLab is a lab whose releases come from the GitHub source self-update
// ships, pointed at gh.
func githubLab(t *testing.T, gh *fakeGitHub) {
	t.Helper()
	lab(t, running, nil)
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	prevSource, prevURL := newSource, githubAPIURL
	newSource, githubAPIURL = githubReleases, srv.URL+"/"
	t.Cleanup(func() { newSource, githubAPIURL = prevSource, prevURL })
}

// GH_TOKEN wins over GITHUB_TOKEN, and either over the token gh keeps;
// without any of them the call is anonymous.
func TestCredentialOrder(t *testing.T) {
	const (
		fromGHEnv     = "token-from-gh-env"
		fromGitHubEnv = "token-from-github-env"
		fromGHCLI     = "token-from-gh-cli"
	)
	for _, tc := range []struct {
		name, ghToken, githubToken, ghAuth string
		want                               githubCredential
	}{
		{name: "GH_TOKEN first", ghToken: fromGHEnv, githubToken: fromGitHubEnv, ghAuth: fromGHCLI, want: githubCredential{fromGHEnv, ghTokenEnv}},
		{name: "GITHUB_TOKEN next", githubToken: fromGitHubEnv, ghAuth: fromGHCLI, want: githubCredential{fromGitHubEnv, githubTokenEnv}},
		{name: "gh's token last", ghAuth: fromGHCLI, want: githubCredential{fromGHCLI, ghAuthTokenSource}},
		{name: "anonymous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lab(t, running, nil)
			t.Setenv(ghTokenEnv, tc.ghToken)
			t.Setenv(githubTokenEnv, tc.githubToken)
			ghAuthToken = func(context.Context) string { return tc.ghAuth }
			if got := credential(t.Context()); got != tc.want {
				t.Errorf("credential() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// With GH_TOKEN set, the releases are read with it, and the run says so.
func TestRunReadsTheReleasesWithGHToken(t *testing.T) {
	gh := &fakeGitHub{}
	githubLab(t, gh)
	t.Setenv(ghTokenEnv, "gh-env-token")
	t.Setenv(githubTokenEnv, "github-env-token")
	var out bytes.Buffer
	if err := Run(t.Context(), &out, Options{Check: true}); !errors.Is(err, ErrOutdated) {
		t.Fatalf("Run(--check) = %v, want ErrOutdated\n%s", err, out.String())
	}
	auths := gh.authorizations()
	if len(auths) == 0 {
		t.Fatal("GitHub was never asked")
	}
	for _, a := range auths {
		if a != "Bearer gh-env-token" {
			t.Errorf("Authorization = %q, want GH_TOKEN's", a)
		}
	}
	if !strings.Contains(out.String(), "with the token from "+ghTokenEnv) {
		t.Errorf("the run does not say it read with GH_TOKEN:\n%s", out.String())
	}
}

// An anonymous call that meets GitHub's spent rate limit says it was
// anonymous and which variables lift it; a token's says whose limit is
// spent.
func TestRunNamesTheCredentialOfASpentRateLimit(t *testing.T) {
	reset := time.Date(2026, 10, 10, 21, 45, 0, 0, time.UTC)
	t.Run("anonymous", func(t *testing.T) {
		gh := &fakeGitHub{limited: true, reset: reset}
		githubLab(t, gh)
		var out bytes.Buffer
		err := Run(t.Context(), &out, Options{Check: true})
		if err == nil {
			t.Fatal("Run(--check) = nil, want the rate limit")
		}
		for _, want := range []string{"anonymous", ghTokenEnv, githubTokenEnv, "gh auth login", reset.Local().Format(time.RFC3339), "rate limit exceeded"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "online") {
			t.Errorf("a rate limit is worded as a network failure: %v", err)
		}
		for _, a := range gh.authorizations() {
			if a != "" {
				t.Errorf("an anonymous call sent Authorization %q", a)
			}
		}
		if !strings.Contains(out.String(), "anonymously") {
			t.Errorf("the run does not say it reads anonymously:\n%s", out.String())
		}
	})
	t.Run("token", func(t *testing.T) {
		githubLab(t, &fakeGitHub{limited: true, reset: reset})
		t.Setenv(githubTokenEnv, "github-env-token")
		err := Run(t.Context(), &bytes.Buffer{}, Options{Check: true})
		if err == nil || !strings.Contains(err.Error(), "the token from "+githubTokenEnv) || strings.Contains(err.Error(), "anonymous") {
			t.Errorf("Run(--check) = %v, want GITHUB_TOKEN's limit named", err)
		}
		if err != nil && strings.Contains(err.Error(), "github-env-token") {
			t.Error("the error carries the token")
		}
	})
}
