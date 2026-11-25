package lab

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestOrganizationProofPart(t *testing.T) {
	for _, test := range []struct {
		name, part, want string
		wantError        bool
	}{
		{name: "all checks"},
		{name: "named check", part: "cleanup", want: organizationCleanupPart},
		{name: "compatible alias", part: "erasure", want: organizationCleanupPart},
		{name: "credentials", part: organizationCredentialsPart, want: organizationCredentialsPart},
		{name: "admins", part: organizationAdminsPart, want: organizationAdminsPart},
		{name: "unsupported part", part: "invalid", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			part, err := organizationProofPart(test.part)
			if test.wantError {
				require.ErrorContains(t, err, "use cleanup, credentials or admins, or omit --part")
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, part)
		})
	}
}

func TestDeleteIntentRefusal(t *testing.T) {
	organizations := schema.ParseGroupResource(organizationResource)
	forbidden := func(message string) error {
		return apierrors.NewForbidden(organizations, organizationsTestA, errors.New(message))
	}
	for name, test := range map[string]struct {
		err      error
		accepted bool
	}{
		"refused until the delete pull request merged": {err: forbidden("ValidatingAdmissionPolicy 'agent-platform-organization-delete-intent' with binding 'agent-platform-organization-delete-intent' denied request: an Organization applied from git is erased only once its delete pull request merged (Orphaned, reason DeleteMerged): delete it with agent-manager in mode commit, then erase it; refused: agentlab-a"), accepted: true},
		"admitted":                             {},
		"refused for the missing prune marker": {err: forbidden("ValidatingAdmissionPolicy 'agent-platform-organization-delete-intent' with binding 'agent-platform-organization-delete-intent' denied request: deleting an Organization with the Agent Platform enabled erases its Agent Platform: set the annotation kustomize.toolkit.fluxcd.io/prune: enabled on agentlab-a first")},
		"refused by another policy":            {err: forbidden("ValidatingAdmissionPolicy 'agent-platform-organization-names' with binding 'agent-platform-organization-names' denied request: DeleteMerged")},
		"refused by RBAC":                      {err: forbidden(`User "oidc:admin@lab.local" cannot delete resource "organizations"`)},
	} {
		t.Run(name, func(t *testing.T) {
			err := deleteIntentRefusal("the delete", test.err)
			if test.accepted {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
		})
	}
}
