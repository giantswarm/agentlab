package lab

import (
	"bytes"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The chart fixtures of a chartPath lab: a checkout's meta chart at its
// Chart.yaml placeholder version, and a release the registry serves.
const (
	placeholderChartVersion = "1.1.35"
	releasedChartVersion    = "4.40.0"
	metaChartYAML           = "apiVersion: v2\nname: agent-platform\nversion: " + placeholderChartVersion + "\n"
	historyField            = "history"
)

// A chartPath lab installs the checkout's connectivity chart from the
// sibling directory, pushed into the lab registry at the meta chart's own
// version — the version the meta chart pins the component to. A registry
// chart has none of this: its connectivity is published with it.
func TestLocalConnectivityChartFor(t *testing.T) {
	cfg := config.Default()
	cfg.ClusterName = "agentlab-x"
	cfg.Platform.DevRegistryPort = 5017
	if local, err := localConnectivityChartFor(cfg); err != nil || local != nil {
		t.Fatalf("a registry chart has no local connectivity chart: %+v, %v", local, err)
	}

	dir := t.TempDir()
	cfg.Platform.ChartPath = writeChartFiles(t, dir, "agent-platform", map[string]string{
		chartYAML: metaChartYAML,
	})
	local, err := localConnectivityChartFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := &localConnectivityChart{
		Dir:        config.ConnectivityChartDir(cfg.Platform.ChartPath),
		Repository: "oci://agentlab-x-registry:5000/charts",
		Version:    placeholderChartVersion,
	}
	if *local != *want {
		t.Errorf("local connectivity chart = %+v, want %+v", local, want)
	}
	if got, want := local.pushRef(cfg), "localhost:5017/charts/agent-platform-connectivity:"+placeholderChartVersion; got != want {
		t.Errorf("pushRef = %q, want %q", got, want)
	}
	// The roster's mapping reads the same sibling off the chart source.
	if got := platformChartFor(cfg).connectivityDir(); got != want.Dir {
		t.Errorf("connectivityDir = %q, want %q", got, want.Dir)
	}
	if got := (platformChart{ref: config.ChartRepository, version: releasedChartVersion}).connectivityDir(); got != "" {
		t.Errorf("a registry chart has no connectivity directory: %q", got)
	}

	// A meta chart whose Chart.yaml names no version cannot name the
	// connectivity release either.
	cfg.Platform.ChartPath = writeChartFiles(t, dir, "unversioned", map[string]string{chartYAML: "apiVersion: v2\nname: agent-platform\n"})
	if _, err := localConnectivityChartFor(cfg); err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("a Chart.yaml without a version: want the error naming it, got %v", err)
	}
}

// The push packages the directory as `helm package --version` would: the
// archive carries the chart as it is with the version stamped to the meta
// chart's, so the two charts of a working tree share one version.
func TestPackageChartStampsTheVersion(t *testing.T) {
	chartDir := writeChartFiles(t, t.TempDir(), config.ConnectivityChartName, map[string]string{
		chartYAML:    "apiVersion: v2\nname: " + config.ConnectivityChartName + "\nversion: " + placeholderChartVersion + "\n",
		chartValues:  "known: a\n",
		chartConfMap: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n",
	})
	const version = "4.41.1-dev.feat-x.2026-09-18.22-00-00.h1234567"
	data, err := packageChart(chartDir, version)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := loader.LoadArchive(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the package is not a chart archive: %v", err)
	}
	if ch.Metadata.Name != config.ConnectivityChartName || ch.Metadata.Version != version {
		t.Errorf("packaged %s %s, want %s %s", ch.Metadata.Name, ch.Metadata.Version, config.ConnectivityChartName, version)
	}
	if len(ch.Templates) != 1 || !strings.HasSuffix(ch.Templates[0].Name, "cm.yaml") {
		t.Errorf("the package lost the templates: %+v", ch.Templates)
	}
	if _, err := packageChart(chartDir+"-missing", version); err == nil {
		t.Error("a missing directory packaged")
	}
}

// The catch-up wait reads the chart digest the connectivity release runs
// off its current history entry, helm-controller's record of the OCI
// artifact — "" while the release has none yet.
func TestCurrentReleaseOCIDigest(t *testing.T) {
	const digest = "sha256:b24fff2af519aac619af89bda55d9b62c41a3ec117c2c6b15a1aa073959b4c15"
	withHistory := func(history []any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{crStatus: map[string]any{historyField: history}}}
	}
	hr := withHistory([]any{
		map[string]any{"ociDigest": digest},
		map[string]any{"ociDigest": "sha256:0123"},
	})
	if got := currentReleaseOCIDigest(hr); got != digest {
		t.Errorf("current digest = %q, want the newest entry's %q", got, digest)
	}
	for name, hr := range map[string]*unstructured.Unstructured{
		"no status":    {Object: map[string]any{}},
		"no history":   withHistory([]any{}),
		"no ociDigest": withHistory([]any{map[string]any{"appVersion": "none"}}),
	} {
		if got := currentReleaseOCIDigest(hr); got != "" {
			t.Errorf("%s: current digest = %q, want none", name, got)
		}
	}
}
