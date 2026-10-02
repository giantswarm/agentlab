package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// testDevelopers is the lab users' shared group in these tests.
const testDevelopers = "developers"

func TestBeekeeperUsersShareAGroup(t *testing.T) {
	cfg := &config.Config{Users: []config.User{
		{Email: "viewer@lab.local", Groups: []string{"viewers"}},
		{Email: testPortalUser, Groups: []string{"platform-admins", testDevelopers}},
		{Email: "dev@lab.local", Groups: []string{testDevelopers}},
	}}
	asker, other, group, err := beekeeperUsers(cfg)
	if err != nil || asker.Email != testPortalUser || other.Email != "dev@lab.local" || group != testDevelopers {
		t.Fatalf("%v %v %q %v", asker, other, group, err)
	}
	cfg.Users = cfg.Users[:2]
	if _, _, _, err := beekeeperUsers(cfg); err == nil {
		t.Fatal("no shared group: no error")
	}
}

func TestBeekeeperServeConfig(t *testing.T) {
	cfg := &config.Config{DexPort: 32100}
	got := beekeeperServeConfig(cfg, testDevelopers, map[string]string{"asker": testPortalUser}, "CAGENTLABX", "http://127.0.0.1:18090", "/run/token")
	for _, want := range []string{
		`issuer: "https://localhost:32100/dex"`,
		`organization: "` + testDevelopers + `"`,
		`teams: {"` + testDevelopers + `": "agentlab"}`,
		`asker: "` + testPortalUser + `"`,
		`channels: {"agentlab": "CAGENTLABX"}`,
		`url: "http://127.0.0.1:18090"`,
		`tokenFile: "/run/token"`,
		`answerTool: "x_beekeeper_note_answer"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("serve's configuration lacks %s:\n%s", want, got)
		}
	}
}
