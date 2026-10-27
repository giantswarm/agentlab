package lab

import (
	"errors"
	"strings"
	"testing"
)

// P5 counts a session start on the other organization's Agent as refused
// when the controller refused it or the Kubernetes API refused agent-manager's
// read of that Agent as the person; a refusal of another namespace, another
// failure or a start that went through is not.
func TestSessionStartRefuser(t *testing.T) {
	const ns = "org-agentlab-tenants-b"
	cases := []struct {
		name    string
		refused error
		want    string
	}{
		{"controller", errors.New("POST /api/muster/call x_agent-manager_start_session answered 500: forbidden: kagent: PermissionDenied"), "the controller"},
		{"agent read", errors.New(`answered 500: forbidden: get agent ` + ns + `/agentlab-tenants-test: agents.api.kagent.dev "agentlab-tenants-test" is forbidden: User "oidc:alice@lab.local" cannot get resource "agents" in API group "api.kagent.dev" in the namespace "` + ns + `"`), "the Kubernetes API"},
		{"agent read elsewhere", errors.New(`answered 500: forbidden: get agent org-other/x: cannot get resource "agents" in API group "api.kagent.dev" in the namespace "org-other"`), ""},
		{"other failure", errors.New("answered 500: context deadline exceeded"), ""},
		{"not refused", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := sessionStartRefuser(c.refused, ns)
			if ok != (c.want != "") || !strings.HasPrefix(got, c.want) {
				t.Fatalf("sessionStartRefuser = %q %v, want %q", got, ok, c.want)
			}
		})
	}
}
