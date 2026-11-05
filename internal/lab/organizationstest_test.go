package lab

import (
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// Two organization namespaces as the operator names them, and a boundary,
// for the cases below.
const (
	testBoundary = "role bindings"
	orgAlpha     = "agents-alpha-x7k2q"
	orgBeta      = "agents-beta-p4m9z"
)

// Every verdict names its surface and its boundary, and a passing one what
// was seen; the line carries the surface, so the report shows where each
// boundary was judged.
func TestOrganizationVerdictLint(t *testing.T) {
	good := organizationVerdict{surface: surfaceKubernetes, boundary: testBoundary, detail: "403"}
	if err := good.lint(); err != nil {
		t.Errorf("a full verdict: %v", err)
	}
	if line := good.line(); !strings.HasPrefix(line, "PASS ["+surfaceKubernetes+"] "+testBoundary) {
		t.Errorf("line = %q", line)
	}
	skipped := organizationVerdict{surface: surfacePortal, boundary: "the picker", skip: "backstage#3363"}
	if err := skipped.lint(); err != nil {
		t.Errorf("a skip: %v", err)
	}
	if line := skipped.line(); !strings.HasPrefix(line, "SKIP ["+surfacePortal+"] the picker — backstage#3363") {
		t.Errorf("skip line = %q", line)
	}
	for name, v := range map[string]organizationVerdict{
		"no surface":           {boundary: testBoundary, detail: "403"},
		"no boundary":          {surface: surfaceKubernetes, detail: "403"},
		"a pass with no sight": {surface: surfaceKubernetes, boundary: testBoundary},
	} {
		if err := v.lint(); err == nil {
			t.Errorf("%s passed lint", name)
		}
	}
}

// A check fails naming its surface and boundary; a skip runs nothing; a
// check with neither a run nor a skip is refused.
func TestOrganizationCheckVerdict(t *testing.T) {
	failing := organizationCheck{surface: surfaceMuster, boundary: "the listing", run: func() (string, error) { return "", errors.New("listed agentlab-b's server") }}
	if _, err := failing.verdict(); err == nil || !strings.Contains(err.Error(), "FAIL ["+surfaceMuster+"] the listing") {
		t.Errorf("a failing check: %v", err)
	}
	ran := false
	skipped := organizationCheck{surface: surfaceProfile, boundary: "the erasure", skip: organizationErasureSkip, run: func() (string, error) { ran = true; return "x", nil }}
	v, err := skipped.verdict()
	if err != nil || ran || v.skip == "" {
		t.Errorf("a skipped check: %+v, %v, ran=%v", v, err, ran)
	}
	if _, err := (organizationCheck{surface: surfaceLLM, boundary: "the model"}).verdict(); err == nil {
		t.Error("a check without a run or a skip passed")
	}
	passing := organizationCheck{surface: surfaceEgress, boundary: "443", run: func() (string, error) { return "kept out", nil }}
	if v, err := passing.verdict(); err != nil || v.detail != "kept out" {
		t.Errorf("a passing check: %+v, %v", v, err)
	}
}

// The organization's state: Ready only for its current generation with a
// namespace; a deleted one names its erasure step.
func TestOrganizationReady(t *testing.T) {
	org := func(generation int64, status map[string]any, deleting bool) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{}}
		u.SetAPIVersion(organizationAPIVersion)
		u.SetKind(organizationKind)
		u.SetName("agentlab-a")
		u.SetGeneration(generation)
		if status != nil {
			u.Object["status"] = status
		}
		if deleting {
			u.Object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-05T10:00:00Z"
		}
		return u
	}
	conditions := func(gen int64, status string) []any {
		return []any{map[string]any{"type": conditionReady, crStatus: status, fieldReason: "Reconciled", "observedGeneration": gen}}
	}
	ready := func(gen int64, status string) map[string]any {
		return map[string]any{organizationStatusNamespaceKey: orgAlpha, organizationStatusConditionsKey: conditions(gen, status)}
	}
	for name, tc := range map[string]struct {
		obj  *unstructured.Unstructured
		want bool
		why  string
	}{
		"Ready now":                 {org(2, ready(2, conditionTrue), false), true, ""},
		"an older generation":       {org(3, ready(2, conditionTrue), false), false, "generation 2"},
		"not Ready":                 {org(2, ready(2, condFalseStatus), false), false, "Ready=False"},
		"without a status":          {org(1, nil, false), false, "no Ready condition"},
		"being deleted":             {org(2, map[string]any{organizationStatusErasureStepKey: "Sessions"}, true), false, `"Sessions"`},
		"Ready without a namespace": {org(1, map[string]any{organizationStatusConditionsKey: conditions(1, conditionTrue)}, false), false, "without status.namespace"},
	} {
		state, err := organizationStateFrom(tc.obj)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, why := state.ready()
		if got != tc.want || !strings.Contains(why, tc.why) {
			t.Errorf("%s: ready=%v %q, want %v %q", name, got, why, tc.want, tc.why)
		}
		if got && state.namespace != orgAlpha {
			t.Errorf("%s: namespace %q", name, state.namespace)
		}
	}
}

