package lab

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestAppliedChartVersion: the version a HelmRelease's newest history entry
// applied; none before its first revision.
func TestAppliedChartVersion(t *testing.T) {
	hr := &unstructured.Unstructured{Object: map[string]any{crStatus: map[string]any{historyField: []any{
		map[string]any{chartVersionField: "4.6.1"},
		map[string]any{chartVersionField: "4.6.0"},
	}}}}
	if v := appliedChartVersion(hr); v != "4.6.1" {
		t.Errorf("appliedChartVersion = %q", v)
	}
	if v := appliedChartVersion(&unstructured.Unstructured{Object: map[string]any{}}); v != "" {
		t.Errorf("appliedChartVersion without history = %q", v)
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
