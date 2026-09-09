package lab

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestPrometheusAvailableReplicas reads the operator's verdict off the
// Prometheus CR: no CR yet is an error naming it, a CR without the field is
// "" (no verdict), a CR with the field its count.
func TestPrometheusAvailableReplicas(t *testing.T) {
	ctx := context.Background()
	newFakeLab(t)
	if _, err := prometheusAvailableReplicas(ctx); err == nil || !strings.Contains(err.Error(), "no prometheuses.monitoring.coreos.com in monitoring yet") {
		t.Errorf("no CR: %v", err)
	}
	prometheus := func(status map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{
			fieldAPIVersion: prometheusGVK.GroupVersion().String(),
			fieldKind:       prometheusGVK.Kind,
			fieldMetadata:   map[string]any{nameKey: kpsRelease + "-prometheus", fieldNamespace: observabilityNamespace},
		}}
		if status != nil {
			u.Object[fieldStatus] = status
		}
		return u
	}
	newFakeLab(t, prometheus(nil))
	if got, err := prometheusAvailableReplicas(ctx); err != nil || got != "" {
		t.Errorf("CR without a status: %q, %v; want \"\"", got, err)
	}
	newFakeLab(t, prometheus(map[string]any{"availableReplicas": int64(1)}))
	if got, err := prometheusAvailableReplicas(ctx); err != nil || got != "1" {
		t.Errorf("CR with one available replica: %q, %v", got, err)
	}
}