// The default organization and the proof's are AgentPlatformOrganizations
// of the lab's root namespace, on the platform Harness with one worker.
func TestOrganizationManifest(t *testing.T) {
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(organizationManifest(organizationsRootNamespace, defaultOrganization, defaultOrganizationDisplayName)), &obj); err != nil {
		t.Fatal(err)
	}
	u := unstructured.Unstructured{Object: obj}
	if u.GetAPIVersion() != organizationAPIVersion || u.GetKind() != organizationKind || u.GetNamespace() != organizationsRootNamespace || u.GetName() != defaultOrganization {
		t.Errorf("object %s %s %s/%s", u.GetAPIVersion(), u.GetKind(), u.GetNamespace(), u.GetName())
	}
	if got := u.GetAnnotations()[organizationDisplayNameAnnotation]; got != defaultOrganizationDisplayName {
		t.Errorf("display name %q", got)
	}
	harnesses, _, _ := unstructured.NestedStringSlice(obj, "spec", "harnesses")
	replicas, _, _ := unstructured.NestedFieldNoCopy(obj, "spec", "workers", "replicas")
	if !slices.Equal(harnesses, []string{kagentHarness}) || replicas != 1 {
		t.Errorf("spec harnesses %v workers %v", harnesses, replicas)
	}
}

// The organization's model names its key by reference only, the tenant
// credential the chart's policy admits, carries both default marks and no
// baseUrl (the chart's policy fills in the LLM listener).
func TestOrganizationModelManifest(t *testing.T) {
	raw := organizationModelManifest(orgAlpha, "claude-sonnet-4-6")
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatal(err)
	}
	u := unstructured.Unstructured{Object: obj}
	spec, _, _ := unstructured.NestedStringMap(obj, "spec")
	if !strings.HasPrefix(spec["apiKeySecret"], "tenant-credential-") || spec["apiKeySecretKey"] != anthropicSecretKey || spec["provider"] != "Anthropic" {
		t.Errorf("spec %v", spec)
	}
	if strings.Contains(raw, "baseUrl") {
		t.Error("the model sets a baseUrl; the chart fills in the listener")
	}
	for _, a := range []string{defaultModelConfigAnnotation, isDefaultModelConfigAnnotation} {
		if marked, _ := strconv.ParseBool(u.GetAnnotations()[a]); !marked {
			t.Errorf("no default mark %s", a)
		}
	}
}

