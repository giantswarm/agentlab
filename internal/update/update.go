// Package update keeps the agentlab binary current. `agentlab self-update`
// installs the newest GitHub release over the running executable, the way
// muster and mcp-kubernetes do it (giantswarm/go-selfupdate against the
// repository's releases; a development build is refused) — and only after the
// release's cosign Sigstore bundle verifies: architect signs every binary it
// publishes in CircleCI, keyless, and the shared validator
// (github.com/giantswarm/selfupdate-cosign) checks the download against that
// signature before anything is written. A release without a bundle, or a
// download that does not match its bundle, is refused and the installed
// binary stays as it is. Remind prints a one-line hint ahead of every command
// while a newer release exists — the per-command check devctl runs, with two
// deliberate differences: it never blocks (an outdated lab still runs), and it
// gives up fast when GitHub cannot be reached, so a machine without internet
// is never held up. GitHub's answer is remembered under the user's cache
// directory for an hour (a failed attempt for ten minutes), so the round trip
// is rare, and capped at two seconds when it happens. The hint installs
// nothing, so it does not ask for a bundle. Release candidates (GitHub
// prereleases, v0.97.0-rc.1) are left out unless self-update is told to follow
// them (Options.Pre): then the newest of the release candidates and the stable
// releases is installed, in semver order (v0.96.0-rc.6 < v0.96.0 <
// v0.97.0-rc.1), verified like any release. The hint stays on stable releases
// and its cache holds only their answer.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/giantswarm/go-selfupdate"
	selfupdatecosign "github.com/giantswarm/selfupdate-cosign"
	"github.com/google/go-github/v86/github"

	"github.com/giantswarm/agentlab/internal/xdg"
	"github.com/giantswarm/agentlab/pkg/project"
)

// Repository is the GitHub repository whose releases carry the binaries. Its
// assets are named agentlab-<os>-<arch> (agentlab-windows-<arch>.exe), the
// shape go-selfupdate picks out for the running OS and architecture by
// itself, each next to its cosign Sigstore bundle (agentlab-<os>-<arch>.bundle)
// — the signature self-update verifies the download against.
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

// The environment variables a GitHub token is read from, in gh's order:
// GH_TOKEN wins over GITHUB_TOKEN.
const (
	ghTokenEnv     = "GH_TOKEN"     // #nosec G101 -- variable NAME, not a credential
	githubTokenEnv = "GITHUB_TOKEN" // #nosec G101 -- variable NAME, not a credential
)

// ghAuthTokenSource names a token gh keeps for its signed-in account.
const ghAuthTokenSource = "`gh auth token`" // #nosec G101 -- a command, not a credential

