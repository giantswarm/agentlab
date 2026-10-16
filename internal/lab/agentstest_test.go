package lab

import (
	"errors"
	"reflect"
	"slices"
	"strings"
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

// mintRoleRows is the skills-Secret Role agent-manager's chart renders with
// skills.mintGitAuthSecret (templates/skills-token.yaml), as the rules
// review words it.
var mintRoleRows = ruleRows(&authorizationv1.SubjectRulesReviewStatus{ResourceRules: []authorizationv1.ResourceRule{
	{Verbs: mintGrantVerbs, APIGroups: []string{""}, Resources: []string{gvrSecrets.Resource}, ResourceNames: []string{"tenant-credential-agent-manager-skills-token", "agent-manager-skills-token"}},
}})

// tenantsRoleRows is the tenant reconciler's ClusterRole agent-manager's
// chart renders with tenants.enabled (templates/tenants-rbac.yaml), the
// default ModelConfig's key kagent-anthropic, without a network policy, as
// `auth can-i --list` printed it on a lab with the tenants on.
var tenantsRoleRows = strings.Split(`namespaces  []  []  [get list watch]
agents.api.kagent.dev  []  []  [list watch]
helmreleases.helm.toolkit.fluxcd.io  []  []  [list watch]
harnesses.api.kagent.dev  []  []  [get list watch create update delete]
modelconfigs.api.kagent.dev  []  []  [get list watch create update delete]
workerpools.ate.dev  []  []  [get list watch create update delete]
ocirepositories.source.toolkit.fluxcd.io  []  []  [get list watch create update]
serviceaccounts  []  []  [get list watch create delete]
rolebindings.rbac.authorization.k8s.io  []  []  [get list watch create update delete]
clusterroles.rbac.authorization.k8s.io  []  [agent-platform-tenant-flux]  [bind]
secrets  []  [kagent-anthropic]  [get list watch update delete]
secrets  []  []  [create]`, "\n")

// switchValues is the lab's rendered values with minting and the tenants
// switched as asked, the default provider's key named key.
func switchValues(mint, tenants bool, key string) map[string]any {
	return map[string]any{
		agentManagerMCPServer: map[string]any{skillsKey: map[string]any{skillsMintValue: mint}},
		"tenants":             map[string]any{"organizations": map[string]any{valuesEnabled: tenants}},
		"kagent":              map[string]any{"providers": map[string]any{providersDefaultKey: "lab", "lab": map[string]any{"apiKeySecretRef": key}}},
	}
}

// joined is the rows of several sets after each other.
func joined(sets ...[]string) []string {
	var rows []string
	for _, s := range sets {
		rows = append(rows, s...)
	}
	return rows
}

// TestRulesBeyondGrants: for every combination of minting and the tenants,
// exactly the RBAC the chart renders for the switches that are on passes;
// a grant of a switch that is off, a grant beyond the chart's on any
// resource, name or verb, and a switch's grant missing in part all fail.
func TestRulesBeyondGrants(t *testing.T) {
	if got := mintRoleRows[0]; got != "secrets  []  [tenant-credential-agent-manager-skills-token agent-manager-skills-token]  [get update]" {
		t.Fatalf("the Role's row reads %q, not the shape the check parses", got)
	}
	extra := []string{"agenttemplates.api.kagent.dev  []  []  [get list]"}

	for _, c := range []struct {
		name          string
		mint, tenants bool
		held          []string
	}{
		{"no switch", false, false, nil},
		{"minting", true, false, mintRoleRows},
		{"tenants", false, true, tenantsRoleRows},
		{"minting and tenants", true, true, joined(tenantsRoleRows, mintRoleRows)},
	} {
		t.Run(c.name, func(t *testing.T) {
			grants := agentManagerGrants(switchValues(c.mint, c.tenants, "kagent-anthropic"))
			if got, err := rulesBeyondGrants(c.held, grants); err != nil || len(got) != 0 {
				t.Errorf("exactly the switches' RBAC: %v, %v; want nothing granted", got, err)
			}
			if got, err := rulesBeyondGrants(joined(c.held, extra), grants); err != nil || !reflect.DeepEqual(got, extra) {
				t.Errorf("with an extra grant: %v, %v; want %v", got, err, extra)
			}
			if !c.mint {
				if got, err := rulesBeyondGrants(joined(c.held, mintRoleRows), grants); err != nil || !reflect.DeepEqual(got, mintRoleRows) {
					t.Errorf("the minting Role with minting off: %v, %v; want it granted", got, err)
				}
			}
			if !c.tenants {
				if got, err := rulesBeyondGrants(joined(c.held, tenantsRoleRows), grants); err != nil || !reflect.DeepEqual(got, tenantsRoleRows) {
					t.Errorf("the tenants' ClusterRole with the tenants off: %v, %v; want every row granted", got, err)
				}
			}
			for i := range c.held {
				if _, err := rulesBeyondGrants(slices.Delete(slices.Clone(c.held), i, i+1), grants); err == nil {
					t.Errorf("without %q: no error", c.held[i])
				}
			}
		})
	}

	grants := agentManagerGrants(switchValues(true, true, "kagent-anthropic"))
	for name, row := range map[string]string{ // #nosec G101 -- RBAC rows naming Secrets, not credentials
		"another minted Secret": "secrets  []  [agent-manager-skills-token another]  [get update]",
		"another verb":          "secrets  []  [agent-manager-skills-token]  [get update list]",
		"every Secret":          "secrets  []  []  [get]",
		"any Secret deleted":    "secrets  []  []  [create delete]",
		"another model key":     "secrets  []  [kagent-openai]  [get]",
		"the names on pods":     "pods  []  [kagent-anthropic]  [get]",
		"bind any ClusterRole":  "clusterroles.rbac.authorization.k8s.io  []  []  [bind]",
		"escalate":              "clusterroles.rbac.authorization.k8s.io  []  [agent-platform-tenant-flux]  [escalate]",
		"patch a WorkerPool":    "workerpools.ate.dev  []  []  [patch]",
		"a network policy":      "networkpolicies.networking.k8s.io  []  []  [get]",
	} {
		if got, err := rulesBeyondGrants(joined(tenantsRoleRows, mintRoleRows, []string{row}), grants); err != nil || !reflect.DeepEqual(got, []string{row}) {
			t.Errorf("%s: %v, %v; want %q still granted", name, got, err, row)
		}
	}
}

// TestAgentManagerGrants: the tenants' model key follows the default
// provider's apiKeySecretRef; with networkPolicy enabled the flavor's
// policy kind is allowed, both kinds for auto, and neither is required.
func TestAgentManagerGrants(t *testing.T) {
	keyRow := "secrets  []  [kagent-openai]  [get list watch update delete]"
	held := slices.DeleteFunc(slices.Clone(tenantsRoleRows), func(r string) bool { return strings.Contains(r, "kagent-anthropic") })
	if got, err := rulesBeyondGrants(append(held, keyRow), agentManagerGrants(switchValues(false, true, "kagent-openai"))); err != nil || len(got) != 0 {
		t.Errorf("the default provider's key kagent-openai: %v, %v; want nothing granted", got, err)
	}

	withPolicy := func(flavor string) []saGrant {
		values := switchValues(false, true, "kagent-anthropic")
		values["networkPolicy"] = map[string]any{valuesEnabled: true, "flavor": flavor}
		return agentManagerGrants(values)
	}
	for flavor, want := range map[string][]string{
		"cilium":     {"ciliumnetworkpolicies.cilium.io"},
		"kubernetes": {"networkpolicies.networking.k8s.io"},
		"auto":       {"ciliumnetworkpolicies.cilium.io", "networkpolicies.networking.k8s.io"},
	} {
		var kinds []string
		for _, g := range withPolicy(flavor) {
			if strings.Contains(g.resource, "networkpolicies") {
				if !g.optional {
					t.Errorf("flavor %q: %s is required", flavor, g)
				}
				kinds = append(kinds, g.resource)
			}
		}
		if !reflect.DeepEqual(kinds, want) {
			t.Errorf("flavor %q: %v, want %v", flavor, kinds, want)
		}
	}
	cilium := []string{"ciliumnetworkpolicies.cilium.io  []  []  [get list watch create update]"}
	if got, err := rulesBeyondGrants(joined(tenantsRoleRows, cilium), withPolicy("cilium")); err != nil || len(got) != 0 {
		t.Errorf("the tenants with the cilium policy: %v, %v; want nothing granted", got, err)
	}
	kubernetes := []string{"networkpolicies.networking.k8s.io  []  []  [get]"}
	if got, _ := rulesBeyondGrants(joined(tenantsRoleRows, kubernetes), withPolicy("cilium")); !reflect.DeepEqual(got, kubernetes) {
		t.Errorf("the kubernetes policy kind with flavor cilium: %v, want it granted", got)
	}
	if got := agentManagerGrants(switchValues(false, false, "kagent-anthropic")); got != nil {
		t.Errorf("no switch: %v, want no grants", got)
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
