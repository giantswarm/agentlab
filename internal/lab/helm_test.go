package lab

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chart "helm.sh/helm/v4/pkg/chart/v2"
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

// The files a chart fixture is made of. Named because several fixtures below
// lay out charts of their own and goconst counts the repeats.
const (
	chartYAML    = "Chart.yaml"
	chartValues  = "values.yaml"
	chartSchema  = "values.schema.json"
	chartConfMap = "templates/cm.yaml"
	// The keys the schema-chart tests send — one the fixtures' schemas know,
	// one they do not — and the knob a release-mode refusal points at.
	knownKey         = "known"
	unknownKey       = "unknown"
	chartVersionKnob = "platform.chartVersion"
)

// writeChartFiles lays out a chart directory under dir from name -> content,
// creating the template subdirectories on the way, and returns its path.
func writeChartFiles(t *testing.T, dir, name string, files map[string]string) string {
	t.Helper()
	chartDir := filepath.Join(dir, name)
	for file, content := range files {
		path := filepath.Join(chartDir, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return chartDir
}

// writeChart lays out a minimal chart: a Deployment whose image comes from
// the values, a ServiceMonitor guarded on the Prometheus Operator API being
// known, a pre-install hook Job, and a CRD.
func writeChart(t *testing.T, dir string) string {
	t.Helper()
	files := map[string]string{
		chartYAML:   "apiVersion: v2\nname: testchart\nversion: 0.1.0\n",
		chartValues: "image: registry.example/app:1.0.0\nmonitor: true\n",
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
	return writeChartFiles(t, dir, "testchart", files)
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

	rendered, resolved, err := helmTemplate("apps", "myrel", chartDir, "", vals, []string{"monitoring.coreos.com/v1"})
	if err != nil {
		t.Fatalf("helmTemplate: %v", err)
	}
	// The version rendered is read off the loaded chart: what a range
	// resolved to, for the boot log and a refusal to name.
	if resolved != "0.1.0" {
		t.Errorf("resolved version = %q, want the chart's 0.1.0", resolved)
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
	rendered, _, err = helmTemplate("apps", "myrel", chartDir, "", vals, nil)
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
	withDependency := "apiVersion: v2\nname: testchart\nversion: 0.1.0\ndependencies:\n  - name: sub\n    version: 1.0.0\n    repository: oci://registry.example/charts\n"
	if err := os.WriteFile(filepath.Join(chartDir, chartYAML), []byte(withDependency), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := helmTemplate("apps", "myrel", chartDir, "", map[string]any{}, nil)
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

// TestReleaseDeployedAs: a revision is up to date when it is deployed from
// the very chart version with the very values — read back from the release
// storage, where every number is a float64 and keys come in any order; a
// superseded or failed revision, another version or other values are not.
func TestReleaseDeployedAs(t *testing.T) {
	vals := func(port any) map[string]any {
		return map[string]any{"port": port, "hosts": []any{"a", "b"}, "nested": map[string]any{"flag": true}}
	}
	stored, fresh, changed := vals(float64(8080)), vals(8080), vals(8081)
	rel := func(status releasecommon.Status, version string) *release.Release {
		return &release.Release{
			Name:    platformRelease,
			Version: 3,
			Info:    &release.Info{Status: status},
			Chart:   &chart.Chart{Metadata: &chart.Metadata{Name: platformRelease, Version: version}},
			Config:  stored,
		}
	}
	const v = "3.22.1-dev.poc-kagent-main.2026-09-09.22-55-49.h7aa7836"
	if !releaseDeployedAs(rel(releasecommon.StatusDeployed, v), v, fresh) {
		t.Error("deployed from the same chart with the same values must be up to date")
	}
	if releaseDeployedAs(rel(releasecommon.StatusSuperseded, v), v, fresh) {
		t.Error("a superseded revision is not")
	}
	if releaseDeployedAs(rel(releasecommon.StatusFailed, v), v, fresh) {
		t.Error("a failed revision is not")
	}
	if releaseDeployedAs(rel(releasecommon.StatusDeployed, "3.22.2"), v, fresh) {
		t.Error("another chart version is not")
	}
	if releaseDeployedAs(rel(releasecommon.StatusDeployed, v), v, changed) {
		t.Error("changed values are not")
	}
	if releaseDeployedAs(&release.Release{Info: &release.Info{Status: releasecommon.StatusDeployed}}, v, fresh) {
		t.Error("a revision without a chart is not")
	}
}

// TestSameValues: nil and empty are one values set; a list's order matters
// (lists replace, they never merge).
func TestSameValues(t *testing.T) {
	if !sameValues(nil, map[string]any{}) {
		t.Error("nil and empty must be equal")
	}
	if sameValues(map[string]any{"a": 1}, nil) {
		t.Error("a value against none must differ")
	}
	if sameValues(map[string]any{"l": []any{"a", "b"}}, map[string]any{"l": []any{"b", "a"}}) {
		t.Error("a reordered list is a different list")
	}
	if !sameValues(map[string]any{"n": nil}, map[string]any{"n": nil}) {
		t.Error("a null (Helm's deletion marker) equals a null")
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
	// A chart directory is never up to date, and that answer needs no
	// cluster; a versioned chart's check does, and fails the same way.
	if rev, err := helmDeployedRevision(platformNamespace, platformRelease, "", nil); err != nil || rev != 0 {
		t.Errorf("a chart directory: %d, %v; want 0 without touching a cluster", rev, err)
	}
	if _, err := helmDeployedRevision(platformNamespace, platformRelease, "3.22.2", nil); err == nil {
		t.Error("helmDeployedRevision must fail without a lab kubeconfig")
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

// closedSchema is a closed values.schema.json (additionalProperties: false)
// over the keys writeSchemaChart's values carry — the shape every component
// chart of the platform that rejects a forwarded key has.
const closedSchema = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "properties": {"known": {"type": "string"}, "boom": {"type": "boolean"}},
  "additionalProperties": false
}`

// writeSchemaChart lays out a chart named name whose values.schema.json is
// schema, with fixed values (`known`, `boom`) and a template that fails on
// demand (`boom: true`), so a schema verdict and an ordinary render failure
// can be told apart in the same chart.
func writeSchemaChart(t *testing.T, dir, name, schema string) string {
	t.Helper()
	return writeChartFiles(t, dir, name, map[string]string{
		chartYAML:   "apiVersion: v2\nname: " + name + "\nversion: 0.1.0\n",
		chartValues: "known: a\nboom: false\n",
		chartSchema: schema,
		chartConfMap: `{{- if .Values.boom }}{{ fail "boom" }}{{ end }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}
`,
	})
}

// writeClosedSchemaChart is writeSchemaChart with closedSchema, as
// `closedchart`.
func writeClosedSchemaChart(t *testing.T, dir string) string {
	t.Helper()
	return writeSchemaChart(t, dir, "closedchart", closedSchema)
}

// TestHelmTemplateSchemaRejection: a value a schema forbids — the chart's own
// or a subchart's — comes back as a *schemaRejection carrying Helm's whole
// verdict, the class the install must refuse before it starts, while a
// template that merely fails does not, however it is worded.
func TestHelmTemplateSchemaRejection(t *testing.T) {
	dir := isolateHelm(t)
	chartDir := writeClosedSchemaChart(t, dir)

	_, _, err := helmTemplate("apps", "rel", chartDir, "", map[string]any{unknownKey: 1}, nil)
	if err == nil {
		t.Fatal("a value outside the chart's closed schema rendered")
	}
	var rejected *schemaRejection
	if !errors.As(err, &rejected) {
		t.Fatalf("a schema rejection is not reported as one: %v", err)
	}
	for _, want := range []string{"closedchart", unknownKey} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the rejection does not name %q (the chart whose schema refused and the offending key): %v", want, err)
		}
	}
	// Helm's headline is the callers' to word; the verdict starts with the
	// chart it came from.
	if strings.Contains(err.Error(), helmSchemaPrefix) {
		t.Errorf("the rejection still carries Helm's headline: %v", err)
	}

	// The values are fine, the template fails: nothing here predicts the
	// install, so it must NOT be a schema rejection.
	_, _, err = helmTemplate("apps", "rel", chartDir, "", map[string]any{"boom": true}, nil)
	if err == nil {
		t.Fatal("the failing template rendered")
	}
	if isSchemaRejection(err) {
		t.Fatalf("an ordinary template failure is read as a schema rejection: %v", err)
	}

	// A refusal that lives only in a SUBCHART's schema is one too: Helm
	// validates the dependency tree, and so does the classification.
	parent := writeChartFiles(t, dir, "parent", map[string]string{
		chartYAML:                          "apiVersion: v2\nname: parent\nversion: 0.1.0\n",
		chartValues:                        "{}\n",
		chartConfMap:                       "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n",
		"charts/closedsub/" + chartYAML:    "apiVersion: v2\nname: closedsub\nversion: 0.1.0\n",
		"charts/closedsub/" + chartValues:  "known: a\n",
		"charts/closedsub/" + chartSchema:  closedSchema,
		"charts/closedsub/" + chartConfMap: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-sub\n",
	})
	_, _, err = helmTemplate("apps", "rel", parent, "", map[string]any{"closedsub": map[string]any{unknownKey: 1}}, nil)
	if err == nil {
		t.Fatal("a value outside the subchart's closed schema rendered")
	}
	if !errors.As(err, &rejected) {
		t.Fatalf("a subchart's schema rejection is not reported as one: %v", err)
	}
	if !strings.Contains(err.Error(), "closedsub") {
		t.Errorf("the rejection does not name the subchart whose schema refused: %v", err)
	}
}

// TestHelmTemplateSchemaLoadFailureIsNotARejection: a chart whose schema Helm
// cannot compile fails the render, but says nothing about the values — a
// remote $ref this host cannot fetch is the real case, and helm-controller
// with cluster egress may render the same chart fine. It must stay an ordinary
// failure, or an offline host turns an installable chart into a refused one.
// The fixture's $ref fails to compile without touching the network (a JSON
// pointer into nothing): the same branch of Helm's validation as the remote
// one, at no cost and with no proxy to stall on.
func TestHelmTemplateSchemaLoadFailureIsNotARejection(t *testing.T) {
	dir := isolateHelm(t)
	chartDir := writeSchemaChart(t, dir, "badschema", `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "properties": {"known": {"$ref": "#/definitions/missing"}}
}`)

	_, _, err := helmTemplate("apps", "rel", chartDir, "", map[string]any{knownKey: "a"}, nil)
	if err == nil {
		t.Fatal("a schema that cannot compile rendered")
	}
	// It did fail at validation, and named the chart — exactly the failure
	// the classification must leave alone.
	for _, want := range []string{helmSchemaPrefix, "badschema"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure is not Helm's schema failure for the chart (lacks %q): %v", want, err)
		}
	}
	if isSchemaRejection(err) {
		t.Fatalf("a schema Helm could not load is read as a refusal of the values: %v", err)
	}
}
