package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// The stage's verdict claims only what ran: list_providers and
// list_workspaces PASS once the people granted the pinned App, and SKIP with
// the sign-in link while one has not.
func TestWorkspaceManagerVerdict(t *testing.T) {
	const link = "https://muster.lab.local/oauth/proxy/start?server=workspace-manager"
	tests := []struct {
		name      string
		proof     *workspaceManagerProof
		agents    bool
		want      []string
		wantNever []string
	}{
		{
			name:   "granted: the checks ran",
			proof:  &workspaceManagerProof{providers: []string{"github (github, connected)", "gitlab (gitlab, not connected)"}},
			agents: true,
			want: []string{
				"PASS: the workspace-manager is registered with muster (MCPServer workspace-manager Connected), cleans session directories up from kagent's Sessions,",
				"lists its 2 provider instances to the admin: github (github, connected), gitlab (gitlab, not connected)",
				"list_workspaces for Organization " + config.WorkspaceOrganization + " answers a member and refuses a viewer",
			},
			wantNever: []string{"SKIP", "sign-in link"},
		},
		{
			name:   "not granted: the checks skipped, the sign-in link named",
			proof:  &workspaceManagerProof{ungranted: &workspaceGrantStop{pinned: "github-org", email: "viewer@lab.local", link: link}},
			agents: false,
			want: []string{
				"PASS: the workspace-manager is registered with muster (MCPServer workspace-manager Connected) (no kagent: the agents are off) and asserts muster's sign-in link for viewer@lab.local, who has not granted the pinned App of github-org",
				"\nSKIP: the workspace-manager's list_providers and list_workspaces for Organization " + config.WorkspaceOrganization + " — viewer@lab.local has not granted the pinned App of github-org",
				"muster's sign-in link: " + link,
			},
			wantNever: []string{"lists its", "answers a member", "refuses a viewer"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := workspaceManagerVerdict(tt.proof, tt.agents)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("verdict %q\nwants %q", got, w)
				}
			}
			for _, n := range tt.wantNever {
				if strings.Contains(got, n) {
					t.Errorf("verdict %q\nclaims %q, which did not run", got, n)
				}
			}
		})
	}
}
