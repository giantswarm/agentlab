package lab

import (
	"context"
	_ "embed"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// The platform manager's GitHub for pm-test (docs/platform-manager.md): the
// registry fixture's repositories (templates/platform-manager-fixture.yaml),
// read-only, in memory, on the REST paths giantswarm-platform-manager reads
// an installation through as the caller — the repository, a file's contents,
// the recursive tree at HEAD and the blobs by sha — with GET /user answering
// the login a bearer stands for (githubFakeLogin: the lab Dex token's e-mail,
// the token muster holds for the person through the manager's pinned
// authorization server). The fixture's repositories have no releases and no
// history beyond their one commit; every path the fake does not serve is
// answered 404 and kept in its request log, so a dry run that reads more
// than the fixture holds names what it missed.

//go:embed templates/platform-manager-fixture.yaml
var platformManagerFixture []byte

// registryFakeFlag is the github-fake flag that serves the fixture;
// registryFakeMissesPath the log of the paths it answered 404.
const (
	registryFakeFlag       = "--platform-manager"
	registryFakeMissesPath = "/_fake/misses"
	registryFakeBranch     = "main"
	githubFullName         = "full_name"
)

// registryFixture is the fixture: owner/name → path → content.
type registryFixture struct {
	Repositories map[string]map[string]string `yaml:"repositories"`
}

func loadRegistryFixture() (registryFixture, error) {
	var f registryFixture
	if err := yaml.Unmarshal(platformManagerFixture, &f); err != nil {
		return f, fmt.Errorf("the platform manager's registry fixture: %w", err)
	}
	if len(f.Repositories) == 0 {
		return f, fmt.Errorf("the platform manager's registry fixture holds no repository")
	}
	return f, nil
}

// registryRepo is one repository: its commit, tree and files by path.
type registryRepo struct {
	commit, tree string
	files        map[string]string // path → blob sha
}

// registryGitHub serves the fixture.
type registryGitHub struct {
	repos map[string]registryRepo
	blobs map[string][]byte

	mu     sync.Mutex
	misses []string
}

func newRegistryGitHub(f registryFixture) *registryGitHub {
	g := &registryGitHub{repos: map[string]registryRepo{}, blobs: map[string][]byte{}}
	for _, name := range slices.Sorted(maps.Keys(f.Repositories)) {
		repo := registryRepo{files: map[string]string{}}
		var tree strings.Builder
		for _, p := range slices.Sorted(maps.Keys(f.Repositories[name])) {
			content := []byte(f.Repositories[name][p])
			sha := gitObjectID("blob", content)
			g.blobs[sha] = content
			repo.files[p] = sha
			tree.WriteString(p + "\x00" + sha + "\n")
		}
		repo.tree = gitObjectID(gitTree, []byte(tree.String()))
		repo.commit = gitObjectID(gitCommit, []byte(name+"\n"+repo.tree))
		g.repos[name] = repo
	}
	return g
}

func (g *registryGitHub) handler() http.Handler {
	repoPath := githubFakeAPIPath + "/repos/{owner}/{repo}"
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+githubFakeHealthPath, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET "+registryFakeMissesPath, func(w http.ResponseWriter, _ *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		writeGitHubJSON(w, http.StatusOK, append([]string{}, g.misses...))
	})
	mux.HandleFunc("GET "+githubFakeAPIPath+"/user", func(w http.ResponseWriter, r *http.Request) {
		login := githubFakeLogin(r)
		if login == "" {
			githubFakeError(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		writeGitHubJSON(w, http.StatusOK, map[string]any{githubLogin: login, "id": len(login), fieldTypeKey: "User"})
	})
	mux.HandleFunc("GET "+repoPath, g.repoCall(func(w http.ResponseWriter, _ *http.Request, name string, _ registryRepo) {
		writeGitHubJSON(w, http.StatusOK, map[string]any{githubFullName: name, "default_branch": registryFakeBranch, githubPrivate: true})
	}))
	mux.HandleFunc("GET "+repoPath+"/contents/{path...}", g.repoCall(func(w http.ResponseWriter, r *http.Request, _ string, repo registryRepo) {
		p := r.PathValue("path")
		sha, ok := repo.files[p]
		if !ok {
			g.miss(w, r)
			return
		}
		writeGitHubJSON(w, http.StatusOK, blobJSON(map[string]any{fieldTypeKey: "file", pathArg: p}, sha, g.blobs[sha]))
	}))
	mux.HandleFunc("GET "+repoPath+"/git/trees/{ref}", g.repoCall(func(w http.ResponseWriter, r *http.Request, _ string, repo registryRepo) {
		if ref := r.PathValue("ref"); ref != "HEAD" && ref != registryFakeBranch && ref != repo.commit && ref != repo.tree {
			g.miss(w, r)
			return
		}
		entries := []map[string]any{}
		for _, p := range slices.Sorted(maps.Keys(repo.files)) {
			sha := repo.files[p]
			entries = append(entries, map[string]any{pathArg: p, "mode": "100644", fieldTypeKey: "blob", githubSHA: sha, "size": len(g.blobs[sha])})
		}
		w.Header().Set("ETag", `"`+repo.tree+`"`)
		writeGitHubJSON(w, http.StatusOK, map[string]any{githubSHA: repo.tree, gitTree: entries, "truncated": false})
	}))
	mux.HandleFunc("GET "+repoPath+"/git/blobs/{sha}", g.repoCall(func(w http.ResponseWriter, r *http.Request, _ string, repo registryRepo) {
		sha := r.PathValue(githubSHA)
		if !slices.Contains(slices.Collect(maps.Values(repo.files)), sha) {
			g.miss(w, r)
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "raw") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(g.blobs[sha])
			return
		}
		writeGitHubJSON(w, http.StatusOK, blobJSON(map[string]any{}, sha, g.blobs[sha]))
	}))
	// No release and no history beyond the one commit: empty lists, as
	// GitHub answers them for such a repository.
	for _, list := range []string{"/releases", "/commits"} {
		mux.HandleFunc("GET "+repoPath+list, g.repoCall(func(w http.ResponseWriter, _ *http.Request, _ string, _ registryRepo) {
			writeGitHubJSON(w, http.StatusOK, []any{})
		}))
	}
	mux.HandleFunc("/", g.miss)
	return mux
}

// repoCall admits a call with a bearer on a repository of the fixture; any
// other repository is 404, as GitHub answers one the caller cannot read.
func (g *registryGitHub) repoCall(h func(w http.ResponseWriter, r *http.Request, name string, repo registryRepo)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if githubFakeLogin(r) == "" {
			githubFakeError(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		name := r.PathValue("owner") + "/" + r.PathValue("repo")
		repo, ok := g.repos[name]
		if !ok {
			g.miss(w, r)
			return
		}
		h(w, r, name, repo)
	}
}

// miss answers 404 and records the request.
func (g *registryGitHub) miss(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.misses = append(g.misses, r.Method+" "+r.URL.RequestURI())
	g.mu.Unlock()
	githubFakeError(w, http.StatusNotFound, "Not Found")
}

// ServeRegistryGitHub serves the platform manager's registry fixture on addr
// until ctx ends: `agentlab github-fake --platform-manager`, what pm-test
// runs in a container on the kind network.
func ServeRegistryGitHub(ctx context.Context, addr string) error {
	f, err := loadRegistryFixture()
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("the platform manager's GitHub cannot listen on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: newRegistryGitHub(f).handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	fmt.Printf("the platform manager's GitHub on %s (%d repositories)\n", l.Addr(), len(f.Repositories))
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
