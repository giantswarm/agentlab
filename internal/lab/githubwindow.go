package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/giantswarm/agentlab/pkg/project"
)

// gitHubRateLimitURL is GitHub's window endpoint; a call to it does not count
// against the window it reports.
const gitHubRateLimitURL = "https://api.github.com/rate_limit"

// gitHubWindowMaxWait bounds the one wait on an exhausted window: GitHub's
// windows are an hour, so a reset further out than this is a clock problem,
// not something to sleep through.
const gitHubWindowMaxWait = 65 * time.Minute

// Seams the tests replace: the endpoint, the clock and the wait.
var (
	gitHubRateLimitEndpoint = gitHubRateLimitURL
	gitHubNow               = time.Now
	gitHubSleep             = time.Sleep
)

// gitHubWindow is the core REST API window as /rate_limit reports it.
type gitHubWindow struct {
	Limit, Remaining int
	Reset            time.Time
	Authenticated    bool
}

// String is the window print — never the token.
func (w gitHubWindow) String() string {
	who := "unauthenticated: this machine's shared window"
	if w.Authenticated {
		who = "authenticated with $" + GitHubTokenEnv
	}
	return fmt.Sprintf("GitHub API window (%s): %d of %d requests remaining, resets %s", who, w.Remaining, w.Limit, w.Reset.Local().Format("15:04:05 MST"))
}

// readGitHubWindow asks /rate_limit once, with the host's token when set;
// its own deadline, since the caller may sleep an hour between two reads.
func readGitHubWindow() (gitHubWindow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gitHubRateLimitEndpoint, nil)
	if err != nil {
		return gitHubWindow{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "agentlab/"+project.Version())
	token := os.Getenv(GitHubTokenEnv)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return gitHubWindow{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return gitHubWindow{}, fmt.Errorf("GET %s answered %d", gitHubRateLimitEndpoint, resp.StatusCode)
	}
	var body struct {
		Resources struct {
			Core struct {
				Limit     int   `json:"limit"`
				Remaining int   `json:"remaining"`
				Reset     int64 `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return gitHubWindow{}, fmt.Errorf("GET %s: not the expected JSON: %w", gitHubRateLimitEndpoint, err)
	}
	core := body.Resources.Core
	return gitHubWindow{Limit: core.Limit, Remaining: core.Remaining, Reset: time.Unix(core.Reset, 0), Authenticated: token != ""}, nil
}

// awaitGitHubWindow prints the GitHub API window before a proof step that
// resolves skills through GitHub (the portal's discovery, agent-manager's
// list_skills and create_agent) and, when the window is exhausted, waits once
// — bounded — until it resets rather than letting the step fail on a
// truncated listing. The consumers in the cluster share this machine's egress
// address, so an unauthenticated read here is their window; with
// $GITHUB_TOKEN set on the host the lab handed them the same token
// (githubtoken.go), so the read is authenticated as theirs are. An endpoint
// that cannot be read is a note, never a failure; a window still exhausted
// after the wait is.
func awaitGitHubWindow(what string) error {
	w, err := readGitHubWindow()
	if err != nil {
		note("GitHub API window not read (%v); %s proceeds without it", err, what)
		return nil
	}
	note("%s", w)
	if w.Remaining > 0 {
		return nil
	}
	wait := max(w.Reset.Sub(gitHubNow())+5*time.Second, 0)
	if wait > gitHubWindowMaxWait {
		wait = gitHubWindowMaxWait
	}
	note("the window is exhausted — waiting %s for it to reset at %s before %s (one bounded wait; export $%s to lift it to 5000 an hour)", wait.Round(time.Second), w.Reset.Local().Format("15:04:05 MST"), what, GitHubTokenEnv)
	gitHubSleep(wait)
	if w, err = readGitHubWindow(); err != nil {
		note("GitHub API window not re-read after the wait (%v); %s proceeds", err, what)
		return nil
	}
	note("%s", w)
	if w.Remaining == 0 {
		return fmt.Errorf("GitHub API window still exhausted after waiting for its reset (%s): %s cannot resolve skills — export $%s or try again after %s", w.Reset.Local().Format("15:04:05 MST"), what, GitHubTokenEnv, w.Reset.Local().Format("15:04:05 MST"))
	}
	return nil
}
