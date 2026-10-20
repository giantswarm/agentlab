package lab

import (
	"net/http"
	"slices"
	"testing"
)

// TestWorkspaceGitHubUserInstallations: a user's App token reaches the
// installations on the owners the fixture grants it a repository of, never
// the owner the App is not installed on; any other credential is refused.
func TestWorkspaceGitHubUserInstallations(t *testing.T) {
	f := startWorkspaceFake(t)
	reached := func(login string) []string {
		t.Helper()
		token, _ := f.signIn(t, login)[wsAccess].(string)
		var out struct {
			TotalCount    int `json:"total_count"`
			Installations []struct {
				ID      int64 `json:"id"`
				Account struct {
					Login string `json:"login"`
					Type  string `json:"type"`
				} `json:"account"`
			} `json:"installations"`
		}
		if resp := f.call(t, http.MethodGet, "/api/v3/user/installations", token, nil, &out); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /user/installations as %s = %d", login, resp.StatusCode)
		}
		var logins []string
		for _, in := range out.Installations {
			if in.Account.Type != githubOrganization || in.ID == 0 {
				t.Fatalf("installation %+v", in)
			}
			logins = append(logins, in.Account.Login)
		}
		if out.TotalCount != len(logins) {
			t.Fatalf("total_count %d for %d installations", out.TotalCount, len(logins))
		}
		slices.Sort(logins)
		return logins
	}
	if got := reached("admin"); !slices.Equal(got, []string{"agentlab-org", "agentlab-oss"}) {
		t.Fatalf("admin reaches %v, want agentlab-org and agentlab-oss, not agentlab-ext without the App", got)
	}
	if got := reached(wsDev); !slices.Equal(got, []string{"agentlab-org", "agentlab-oss"}) {
		t.Fatalf("dev reaches %v, want agentlab-org and agentlab-oss", got)
	}
	if got := reached("viewer"); len(got) != 0 {
		t.Fatalf("viewer, granted no repository, reaches %v, want none", got)
	}
	if resp := f.call(t, http.MethodGet, "/api/v3/user/installations", "", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a token = %d, want 401", resp.StatusCode)
	}
}
