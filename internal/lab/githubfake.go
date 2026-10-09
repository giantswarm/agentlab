package lab

import (
	"context"
	"crypto/sha1" // #nosec G505 -- git's object ids, not a security boundary
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The GitHub side of models-test's commit proof, headless.
//
// model-manager's commit mode opens a pull request as the person through
// GitHub's REST API (gitops-commit's GitHub remote, go-github against
// `--github-api-url`), and its App pin verifies the person's bearer with
// GET /user on the same API. fakeGitHub is that API for one run, in memory:
// one repository with a base branch the proof seeds, git's object model as
// far as the remote uses it (refs, commits, trees, blobs, the contents
// read, the merge of one branch into another that brings an existing branch
// up to date), pull requests, and GET /user. The login it answers is the local
// part of the e-mail the bearer carries: the lab's Dex access token, which
// muster obtained for the person through the registration's pinned
// authorization server — the same token model-manager then opens the pull
// request with. The fake reads the token's claims and does not verify its
// signature: model-manager validates the person's identity itself (the
// forwarded ID token), and the proof asserts the author is that person.
//
// The proof reads back what landed: every pull request with the files its
// head changes against its base (githubFakePullsPath).

// Where the fake serves: the REST API under GitHub Enterprise's prefix, the
// health, and the read-back.
const (
	githubFakeAPIPath    = "/api/v3"
	githubFakeHealthPath = "/healthz"
	githubFakePullsPath  = "/_fake/pulls"
	githubFakeCommand    = "github-fake"
)

// Words of git's object model and GitHub's JSON the fake speaks.
const (
	gitCommit     = "commit"
	githubBase64  = "base64"
	githubLogin   = "login"
	githubMessage = "message"
	githubRef     = "ref"
	githubSHA     = "sha"
	gitTree       = "tree"
)

// fakeGitHub is the GitHub REST API of one proof run.
type fakeGitHub struct {
	listener net.Listener
	server   *http.Server

	mu      sync.Mutex
	seq     int
	repo    string                       // owner/name
	refs    map[string]string            // branch → commit sha
	commits map[string]githubFakeCommit  // sha → commit
	trees   map[string]map[string]string // sha → path → blob sha
	blobs   map[string][]byte            // sha → content
	pulls   []*githubFakePull
}

// githubFakeCommit is one commit; MergeParent is the second parent of a
// merge commit, "" otherwise.
type githubFakeCommit struct {
	Tree, Parent, MergeParent, Message string
}

type githubFakePull struct {
	Number     int
	Title      string
	Body       string
	Head, Base string
	Author     string
	State      string
}

// githubFakePullView is a pull request as the proof reads it back: the files
// the head writes and the paths it removes, against the base.
type githubFakePullView struct {
	Number  int               `json:"number"`
	Title   string            `json:"title"`
	Body    string            `json:"body"`
	Head    string            `json:"head"`
	Base    string            `json:"base"`
	Author  string            `json:"author"`
	Files   map[string]string `json:"files"`
	Removed []string          `json:"removed"`
}

// startFakeGitHub serves the fake on addr (port 0 picks one) with repo
// (owner/name) at branch holding files.
func startFakeGitHub(addr, repo, branch string, files map[string][]byte) (*fakeGitHub, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("--repo %q is not owner/name", repo)
	}
	if branch == "" {
		return nil, fmt.Errorf("the fake GitHub needs a base branch")
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("the fake GitHub API cannot listen on %s: %w", addr, err)
	}
	f := &fakeGitHub{listener: l, repo: repo, refs: map[string]string{}, commits: map[string]githubFakeCommit{},
		trees: map[string]map[string]string{}, blobs: map[string][]byte{}}
	tree := map[string]string{}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		tree[p] = f.putBlob(files[p])
	}
	f.refs[branch] = f.putCommit(f.putTree(tree), "", "seed "+branch)

	repoPath := githubFakeAPIPath + "/repos/{owner}/{repo}"
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+githubFakeHealthPath, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("GET "+githubFakePullsPath, f.servePulls)
	mux.HandleFunc("GET "+githubFakeAPIPath+"/user", f.serveUser)
	mux.HandleFunc("GET "+repoPath, f.repoCall(f.getRepository))
	mux.HandleFunc("GET "+repoPath+"/git/ref/heads/{branch...}", f.repoCall(f.getRef))
	mux.HandleFunc("POST "+repoPath+"/git/refs", f.repoCall(f.createRef))
	mux.HandleFunc("PATCH "+repoPath+"/git/refs/heads/{branch...}", f.repoCall(f.updateRef))
	mux.HandleFunc("GET "+repoPath+"/git/commits/{sha}", f.repoCall(f.getCommit))
	mux.HandleFunc("POST "+repoPath+"/git/commits", f.repoCall(f.createCommit))
	mux.HandleFunc("POST "+repoPath+"/git/trees", f.repoCall(f.createTree))
	mux.HandleFunc("POST "+repoPath+"/git/blobs", f.repoCall(f.createBlob))
	mux.HandleFunc("GET "+repoPath+"/git/blobs/{sha}", f.repoCall(f.getBlob))
	mux.HandleFunc("GET "+repoPath+"/contents/{path...}", f.repoCall(f.getContents))
	mux.HandleFunc("POST "+repoPath+"/merges", f.repoCall(f.mergeBranch))
	mux.HandleFunc("POST "+repoPath+"/pulls", f.repoCall(f.createPull))
	mux.HandleFunc("GET "+repoPath+"/pulls", f.repoCall(f.listPulls))
	f.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = f.server.Serve(l) }()
	return f, nil
}

