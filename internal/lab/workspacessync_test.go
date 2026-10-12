package lab

import (
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The request log as the workspace sync leaves it: a git request names its
// repository, with or without .git; an API path names none.
func TestGitRequestRepository(t *testing.T) {
	for path, want := range map[string]string{
		"/agentlab-oss/docs.git/info/refs":        syncMovingRepo,
		"/agentlab-oss/docs.git/git-upload-pack":  syncMovingRepo,
		"/agentlab-org/platform-infra/info/refs":  syncPrivateRepo,
		"/agentlab-oss/docs.git/git-receive-pack": syncMovingRepo,
		"/api/v3/orgs/agentlab-org/repos":         "",
		"/api/v3/repos/agentlab-oss/docs":         "",
		"/_fake/requests":                         "",
		"/agentlab-oss/docs.git/objects/info":     "",
		"/login/oauth/access_token":               "",
	} {
		if got := gitRequestRepository(path); got != want {
			t.Errorf("gitRequestRepository(%q) = %q, want %q", path, got, want)
		}
	}
}

func syncRequests(lines ...string) []githubRequest {
	var out []githubRequest
	for _, l := range lines {
		method, path, _ := strings.Cut(l, " ")
		out = append(out, githubRequest{Method: method, Path: path, Status: 200})
	}
	return out
}

// A no-op is judged on the request log and the manifest, never on timing:
// listings without a git request and the same manifest pass; a fetch, a
// cycle that listed nothing and a changed manifest each fail, naming why.
func TestJudgeNoOp(t *testing.T) {
	listing := syncRequests(
		"POST /api/v3/app/installations/2/access_tokens",
		"GET /api/v3/orgs/agentlab-org/repos",
		"GET /api/v3/installation/repositories",
	)
	w := classifyRequests(listing)
	if w.listings != 2 || w.other != 1 || len(w.fetched) != 0 {
		t.Fatalf("classifyRequests = %+v", w)
	}
	if err := judgeNoOp(w, "{}", "{}"); err != nil {
		t.Fatalf("listings alone with the same manifest: %v", err)
	}
	fetched := classifyRequests(append(listing, syncRequests("GET /agentlab-oss/docs.git/info/refs", "POST /agentlab-oss/docs.git/git-upload-pack")...))
	if err := judgeNoOp(fetched, "{}", "{}"); err == nil || !strings.Contains(err.Error(), syncMovingRepo) {
		t.Fatalf("a fetch in a no-op = %v, want the repository named", err)
	}
	if err := judgeNoOp(classifyRequests(nil), "{}", "{}"); err == nil || !strings.Contains(err.Error(), "no listing") {
		t.Fatalf("a cycle that listed nothing = %v, want refused", err)
	}
	if err := judgeNoOp(w, `{"syncedAt":"a"}`, `{"syncedAt":"b"}`); err == nil || !strings.Contains(err.Error(), "manifest changed") {
		t.Fatalf("a changed manifest = %v, want refused", err)
	}
}

// The last change at the provider is a push that went through or a topic
// replacement; a fetch, a refused push and a read are none.
func TestLastProviderChange(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 10, 12, 12, m, 0, 0, time.UTC) }
	reqs := []githubRequest{
		{Time: at(1), Method: http.MethodPost, Path: "/agentlab-oss/docs.git/git-receive-pack", Status: 200},
		{Time: at(2), Method: http.MethodPut, Path: "/api/v3/repos/agentlab-oss/docs/topics", Status: 200},
		{Time: at(3), Method: http.MethodPost, Path: "/agentlab-oss/docs.git/git-upload-pack", Status: 200},
		{Time: at(4), Method: http.MethodPost, Path: "/agentlab-org/platform-api.git/git-receive-pack", Status: 403},
		{Time: at(5), Method: http.MethodGet, Path: "/api/v3/repos/agentlab-oss/docs/topics", Status: 200},
	}
	if got := lastProviderChange(reqs); !got.Equal(at(2)) {
		t.Fatalf("lastProviderChange = %v, want %v", got, at(2))
	}
	if got := lastProviderChange(nil); !got.IsZero() {
		t.Fatalf("lastProviderChange(nil) = %v", got)
	}
}

