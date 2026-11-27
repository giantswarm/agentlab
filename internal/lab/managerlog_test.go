package lab

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestManagerLoggedUser(t *testing.T) {
	const email, subject = "admin@lab.local", "CiQwOGE4Njg0Yi1kYjg4LTRiNzMtOTBhOS0zY2QxNjYxZjU0NjYSBWxvY2Fs"
	const issuer = "https://dex.lab"
	for _, tc := range []struct {
		name string
		line string
		want bool
	}{
		{"user subject", `level=INFO msg="agent created" user.issuer=https://dex.lab user.subject=` + subject + ` namespace=a`, true},
		{"quoted identity", `level=INFO msg="agent created" user.issuer="https://dex.lab" user.subject="` + subject + `"`, true},
		{"caller email", `level=INFO msg="agent created" caller=` + email + ` namespace=a`, true},
		{"another subject", `level=INFO msg="agent created" user.subject=other`, false},
		{"subject prefix only", `level=INFO msg="agent created" user.subject=` + subject[:10], false},
		{"longer subject", `level=INFO msg="agent created" user.subject=` + subject + `x`, false},
		{"another issuer", `level=INFO msg="agent created" user.issuer=https://other.lab user.subject=` + subject, false},
		{"missing issuer", `level=INFO msg="agent created" user.subject=` + subject, false},
		{"missing subject", `level=INFO msg="agent created" user.issuer=https://dex.lab`, false},
		{"contradictory caller", `level=INFO msg="agent created" user.issuer=https://other.lab user.subject=` + subject + ` caller=` + email, false},
		{"identity in message", `level=INFO msg="user.issuer=https://dex.lab user.subject=` + subject + `"`, false},
		{"caller prefix", `level=INFO msg="agent created" caller=` + email + `.other`, false},
		{"no user", `level=INFO msg="agent created" namespace=a`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, managerLoggedUser(tc.line, email, issuer, subject))
		})
	}
}
