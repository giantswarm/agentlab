package lab

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestAppliedChartVersion: the version a HelmRelease's newest history entry
// applied; none before its first revision.
func TestAppliedChartVersion(t *testing.T) {
	hr := &unstructured.Unstructured{Object: map[string]any{crStatus: map[string]any{historyField: []any{
		map[string]any{chartVersionField: testGatewayVersion},
		map[string]any{chartVersionField: "4.6.0"},
	}}}}
	if v := appliedChartVersion(hr); v != testGatewayVersion {
		t.Errorf("appliedChartVersion = %q", v)
	}
	if v := appliedChartVersion(&unstructured.Unstructured{Object: map[string]any{}}); v != "" {
		t.Errorf("appliedChartVersion without history = %q", v)
	}
}

// TestChartImageTag: Flux's chart digest suffix is no part of the image tag.
func TestChartImageTag(t *testing.T) {
	for in, want := range map[string]string{testGatewayVersion + "+f7b558f04b24": testGatewayVersion, testGatewayVersion: testGatewayVersion, "4.7.0-rc.1": "4.7.0-rc.1"} {
		if got := chartImageTag(in); got != want {
			t.Errorf("chartImageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestResolveGatewayImageNamed: a named image or a local build is used as
// given, without reading the cluster or the chart.
func TestResolveGatewayImageNamed(t *testing.T) {
	if img, err := resolveGatewayImage(nil, "example.io/gw:1", ""); err != nil || img != "example.io/gw:1" {
		t.Errorf("named image = %q, %v", img, err)
	}
	if img, err := resolveGatewayImage(nil, "", "/bin/gw"); err != nil || img != "" {
		t.Errorf("binary = %q, %v", img, err)
	}
}

// testGatewayVersion is a klaus-gateway release the tests name.
const testGatewayVersion = "4.6.1"
