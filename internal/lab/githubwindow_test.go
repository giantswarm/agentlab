package lab

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// rateLimitServer is /rate_limit on a test server: the window it answers
// with, the Authorization headers it saw, and the seams pointed at it.
type rateLimitServer struct {
	mu        sync.Mutex
	remaining int
	reset     time.Time
	auth      []string
	slept     []time.Duration
}

func newRateLimitServer(t *testing.T, reset time.Time) *rateLimitServer {
	t.Helper()
	f := &rateLimitServer{reset: reset}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		_, _ = fmt.Fprintf(w, `{"resources":{"core":{"limit":60,"remaining":%d,"reset":%d}}}`, f.remaining, f.reset.Unix())
	}))
	prevEndpoint, prevNow, prevSleep, prevLab, prevPortal := gitHubRateLimitEndpoint, gitHubNow, gitHubSleep, labGitHubAuthenticated, portalAuthenticated
	gitHubRateLimitEndpoint = srv.URL
	labGitHubAuthenticated = func() (bool, error) { return false, nil }
	portalAuthenticated = func() (bool, error) { return false, nil }
	gitHubNow = func() time.Time { return reset.Add(-30 * time.Minute) }
	// The reset refills the window, as GitHub's does.
	gitHubSleep = func(d time.Duration) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.slept = append(f.slept, d)
		f.remaining = 60
	}
	t.Cleanup(func() {
		srv.Close()
		gitHubRateLimitEndpoint, gitHubNow, gitHubSleep, labGitHubAuthenticated, portalAuthenticated = prevEndpoint, prevNow, prevSleep, prevLab, prevPortal
	})
	return f
}

func (f *rateLimitServer) set(remaining int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remaining, f.auth, f.slept = remaining, nil, nil
}

func (f *rateLimitServer) seen() (auth []string, slept []time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...), append([]time.Duration(nil), f.slept...)
}

// TestAwaitGitHubWindow: one read, always unauthenticated — agentlab holds no
// GitHub credential, a $GITHUB_TOKEN in its environment included; no wait
// while the window has budget; one bounded wait until the reset when it is
// exhausted, then a re-read; an error only when the window is still empty
// after the wait.
func TestAwaitGitHubWindow(t *testing.T) {
	reset := time.Unix(time.Now().Add(30*time.Minute).Unix(), 0)
	f := newRateLimitServer(t, reset)

	t.Run("unauthenticated with budget, whatever the environment holds", func(t *testing.T) {
		t.Setenv(GitHubTokenEnv, gitHubTestToken)
		f.set(42)
		if err := awaitGitHubWindow("the proof", 1, false); err != nil {
			t.Fatal(err)
		}
		auth, slept := f.seen()
		if len(auth) != 1 || auth[0] != "" {
			t.Errorf("Authorization headers = %q, want one empty: agentlab never reads $%s", auth, GitHubTokenEnv)
		}
		if len(slept) != 0 {
			t.Errorf("waited %v on a window with budget", slept)
		}
	})

	t.Run("exhausted waits once until the reset", func(t *testing.T) {
		f.set(0)
		if err := awaitGitHubWindow("the proof", 1, false); err != nil {
			t.Fatal(err)
		}
		auth, slept := f.seen()
		if len(slept) != 1 || slept[0] != 30*time.Minute+5*time.Second {
			t.Errorf("slept %v, want one wait of 30m5s (until the reset plus a margin)", slept)
		}
		if len(auth) != 2 {
			t.Errorf("%d reads, want the read and the re-read after the wait", len(auth))
		}
	})

	t.Run("a lab with its own token never waits on the host's window", func(t *testing.T) {
		prev := labGitHubAuthenticated
		labGitHubAuthenticated = func() (bool, error) { return true, nil }
		t.Cleanup(func() { labGitHubAuthenticated = prev })
		f.set(0)
		if err := awaitGitHubWindow("the proof", 1, false); err != nil {
			t.Fatal(err)
		}
		if _, slept := f.seen(); len(slept) != 0 {
			t.Errorf("waited %v on the host's window although the lab's consumers carry a token", slept)
		}
	})

	t.Run("a portal step waits while only agent-manager carries a credential", func(t *testing.T) {
		prevLab, prevPortal := labGitHubAuthenticated, portalAuthenticated
		labGitHubAuthenticated = func() (bool, error) { return true, nil }
		t.Cleanup(func() { labGitHubAuthenticated, portalAuthenticated = prevLab, prevPortal })
		f.set(0)
		if err := awaitGitHubWindow("the proof", 1, true); err != nil {
			t.Fatal(err)
		}
		if _, slept := f.seen(); len(slept) != 1 {
			t.Errorf("slept %v, want one wait: the portal reads on the anonymous window", slept)
		}
		portalAuthenticated = func() (bool, error) { return true, nil }
		f.set(0)
		if err := awaitGitHubWindow("the proof", 1, true); err != nil {
			t.Fatal(err)
		}
		if _, slept := f.seen(); len(slept) != 0 {
			t.Errorf("waited %v although agent-manager and the portal carry a credential", slept)
		}
	})

	t.Run("a reset beyond an hour is capped", func(t *testing.T) {
		prev := gitHubNow
		gitHubNow = func() time.Time { return reset.Add(-3 * time.Hour) }
		t.Cleanup(func() { gitHubNow = prev })
		f.set(0)
		if err := awaitGitHubWindow("the proof", 1, false); err != nil {
			t.Fatal(err)
		}
		if _, slept := f.seen(); len(slept) != 1 || slept[0] != gitHubWindowMaxWait {
			t.Errorf("slept %v, want the %s cap", slept, gitHubWindowMaxWait)
		}
	})

	t.Run("short of the need waits once", func(t *testing.T) {
		f.set(9)
		if err := awaitGitHubWindow("the proof", backstageTestGitHubRequests, false); err != nil {
			t.Fatal(err)
		}
		if _, slept := f.seen(); len(slept) != 1 {
			t.Errorf("slept %v with 9 of the %d requests the proof needs, want one wait", slept, backstageTestGitHubRequests)
		}
	})

	t.Run("still exhausted after the wait", func(t *testing.T) {
		prev := gitHubSleep
		gitHubSleep = func(time.Duration) {} // no refill
		t.Cleanup(func() { gitHubSleep = prev })
		f.set(0)
		err := awaitGitHubWindow("the proof", 1, false)
		if err == nil || !strings.Contains(err.Error(), "still short") {
			t.Errorf("err = %v, want the still-exhausted verdict", err)
		}
	})
}

// TestAwaitGitHubWindowUnreachableIsANote: an endpoint that cannot be read
// never fails the proof.
func TestAwaitGitHubWindowUnreachableIsANote(t *testing.T) {
	prev := gitHubRateLimitEndpoint
	gitHubRateLimitEndpoint = "http://127.0.0.1:1/rate_limit"
	t.Cleanup(func() { gitHubRateLimitEndpoint = prev })
	if err := awaitGitHubWindow("the proof", 1, false); err != nil {
		t.Errorf("an unreachable /rate_limit failed the proof: %v", err)
	}
}

// TestGitHubWindowPrint pins the print's shape: the machine's anonymous
// window, never a variable to export.
func TestGitHubWindowPrint(t *testing.T) {
	got := gitHubWindow{Limit: 60, Remaining: 42, Reset: time.Unix(1_800_000_000, 0)}.String()
	for _, want := range []string{"unauthenticated", "42 of 60", "resets "} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "$"+GitHubTokenEnv) {
		t.Errorf("%q names $%s, which agentlab never reads", got, GitHubTokenEnv)
	}
}
