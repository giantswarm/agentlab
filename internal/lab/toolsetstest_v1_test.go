package lab

import (
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The composed manifest is the composer's shape: OCIRepository first, then
// the HelmRelease with the top-level toolset value.
func TestComposeAgentManifestV1(t *testing.T) {
	m := composeAgentManifestV1("probe", "default-model-config", []string{presetReadOnly, workflowIncidentTriage})
	docs := strings.Split(m, "\n---\n")
	if len(docs) != 2 || !strings.Contains(docs[0], "kind: OCIRepository") || !strings.Contains(docs[1], "kind: HelmRelease") {
		t.Fatalf("wanted OCIRepository then HelmRelease, got:\n%s", m)
	}
	for _, want := range []string{
		"url: oci://gsoci.azurecr.io/charts/giantswarm/agent",
		"semver: x.x.x",
		"name: probe\n  namespace: kagent",
		`toolset: ["preset:read-only", "` + workflowIncidentTriage + `"]`,
		"modelConfig:\n      name: default-model-config",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
}

// helmReleaseWithToolset seeds an agent's HelmRelease carrying the top-level
// toolset value (nil for one applied without it).
func helmReleaseWithToolset(name string, toolset []any) *unstructured.Unstructured {
	hr := customObject(fluxHelmReleaseGVK, kagentNamespace, name, nil)
	if toolset != nil {
		_ = unstructured.SetNestedSlice(hr.Object, toolset, "spec", "values", "toolset")
	}
	return hr
}

// TestHelmReleaseToolset: the value is read off spec.values.toolset as a
// list of strings, reported unset when the HelmRelease carries none, refused
// when it is not a string list; a missing HelmRelease is the apiserver's
// NotFound.
func TestHelmReleaseToolset(t *testing.T) {
	odd := customObject(fluxHelmReleaseGVK, kagentNamespace, "odd", nil)
	_ = unstructured.SetNestedField(odd.Object, "preset:read-only", "spec", "values", "toolset")
	newFakeLab(t,
		helmReleaseWithToolset(toolsetsAgentReadOnly, []any{presetReadOnly, workflowIncidentTriage}),
		helmReleaseWithToolset(toolsetsAgentLegacy, nil),
		odd,
	)
	toolset, set, err := helmReleaseToolset(toolsetsAgentReadOnly)
	if err != nil || !set || !reflect.DeepEqual(toolset, []string{presetReadOnly, workflowIncidentTriage}) {
		t.Errorf("read-only agent: %v %v %v", toolset, set, err)
	}
	if toolset, set, err := helmReleaseToolset(toolsetsAgentLegacy); err != nil || set || toolset != nil {
		t.Errorf("legacy agent: %v %v %v, want unset", toolset, set, err)
	}
	if _, _, err := helmReleaseToolset("odd"); err == nil || !strings.Contains(err.Error(), "is not a string list") {
		t.Errorf("a scalar toolset: %v, want the refusal", err)
	}
	if _, _, err := helmReleaseToolset("absent"); !apierrors.IsNotFound(err) {
		t.Errorf("a missing HelmRelease: %v, want the apiserver's NotFound", err)
	}
}

// TestWaitAgentCR: the Agent is decoded as the apiserver returns it, its
// muster tool entry's header read; an Agent without a tool entry has no
// header to read.
func TestWaitAgentCR(t *testing.T) {
	agent := customObject(gvkAgent, kagentNamespace, toolsetsAgentReadOnly, nil)
	_ = unstructured.SetNestedSlice(agent.Object, []any{map[string]any{
		fieldType:     "McpServer",
		"headersFrom": []any{map[string]any{nameKey: toolsetHeader, "value": presetReadOnly}},
		"mcpServer":   map[string]any{nameKey: componentMuster},
	}}, "spec", "declarative", "tools")
	none := customObject(gvkAgent, kagentNamespace, toolsetsAgentNone, nil)
	_ = unstructured.SetNestedSlice(none.Object, []any{}, "spec", "declarative", "tools")
	newFakeLab(t, agent, none)

	cr, err := waitAgentCR(toolsetsAgentReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	header, err := toolsetHeaderOfCR(cr)
	if err != nil || header != presetReadOnly {
		t.Errorf("header = %q, %v", header, err)
	}
	if cr.Spec.Declarative.Tools[0].MCPServer == nil || cr.Spec.Declarative.Tools[0].MCPServer.Name != componentMuster {
		t.Errorf("mcpServer entry lost: %+v", cr.Spec.Declarative.Tools)
	}
	cr, err = waitAgentCR(toolsetsAgentNone)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := toolsetHeaderOfCR(cr); err == nil || len(cr.Spec.Declarative.Tools) != 0 {
		t.Errorf("an agent without a tool entry: %+v, %v", cr.Spec.Declarative.Tools, err)
	}
}

// TestDeleteAgentHelmRelease: the cleanup's delete is --ignore-not-found
// --wait=false — a present HelmRelease goes, a missing one is no failure.
func TestDeleteAgentHelmRelease(t *testing.T) {
	f := newFakeLab(t, helmReleaseWithToolset(toolsetsAgentFull, []any{presetFull}))
	if !agentHelmReleaseExists(toolsetsAgentFull) {
		t.Fatal("seed not visible")
	}
	deleteAgentHelmRelease(toolsetsAgentFull)
	deleteAgentHelmRelease("absent")
	if _, err := f.dyn.Tracker().Get(fluxHelmReleaseGVR, kagentNamespace, toolsetsAgentFull); !apierrors.IsNotFound(err) {
		t.Errorf("HelmRelease still there: %v", err)
	}
	if agentHelmReleaseExists(toolsetsAgentFull) {
		t.Error("agentHelmReleaseExists still true after the delete")
	}
}