// A member's agent is the HelmRelease the chart's field lock admits: the
// namespace's own chart source and Helm ServiceAccount, no chart, no
// valuesFrom, and the organization's model.
func TestOrganizationAgentRelease(t *testing.T) {
	u := organizationAgentRelease(orgAlpha, organizationsTestAgent, agentChartOCIRepository, organizationModel)
	ref, _, _ := unstructured.NestedStringMap(u.Object, "spec", "chartRef")
	if ref[kindKey] != kindOCIRepository || ref[nameKey] != agentChartOCIRepository || ref[namespaceKey] != orgAlpha {
		t.Errorf("chartRef %v", ref)
	}
	if sa, _, _ := unstructured.NestedString(u.Object, "spec", "serviceAccountName"); sa != kagentFluxServiceAccount {
		t.Errorf("serviceAccountName %q", sa)
	}
	for _, field := range []string{"chart", "valuesFrom", "postRenderers", "kubeConfig", "targetNamespace", "storageNamespace"} {
		if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", field); found {
			t.Errorf("spec.%s set: the field lock refuses it", field)
		}
	}
	values, _, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "values")
	mc, _ := values.(map[string]any)[modelConfigKey].(map[string]any)
	if mc[nameKey] != organizationModel {
		t.Errorf("values.modelConfig %v", mc)
	}
	if u.GetLabels()[organizationsTestLabel] != organizationsTestMark {
		t.Error("not labelled for the cleanup")
	}
}

// The same answer: equal sets on every surface pass, a surface that sees
// another organization's agent fails naming it; one surface is no
// comparison.
func TestSameAnswer(t *testing.T) {
	members := [2]*config.User{{Email: "alice@lab.local"}, {Email: "bob@lab.local"}}
	own := [2]refSet{{orgAlpha + "/x": true}, {orgBeta + "/x": true}}
	rosters := map[string][2]refSet{surfaceKubernetes: own, surfaceKagent: own, surfaceSlack: own}
	line, err := sameAnswer(rosters, members)
	if err != nil || !strings.Contains(line, "alice@lab.local sees "+orgAlpha+"/x") {
		t.Errorf("equal answers: %q, %v", line, err)
	}
	leaky := own
	leaky[0] = refSet{orgAlpha + "/x": true, orgBeta + "/x": true}
	rosters[surfaceSlack] = leaky
	if _, err := sameAnswer(rosters, members); err == nil || !strings.Contains(err.Error(), surfaceSlack) {
		t.Errorf("a surface showing the other organization's agent: %v", err)
	}
	if _, err := sameAnswer(map[string][2]refSet{surfaceKagent: own}, members); err == nil {
		t.Error("one surface passed as the same answer")
	}
}

// The platform tenant's agents are left out of a roster before it is
// judged; every other namespace stays.
func TestOutsidePlatform(t *testing.T) {
	got := outsidePlatform(refSet{kagentNamespace + "/assistant": true, orgAlpha + "/x": true, orgBeta + "/y": true})
	if want := (refSet{orgAlpha + "/x": true, orgBeta + "/y": true}); rosterDiff(got, want) != "" {
		t.Errorf("outsidePlatform = %s", got)
	}
}

// An in-cluster target passes when the gate refused it for the actor's
// egress policy, or when no request reached the gateway and the probe's own
// line shows it failed; it fails when the gateway forwarded it, refused it
// for another reason, the probe reached it around the gateway, or the probe
// never reported it.
func TestInClusterVerdict(t *testing.T) {
	target := "https://" + kubernetesAPIHost + "/version"
	request := func(status, reason, endpoint string) egressGatewayRecord {
		return egressGatewayRecord{actor: "a", requests: []egressGatewayRequest{{host: kubernetesAPIHost, path: "/version", status: status, reason: reason, endpoint: endpoint}}}
	}
	for name, tc := range map[string]struct {
		record egressGatewayRecord
		line   string
		want   string
	}{
		"refused by the gate":        {request(egressGateStatus, egressPolicyDenied, ""), "rc8 403 Forbidden", "refused by the gate"},
		"held before the gateway":    {egressGatewayRecord{actor: "a"}, "rc4 bad address", "held before the gateway"},
		"forwarded":                  {request("200", "", "10.96.0.1:443"), rcSuccess, ""},
		"refused for another reason": {request("503", "upstream connect error", ""), "rc8", ""},
		"reached around the gateway": {egressGatewayRecord{actor: "a"}, rcSuccess + " ", ""},
		"the probe never ran":        {egressGatewayRecord{actor: "a"}, "", ""},
	} {
		got, err := inClusterVerdict(tc.record, target, tc.line)
		switch {
		case tc.want == "" && err == nil:
			t.Errorf("%s: passed: %q", name, got)
		case tc.want != "" && (err != nil || !strings.Contains(got, tc.want)):
			t.Errorf("%s: %q, %v; want %q", name, got, err, tc.want)
		}
	}
}

