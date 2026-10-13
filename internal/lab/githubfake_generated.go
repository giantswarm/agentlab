package lab

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"time"
)

// A measurement that wants a large repository of another size than the
// fixture's adds one to the running fake and removes it again
// (workspaces-measure --large-repo-mib): PUT <githubFakeReposPath>/{owner}/{repo}
// seeds a public repository of the fixture's owner with generatedMiB of
// incompressible content and a Go module to build; DELETE removes it. Only a
// repository added this way can be removed: the fixture's stay. Like the
// request log, these paths are the lab's own, not GitHub's.

// githubFakeReposPath is where generated repositories are added and removed.
const githubFakeReposPath = "/_fake/repos"

// githubFakeMaxGeneratedMiB bounds a generated repository: the fake writes
// it to its data directory on the host.
const githubFakeMaxGeneratedMiB = 16 << 10

// githubRepoName is a repository name GitHub accepts, never starting with a
// dot.
var githubRepoName = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,99}$`)

// generatedRepoRequest is the body of the PUT.
type generatedRepoRequest struct {
	GeneratedMiB int      `json:"generatedMiB"`
	Language     string   `json:"language,omitempty"`
	Topics       []string `json:"topics,omitempty"`
}

// generatedRepoAnswer is the answer to the PUT: the repository and its size
// in KiB, as GitHub's `size` reports it.
type generatedRepoAnswer struct {
	FullName string `json:"full_name"`
	Size     int    `json:"size"`
}

// generatedRepoFiles are a generated repository's files besides its data:
// a README and a Go module with a main package, so a measurement has a build
// to run in it.
func generatedRepoFiles(owner, name string) map[string]string {
	return map[string]string{
		"README.md": "# " + name + "\n\nA large repository generated for the measurements.\n",
		"go.mod":    "module github.com/" + owner + "/" + name + "\n\ngo 1.26\n",
		"main.go":   "package main\n\nfunc main() {}\n",
	}
}

// addGeneratedRepo seeds the repository and adds it; the same request again
// answers the existing one.
func (g *workspaceGitHub) addGeneratedRepo(w http.ResponseWriter, r *http.Request) {
	var in generatedRepoRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.GeneratedMiB < 1 || in.GeneratedMiB > githubFakeMaxGeneratedMiB {
		githubFakeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("generatedMiB is required: 1 to %d", githubFakeMaxGeneratedMiB))
		return
	}
	owner, name := r.PathValue("owner"), r.PathValue("repo")
	if _, ok := g.owner(owner); !ok || !githubRepoName.MatchString(name) {
		githubFakeError(w, http.StatusUnprocessableEntity, "the owner must be the fixture's and the name a repository name")
		return
	}
	full := owner + "/" + name
	g.mu.Lock()
	existing, ok := g.repos[full]
	g.mu.Unlock()
	switch {
	case ok && existing.generated && existing.GeneratedMiB == in.GeneratedMiB:
		writeGitHubJSON(w, http.StatusOK, generatedRepoAnswer{FullName: full, Size: existing.sizeKiB})
		return
	case ok:
		githubFakeError(w, http.StatusConflict, full+" exists: remove the generated one first, the fixture's stay")
		return
	}
	repo := githubFixtureRepo{Owner: owner, Name: name, Language: in.Language, Topics: in.Topics, DefaultBranch: "main",
		GeneratedMiB: in.GeneratedMiB, Files: generatedRepoFiles(owner, name)}
	// Seeding writes the content and runs git: minutes for a large one, so
	// outside the lock.
	size, err := g.git.seed(repo)
	if err != nil {
		githubFakeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	id := 0
	for _, existing := range g.repos {
		id = max(id, existing.id)
	}
	g.repos[full] = &workspaceRepo{githubFixtureRepo: repo, id: id + 1, sizeKiB: size, pushedAt: g.now().UTC().Truncate(time.Second), generated: true}
	writeGitHubJSON(w, http.StatusCreated, generatedRepoAnswer{FullName: full, Size: size})
}

// removeGeneratedRepo removes a generated repository and its git data.
func (g *workspaceGitHub) removeGeneratedRepo(w http.ResponseWriter, r *http.Request) {
	full := r.PathValue("owner") + "/" + r.PathValue("repo")
	g.mu.Lock()
	defer g.mu.Unlock()
	repo, ok := g.repos[full]
	switch {
	case !ok:
		githubFakeError(w, http.StatusNotFound, "Not Found")
		return
	case !repo.generated:
		githubFakeError(w, http.StatusConflict, full+" is the fixture's: only a generated repository is removed")
		return
	}
	if err := os.RemoveAll(g.git.path(full)); err != nil {
		githubFakeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	delete(g.repos, full)
	w.WriteHeader(http.StatusNoContent)
}
