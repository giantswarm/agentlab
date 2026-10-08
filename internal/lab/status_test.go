package lab

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	releasecommon "helm.sh/helm/v4/pkg/release/common"

	"github.com/giantswarm/agentlab/internal/config"
)

// The chart versions the status tests see in place: a release, a release
// candidate, a checkout's placeholder, a dev build in gitsemver 3's shape
// and one in the superseded shape; the branch a dev build is of; and the
// words the status wording is checked for.
const (
	releaseInPlace     = "4.116.0"
	rcInPlace          = "4.108.0-rc.1"
	placeholderInPlace = "1.1.35"
	devBuildInPlace    = "4.115.1-r7fb489f8t20261006135307h75833ed"
	oldDevBuildInPlace = "4.66.5-dev.my-feature.2026-09-11.08-12-33.h7f841be"
	devBranch          = "feat/x"
	aDevBuild          = "a dev build"
	aDevChart          = "dev chart"
	replacesWord       = "replaces"
)

// TestInstalledChartWording: a chart directory's build is known by the
// label alone (its version is a placeholder), a dev build by its version
// however it was installed — labelled, pinned by hand or from before the
// label; a release and a release candidate are neither.
func TestInstalledChartWording(t *testing.T) {
	for _, c := range []struct {
		chart installedChart
		dev   bool
		want  string
	}{
		{installedChart{Version: releaseInPlace, Channel: config.ChartChannelStable}, false, "agent-platform " + releaseInPlace},
		{installedChart{Version: rcInPlace}, false, "agent-platform " + rcInPlace},
		{installedChart{Version: placeholderInPlace, Channel: config.ChartChannelPath}, true, aDirectoryBuild},
		{installedChart{Version: devBuildInPlace, Channel: config.ChartChannelDev}, true, aDevBuild},
		{installedChart{Version: devBuildInPlace}, true, aDevBuild},
		{installedChart{Version: devBuildInPlace, Channel: config.ChartChannelStable}, true, aDevBuild},
		{installedChart{Version: oldDevBuildInPlace}, true, aDevBuild},
	} {
		if got := c.chart.Dev(); got != c.dev {
			t.Errorf("%+v: Dev() = %v, want %v", c.chart, got, c.dev)
		}
		if got := c.chart.String(); !strings.Contains(got, c.want) || !strings.Contains(got, c.chart.Version) {
			t.Errorf("%+v: String() = %q, want it to name %q and the version", c.chart, got, c.want)
		}
	}
}

// TestReplacingNote: a platform run names the chart in place when it is not
// the one being installed — a dev chart as such, with the revision's own
// description — and says nothing when nothing is in place, when the chart in
// place is the one installed, or when a chart directory re-installs over its
// own placeholder version.
func TestReplacingNote(t *testing.T) {
	release := platformChart{ref: config.ChartRepository, version: releaseInPlace}
	dir := writeChartFiles(t, t.TempDir(), "agent-platform", map[string]string{chartYAML: "apiVersion: v2\nname: agent-platform\nversion: " + placeholderInPlace + "\n"})
	local := platformChart{ref: dir}
	devChart := &installedChart{Version: placeholderInPlace, Channel: config.ChartChannelPath, Description: "agentlab v0.82.0: the chart directory " + srcChartDir}

	for _, c := range []struct {
		name      string
		installed *installedChart
		chart     platformChart
		want      []string
		never     string
	}{
		{nothingInPlace, nil, release, nil, replacesWord},
		{"the release in place", &installedChart{Version: releaseInPlace, Channel: config.ChartChannelStable}, release, nil, replacesWord},
		{"a dev chart under the release", devChart, release, []string{"a dev chart left in place", placeholderInPlace, aDirectoryBuild, devChart.Description, "agent-platform " + releaseInPlace + " replaces it"}, ""},
		{"a dev build under the release", &installedChart{Version: devBuildInPlace}, release, []string{"a dev chart left in place", devBuildInPlace, aDevBuild, "replaces it"}, ""},
		{"the directory over its own build", devChart, local, nil, replacesWord},
		{"an older release under the release", &installedChart{Version: "4.110.0", Channel: config.ChartChannelStable}, release, []string{"the cluster runs agent-platform 4.110.0", "agent-platform " + releaseInPlace + " replaces it"}, "downgrade"},
		{"a restored pin under a newer release", &installedChart{Version: "4.120.0", Channel: config.ChartChannelStable}, release, []string{"the cluster runs agent-platform 4.120.0", "agent-platform " + releaseInPlace + " replaces it (a downgrade)"}, aDevChart},
		{"the release under the directory", &installedChart{Version: releaseInPlace, Channel: config.ChartChannelStable}, local, []string{"the cluster runs agent-platform " + releaseInPlace, "the local chart at " + dir + " replaces it"}, aDevChart},
	} {
		got := replacingNote(c.installed, c.chart)
		if len(c.want) == 0 && got != "" {
			t.Errorf("%s: note %q, want none", c.name, got)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: note %q lacks %q", c.name, got, w)
			}
		}
		if c.never != "" && strings.Contains(got, c.never) {
			t.Errorf("%s: note %q says %q", c.name, got, c.never)
		}
	}
}

