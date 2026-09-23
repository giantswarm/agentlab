package lab

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// The preload joins each rendered HelmRelease with the OCIRepository its
// chartRef names (same namespace unless given), keeps the release name,
// target namespace, semver range, semverFilter and inlined values, and skips
// a release whose source is not in the manifest.
func TestFluxReleases(t *testing.T) {
	manifests := `
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: muster
  namespace: agent-platform
spec:
  interval: 10m
  url: oci://gsoci.azurecr.io/charts/giantswarm/muster
  ref:
    semver: ">=5.12.0 <6.0.0"
    semverFilter: ".*-dev\\.poc-kagent-main\\..*"
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: muster
  namespace: agent-platform
spec:
  releaseName: muster
  targetNamespace: agent-platform
  chartRef:
    kind: OCIRepository
    name: muster
    namespace: agent-platform
  postRenderers:
    - kustomize:
        patches: []
  values:
    fullnameOverride: muster
    image:
      registry: gsoci.azurecr.io
---
# the lab's own release: an exact version, targetNamespace elsewhere, no
# chartRef namespace
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: mcp-prometheus
  namespace: agent-platform
spec:
  url: oci://gsoci.azurecr.io/charts/giantswarm/mcp-prometheus
  ref:
    semver: "0.8.0"
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: mcp-prometheus
  namespace: agent-platform
spec:
  targetNamespace: monitoring
  chartRef:
    kind: OCIRepository
    name: mcp-prometheus
  values:
    app:
      env:
        - name: PROMETHEUS_URL
          value: http://prometheus-operated.monitoring.svc.cluster.local:9090
---
# a HelmRelease whose source is a HelmRepository chart: nothing to resolve
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: other
  namespace: agent-platform
spec:
  chart:
    spec:
      chart: other
      sourceRef:
        kind: HelmRepository
        name: other
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: flux-operator
  namespace: agent-platform
spec: {}
`
	got, err := fluxReleases(manifests)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 releases, got %d: %+v", len(got), got)
	}
	muster := got[0]
	if muster.Name != "muster" || muster.Namespace != "agent-platform" ||
		muster.URL != "oci://gsoci.azurecr.io/charts/giantswarm/muster" || muster.Version != ">=5.12.0 <6.0.0" ||
		muster.Filter != `.*-dev\.poc-kagent-main\..*` {
		t.Errorf("muster: %+v", muster)
	}
	if !strings.Contains(string(muster.Values), "fullnameOverride: muster") || !strings.Contains(string(muster.Values), "registry: gsoci.azurecr.io") {
		t.Errorf("muster values not carried over: %s", muster.Values)
	}
	prom := got[1]
	if prom.Name != mcpPrometheusRelease || prom.Namespace != observabilityNamespace || prom.Version != "0.8.0" ||
		prom.URL != "oci://gsoci.azurecr.io/charts/giantswarm/mcp-prometheus" || prom.Filter != "" {
		t.Errorf("mcp-prometheus: %+v", prom)
	}
	if !strings.Contains(string(prom.Values), "PROMETHEUS_URL") {
		t.Errorf("mcp-prometheus values not carried over: %s", prom.Values)
	}
}

func TestFluxReleasesRejectsGarbage(t *testing.T) {
	if _, err := fluxReleases("kind: [unterminated"); err == nil {
		t.Fatal("want a parse error")
	}
}

