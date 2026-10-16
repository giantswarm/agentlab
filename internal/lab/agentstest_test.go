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

// mintRole is the skills-Secret Role agent-manager's chart renders with
// skills.mintGitAuthSecret (templates/skills-token.yaml).
var mintRole = []authorizationv1.ResourceRule{
	{Verbs: []string{"get", "update"}, APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"tenant-credential-agent-manager-skills-token", "agent-manager-skills-token"}},
}

// tenantsClusterRole is the tenant reconciler's ClusterRole agent-manager's
// chart renders with tenants.enabled (templates/tenants-rbac.yaml), the
// default ModelConfig's key kagent-anthropic, without a network policy.
var tenantsClusterRole = []authorizationv1.ResourceRule{
	{Verbs: []string{"get", "list", "watch"}, APIGroups: []string{""}, Resources: []string{"namespaces"}},
	{Verbs: []string{"list", "watch"}, APIGroups: []string{"api.kagent.dev"}, Resources: []string{"agents"}},
	{Verbs: []string{"list", "watch"}, APIGroups: []string{"helm.toolkit.fluxcd.io"}, Resources: []string{"helmreleases"}},
	{Verbs: []string{"get", "list", "watch", "create", "update", "delete"}, APIGroups: []string{"api.kagent.dev"}, Resources: []string{"harnesses", "modelconfigs"}},
	{Verbs: []string{"get", "list", "watch", "create", "update", "delete"}, APIGroups: []string{"ate.dev"}, Resources: []string{"workerpools"}},
	{Verbs: []string{"get", "list", "watch", "create", "update"}, APIGroups: []string{"source.toolkit.fluxcd.io"}, Resources: []string{"ocirepositories"}},
	{Verbs: []string{"get", "list", "watch", "create", "delete"}, APIGroups: []string{""}, Resources: []string{"serviceaccounts"}},
	{Verbs: []string{"get", "list", "watch", "create", "update", "delete"}, APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"rolebindings"}},
	{Verbs: []string{"bind"}, APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"clusterroles"}, ResourceNames: []string{"agent-platform-tenant-flux"}},
	{Verbs: []string{"get", "list", "watch", "update", "delete"}, APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"kagent-anthropic"}},
	{Verbs: []string{"create"}, APIGroups: []string{""}, Resources: []string{"secrets"}},
}

// switchValues is the lab's rendered values with minting and the tenants
// switched as asked, the default provider's key kagent-anthropic.
func switchValues(mint, tenants bool) map[string]any {
	return map[string]any{
		agentManagerMCPServer: map[string]any{skillsKey: map[string]any{skillsMintValue: mint}},
		"tenants":             map[string]any{"organizations": map[string]any{"enabled": tenants}},
		"kagent": map[string]any{"providers": map[string]any{
			"default":   "anthropic",
			"anthropic": map[string]any{"apiKeySecretRef": "kagent-anthropic"},
		}},
	}
}

// rowsOf words rules as the rules review's rows.
func rowsOf(rules ...[]authorizationv1.ResourceRule) []string {
	var all []authorizationv1.ResourceRule
	for _, r := range rules {
		all = append(all, r...)
	}
	return ruleRows(&authorizationv1.SubjectRulesReviewStatus{ResourceRules: all})
}

