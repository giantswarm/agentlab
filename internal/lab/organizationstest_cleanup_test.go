package lab

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOrganizationProofPart(t *testing.T) {
	for _, test := range []struct {
		name, part, want string
		wantError        bool
	}{
		{name: "all checks"},
		{name: "named check", part: "cleanup", want: organizationCleanupPart},
		{name: "compatible alias", part: "erasure", want: organizationCleanupPart},
		{name: "unsupported part", part: "invalid", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			part, err := organizationProofPart(test.part)
			if test.wantError {
				require.ErrorContains(t, err, "use cleanup or omit --part")
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, part)
		})
	}
}
