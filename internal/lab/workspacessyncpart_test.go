package lab

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

const (
	proofPushed = "giantswarm/workspace-proof"
	proofStill  = "giantswarm/agent-sandbox"
)

func TestJudgeNoOp(t *testing.T) {
	quiet := &syncLastSync{Result: syncResultNoChange}
	if err := judgeNoOp(quiet, "m", "m", nil); err != nil {
		t.Fatalf("a quiet cycle: %v", err)
	}
	for name, c := range map[string]struct {
		last          *syncLastSync
		before, after string
		fetched       []string
		want          string
	}{
		"a sync job":      {&syncLastSync{Result: syncResultSucceed, Job: "sync-1"}, "m", "m", nil, "want NoChange and no sync job"},
		"a fetch":         {quiet, "m", "m", []string{proofStill}, "fetched " + proofStill},
		"manifest change": {quiet, "m", "n", nil, "the manifest changed"},
	} {
		err := judgeNoOp(c.last, c.before, c.after, c.fetched)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: judgeNoOp = %v, want %q", name, err, c.want)
		}
	}
}

func TestFetchedSince(t *testing.T) {
	before := map[string]map[string]string{
		proofPushed: {probeFetchedKey: "0"},
		proofStill:  {probeFetchedKey: "100"},
	}
	after := map[string]map[string]string{
		proofPushed: {probeFetchedKey: "200"},
		proofStill:  {probeFetchedKey: "100"},
		"new/clone": {probeFetchedKey: "0"},
	}
	if got := fetchedSince(before, after); !slices.Equal(got, []string{proofPushed}) {
		t.Fatalf("fetchedSince = %v, want %s alone: a FETCH_HEAD unchanged or never written is no fetch", got, proofPushed)
	}
	if got := fetchedSince(nil, map[string]map[string]string{proofStill: {probeFetchedKey: "5"}}); !slices.Equal(got, []string{proofStill}) {
		t.Fatalf("a mirror first probed with a FETCH_HEAD: %v", got)
	}
}

// The volume probe's script runs against real mirrors: a clone, then a
// fetch, read the way the part reads it.
func TestSyncStampScriptOnMirrors(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...) // #nosec G204 -- the test's own git arguments
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	src := filepath.Join(root, "src")
	run(root, "init", "-q", src)
	run(src, "commit", "-q", "--allow-empty", "-m", "one")
	mirrors := filepath.Join(root, "workspace", workspacesTestMirrors)
	for _, repo := range []string{proofPushed, proofStill} {
		run(root, "clone", "-q", "--mirror", "-c", "gc.pruneExpire=never", src, filepath.Join(mirrors, repo+".git"))
	}
	script := strings.ReplaceAll(syncStampScript([]string{proofPushed, proofStill}), "/workspace/", filepath.Join(root, "workspace")+"/")
	script = strings.Replace(script, workspacesGitSetup, "", 1)
	probe := func() map[string]map[string]string {
		cmd := exec.Command("sh", shExitOnError, script) // #nosec G204 -- the part's own probe script, on the test's mirrors
		cmd.Env = append(os.Environ(), "HOME="+root)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the probe: %v\n%s", err, out)
		}
		return parseMirrorProbe(string(out))
	}
	first := probe()
	head := run(src, "rev-parse", "HEAD")
	for _, repo := range []string{proofPushed, proofStill} {
		v := first[repo]
		if v[probeHeadKey] != head || v[probeBranchKey] != probeNone || v[probeFetchedKey] != "0" || v["gc.pruneExpire"] != "never" {
			t.Fatalf("%s after the clone: %v", repo, v)
		}
	}
	run(src, "branch", proofBranch)
	run(src, "commit", "-q", "--allow-empty", "-m", "two")
	pushed := run(src, "rev-parse", "HEAD")
	run(src, "branch", "-f", proofBranch, pushed)
	run(filepath.Join(mirrors, proofPushed+".git"), "fetch", "-q", "--prune", "origin")
	second := probe()
	if got := fetchedSince(first, second); !slices.Equal(got, []string{proofPushed}) {
		t.Fatalf("fetched after one fetch: %v", got)
	}
	if second[proofPushed][probeBranchKey] != pushed {
		t.Fatalf("the scratch branch after the fetch: %v, want %s", second[proofPushed], pushed)
	}
	if lines := mirrorStampLines(second); !strings.Contains(lines, proofPushed+": bare") || !strings.Contains(lines, "never fetched") {
		t.Fatalf("mirrorStampLines =\n%s", lines)
	}
}

func TestProofReposSources(t *testing.T) {
	pr := &proofRepos{instance: config.WorkspaceProviderGitHub, pushed: proofPushed, still: "teemow/other"}
	got := pr.sources(pr.repos()...)
	if len(got) != 2 || got[0][wmOwnerArg] != "giantswarm" || got[1][wmOwnerArg] != "teemow" || got[0][wmProviderArg] != config.WorkspaceProviderGitHub {
		t.Fatalf("sources = %v", got)
	}
	if names := got[0][repositoriesKey].([]string); !slices.Equal(names, []string{"workspace-proof"}) {
		t.Fatalf("giantswarm's repositories = %v", names)
	}
	same := (&proofRepos{instance: config.WorkspaceProviderGitLab}).sources("workspaces/sub/a", "workspaces/sub/b")
	if len(same) != 1 || same[0][wmOwnerArg] != "workspaces/sub" || !slices.Equal(same[0][repositoriesKey].([]string), []string{"a", "b"}) {
		t.Fatalf("a nested GitLab group: %v", same)
	}
}

