package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// The releases the drift tests pin and find in place: an agentlab.yaml put
// back from a copy pins the older one while the cluster runs the newer.
const (
	restoredPin     = "4.93.0"
	runsInPlace     = "4.114.0"
	adoptFlag       = "--adopt-chart"
	srcChartDir     = "/src/helm/agent-platform"
	nothingInPlace  = "nothing in place"
	aDirectoryBuild = "a chart directory's build"
	behindWord      = "behind"
	overIt          = "installs the configured chart over it"
)

// TestDriftBetween: no drift without a chart in place, none when the chart
// in place is the configured one — a chart directory over its own
// placeholder included — and one whenever the versions differ.
func TestDriftBetween(t *testing.T) {
	release := platformChart{ref: config.ChartRepository, version: runsInPlace}
	dir := writeChartFiles(t, t.TempDir(), "agent-platform", map[string]string{chartYAML: "apiVersion: v2\nname: agent-platform\nversion: " + placeholderInPlace + "\n"})
	for _, c := range []struct {
		name      string
		installed *installedChart
		chart     platformChart
		want      bool
	}{
		{nothingInPlace, nil, release, false},
		{"the release in place", &installedChart{Version: runsInPlace, Channel: config.ChartChannelStable}, release, false},
		{"the directory over its own build", &installedChart{Version: placeholderInPlace, Channel: config.ChartChannelPath}, platformChart{ref: dir}, false},
		{"an older pin", &installedChart{Version: runsInPlace, Channel: config.ChartChannelStable}, platformChart{ref: config.ChartRepository, version: restoredPin}, true},
		{"a dev build under the release", &installedChart{Version: devBuildInPlace}, release, true},
	} {
		if got := driftBetween(c.installed, c.chart) != nil; got != c.want {
			t.Errorf("%s: drift %v, want %v", c.name, got, c.want)
		}
	}
}

// TestChartDriftWording: the drift names both charts and the revision in
// place; two releases say which side is behind and what a platform run does
// to the cluster (a downgrade for a pin behind the cluster, an upgrade for a
// cluster behind the pin); a dev chart on either side orders nothing; the
// adopt command is offered with the version it writes, except for a chart
// directory's build, whose version is a placeholder.
func TestChartDriftWording(t *testing.T) {
	pin := func(v string) platformChart { return platformChart{ref: config.ChartRepository, version: v} }
	for _, c := range []struct {
		name      string
		installed installedChart
		chart     platformChart
		direction driftDirection
		want      []string
		never     []string
	}{
		{"the config behind the cluster", installedChart{Version: runsInPlace, Channel: config.ChartChannelStable, Revision: 7}, pin(restoredPin), configBehind,
			[]string{config.File + " installs agent-platform " + restoredPin, "the cluster runs agent-platform " + runsInPlace + " (Helm revision 7)", "the config is behind the cluster", "downgrades the cluster", adoptFlag + "` writes " + runsInPlace + " into " + config.File}, []string{"upgrades"}},
		{"the cluster behind the config", installedChart{Version: "4.110.0", Channel: config.ChartChannelStable, Revision: 3}, pin(releaseInPlace), clusterBehind,
			[]string{"the cluster is behind the config", "upgrades the cluster", adoptFlag + "` writes 4.110.0"}, []string{"downgrades"}},
		{"a dev build in place", installedChart{Version: devBuildInPlace, Channel: config.ChartChannelDev, Revision: 2}, pin(releaseInPlace), driftUnordered,
			[]string{aDevBuild, overIt, adoptFlag + "` writes " + devBuildInPlace}, []string{behindWord}},
		{"a dev build pinned", installedChart{Version: releaseInPlace, Channel: config.ChartChannelStable, Revision: 2}, pin(devBuildInPlace), driftUnordered,
			[]string{overIt}, []string{behindWord}},
		{"a chart directory's build in place", installedChart{Version: placeholderInPlace, Channel: config.ChartChannelPath, Revision: 5}, pin(releaseInPlace), driftUnordered,
			[]string{aDirectoryBuild, overIt}, []string{adoptFlag, behindWord}},
		{"a directory configured", installedChart{Version: releaseInPlace, Channel: config.ChartChannelStable, Revision: 1}, platformChart{ref: srcChartDir}, driftUnordered,
			[]string{"installs the local chart at /src/helm/agent-platform", adoptFlag + "` writes " + releaseInPlace}, []string{behindWord}},
	} {
		d := chartDrift{configured: c.chart, installed: c.installed}
		if got := d.direction(); got != c.direction {
			t.Errorf("%s: direction %d, want %d", c.name, got, c.direction)
		}
		got := d.String()
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", c.name, got, w)
			}
		}
		for _, n := range c.never {
			if strings.Contains(got, n) {
				t.Errorf("%s: %q says %q", c.name, got, n)
			}
		}
	}
}

// TestAdoptChart: a release in place puts the config on the stable channel
// at that release, a chart directory and a branch it named cleared; a dev
// build of the followed branch pins the channel at it; one of another branch
// is pinned by hand, the branch cleared; no release and a chart directory's
// build are refused with the way out, the config untouched.
func TestAdoptChart(t *testing.T) {
	// chartFields are the config's chart source after the adoption.
	type chartFields struct {
		version, path, branch string
		pinned                bool
	}
	fieldsOf := func(cfg *config.Config) chartFields {
		return chartFields{cfg.Platform.ChartVersion, cfg.Platform.ChartPath, cfg.Platform.ChartBranch, cfg.Platform.ChartPinned}
	}
	devOfBranch := "4.115.1-r" + config.BranchHash(devBranch) + "t20261006135307h75833ed"
	for _, c := range []struct {
		name      string
		setup     func(*config.Config)
		installed *installedChart
		want      chartFields
		refused   string
	}{
		{"a release over a directory and a branch", func(cfg *config.Config) {
			cfg.Platform.ChartPath, cfg.Platform.ChartBranch, cfg.Platform.ChartPinned = srcChartDir, devBranch, true
		}, &installedChart{Version: runsInPlace, Channel: config.ChartChannelStable}, chartFields{version: runsInPlace}, ""},
		{"the followed branch's build", func(cfg *config.Config) { cfg.Platform.ChartBranch = devBranch },
			&installedChart{Version: devOfBranch, Channel: config.ChartChannelDev}, chartFields{version: devOfBranch, branch: devBranch, pinned: true}, ""},
		{"another branch's build", func(cfg *config.Config) { cfg.Platform.ChartBranch = devBranch },
			&installedChart{Version: devBuildInPlace}, chartFields{version: devBuildInPlace}, ""},
		{nothingInPlace, func(*config.Config) {}, nil, chartFields{version: restoredPin}, "`agentlab platform` installs agent-platform " + restoredPin},
		{aDirectoryBuild, func(*config.Config) {},
			&installedChart{Version: placeholderInPlace, Channel: config.ChartChannelPath, Description: "agentlab v0.82.0: the chart directory " + srcChartDir}, chartFields{version: restoredPin}, "placeholder nothing can pin"},
	} {
		cfg := &config.Config{}
		cfg.Platform.ChartVersion = restoredPin
		c.setup(cfg)
		err := adoptChart(cfg, c.installed)
		switch {
		case c.refused == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.refused != "" && (err == nil || !strings.Contains(err.Error(), c.refused)):
			t.Errorf("%s: err %v, want it to say %q", c.name, err, c.refused)
		}
		if got := fieldsOf(cfg); got != c.want {
			t.Errorf("%s: chart fields %+v, want %+v", c.name, got, c.want)
		}
	}
}
