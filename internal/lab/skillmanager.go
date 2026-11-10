package lab

import "github.com/giantswarm/agentlab/internal/config"

// skillManagerChartCarries reports whether the configured meta chart runs
// the skill-manager and so takes its values block: its schema or defaults
// carry the component's key, whatever the release, branch build or chart
// directory. A chart that cannot be loaded is a note and no block, as for
// the workspaces' values (workspacesChartFor).
func skillManagerChartCarries(cfg *config.Config) bool {
	if !cfg.Platform.Enabled {
		return false
	}
	chart := platformChartFor(cfg)
	v, err := platformChartValuesOf(chart)
	if err != nil {
		note("the platform chart %s could not be read for the skill-manager's values (%v): the values carry no skill-manager block", chart, err)
		return false
	}
	return v.carries(componentSkillManager)
}
