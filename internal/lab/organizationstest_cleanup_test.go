package lab

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// TestRequireSessionErasedRetriesTransportErrorsOnly: the cleanup proof's
// reads of the erased session try a transport error (Unavailable, the
// controller restarting) again within endStateReadTimeout, and judge every
// other answer at once: NotFound and PermissionDenied pass, a session still
// there or another error fails. Past the bound the proof fails with the
// last answer and the bound.
func TestRequireSessionErasedRetriesTransportErrorsOnly(t *testing.T) {
	timeout, backoff := endStateReadTimeout, sessionSettleBackoff
	t.Cleanup(func() { endStateReadTimeout, sessionSettleBackoff = timeout, backoff })
	endStateReadTimeout, sessionSettleBackoff = 500*time.Millisecond, 10*time.Millisecond
	reset := status.Error(codes.Unavailable, "upstream call failed: SendRequest: connection error: connection reset")
	for _, tc := range []struct {
		name          string
		live          bool
		get, resume   []error
		wantCalls     []string
		wantErr       string
		wantErrorCode codes.Code
	}{
		{name: "a restart before each read is waited out", get: []error{reset, reset}, resume: []error{reset},
			wantCalls: []string{getSessionMethod, getSessionMethod, getSessionMethod, resumeSessionMethod, resumeSessionMethod}},
		{name: "PermissionDenied on the read passes at once", get: []error{status.Error(codes.PermissionDenied, "no")},
			wantCalls: []string{getSessionMethod, resumeSessionMethod}},
		{name: "a session still there fails at once", live: true,
			wantCalls: []string{getSessionMethod}, wantErr: "retains session"},
		{name: "an unknown read error fails at once", get: []error{status.Error(codes.Internal, "boom")},
			wantCalls: []string{getSessionMethod}, wantErr: "boom", wantErrorCode: codes.Internal},
		{name: "an unknown resume error fails at once", resume: []error{status.Error(codes.Internal, "boom")},
			wantCalls: []string{getSessionMethod, resumeSessionMethod}, wantErr: "not proven unrunnable", wantErrorCode: codes.Internal},
		{name: "a transport error past the bound fails with it", get: slices.Repeat([]error{reset}, 1000),
			wantErr: "no final answer within 500ms", wantErrorCode: codes.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeKagent()
			if tc.live {
				f.sessions[fakeSessionID] = fakeSession(fakeSessionID, fakeToken, a2aTestAgent)
			}
			f.answers = map[string][]error{getSessionMethod: tc.get, resumeSessionMethod: tc.resume}
			api := f.serve(t, fakeToken)
			started := time.Now()
			err := requireSessionErased(t.Context(), api, fakeSessionID)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
				if tc.wantErrorCode != codes.OK {
					require.Equal(t, tc.wantErrorCode, status.Code(err))
				}
			}
			if tc.wantCalls != nil {
				require.Equal(t, tc.wantCalls, f.order)
			}
			require.Less(t, time.Since(started), 2*endStateReadTimeout)
		})
	}
}
