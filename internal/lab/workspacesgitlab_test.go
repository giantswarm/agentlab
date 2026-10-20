package lab

import (
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
// the GitLab part judges: none, or the names that would carry a token.
func TestGitLabCredentialEnv(t *testing.T) {
	for names, want := range map[string][]string{
		"": nil,
		"CLAUDE_CODE_MESSAGING_TOKEN GH_TOKEN KAGENT_PROPAGATE_TOKEN ": nil,
		"GITLAB_TOKEN KAGENT_PROPAGATE_TOKEN":                          {"GITLAB_TOKEN"},
		"GIT_ASKPASS GLAB_TOKEN":                                       {"GIT_ASKPASS", "GLAB_TOKEN"},
	} {
		if got := gitlabCredentialEnv(names); !slices.Equal(got, want) {
			t.Errorf("gitlabCredentialEnv(%q) = %v, want %v", names, got, want)
		}
	}
}

// The part runs with the registration pinned to GitLab's application or
// with one App per instance, and skips with the pin on GitHub's.
func TestGitLabSkip(t *testing.T) {
	cfg := workspaceLab(config.WorkspaceProviderGitHub + "," + config.WorkspaceProviderGitLab)
	cfg.Platform.Workspaces.GitLab = testWorkspaceGitLab(testGrantClientID)
	for grant, runs := range map[string]bool{"": false, config.WorkspaceProviderGitHub: false, config.WorkspaceProviderGitLab: true, config.WorkspaceGrantProviderApps: true} {
		cfg.Platform.Workspaces.GrantProvider = grant
		if skip := gitlabSkip(cfg); (skip == "") != runs {
			t.Errorf("grantProvider %q: gitlabSkip = %q, want the part to run: %t", grant, skip, runs)
		}
	}
}
