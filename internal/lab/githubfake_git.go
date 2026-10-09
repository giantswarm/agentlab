package lab

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The workspace fake's repositories are bare git repositories on disk, served
// over git's smart HTTP by git's own upload-pack and receive-pack in their
// stateless RPC mode — what `git http-backend` runs, without needing that
// optional program (Alpine's git leaves it to another package) — so a
// clone, a fetch and a push are git's, byte for byte; the fake only checks
// the credential first.

// The services of git's smart HTTP: a clone or fetch, and a push.
const (
	gitUploadPack  = "git-upload-pack"
	gitReceivePack = "git-receive-pack"
)

// githubGit holds the bare repositories under root/<owner>/<name>.git.
type githubGit struct {
	binary, root, home string
}

func newGitHubGit(dataDir string) (*githubGit, error) {
	binary, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("the fake GitHub serves git with git itself, and git is not on PATH: %w", err)
	}
	g := &githubGit{binary: binary, root: filepath.Join(dataDir, "repositories"), home: filepath.Join(dataDir, "home")}
	for _, dir := range []string{g.root, g.home} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// env is git's environment: no system or user configuration of the host.
func (g *githubGit) env(extra ...string) []string {
	return append([]string{"HOME=" + g.home, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "PATH=" + os.Getenv("PATH")}, extra...)
}

func (g *githubGit) run(dir string, args ...string) (string, error) {
	cmd := exec.Command(g.binary, args...) // #nosec G204 -- git with the fake's own arguments
	cmd.Dir, cmd.Env = dir, g.env()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, excerpt(out.String(), 300))
	}
	return strings.TrimSpace(out.String()), nil
}

func (g *githubGit) path(fullName string) string { return filepath.Join(g.root, fullName+".git") }

// seed creates repo as a bare repository with one commit on its default
// branch holding its files and GeneratedMiB of incompressible content
// (seeded by the name, so every lab clones the same bytes), and returns its
// size in KiB, the unit of GitHub's `size`.
func (g *githubGit) seed(repo githubFixtureRepo) (int, error) {
	bare := g.path(repo.fullName())
	if _, err := os.Stat(bare); err == nil {
		return dirKiB(bare), nil
	}
	work, err := os.MkdirTemp(g.home, "seed-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	for p, content := range repo.Files {
		if err := writeSeedFile(work, p, []byte(content)); err != nil {
			return 0, err
		}
	}
	rng := rand.NewChaCha8(nameSeed(repo.fullName()))
	for i := range repo.GeneratedMiB {
		chunk := make([]byte, 1<<20)
		_, _ = rng.Read(chunk)
		if err := writeSeedFile(work, fmt.Sprintf("data/chunk-%04d.bin", i), chunk); err != nil {
			return 0, err
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", repo.DefaultBranch},
		{"add", "-A"},
		{"-c", "user.name=agentlab", "-c", "user.email=github-fake@lab.local", "commit", "-q", "-m", "Seed " + repo.fullName()},
		{"clone", "-q", "--bare", work, bare},
	} {
		if _, err := g.run(work, args...); err != nil {
			return 0, fmt.Errorf("seeding %s: %w", repo.fullName(), err)
		}
	}
	return dirKiB(bare), nil
}

func writeSeedFile(root, p string, content []byte) error {
	full := filepath.Join(root, filepath.FromSlash(p))
	if !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return fmt.Errorf("file %q leaves the repository", p)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return err
	}
	return os.WriteFile(full, content, 0o600)
}

// nameSeed is a ChaCha8 seed from a name.
func nameSeed(s string) [32]byte {
	var seed [32]byte
	copy(seed[:], s)
	return seed
}

// dirKiB is the disk size of dir's files in KiB.
func dirKiB(dir string) int {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, ierr := d.Info(); ierr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return int((total + 1023) / 1024)
}