// TestInstallDescription: the revision's log line names the agentlab that
// ran and the chart source the config selected — the directory, the branch,
// a dev build pinned by hand, or the release — and the label carries the
// channel.
func TestInstallDescription(t *testing.T) {
	cfg := &config.Config{}
	cfg.Platform.ChartVersion = releaseInPlace
	for _, c := range []struct {
		setup func()
		want  string
	}{
		{func() {}, "the pinned release"},
		{func() { cfg.Platform.ChartVersion = devBuildInPlace }, "the dev build pinned in platform.chartVersion"},
		{func() { cfg.Platform.ChartBranch = devBranch }, "a dev build of branch " + devBranch},
		{func() { cfg.Platform.ChartBranch, cfg.Platform.ChartPath = "", srcChartDir }, "the chart directory " + srcChartDir},
	} {
		c.setup()
		opts := platformInstallOptions(cfg)
		if !strings.HasPrefix(opts.Description, "agentlab ") || !strings.HasSuffix(opts.Description, c.want) {
			t.Errorf("description = %q, want `agentlab <version>: %s`", opts.Description, c.want)
		}
		if opts.Labels[chartChannelLabel] != cfg.ChartChannel() {
			t.Errorf("label = %q, want the channel %q", opts.Labels[chartChannelLabel], cfg.ChartChannel())
		}
	}
}

// TestPrintStatus: the block names the chart in place with its kind,
// revision and provenance, what agentlab.yaml installs and whether a platform
// run replaces the chart in place with it, and the HelmReleases' Ready count
// with the others' messages; JSON carries the same; a lab without a platform
// release, and one whose platform is disabled, read as such.
func TestPrintStatus(t *testing.T) {
	deployed := string(releasecommon.StatusDeployed)
	devBuild := &Status{
		Name: "agentlab", Dir: "/labs/agentlab",
		Chart: &installedChart{
			Version: devBuildInPlace, Channel: config.ChartChannelDev, Status: deployed, Revision: 2,
			Deployed:    time.Date(2026, 10, 7, 5, 56, 0, 0, time.UTC),
			Description: "agentlab v0.82.0: a dev build of branch " + devBranch,
		},
		Configured: "agent-platform " + releaseInPlace, Replaces: true,
		Drift: config.File + " installs agent-platform " + releaseInPlace + ", the cluster runs agent-platform " + devBuildInPlace + ", a dev build (Helm revision 2) — `agentlab platform` installs the configured chart over it, `agentlab configure --adopt-chart` writes " + devBuildInPlace + " into " + config.File,
		Releases: []platformReleaseStatus{
			{Name: componentMuster, Ready: conditionTrue, Message: "Helm install succeeded"},
			{Name: "agent-manager", Ready: condFalse, Message: "no match found for semver: >=1.10.0 <2.0.0"},
		},
		AppConfigDrift: []string{labAppConfigMap + " differs from the lab's render"},
	}
	var out bytes.Buffer
	if err := PrintStatus(&out, devBuild, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"agentlab  /labs/agentlab",
		"chart       agent-platform " + devBuildInPlace + ", a dev build (a dev chart) — Helm revision 2 " + deployed,
		devBuild.Chart.Description,
		"config      " + devBuild.Drift,
		"releases    1 of 2 Ready; agent-manager Ready=False no match found for semver",
		"app-config  drifted: " + labAppConfigMap + " differs from the lab's render",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status lacks %q:\n%s", want, text)
		}
	}

	out.Reset()
	if err := PrintStatus(&out, devBuild, true); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Chart    struct{ Version, Channel string }
		Replaces bool
		Drift    string
		Releases []struct{ Name, Ready string }
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if decoded.Chart.Version != devBuildInPlace || decoded.Chart.Channel != config.ChartChannelDev || !decoded.Replaces || decoded.Drift != devBuild.Drift || len(decoded.Releases) != 2 || decoded.Releases[1].Name != "agent-manager" {
		t.Errorf("json = %+v", decoded)
	}

	for _, c := range []struct {
		status *Status
		want   []string
		never  string
	}{
		{&Status{Name: "unprovisioned", Dir: "/labs/unprovisioned", Configured: "agent-platform " + releaseInPlace}, []string{"chart       none installed", "`agentlab platform` installs it"}, "releases"},
		{&Status{Name: "sandbox", Dir: "/labs/sandbox"}, []string{"none installed", "the platform is disabled"}, replacesWord},
		{&Status{Name: "same", Dir: "/labs/same", Chart: &installedChart{Version: releaseInPlace, Channel: config.ChartChannelStable, Status: deployed, Revision: 3}, Configured: "agent-platform " + releaseInPlace}, []string{"chart       agent-platform " + releaseInPlace + " — Helm revision 3 " + deployed, "agent-platform " + releaseInPlace + ", the chart in place", "releases    0 of 0 Ready"}, aDevChart},
		{&Status{Name: "restored", Dir: "/labs/restored", Chart: &installedChart{Version: "4.114.0", Channel: config.ChartChannelStable, Status: deployed, Revision: 7}, Configured: "agent-platform 4.93.0", Replaces: true, Drift: "agentlab.yaml installs agent-platform 4.93.0, the cluster runs agent-platform 4.114.0 (Helm revision 7): the config is behind the cluster — `agentlab platform` downgrades the cluster to the configured release, `agentlab configure --adopt-chart` writes 4.114.0 into agentlab.yaml"}, []string{"config      agentlab.yaml installs agent-platform 4.93.0, the cluster runs agent-platform 4.114.0", "the config is behind the cluster", "--adopt-chart` writes 4.114.0"}, replacesWord},
	} {
		out.Reset()
		if err := PrintStatus(&out, c.status, false); err != nil {
			t.Fatal(err)
		}
		for _, w := range c.want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%s: status lacks %q:\n%s", c.status.Name, w, out.String())
			}
		}
		if strings.Contains(out.String(), c.never) {
			t.Errorf("%s: status says %q:\n%s", c.status.Name, c.never, out.String())
		}
	}
}
