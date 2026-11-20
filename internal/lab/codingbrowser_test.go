package lab

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
)

func TestJudgeBrowserProof(t *testing.T) {
	readings := codingProbe{}
	for _, key := range []string{"fetch", "fetch-dot", "redirect", "redirect-dot", browserWorkerCase, "worker-dot", "suite", "suite-dot"} {
		readings["browser-"+key] = "net::ERR_NAME_NOT_RESOLVED"
	}
	readings["browser-preflight"] = "blocked"
	readings["browser-anonymous"] = "401"
	readings["browser-extra-header"] = "400"
	readings["browser-memory"] = "bounded"
	require.NoError(t, judgeBrowserProof(readings))
	for key := range readings {
		t.Run(key, func(t *testing.T) {
			missing := maps.Clone(readings)
			delete(missing, key)
			require.Error(t, judgeBrowserProof(missing))
			wrong := maps.Clone(readings)
			wrong[key] = "403"
			require.Error(t, judgeBrowserProof(wrong))
		})
	}
}

func TestBrowserMusterTarget(t *testing.T) {
	policy := &ateapi.EgressPolicy{Rules: []*ateapi.EgressRule{{Http: &ateapi.HTTPRule{
		Hostnames: []string{"muster.agent-platform.svc.cluster.local"},
		Ports:     &ateapi.Ports{Numbers: []int32{8090}},
		Effects:   &ateapi.HttpRuleEffects{ReplaceHeaders: []*ateapi.CredentialHeader{{Header: "Authorization", ActorJwt: &ateapi.ActorJWTSource{}}}},
	}}}}
	target, err := browserMusterTarget(policy)
	require.NoError(t, err)
	require.Equal(t, "http://muster.agent-platform.svc.cluster.local:8090/mcp", target)
	policy.Rules[0].Http.Effects.ReplaceHeaders[0].ActorJwt = nil
	_, err = browserMusterTarget(policy)
	require.ErrorContains(t, err, "actor-JWT")
	_, err = browserMusterTarget(nil)
	require.Error(t, err)
}

func TestJudgeBrowserAttachment(t *testing.T) {
	readings := codingProbe{"browser-person": "person", "browser-comment": "https://github.com/owner/repo/pull/1#issuecomment-1", "browser-upload": "attached", "browser-close": browserClosed}
	require.NoError(t, judgeBrowserAttachment(readings))
	for key := range readings {
		missing := maps.Clone(readings)
		delete(missing, key)
		require.Error(t, judgeBrowserAttachment(missing))
	}
	readings["browser-comment"] = "https://example.org/#issuecomment-1"
	require.Error(t, judgeBrowserAttachment(readings))
}
