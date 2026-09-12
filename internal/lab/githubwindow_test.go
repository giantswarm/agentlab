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

// fakeGitHubRateLimit is /rate_limit on a test server: the window it answers
// with, the Authorization headers it saw, and the seams pointed at it.
type fakeGitHubRateLimit struct {
	mu        sync.Mutex
	remaining int
	reset     time.Time
	auth      []string
	slept     []time.Duration
}

func newFakeGitHubRateLimit(t *testing.T, reset time.Time) *fakeGitHubRateLimit {
	t.Helper()
	f := &fakeGitHubRateLimit{reset: reset}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		_, _ = fmt.Fprintf(w, `{"resources":{"core":{"limit":60,"remaining":%d,"reset":%d}}}`, f.remaining, f.reset.Unix())
	}))
	prevEndpoint, prevNow, prevSleep := gitHubRateLimitEndpoint, gitHubNow, gitHubSleep
	gitHubRateLimitEndpoint = srv.URL
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
		gitHubRateLimitEndpoint, gitHubNow, gitHubSleep = prevEndpoint, prevNow, prevSleep
	})
	return f
}

func (f *fakeGitHubRateLimit) set(remaining int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remaining, f.auth, f.slept = remaining, nil, nil
}

func (f *fakeGitHubRateLimit) seen() (auth []string, slept []time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...), append([]time.Duration(nil), f.slept...)
}

// TestAwaitGitHubWindow: one read, authenticated exactly when the host has a
// token; no wait while the window has budget; one bounded wait until the
// reset when it is exhausted, then a re-read; an error only when the window
// is still empty after the wait. The print never carries the token.
func TestAwaitGitHubWindow(t *testing.T) {
	reset := time.Unix(time.Now().Add(30*time.Minute).Unix(), 0)
	f := newFakeGitHubRateLimit(t, reset)

	t.Run("unauthenticated with budget", func(t *testing.T) {
		t.Setenv(GitHubTokenEnv, "")
		f.set(42)
		if err := awaitGitHubWindow("the proof"); err != nil {
			t.Fatal(err)
		}
		auth, slept := f.seen()
		if len(auth) != 1 || auth[0] != "" {
			t.Errorf("Authorization headers = %q without a token, want one empty", auth)
		}
		if len(slept) != 0 {
			t.Errorf("waited %v on a window with budget", slept)
		}
	})

	t.Run("authenticated", func(t *testing.T) {
		t.Setenv(GitHubTokenEnv, gitHubTestToken)
		f.set(4999)
		if err := awaitGitHubWindow("the proof"); err != nil {
			t.Fatal(err)
		}
		auth, _ := f.seen()
		if len(auth) != 1 || auth[0] != "Bearer "+gitHubTestToken {
			t.Errorf("Authorization headers = %q, want the bearer token once", auth)
		}
	})

	t.Run("exhausted waits once until the reset", func(t *testing.T) {
		t.Setenv(GitHubTokenEnv, "")
		f.set(0)
		if err := awaitGitHubWindow("the proof"); err != nil {
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

	t.Run("a reset beyond an hour is capped", func(t *testing.T) {
		prev := gitHubNow
		gitHubNow = func() time.Time { return reset.Add(-3 * time.Hour) }
		t.Cleanup(func() { gitHubNow = prev })
		f.set(0)
		if err := awaitGitHubWindow("the proof"); err != nil {
			t.Fatal(err)
		}
		if _, slept := f.seen(); len(slept) != 1 || slept[0] != gitHubWindowMaxWait {
			t.Errorf("slept %v, want the %s cap", slept, gitHubWindowMaxWait)
		}
	})

	t.Run("still exhausted after the wait", func(t *testing.T) {
		prev := gitHubSleep
		gitHubSleep = func(time.Duration) {} // no refill
		t.Cleanup(func() { gitHubSleep = prev })
		f.set(0)
		err := awaitGitHubWindow("the proof")
		if err == nil || !strings.Contains(err.Error(), "still exhausted") {
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
	if err := awaitGitHubWindow("the proof"); err != nil {
		t.Errorf("an unreachable /rate_limit failed the proof: %v", err)
	}
}

// TestGitHubWindowPrintNamesTheVariableNotTheToken pins the print's shape.
func TestGitHubWindowPrintNamesTheVariableNotTheToken(t *testing.T) {
	w := gitHubWindow{Limit: 5000, Remaining: 4321, Reset: time.Unix(1_800_000_000, 0), Authenticated: true}
	got := w.String()
	for _, want := range []string{"4321 of 5000", "$" + GitHubTokenEnv, "resets "} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	if !strings.Contains(gitHubWindow{Limit: 60, Remaining: 0}.String(), "unauthenticated") {
		t.Errorf("an unauthenticated window must say so: %q", gitHubWindow{Limit: 60}.String())
	}
}
