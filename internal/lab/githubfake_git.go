package lab

import (
	"bytes"
	"cmp"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"net/http/cgi" // #nosec G504 -- Go 1.26: the Httpoxy fix has been in since 1.6.3
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The workspace fake's repositories are bare git repositories on disk, served
// by `git http-backend` (git's own smart HTTP), so a clone, a fetch and a
// push are git's, byte for byte; the fake only checks the credential first.

// gitReceivePack is the service of a push.
const gitReceivePack = "git-receive-pack"

// githubGit holds the bare repositories under root/<owner>/<name>.git.
type githubGit struct {
	binary, root, home string
}

func newGitHubGit(dataDir string) (*githubGit, error) {
	binary, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("the fake GitHub serves git through `git http-backend`, and git is not on PATH: %w", err)
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
// the repository and whether the request pushes; ok false for a path that is
// not git's.
func gitService(r *http.Request) (fullName, rest string, push, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	if len(parts) != 3 {
		return "", "", false, false
	}
	switch parts[2] {
	case "info/refs":
		push = r.URL.Query().Get("service") == gitReceivePack
	case "git-upload-pack":
	case gitReceivePack:
		push = true
	default:
		return "", "", false, false
	}
	return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git"), parts[2], push, true
}

// serveGit admits a smart HTTP request as GitHub does and hands it to `git
// http-backend`: a public repository is cloned by anyone; a private one
// answers 401 (git then asks for credentials) without a credential and 404
// to one that does not read it (an invalid credential got its 401 already); a push needs write — 401 without a
// credential, 403 naming the repository and the user otherwise. A push
// that went through moves pushed_at.
func (g *workspaceGitHub) serveGit(w http.ResponseWriter, r *http.Request) {
	fullName, rest, push, ok := gitService(r)
	if !ok {
		githubFakeError(w, http.StatusNotFound, "Not Found")
		return
	}
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
	backend := &cgi.Handler{
		Path: g.git.binary, Args: []string{"http-backend"}, Dir: g.git.root,
		Env: g.git.env("GIT_PROJECT_ROOT="+g.git.root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER="+cmp.Or(c.name(), "anonymous")),
	}
	backendReq := r.Clone(r.Context())
	backendReq.URL.Path = "/" + fullName + ".git/" + rest
	sw := &statusWriter{ResponseWriter: w}
	backend.ServeHTTP(sw, backendReq)
	if push && rest == gitReceivePack && sw.status == http.StatusOK {
		g.mu.Lock()
		repo.pushedAt = g.now().UTC()
		g.mu.Unlock()
	}
}
