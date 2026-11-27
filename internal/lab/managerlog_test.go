package lab

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestManagerLoggedUser(t *testing.T) {
	const email, subject = "admin@lab.local", "CiQwOGE4Njg0Yi1kYjg4LTRiNzMtOTBhOS0zY2QxNjYxZjU0NjYSBWxvY2Fs"
	for _, tc := range []struct {
		name string
		line string
		want bool
	}{
		{"user subject", `level=INFO msg="agent created" user.issuer=https://dex.lab user.subject=` + subject + ` namespace=a`, true},
		{"quoted subject", `level=INFO msg="agent created" user.subject="` + subject + `"`, true},
		{"caller email", `level=INFO msg="agent created" caller=` + email + ` namespace=a`, true},
		{"another subject", `level=INFO msg="agent created" user.subject=other`, false},
		{"subject prefix only", `level=INFO msg="agent created" user.subject=` + subject[:10], false},
		{"longer subject", `level=INFO msg="agent created" user.subject=` + subject + `x`, false},
		{"no user", `level=INFO msg="agent created" namespace=a`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, managerLoggedUser(tc.line, email, subject))
		})
	}
}
