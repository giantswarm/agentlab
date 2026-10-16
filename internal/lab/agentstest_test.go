package lab

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// TestRulesBeyond: of what `can-i --list` grants, every row a ServiceAccount no
// binding names holds too — the selfsubject* reviews, the non-resource URLs,
// the bootstrap trust-bundle discovery — is everyone's; any other row is a
// permission of the principal's own. The CLI's header row, were it there, is
// skipped too.
func TestRulesBeyond(t *testing.T) {
	baseline := ruleRows(&authorizationv1.SubjectRulesReviewStatus{
		ResourceRules: []authorizationv1.ResourceRule{
			{Verbs: []string{verbCreate}, APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"}},
			{Verbs: []string{verbCreate}, APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"selfsubjectreviews"}},
			{Verbs: []string{verbGet, verbList, "watch"}, APIGroups: []string{"certificates.k8s.io"}, Resources: []string{"clustertrustbundles"}},
		},
		NonResourceRules: []authorizationv1.NonResourceRule{{Verbs: []string{verbGet}, NonResourceURLs: []string{"/healthz", "/api", "/api/*"}}},
	})
	if got := rulesBeyond(append([]string{"Resources  Non-Resource URLs  Resource Names  Verbs", ""}, baseline...), baseline); len(got) != 0 {
		t.Errorf("everyone's rules judged as permissions: %v", got)
	}
	extra := ruleRows(&authorizationv1.SubjectRulesReviewStatus{ResourceRules: []authorizationv1.ResourceRule{
		{Verbs: []string{verbGet, verbList}, APIGroups: []string{kagentGroupVersion.Group}, Resources: []string{gvrAgentTemplates.Resource}},
	}})
	if got := rulesBeyond(append(baseline, extra...), baseline); !reflect.DeepEqual(got, extra) {
		t.Errorf("granted = %v, want %v", got, extra)
	}
	if got := bootstrapExtras(baseline); got != ", clustertrustbundles.certificates.k8s.io" {
		t.Errorf("bootstrapExtras = %q", got)
	}
	if got := bootstrapExtras(baseline[:3]); got != "" {
		t.Errorf("bootstrapExtras without extras = %q", got)
	}
}

// TestMintedSkillsSecretNames: without skills.mintGitAuthSecret no names;
// with it the chart's current and earlier minted names, after the
// fullnameOverride, then the previous names not already among them.
func TestMintedSkillsSecretNames(t *testing.T) {
	if got := mintedSkillsSecretNames(map[string]any{agentManagerMCPServer: map[string]any{skillsKey: map[string]any{skillsMintValue: false}}}); got != nil {
		t.Errorf("minting off: %v, want none", got)
	}
	if got := mintedSkillsSecretNames(map[string]any{}); got != nil {
		t.Errorf("no agent-manager values: %v, want none", got)
	}
	want := []string{"tenant-credential-agent-manager-skills-token", "agent-manager-skills-token"}
	on := map[string]any{agentManagerMCPServer: map[string]any{skillsKey: map[string]any{skillsMintValue: true}}}
	if got := mintedSkillsSecretNames(on); !reflect.DeepEqual(got, want) {
		t.Errorf("minting on: %v, want %v", got, want)
	}
	renamed := map[string]any{agentManagerMCPServer: map[string]any{
		"fullnameOverride": "am",
		skillsKey:          map[string]any{skillsMintValue: true, "previousGitAuthSecretNames": []any{"am-skills-token", "old-skills"}},
	}}
	if got, want := mintedSkillsSecretNames(renamed), []string{"tenant-credential-am-skills-token", "am-skills-token", "old-skills"}; !reflect.DeepEqual(got, want) {
		t.Errorf("fullnameOverride and previous names: %v, want %v", got, want)
	}
}

