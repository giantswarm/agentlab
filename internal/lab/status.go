package lab

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/pkg/project"
)

// chartChannelLabel is the release label the lab stamps on the platform
// release at every install and upgrade: the channel the chart came from
// (config.ChartChannel — stable, dev or path). Helm keeps a release's labels
// across upgrades, so the stamp is the run's that put the chart in place, and
// a later run or `agentlab status` reads what that run knew. It is what tells
// a checkout's build from a release: a checkout's Chart.yaml carries a
// placeholder version (the same on both lines), which nothing else marks.
const chartChannelLabel = "agentlab.giantswarm.io/chart-channel"

// installedChart is the meta chart the lab cluster runs: the platform
// release's newest revision as Helm stores it, whatever its status.
type installedChart struct {
	// Version is the chart's: a release's tag, a dev build's, or a checkout's
	// Chart.yaml placeholder.
	Version string `json:"version"`
	// Channel is the chartChannelLabel's value, "" when an agentlab from
	// before the label installed the revision.
	Channel string `json:"channel,omitempty"`
	// Description is the revision's Helm description: the lab's own line
	// (installDescription), or Helm's "Install complete" for a release the
	// CLI or an earlier agentlab wrote.
	Description string `json:"description,omitempty"`
	// Status is the revision's: deployed, failed, pending-upgrade, …
	Status   string    `json:"status"`
	Revision int       `json:"revision"`
	Deployed time.Time `json:"deployed"`
}

// installedPlatformChart reads the chart in place, nil when the cluster
// holds no platform release.
func installedPlatformChart() (*installedChart, error) {
	rel, err := helmReleaseNewest(platformNamespace, platformRelease)
	if err != nil || rel == nil {
		return nil, err
	}
	c := &installedChart{Channel: rel.Labels[chartChannelLabel], Revision: rel.Version}
	if rel.Chart != nil && rel.Chart.Metadata != nil {
		c.Version = rel.Chart.Metadata.Version
	}
	if rel.Info != nil {
		c.Description = rel.Info.Description
		c.Status = rel.Info.Status.String()
		c.Deployed = rel.Info.LastDeployed
	}
	return c, nil
}

// Dev reports whether the chart in place is a dev chart: a chart directory's
// build (the label says so; its version alone cannot) or a dev build of a
// branch, whose version says so however it was installed.
func (c installedChart) Dev() bool {
	return c.Channel == config.ChartChannelPath || IsDevBuild(c.Version)
}

// String words the chart in place for a person: the version and, for a dev
// chart, what kind.
func (c installedChart) String() string {
	switch {
	case c.Channel == config.ChartChannelPath:
		return fmt.Sprintf("agent-platform %s, a chart directory's build", c.Version)
	case IsDevBuild(c.Version):
		return fmt.Sprintf("agent-platform %s, a dev build", c.Version)
	default:
		return "agent-platform " + c.Version
	}
}

// platformInstallOptions are the marks a platform install or upgrade leaves
// on the release: the channel label (chartChannelLabel) and a description
// `helm history` shows next to the revision — which agentlab installed what,
// from where.
func platformInstallOptions(cfg *config.Config) helmInstallOptions {
	return helmInstallOptions{
		Labels:      map[string]string{chartChannelLabel: cfg.ChartChannel()},
		Description: installDescription(cfg),
	}
}

// installDescription is the revision's log line: the agentlab that ran and
// the chart source the config named.
func installDescription(cfg *config.Config) string {
	by := "agentlab " + project.Version() + ": "
	switch cfg.ChartChannel() {
	case config.ChartChannelPath:
		return by + "the chart directory " + cfg.Platform.ChartPath
	case config.ChartChannelDev:
		return by + "a dev build of branch " + cfg.Platform.ChartBranch
	}
	if IsDevBuild(cfg.Platform.ChartVersion) {
		return by + "the dev build pinned in platform.chartVersion"
	}
	return by + "the pinned release"
}

