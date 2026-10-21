package lab

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gitWritten is a write turn's lines of a run that passes.
func gitWritten() map[string]string {
	return map[string]string{
		"pwd": "/workspace", gitProbeClone: "./o/r", gitProbeGHToken: gitTokenPlaceholder, gitProbeTokenEnv: "", gitProbeTokenFiles: "",
		gitProbeCommitter: testPortalUser, gitProbePush: "rc0", gitProbeGHUser: "rc0 octo User",
		gitProbePRCreate: "rc0 https://github.com/o/r/pull/7", gitProbePRAuthor: "rc0 octo https://github.com/o/r/pull/7",
		gitProbePushActor: "octo", gitProbePRClose: "rc0 ✓ Closed pull request o/r#7",
	}
}

func TestJudgeGitWrite(t *testing.T) {
	login, pr, err := judgeGitWrite(gitWritten(), "o/r", testPortalUser)
	if err != nil || login != "octo" || pr != "https://github.com/o/r/pull/7" {
		t.Fatalf("judgeGitWrite() = %q, %q, %v; want octo, the pull request, nil", login, pr, err)
	}
	for _, tc := range []struct {
		name    string
		change  map[string]string
		wantErr string
	}{
		{"no clone", map[string]string{gitProbeClone: probeNone}, "holds no clone"},
		{"a token in the environment", map[string]string{gitProbeTokenEnv: "AGENT_SECRET_VAR"}, "a GitHub token in the sandbox"},
		{"a token in a file", map[string]string{gitProbeTokenFiles: "/data/x"}, "a GitHub token in the sandbox"},
		{"GH_TOKEN unset", map[string]string{gitProbeGHToken: gitTokenUnset}, "want the Harness's placeholder"},
		{"another committer", map[string]string{gitProbeCommitter: "agentlab@lab.local"}, "committer"},
		{"the push refused", map[string]string{gitProbePush: "rc128 fatal: Authentication failed"}, "set no person's credential"},
		{"an App's bot", map[string]string{gitProbeGHUser: "rc0 lab-agents[bot] Bot"}, "want a person's login"},
		{"gh api user refused", map[string]string{gitProbeGHUser: "rc1 HTTP 403"}, "want a person's login"},
		{"the pull request refused", map[string]string{gitProbePRCreate: "rc1 GraphQL: denied"}, "gh pr create answered"},
		{"another author", map[string]string{gitProbePRAuthor: "rc0 someone https://github.com/o/r/pull/7"}, "someone else's credential"},
		{"another actor", map[string]string{gitProbePushActor: "someone"}, "push's actor"},
		{"not closed", map[string]string{gitProbePRClose: "rc1 failed"}, "closing the pull request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := gitWritten()
			maps.Copy(v, tc.change)
			if _, _, err := judgeGitWrite(v, "o/r", testPortalUser); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("judgeGitWrite() = %v, want an error with %q", err, tc.wantErr)
			}
		})
	}
}

func TestJudgeGitBare(t *testing.T) {
	refused := map[string]string{gitProbeGHUser: "rc1 HTTP 401: Bad credentials", gitProbePush: "rc128 fatal: Authentication failed"}
	if err := judgeGitBare(refused); err != nil {
		t.Fatalf("judgeGitBare(refused) = %v", err)
	}
	for _, tc := range []struct {
		name    string
		change  map[string]string
		wantErr string
	}{
		{"gh as someone", map[string]string{gitProbeGHUser: "rc0 octo User"}, "reached GitHub as someone"},
		{"the push accepted", map[string]string{gitProbePush: "rc0"}, "pushed"},
		{"a token", map[string]string{gitProbeTokenFiles: "/data/x"}, "a GitHub token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := maps.Clone(refused)
			maps.Copy(v, tc.change)
			if err := judgeGitBare(v); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("judgeGitBare() = %v, want an error with %q", err, tc.wantErr)
			}
		})
	}
}

