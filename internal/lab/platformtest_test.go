package lab

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestSubstrateShipped: the chart in place ships Agent Substrate when it
// renders the substrate HelmRelease in the platform namespace — a
// HelmRelease elsewhere or of another name is not it; the 3.x line ships
// none and is told apart without a read; a read the apiserver refuses is an
// error, never "no Substrate".
func TestSubstrateShipped(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	newFakeLab(t, customObject(fluxHelmReleaseGVK, platformNamespace, "muster", nil))
	if shipped, err := substrateShipped(ctx, cfg); err != nil || shipped {
		t.Errorf("without a substrate HelmRelease: %v, %v; want false", shipped, err)
	}
	newFakeLab(t, customObject(fluxHelmReleaseGVK, kagentNamespace, substrateRelease, nil))
	if shipped, err := substrateShipped(ctx, cfg); err != nil || shipped {
		t.Errorf("a substrate HelmRelease in another namespace: %v, %v; want false", shipped, err)
	}
	newFakeLab(t, customObject(fluxHelmReleaseGVK, platformNamespace, substrateRelease, nil))
	if shipped, err := substrateShipped(ctx, cfg); err != nil || !shipped {
		t.Errorf("with the substrate HelmRelease: %v, %v; want true", shipped, err)
	}

	legacy := config.Default()
	legacy.Platform.ChartVersion = legacyChartVersion
	f := newFakeLab(t, customObject(fluxHelmReleaseGVK, platformNamespace, substrateRelease, nil))
	reads := 0
	f.dyn.PrependReactor("list", "helmreleases", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		return true, nil, errors.New("the apiserver is down")
	})
	if shipped, err := substrateShipped(ctx, legacy); err != nil || shipped || reads != 0 {
		t.Errorf("the 3.x line: %v, %v after %d reads; want false without a read", shipped, err, reads)
	}
	if _, err := substrateShipped(ctx, cfg); err == nil || !strings.Contains(err.Error(), "the apiserver is down") {
		t.Errorf("a refused read: %v, want the apiserver's error", err)
	}
}
