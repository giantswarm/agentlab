package lab

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestSwitchOffRefusal: the switch-off verdict passes on the refusal of the
// chart's switch-off policy only; an admitted update, another policy's
// refusal, RBAC's refusal and an error that is no refusal fail it.
func TestSwitchOffRefusal(t *testing.T) {
	organizations := schema.ParseGroupResource(organizationResource)
	forbidden := func(message string) error {
		return apierrors.NewForbidden(organizations, adminsOrgA, errors.New(message))
	}
	for name, test := range map[string]struct {
		err      error
		accepted bool
	}{
		"refused by the switch-off policy":    {err: forbidden("ValidatingAdmissionPolicy 'agent-platform-organization-switch-off' with binding 'agent-platform-organization-switch-off' denied request: the Agent Platform cannot be switched off for an organization yet: keep spec.capabilities.agentPlatform.enabled: true; refused: agentlab-admins-a"), accepted: true},
		"admitted":                            {},
		"refused by another policy":           {err: forbidden("ValidatingAdmissionPolicy 'agent-platform-organization-names' with binding 'agent-platform-organization-names' denied request: an Organization with the Agent Platform enabled has a name that makes agents-<name> a namespace name; refused: agentlab-admins-a")},
		"refused by RBAC":                     {err: forbidden(`User "oidc:admin@lab.local" cannot patch resource "organizations" in API group "security.giantswarm.io" at the cluster scope`)},
		"an internal error naming the policy": {err: apierrors.NewInternalError(errors.New("ValidatingAdmissionPolicy 'agent-platform-organization-switch-off' with binding 'agent-platform-organization-switch-off': etcdserver: request timed out"))},
	} {
		t.Run(name, func(t *testing.T) {
			err := switchOffRefusal("the update", test.err)
			if test.accepted {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
		})
	}
}
