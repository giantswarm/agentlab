package lab

import "testing"

func TestAgentChartLine(t *testing.T) {
	for semver, want := range map[string]bool{
		"1.x":              true,
		">=1.0.0 <1.5.0":   true,
		">=1.5.0 <2.0.0":   true,
		"1.4.4":            true,
		"x.x.x":            false,
		">=0.2.1 <1.0.0":   false,
		">=1.0.0 <3.0.0":   false,
		"2.x":              false,
		"not a constraint": false,
	} {
		if got := agentChartLine(semver); got != want {
			t.Errorf("agentChartLine(%q) = %v, want %v", semver, got, want)
		}
	}
}
