package lab

import (
	"strings"
	"testing"
)

// The preload joins each rendered HelmRelease with the OCIRepository its
// chartRef names (same namespace unless given), keeps the release name,
// target namespace and inlined values, and skips a release whose source is
// not in the manifest.
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
		muster.URL != "oci://gsoci.azurecr.io/charts/giantswarm/muster" || muster.Version != ">=5.12.0 <6.0.0" {
		t.Errorf("muster: %+v", muster)
	}
	if !strings.Contains(string(muster.Values), "fullnameOverride: muster") || !strings.Contains(string(muster.Values), "registry: gsoci.azurecr.io") {
		t.Errorf("muster values not carried over: %s", muster.Values)
	}
	prom := got[1]
	if prom.Name != mcpPrometheusRelease || prom.Namespace != observabilityNamespace || prom.Version != "0.8.0" ||
		prom.URL != "oci://gsoci.azurecr.io/charts/giantswarm/mcp-prometheus" {
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