// A refusal or a failure counts for the model boundary only when it is
// about the model reference: an unrelated failure is no proof.
func TestNamesModelRef(t *testing.T) {
	for text, want := range map[string]bool{
		`ModelConfig "` + orgBeta + `/anthropic" not found`:                true,
		"admission webhook denied: namespace " + orgBeta + " is not yours": true,
		"Back-off pulling image ghcr.io/kagent/app":                        false,
		`chart "agent" version "2.x" not found`:                            false,
	} {
		if got := namesModelRef(text, orgBeta); got != want {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
}

// The PolicyException lifts every rule of every policy for every
// organization namespace: the widest an Organization's automation could
// write, so the sandbox policies' refusal under it is not a narrower
// exception missing a rule.
func TestPolicyExceptionManifest(t *testing.T) {
	var obj struct {
		Spec struct {
			Exceptions []struct {
				PolicyName string   `yaml:"policyName"`
				RuleNames  []string `yaml:"ruleNames"`
			} `yaml:"exceptions"`
			Match struct {
				Any []struct {
					Resources struct {
						Namespaces []string `yaml:"namespaces"`
					} `yaml:"resources"`
				} `yaml:"any"`
			} `yaml:"match"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(policyExceptionManifest()), &obj); err != nil {
		t.Fatal(err)
	}
	e, m := obj.Spec.Exceptions, obj.Spec.Match.Any
	if len(e) != 1 || e[0].PolicyName != "*" || !slices.Equal(e[0].RuleNames, []string{"*"}) {
		t.Errorf("exceptions %+v, want every rule of every policy", e)
	}
	if len(m) != 1 || !slices.Equal(m[0].Resources.Namespaces, []string{"agents-*"}) {
		t.Errorf("match %+v, want every organization namespace", m)
	}
}

// Only the organization policies' refusal passes; an accepted write, an
// exec that reached the upgrade, and another guard's refusal (Kyverno's
// among them) each fail.
func TestSandboxPolicyRefusal(t *testing.T) {
	for name, msg := range map[string]string{
		"an organization policy": `workerpools.ate.dev "kagent-default" is forbidden: ValidatingAdmissionPolicy 'agent-platform-organization-workerpool-copy' with binding 'agent-platform-organization-workerpool-copy' denied request: only the operator writes a WorkerPool here`,
		"the tenant pair":        `pods "x" is forbidden: ValidatingAdmissionPolicy 'agent-platform-tenant-workers' with binding 'agent-platform-tenant-workers' denied request: no worker label`,
	} {
		if err := sandboxPolicyRefusal(name, errors.New(msg)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, in := range map[string]error{
		"went through":      nil,
		"upgrade":           errors.New("an error on the server (\"Upgrade request required\") has prevented the request from succeeding"),
		"a Kyverno webhook": errors.New(`admission webhook "validate.kyverno.svc-fail" denied the request: policy restrict-workers`),
	} {
		if err := sandboxPolicyRefusal(name, in); err == nil {
			t.Errorf("%s: passed as the organization policies' refusal", name)
		}
	}
}

// The live grants pass when each organization's atespace reads its own
// namespace only; another atespace reading it fails, ate-golden reading it
// does not.
func TestOwnGrantsOnly(t *testing.T) {
	ns := []string{orgAlpha, orgBeta}
	good := map[string][]string{kagentNamespace: {kagentNamespace}, credentialProviderGoldenSpace: {kagentNamespace, orgAlpha, orgBeta}, orgAlpha: {orgAlpha}, orgBeta: {orgBeta}}
	if err := ownGrantsOnly(good, ns); err != nil {
		t.Errorf("own grants: %v", err)
	}
	for name, grants := range map[string]map[string][]string{
		"wider own grant":  {orgAlpha: {orgAlpha, orgBeta}, orgBeta: {orgBeta}},
		"no grant":         {orgAlpha: {orgAlpha}},
		"another atespace": {orgAlpha: {orgAlpha}, orgBeta: {orgBeta}, kagentNamespace: {kagentNamespace, orgAlpha}},
	} {
		if err := ownGrantsOnly(grants, ns); err == nil {
			t.Errorf("%s: %v passed", name, grants)
		}
	}
}

// The provider's JSON log as it reads on the lab: the namespaces read for
// one atespace's actors, other atespaces' and other lines left out.
func TestCredentialReads(t *testing.T) {
	logs := `{"time":"2026-10-15T15:34:44.280295797Z","level":"INFO","msg":"statusz listening","address":":4040"}
{"time":"2026-10-15T15:42:29.846839471Z","level":"INFO","msg":"resolving credential","provider":"k8s.io","namespace":"` + orgAlpha + `","secret":"tenant-credential-anthropic","actor":"spiffe://substrate-actor.local/actor/` + orgAlpha + `/session-01a1403a"}
{"time":"2026-10-15T15:43:05.512155877Z","level":"INFO","msg":"resolving credential","provider":"k8s.io","namespace":"` + orgBeta + `","secret":"tenant-credential-anthropic","actor":"spiffe://substrate-actor.local/actor/` + orgBeta + `/session-01a1403b"}
not json`
	if got := credentialReads(logs, orgAlpha); !slices.Equal(got, []string{orgAlpha}) {
		t.Errorf("%s's reads = %v", orgAlpha, got)
	}
	if got := credentialReads(logs, "agents-gamma-aaaaa"); got != nil {
		t.Errorf("an atespace without reads = %v", got)
	}
}

// The members are the first user of each member group, and the proof
// refuses a lab without one, or one whose member also reaches agents
// elsewhere (a viewer, a platform admin, a developer of the default
// organization).
func TestOrganizationsTestMembers(t *testing.T) {
	cfg := config.Default()
	if _, err := organizationsTestMembers(cfg); err == nil || !strings.Contains(err.Error(), "agentlab configure --organizations") {
		t.Errorf("without members: %v, want the configure hint", err)
	}
	cfg.EnsureTenantUsers()
	users, err := organizationsTestMembers(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if users[0].Email != "alice@lab.local" || users[1].Email != "bob@lab.local" || users[0].Groups[0] != config.GroupTenantA {
		t.Errorf("members %s %v, %s %v", users[0].Email, users[0].Groups, users[1].Email, users[1].Groups)
	}
	for _, group := range []string{viewerGroup, developersGroup, platformAdminsGroup} {
		c := config.Default()
		c.EnsureTenantUsers()
		c.FindUser("bob@lab.local").Groups = []string{config.GroupTenantB, group}
		if _, err := organizationsTestMembers(c); err == nil {
			t.Errorf("a member in %s accepted", group)
		}
	}
}

// The authorizer check passes only on the lab's shape and otherwise names
// the missing piece: the red verdict of a lab without the authorizer.
func TestControllerAuthorizationProblem(t *testing.T) {
	lab := map[string]string{
		authorizationModeKey:           authorizationModeKubernetes,
		authorizationGroupsClaimKey:    tenantsTestGroupsClaim,
		authorizationUsernamePrefixKey: apiserverUserPrefix,
		authorizationGroupsPrefixKey:   apiserverUserPrefix,
	}
	if p := controllerAuthorizationFrom(lab).problem(); p != "" {
		t.Errorf("the lab's shape: %s", p)
	}
	for name, tc := range map[string]struct {
		key, value, want string
	}{
		"no authorizer (kagent before 1.19)": {authorizationModeKey, "", `authorization mode "noop"`},
		"noop":                               {authorizationModeKey, "noop", "kagent.controller.authorization.mode kubernetes"},
		"no groups claim":                    {authorizationGroupsClaimKey, "", "reads the groups from claim"},
		"the fleet's empty group prefix":     {authorizationGroupsPrefixKey, "", "groupsPrefix"},
		"the fleet's empty user prefix":      {authorizationUsernamePrefixKey, "", "usernamePrefix"},
	} {
		data := maps.Clone(lab)
		data[tc.key] = tc.value
		if p := controllerAuthorizationFrom(data).problem(); !strings.Contains(p, tc.want) {
			t.Errorf("%s: %q, want it to name %q", name, p, tc.want)
		}
	}
}

// A Slack roster passes when it lists the member's organization's agent,
// its row naming the tenant as klaus-gateway words it, and nothing of
// another organization; the platform tenant's rows are not judged.
func TestRosterProblem(t *testing.T) {
	ownRef, otherRef := orgAlpha+"/"+organizationsTestAgent, orgBeta+"/"+organizationsTestAgent
	good := map[string]string{
		kagentNamespace + "/assistant": "*assistant* · platform",
		ownRef:                         "*" + organizationsTestAgent + "* · namespace " + orgAlpha,
	}
	if p := rosterProblem(good, orgAlpha); p != "" {
		t.Errorf("a good roster: %s", p)
	}
	if got := slackRoster(good); rosterDiff(got, refSet{ownRef: true}) != "" {
		t.Errorf("slackRoster = %s", got)
	}
	withOther := maps.Clone(good)
	withOther[otherRef] = "*" + organizationsTestAgent + "* · namespace " + orgBeta
	unlabelled := maps.Clone(good)
	unlabelled[ownRef] = "*" + organizationsTestAgent + "*"
	for name, tc := range map[string]struct {
		rows map[string]string
		want string
	}{
		"another organization's agent": {withOther, "another organization's"},
		"no tenant on the row":         {unlabelled, "does not name its tenant"},
		"no row of their own":          {map[string]string{kagentNamespace + "/assistant": "x"}, "no row of " + ownRef},
	} {
		if p := rosterProblem(tc.rows, orgAlpha); !strings.Contains(p, tc.want) {
			t.Errorf("%s: %q, want %q", name, p, tc.want)
		}
	}
	if got := organizationTenant("org-acme"); got != "organization acme" {
		t.Errorf("a KaaS Organization's namespace: %q", got)
	}
}

// The refusal must be the not-permitted note, name the other tenant and say
// nothing was sent: a generic failure note would pass for a refusal
// otherwise.
func TestRefusalProblem(t *testing.T) {
	note := "You cannot use *agentlab-organizations* (namespace " + orgBeta + "): it is not one of the agents you have access to. Nothing was sent to it."
	if p := refusalProblem(note, orgBeta); p != "" {
		t.Errorf("the gateway's note: %s", p)
	}
	for name, text := range map[string]string{
		"a failure note":  "Something went wrong talking to the agent.",
		"another tenant":  strings.ReplaceAll(note, orgBeta, orgAlpha),
		"no nothing-sent": strings.ReplaceAll(note, "Nothing was sent to it.", ""),
	} {
		if refusalProblem(text, orgBeta) == "" {
			t.Errorf("%s accepted as the refusal", name)
		}
	}
}

// The muster check passes on a muster with giantswarm/muster#1603 and the
// setting on, and otherwise names the missing piece.
func TestMusterTenancyProblem(t *testing.T) {
	const tooOld = "needs 5.48.0 or later"
	for name, tc := range map[string]struct {
		tenancy musterTenancy
		want    string
	}{
		"the first with #1603":    {musterTenancy{"gsoci.azurecr.io/giantswarm/muster:5.48.0", true}, ""},
		"a dev image, no version": {musterTenancy{"localhost:5001/muster:dev-1a2b3c", true}, ""},
		"by digest":               {musterTenancy{"gsoci.azurecr.io/giantswarm/muster:5.48.0@sha256:0123", true}, ""},
		"the release before":      {musterTenancy{"gsoci.azurecr.io/giantswarm/muster:5.47.0", true}, tooOld},
		"a candidate of it":       {musterTenancy{"gsoci.azurecr.io/giantswarm/muster:5.48.0-rc.1", true}, tooOld},
		"the setting off":         {musterTenancy{"gsoci.azurecr.io/giantswarm/muster:5.48.0", false}, "without mcpServersInAllNamespaces"},
	} {
		p := tc.tenancy.problem()
		if tc.want == "" && p != "" || tc.want != "" && !strings.Contains(p, tc.want) {
			t.Errorf("%s: %q, want %q", name, p, tc.want)
		}
	}
}

// The values: agentPlatformOrganizations and the LLM listener with the
// organizations on a chart that carries them, muster serving every
// namespace's MCPServers; nothing of it below the chart floor or off; the
// tenants key and a hand-listed credential policy never in an install.
func TestPlatformValuesOrganizations(t *testing.T) {
	render := func(cfg *config.Config) map[string]any {
		out, err := renderTemplate(cfg, platformValuesTemplate, nil)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(out, &values); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return values
	}
	on := config.Default()
	on.Platform.ChartVersion = "4.175.0-rc.9"
	below := config.Default()
	below.Platform.ChartVersion = "4.174.3"
	off := config.Default()
	off.Platform.ChartVersion = "4.175.0-rc.9"
	off.Platform.Organizations.Enabled = false
	musterAll := func(values map[string]any) any {
		chart, _ := values[componentMuster].(map[string]any)[componentMuster].(map[string]any)
		return chart["mcpServersInAllNamespaces"]
	}
	for name, tc := range map[string]struct {
		cfg *config.Config
		on  bool
	}{"on": {on, true}, "below the floor": {below, false}, "switched off": {off, false}} {
		values := render(tc.cfg)
		orgs, _ := values["agentPlatformOrganizations"].(map[string]any)
		routing, _ := values["llmRouting"].(map[string]any)
		if got := orgs["enabled"] == true && routing["enabled"] == true && musterAll(values) == true; got != tc.on {
			t.Errorf("%s: agentPlatformOrganizations %v, llmRouting %v, mcpServersInAllNamespaces %v; want all on: %v", name, values["agentPlatformOrganizations"], values["llmRouting"], musterAll(values), tc.on)
		}
		if !tc.on && (values["agentPlatformOrganizations"] != nil || values["llmRouting"] != nil || musterAll(values) != nil) {
			t.Errorf("%s: a part renders: %v %v %v", name, values["agentPlatformOrganizations"], values["llmRouting"], musterAll(values))
		}
		if values["tenants"] != nil {
			t.Errorf("%s: the retired tenants key renders: %v", name, values["tenants"])
		}
		// The operator's quota: the bare serviceaccounts key, which the API
		// server refuses, dropped (null) and counted under its count/ key.
		quota, _ := nestedValue(values, "agent-platform-operator", "objectQuota").(map[string]any)
		bare, dropped := quota[organizationQuotaResource]
		if got := dropped && bare == nil && quota[organizationQuotaKey] == "100"; got != tc.on {
			t.Errorf("%s: agent-platform-operator.objectQuota = %v, want %s dropped and %s counted: %v", name, quota, organizationQuotaResource, organizationQuotaKey, tc.on)
		}
		if sub, _ := values["substrate"].(map[string]any); sub["credentialProvider"] != nil {
			t.Errorf("%s: the lab lists credential policies by hand: %v", name, sub["credentialProvider"])
		}
	}
}

// agent-manager's --multi-tenant as the chart writes it.
func TestMultiTenantArg(t *testing.T) {
	for args, want := range map[string]bool{
		"--multi-tenant=true":      true,
		"--multi-tenant":           true,
		"--multi-tenant=false":     false,
		"--port=8080":              false,
		"--multi-tenant-x=true":    false,
		"--multi-tenant=true --mt": true,
	} {
		if got := multiTenantArg(strings.Fields(args)); got != want {
			t.Errorf("%q: %v, want %v", args, got, want)
		}
	}
}