// Seams the tests replace: the release source (GitHub, read with the token
// it is handed, anonymously without one) and the API it asks, the token gh
// keeps, the signature check (the shared cosign validator for releases of
// Repository: it pins the CircleCI issuer, a CircleCI pipeline as the subject
// and this repository as the source, against the Sigstore public-good trust
// root), the cache location ("" disables the cache), the clock, the
// executable to replace, the folder it settles in and the running version.
var (
	newSource    = githubReleases
	githubAPIURL = ""
	ghAuthToken  = func(ctx context.Context) string {
		if _, err := exec.LookPath("gh"); err != nil {
			return ""
		}
		out, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	newValidator = func() selfupdate.Validator { return selfupdatecosign.New(Repository) }
	cacheDir     = func() string {
		dir, err := xdg.CacheHome()
		if err != nil {
			return ""
		}
		return dir
	}
	binHome        = xdg.BinHome
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

// Options are the flags of `agentlab self-update`.
type Options struct {
	// Check only reports the versions; ErrOutdated when a newer release
	// exists.
	Check bool
	// Pre follows release candidates: a prerelease counts as a release, so
	// an rc binary moves to the newest rc, or to the stable release after it.
	Pre bool
}

// Run is `agentlab self-update`. It looks up the newest release (release
// candidates included with opts.Pre) and, unless opts.Check, downloads the binary for this OS and architecture and its
// Sigstore bundle, verifies the one against the other, and only then writes
// it over the running executable — the flow muster and mcp-kubernetes ship —
// which then settles in ~/.local/bin (settle), also when nothing is newer.
// Progress goes to w. A development build is refused: "dev" compares with
// nothing and was not installed from a release to begin with. A release
// without a bundle for this platform's binary is refused before anything is
// downloaded, a download that does not verify before anything is written;
// both leave the executable as it is. With opts.Check the versions are
// reported and the result is ErrOutdated when a newer release exists, nil when
// nothing newer is out — and the same refusal when the newest release has no
// bundle, since self-update would install nothing from it.
func Run(ctx context.Context, w io.Writer, opts Options) error {
	current := currentVersion()
	cur, err := semver.NewVersion(current)
	if err != nil {
		return fmt.Errorf("cannot self-update a development build (version %s): install a release with `go install github.com/giantswarm/agentlab@latest` or from %s/releases", current, project.Source)
	}
	_, _ = fmt.Fprintf(w, "Current version: %s\n", current)
	which := "release"
	if opts.Pre {
		which = "release or release candidate"
	}
	lookup, cancel := context.WithTimeout(ctx, selfUpdateTimeout)
	cred := credential(lookup)
	as := "anonymously"
	if cred.source != "" {
		as = "with the token from " + cred.source
	}
	_, _ = fmt.Fprintf(w, "Looking up the latest %s of %s %s...\n", which, Repository, as)
	up, rel, found, err := detect(lookup, cred, newValidator(), opts.Pre)
	cancel()
	if errors.Is(err, selfupdate.ErrValidationAssetNotFound) {
		// go-selfupdate found the newest release with a binary for this
		// platform but no bundle next to it; nothing has been downloaded.
		return fmt.Errorf("the latest %s of %s has no signature bundle for this platform's binary, so it cannot be verified; refusing to install it: %w", which, Repository, err)
	}
	if err != nil {
		return lookupFailed(err, cred)
	}
	if !found {
		return fmt.Errorf("no release of %s carries a binary for %s/%s", Repository, runtime.GOOS, runtime.GOARCH)
	}
	latest := tag(rel)
	if !opts.Pre {
		// The next hint agrees with what the command just learned, without
		// a round trip of its own. An answer with release candidates is
		// not the hint's, which names stable releases only.
		store(cache{CheckedAt: now(), Latest: latest})
	}
	if !newer(latest, current) {
		_, _ = fmt.Fprintf(w, "Nothing newer than %s on GitHub (latest %s %s).\n", current, which, latest)
		if cur.Prerelease() != "" && !opts.Pre {
			_, _ = fmt.Fprintf(w, "%s is a release candidate: `agentlab self-update --pre` follows release candidates too.\n", current)
		}
		if opts.Check {
			return nil
		}
		exe, err := executable()
		if err != nil {
			return fmt.Errorf("locating the running executable: %w", err)
		}
		return settle(w, exe)
	}
	_, _ = fmt.Fprintf(w, "Newer %s: %s (published %s)\n  %s\n", which, latest, rel.PublishedAt.Format(time.RFC3339), rel.URL)
	if opts.Check {
		return ErrOutdated
	}
	exe, err := executable()
	if err != nil {
		return fmt.Errorf("locating the running executable: %w", err)
	}
	_, _ = fmt.Fprintf(w, "Updating %s to %s...\n", exe, latest)
	// Downloads the binary and its bundle, verifies, then renames it over the
	// file, symbolic links resolved, in a single step: an agentlab started
	// meanwhile runs the old binary or the new one, and several updates may
	// run at once. A failed verification leaves the file untouched.
	if err := selfupdatecosign.Install(ctx, up, rel, exe); err != nil {
		return fmt.Errorf("updating %s failed, it is unchanged: %w", exe, err)
	}
	_, _ = fmt.Fprintf(w, "Verified the signature and updated to %s.\n", latest)
	return settle(w, exe)
}

// settle moves the installed binary at exe into the freedesktop folder of
// the user's executables (xdg.BinHome, ~/.local/bin) when it lives anywhere
// else, and leaves a link at exe: whichever of the two folders comes first
// on PATH, `agentlab` runs the one binary self-update keeps current. A file
// already in that folder is replaced by the rename, in one step. On
// Windows, where a link needs a privilege, the binary stays where it is.
func settle(w io.Writer, exe string) error {
	if runtime.GOOS == goosWindows {
		return nil
	}
	bin, err := binHome()
	if err != nil {
		return fmt.Errorf("locating the folder of your executables: %w", err)
	}
	from, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	if realBin, err := filepath.EvalSymlinks(bin); err == nil && filepath.Dir(from) == realBin {
		return nil
	}
	to := filepath.Join(bin, project.Name)
	if err := os.MkdirAll(bin, 0o750); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("moving %s to %s (it stays where it is): %w", from, to, err)
	}
	if err := os.Symlink(to, from); err != nil {
		return fmt.Errorf("moved %s to %s, but the link at the old path is missing: %w", from, to, err)
	}
	_, _ = fmt.Fprintf(w, "Installed at %s; %s links to it.\n", to, from)
	if !onPath(bin) {
		_, _ = fmt.Fprintf(w, "  %s is not on your PATH: add it, so `agentlab` finds it there.\n", bin)
	}
	return nil
}

// goosWindows is runtime.GOOS on Windows.
const goosWindows = "windows"

// onPath reports whether dir is one of PATH's folders.
func onPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

// detect asks GitHub for the newest release with a binary for this platform
// (found is false when none has one), read with cred's token, prereleases
// included when pre. The updater comes back with the release
// because the download has to go through the same source. With a validator,
// the release must also carry the validator's bundle for that binary — else
// the error wraps selfupdate.ErrValidationAssetNotFound — and Install checks
// the download against it; nil asks for no bundle, for a caller that installs
// nothing.
func detect(ctx context.Context, cred githubCredential, validator selfupdate.Validator, pre bool) (up *selfupdate.Updater, rel *selfupdate.Release, found bool, err error) {
	up, err = selfupdate.NewUpdater(selfupdate.Config{Source: newSource(cred.token), Validator: validator, Prerelease: pre})
	if err != nil {
		return nil, nil, false, err
	}
	rel, found, err = up.DetectLatest(ctx, selfupdate.ParseSlug(Repository))
	return up, rel, found, err
}

// githubReleases is the GitHub releases source, authenticated with token
// when it is not empty: 5,000 calls an hour for the token's account instead
// of the 60 an hour GitHub grants every anonymous caller behind one address.
func githubReleases(token string) selfupdate.Source {
	src, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{APIToken: token, EnterpriseBaseURL: githubAPIURL})
	if err != nil {
		// Only an unparsable API URL fails; nil makes NewUpdater build the
		// public-GitHub source itself.
		return nil
	}
	return src
}

