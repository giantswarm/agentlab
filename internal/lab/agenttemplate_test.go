package lab

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// readyTemplate seeds an AgentTemplate with the controller's status shape for
// one Harness: the four conditions, the revisions, the warnings.
const (
	testRevision    = "3bd7156d4194"
	fieldConditions = "conditions"
	testReadyName   = "ready"
	testPong        = "pong"
	testDevUser     = "dev@lab.local"
	testSmoke       = "smoke"
	testIDPrefix    = "01a0"
	testToken       = "tok"
)

func readyTemplate(name, harness, readyStatus, readyMessage string) *unstructured.Unstructured {
	template := agentTemplateBinding(name, componentMuster)
	_ = unstructured.SetNestedField(template.Object, "default-model-config", "spec", "modelConfig", nameKey)
	_ = unstructured.SetNestedSlice(template.Object, []any{map[string]any{
		"harness":                  harness,
		"desiredRevision":          testRevision,
		"latestSuccessfulRevision": testRevision,
		"warnings":                 []any{"tool narrowing downgraded"},
		fieldConditions: []any{
			map[string]any{fieldType: "Accepted", fieldStatus: conditionTrue, fieldReason: "Accepted", fieldMessage: "Harness admission selector matches the AgentTemplate"},
			map[string]any{fieldType: condReady, fieldStatus: readyStatus, fieldReason: "Ready", fieldMessage: readyMessage},
		},
	}}, fieldStatus, "harnesses")
	return template
}

// TestAgentTemplateFrom: the status is read per Harness — the Ready condition
// of the named Harness, "" for another Harness or a missing condition — and
// the spec's binding, model and label the way the proofs assert them.
func TestAgentTemplateFrom(t *testing.T) {
	template, err := agentTemplateFrom(readyTemplate("smoke", kagentHarness, "True", "ActorTemplate golden snapshot is ready"))
	if err != nil {
		t.Fatal(err)
	}
	if status, msg := template.harnessCondition(kagentHarness, condReady); status != "True" || !strings.Contains(msg, "golden snapshot") {
		t.Errorf("Ready on %s = %q %q", kagentHarness, status, msg)
	}
	if status, _ := template.harnessCondition("claude", condReady); status != "" {
		t.Errorf("Ready on another Harness = %q, want none", status)
	}
	if status, _ := template.harnessCondition(kagentHarness, "Compatible"); status != "" {
		t.Errorf("a missing condition = %q, want none", status)
	}
	if template.mcpServer() != componentMuster || template.Spec.ModelConfig == nil || template.Spec.ModelConfig.Name != "default-model-config" || template.Metadata.Labels[harnessLabel] != kagentHarness {
		t.Errorf("spec lost: %+v", template.Spec)
	}
	if template.Status.Harnesses[0].LatestSuccessfulRevision != testRevision || len(template.Status.Harnesses[0].Warnings) != 1 {
		t.Errorf("harness status lost: %+v", template.Status.Harnesses[0])
	}
	if bare, err := agentTemplateFrom(customObject(gvkAgentTemplate, kagentNamespace, "bare", nil)); err != nil || bare.mcpServer() != "" || len(bare.Status.Harnesses) != 0 {
		t.Errorf("a bare template: %+v %v", bare, err)
	}
}

// TestWaitAgentTemplateReady: a template Ready on the Harness returns at
// once; one that is not names the Harness, the last Ready and Accepted
// conditions and the warnings, and points at the label the Harness admits.
func TestWaitAgentTemplateReady(t *testing.T) {
	newFakeLab(t,
		readyTemplate(testReadyName, kagentHarness, "True", "ActorTemplate golden snapshot is ready"),
		readyTemplate("stuck", kagentHarness, "False", "waiting for the golden snapshot"),
	)
	template, err := waitAgentTemplateReady(testReadyName, kagentHarness, 4*time.Second)
	if err != nil || template == nil || template.Metadata.Name != testReadyName {
		t.Fatalf("ready template: %v %v", template, err)
	}
	_, err = waitAgentTemplateReady("stuck", kagentHarness, pollInterval)
	if err == nil {
		t.Fatal("a template that never becomes Ready must fail")
	}
	for _, want := range []string{"never became Ready on Harness " + kagentHarness, `Ready="False" waiting for the golden snapshot`, `Accepted="True"`, "tool narrowing downgraded", harnessLabel + "=" + kagentHarness} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if _, err := waitAgentTemplateReady("absent", kagentHarness, pollInterval); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Errorf("a missing template: %v", err)
	}
}

// TestThrowawayAgentTemplate: the agent a proof brings along is a v1alpha3
// AgentTemplate in the kagent namespace on the Go ADK Harness, on the
// ModelConfig, labelled as agentlab's, without tool bindings.
func TestThrowawayAgentTemplate(t *testing.T) {
	manifest := throwawayAgentTemplate(testSmoke, defaultModelConfig, "a probe")
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(manifest), &obj); err != nil {
		t.Fatalf("%v\n%s", err, manifest)
	}
	u := &unstructured.Unstructured{Object: obj}
	labels := u.GetLabels()
	modelConfig, _, _ := unstructured.NestedString(obj, "spec", "modelConfig", nameKey)
	description, _, _ := unstructured.NestedString(obj, "spec", "description")
	if u.GetAPIVersion() != agentTemplateAPIVersion || u.GetKind() != "AgentTemplate" || u.GetName() != testSmoke || u.GetNamespace() != kagentNamespace ||
		labels[harnessLabel] != kagentHarness || labels[managedByLabel] != managedByAgentlabValue || modelConfig != defaultModelConfig || description != "a probe" {
		t.Errorf("throwaway template:\n%s", manifest)
	}
	if _, found, _ := unstructured.NestedSlice(obj, "spec", "tools"); found {
		t.Error("a throwaway agent binds no tools")
	}
}

// TestPortalAgentRows: the agents list is read off a bare list or an object
// carrying one, each row's name and readiness in the shapes a portal might
// use; a row without a name and a payload without a list are refused.
func TestPortalAgentRows(t *testing.T) {
	rows, err := portalAgentRows(map[string]any{"agents": []any{
		map[string]any{nameKey: "smoke", fieldNamespace: kagentNamespace, testReadyName: true},
		map[string]any{"metadata": map[string]any{nameKey: "stuck", fieldNamespace: kagentNamespace}, "state": "NotReady"},
		map[string]any{nameKey: "old", "status": "Ready"},
	}})
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	if !rows[0].ready || rows[0].name != "smoke" || rows[1].ready || rows[1].name != "stuck" || rows[1].readiness != "state=NotReady" || !rows[2].ready || rows[2].namespace != "" {
		t.Errorf("rows = %+v", rows)
	}
	if rows, err := portalAgentRows([]any{map[string]any{nameKey: "a"}}); err != nil || len(rows) != 1 || rows[0].ready || rows[0].readiness != "" {
		t.Errorf("a bare list: %+v %v", rows, err)
	}
	if _, err := portalAgentRows(map[string]any{"total": 0}); err == nil || !strings.Contains(err.Error(), "no agents/items list") {
		t.Errorf("no list: %v", err)
	}
	if _, err := portalAgentRows([]any{map[string]any{testReadyName: true}}); err == nil || !strings.Contains(err.Error(), "names no agent") {
		t.Errorf("a nameless row: %v", err)
	}
	if _, err := portalAgentRows("nope"); err == nil {
		t.Error("a string payload must fail")
	}
	if got := lastTextBut([]string{"ping", "", testPong}, "ping"); got != testPong {
		t.Errorf("lastTextBut = %q", got)
	}
}
