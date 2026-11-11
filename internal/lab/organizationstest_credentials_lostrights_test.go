package lab

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The lost-rights verdicts are a SKIP naming agent-manager#360 below the
// release that lets an admin outside platform-admins prepare; from it on
// they run.
func TestLostRightsSkip(t *testing.T) {
	for _, v := range []string{"2.23.1", "2.24.0-rc.0", "unreadable"} {
		require.Contains(t, lostRightsSkip(v), "giantswarm/agent-manager#360", v)
	}
	for _, v := range []string{credentialsWriteOnlyFloor, "2.24.0-rc.3", "2.24.1"} {
		require.Empty(t, lostRightsSkip(v), v)
	}
}

// A person who may still create Secrets or ModelConfigs after the binding's
// removal did not lose the right: the fixture is wrong.
func TestLostRightsFixtureProblem(t *testing.T) {
	require.Empty(t, lostRightsFixtureProblem("d@lab.test", "ns", organizationRights{listModelConfigs: true}))
	require.Empty(t, lostRightsFixtureProblem("d@lab.test", "ns", organizationRights{}))
	for name, r := range map[string]organizationRights{
		"still creates secrets":      {createSecrets: true},
		"still creates modelconfigs": {createModelConfigs: true},
		"still an admin":             {listModelConfigs: true, createSecrets: true, createModelConfigs: true},
	} {
		require.Contains(t, lostRightsFixtureProblem("d@lab.test", "ns", r), "fixture wrong", name)
	}
}

// restError is agent-manager's REST error body.
func restError(t *testing.T, code, message string) []byte {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	body.Error.Code, body.Error.Message = code, message
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return raw
}

// The refusal passes only as 403 forbidden on the save and on the read, the
// read naming the denied create of this operation's ModelConfig or Secret
// in the organization's namespace and the admin role. Another person's
// refusal (404), a stale ETag (412), an expired operation (409), an
// accepted save, and a refusal naming another object, namespace or guard
// fail.
func TestLostRightsRefusal(t *testing.T) {
	const ns, model, secret = "org-ns", "agentlab-credentials-lost", "tenant-credential-agentlab-credentials-lost" // #nosec G101 -- names, not credentials
	way := "; this needs agent-platform:admin in organization namespace org-ns; an admin of the organization adds you (add_organization_member), or an organizations-admin (agent-platform:organizations-admin) does"
	modelDenied := "forbidden: you may not create modelconfigs.api.kagent.dev/agentlab-credentials-lost in org-ns" + way
	secretDenied := "forbidden: you may not create secrets/tenant-credential-agentlab-credentials-lost in org-ns" + way
	save := restAnswer{code: http.StatusForbidden, body: restError(t, "forbidden", "forbidden: credential save was refused; refresh the operation before retrying")}
	read := func(message string) restAnswer {
		return restAnswer{code: http.StatusForbidden, body: restError(t, "forbidden", message)}
	}

	denied, err := lostRightsRefusal(ns, model, secret, save, read(modelDenied))
	require.NoError(t, err)
	require.Equal(t, "you may not create modelconfigs.api.kagent.dev/agentlab-credentials-lost in org-ns", denied)
	denied, err = lostRightsRefusal(ns, model, secret, save, read(secretDenied))
	require.NoError(t, err)
	require.Equal(t, "you may not create secrets/tenant-credential-agentlab-credentials-lost in org-ns", denied)

	for name, s := range map[string]restAnswer{
		"save accepted":            {code: http.StatusOK, body: []byte(`{"pending":false}`)},
		"another person's (404)":   {code: http.StatusNotFound, body: restError(t, "not_found", "not found: credential save was refused")},
		"stale ETag (412)":         {code: http.StatusPreconditionFailed, body: restError(t, "precondition_failed", "credential operation changed")},
		"expired operation (409)":  {code: http.StatusConflict, body: restError(t, "conflict", "conflict: credential save was refused")},
		"403 with another code":    {code: http.StatusForbidden, body: restError(t, "conflict", "conflict")},
		"403 without agent's body": {code: http.StatusForbidden, body: []byte("forbidden")},
	} {
		_, err := lostRightsRefusal(ns, model, secret, s, read(modelDenied))
		require.ErrorContains(t, err, "the save after the lost right", name)
	}
	for name, r := range map[string]restAnswer{
		"read allowed":      {code: http.StatusOK, body: []byte(`{"state":"pending"}`)},
		"read as not found": {code: http.StatusNotFound, body: restError(t, "not_found", "not found: credential operation")},
	} {
		_, err := lostRightsRefusal(ns, model, secret, save, r)
		require.ErrorContains(t, err, "the read of their own operation", name)
	}
	for name, message := range map[string]string{
		"another namespace": strings.ReplaceAll(modelDenied, "org-ns", "other-ns"),
		"another model":     strings.ReplaceAll(modelDenied, "/agentlab-credentials-lost", "/agentlab-credentials-other"),
		"another verb":      strings.ReplaceAll(modelDenied, "may not create", "may not update"),
		"another guard":     "forbidden: organization agentlab-credentials is being deleted",
		"no role named":     "forbidden: you may not create modelconfigs.api.kagent.dev/agentlab-credentials-lost in org-ns;",
	} {
		_, err := lostRightsRefusal(ns, model, secret, save, read(message))
		require.ErrorContains(t, err, "unrelated guard", name)
	}
}

// The refused save leaves the operation pending at the same ETag; another
// state or ETag means the refusal changed it.
func TestOperationUnchanged(t *testing.T) {
	require.NoError(t, operationUnchanged(`"7"`, operationPending, `"7"`))
	require.Error(t, operationUnchanged(`"7"`, operationPending, `"8"`))
	require.Error(t, operationUnchanged(`"7"`, operationCancelled, `"7"`))
	require.Error(t, operationUnchanged(`"7"`, "uncertain", `"7"`))
}
