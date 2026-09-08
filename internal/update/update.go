// Package update keeps the agentlab binary current. `agentlab self-update`
// installs the newest GitHub release over the running executable, the way
// muster and mcp-kubernetes do it (creativeprojects/go-selfupdate against the
// repository's releases; a development build is refused). Remind prints a
// one-line hint ahead of every command while a newer release exists — the
// per-command check devctl runs, with two deliberate differences: it never
// blocks (an outdated lab still runs), and it gives up fast when GitHub cannot
// be reached, so a machine without internet is never held up. GitHub's answer
// is remembered under the user's cache directory for an hour (a failed attempt
// for ten minutes), so the round trip is rare, and capped at two seconds when
// it happens.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/creativeprojects/go-selfupdate"

	"github.com/giantswarm/agentlab/pkg/project"
)

// Repository is the GitHub repository whose releases carry the binaries. Its
// assets are named agentlab-<os>-<arch> (agentlab-windows-<arch>.exe), the
// shape go-selfupdate picks out for the running OS and architecture by
// itself; the .bundle signatures next to them never match.
const Repository = "giantswarm/agentlab"

// OptOutEnv silences the newer-release hint when set to any value. The
// self-update command ignores it.
const OptOutEnv = "AGENTLAB_NO_UPDATE_CHECK"

const (
	// cacheTTL is how long GitHub's answer stands before the hint asks
	// again; retryAfterFailure how long a failed attempt keeps it from
	// trying (offline for the afternoon: one short wait every ten minutes,
	// not one per command).
	cacheTTL          = time.Hour
	retryAfterFailure = 10 * time.Minute

	// selfUpdateTimeout bounds finding the latest release for the command;
	// the download itself runs until done or interrupted.
	selfUpdateTimeout = 15 * time.Second

	cacheFile = "latest-release.json"
)

// remindTimeout caps the GitHub round trip behind the hint: no route, a
// resolver that never answers, a captive portal that swallows the request —
// the command starts after at most this long, without the hint. A variable so
// the tests need not wait for it.
var remindTimeout = 2 * time.Second

// ErrOutdated is what `agentlab self-update --check` returns when a newer
// release exists; main turns it into exit status 125, devctl's convention for
// `version check`.
var ErrOutdated = errors.New("a newer agentlab release is available")

// Seams the tests replace: the release source (GitHub), the cache location
// ("" disables the cache), the clock, the executable to replace and the
// running version.
var (
	newSource = func() selfupdate.Source {
		// The error is for GitHub Enterprise URLs only; nil makes NewUpdater
		// build the same public-GitHub source itself. A GITHUB_TOKEN in the
		// environment lifts the anonymous rate limit, nothing else changes.
		src, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{})
		if err != nil {
			return nil
		}
		return src
	}
	cacheDir = func() string {
		dir, err := os.UserCacheDir()
		if err != nil {
			return ""
		}
		return filepath.Join(dir, project.Name)
	}
	now            = time.Now
	executable     = selfupdate.ExecutablePath
	currentVersion = project.Version
)

// Remind writes a hint on w when a release newer than the running binary
// exists, and nothing otherwise — never an error, never a wait worth
// noticing: the lab works the same on an old binary. Quiet for a build that is
// no release ("dev"), under OptOutEnv, and whenever GitHub cannot say within
// remindTimeout (offline, rate-limited, no binary for this platform).
func Remind(ctx context.Context, w io.Writer) {
	if os.Getenv(OptOutEnv) != "" {
		return
	}
	current := currentVersion()
	if _, err := semver.NewVersion(current); err != nil {
		// "dev": nothing to compare with, and nothing worth a round trip.
		return
	}
	latest := rememberedLatest(ctx)
	if latest == "" || !newer(latest, current) {
		return
	}
	_, _ = fmt.Fprintf(w, "A newer agentlab is available: %s (this is %s).\n", latest, current)
	_, _ = fmt.Fprintf(w, "  Update with `agentlab self-update`; %s=1 silences this hint.\n\n", OptOutEnv)
}

