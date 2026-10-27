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
			proof:  &workspaceManagerProof{providers: []string{"github (github, connected)", "gitlab (gitlab, not connected)"}, viewer: "refused as not a member: x"},
			agents: true,
			want: []string{
				"PASS: the workspace-manager is registered with muster (MCPServer workspace-manager Connected), cleans session directories up from kagent's Sessions,",
				"lists its 2 provider instances to the admin: github (github, connected), gitlab (gitlab, not connected)",
				"list_workspaces for Organization " + config.WorkspaceOrganization + " answers a member and keeps a viewer out (refused as not a member: x)",
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
			wantNever: []string{"lists its", "answers a member", "keeps a viewer out"},
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

// The viewer, outside the lab's Organization, is kept out of list_workspaces
// either by the workspace-manager's refusal (they hold the pinned App's
// grant) or by muster's auth_required with its sign-in link (they do not);
// anything else fails the stage.
func TestViewerKeptOut(t *testing.T) {
	const link = "https://muster.lab.local/oauth/proxy/start?server=workspace-manager"
	answer := func(isError bool, text string, structured map[string]any) *toolEnvelope {
		env := &toolEnvelope{IsError: isError, StructuredContent: structured}
		env.Content = append(env.Content, struct {
			Text string `json:"text"`
		}{Text: text})
		return env
	}
	tests := []struct {
		name    string
		env     *toolEnvelope
		err     error
		want    string
		wantErr string
	}{
		{
			name: "granted viewer: refused as not a member",
			env:  answer(true, "viewer@lab.local is not a member of Organization lab", nil),
			want: "refused as not a member: viewer@lab.local is not a member",
		},
		{
			name: "ungranted viewer: muster's auth_required with its sign-in link",
			env:  answer(true, "auth_required: server 'workspace-manager' requires authentication before its tools can be called; sign in at "+link, nil),
			want: "asked to grant the pinned App first, muster's sign-in link " + link,
		},
		{
			name: "ungranted viewer: the link as authUrl",
			env:  answer(true, "auth_required", map[string]any{"authUrl": link}),
			want: "asked to grant the pinned App first, muster's sign-in link " + link,
		},
		{
			name:    "auth_required without a sign-in link",
			env:     answer(true, "auth_required: server 'workspace-manager' requires authentication", nil),
			wantErr: "want a refusal as not a member, or muster's auth_required with its sign-in link",
		},
		{
			name:    "the viewer listed workspaces",
			env:     answer(false, "0 workspaces", nil),
			wantErr: `answered "0 workspaces"`,
		},
		{
			name:    "the call failed",
			err:     errEmptyToolAnswer,
			wantErr: errEmptyToolAnswer.Error(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := viewerKeptOut(tt.env, tt.err)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("viewerKeptOut = %q, %v; want an error with %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || !strings.HasPrefix(got, tt.want) {
				t.Fatalf("viewerKeptOut = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
