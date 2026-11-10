package lab

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// skillManagerGitHub is the skill-manager chart's key of its GitHub
// credential; skillManagerAbsentChart a released chart without the component.
const (
	skillManagerGitHub      = "github"
	skillManagerAbsentChart = "4.177.5"
)

// skillManagerTestConfig is a lab on a dev build of main, which runs the
// skill-manager whatever the release floor is.
func skillManagerTestConfig() *config.Config {
	cfg := gitHubTokenTestConfig()
	cfg.Platform.ChartVersion, cfg.Platform.ChartBranch = "4.178.1-rbf28cd64t20261110120000h7f841be", testRefMain
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
// lab's policies do.
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

// TestSkillManagerPolicyGetsTheDexPeer: with the switch on, patch 11 appends
// the lab Dex to the skill-manager chart's own policy. Its Deployment dials
// the issuer through --dex-issuer-url, and its policy isolates egress with
// rules of its own, as the chart renders both.
func TestSkillManagerPolicyGetsTheDexPeer(t *testing.T) {
	cfg := skillManagerTestConfig()
	render := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: skill-manager
  namespace: agent-platform
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: skill-manager
        app.kubernetes.io/instance: skill-manager
    spec:
      containers:
        - name: skill-manager
          args:
            - --dex-issuer-url=https://%s
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: skill-manager
  namespace: agent-platform
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: skill-manager
      app.kubernetes.io/instance: skill-manager
  policyTypes: [Ingress, Egress]
  egress:
    - ports:
        - port: 53
          protocol: UDP
`, dexLocalhostAddr(cfg))
	renders := map[string]string{componentSkillManager: render}
	dialers, err := dexDialers(cfg, renders)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(dialers[componentSkillManager], func(d dexLocalhostTarget) bool { return d.deployment == componentSkillManager }) {
		t.Fatalf("dexDialers = %+v, want the skill-manager Deployment", dialers)
	}
	policies, err := dexEgressPolicies(renders, dialers)
	if err != nil {
		t.Fatal(err)
	}
	want := []dexEgressPolicy{{name: componentSkillManager, namespace: platformNamespace, hasEgress: true}}
	if !slices.Equal(policies[componentSkillManager], want) {
		t.Errorf("dexEgressPolicies = %+v, want %+v", policies, want)
	}
}

// TestSkillManagerAbsentBeforeItsChart: a chart without the component
// refuses its values (the meta chart's schema is closed), so nothing names it.
func TestSkillManagerAbsentBeforeItsChart(t *testing.T) {
	cfg := gitHubTokenTestConfig()
	cfg.Platform.ChartVersion = skillManagerAbsentChart
	cfg.GitHubToken.Source = gitHubTokenTestSource
	if s := renderGitHubTokenSurfaces(t, cfg); strings.Contains(s.valuesRaw, "\n"+componentSkillManager+":") {
		t.Errorf("platform.chartVersion %s names skill-manager:\n%s", cfg.Platform.ChartVersion, s.valuesRaw)
	}
}
