package lab

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fakeAgentCRD is kagent's Agent CRD with the given served versions, each
// with an empty spec schema (or the given extra spec properties).
func fakeAgentCRD(versions ...string) *unstructured.Unstructured {
	var vs []any
	for _, v := range versions {
		vs = append(vs, map[string]any{
			nameKey:  v,
			"served": true,
			"schema": map[string]any{"openAPIV3Schema": map[string]any{"properties": map[string]any{
				"spec": map[string]any{"properties": map[string]any{
					"description": map[string]any{fieldType: "string"},
				}},
			}}},
		})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		fieldAPIVersion: gvkCRD.GroupVersion().String(),
		fieldKind:       gvkCRD.Kind,
		fieldMetadata:   map[string]any{nameKey: agentCRD},
		"spec":          map[string]any{"group": "kagent.dev", "versions": vs},
	}}
}

// TestPatchAgentCRDIconURL: the 0.9.x CRD gets spec.iconUrl added to its
// v1alpha2 schema by index (the other version untouched), a second run finds
// it and patches nothing, a CRD without v1alpha2 is refused by name, and a
// missing CRD fails with the read.
func TestPatchAgentCRDIconURL(t *testing.T) {
	f := newFakeLab(t, fakeAgentCRD("v1alpha1", "v1alpha2"))
	if err := patchAgentCRDIconURL(); err != nil {
		t.Fatal(err)
	}
	versions, _, _ := unstructured.NestedSlice(f.stored(t, gvrCRDs, "", agentCRD).Object, "spec", "versions")
	iconURL := func(i int) map[string]any {
		m, _, _ := unstructured.NestedMap(versions[i].(map[string]any), "schema", "openAPIV3Schema", "properties", "spec", "properties", "iconUrl")
		return m
	}
	if got := iconURL(1); got[fieldType] != "string" || got["format"] != "uri" {
		t.Errorf("v1alpha2 spec.iconUrl = %v, want an optional uri string", got)
	}
	if got := iconURL(0); got != nil {
		t.Errorf("v1alpha1 was patched too: %v", got)
	}
	if err := patchAgentCRDIconURL(); err != nil {
		t.Fatal(err)
	}
	if n := f.patchCount(gvrCRDs); n != 1 {
		t.Errorf("CRD patched %d times, want 1 (the second run finds the field)", n)
	}

	newFakeLab(t, fakeAgentCRD("v1alpha1"))
	if err := patchAgentCRDIconURL(); err == nil || !strings.Contains(err.Error(), "serves no v1alpha2") {
		t.Errorf("a CRD without v1alpha2: %v", err)
	}
	newFakeLab(t)
	if err := patchAgentCRDIconURL(); err == nil || !strings.Contains(err.Error(), "reading the agents.kagent.dev CRD") {
		t.Errorf("a missing CRD: %v", err)
	}
}