// branchSHA is the commit a branch of the repository points at, "" for no
// such branch.
func (g *githubGit) branchSHA(fullName, branch string) string {
	sha, err := g.run(g.path(fullName), "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	return sha
}

// gitService splits a smart HTTP path, /<owner>/<repo>[.git]/<rest>, into
// the repository, the service and whether the request is the service's
// advertisement (GET info/refs) rather than its RPC (POST /<service>); ok
// false for a path that is not git's.
func gitService(r *http.Request) (fullName, service string, advertise, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	if len(parts) != 3 {
		return "", "", false, false
	}
	switch {
	case parts[2] == "info/refs" && r.Method == http.MethodGet:
		service, advertise = r.URL.Query().Get("service"), true
	case r.Method == http.MethodPost:
		service = parts[2]
	}
	if service != gitUploadPack && service != gitReceivePack {
		return "", "", false, false
	}
	return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git"), service, advertise, true
}

// serveGit admits a smart HTTP request as GitHub does and hands it to git:
// a public repository is cloned by anyone; a private one answers 401 (git
// then asks for credentials) without a credential and 404 to one that does
// not read it (an invalid credential got its 401 already); a push needs
// write — 401 without a credential, 403 naming the repository and the user
// otherwise. A push that went through moves pushed_at.
func (g *workspaceGitHub) serveGit(w http.ResponseWriter, r *http.Request) {
	fullName, service, advertise, ok := gitService(r)
	if !ok {
		githubFakeError(w, http.StatusNotFound, "Not Found")
		return
	}
	push := service == gitReceivePack
	c := callerOf(r)
	g.mu.Lock()
	repo, known := g.repos[fullName]
	readable := known && g.canRead(c, repo)
	writable := known && g.canWrite(c, repo)
	g.mu.Unlock()
	anonymous := c.kind != credUser && c.kind != credInstallation
	switch {
	case !known:
		http.Error(w, "Repository not found.", http.StatusNotFound)
		return
	case anonymous && (push || !readable):
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		http.Error(w, "Authentication required.", http.StatusUnauthorized)
		return
	case !readable:
		http.Error(w, "Repository not found.", http.StatusNotFound)
		return
	case push && repo.Archived:
		http.Error(w, fmt.Sprintf("This repository (%s) was archived so it is read-only.", fullName), http.StatusForbidden)
		return
	case push && !writable:
		http.Error(w, fmt.Sprintf("Permission to %s.git denied to %s.", fullName, c.name()), http.StatusForbidden)
		return
	}
	err := g.git.serveService(w, r, fullName, service, advertise)
	if err != nil {
		fmt.Printf("git %s %s: %v\n", service, fullName, err)
		return
	}
	if push && !advertise {
		g.mu.Lock()
		repo.pushedAt = g.now().UTC()
		g.mu.Unlock()
	}
}

// serveService runs `git <service> --stateless-rpc` on the repository the
// way git http-backend does: the advertisement of its refs, opened by the
// service line in protocol v0 and v1, or the RPC with the request body
// (gzip-encoded when the client says so) on its stdin. The client's
// Git-Protocol header reaches git as GIT_PROTOCOL.
func (g *githubGit) serveService(w http.ResponseWriter, r *http.Request, fullName, service string, advertise bool) error {
	args := []string{strings.TrimPrefix(service, "git-"), "--stateless-rpc"}
	var stdin io.Reader = http.NoBody
	if advertise {
		args = append(args, "--advertise-refs")
	} else {
		stdin = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, "the request body is not gzip", http.StatusBadRequest)
				return err
			}
			defer func() { _ = gz.Close() }()
			stdin = gz
		}
	}
	cmd := exec.CommandContext(r.Context(), g.binary, append(args, g.path(fullName))...) // #nosec G204 -- git with the fake's own arguments
	cmd.Env = g.env("GIT_PROTOCOL=" + r.Header.Get("Git-Protocol"))
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	w.Header().Set("Cache-Control", "no-cache")
	if advertise {
		w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
		if !strings.Contains(r.Header.Get("Git-Protocol"), "version=2") {
			_, _ = io.WriteString(w, pktLine("# service="+service+"\n")+"0000")
		}
	} else {
		w.Header().Set("Content-Type", "application/x-"+service+"-result")
	}
	cmd.Stdout = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, excerpt(stderr.String(), 300))
	}
	return nil
}

// pktLine is s as one of git's pkt-lines: its length in four hex digits,
// the length included, then s.
func pktLine(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
