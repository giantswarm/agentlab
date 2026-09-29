package lab

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// readyTemplate seeds an AgentTemplate with a model, a skill and the chart's
// annotations; readyAgentObject the Agent pairing it with a Harness, with the
// controller's status shape: the conditions, the revisions, the warnings.
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

func readyTemplate(name string) *unstructured.Unstructured {
	template := agentTemplateBinding(name, name)
	template.SetGeneration(1)
	template.SetAnnotations(map[string]string{displayNameAnnotation: "Smoke", iconURLAnnotation: testIconURL})
	_ = unstructured.SetNestedField(template.Object, "default-model-config", "spec", "modelConfig", nameKey)
	_ = unstructured.SetNestedSlice(template.Object, []any{map[string]any{
		nameKey: testSkillName, "source": map[string]any{"git": map[string]any{"url": testSkillURL, "commit": testCommit}, "path": testSkillName},
	}}, "spec", "skills")
	return template
}

// agentObjectSeed seeds an Agent referencing the template of its own name
// and the Harness, with no status yet.
func agentObjectSeed(name, harness string) *unstructured.Unstructured {
	agent := customObject(gvkAgent, kagentNamespace, name, nil)
	agent.SetGeneration(1)
	_ = unstructured.SetNestedField(agent.Object, name, "spec", "templateRef", nameKey)
	_ = unstructured.SetNestedField(agent.Object, harness, "spec", "harnessRef", nameKey)
	return agent
}

// readyAgentObject seeds an Agent with the controller's status shape: the
// Accepted and Ready conditions, the revisions, the warnings.
func readyAgentObject(name, harness, readyStatus, readyMessage string) *unstructured.Unstructured {
	agent := agentObjectSeed(name, harness)
	_ = unstructured.SetNestedField(agent.Object, int64(1), fieldStatus, "observedGeneration")
	_ = unstructured.SetNestedField(agent.Object, testRevision, fieldStatus, fieldDesiredRevision)
	_ = unstructured.SetNestedField(agent.Object, testRevision, fieldStatus, "latestSuccessfulRevision")
	_ = unstructured.SetNestedStringSlice(agent.Object, []string{"tool narrowing downgraded"}, fieldStatus, "warnings")
	_ = unstructured.SetNestedSlice(agent.Object, []any{
		map[string]any{fieldType: "Accepted", fieldStatus: conditionTrue, fieldReason: "Accepted", fieldMessage: "Agent accepted"},
		map[string]any{fieldType: condReady, fieldStatus: readyStatus, fieldReason: "Ready", fieldMessage: readyMessage},
	}, fieldStatus, fieldConditions)
	return agent
}

// TestAgentTemplateFrom: the template's binding, model, skills and
// annotations read the way the proofs assert them; the Agent's references,
// generations, revisions, warnings and conditions ("" for a missing
// condition) likewise.
func TestAgentTemplateFrom(t *testing.T) {
	template, err := agentTemplateFrom(readyTemplate(testSmoke))
	if err != nil {
		t.Fatal(err)
	}
	if template.mcpServer() != testSmoke || template.Spec.ModelConfig == nil || template.Spec.ModelConfig.Name != "default-model-config" || template.Metadata.Generation != 1 {
		t.Errorf("spec lost: %+v", template.Spec)
	}
	if template.Metadata.Annotations[displayNameAnnotation] != "Smoke" || template.Metadata.Annotations[iconURLAnnotation] != testIconURL {
		t.Errorf("annotations lost: %v", template.Metadata.Annotations)
	}
	skill := template.skill(testSkillName)
	if skill == nil || skill.Source.Git == nil || skill.Source.Git.URL != testSkillURL || skill.Source.Git.Commit != testCommit || skill.Source.Path != testSkillName || template.skill("absent") != nil {
		t.Errorf("skills lost: %+v", template.Spec.Skills)
	}
	if bare, err := agentTemplateFrom(customObject(gvkAgentTemplate, kagentNamespace, "bare", nil)); err != nil || bare.mcpServer() != "" {
		t.Errorf("a bare template: %+v %v", bare, err)
	}

	agent, err := agentFrom(readyAgentObject(testSmoke, kagentHarness, "True", "ActorTemplate golden snapshot is ready"))
	if err != nil {
		t.Fatal(err)
	}
	if agent.templateName() != testSmoke || agent.harnessName() != kagentHarness || agent.Metadata.Generation != 1 || agent.Status.ObservedGeneration != 1 {
		t.Errorf("agent lost: %+v", agent)
	}
	if status, msg := agent.Status.condition(condReady); status != "True" || !strings.Contains(msg, "golden snapshot") {
		t.Errorf("Ready = %q %q", status, msg)
	}
	if status, _ := agent.Status.condition("Compatible"); status != "" {
		t.Errorf("a missing condition = %q, want none", status)
	}
	if agent.Status.LatestSuccessfulRevision != testRevision || !reflect.DeepEqual(agent.Status.Warnings, []string{"tool narrowing downgraded"}) {
		t.Errorf("agent status lost: %+v", agent.Status)
	}
	if inline, err := agentFrom(customObject(gvkAgent, kagentNamespace, "inline", nil)); err != nil || inline.templateName() != "" || inline.harnessName() != "" || len(inline.Status.Conditions) != 0 {
		t.Errorf("an inline Agent: %+v %v", inline, err)
	}
}
