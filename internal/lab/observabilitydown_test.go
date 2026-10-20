package lab

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/agentlab/internal/config"
)

// observabilityLeftover is what a run with platform.observability on leaves
// on the cluster: the observability namespace, the mcp-prometheus
// HelmRelease and its OCIRepository, and a bystander in the platform
// namespace.
func observabilityLeftover() []runtime.Object {
	return []runtime.Object{
		namespaceObject(observabilityNamespace),
		fluxObject(fluxHelmReleaseGVK, platformNamespace, mcpPrometheusRelease, ""),
		fluxObject(fluxOCIRepositoryGVK, platformNamespace, mcpPrometheusRelease, ""),
		fluxObject(fluxHelmReleaseGVK, platformNamespace, componentMuster, platformRelease),
	}
}

// TestObservabilityDown: observability off removes the stack an earlier run
// installed, and leaves the platform's own releases.
func TestObservabilityDown(t *testing.T) {
	newFakeLab(t, observabilityLeftover()...)
	ctx := context.Background()
	if err := observabilityDown(ctx); err != nil {
		t.Fatal(err)
	}
	if exists, _ := objectExists(ctx, gvrNamespaces, "", observabilityNamespace); exists {
		t.Errorf("namespace %s must be gone", observabilityNamespace)
	}
	if mcpPrometheusReleaseExists(ctx) {
		t.Errorf("HelmRelease %s must be gone", mcpPrometheusRelease)
	}
	if exists, _ := objectExists(ctx, fluxOCIRepositoryGVR, platformNamespace, mcpPrometheusRelease); exists {
		t.Errorf("OCIRepository %s must be gone", mcpPrometheusRelease)
	}
	if exists, _ := objectExists(ctx, fluxHelmReleaseGVR, platformNamespace, componentMuster); !exists {
		t.Error("the platform's own HelmReleases must be left alone")
	}
	// A second run finds nothing to do.
	if err := observabilityDown(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestObservabilityStatus: status names a stack left over while the switch
// is off, and agrees with a lab that runs what it configures.
func TestObservabilityStatus(t *testing.T) {
	ctx := context.Background()
	on := &config.Config{Platform: config.Platform{Enabled: true, Observability: true}}
	off := &config.Config{Platform: config.Platform{Enabled: true}}

	newFakeLab(t, observabilityLeftover()...)
	for _, c := range []struct {
		cfg  *config.Config
		want string
	}{
		{on, "on (platform.observability)"},
		{off, "off, but left over from an earlier run: namespace monitoring, HelmRelease mcp-prometheus; `agentlab platform` removes it"},
	} {
		s, err := readObservabilityStatus(ctx, c.cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.String(); got != c.want {
			t.Errorf("status\n got %q\nwant %q", got, c.want)
		}
	}

	newFakeLab(t)
	s, err := readObservabilityStatus(ctx, off)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.String(); got != "off (platform.observability)" {
		t.Errorf("a lab without the stack: got %q", got)
	}
	var out strings.Builder
	if err := PrintStatus(&out, &Status{Name: teardownCluster, Observability: s}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "    prometheus  off (platform.observability)\n") {
		t.Errorf("PrintStatus must carry the observability line, got:\n%s", out.String())
	}
}
