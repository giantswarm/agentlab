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

// Seams the tests replace: the endpoint, the clock, the wait and the look at
// whether the lab's consumers carry a token of their own.
var (
	gitHubRateLimitEndpoint = gitHubRateLimitURL
	gitHubNow               = time.Now
	gitHubSleep             = time.Sleep
	labGitHubAuthenticated  = agentManagerGitHubAuthenticated
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
// list_skills and create_agent) and, when fewer than need requests are left,
// waits once — bounded — until it resets rather than letting the step fail on
// a truncated listing. need is what the proof spends of the window; a run
// that starts with less would fail part-way. The consumers in the cluster
// share this machine's egress address, so an unauthenticated read here is
// their window; with $GITHUB_TOKEN set on the host the lab handed them the
// same token (githubtoken.go), so the read is authenticated as theirs are.
// Without a host token, a lab whose consumers carry one from
// githubToken.source never waits on the host's anonymous window: it is not
// theirs. An endpoint that cannot be read is a note, never a failure; a
// window still short after the wait is.
func awaitGitHubWindow(what string, need int) error {
	w, err := readGitHubWindow()
	if err != nil {
		note("GitHub API window not read (%v); %s proceeds without it", err, what)
		return nil
	}
	note("%s", w)
	if w.Remaining >= need {
		return nil
	}
	if !w.Authenticated {
		authenticated, err := labGitHubAuthenticated()
		if err != nil {
			note("whether the lab's consumers carry a GitHub token was not read (%v); the host's window decides", err)
		}
		if authenticated {
			note("the lab's consumers call GitHub with the token in secret %s/%s, not on this window; %s proceeds", platformNamespace, gitHubTokenSecret, what)
			return nil
		}
	}
	wait := max(w.Reset.Sub(gitHubNow())+5*time.Second, 0)
	if wait > gitHubWindowMaxWait {
		wait = gitHubWindowMaxWait
	}
	note("the window holds fewer than the %d requests %s spends — waiting %s for it to reset at %s (one bounded wait; export $%s to lift it to 5000 an hour)", need, what, wait.Round(time.Second), w.Reset.Local().Format("15:04:05 MST"), GitHubTokenEnv)
	gitHubSleep(wait)
	if w, err = readGitHubWindow(); err != nil {
		note("GitHub API window not re-read after the wait (%v); %s proceeds", err, what)
		return nil
	}
	note("%s", w)
	if w.Remaining < need {
		return fmt.Errorf("GitHub API window still short after waiting for its reset (%s): %d of the %d requests %s spends — export $%s or try again after %s", w.Reset.Local().Format("15:04:05 MST"), w.Remaining, need, what, GitHubTokenEnv, w.Reset.Local().Format("15:04:05 MST"))
	}
	return nil
}
