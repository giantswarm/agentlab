package lab

import "net/http"

// userInstallations is GET /user/installations with the App's user token:
// the App's installations the signed-in user can access, which on GitHub
// are those on an owner the user holds a permission in; here an owner with
// a repository the fixture grants the user, so a user without one on an
// installed owner reaches none of it.
func (g *workspaceGitHub) userInstallations(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	if c.kind != credUser {
		badCredentials(w)
		return
	}
	reached := map[string]bool{}
	g.mu.Lock()
	for _, repo := range g.repos {
		if g.permission(c, repo) != "" {
			reached[repo.Owner] = true
		}
	}
	g.mu.Unlock()
	out := []map[string]any{}
	for _, o := range g.fixture.Owners {
		if o.Installation != 0 && reached[o.Login] {
			out = append(out, g.installationJSON(o))
		}
	}
	writeGitHubJSON(w, http.StatusOK, map[string]any{"total_count": len(out), "installations": out})
}