// githubCredential is the token the releases are read with and where it
// came from: GH_TOKEN, GITHUB_TOKEN, else the token gh keeps for its
// signed-in account. Both are empty for an anonymous call.
type githubCredential struct {
	token, source string
}

func credential(ctx context.Context) githubCredential {
	for _, env := range []string{ghTokenEnv, githubTokenEnv} {
		if token := os.Getenv(env); token != "" {
			return githubCredential{token: token, source: env}
		}
	}
	if token := ghAuthToken(ctx); token != "" {
		return githubCredential{token: token, source: ghAuthTokenSource}
	}
	return githubCredential{}
}

// lookupFailed words a failed release lookup. A spent rate limit says
// whether the call was anonymous, and how to make it authenticated, or
// which token's limit is spent.
func lookupFailed(err error, cred githubCredential) error {
	var limit *github.RateLimitError
	if !errors.As(err, &limit) {
		return fmt.Errorf("looking up the latest release of %s: %w (is this machine online?)", Repository, err)
	}
	until := ""
	if reset := limit.Rate.Reset.Time; !reset.IsZero() {
		until = " until " + reset.Local().Format(time.RFC3339)
	}
	if cred.source == "" {
		return fmt.Errorf("looking up the latest release of %s: the call was anonymous, and GitHub's anonymous rate limit (60 calls an hour, shared by every machine behind this address) is spent%s; set %s or %s, or sign in with `gh auth login`, to read the releases with your token: %w", Repository, until, ghTokenEnv, githubTokenEnv, err)
	}
	return fmt.Errorf("looking up the latest release of %s: the rate limit of the token from %s is spent%s: %w", Repository, cred.source, until, err)
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
// current, else from GitHub within remindTimeout — "" when neither knows. The
// hint installs nothing, so it asks for no bundle: a release self-update would
// refuse is still a newer release worth knowing about.
func rememberedLatest(ctx context.Context) string {
	c := load()
	at := now()
	if c.current(at) {
		return c.Latest
	}
	ctx, cancel := context.WithTimeout(ctx, remindTimeout)
	defer cancel()
	_, rel, found, err := detect(ctx, credential(ctx), nil, false)
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
