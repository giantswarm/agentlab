package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// skillManagerTestChart is a released chart with the skill-manager;
// skillManagerGitHub the chart's key of its GitHub credential.
const (
	skillManagerTestChart = "4.178.0"
	skillManagerGitHub    = "github"
)

func skillManagerTestConfig() *config.Config {
	cfg := gitHubTokenTestConfig()
	cfg.Platform.ChartVersion = skillManagerTestChart
	return cfg
}

// TestSkillManagerReadsWithTheLabsGitHubCredential: the skill-manager lists
// and pins skills with the credential agent-manager has, the App when one is
// recorded (the chart refuses both), else the token, by the Secret's name.
func TestSkillManagerReadsWithTheLabsGitHubCredential(t *testing.T) {
	cfg := skillManagerTestConfig()
	cfg.GitHubToken.Source = gitHubTokenTestSource
	s := renderGitHubTokenSurfaces(t, cfg)
	token := []string{componentSkillManager, skillManagerGitHub, "tokenSecret"}
	if got := dig(s.values, append(token, nameKey)...); got != gitHubTokenSecret {
		t.Errorf("skill-manager.github.tokenSecret.name = %v, want %s\n%s", got, gitHubTokenSecret, s.valuesRaw)
	}
	if got := dig(s.values, append(token, fieldKey)...); got != gitHubTokenSecretKey {
		t.Errorf("skill-manager.github.tokenSecret.key = %v, want %s", got, gitHubTokenSecretKey)
	}

	cfg = skillManagerTestConfig()
	cfg.GitHubToken.Source = gitHubTokenTestSource
	cfg.GitHubApp = gitHubAppTestConfig().GitHubApp
	s = renderGitHubTokenSurfaces(t, cfg)
	if got := dig(s.values, componentSkillManager, skillManagerGitHub, "app", "secretName"); got != gitHubAppSecret {
		t.Errorf("skill-manager.github.app.secretName = %v, want %s", got, gitHubAppSecret)
	}
	if got := dig(s.values, token...); got != nil {
		t.Errorf("skill-manager.github.tokenSecret = %v next to the App, want nothing", got)
	}

	if got := dig(renderGitHubTokenSurfaces(t, skillManagerTestConfig()).values, componentSkillManager, skillManagerGitHub); got != nil {
		t.Errorf("skill-manager.github = %v without a recorded credential, want nothing", got)
	}
}

// TestSkillManagerNetworkPolicyFollowsTheSwitch: the skill-manager chart's
// own policy is on by default and would cut the pod off the lab Dex on its
// private address, so it follows platform.networkPolicy, as the rest of the
// lab's policies do (on, patch 11 adds the Dex peer).
func TestSkillManagerNetworkPolicyFollowsTheSwitch(t *testing.T) {
	for _, on := range []bool{false, true} {
		cfg := skillManagerTestConfig()
		cfg.Platform.NetworkPolicy, cfg.Platform.MusterPodNetwork = on, on
		s := renderGitHubTokenSurfaces(t, cfg)
		if got := dig(s.values, componentSkillManager, "networkPolicy", "enabled"); got != on {
			t.Errorf("platform.networkPolicy %v: skill-manager.networkPolicy.enabled = %v, want %v", on, got, on)
		}
	}
}

// TestSkillManagerAbsentBeforeItsChart: a chart without the component
// refuses its values (the meta chart's schema is closed), so nothing names it.
func TestSkillManagerAbsentBeforeItsChart(t *testing.T) {
	cfg := gitHubTokenTestConfig()
	cfg.GitHubToken.Source = gitHubTokenTestSource
	if s := renderGitHubTokenSurfaces(t, cfg); dig(s.values, componentSkillManager) != nil {
		t.Errorf("platform.chartVersion %s renders skill-manager:\n%s", cfg.Platform.ChartVersion, s.valuesRaw)
	} else if strings.Contains(s.valuesRaw, "\n"+componentSkillManager+":") {
		t.Errorf("platform.chartVersion %s names skill-manager:\n%s", cfg.Platform.ChartVersion, s.valuesRaw)
	}
}
