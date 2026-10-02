package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

func TestBeekeeperUsersShareAGroup(t *testing.T) {
	cfg := &config.Config{Users: []config.User{
		{Email: "viewer@lab.local", Groups: []string{"viewers"}},
		{Email: "admin@lab.local", Groups: []string{"platform-admins", "developers"}},
		{Email: "dev@lab.local", Groups: []string{"developers"}},
	}}
	asker, other, group, err := beekeeperUsers(cfg)
	if err != nil || asker.Email != "admin@lab.local" || other.Email != "dev@lab.local" || group != "developers" {
		t.Fatalf("%v %v %q %v", asker, other, group, err)
	}
	cfg.Users = cfg.Users[:2]
	if _, _, _, err := beekeeperUsers(cfg); err == nil {
		t.Fatal("no shared group: no error")
	}
}

func TestBeekeeperServeConfig(t *testing.T) {
	cfg := &config.Config{DexPort: 32100}
	got := beekeeperServeConfig(cfg, "developers", map[string]string{"asker": "admin@lab.local"}, "CAGENTLABX", "http://127.0.0.1:18090", "/run/token")
	for _, want := range []string{
		`issuer: "https://localhost:32100/dex"`,
		`organization: "developers"`,
		`teams: {"developers": "agentlab"}`,
		`asker: "admin@lab.local"`,
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
