package lab

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ri "helm.sh/helm/v4/pkg/release"
	releasecommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"
)

// isolateHelm keeps a test's embedded Helm off the machine's Helm state: its
// caches, repository files, registry credentials and plugins under a temp
// directory, debug off, and the shell's kubeconfig pointed elsewhere — the
// lab must never read it. Then moves into a fresh working directory, where
// state/kubeconfig does not exist: an offline render needs no cluster at all.
func isolateHelm(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, v := range []string{"HELM_REGISTRY_CONFIG", "HELM_REPOSITORY_CONFIG", "HELM_REPOSITORY_CACHE", "HELM_CONTENT_CACHE", "HELM_PLUGINS"} {
		t.Setenv(v, filepath.Join(dir, strings.ToLower(v)))
	}
	t.Setenv(helmDebugEnv, "")
	t.Setenv("KUBECONFIG", "/elsewhere/config")
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	return dir
}

// writeChart lays out a minimal chart: a Deployment whose image comes from
// the values, a ServiceMonitor guarded on the Prometheus Operator API being
// known, a pre-install hook Job, and a CRD.
func writeChart(t *testing.T, dir string) string {
	t.Helper()
	chartDir := filepath.Join(dir, "testchart")
	files := map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: testchart\nversion: 0.1.0\n",
		"values.yaml": "image: registry.example/app:1.0.0\nmonitor: true\n",
		"templates/deploy.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
  namespace: {{ .Release.Namespace }}
spec:
  template:
    spec:
      containers:
        - name: app
          image: {{ .Values.image }}
`,
		"templates/monitor.yaml": `{{- if and .Values.monitor (.Capabilities.APIVersions.Has "monitoring.coreos.com/v1") }}
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: {{ .Release.Name }}
{{- end }}
`,
		"templates/hook.yaml": `apiVersion: batch/v1
kind: Job
metadata:
  name: {{ .Release.Name }}-hook
  annotations:
    "helm.sh/hook": pre-install
spec:
  template:
    spec:
      containers:
        - name: hook
          image: registry.example/hook:2.0.0
`,
		"crds/widgets.yaml": `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
`,
	}
	for name, content := range files {
		path := filepath.Join(chartDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return chartDir
}

// TestHelmTemplateOffline: the `helm template` equivalent renders a chart
// directory with the lab's values and release coordinates without a cluster
// or a kubeconfig — templates and hooks in, CRDs out, and .Capabilities
// extended by the API versions the caller declares (the preload's offline
// guards), exactly the flags the CLI took.
func TestHelmTemplateOffline(t *testing.T) {
	dir := isolateHelm(t)
	chartDir := writeChart(t, dir)
	vals, err := helmValues([]byte("image: registry.example/app:override\n"))
	if err != nil {
		t.Fatal(err)
	}

	rendered, err := helmTemplate("apps", "myrel", chartDir, "", vals, []string{"monitoring.coreos.com/v1"})
	if err != nil {
		t.Fatalf("helmTemplate: %v", err)
	}
	for _, want := range []string{
		"image: registry.example/app:override",                                          // the values override the chart's default
		"name: myrel\n  namespace: apps",                                                // the release coordinates
		"kind: ServiceMonitor",                                                          // rendered because the API version was declared
		"# Source: testchart/templates/hook.yaml", "image: registry.example/hook:2.0.0", // hooks are part of the render
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("render lacks %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "CustomResourceDefinition") {
		t.Errorf("render must leave the CRDs out (no --include-crds):\n%s", rendered)
	}
	if images := scrapeImages(rendered); len(images) != 2 {
		t.Errorf("scraped %v, want the app and the hook image", images)
	}

	// Without the declared API version the guarded object does not render —
	// the client-only capabilities are Helm's default set.
	rendered, err = helmTemplate("apps", "myrel", chartDir, "", vals, nil)
	if err != nil {
		t.Fatalf("helmTemplate without api versions: %v", err)
	}
	if strings.Contains(rendered, "ServiceMonitor") {
		t.Errorf("render must not carry the ServiceMonitor without its API version:\n%s", rendered)
	}
}

// TestHelmTemplateMissingDependency: a chart directory whose Chart.yaml
// declares a dependency that is not in charts/ is refused with the CLI's
// diagnosis — the lab reads a chart directory, it never builds into it.
func TestHelmTemplateMissingDependency(t *testing.T) {
	dir := isolateHelm(t)
	chartDir := writeChart(t, dir)
	chartYAML := "apiVersion: v2\nname: testchart\nversion: 0.1.0\ndependencies:\n  - name: sub\n    version: 1.0.0\n    repository: oci://registry.example/charts\n"
	if err := os.WriteFile(filepath.Join(chartDir, "Chart.yaml"), []byte(chartYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := helmTemplate("apps", "myrel", chartDir, "", map[string]any{}, nil)
	if err == nil {
		t.Fatal("expected the missing dependency to be refused")
	}
	for _, want := range []string{"missing in charts/ directory: sub", "helm dependency build", "helm template myrel"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// TestHelmValuesLoaders: the values file and the inlined values go through
// the same Helm loader — a `null` survives as the key's deletion marker (the
// chart's default is dropped at render time, as with `helm -f`), documents
// merge, and empty input is an empty set.
func TestHelmValuesLoaders(t *testing.T) {
	isolateHelm(t)
	raw := []byte("a:\n  b: 1\n  c: null\n---\na:\n  d: two\n")
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := helmValuesFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inlined, err := helmValues(raw)
	if err != nil {
		t.Fatal(err)
	}
	for name, vals := range map[string]map[string]any{"file": fromFile, "inlined": inlined} {
		a, ok := vals["a"].(map[string]any)
		if !ok {
			t.Fatalf("%s: a = %#v, want a map", name, vals["a"])
		}
		if a["b"] != float64(1) || a["d"] != "two" { // Helm's loader goes through JSON: numbers are float64
			t.Errorf("%s: documents not merged: %#v", name, a)
		}
		if v, present := a["c"]; !present || v != nil {
			t.Errorf("%s: c = %#v (present %v), want the nil marker kept", name, v, present)
		}
	}
	empty, err := helmValues(nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("helmValues(nil) = %#v, %v; want an empty set", empty, err)
	}
	if _, err := helmValuesFile(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("a missing values file must be an error")
	}
}

// TestLastRevisionUninstalled: an upgrade-or-install installs afresh when the
// newest revision in the history is an uninstalled one, and upgrades otherwise.
func TestLastRevisionUninstalled(t *testing.T) {
	rel := func(status releasecommon.Status) *release.Release {
		return &release.Release{Name: "r", Info: &release.Info{Status: status}}
	}
	if got, err := lastRevisionUninstalled(nil); err != nil || got {
		t.Errorf("no history: %v, %v; want false", got, err)
	}
	if got, err := lastRevisionUninstalled([]ri.Releaser{rel(releasecommon.StatusDeployed)}); err != nil || got {
		t.Errorf("deployed: %v, %v; want false", got, err)
	}
	if got, err := lastRevisionUninstalled([]ri.Releaser{rel(releasecommon.StatusDeployed), rel(releasecommon.StatusUninstalled)}); err != nil || !got {
		t.Errorf("uninstalled last: %v, %v; want true", got, err)
	}
	if _, err := lastRevisionUninstalled([]ri.Releaser{"not a release"}); err == nil {
		t.Error("an unexpected release type must be an error")
	}
}

// TestHelmChartLabel: the chart reference as messages word it.
func TestHelmChartLabel(t *testing.T) {
	if got := helmChartLabel("oci://reg/charts/x", "1.2.3"); got != "oci://reg/charts/x --version 1.2.3" {
		t.Errorf("got %q", got)
	}
	if got := helmChartLabel("/path/to/chart", ""); got != "/path/to/chart" {
		t.Errorf("got %q", got)
	}
}

// TestHelmReleaseProbesWithoutCluster: the release probes answer "no" (and the
// chart lookup fails by name) when the lab is not up — no kubeconfig file — instead
// of reading whatever cluster the shell's KUBECONFIG points at.
func TestHelmReleaseProbesWithoutCluster(t *testing.T) {
	isolateHelm(t)
	if helmReleaseExists("agent-platform", "agent-platform") {
		t.Error("helmReleaseExists must be false without a lab kubeconfig")
	}
	_, err := helmReleaseChart("agent-platform", "agent-platform")
	if err == nil {
		t.Fatal("helmReleaseChart must fail without a lab kubeconfig")
	}
	var pathErr *os.PathError
	if !strings.Contains(err.Error(), labKubeconfigPath) && !errors.As(err, &pathErr) {
		t.Errorf("error should name the missing lab kubeconfig: %v", err)
	}
}

// TestHelmValuesFilesLaterWins: platform.valuesFiles are read as repeated
// `-f` flags — maps merge, lists replace, the later file wins.
func TestHelmValuesFilesLaterWins(t *testing.T) {
	isolateHelm(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "base.yaml")
	overlay := filepath.Join(dir, "overlay.yaml")
	if err := os.WriteFile(base, []byte("components:\n  kagent:\n    enabled: true\n    omitKeys: [a, b]\nkeep: base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlay, []byte("components:\n  kagent:\n    repository: oci://kind-registry:5000/charts\n    omitKeys: [c]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vals, err := helmValuesFiles(base, overlay)
	if err != nil {
		t.Fatal(err)
	}
	kagent, _ := vals["components"].(map[string]any)["kagent"].(map[string]any)
	if kagent["enabled"] != true || kagent["repository"] != "oci://kind-registry:5000/charts" {
		t.Errorf("maps not merged: %#v", kagent)
	}
	if keys, _ := kagent["omitKeys"].([]any); len(keys) != 1 || keys[0] != "c" {
		t.Errorf("lists must be replaced by the later file, got %#v", kagent["omitKeys"])
	}
	if vals["keep"] != "base" {
		t.Errorf("keys only the base file has must survive, got %#v", vals["keep"])
	}
}
