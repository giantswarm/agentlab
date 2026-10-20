package lab

import (
	"maps"
	"slices"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// testGitLabPerson is the GitLab username the tests sign in as, at the
// GitLab testGitLabURL.
const (
	testGitLabPerson = "gitlab-person"
	testGitLabURL    = "https://gitlab.example.com"
)

// testWorkspaceGitLab is the gitlab instance at testGitLabURL with the
// OAuth application clientID; testWorkspaceGitLabAt the same at url.
func testWorkspaceGitLab(clientID string) config.WorkspaceGitLab {
	return testWorkspaceGitLabAt(testGitLabURL, clientID)
}

func testWorkspaceGitLabAt(url, clientID string) config.WorkspaceGitLab {
	return config.WorkspaceGitLab{URL: url, ClientID: clientID, Group: "proof-group", Project: "proof", Person: testGitLabPerson}
}

// A Session's credential-like variables beyond the platform's own are what
// the GitLab part judges: none, or the names that would carry a token. A
// GITLAB_TOKEN is the platform's own only as the Harness's placeholder.
func TestGitLabCredentialEnv(t *testing.T) {
	for _, tc := range []struct {
		names, gitlabToken string
		want               []string
	}{
		{"", gitlabTokenUnset, nil},
		{"CLAUDE_CODE_MESSAGING_TOKEN GH_TOKEN KAGENT_PROPAGATE_TOKEN ", gitlabTokenUnset, nil},
		{"GH_TOKEN GITLAB_TOKEN GLAB_CONFIG_DIR GLAB_IS_OAUTH2 KAGENT_PROPAGATE_TOKEN", gitlabTokenPlaceholder, nil},
		{"GITLAB_TOKEN KAGENT_PROPAGATE_TOKEN", gitlabTokenForeign, []string{"GITLAB_TOKEN"}},
		{"GIT_ASKPASS GLAB_TOKEN", gitlabTokenUnset, []string{"GIT_ASKPASS", "GLAB_TOKEN"}},
	} {
		if got := gitlabCredentialEnv(tc.names, tc.gitlabToken); !slices.Equal(got, tc.want) {
			t.Errorf("gitlabCredentialEnv(%q, %q) = %v, want %v", tc.names, tc.gitlabToken, got, tc.want)
		}
	}
}

// The Session's merge request is judged only on a Harness that gives glab
// its placeholder, and then must be the person's, opened and closed.
func TestJudgeSessionMergeRequest(t *testing.T) {
	gp := &gitlabProof{repo: "group/project", gitlabHost: "gitlab.example.com"}
	gp.gl.Person = testGitLabPerson
	person := `"username":"` + testGitLabPerson + `"`
	good := map[string]string{
		gitlabProbeGitLabToken: gitlabTokenPlaceholder,
		gitlabProbeGlabUser:    "rc0 " + person,
		gitlabProbeMRCreate:    "rc0 https://gitlab.example.com/group/project/-/merge_requests/7",
		gitlabProbeMRAuthor:    person,
		gitlabProbeMRClose:     `rc0 "state":"closed"`,
	}
	with := func(key, value string) map[string]string {
		probe := maps.Clone(good)
		probe[key] = value
		return probe
	}
	for name, tc := range map[string]struct {
		probe   map[string]string
		wantErr bool
	}{
		"the person's merge request":    {good, false},
		"no placeholder, not judged":    {map[string]string{gitlabProbeGitLabToken: gitlabTokenUnset, gitlabProbeGlabUser: "rc1 401"}, false},
		"glab unauthenticated":          {with(gitlabProbeGlabUser, "rc1 401 Unauthorized"), true},
		"glab as someone else":          {with(gitlabProbeGlabUser, `rc0 "username":"other"`), true},
		"merge request refused":         {with(gitlabProbeMRCreate, "rc1 403 Forbidden"), true},
		"merge request of another":      {with(gitlabProbeMRAuthor, `"username":"other"`), true},
		"merge request left open":       {with(gitlabProbeMRClose, `rc0 "state":"opened"`), true},
		"merge request of another repo": {with(gitlabProbeMRCreate, "rc0 https://gitlab.example.com/other/project/-/merge_requests/7"), true},
	} {
		if err := gp.judgeSessionMergeRequest(tc.probe); (err != nil) != tc.wantErr {
			t.Errorf("%s: judgeSessionMergeRequest() = %v, want error %v", name, err, tc.wantErr)
		}
	}
}