// replacingNote is the line a platform run prints when the cluster runs
// another chart than the one it is about to install — a dev chart an earlier
// run left in place named as such, so whoever reads the boot learns what the
// lab ran and that it is gone. Empty when nothing is in place or the chart in
// place is the one being installed.
func replacingNote(installed *installedChart, chart platformChart) string {
	if installed == nil || installed.Version == chart.installedVersion() {
		return ""
	}
	if installed.Dev() {
		return fmt.Sprintf("the cluster runs a dev chart left in place: %s (%s); %s replaces it", installed, installed.Description, chart)
	}
	return fmt.Sprintf("the cluster runs %s; %s replaces it", installed, chart)
}

// Status is the lab's live state as `agentlab status` reports it, and its
// -o json shape: the chart the cluster runs against the one agentlab.yaml
// installs, and the platform's HelmReleases.
type Status struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
	// Chart is the chart in place; nil when the cluster holds no platform
	// release.
	Chart *installedChart `json:"chart,omitempty"`
	// Configured is the chart agentlab.yaml installs, in the install's own
	// words; "" when the platform is disabled.
	Configured string `json:"configured,omitempty"`
	// Replaces reports that `agentlab platform` would replace the chart in
	// place: the configured chart is not it.
	Replaces bool `json:"replaces"`
	// Releases are the platform HelmReleases with their Ready condition; none
	// without a platform release.
	Releases []platformReleaseStatus `json:"releases,omitempty"`
}

// LabStatus reads the lab's live state. A lab that is not running is refused
// with the fact, like `pods`.
func LabStatus(cfg *config.Config, dir string) (*Status, error) {
	if err := requireRunningCluster(cfg); err != nil {
		return nil, err
	}
	if err := useClusterKubeconfig(cfg); err != nil {
		return nil, err
	}
	s := &Status{Name: cfg.ClusterName, Dir: dir}
	installed, err := installedPlatformChart()
	if err != nil {
		return nil, err
	}
	s.Chart = installed
	if cfg.Platform.Enabled {
		chart := platformChartFor(cfg)
		s.Configured = chart.String()
		s.Replaces = installed != nil && installed.Version != chart.installedVersion()
	}
	if installed != nil {
		if s.Releases, err = platformReleases(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// PrintStatus writes the status for a person, in the shape of `list`'s
// block, or as JSON.
func PrintStatus(w io.Writer, s *Status, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", s.Name, s.Dir)
	fmt.Fprintf(&b, "    chart       %s\n", chartLine(s))
	fmt.Fprintf(&b, "    config      %s\n", configLine(s))
	if s.Chart != nil {
		fmt.Fprintf(&b, "    releases    %s\n", releasesLine(s.Releases))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// chartLine is the chart in place with its revision's provenance, or none.
func chartLine(s *Status) string {
	c := s.Chart
	if c == nil {
		return "none installed"
	}
	line := c.String()
	if c.Dev() {
		line += " (a dev chart)"
	}
	line += fmt.Sprintf(" — Helm revision %d %s", c.Revision, c.Status)
	if !c.Deployed.IsZero() {
		line += " " + c.Deployed.Local().Format("2006-01-02 15:04")
	}
	if c.Description != "" {
		line += ", " + c.Description
	}
	return line
}

// configLine is what agentlab.yaml installs, and whether a platform run
// would replace the chart in place with it.
func configLine(s *Status) string {
	switch {
	case s.Configured == "":
		return "the platform is disabled (platform.enabled in agentlab.yaml)"
	case s.Chart == nil:
		return s.Configured + "; `agentlab platform` installs it"
	case s.Replaces:
		return s.Configured + "; `agentlab platform` replaces the chart in place with it"
	default:
		return s.Configured + ", the chart in place"
	}
}

// releasesLine counts the Ready HelmReleases and names the others with
// helm-controller's message.
func releasesLine(releases []platformReleaseStatus) string {
	ready := 0
	var pending []string
	for _, r := range releases {
		if r.Ready == conditionTrue {
			ready++
			continue
		}
		pending = append(pending, fmt.Sprintf("%s Ready=%s %s", r.Name, orNone(r.Ready), r.Message))
	}
	line := fmt.Sprintf("%d of %d Ready", ready, len(releases))
	if len(pending) > 0 {
		line += "; " + strings.Join(pending, "; ")
	}
	return line
}
