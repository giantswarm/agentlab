package lab

import (
	"errors"
	"strings"
	"testing"
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
