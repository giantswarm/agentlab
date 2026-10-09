package lab

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// Pull requests on the workspace fake: REST's POST and GET
// /repos/{owner}/{repo}/pulls, and the GraphQL operations `gh pr create`
// sends (GitHub Enterprise's /api/graphql). A pull request is opened by a
// caller who reads the repository, between two branches that exist in it;
// the proof reads every one back (githubFakePullsPath) with its author.

// workspacePull is a pull request.
type workspacePull struct {
	Number int    `json:"number"`
	Repo   string `json:"repository"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Head   string `json:"head"`
	Base   string `json:"base"`
	Draft  bool   `json:"draft"`
	Author string `json:"author"`
	State  string `json:"state"`
}

// openPull opens a pull request on repo as c, or says why not; the caller
// holds g.mu.
func (g *workspaceGitHub) openPull(c githubCaller, repo *workspaceRepo, in workspacePull) (*workspacePull, string) {
	_, head, cross := strings.Cut(in.Head, ":")
	if !cross {
		head = in.Head
	}
	switch {
	case in.Title == "":
		return nil, "Validation Failed: title is missing"
	case head == in.Base || g.git.branchSHA(repo.fullName(), head) == "":
		return nil, "Validation Failed: head " + head + " is not a branch of " + repo.fullName()
	case g.git.branchSHA(repo.fullName(), in.Base) == "":
		return nil, "Validation Failed: base " + in.Base + " is not a branch of " + repo.fullName()
	}
	for _, pr := range repo.pulls {
		if pr.Head == head && pr.State == githubOpen {
			return nil, "A pull request already exists for " + repo.Owner + ":" + head + "."
		}
	}
	pr := &workspacePull{Number: len(repo.pulls) + 1, Repo: repo.fullName(), Title: in.Title, Body: in.Body,
		Head: head, Base: in.Base, Draft: in.Draft, Author: c.name(), State: githubOpen}
	repo.pulls = append(repo.pulls, pr)
	return pr, ""
}

func (g *workspaceGitHub) pullURL(r *http.Request, pr *workspacePull) string {
	return fmt.Sprintf("https://%s/%s/pull/%d", r.Host, pr.Repo, pr.Number)
}

func (g *workspaceGitHub) restPullJSON(r *http.Request, repo *workspaceRepo, pr *workspacePull) map[string]any {
	return map[string]any{
		githubNumber: pr.Number, githubState: pr.State, githubTitle: pr.Title, githubBody: pr.Body, "draft": pr.Draft,
		githubHTMLURL: g.pullURL(r, pr), githubUserKey: map[string]any{githubLogin: pr.Author},
		"head":     map[string]any{githubRef: pr.Head, githubSHA: g.git.branchSHA(pr.Repo, pr.Head), "label": repo.Owner + ":" + pr.Head},
		githubBase: map[string]any{githubRef: pr.Base, githubSHA: g.git.branchSHA(pr.Repo, pr.Base)},
	}
}

// pullCaller admits a caller that may open a pull request: a user's or an
// installation's token, on a repository it reads.
func (g *workspaceGitHub) pullCaller(w http.ResponseWriter, r *http.Request) (githubCaller, bool) {
	c := callerOf(r)
	if c.kind != credUser && c.kind != credInstallation {
		badCredentials(w)
		return c, false
	}
	return c, true
}

func (g *workspaceGitHub) createPull(w http.ResponseWriter, r *http.Request) {
	c, ok := g.pullCaller(w, r)
	if !ok {
		return
	}
	var in workspacePull
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		githubFakeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	repo := g.readableRepo(w, r)
	if repo == nil {
		return
	}
	pr, why := g.openPull(c, repo, in)
	if pr == nil {
		githubFakeError(w, http.StatusUnprocessableEntity, why)
		return
	}
	writeGitHubJSON(w, http.StatusCreated, g.restPullJSON(r, repo, pr))
}

func (g *workspaceGitHub) listPulls(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	repo := g.readableRepo(w, r)
	if repo == nil {
		return
	}
	_, head, _ := strings.Cut(r.URL.Query().Get("head"), ":")
	state := cmp.Or(r.URL.Query().Get(githubState), githubOpen)
	out := []map[string]any{}
	for _, pr := range repo.pulls {
		if (head == "" || pr.Head == head) && (state == githubAll || pr.State == state) {
			out = append(out, g.restPullJSON(r, repo, pr))
		}
	}
	writeGitHubJSON(w, http.StatusOK, out)
}

// servePulls is the read-back: every pull request of every repository.
func (g *workspaceGitHub) servePulls(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := []*workspacePull{}
	for _, repo := range g.fixture.Repositories {
		out = append(out, g.repos[repo.fullName()].pulls...)
	}
	writeGitHubJSON(w, http.StatusOK, out)
}

// --- GraphQL ------------------------------------------------------------------

// graphqlData is the key of a GraphQL answer.
const graphqlData = "data"

var graphqlTypeProbe = regexp.MustCompile(`(\w+)\s*:\s*__type\(`)

var graphqlOperation = regexp.MustCompile(`(?m)^\s*(query|mutation)\s+(\w+)`)

// graphql answers the operations of `gh pr create` by name; any other
// operation is an error naming it, so a gh that asks something new says so.
func (g *workspaceGitHub) graphql(w http.ResponseWriter, r *http.Request) {
	c, ok := g.pullCaller(w, r)
	if !ok {
		return
	}
	var in struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		githubFakeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	op := ""
	if m := graphqlOperation.FindStringSubmatch(in.Query); m != nil {
		op = m[2]
	}
	// gh's feature detection introspects types (`Issue: __type(name:
	// "Issue") {fields ...}`); no field it probes for exists here, so gh
	// takes the path of the oldest GitHub Enterprise it supports.
	if aliases := graphqlTypeProbe.FindAllStringSubmatch(in.Query, -1); aliases != nil {
		data := map[string]any{}
		for _, a := range aliases {
			data[a[1]] = map[string]any{"fields": []any{}}
		}
		writeGitHubJSON(w, http.StatusOK, map[string]any{graphqlData: data})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	answer, ok := map[string]func(*http.Request, githubCaller, map[string]any) (any, string){
		"RepositoryInfo":       g.gqlRepositoryInfo,
		"PullRequestForBranch": g.gqlPullRequestForBranch,
		"PullRequestCreate":    g.gqlPullRequestCreate,
	}[op]
	if !ok {
		fmt.Printf("graphql: unanswered operation %s\n%s\n", op, in.Query)
		writeGraphQLError(w, "the fake GitHub does not answer the GraphQL operation "+op)
		return
	}
	data, why := answer(r, c, in.Variables)
	if why != "" {
		writeGraphQLError(w, why)
		return
	}
	writeGitHubJSON(w, http.StatusOK, map[string]any{graphqlData: data})
}

func writeGraphQLError(w http.ResponseWriter, message string) {
	writeGitHubJSON(w, http.StatusOK, map[string]any{"errors": []map[string]string{{githubMessage: message}}})
}

// gqlRepo is the repository the variables owner and name (or repo) name,
// when c reads it; the caller holds g.mu.
func (g *workspaceGitHub) gqlRepo(c githubCaller, vars map[string]any) (*workspaceRepo, string) {
	owner, _ := vars["owner"].(string)
	name, _ := vars[nameKey].(string)
	if name == "" {
		name, _ = vars["repo"].(string)
	}
	repo, ok := g.repos[owner+"/"+name]
	if !ok || !g.canRead(c, repo) {
		return nil, fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, name)
	}
	return repo, ""
}

func (g *workspaceGitHub) gqlRepositoryJSON(r *http.Request, c githubCaller, repo *workspaceRepo) map[string]any {
	permission := "READ"
	if g.canWrite(c, repo) {
		permission = "WRITE"
	}
	return map[string]any{
		"id": "R_" + fmt.Sprint(repo.id), "databaseId": repo.id, nameKey: repo.Name, "owner": map[string]any{githubLogin: repo.Owner},
		"sshUrl": "git@" + r.Host + ":" + repo.fullName() + ".git", githubURL: "https://" + r.Host + "/" + repo.fullName(),
		"hasIssuesEnabled": true, "hasWikiEnabled": false, "description": "", "viewerPermission": permission,
		"defaultBranchRef": map[string]any{nameKey: repo.DefaultBranch}, "isPrivate": repo.Private, "isArchived": repo.Archived,
		"mergeCommitAllowed": true, "rebaseMergeAllowed": true, "squashMergeAllowed": true,
	}
}

func (g *workspaceGitHub) gqlRepositoryInfo(r *http.Request, c githubCaller, vars map[string]any) (any, string) {
	repo, why := g.gqlRepo(c, vars)
	if repo == nil {
		return nil, why
	}
	return map[string]any{repositoryArg: g.gqlRepositoryJSON(r, c, repo)}, ""
}

func (g *workspaceGitHub) gqlPullJSON(r *http.Request, repo *workspaceRepo, pr *workspacePull) map[string]any {
	return map[string]any{
		"id": fmt.Sprintf("PR_%d_%d", repo.id, pr.Number), githubNumber: pr.Number, githubURL: g.pullURL(r, pr),
		githubState: strings.ToUpper(pr.State), githubTitle: pr.Title, githubBody: pr.Body, "isDraft": pr.Draft,
		"baseRefName": pr.Base, "headRefName": pr.Head, "isCrossRepository": false,
		"headRepositoryOwner": map[string]any{"id": repo.Owner, githubLogin: repo.Owner},
		"author":              map[string]any{githubLogin: pr.Author},
	}
}

// gqlPullRequestForBranch is gh's check for an open pull request of the
// head branch.
func (g *workspaceGitHub) gqlPullRequestForBranch(r *http.Request, c githubCaller, vars map[string]any) (any, string) {
	repo, why := g.gqlRepo(c, vars)
	if repo == nil {
		return nil, why
	}
	head, _ := vars["headRefName"].(string)
	states, _ := vars["states"].([]any)
	nodes := []map[string]any{}
	for _, pr := range slices.Backward(repo.pulls) {
		if pr.Head == head && (len(states) == 0 || slices.Contains(states, any(strings.ToUpper(pr.State)))) {
			nodes = append(nodes, g.gqlPullJSON(r, repo, pr))
		}
	}
	return map[string]any{repositoryArg: map[string]any{
		"pullRequests":     map[string]any{"nodes": nodes},
		"defaultBranchRef": map[string]any{nameKey: repo.DefaultBranch},
	}}, ""
}

// gqlPullRequestCreate is gh's createPullRequest mutation; the repository
// is the one of its repositoryId.
func (g *workspaceGitHub) gqlPullRequestCreate(r *http.Request, c githubCaller, vars map[string]any) (any, string) {
	input, _ := vars["input"].(map[string]any)
	str := func(k string) string { v, _ := input[k].(string); return v }
	var repo *workspaceRepo
	for _, candidate := range g.repos {
		if "R_"+fmt.Sprint(candidate.id) == str("repositoryId") && g.canRead(c, candidate) {
			repo = candidate
		}
	}
	if repo == nil {
		return nil, "Could not resolve to a node with the global id of '" + str("repositoryId") + "'"
	}
	draft, _ := input["draft"].(bool)
	pr, why := g.openPull(c, repo, workspacePull{Title: str("title"), Body: str("body"), Head: str("headRefName"), Base: str("baseRefName"), Draft: draft})
	if pr == nil {
		return nil, why
	}
	return map[string]any{"createPullRequest": map[string]any{"pullRequest": g.gqlPullJSON(r, repo, pr)}}, ""
}
