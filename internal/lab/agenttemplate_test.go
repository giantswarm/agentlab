package lab

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// readyTemplate seeds an AgentTemplate with the controller's status shape for
// one Harness: the four conditions, the revisions, the warnings.
const (
	testRevision         = "3bd7156d4194"
	fieldConditions      = "conditions"
	fieldHarness         = "harness"
	fieldDesiredRevision = "desiredRevision"
	fieldValue           = "value"
	testReadyName        = "ready"
	testPong             = "pong"
	testDevUser          = "dev@lab.local"
	testSmoke            = "smoke"
	testIDPrefix         = "01a0"
	testToken            = "tok"
)

func readyTemplate(name, harness, readyStatus, readyMessage string) *unstructured.Unstructured {
	template := agentTemplateBinding(name, name)
	template.SetGeneration(1)
	template.SetAnnotations(map[string]string{displayNameAnnotation: "Smoke", iconURLAnnotation: testIconURL})
	_ = unstructured.SetNestedField(template.Object, "default-model-config", "spec", "modelConfig", nameKey)
	_ = unstructured.SetNestedSlice(template.Object, []any{map[string]any{
		nameKey: testSkillName, "source": map[string]any{"git": map[string]any{"url": testSkillURL, "commit": testCommit}, "path": testSkillName},
	}}, "spec", "skills")
	_ = unstructured.SetNestedField(template.Object, int64(1), fieldStatus, "observedGeneration")
	_ = unstructured.SetNestedSlice(template.Object, []any{map[string]any{
		fieldHarness:               harness,
		fieldDesiredRevision:       testRevision,
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
// of the named Harness, "" for another Harness or a missing condition, the
// Harnesses that reported, the generations — and the spec's binding, model,
// skills, label and annotations the way the proofs assert them.
func TestAgentTemplateFrom(t *testing.T) {
	template, err := agentTemplateFrom(readyTemplate(testSmoke, kagentHarness, "True", "ActorTemplate golden snapshot is ready"))
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
	if !reflect.DeepEqual(template.harnessNames(), []string{kagentHarness}) || template.Metadata.Generation != 1 || template.Status.ObservedGeneration != 1 {
		t.Errorf("harnesses %v, generation %d observed %d", template.harnessNames(), template.Metadata.Generation, template.Status.ObservedGeneration)
	}
	if template.mcpServer() != testSmoke || template.Spec.ModelConfig == nil || template.Spec.ModelConfig.Name != "default-model-config" || template.Metadata.Labels[harnessLabel] != kagentHarness {
		t.Errorf("spec lost: %+v", template.Spec)
	}
	if template.Metadata.Annotations[displayNameAnnotation] != "Smoke" || template.Metadata.Annotations[iconURLAnnotation] != testIconURL {
		t.Errorf("annotations lost: %v", template.Metadata.Annotations)
	}
	skill := template.skill(testSkillName)
	if skill == nil || skill.Source.Git == nil || skill.Source.Git.URL != testSkillURL || skill.Source.Git.Commit != testCommit || skill.Source.Path != testSkillName || template.skill("absent") != nil {
		t.Errorf("skills lost: %+v", template.Spec.Skills)
	}
	if template.Status.Harnesses[0].LatestSuccessfulRevision != testRevision || len(template.Status.Harnesses[0].Warnings) != 1 {
		t.Errorf("harness status lost: %+v", template.Status.Harnesses[0])
	}
	if bare, err := agentTemplateFrom(customObject(gvkAgentTemplate, kagentNamespace, "bare", nil)); err != nil || bare.mcpServer() != "" || len(bare.Status.Harnesses) != 0 || len(bare.harnessNames()) != 0 {
		t.Errorf("a bare template: %+v %v", bare, err)
	}
}
