package lab

import (
	"slices"
	"strings"
	"testing"
)

// The branches the commit stage deletes are agent-manager's for its own
// agent, whatever the verb; another agent's branch and other branches stay.
func TestCommitStageBranches(t *testing.T) {
	listing := strings.Join([]string{
		"1111\trefs/heads/agent-manager/create-kagent-agentlab-commit",
		"2222\trefs/heads/agent-manager/update-kagent-agentlab-commit",
		"3333\trefs/heads/agent-manager/delete-kagent-agentlab-commit",
		"4444\trefs/heads/agent-manager/create-kagent-someone-else",
		"5555\trefs/heads/agent-manager/create-kagent-agentlab-commit-seed",
		"6666\trefs/heads/main",
	}, "\n")
	got := commitStageBranches(listing)
	want := []string{"agent-manager/create-kagent-agentlab-commit", "agent-manager/delete-kagent-agentlab-commit", "agent-manager/update-kagent-agentlab-commit"}
	if !slices.Equal(got, want) {
		t.Errorf("commitStageBranches = %v, want %v", got, want)
	}
}

// testDefaultBranch is the fixture repository's default branch in the tests.
const testDefaultBranch = "main"

func TestDefaultBranchOf(t *testing.T) {
	if got := defaultBranchOf("ref: refs/heads/" + testDefaultBranch + "\tHEAD\nabc123\tHEAD\n"); got != testDefaultBranch {
		t.Errorf("defaultBranchOf = %q, want %s", got, testDefaultBranch)
	}
	if got := defaultBranchOf("abc123\tHEAD\n"); got != "" {
		t.Errorf("defaultBranchOf without a symref = %q, want none", got)
	}
}

// The fixture's GitRepository is suspended, on the repository's GitHub URL
// at the branch, and agentlab's.
func TestGitRepositoryManifest(t *testing.T) {
	m := string(gitRepositoryManifest("giantswarm/workspace-proof", testDefaultBranch))
	for _, want := range []string{"kind: GitRepository", "name: " + gitopsFluxFixture, "suspend: true", "url: https://github.com/giantswarm/workspace-proof", "branch: " + testDefaultBranch, managedByLabel + ": " + managedByAgentlabValue} {
		if !strings.Contains(m, want) {
			t.Errorf("the GitRepository lacks %q:\n%s", want, m)
		}
	}
}