func TestJudgeFetched(t *testing.T) {
	w := classifyRequests(syncRequests(
		"GET /agentlab-oss/docs.git/info/refs",
		"POST /agentlab-oss/docs.git/git-upload-pack",
		"GET /api/v3/users/agentlab-oss/repos",
	))
	if err := judgeFetched(w, syncMovingRepo); err != nil {
		t.Fatalf("only the pushed repository fetched: %v", err)
	}
	if err := judgeFetched(w); err == nil {
		t.Fatal("a fetch where none was wanted passed")
	}
	both := classifyRequests(syncRequests("GET /agentlab-oss/docs.git/info/refs", "GET /agentlab-org/platform-infra.git/info/refs"))
	if err := judgeFetched(both, syncMovingRepo); err == nil || !strings.Contains(err.Error(), syncPrivateRepo) {
		t.Fatalf("a second repository fetched = %v, want it named", err)
	}
	if err := judgeFetched(both, syncPrivateRepo, syncMovingRepo); err != nil {
		t.Fatalf("both wanted, in another order: %v", err)
	}
	if got := both.String(); got != "git fetched agentlab-org/platform-infra, agentlab-oss/docs; 0 listings, 0 other API calls" {
		t.Fatalf("String() = %q", got)
	}
}

// The filter selects over both owners with one language and one topic
// filter each, the private repository's owner only where the App is
// installed; the list names each repository under its owner.
func TestSyncSources(t *testing.T) {
	with := syncFilterSources(true)
	if owners := sourceOwners(with); !slices.Equal(owners, []string{syncOrg, syncOSS}) {
		t.Fatalf("the filter's owners = %v", owners)
	}
	for _, s := range with {
		if s[wmProviderArg] != config.WorkspaceProviderFake || s["filters"] == nil || s["repositories"] != nil {
			t.Fatalf("a filter source = %v", s)
		}
	}
	if owners := sourceOwners(syncFilterSources(false)); !slices.Equal(owners, []string{syncOSS}) {
		t.Fatalf("without the App on %s the filter's owners = %v", syncOrg, owners)
	}
	named := syncNamedSources(syncListRepoA, syncListRepoB, syncOrg+"/monorepo")
	if len(named) != 2 || !slices.Equal(named[0]["repositories"].([]string), []string{"platform-api", "monorepo"}) || named[1][wmOwnerArg] != syncOSS {
		t.Fatalf("syncNamedSources = %v", named)
	}
	// The fixture holds what the filter selects: the private HCL repository
	// with topic infrastructure and the public MDX one with the moving topic.
	fixture, err := loadGitHubFixture("")
	if err != nil {
		t.Fatal(err)
	}
	var selected []string
	for _, r := range fixture.Repositories {
		langs := syncFilters["languages"].([]string)
		topics := syncFilters["topics"].(map[string]any)["names"].([]string)
		if !r.Archived && !r.Fork && slices.Contains(langs, r.Language) && slices.ContainsFunc(r.Topics, func(t string) bool { return slices.Contains(topics, t) }) {
			selected = append(selected, r.fullName())
			if r.fullName() == syncPrivateRepo && !r.Private {
				t.Fatalf("%s is not private in the fixture", syncPrivateRepo)
			}
		}
	}
	slices.Sort(selected)
	if !slices.Equal(selected, []string{syncPrivateRepo, syncMovingRepo}) {
		t.Fatalf("the filters select %v in the fixture", selected)
	}
}