// TestRulesBeyondGrants: for every combination of minting and the tenants,
// exactly the RBAC the chart renders for the switches that are on passes;
// a grant of a switch that is off, a grant beyond the chart's on any
// resource, name or verb, and a switch's grant missing in part all fail.
func TestRulesBeyondGrants(t *testing.T) {
	if got := rowsOf(mintRole)[0]; got != "secrets  []  [tenant-credential-agent-manager-skills-token agent-manager-skills-token]  [get update]" {
		t.Fatalf("the Role's row reads %q, not the shape the check parses", got)
	}
	extra := rowsOf([]authorizationv1.ResourceRule{{Verbs: []string{"get", "list"}, APIGroups: []string{"api.kagent.dev"}, Resources: []string{"agenttemplates"}}})

	for _, c := range []struct {
		name          string
		mint, tenants bool
		held          []string
	}{
		{"neither", false, false, nil},
		{"minting", true, false, rowsOf(mintRole)},
		{"tenants", false, true, rowsOf(tenantsClusterRole)},
		{"minting and tenants", true, true, rowsOf(tenantsClusterRole, mintRole)},
	} {
		t.Run(c.name, func(t *testing.T) {
			grants := agentManagerGrants(switchValues(c.mint, c.tenants))
			if got, err := rulesBeyondGrants(c.held, grants); err != nil || len(got) != 0 {
				t.Errorf("exactly the switches' RBAC: %v, %v; want nothing granted", got, err)
			}
			if got, err := rulesBeyondGrants(append(slices.Clone(c.held), extra...), grants); err != nil || !reflect.DeepEqual(got, extra) {
				t.Errorf("with an extra grant: %v, %v; want %v", got, err, extra)
			}
			if !c.mint {
				if got, err := rulesBeyondGrants(append(slices.Clone(c.held), rowsOf(mintRole)...), grants); err != nil || !reflect.DeepEqual(got, rowsOf(mintRole)) {
					t.Errorf("the minting Role with minting off: %v, %v; want it granted", got, err)
				}
			}
			if !c.tenants {
				got, err := rulesBeyondGrants(append(slices.Clone(c.held), rowsOf(tenantsClusterRole)...), grants)
				if err != nil || len(got) != len(tenantsClusterRole)+1 { // harnesses and modelconfigs are one rule, two rows
					t.Errorf("the tenants' ClusterRole with the tenants off: %v, %v; want every row granted", got, err)
				}
			}
			for i := range c.held {
				short := slices.Delete(slices.Clone(c.held), i, i+1)
				if _, err := rulesBeyondGrants(short, grants); err == nil {
					t.Errorf("without %q: no error", c.held[i])
				}
			}
		})
	}

	grants := agentManagerGrants(switchValues(true, true))
	for name, rule := range map[string]authorizationv1.ResourceRule{
		"another minted Secret": {Verbs: []string{"get", "update"}, Resources: []string{"secrets"}, ResourceNames: []string{"agent-manager-skills-token", "another"}},
		"another verb":          {Verbs: []string{"get", "update", "list"}, Resources: []string{"secrets"}, ResourceNames: []string{"agent-manager-skills-token"}},
		"every Secret":          {Verbs: []string{"get"}, Resources: []string{"secrets"}},
		"the model key deleted": {Verbs: []string{"create", "delete"}, Resources: []string{"secrets"}},
		"the names on pods":     {Verbs: []string{"get"}, Resources: []string{"pods"}, ResourceNames: []string{"kagent-anthropic"}},
		"bind any ClusterRole":  {Verbs: []string{"bind"}, APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"clusterroles"}},
		"escalate":              {Verbs: []string{"escalate"}, APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"clusterroles"}, ResourceNames: []string{"agent-platform-tenant-flux"}},
		"patch a WorkerPool":    {Verbs: []string{"patch"}, APIGroups: []string{"ate.dev"}, Resources: []string{"workerpools"}},
		"a network policy":      {Verbs: []string{"get"}, APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"}},
	} {
		wider := rowsOf([]authorizationv1.ResourceRule{rule})
		got, err := rulesBeyondGrants(rowsOf(tenantsClusterRole, mintRole, []authorizationv1.ResourceRule{rule}), grants)
		if err != nil || !reflect.DeepEqual(got, wider) {
			t.Errorf("%s: %v, %v; want %v still granted", name, got, err, wider)
		}
	}
}

// TestAgentManagerGrants: the tenants' model key follows the default
// provider's apiKeySecretRef; with networkPolicy enabled the flavor's
// policy kind is allowed, both kinds for auto, and neither is required.
func TestAgentManagerGrants(t *testing.T) {
	values := switchValues(false, true)
	values["kagent"] = map[string]any{"providers": map[string]any{"default": "openai", "openai": map[string]any{"apiKeySecretRef": "kagent-openai"}}}
	if !slices.ContainsFunc(agentManagerGrants(values), func(g saGrant) bool { return slices.Equal(g.names, []string{"kagent-openai"}) }) {
		t.Errorf("the default provider openai's key kagent-openai is not among %v", agentManagerGrants(values))
	}

	policies := func(flavor string) []string {
		values := switchValues(false, true)
		values["networkPolicy"] = map[string]any{"enabled": true, "flavor": flavor}
		var kinds []string
		for _, g := range agentManagerGrants(values) {
			if strings.Contains(g.resource, "networkpolicies") {
				if !g.optional {
					t.Errorf("flavor %q: %s is required", flavor, g)
				}
				kinds = append(kinds, g.resource)
			}
		}
		return kinds
	}
	for flavor, want := range map[string][]string{
		"cilium":     {"ciliumnetworkpolicies.cilium.io"},
		"kubernetes": {"networkpolicies.networking.k8s.io"},
		"auto":       {"ciliumnetworkpolicies.cilium.io", "networkpolicies.networking.k8s.io"},
	} {
		if got := policies(flavor); !reflect.DeepEqual(got, want) {
			t.Errorf("flavor %q: %v, want %v", flavor, got, want)
		}
	}
	values = switchValues(false, true)
	values["networkPolicy"] = map[string]any{"enabled": true, "flavor": "cilium"}
	cilium := rowsOf([]authorizationv1.ResourceRule{{Verbs: []string{"get", "list", "watch", "create", "update"}, APIGroups: []string{"cilium.io"}, Resources: []string{"ciliumnetworkpolicies"}}})
	if got, err := rulesBeyondGrants(append(rowsOf(tenantsClusterRole), cilium...), agentManagerGrants(values)); err != nil || len(got) != 0 {
		t.Errorf("tenants with the cilium policy: %v, %v; want nothing granted", got, err)
	}
	kubernetes := rowsOf([]authorizationv1.ResourceRule{{Verbs: []string{"get"}, APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"}}})
	if got, _ := rulesBeyondGrants(append(rowsOf(tenantsClusterRole), kubernetes...), agentManagerGrants(values)); !reflect.DeepEqual(got, kubernetes) {
		t.Errorf("the kubernetes policy kind with flavor cilium: %v, want it granted", got)
	}
	if got := agentManagerGrants(switchValues(false, false)); got != nil {
		t.Errorf("neither switch: %v, want no grants", got)
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
