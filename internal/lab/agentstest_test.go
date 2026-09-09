package lab

import (
	"errors"
	"reflect"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// TestRulesBeyondDiscovery: of what `can-i --list` grants, the selfsubject*
// reviews and the non-resource URLs are everyone's; any other row is a
// permission of the principal's own. The CLI's header row, were it there,
// is skipped too.
func TestRulesBeyondDiscovery(t *testing.T) {
	rows := ruleRows(&authorizationv1.SubjectRulesReviewStatus{
		ResourceRules: []authorizationv1.ResourceRule{
			{Verbs: []string{verbCreate}, APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"}},
			{Verbs: []string{verbCreate}, APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"selfsubjectreviews"}},
		},
		NonResourceRules: []authorizationv1.NonResourceRule{{Verbs: []string{verbGet}, NonResourceURLs: []string{"/healthz", "/api", "/api/*"}}},
	})
	if got := rulesBeyondDiscovery(append([]string{"Resources  Non-Resource URLs  Resource Names  Verbs", ""}, rows...)); len(got) != 0 {
		t.Errorf("discovery-only rules judged as permissions: %v", got)
	}
	extra := ruleRows(&authorizationv1.SubjectRulesReviewStatus{ResourceRules: []authorizationv1.ResourceRule{
		{Verbs: []string{verbGet, verbList}, APIGroups: []string{fluxHelmReleaseGVK.Group}, Resources: []string{fluxHelmReleaseGVR.Resource}},
	}})
	if got := rulesBeyondDiscovery(append(rows, extra...)); !reflect.DeepEqual(got, extra) {
		t.Errorf("granted = %v, want %v", got, extra)
	}
}

// TestServiceAccountRules: the rules review goes out impersonating the
// ServiceAccount (kubectl's --as) in the asked namespace, on the lab admin's
// config.
func TestServiceAccountRules(t *testing.T) {
	newFakeLab(t)
	cs := kubefake.NewClientset()
	var namespaces []string
	cs.PrependReactor("create", "selfsubjectrulesreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review, ok := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectRulesReview)
		if !ok {
			return true, nil, errors.New("not a rules review")
		}
		namespaces = append(namespaces, review.Spec.Namespace)
		review.Status.ResourceRules = []authorizationv1.ResourceRule{{Verbs: []string{verbGet}, Resources: []string{gvrPods.Resource}}}
		return true, review, nil
	})
	seen := stubClientsetFor(t, cs)
	rows, err := serviceAccountRules(agentManagerServiceAccount, kagentNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows, []string{"pods  []  []  [get]"}) || !reflect.DeepEqual(namespaces, []string{kagentNamespace}) {
		t.Errorf("rows = %v in %v", rows, namespaces)
	}
	if len(*seen) != 1 || (*seen)[0].Impersonate.UserName != agentManagerServiceAccount || (*seen)[0].Host != fakeKindServer {
		t.Errorf("the review was not asked as the impersonated ServiceAccount on the lab's endpoint: %+v", *seen)
	}
}

// TestAgentTemplateManagers: the field managers of the agent's AgentTemplate
// are read off metadata.managedFields — who wrote it; a missing template is
// the apiserver's NotFound.
func TestAgentTemplateManagers(t *testing.T) {
	template := customObject(gvkAgentTemplate, kagentNamespace, agentsTestAgent, nil)
	_ = unstructured.SetNestedSlice(template.Object, []any{
		map[string]any{"manager": agentManagerMCPServer, "operation": "Apply"},
		map[string]any{"manager": "kagent-controller", "operation": "Update", "subresource": fieldStatus},
	}, "metadata", "managedFields")
	newFakeLab(t, template)
	managers, err := agentTemplateManagers(agentsTestAgent)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(managers, []string{agentManagerMCPServer, "kagent-controller"}) {
		t.Errorf("managers = %v", managers)
	}
	if _, err := agentTemplateManagers("absent"); err == nil {
		t.Error("a missing AgentTemplate must fail the read")
	}
	if !agentTemplateExists(agentsTestAgent) || agentTemplateExists("absent") {
		t.Error("agentTemplateExists disagrees with the store")
	}
}