func TestParseMirrorProbe(t *testing.T) {
	out := `agentlab-oss/docs bare=true head=abc branches=1 tags=0 gc.auto=0 gc.pruneExpire=never
some other line
agentlab-org/platform-infra bare=true head=def branches=1 tags=0 gc.auto=0 gc.pruneExpire=never`
	probe := parseMirrorProbe(out)
	if len(probe) != 2 || probe[syncMovingRepo]["head"] != "abc" || probe[syncPrivateRepo]["gc.pruneExpire"] != "never" {
		t.Fatalf("parseMirrorProbe = %v", probe)
	}
	if got := probeValue("alternates=/x head=1234 own-objects=0", "head"); got != "1234" {
		t.Fatalf("probeValue = %q", got)
	}
}

func TestSyncMirrorLines(t *testing.T) {
	at := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	lines := syncMirrorLines([]syncMirror{
		{Repository: syncMovingRepo, Head: "0123456789abcdef", PushedAt: at, SizeBytes: 2048, DroppedAt: &at},
		{Repository: syncPrivateRepo, Head: "fedcba9876543210", PushedAt: at, SizeBytes: 1},
	})
	want := []string{
		"agentlab-org/platform-infra  head fedcba987654  pushed 2026-07-01T10:00:00Z  1 KiB",
		"agentlab-oss/docs  head 0123456789ab  pushed 2026-07-01T10:00:00Z  2 KiB  dropped 2026-07-01T10:00:00Z",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("syncMirrorLines =\n%s", strings.Join(lines, "\n"))
	}
	d := &syncDetail{Mirrors: []syncMirror{{Repository: syncMovingRepo, DroppedAt: &at}, {Repository: syncPrivateRepo}}}
	if live := d.live(); !slices.Equal(live, []string{syncPrivateRepo}) || d.mirror(syncMovingRepo) == nil || d.mirror("x/y") != nil {
		t.Fatalf("live = %v", live)
	}
}

// The volume probes are POSIX shell the git image runs with sh -e.
func TestSyncScriptsParse(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for name, script := range map[string]string{
		"mirrors":  syncMirrorsScript([]string{syncMovingRepo, syncPrivateRepo}),
		"borrow":   syncBorrowScript,
		"borrowed": syncBorrowedScript("0123456789abcdef0123456789abcdef01234567"),
		"presence": syncMirrorPresenceScript,
		"remove":   syncRemoveSessionScript,
	} {
		// #nosec G204 -- the test's own scripts, parsed and not run
		if out, err := exec.Command(sh, "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("the %s probe does not parse: %v\n%s", name, err, out)
		}
	}
	if !strings.Contains(syncBorrowScript, "clone -q --shared "+syncMirrorDir) {
		t.Fatal("the session directory's clone does not borrow the mirror's objects")
	}
}

func TestWorkspacesTestOptionsValidate(t *testing.T) {
	for _, tc := range []struct {
		opts WorkspacesTestOptions
		want string
	}{
		{WorkspacesTestOptions{}, ""},
		{WorkspacesTestOptions{StorageOnly: true}, ""},
		{WorkspacesTestOptions{Part: WorkspacesPartSync}, ""},
		{WorkspacesTestOptions{Part: WorkspacesPartSync, Email: testDevUser}, ""},
		{WorkspacesTestOptions{Email: testDevUser}, "signs in as nobody"},
		{WorkspacesTestOptions{Part: WorkspacesPartSync, StorageOnly: true}, "--storage-only"},
		{WorkspacesTestOptions{Part: WorkspacesPartAccess}, ""},
		{WorkspacesTestOptions{Part: WorkspacesPartAccess, Email: testDevUser}, ""},
		{WorkspacesTestOptions{Part: WorkspacesPartAccess, StorageOnly: true}, "--storage-only"},
		{WorkspacesTestOptions{Part: "billing"}, "no part"},
	} {
		err := tc.opts.validate()
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("validate(%+v) = %v, want %q", tc.opts, err, tc.want)
		}
	}
}