func TestProofReposSkip(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Workspaces.Enabled = true
	cfg.Platform.Workspaces.Provider = config.WorkspaceProviderGitHub
	if skip := proofReposSkip(cfg, config.WorkspaceProviderGitHub); !strings.Contains(skip, "proofRepositories") {
		t.Fatalf("no proof repositories: %q", skip)
	}
	cfg.Platform.Workspaces.GitHub.ProofRepositories = []string{proofPushed, proofStill}
	if skip := proofReposSkip(cfg, config.WorkspaceProviderGitHub); skip != "" {
		t.Fatalf("configured: %q", skip)
	}
	if skip := proofReposSkip(cfg, config.WorkspaceProviderGitLab); !strings.Contains(skip, config.WorkspaceProviderGitLab) {
		t.Fatalf("gitlab on a GitHub-only lab: %q", skip)
	}
}

// loginGit's helper answers the login from git's environment: the password
// is in no argument git or a process listing shows.
func TestLoginGitHelper(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	if strings.Contains(loginGitHelper, "s3cret") {
		t.Fatal("the helper carries a value")
	}
	c := exec.Command("git", "-c", "credential.helper=", "-c", "credential.helper="+loginGitHelper, "credential", "fill") // #nosec G204 -- the test's own helper
	c.Stdin = strings.NewReader("protocol=https\nhost=gitlab.example\n\n")
	c.Env = append(os.Environ(), "PROOF_GIT_LOGIN=person", "PROOF_GIT_PASSWORD=s3cret", "GIT_TERMINAL_PROMPT=0")
	got, err := c.CombinedOutput()
	if err != nil || !strings.Contains(string(got), "username=person") || !strings.Contains(string(got), "password=s3cret") {
		t.Fatalf("the helper answered %q, %v", got, err)
	}
}

func TestWorkspacesTestOptionsParts(t *testing.T) {
	for name, c := range map[string]struct {
		opts WorkspacesTestOptions
		want string
	}{
		"storage":            {WorkspacesTestOptions{}, ""},
		"sync":               {WorkspacesTestOptions{Part: WorkspacesPartSync}, ""},
		"sync on gitlab":     {WorkspacesTestOptions{Part: WorkspacesPartSync, Provider: config.WorkspaceProviderGitLab}, ""},
		"sessions":           {WorkspacesTestOptions{Part: WorkspacesPartSessions, Sessions: 4}, ""},
		"gitlab with email":  {WorkspacesTestOptions{Part: WorkspacesPartGitLab, Email: accessDefaultSecond}, ""},
		"sync with email":    {WorkspacesTestOptions{Part: WorkspacesPartSync, Email: accessDefaultSecond}, "takes no [email]"},
		"unknown provider":   {WorkspacesTestOptions{Part: WorkspacesPartSync, Provider: "bitbucket"}, "--provider \"bitbucket\""},
		"provider on gitlab": {WorkspacesTestOptions{Part: WorkspacesPartGitLab, Provider: config.WorkspaceProviderGitHub}, "--provider names"},
		"sessions on sync":   {WorkspacesTestOptions{Part: WorkspacesPartSync, Sessions: 4}, "--sessions is the sessions part's"},
		"too few sessions":   {WorkspacesTestOptions{Part: WorkspacesPartSessions, Sessions: 2}, "needs 3 or more"},
		"storage-only sync":  {WorkspacesTestOptions{Part: WorkspacesPartSync, StorageOnly: true}, "--storage-only"},
		"unknown part":       {WorkspacesTestOptions{Part: "access"}, "no part \"access\""},
	} {
		err := c.opts.validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: validate = %v, want %q", name, err, c.want)
		}
	}
}

func TestSyncScriptsNameTheScratchBranch(t *testing.T) {
	for name, script := range map[string]string{
		"borrow":   syncBorrowScript(proofPushed),
		"borrowed": syncBorrowedScript(proofPushed, "abc"),
		"stamps":   syncStampScript([]string{proofPushed}),
	} {
		if !strings.Contains(script, proofBranch) {
			t.Errorf("the %s script does not name %s:\n%s", name, proofBranch, script)
		}
		if strings.Contains(script, "refs/heads/"+syncMainBranch) {
			t.Errorf("the %s script reads the default branch by name: the parts never write it", name)
		}
	}
	if s := syncMirrorPresenceScript(proofPushed); !strings.Contains(s, syncMirrorDir(proofPushed)) {
		t.Fatalf("the presence script looks elsewhere:\n%s", s)
	}
}

func TestSyncFetchRetriesBounded(t *testing.T) {
	if wait := time.Duration(syncFetchTries-1) * syncFetchPause; wait > 2*time.Minute {
		t.Fatalf("the part waits up to %s for the provider's listing after a push", wait)
	}
}