// TestResolveComponentVersion: a component chart is rendered at the version
// source-controller would pull. A range without a semverFilter is left to
// Helm's own resolution (the registry is not even listed); with a filter the
// pick is the highest tag the filter's regexp matches within the range — the
// branch's newest dev build, never the stable release Helm would pick, never
// another branch's build; a range that admits no prerelease (no `-0` floor)
// admits no dev build, as in Flux; no match is an error the caller reports
// as a skipped render, a registry failure likewise.
func TestResolveComponentVersion(t *testing.T) {
	const (
		repo     = "oci://gsoci.azurecr.io/charts/giantswarm/agent-platform-connectivity"
		filter   = `.*-dev\.poc-kagent-main\..*`
		newest   = "3.22.1-dev.poc-kagent-main.2026-09-09.22-55-49.h7aa7836"
		devRange = ">=3.0.0-0 <4.0.0-0"
	)
	tags := []string{
		"3.22.2",
		"3.22.1",
		"3.22.1-dev.poc-kagent-main.2026-09-09.21-49-46.h173e421",
		newest,
		"3.22.2-dev.renovate-axios-1-x.2026-09-11.19-47-39.h4fb8c37",
		"3.22.2-dev.renovate-major-github--e-versions.2026-09-11.20-00-00.h1111111",
		"3.22.1-dev.teams-alignment-branch.2026-09-09.21-30-24.hb693e7a",
		"2.9.0-dev.poc-kagent-main.2026-09-08.10-00-00.h0000000",
		"not-a-version",
	}
	var listed []string
	t.Cleanup(func() { listChartTags = helmChartTags })
	listChartTags = func(url string) ([]string, error) {
		listed = append(listed, url)
		if strings.HasSuffix(url, "/unreachable") {
			return nil, errors.New("registry down")
		}
		return tags, nil
	}
	for _, tc := range []struct {
		name, url, versionRange, filter string
		want                            string
		wantErr                         string
	}{
		{name: "stable range, no filter: Helm's own resolution", url: repo, versionRange: ">=3.0.0 <4.0.0", want: ">=3.0.0 <4.0.0"},
		{name: "range and filter: the branch's newest dev build", url: repo, versionRange: devRange, filter: filter, want: newest},
		{name: "the filter keeps the range: an old major's build is out", url: repo, versionRange: ">=3.0.0-0 <3.22.1-0", filter: filter, wantErr: "no tag matches"},
		{name: "another branch's builds", url: repo, versionRange: devRange, filter: `.*-dev\.teams-alignment-branch\..*`, want: "3.22.1-dev.teams-alignment-branch.2026-09-09.21-30-24.hb693e7a"},
		{name: "no build of the branch: skipped, not fatal", url: repo, versionRange: devRange, filter: `.*-dev\.main\..*`, wantErr: "no tag matches semverFilter"},
		{name: "a range without a prerelease floor admits no dev build (Flux)", url: repo, versionRange: "3.x", filter: filter, wantErr: "no tag matches"},
		{name: "an invalid filter", url: repo, versionRange: devRange, filter: "(", wantErr: "semverFilter"},
		{name: "an invalid range", url: repo, versionRange: "not a range", filter: filter, wantErr: "version range"},
		{name: "the registry cannot be listed", url: repo + "/unreachable", versionRange: devRange, filter: filter, wantErr: "registry down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveComponentVersion(tc.url, tc.versionRange, tc.filter)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %q, %v; want an error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	// Only the filtered lookups list the registry: one per filtered case
	// above, none for the stable range.
	if want := 8; len(listed) != want {
		t.Errorf("the registry was listed %d times (%v), want %d — a range without a filter must not list it", len(listed), listed, want)
	}
}

// TestFilteredNote: the render count names the versions picked through a
// semverFilter; the stable channel adds nothing.
func TestFilteredNote(t *testing.T) {
	if got := filteredNote(nil); got != "" {
		t.Errorf("stable channel: %q", got)
	}
	got := filteredNote([]string{"agent-platform-connectivity 3.22.1-dev.x", "backstage 0.244.5-dev.x"})
	if got != " (2 through a semverFilter: agent-platform-connectivity 3.22.1-dev.x, backstage 0.244.5-dev.x)" {
		t.Errorf("got %q", got)
	}
}

// The roster says what the chart ships: a `substrate` release means Agent
// Substrate comes with the chart (the lab installs none, checks the gates
// and budgets for the pool), a `cloudnative-pg` release the platform
// Postgres; a roster that could not be rendered ships nothing.
func TestPlatformRosterShips(t *testing.T) {
	fourX := &platformRoster{releases: []fluxRelease{{Name: componentKagent}, {Name: substrateRelease}, {Name: "substrate-crds"}, {Name: cnpgRelease}}}
	if !fourX.shipsSubstrate() || !fourX.shipsCNPG() {
		t.Errorf("the 4.x roster ships Substrate and CNPG: %v / %v", fourX.shipsSubstrate(), fourX.shipsCNPG())
	}
	stable := &platformRoster{releases: []fluxRelease{{Name: componentKagent}, {Name: componentMuster}}}
	if stable.shipsSubstrate() || stable.shipsCNPG() {
		t.Errorf("a roster without the releases ships neither: %v / %v", stable.shipsSubstrate(), stable.shipsCNPG())
	}
	var none *platformRoster
	if none.shipsSubstrate() || none.has(componentMuster) {
		t.Error("a nil roster (the render failed) ships nothing")
	}
}

// The side-load carries tagged references only: a digest-pinned one is the
// node's to pull (an archive of it imports unnamed and the CRI cannot start a
// pod from it).
func TestSplitDigestRefs(t *testing.T) {
	tagged, byDigest := splitDigestRefs([]string{
		"gsoci.azurecr.io/giantswarm/kagent/golang-adk@sha256:a2d23f5eb9c01e1903459a6e742f7d4aaa5e950d7e9aa6f07f8982761be0163a",
		"gsoci.azurecr.io/giantswarm/muster:5.18.3",
		"rustfs/rustfs:1.0.0-beta.3@sha256:378642b05b7dcb4849fb77ebe6aca4ced1c3f66e7e504247df95a5c9018d3358",
	})
	if len(tagged) != 1 || tagged[0] != "gsoci.azurecr.io/giantswarm/muster:5.18.3" {
		t.Errorf("tagged = %v", tagged)
	}
	if len(byDigest) != 2 {
		t.Errorf("byDigest = %v", byDigest)
	}
}

// componentManifest is one OCIRepository + HelmRelease pair of a rendered
// meta chart, the component release named `component`: the chart at chartDir,
// the HelmRelease's spec.values as given (indented YAML), and any further
// spec lines (a valuesFrom, an install block) before them.
func componentManifest(chartDir, spec, values string) string {
	return `apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: component
  namespace: default
spec:
  url: ` + chartDir + `
  ref:
    tag: "0.1.0"
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: component
  namespace: default
spec:
  chartRef:
    kind: OCIRepository
    name: component
    namespace: default
` + spec + `  values:
` + values
}

// TestPlatformImagesRefusesARejectedComponent: a component chart that refuses
// the values its HelmRelease carries stops the install, while a component
// chart that fails otherwise, or is not there, stays a note and the rest of
// the preload proceeds.
// These are the two halves of the same step: one predicts the install's
// outcome, the other only says an image will be pulled in-node. The release's
// own facts decide the borderline: whose it is changes the advice, a
// valuesFrom keeps only a verdict the reference could answer a note, and a
// HelmRelease that turns validation off gets no verdict on the cluster.
func TestPlatformImagesRefusesARejectedComponent(t *testing.T) {
	dir := isolateHelm(t)
	closed := writeClosedSchemaChart(t, dir)
	// A schema that wants a key the values do not carry: the `missing
	// property` class, the one a valuesFrom can answer.
	demanding := writeSchemaChart(t, dir, "demandingchart", `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "properties": {"known": {"type": "string"}, "boom": {"type": "boolean"}, "needed": {"type": "string"}},
  "required": ["needed"]
}`)
	cfg := &config.Config{}
	chart := platformChart{ref: config.ChartRepository, version: "0.0.0-test"}

	rosterFor := func(chartDir, values string) *platformRoster {
		t.Helper()
		manifest := componentManifest(chartDir, "", values)
		releases, err := fluxReleases(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if len(releases) != 1 {
			t.Fatalf("fixture joined %d releases, want 1", len(releases))
		}
		return &platformRoster{chart: chart, manifest: manifest, releases: releases}
	}
	refusal := func(what string, roster *platformRoster, wants ...string) {
		t.Helper()
		_, _, err := platformImages(cfg, roster)
		if err == nil {
			t.Fatalf("%s did not stop the install", what)
		}
		for _, want := range wants {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal of %s does not mention %q:\n%s", what, want, err)
			}
		}
		// Whole, not an excerpt: the offending path is the end of Helm's
		// message.
		if strings.Contains(err.Error(), "...") {
			t.Errorf("the refusal of %s is truncated, hiding the schema path:\n%s", what, err)
		}
	}
	proceeds := func(what string, roster *platformRoster) {
		t.Helper()
		if _, _, err := platformImages(cfg, roster); err != nil {
			t.Errorf("%s stopped the install: %v", what, err)
		}
	}

	// Refused: a key the chart's closed schema does not allow, with the
	// release, the chart and the knob that selects it.
	refusal("a component chart that refuses its values", rosterFor(closed, "    nosuchkey: 1\n"),
		"refuse", "component (", "nosuchkey", chartVersionKnob)

	// Accepted values, failing template: nothing here says the install would
	// fail, so the preload notes it and carries on.
	proceeds("an ordinary render failure", rosterFor(closed, "    boom: true\n"))

	// A chart that is not there is the same best-effort case: the source
	// answered, no — unlike a registry that does not answer at all, which
	// stops the install (TestPlatformImagesStopsOnAnUnreachableRegistry).
	absent := rosterFor(closed, "    known: a\n")
	absent.releases[0].URL = filepath.Join(dir, "no-such-chart")
	proceeds("a component chart that is not there", absent)

	// A release the LAB renders, not the meta chart's: refused all the same
	// — helm-controller installs it and waits for it like any component —
	// but platform.chartVersion has no say over its chart or its values, so
	// the advice is agentlab's, not the chart's.
	labOwned := rosterFor(closed, "    nosuchkey: 1\n")
	labOwned.releases[0].LabOwned = true
	refusal("a refusal in the lab's own release", labOwned, "nosuchkey", "lab's own release", "platform.observability")
	if _, _, err := platformImages(cfg, labOwned); strings.Contains(err.Error(), chartVersionKnob) {
		t.Errorf("the lab's own release is pointed at platform.chartVersion, which does not govern it:\n%s", err)
	}

	// A HelmRelease that also draws values from the cluster: a reference can
	// add keys, so a `missing property` verdict may be answered there and
	// stays a note — but it cannot take a key away, so `additional
	// properties … not allowed` is refused on the cluster too.
	withValuesFrom := rosterFor(closed, "    nosuchkey: 1\n")
	withValuesFrom.releases[0].ValuesFrom = true
	refusal("a closed schema refusing a key of a release with valuesFrom", withValuesFrom, "nosuchkey")
	missing := rosterFor(demanding, "    known: a\n")
	missing.releases[0].ValuesFrom = true
	proceeds("a missing property on a release with valuesFrom", missing)
	// Without the reference the same verdict is final.
	refusal("a missing property on a release without valuesFrom", rosterFor(demanding, "    known: a\n"), "needed")

	// A HelmRelease that turns helm-controller's validation off: the verdict
	// is one the cluster never asks for.
	unvalidated := rosterFor(closed, "    nosuchkey: 1\n")
	unvalidated.releases[0].SchemaValidationOff = true
	proceeds("a refusal on a release with schema validation off", unvalidated)
}

// TestFluxReleasesReadsTheValidationFacts: the join records what decides
// whether a render failure predicts the install — a valuesFrom (the lab
// cannot see those values) and a HelmRelease that turns helm-controller's
// schema validation off (the cluster never asks for the verdict).
func TestFluxReleasesReadsTheValidationFacts(t *testing.T) {
	one := func(spec string) fluxRelease {
		t.Helper()
		releases, err := fluxReleases(componentManifest("oci://example.test/c", spec, "    a: 1\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(releases) != 1 {
			t.Fatalf("joined %d releases, want 1", len(releases))
		}
		return releases[0]
	}

	plain := one("")
	if plain.ValuesFrom || plain.SchemaValidationOff {
		t.Errorf("a HelmRelease with spec.values only is recorded as %+v", plain)
	}
	withRef := one("  valuesFrom:\n    - kind: ConfigMap\n      name: kagent-images\n")
	if !withRef.ValuesFrom {
		t.Error("a HelmRelease with spec.valuesFrom is not recorded as drawing values from the cluster")
	}
	if withRef.SchemaValidationOff {
		t.Error("a HelmRelease with spec.valuesFrom is recorded as turning validation off")
	}
	for _, block := range []string{"install", "upgrade"} {
		if !one("  " + block + ":\n    disableSchemaValidation: true\n").SchemaValidationOff {
			t.Errorf("a HelmRelease with spec.%s.disableSchemaValidation is not recorded as turning validation off", block)
		}
	}
	if one("  install:\n    createNamespace: true\n").SchemaValidationOff {
		t.Error("an install block without disableSchemaValidation is recorded as turning validation off")
	}
}

// TestRecheckReleasesJudgesChangedValues: the dev-image swap re-renders the
// values after the preload's renders, so the pair the install carries goes
// to the charts once more — only the components whose spec.values moved, and
// with the same verdict as the first pass.
func TestRecheckReleasesJudgesChangedValues(t *testing.T) {
	dir := isolateHelm(t)
	component := writeClosedSchemaChart(t, dir)
	// A meta chart of one component whose spec.values are .Values.forward.
	meta := writeChartFiles(t, dir, "meta", map[string]string{
		chartYAML:   "apiVersion: v2\nname: meta\nversion: 0.1.0\n",
		chartValues: "component: \"\"\nforward: {}\n",
		"templates/component.yaml": `apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: component
  namespace: default
spec:
  url: {{ .Values.component }}
  ref:
    tag: "0.1.0"
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: component
  namespace: default
spec:
  chartRef:
    kind: OCIRepository
    name: component
    namespace: default
  values:
{{ toYaml .Values.forward | indent 4 }}
`,
	})
	chart := platformChart{ref: meta}
	valuesWith := func(forward map[string]any) map[string]any {
		return map[string]any{"component": component, "forward": forward}
	}
	before, err := renderPlatformRoster(chart, valuesWith(map[string]any{knownKey: "a"}))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}

	if err := recheckReleases(cfg, chart, before, valuesWith(map[string]any{knownKey: "a"})); err != nil {
		t.Errorf("unchanged values were refused: %v", err)
	}
	if err := recheckReleases(cfg, chart, before, valuesWith(map[string]any{knownKey: "b"})); err != nil {
		t.Errorf("changed but accepted values were refused: %v", err)
	}
	// The swap put a key the component's closed schema forbids.
	err = recheckReleases(cfg, chart, before, valuesWith(map[string]any{"nosuchkey": 1}))
	if err == nil {
		t.Fatal("a component that refuses the changed values did not stop the install")
	}
	for _, want := range []string{"nosuchkey", "Fix the chart at " + meta} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
}

// TestExcerptEnds: a render note keeps the tail of a long message, where Helm
// puts the part worth reading.
func TestExcerptEnds(t *testing.T) {
	long := "head " + strings.Repeat("x", 400) + "\n- at '/kyvernoPolicies': additional properties not allowed"
	got := excerptEnds(long, 120)
	if !strings.HasPrefix(got, "head ") || !strings.HasSuffix(got, "additional properties not allowed") || !strings.Contains(got, " ... ") {
		t.Errorf("excerptEnds kept the wrong ends: %q", got)
	}
	if short := excerptEnds("short\nmessage", 120); short != "short message" {
		t.Errorf("a short message must come back whole, flattened: %q", short)
	}
}

// The roster of a chart directory (platform.chartPath) renders the
// connectivity release from the checkout's own connectivity chart — the one
// the lab pushes into the lab registry, whose kind-network name the host
// cannot resolve — and every other release, and every release of a registry
// chart, from the registry its OCIRepository names.
func TestLocalizeConnectivity(t *testing.T) {
	fresh := func() []fluxRelease {
		return []fluxRelease{
			{Name: componentMuster, URL: "oci://gsoci.azurecr.io/charts/giantswarm/muster", Version: ">=5.0.0 <6.0.0"},
			{Name: config.ConnectivityChartName, URL: "oci://agentlab-registry:5000/charts/agent-platform-connectivity", Version: placeholderChartVersion},
		}
	}
	registry := fresh()
	localizeConnectivity(registry, platformChart{ref: config.ChartRepository, version: releasedChartVersion})
	for _, rel := range registry {
		if rel.LocalChart != "" {
			t.Errorf("a registry chart's %s release is rendered from a directory: %q", rel.Name, rel.LocalChart)
		}
	}
	local := fresh()
	localizeConnectivity(local, platformChart{ref: "/src/agent-platform/helm/agent-platform"})
	if got, want := local[1].LocalChart, "/src/agent-platform/helm/agent-platform-connectivity"; got != want {
		t.Errorf("the connectivity release renders from %q, want the checkout's %q", got, want)
	}
	if local[0].LocalChart != "" {
		t.Errorf("the muster release of a chart directory is still the registry's: %q", local[0].LocalChart)
	}
	if !strings.Contains(local[1].chartLabel(""), local[1].LocalChart) || !strings.Contains(local[1].chartLabel(""), local[1].URL) {
		t.Errorf("the label of a localized release names neither the directory nor the push: %s", local[1].chartLabel(""))
	}
}

// A release with a LocalChart renders from that directory — the registry
// is not asked for the URL's tags, and the version reported is the
// directory's — while its values still go through the same render.
func TestRenderFluxReleaseFromTheLocalChart(t *testing.T) {
	dir := isolateHelm(t)
	chartDir := writeClosedSchemaChart(t, dir)
	listChartTags = func(string) ([]string, error) {
		t.Fatal("the registry was listed for a chart rendered from a directory")
		return nil, nil
	}
	t.Cleanup(func() { listChartTags = helmChartTags })

	rel := fluxRelease{
		Name: "closedchart", Namespace: "default",
		URL: "oci://agentlab-registry:5000/charts/closedchart", Version: placeholderChartVersion, Filter: ".*-dev.*",
		LocalChart: chartDir,
		Values:     []byte("known: a\n"),
	}
	rendered, version, err := renderFluxRelease(rel, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := chartDirVersion(chartDir); version != want {
		t.Errorf("version rendered = %q, want the directory's %s", version, want)
	}
	if !strings.Contains(rendered, "kind: ConfigMap") {
		t.Errorf("the directory's template did not render:\n%s", rendered)
	}
	// The directory's schema still judges the values.
	rel.Values = []byte("nosuchkey: 1\n")
	if _, _, err := renderFluxRelease(rel, nil); err == nil || !isSchemaRejection(err) {
		t.Errorf("a key the local chart's closed schema forbids rendered: %v", err)
	}
}
