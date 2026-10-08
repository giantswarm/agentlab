package lab

import (
	"fmt"

	"github.com/Masterminds/semver/v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// The chart in place against the one agentlab.yaml installs. The two drift
// apart when an agentlab.yaml is put back from a copy without the
// `agentlab platform` that would install it — a holder restoring a shared
// lab as found — or when a pin is bumped and no platform run follows: the
// config then names a release the cluster does not run, and anything that
// quoted it (a proof's verdict) would claim a version not under test. So the
// proofs read the chart in place and open with it (NoteChartUnderTest),
// `agentlab status` words the drift with both versions (chartDrift), and
// `agentlab configure --adopt-chart` writes the chart in place back into the
// config (AdoptInstalledChart); `agentlab platform` is the other way to end
// it, installing the configured chart over the one in place.

// chartDrift is the disagreement between the chart agentlab.yaml installs and
// the chart in place.
type chartDrift struct {
	configured platformChart
	installed  installedChart
}

// driftDirection says which side is behind, when the two are releases to
// order.
type driftDirection int

const (
	// driftUnordered: a dev chart or a chart directory on one side — two
	// versions nothing orders.
	driftUnordered driftDirection = iota
	// configBehind: the config pins an older release than runs — the restore
	// case, a platform run would downgrade the cluster.
	configBehind
	// clusterBehind: the config pins a newer release than runs — a bumped pin
	// no platform run followed.
	clusterBehind
)

// driftBetween is the drift of the chart in place from the configured one,
// nil when they agree or when nothing is in place.
func driftBetween(installed *installedChart, configured platformChart) *chartDrift {
	if installed == nil || installed.Version == configured.installedVersion() {
		return nil
	}
	return &chartDrift{configured: configured, installed: *installed}
}

// direction orders the two releases; driftUnordered when either side is a
// dev chart or a chart directory, or no version.
func (d chartDrift) direction() driftDirection {
	if d.installed.Dev() || d.configured.version == "" || IsDevBuild(d.configured.version) {
		return driftUnordered
	}
	have, err := semver.NewVersion(d.installed.Version)
	if err != nil {
		return driftUnordered
	}
	want, err := semver.NewVersion(d.configured.version)
	switch {
	case err != nil:
		return driftUnordered
	case want.LessThan(have):
		return configBehind
	case have.LessThan(want):
		return clusterBehind
	}
	return driftUnordered
}

// adoptable reports whether the chart in place can be pinned: a chart
// directory's build cannot, its Chart.yaml version is a placeholder.
func (d chartDrift) adoptable() bool {
	return d.installed.Channel != config.ChartChannelPath
}

// String words the drift for a person: both charts with the revision in
// place, which side is behind, and the two ways to end it — a platform run
// installing the configured chart, or adopting the chart in place into the
// config where that can be pinned.
func (d chartDrift) String() string {
	line := fmt.Sprintf("%s installs %s, the cluster runs %s (Helm revision %d)", config.File, d.configured, d.installed, d.installed.Revision)
	switch d.direction() {
	case configBehind:
		line += ": the config is behind the cluster — `agentlab platform` downgrades the cluster to the configured release"
	case clusterBehind:
		line += ": the cluster is behind the config — `agentlab platform` upgrades the cluster to the configured release"
	default:
		line += " — `agentlab platform` installs the configured chart over it"
	}
	if d.adoptable() {
		line += fmt.Sprintf(", `agentlab configure --adopt-chart` writes %s into %s", d.installed.Version, config.File)
	}
	return line
}

// NoteChartUnderTest opens a proof with the chart it runs against — the one
// in place, read from the cluster, never agentlab.yaml's pin — and warns of a
// drift between the two with both versions, so a verdict never claims a
// release that is not under test. Returns the chart in place, nil when none
// is installed (the proof then fails on its own first contact, by name).
func NoteChartUnderTest(cfg *config.Config) (*installedChart, error) {
	if err := useClusterKubeconfig(cfg); err != nil {
		return nil, err
	}
	installed, err := installedPlatformChart()
	if err != nil || installed == nil {
		return nil, err
	}
	step("Proving %s (Helm revision %d, %s)", installed, installed.Revision, installed.Status)
	if cfg.Platform.Enabled {
		if drift := driftBetween(installed, platformChartFor(cfg)); drift != nil {
			warn("%s", drift)
		}
	}
	return installed, nil
}

// AdoptInstalledChart writes the chart in place into agentlab.yaml's pin —
// `agentlab configure --adopt-chart`, the restore step for a config put back
// apart from its cluster: afterwards the config says what runs, and a
// platform run changes nothing. Refused when the lab is not running or holds
// no platform release, and for a chart directory's build, whose version is
// a placeholder nothing can pin. Returns the chart adopted.
func AdoptInstalledChart(cfg *config.Config) (*installedChart, error) {
	if err := requireRunningCluster(cfg); err != nil {
		return nil, err
	}
	if err := useClusterKubeconfig(cfg); err != nil {
		return nil, err
	}
	installed, err := installedPlatformChart()
	if err != nil {
		return nil, err
	}
	if err := adoptChart(cfg, installed); err != nil {
		return nil, err
	}
	return installed, nil
}

// adoptChart is AdoptInstalledChart without the cluster: the config made to
// install the chart in place. A release puts the lab on the stable channel at
// that release — a chart directory or a branch the config named gives way,
// the cluster runs neither; a dev build of the branch the config follows is
// pinned (platform.chartPinned), one of another branch is pinned by hand on
// the stable channel.
func adoptChart(cfg *config.Config, installed *installedChart) error {
	switch {
	case installed == nil:
		return fmt.Errorf("the cluster holds no platform release to adopt; `agentlab platform` installs %s", platformChartFor(cfg))
	case installed.Channel == config.ChartChannelPath:
		return fmt.Errorf("the cluster runs %s (%s), whose version is a placeholder nothing can pin; `agentlab configure --chart-path <that directory>` keeps installing it, `agentlab platform` replaces it with %s", installed, installed.Description, platformChartFor(cfg))
	}
	cfg.Platform.ChartPath = ""
	cfg.Platform.ChartVersion = installed.Version
	cfg.Platform.ChartPinned = false
	if branch := cfg.Platform.ChartBranch; branch != "" {
		if v, err := semver.NewVersion(installed.Version); err == nil && devTagFilter(branch)(v.Prerelease()) {
			cfg.Platform.ChartPinned = true
		} else {
			cfg.Platform.ChartBranch = ""
		}
	}
	return nil
}
