package lab

import (
	"slices"
	"testing"
)

// testGitLabPerson is the GitLab username the tests sign in as.
const testGitLabPerson = "gitlab-person"

// A Session's credential-like variables beyond the platform's own are what
// the GitLab part judges: none, or the names that would carry a token.
func TestGitLabCredentialEnv(t *testing.T) {
	for names, want := range map[string][]string{
		"": nil,
		"CLAUDE_CODE_MESSAGING_TOKEN KAGENT_PROPAGATE_TOKEN ": nil,
		"GITLAB_TOKEN KAGENT_PROPAGATE_TOKEN":                 {"GITLAB_TOKEN"},
		"GIT_ASKPASS GLAB_TOKEN":                              {"GIT_ASKPASS", "GLAB_TOKEN"},
	} {
		if got := gitlabCredentialEnv(names); !slices.Equal(got, want) {
			t.Errorf("gitlabCredentialEnv(%q) = %v, want %v", names, got, want)
		}
	}
}
