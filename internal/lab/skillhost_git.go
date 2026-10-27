package lab

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The skill host's repository is a bare git repository on disk, served over
// git's smart HTTP by git's own upload-pack in its stateless RPC mode — what
// `git http-backend` runs, without needing that optional program (Alpine's
// git leaves it to another package) — so a clone and a fetch are git's,
// byte for byte; the skill host only checks the credential first.

// gitUploadPack is the service of git's smart HTTP a clone or fetch calls.
const gitUploadPack = "git-upload-pack"

// gitServer holds bare repositories under root/<owner>/<name>.git.
type gitServer struct {
	binary, root, home string
}

func newGitServer(dataDir string) (*gitServer, error) {
	binary, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("the skill host serves git with git itself, and git is not on PATH: %w", err)
	}
	g := &gitServer{binary: binary, root: filepath.Join(dataDir, "repositories"), home: filepath.Join(dataDir, "home")}
	for _, dir := range []string{g.root, g.home} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// env is git's environment: no system or user configuration of the host.
func (g *gitServer) env(extra ...string) []string {
	return append([]string{"HOME=" + g.home, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "PATH=" + os.Getenv("PATH")}, extra...)
}

func (g *gitServer) run(dir string, args ...string) (string, error) {
	cmd := exec.Command(g.binary, args...) // #nosec G204 G702 -- git with the skill host's own arguments
	cmd.Dir, cmd.Env = dir, g.env()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, excerpt(out.String(), 300))
	}
	return strings.TrimSpace(out.String()), nil
}

func (g *gitServer) path(fullName string) string { return filepath.Join(g.root, fullName+".git") }

// gitFetchService splits a smart HTTP path, /<owner>/<repo>[.git]/<rest>,
// into the repository and whether the request is upload-pack's
// advertisement (GET info/refs) rather than its RPC (POST /git-upload-pack);
// ok false for a path that is no fetch.
func gitFetchService(r *http.Request) (fullName string, advertise, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	if len(parts) != 3 {
		return "", false, false
	}
	var service string
	switch {
	case parts[2] == "info/refs" && r.Method == http.MethodGet:
		service, advertise = r.URL.Query().Get("service"), true
	case r.Method == http.MethodPost:
		service = parts[2]
	}
	if service != gitUploadPack {
		return "", false, false
	}
	return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git"), advertise, true
}

// serveFetch runs `git upload-pack --stateless-rpc` on the repository the
// way git http-backend does: the advertisement of its refs, opened by the
// service line in protocol v0 and v1, or the RPC with the request body
// (gzip-encoded when the client says so) on its stdin. The client's
// Git-Protocol header reaches git as GIT_PROTOCOL.
func (g *gitServer) serveFetch(w http.ResponseWriter, r *http.Request, fullName string, advertise bool) error {
	args := []string{strings.TrimPrefix(gitUploadPack, "git-"), "--stateless-rpc"}
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
	cmd := exec.CommandContext(r.Context(), g.binary, append(args, g.path(fullName))...) // #nosec G204 G702 -- git with the skill host's own arguments
	cmd.Env = g.env("GIT_PROTOCOL=" + r.Header.Get("Git-Protocol"))
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	w.Header().Set("Cache-Control", "no-cache")
	if advertise {
		w.Header().Set("Content-Type", "application/x-"+gitUploadPack+"-advertisement")
		if !strings.Contains(r.Header.Get("Git-Protocol"), "version=2") {
			_, _ = io.WriteString(w, pktLine("# service="+gitUploadPack+"\n")+"0000")
		}
	} else {
		w.Header().Set("Content-Type", "application/x-"+gitUploadPack+"-result")
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