// Run is `agentlab self-update`. It looks up the newest release and, unless
// checkOnly, downloads the binary for this OS and architecture over the
// running executable — the flow muster and mcp-kubernetes ship. Progress goes
// to w. A development build is refused: "dev" compares with nothing and was
// not installed from a release to begin with. With checkOnly the versions are
// reported and the result is ErrOutdated when a newer release exists, nil when
// nothing newer is out.
func Run(ctx context.Context, w io.Writer, checkOnly bool) error {
	current := currentVersion()
	if _, err := semver.NewVersion(current); err != nil {
		return fmt.Errorf("cannot self-update a development build (version %s): install a release with `go install github.com/giantswarm/agentlab@latest` or from %s/releases", current, project.Source)
	}
	_, _ = fmt.Fprintf(w, "Current version: %s\n", current)
	_, _ = fmt.Fprintf(w, "Looking up the latest release of %s...\n", Repository)
	lookup, cancel := context.WithTimeout(ctx, selfUpdateTimeout)
	up, rel, found, err := detect(lookup)
	cancel()
	if err != nil {
		return fmt.Errorf("looking up the latest release of %s: %w (is this machine online?)", Repository, err)
	}
	if !found {
		return fmt.Errorf("no release of %s carries a binary for %s/%s", Repository, runtime.GOOS, runtime.GOARCH)
	}
	latest := tag(rel)
	// The next hint agrees with what the command just learned, without a
	// round trip of its own.
	store(cache{CheckedAt: now(), Latest: latest})
	if !newer(latest, current) {
		_, _ = fmt.Fprintf(w, "Nothing newer than %s on GitHub (latest release %s).\n", current, latest)
		return nil
	}
	_, _ = fmt.Fprintf(w, "Newer release: %s (published %s)\n  %s\n", latest, rel.PublishedAt.Format(time.RFC3339), rel.URL)
	if checkOnly {
		return ErrOutdated
	}
	exe, err := executable()
	if err != nil {
		return fmt.Errorf("locating the running executable: %w", err)
	}
	_, _ = fmt.Fprintf(w, "Updating %s to %s...\n", exe, latest)
	if err := up.UpdateTo(ctx, rel, exe); err != nil {
		return fmt.Errorf("updating %s: %w", exe, err)
	}
	_, _ = fmt.Fprintf(w, "Updated to %s.\n", latest)
	return nil
}

// detect asks GitHub for the newest release with a binary for this platform
// (found is false when none has one). The updater comes back with the release
// because the download has to go through the same source.
func detect(ctx context.Context) (up *selfupdate.Updater, rel *selfupdate.Release, found bool, err error) {
	up, err = selfupdate.NewUpdater(selfupdate.Config{Source: newSource()})
	if err != nil {
		return nil, nil, false, err
	}
	rel, found, err = up.DetectLatest(ctx, selfupdate.ParseSlug(Repository))
	return up, rel, found, err
}

// tag is the version in the form the tags and pkg/project use ("v0.19.3");
// go-selfupdate reports it without the v.
func tag(rel *selfupdate.Release) string {
	return "v" + rel.Version()
}

// newer reports whether latest is a higher version than current. Current is
// what pkg/project reports: a tag ("v0.19.2"), a tag with local edits
// ("v0.19.2+dirty", which is the tag), a Go pseudo-version between tags
// ("v0.19.3-0.20260908093000-8536d36c1a2b": after v0.19.2, before v0.19.3) —
// or "dev", which, like anything semver cannot read, is never outdated.
func newer(latest, current string) bool {
	l, err := semver.NewVersion(latest)
	if err != nil {
		return false
	}
	c, err := semver.NewVersion(current)
	if err != nil {
		return false
	}
	return l.GreaterThan(c)
}

// rememberedLatest is the newest release version: from the cache while it is
// current, else from GitHub within remindTimeout — "" when neither knows.
func rememberedLatest(ctx context.Context) string {
	c := load()
	at := now()
	if c.current(at) {
		return c.Latest
	}
	ctx, cancel := context.WithTimeout(ctx, remindTimeout)
	defer cancel()
	_, rel, found, err := detect(ctx)
	switch {
	case err != nil:
		// Keep the previous answer, if any: it is still the best this
		// machine knows, and there is no point asking again right away.
		c.FailedAt = at
	case !found:
		c = cache{CheckedAt: at}
	default:
		c = cache{CheckedAt: at, Latest: tag(rel)}
	}
	store(c)
	return c.Latest
}

// cache is what the hint remembers between commands, as JSON under the user's
// cache directory: GitHub's last answer and when it came, or when the last
// attempt got none.
type cache struct {
	// CheckedAt is when Latest was fetched; zero when it never was.
	CheckedAt time.Time `json:"checkedAt"`
	// Latest is the newest release GitHub reported ("v0.19.3"); "" when no
	// release carries a binary for this platform.
	Latest string `json:"latest"`
	// FailedAt is the last attempt that got no answer; zero when the last
	// attempt succeeded.
	FailedAt time.Time `json:"failedAt,omitempty"`
}

// current says whether the cache still answers on its own: fetched within
// cacheTTL, or failed within retryAfterFailure — then Latest (the previous
// answer, or nothing) is the best this machine knows and GitHub is left
// alone.
func (c cache) current(at time.Time) bool {
	return (!c.CheckedAt.IsZero() && at.Sub(c.CheckedAt) < cacheTTL) ||
		(!c.FailedAt.IsZero() && at.Sub(c.FailedAt) < retryAfterFailure)
}

// cachePath is the cache file, "" when there is no cache directory.
func cachePath() string {
	dir := cacheDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, cacheFile)
}

// load reads the cache; a missing or unreadable file is an empty cache.
func load() cache {
	path := cachePath()
	if path == "" {
		return cache{}
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return cache{}
	}
	var c cache
	if err := json.Unmarshal(data, &c); err != nil {
		return cache{}
	}
	return c
}

// store writes the cache atomically — a sibling temp file, then a rename — so
// two agentlab commands running at once never leave a torn file behind.
// Failing to write is failing to remember, nothing worse.
func store(c cache) {
	path := cachePath()
	if path == "" {
		return
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), cacheFile+".*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}