// runGitShell runs a part's command in dir with HOME at home and returns
// its marker lines.
func runGitShell(t *testing.T, dir, home, command string) map[string]string {
	t.Helper()
	cmd := exec.Command("sh", "-c", command) // #nosec G204 -- the part's own command
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@lab.local", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@lab.local")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the command failed: %v\n%s", err, out)
	}
	return parseProbeLines(string(out))
}

func TestGitPauseAndResumeCommands(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	// The Harness's clone: a clone with no working tree whose local branch
	// is deleted, origin/HEAD its only head.
	origin, clone := filepath.Join(t.TempDir(), "origin"), filepath.Join(dir, "o", "r")
	for _, args := range [][]string{
		{"init", "-q", "-b", "trunk", origin}, {"-C", origin, "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "seed"},
		{"clone", "-q", "--no-checkout", origin, clone}, {"-C", clone, "update-ref", "-d", "refs/heads/trunk"},
	} {
		cmd := exec.Command("git", args...) // #nosec G204 -- the test's own git arguments
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@lab.local", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@lab.local")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	paused := runGitShell(t, dir, dir, gitPauseCommand("o/r", "b-pause", "tok"))
	if err := judgeGitPaused(paused, "b-pause", "tok"); err != nil {
		t.Fatalf("after the pause command: %v (%v)", err, paused)
	}
	if paused["pwd"] != dir {
		t.Errorf("pwd = %q, want %s", paused["pwd"], dir)
	}
	resumed := runGitShell(t, dir, dir, gitResumeCommand("o/r"))
	if err := judgeGitPaused(resumed, "b-pause", "tok"); err != nil {
		t.Fatalf("after the resume command: %v (%v)", err, resumed)
	}
	if err := judgeGitPaused(resumed, "b-other", "tok"); err == nil {
		t.Errorf("judgeGitPaused accepted another branch")
	}
}

func TestGitShellTokenScan(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	clean := runGitShell(t, dir, home, gitShell+`true`)
	if clean[gitProbeTokenEnv] != "" || clean[gitProbeTokenFiles] != "" {
		t.Fatalf("a clean sandbox reads tokens: %v", clean)
	}
	// A token assembled at run time, so no file of the repository holds one.
	token := "ghu_" + strings.Repeat("A1b2", 9)
	if err := os.WriteFile(filepath.Join(home, "hosts.yml"), []byte("oauth_token: "+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTLAB_TEST_LEAK", token)
	found := runGitShell(t, dir, home, gitShell+`true`)
	if found[gitProbeTokenEnv] != "AGENTLAB_TEST_LEAK" || !strings.Contains(found[gitProbeTokenFiles], "hosts.yml") {
		t.Fatalf("the scan found %v, want the variable and the file", found)
	}
	if strings.Contains(strings.Join(slices.Collect(maps.Values(found)), " "), token) {
		t.Fatalf("the scan printed the token's value")
	}
}

func TestGitCommandsNameTheirLines(t *testing.T) {
	write := gitWriteCommand("o/r", "b")
	for _, key := range []string{gitProbeClone, gitProbePush, gitProbeCommitter, gitProbeGHUser, gitProbePRCreate, gitProbePRAuthor, gitProbePushActor, gitProbePRClose, gitProbeGHToken, gitProbeTokenEnv, gitProbeTokenFiles} {
		if !strings.Contains(write, " "+key) {
			t.Errorf("the write command prints no %s line", key)
		}
	}
	if !strings.Contains(write, "--delete-branch") || !strings.Contains(write, "refs/heads/b") {
		t.Errorf("the write command does not push b and delete it with its pull request:\n%s", write)
	}
	bare := gitBareCommand("o/r", "b-bare")
	if !strings.Contains(bare, "https://github.com/o/r.git") || strings.Contains(bare, "pr create") {
		t.Errorf("the bare command:\n%s", bare)
	}
}