// TestRulesBeyondMintGrant: minting off leaves every row granted; minting on
// drops exactly the skills-Secret Role and keeps any other grant — another
// resource, another Secret, another verb — and refuses a Role that is
// missing or grants less than the chart renders.
func TestRulesBeyondMintGrant(t *testing.T) {
	minted := []string{"tenant-credential-agent-manager-skills-token", "agent-manager-skills-token"}
	role := ruleRows(&authorizationv1.SubjectRulesReviewStatus{ResourceRules: []authorizationv1.ResourceRule{
		{Verbs: mintGrantVerbs, APIGroups: []string{""}, Resources: []string{gvrSecrets.Resource}, ResourceNames: minted},
	}})
	if got := role[0]; got != "secrets  []  [tenant-credential-agent-manager-skills-token agent-manager-skills-token]  [get update]" {
		t.Fatalf("the Role's row reads %q, not the shape the check parses", got)
	}
	extra := ruleRows(&authorizationv1.SubjectRulesReviewStatus{ResourceRules: []authorizationv1.ResourceRule{
		{Verbs: []string{verbGet, verbList}, APIGroups: []string{kagentGroupVersion.Group}, Resources: []string{gvrAgentTemplates.Resource}},
	}})

	if got, err := rulesBeyondMintGrant(role, nil); err != nil || !reflect.DeepEqual(got, role) {
		t.Errorf("minting off: %v, %v; want the Role still granted", got, err)
	}
	if got, err := rulesBeyondMintGrant(role, minted); err != nil || len(got) != 0 {
		t.Errorf("minting on, the Role alone: %v, %v; want nothing granted", got, err)
	}
	if got, err := rulesBeyondMintGrant(append(slices.Clone(role), extra...), minted); err != nil || !reflect.DeepEqual(got, extra) {
		t.Errorf("minting on, the Role and an extra grant: %v, %v; want %v", got, err, extra)
	}

	for name, rule := range map[string]authorizationv1.ResourceRule{
		"another Secret":    {Verbs: mintGrantVerbs, Resources: []string{gvrSecrets.Resource}, ResourceNames: append(slices.Clone(minted), "kagent-anthropic")},
		"another verb":      {Verbs: append(slices.Clone(mintGrantVerbs), verbList), Resources: []string{gvrSecrets.Resource}, ResourceNames: minted},
		"every Secret":      {Verbs: mintGrantVerbs, Resources: []string{gvrSecrets.Resource}},
		"the names on pods": {Verbs: mintGrantVerbs, Resources: []string{gvrPods.Resource}, ResourceNames: minted},
	} {
		wider := ruleRows(&authorizationv1.SubjectRulesReviewStatus{ResourceRules: []authorizationv1.ResourceRule{rule}})
		got, err := rulesBeyondMintGrant(append(slices.Clone(role), wider...), minted)
		if err != nil || !reflect.DeepEqual(got, wider) {
			t.Errorf("%s: %v, %v; want %v still granted", name, got, err, wider)
		}
	}

	if _, err := rulesBeyondMintGrant(nil, minted); err == nil {
		t.Error("minting on without the Role: no error")
	}
	partial := ruleRows(&authorizationv1.SubjectRulesReviewStatus{ResourceRules: []authorizationv1.ResourceRule{
		{Verbs: []string{verbGet}, Resources: []string{gvrSecrets.Resource}, ResourceNames: minted[:1]},
	}})
	if _, err := rulesBeyondMintGrant(partial, minted); err == nil {
		t.Error("minting on with get on one name only: no error")
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

// A managedFields entry's manager and operation.
const (
	managerField   = "manager"
	operationField = "operation"
)

// TestAgentTemplateManagers: the field managers of the agent's AgentTemplate
// are read off metadata.managedFields — who wrote it; a missing template is
// the apiserver's NotFound.
func TestAgentTemplateManagers(t *testing.T) {
	template := customObject(gvkAgentTemplate, kagentNamespace, agentsTestAgent, nil)
	_ = unstructured.SetNestedSlice(template.Object, []any{
		map[string]any{managerField: agentManagerMCPServer, operationField: "Apply"},
		map[string]any{managerField: "kagent-controller", operationField: "Update", "subresource": fieldStatus},
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