func (f *fakeGitHub) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = f.server.Shutdown(ctx)
}

// ServeFakeGitHub serves the fake GitHub REST API on addr until ctx ends:
// `agentlab github-fake`, what models-test runs in a container on the kind
// network. files are `<path>=<base64 content>` pairs of the base branch.
func ServeFakeGitHub(ctx context.Context, addr, repo, branch string, files []string) error {
	seed := make(map[string][]byte, len(files))
	for _, pair := range files {
		p, enc, ok := strings.Cut(pair, "=")
		if !ok || p == "" {
			return fmt.Errorf("--file %q is not <path>=<base64 content>", pair)
		}
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			return fmt.Errorf("--file %s: the content is not base64: %w", p, err)
		}
		seed[p] = raw
	}
	f, err := startFakeGitHub(addr, repo, branch, seed)
	if err != nil {
		return err
	}
	fmt.Printf("fake GitHub API on %s (%s@%s, %d files)\n", f.listener.Addr(), repo, branch, len(seed))
	<-ctx.Done()
	f.close()
	return nil
}

// --- git objects ------------------------------------------------------------

func gitObjectID(kind string, content []byte) string {
	h := sha1.New() // #nosec G401 -- git's object ids
	_, _ = fmt.Fprintf(h, "%s %d\x00", kind, len(content))
	_, _ = h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

func (f *fakeGitHub) putBlob(content []byte) string {
	sha := gitObjectID("blob", content)
	f.blobs[sha] = content
	return sha
}

func (f *fakeGitHub) putTree(tree map[string]string) string {
	var b strings.Builder
	for _, p := range slices.Sorted(maps.Keys(tree)) {
		b.WriteString(p + "\x00" + tree[p] + "\n")
	}
	sha := gitObjectID(gitTree, []byte(b.String()))
	f.trees[sha] = tree
	return sha
}

func (f *fakeGitHub) putCommit(tree, parent, message string) string {
	f.seq++
	sha := gitObjectID(gitCommit, []byte(tree+"\n"+parent+"\n"+message+"\n"+strconv.Itoa(f.seq)))
	f.commits[sha] = githubFakeCommit{Tree: tree, Parent: parent, Message: message}
	return sha
}

// ancestors is every commit reachable from sha, sha included, nearest
// first: the first parent and a merge's second parent alike.
func (f *fakeGitHub) ancestors(sha string) []string {
	var out []string
	seen := map[string]bool{}
	for queue := []string{sha}; len(queue) > 0; queue = queue[1:] {
		c := queue[0]
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
		queue = append(queue, f.commits[c].Parent, f.commits[c].MergeParent)
	}
	return out
}

// mergeTrees is the three-way merge of ours and theirs from their merge base
// ancestor; ok false when both sides changed one path differently.
func mergeTrees(ancestor, ours, theirs map[string]string) (map[string]string, bool) {
	out := map[string]string{}
	paths := maps.Clone(ours)
	maps.Copy(paths, theirs)
	maps.Copy(paths, ancestor)
	for p := range paths {
		o, t, a := ours[p], theirs[p], ancestor[p]
		var merged string
		switch {
		case o == t, t == a:
			merged = o
		case o == a:
			merged = t
		default:
			return nil, false
		}
		if merged != "" {
			out[p] = merged
		}
	}
	return out, true
}

// headTree is the tree at the head of branch; ok false for no such branch.
func (f *fakeGitHub) headTree(branch string) (map[string]string, bool) {
	sha, ok := f.refs[branch]
	if !ok {
		return nil, false
	}
	return f.trees[f.commits[sha].Tree], true
}

// refTree is the tree at ref, a branch or a commit sha as GitHub's ref
// query takes it; ok false for neither.
func (f *fakeGitHub) refTree(ref string) (map[string]string, bool) {
	if tree, ok := f.headTree(ref); ok {
		return tree, true
	}
	c, ok := f.commits[ref]
	if !ok {
		return nil, false
	}
	return f.trees[c.Tree], true
}

// --- the REST API -----------------------------------------------------------

// login is the GitHub login a bearer stands for: the local part of the
// e-mail its JWT claims carry, "" for no bearer or no e-mail.
func githubFakeLogin(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	local, _, _ := strings.Cut(claims.Email, "@")
	return local
}

func writeGitHubJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func githubFakeError(w http.ResponseWriter, status int, message string) {
	writeGitHubJSON(w, status, map[string]string{githubMessage: message})
}

func (f *fakeGitHub) serveUser(w http.ResponseWriter, r *http.Request) {
	login := githubFakeLogin(r)
	if login == "" {
		githubFakeError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	writeGitHubJSON(w, http.StatusOK, map[string]any{githubLogin: login, "id": len(login), fieldTypeKey: "User"})
}

// repoCall admits a call on the fake's repository with a bearer and runs h
// under the lock with the caller's login.
func (f *fakeGitHub) repoCall(h func(w http.ResponseWriter, r *http.Request, login string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		login := githubFakeLogin(r)
		if login == "" {
			githubFakeError(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		if r.PathValue("owner")+"/"+r.PathValue("repo") != f.repo {
			githubFakeError(w, http.StatusNotFound, "Not Found")
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		h(w, r, login)
	}
}

func refJSON(branch, sha string) map[string]any {
	return map[string]any{githubRef: "refs/heads/" + branch, "object": map[string]string{githubSHA: sha, fieldTypeKey: gitCommit}}
}

// getRepository is GET /repos/{owner}/{repo}: model-manager's check that the
// App's token reaches the repository before a commit; every other repository
// answers 404, as GitHub does for one the App is not installed on.
func (f *fakeGitHub) getRepository(w http.ResponseWriter, _ *http.Request, _ string) {
	writeGitHubJSON(w, http.StatusOK, map[string]any{"full_name": f.repo})
}

func (f *fakeGitHub) getRef(w http.ResponseWriter, r *http.Request, _ string) {
	branch := r.PathValue("branch")
	sha, ok := f.refs[branch]
	if !ok {
		githubFakeError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeGitHubJSON(w, http.StatusOK, refJSON(branch, sha))
}

func (f *fakeGitHub) createRef(w http.ResponseWriter, r *http.Request, _ string) {
	var in struct{ Ref, SHA string }
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		githubFakeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	branch, ok := strings.CutPrefix(in.Ref, "refs/heads/")
	if _, known := f.commits[in.SHA]; !ok || !known {
		githubFakeError(w, http.StatusUnprocessableEntity, "Reference update failed")
		return
	}
	if _, exists := f.refs[branch]; exists {
		githubFakeError(w, http.StatusUnprocessableEntity, "Reference already exists")
		return
	}
	f.refs[branch] = in.SHA
	writeGitHubJSON(w, http.StatusCreated, refJSON(branch, in.SHA))
}

func (f *fakeGitHub) updateRef(w http.ResponseWriter, r *http.Request, _ string) {
	branch := r.PathValue("branch")
	var in struct{ SHA string }
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		githubFakeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	head, ok := f.refs[branch]
	commit, known := f.commits[in.SHA]
	if !ok || !known || commit.Parent != head {
		githubFakeError(w, http.StatusUnprocessableEntity, "Update is not a fast forward")
		return
	}
	f.refs[branch] = in.SHA
	writeGitHubJSON(w, http.StatusOK, refJSON(branch, in.SHA))
}

func (f *fakeGitHub) getCommit(w http.ResponseWriter, r *http.Request, _ string) {
	sha := r.PathValue(githubSHA)
	c, ok := f.commits[sha]
	if !ok {
		githubFakeError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeGitHubJSON(w, http.StatusOK, map[string]any{githubSHA: sha, githubMessage: c.Message, gitTree: map[string]string{githubSHA: c.Tree}, "parents": parentsJSON(c)})
}

func parentsJSON(c githubFakeCommit) []map[string]string {
	var parents []map[string]string
	for _, p := range []string{c.Parent, c.MergeParent} {
		if p != "" {
			parents = append(parents, map[string]string{githubSHA: p})
		}
	}
	return parents
}

// mergeBranch is POST /repos/{owner}/{repo}/merges: head (a branch or a
// commit) merged into the branch base, as GitHub answers it — 201 with the
// merge commit, 204 when base contains head already, 409 when both changed
// one path differently, 404 for an unknown base or head.
func (f *fakeGitHub) mergeBranch(w http.ResponseWriter, r *http.Request, _ string) {
	var in struct {
		Base, Head    string
		CommitMessage string `json:"commit_message"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		githubFakeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	baseSHA, ok := f.refs[in.Base]
	if !ok {
		githubFakeError(w, http.StatusNotFound, "Base does not exist")
		return
	}
	headSHA, ok := f.refs[in.Head]
	if _, known := f.commits[in.Head]; !ok && known {
		headSHA, ok = in.Head, true
	}
	if !ok {
		githubFakeError(w, http.StatusNotFound, "Head does not exist")
		return
	}
	ofBase := f.ancestors(baseSHA)
	if slices.Contains(ofBase, headSHA) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	mergeBase := ""
	for _, c := range f.ancestors(headSHA) {
		if slices.Contains(ofBase, c) {
			mergeBase = c
			break
		}
	}
	tree, ok := mergeTrees(f.trees[f.commits[mergeBase].Tree], f.trees[f.commits[baseSHA].Tree], f.trees[f.commits[headSHA].Tree])
	if !ok {
		githubFakeError(w, http.StatusConflict, "Merge conflict")
		return
	}
	message := in.CommitMessage
	if message == "" {
		message = "Merge " + in.Head + " into " + in.Base
	}
	sha := f.putCommit(f.putTree(tree), baseSHA, message)
	c := f.commits[sha]
	c.MergeParent = headSHA
	f.commits[sha] = c
	f.refs[in.Base] = sha
	writeGitHubJSON(w, http.StatusCreated, map[string]any{
		githubSHA: sha, "commit": map[string]any{githubMessage: message, gitTree: map[string]string{githubSHA: c.Tree}}, "parents": parentsJSON(c),
	})
}

func (f *fakeGitHub) createCommit(w http.ResponseWriter, r *http.Request, _ string) {
	var in struct {
		Message string
		Tree    string
		Parents []string
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		githubFakeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if _, ok := f.trees[in.Tree]; !ok || len(in.Parents) != 1 {
		githubFakeError(w, http.StatusUnprocessableEntity, "the fake takes a known tree and exactly one parent")
		return
	}
	if _, ok := f.commits[in.Parents[0]]; !ok {
		githubFakeError(w, http.StatusUnprocessableEntity, "Parent SHA does not exist")
		return
	}
	sha := f.putCommit(in.Tree, in.Parents[0], in.Message)
	writeGitHubJSON(w, http.StatusCreated, map[string]any{githubSHA: sha, gitTree: map[string]string{githubSHA: in.Tree}})
}

func (f *fakeGitHub) createTree(w http.ResponseWriter, r *http.Request, _ string) {
	var in struct {
		BaseTree string `json:"base_tree"`
		Tree     []struct {
			Path string  `json:"path"`
			Type string  `json:"type"`
			SHA  *string `json:"sha"`
		} `json:"tree"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		githubFakeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	base, ok := f.trees[in.BaseTree]
	if !ok {
		githubFakeError(w, http.StatusUnprocessableEntity, "base_tree is not a known tree")
		return
	}
	tree := maps.Clone(base)
	for _, e := range in.Tree {
		switch {
		case e.Type != "blob":
			githubFakeError(w, http.StatusUnprocessableEntity, "the fake takes blob entries only")
			return
		case e.SHA == nil:
			delete(tree, e.Path)
		default:
			if _, known := f.blobs[*e.SHA]; !known {
				githubFakeError(w, http.StatusUnprocessableEntity, "tree.sha "+*e.SHA+" is not a blob")
				return
			}
			tree[e.Path] = *e.SHA
		}
	}
	writeGitHubJSON(w, http.StatusCreated, map[string]any{githubSHA: f.putTree(tree)})
}

func (f *fakeGitHub) createBlob(w http.ResponseWriter, r *http.Request, _ string) {
	var in struct{ Content, Encoding string }
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		githubFakeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	content := []byte(in.Content)
	if in.Encoding == githubBase64 {
		raw, err := base64.StdEncoding.DecodeString(in.Content)
		if err != nil {
			githubFakeError(w, http.StatusUnprocessableEntity, "content is not base64")
			return
		}
		content = raw
	}
	writeGitHubJSON(w, http.StatusCreated, map[string]any{githubSHA: f.putBlob(content)})
}

func (f *fakeGitHub) getBlob(w http.ResponseWriter, r *http.Request, _ string) {
	content, ok := f.blobs[r.PathValue(githubSHA)]
	if !ok {
		githubFakeError(w, http.StatusNotFound, "Not Found")
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "raw") {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(content)
		return
	}
	writeGitHubJSON(w, http.StatusOK, blobJSON(map[string]any{}, r.PathValue(githubSHA), content))
}

// blobJSON adds a blob's sha, size and base64 content to out: the shape of
// a blob and of a file's contents.
func blobJSON(out map[string]any, sha string, content []byte) map[string]any {
	out[githubSHA], out["size"], out["encoding"], out["content"] = sha, len(content), githubBase64, base64.StdEncoding.EncodeToString(content)
	return out
}

func (f *fakeGitHub) getContents(w http.ResponseWriter, r *http.Request, _ string) {
	tree, ok := f.refTree(r.URL.Query().Get("ref"))
	p := r.PathValue("path")
	sha, found := tree[p]
	if !ok || !found {
		githubFakeError(w, http.StatusNotFound, "Not Found")
		return
	}
	content := f.blobs[sha]
	writeGitHubJSON(w, http.StatusOK, blobJSON(map[string]any{fieldTypeKey: "file", pathArg: p}, sha, content))
}

func (f *fakeGitHub) pullJSON(pr *githubFakePull) map[string]any {
	owner, _, _ := strings.Cut(f.repo, "/")
	return map[string]any{
		"number": pr.Number, "state": pr.State, "title": pr.Title, "body": pr.Body,
		"html_url": fmt.Sprintf("https://github.lab.local/%s/pull/%d", f.repo, pr.Number),
		"head":     map[string]any{githubRef: pr.Head, githubSHA: f.refs[pr.Head], "label": owner + ":" + pr.Head},
		"base":     map[string]any{githubRef: pr.Base, githubSHA: f.refs[pr.Base]},
	}
}

func (f *fakeGitHub) createPull(w http.ResponseWriter, r *http.Request, login string) {
	var in struct{ Title, Body, Head, Base string }
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		githubFakeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	_, headOK := f.refs[in.Head]
	_, baseOK := f.refs[in.Base]
	if !headOK || !baseOK || in.Head == in.Base {
		githubFakeError(w, http.StatusUnprocessableEntity, "Validation Failed: head and base must be two known branches")
		return
	}
	for _, pr := range f.pulls {
		if pr.Head == in.Head && pr.State == "open" {
			githubFakeError(w, http.StatusUnprocessableEntity, "A pull request already exists for "+in.Head)
			return
		}
	}
	pr := &githubFakePull{Number: len(f.pulls) + 1, Title: in.Title, Body: in.Body, Head: in.Head, Base: in.Base, Author: login, State: "open"}
	f.pulls = append(f.pulls, pr)
	writeGitHubJSON(w, http.StatusCreated, f.pullJSON(pr))
}

func (f *fakeGitHub) listPulls(w http.ResponseWriter, r *http.Request, _ string) {
	_, head, _ := strings.Cut(r.URL.Query().Get("head"), ":")
	state := r.URL.Query().Get("state")
	out := []map[string]any{}
	for _, pr := range f.pulls {
		if (head == "" || pr.Head == head) && (state == "" || state == "all" || pr.State == state) {
			out = append(out, f.pullJSON(pr))
		}
	}
	writeGitHubJSON(w, http.StatusOK, out)
}

// pullViews is every pull request with what its head changes against its
// base.
func (f *fakeGitHub) pullViews() []githubFakePullView {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []githubFakePullView{}
	for _, pr := range f.pulls {
		head, _ := f.headTree(pr.Head)
		base, _ := f.headTree(pr.Base)
		v := githubFakePullView{Number: pr.Number, Title: pr.Title, Body: pr.Body, Head: pr.Head, Base: pr.Base, Author: pr.Author, Files: map[string]string{}}
		for p, sha := range head {
			if base[p] != sha {
				v.Files[p] = string(f.blobs[sha])
			}
		}
		for p := range base {
			if _, kept := head[p]; !kept {
				v.Removed = append(v.Removed, p)
			}
		}
		slices.Sort(v.Removed)
		out = append(out, v)
	}
	return out
}

func (f *fakeGitHub) servePulls(w http.ResponseWriter, _ *http.Request) {
	writeGitHubJSON(w, http.StatusOK, f.pullViews())
}

// --- the fake in a container on the kind network -----------------------------

// githubFakeContainer is the fake running as a container on the kind network
// (fakecontainer.go).
type githubFakeContainer struct{ *fakeContainer }

// pulls reads the recorded pull requests back.
func (c *githubFakeContainer) pulls() ([]githubFakePullView, error) {
	resp, err := c.client.Get(c.hostURL + githubFakePullsPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out []githubFakePullView
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("reading the fake GitHub's pull requests: %w", err)
	}
	return out, nil
}

// startGitHubFakeContainer runs `<binary> github-fake` on the kind network,
// repo at branch holding files.
func startGitHubFakeContainer(cfg *config.Config, binary, repo, branch string, files map[string][]byte) (*githubFakeContainer, error) {
	args := []string{"--repo", repo, "--branch", branch}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		args = append(args, "--file", p+"="+base64.StdEncoding.EncodeToString(files[p]))
	}
	c, err := startFakeContainer(cfg, binary, fakeContainerSpec{
		what: "the fake GitHub API", suffix: githubFakeCommand, command: githubFakeCommand,
		binaryFlag: "--github-fake-binary", healthPath: githubFakeHealthPath, args: args,
	})
	if err != nil {
		return nil, err
	}
	return &githubFakeContainer{fakeContainer: c}, nil
}
