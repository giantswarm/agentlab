package lab

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// The tenants and the reconciler's objects the cases below name.
const (
	orgAlpha       = "org-alpha"
	orgBeta        = "org-beta"
	keyOCI         = kindOCIRepository + "/" + agentChartOCIRepository
	keyHarness     = "Harness/" + kagentHarness
	keyFluxAccount = "ServiceAccount/kagent-flux"
	keyFluxBinding = "RoleBinding/kagent-flux"
)

// What tenants-test expects in a tenant follows agent-manager's flags as the
// chart sets them, with agent-manager's defaults for a flag not given: the
// always-kept objects, the copies while an agent is there, and the network
// policies only when a flavor and names are both set.
func TestTenantReconcilerExpectations(t *testing.T) {
	live := []string{"--multi-tenant=true", "--harness-name=kagent", "--tenant-model-config=default-model-config",
		"--tenant-model-secret=kagent-anthropic", "--tenant-worker-pool-replicas=1", "--helmrelease-service-account=kagent-flux"}
	r, err := tenantReconcilerFrom(live)
	if err != nil {
		t.Fatal(err)
	}
	if !r.multiTenant || r.replicas != 1 || r.copiesNetworkPolicies() {
		t.Errorf("the live lab's flags: multiTenant=%v replicas=%d netpols=%v, want on, 1, none", r.multiTenant, r.replicas, r.copiesNetworkPolicies())
	}
	wantKept := map[string]string{keyOCI: tenantScopeNamespace, keyFluxAccount: tenantScopeNamespace, keyFluxBinding: tenantScopeNamespace}
	if diff := tenantObjectsDiff(r.alwaysKept(), wantKept); diff != "" {
		t.Errorf("always kept: %s", diff)
	}
	wantAgents := map[string]string{
		keyOCI: tenantScopeNamespace, keyFluxAccount: tenantScopeNamespace, keyFluxBinding: tenantScopeNamespace,
		keyHarness: tenantScopeAgents, "WorkerPool/kagent-default": tenantScopeAgents, "ServiceAccount/kagent-default": tenantScopeAgents,
		"ModelConfig/default-model-config": tenantScopeAgents, "Secret/kagent-anthropic": tenantScopeAgents,
	}
	if diff := tenantObjectsDiff(r.withAgents("kagent-default", "kagent-default"), wantAgents); diff != "" {
		t.Errorf("with an agent: %s", diff)
	}

	off, err := tenantReconcilerFrom([]string{"--multi-tenant=false"})
	if err != nil || off.multiTenant {
		t.Errorf("--multi-tenant=false: multiTenant=%v err=%v", off.multiTenant, err)
	}
	if bare, err := tenantReconcilerFrom([]string{"--multi-tenant"}); err != nil || !bare.multiTenant {
		t.Errorf("a bare --multi-tenant: multiTenant=%v err=%v, want on", bare.multiTenant, err)
	}

	cilium, err := tenantReconcilerFrom(append(slices.Clone(live), "--tenant-network-policy-flavor=cilium", "--tenant-network-policies=substrate-workers, other"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cilium.alwaysKept(); got["CiliumNetworkPolicy/substrate-workers"] != tenantScopeNamespace || got["CiliumNetworkPolicy/other"] != tenantScopeNamespace {
		t.Errorf("the cilium flavor's copies: %v", got)
	}
	namesOnly, _ := tenantReconcilerFrom(append(slices.Clone(live), "--tenant-network-policies=substrate-workers-ingress"))
	if namesOnly.copiesNetworkPolicies() {
		t.Errorf("names without a flavor copy nothing in agent-manager, want no network policy expected")
	}
	if _, err := tenantReconcilerFrom([]string{"--tenant-network-policy-flavor=calico"}); err == nil {
		t.Errorf("an unknown flavor was read without an error")
	}
	if _, err := tenantReconcilerFrom([]string{"--tenant-worker-pool-replicas=many"}); err == nil {
		t.Errorf("a replica count that is not a number was read without an error")
	}
}

// The difference names every kind of disagreement, and agreement is "".
func TestTenantObjectsDiff(t *testing.T) {
	want := map[string]string{keyOCI: tenantScopeNamespace, keyHarness: tenantScopeAgents}
	for _, tc := range []struct {
		name string
		got  map[string]string
		want string
	}{
		{"agree", map[string]string{keyOCI: tenantScopeNamespace, keyHarness: tenantScopeAgents}, ""},
		{"lacking", map[string]string{keyOCI: tenantScopeNamespace}, "missing: " + keyHarness},
		{"extra", map[string]string{keyOCI: tenantScopeNamespace, keyHarness: tenantScopeAgents, "Secret/x": tenantScopeAgents}, "not wanted: Secret/x (agents)"},
		{"scope", map[string]string{keyOCI: tenantScopeAgents, keyHarness: tenantScopeAgents}, "wrong scope: " + keyOCI},
	} {
		got := tenantObjectsDiff(tc.got, want)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: diff %q, want it to say %q", tc.name, got, tc.want)
		}
	}
}

// The policy file as the Substrate chart renders it (`policies: <json>`):
// the live lab's hand-listed policy covers its own namespaces only, and an
// ownNamespaceSelector entry covers any labelled namespace.
func TestCredentialPolicyCoverage(t *testing.T) {
	file := `policies: [{"allowedNamespaces":["kagent"],"atespace":"kagent"},{"allowedNamespaces":["kagent","org-alpha","org-beta"],"atespace":"ate-golden"},{"allowedNamespaces":["org-alpha"],"atespace":"org-alpha"},{"allowedNamespaces":["org-beta"],"atespace":"org-beta"}]` + "\n"
	policies, err := parseCredentialPolicies(file)
	if err != nil {
		t.Fatal(err)
	}
	if selectsOwnNamespaces(policies) {
		t.Errorf("a static policy read as selecting namespaces")
	}
	if got := uncoveredNamespaces(policies, []string{orgAlpha, orgBeta}); len(got) != 0 {
		t.Errorf("the listed namespaces: uncovered %v", got)
	}
	if got := uncoveredNamespaces(policies, tenantsTestNamespaces[:]); !slices.Equal(got, tenantsTestNamespaces[:]) {
		t.Errorf("tenants-test's namespaces under another lab's list: uncovered %v, want both", got)
	}
	// An own entry without ate-golden's: the golden actor could not fetch
	// the template's credentials.
	ownOnly := []credentialPolicy{{Atespace: orgAlpha, AllowedNamespaces: []string{orgAlpha}}}
	if got := uncoveredNamespaces(ownOnly, []string{orgAlpha}); !slices.Equal(got, []string{orgAlpha}) {
		t.Errorf("without ate-golden: uncovered %v, want org-alpha", got)
	}

	selected, err := parseCredentialPolicies(`policies: [{"atespace":"kagent","allowedNamespaces":["kagent"]},{"ownNamespaceSelector":{"matchExpressions":[{"key":"giantswarm.io/organization","operator":"Exists"}]}}]`)
	if err != nil {
		t.Fatal(err)
	}
	if !selectsOwnNamespaces(selected) {
		t.Errorf("an ownNamespaceSelector policy not recognised")
	}
}

// The values the lab renders under platform.tenants pass the proof's own
// check of the credential policy, and off the switch renders nothing.
func TestPlatformValuesTenants(t *testing.T) {
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
	off := render(config.Default())
	if off["tenants"] != nil {
		t.Errorf("off by default: tenants=%v", off["tenants"])
	}
	if sub, _ := off["substrate"].(map[string]any); sub["credentialProvider"] != nil {
		t.Errorf("off by default the chart's own credential policies stay: %v", sub["credentialProvider"])
	}

	cfg := config.Default()
	cfg.Platform.Tenants.Enabled = true
	on := render(cfg)
	if orgs, _ := on["tenants"].(map[string]any)["organizations"].(map[string]any); orgs["enabled"] != true {
		t.Errorf("tenants.organizations.enabled = %v, want true", on["tenants"])
	}
	// The chart refuses tenants without the LLM listener (agent-platform 4.151).
	if r, _ := on["llmRouting"].(map[string]any); r["enabled"] != true {
		t.Errorf("llmRouting = %v, want enabled with tenants", on["llmRouting"])
	}
	if off["llmRouting"] != nil {
		t.Errorf("off by default: llmRouting=%v", off["llmRouting"])
	}
	cp, _ := on["substrate"].(map[string]any)["credentialProvider"].(map[string]any)
	raw, err := yaml.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		NamespacePolicies []struct {
			Atespace          string   `yaml:"atespace"`
			AllowedNamespaces []string `yaml:"allowedNamespaces"`
		} `yaml:"namespacePolicies"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	var policies []credentialPolicy
	for _, p := range file.NamespacePolicies {
		policies = append(policies, credentialPolicy{Atespace: p.Atespace, AllowedNamespaces: p.AllowedNamespaces})
	}
	a, b := "org-agentlab-tenants-a", "org-agentlab-tenants-b"
	want := []credentialPolicy{
		{Atespace: kagentNamespace, AllowedNamespaces: []string{kagentNamespace}},
		{Atespace: credentialProviderGoldenSpace, AllowedNamespaces: []string{kagentNamespace, a, b}},
		{Atespace: a, AllowedNamespaces: []string{a}},
		{Atespace: b, AllowedNamespaces: []string{b}},
	}
	if !slices.EqualFunc(policies, want, func(a, b credentialPolicy) bool {
		return a.Atespace == b.Atespace && slices.Equal(a.AllowedNamespaces, b.AllowedNamespaces)
	}) {
		t.Errorf("substrate.credentialProvider.namespacePolicies = %+v, want %+v", policies, want)
	}
	if got := uncoveredNamespaces(policies, tenantsTestNamespaces[:]); len(got) != 0 {
		t.Errorf("the rendered policy leaves %v uncovered for tenants-test", got)
	}

	cfg.Platform.Agents = false
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "platform.tenants requires platform.agents") {
		t.Errorf("tenants without agents: err = %v, want the agents requirement", err)
	}
}

// The live grants pass when each tenant's atespace reads its own namespace
// only; another atespace reading it fails, ate-golden reading it does not.
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
{"time":"2026-10-15T15:42:29.846839471Z","level":"INFO","msg":"resolving credential","provider":"k8s.io","namespace":"org-alpha","secret":"kagent-anthropic","actor":"spiffe://substrate-actor.local/actor/org-alpha/session-01a1403a"}
{"time":"2026-10-15T15:43:05.512155877Z","level":"INFO","msg":"resolving credential","provider":"k8s.io","namespace":"org-beta","secret":"kagent-anthropic","actor":"spiffe://substrate-actor.local/actor/org-beta/session-01a1403b"}
not json`
	if got := credentialReads(logs, orgAlpha); !slices.Equal(got, []string{orgAlpha}) {
		t.Errorf("org-alpha's reads = %v", got)
	}
	if got := credentialReads(logs, "org-gamma"); got != nil {
		t.Errorf("an atespace without reads = %v", got)
	}
}

// Only the tenant policies' refusal passes; an accepted write, an exec that
// reached the upgrade, and another guard's refusal each fail.
func TestTenantAdmissionRefusal(t *testing.T) {
	vap := errors.New(`workerpools.ate.dev "kagent-default" is forbidden: ValidatingAdmissionPolicy 'agent-platform-tenant-workerpool-copy' with binding 'agent-platform-tenant-workerpool-copy' denied request: only agent-manager writes a WorkerPool here`)
	if err := tenantAdmissionRefusal("patch", vap); err != nil {
		t.Errorf("the policy's refusal: %v", err)
	}
	for name, in := range map[string]error{
		"went through": nil,
		"upgrade":      errors.New("an error on the server (\"Upgrade request required\") has prevented the request from succeeding"),
		"other":        errors.New(`pods "x" is forbidden: violates PodSecurity "restricted:latest"`),
	} {
		if err := tenantAdmissionRefusal(name, in); err == nil {
			t.Errorf("%s: passed as the tenant policies' refusal", name)
		}
	}
}

// The default editors are the first two users outside platform-admins; a
// platform admin or one person twice is refused.
func TestTenantsTestPeople(t *testing.T) {
	cfg := config.Default()
	admin, dev, viewer := cfg.Users[0].Email, cfg.Users[1].Email, cfg.Users[2].Email
	people, err := tenantsTestPeople(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if people[0].Email != dev || people[1].Email != viewer {
		t.Errorf("default people %s, %s; want %s and %s", people[0].Email, people[1].Email, dev, viewer)
	}
	for name, emails := range map[string][]string{
		"a platform admin": {admin, dev},
		"twice":            {dev, dev},
		"not a user":       {dev, "nobody@lab.local"},
		"a single email":   {dev},
		"a tenant user":    {"alice@lab.local", dev},
	} {
		if _, err := tenantsTestPeople(cfg, emails); err == nil {
			t.Errorf("%s: %v accepted", name, emails)
		}
	}
}

// The editors default to users outside platform-admins and the tenant groups,
// whatever their order: a tenant user is bound by group, not as an editor.
func TestTenantsTestPeopleSkipsTenantUsers(t *testing.T) {
	cfg := config.Default()
	cfg.EnsureTenantUsers()
	cfg.Users = append([]config.User{cfg.Users[0], cfg.Users[3], cfg.Users[4]}, cfg.Users[1:3]...)
	people, err := tenantsTestPeople(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dev, viewer := config.Default().Users[1].Email, config.Default().Users[2].Email; people[0].Email != dev || people[1].Email != viewer {
		t.Errorf("default editors %s, %s; want %s and %s", people[0].Email, people[1].Email, dev, viewer)
	}
}

// The tenant users are the first user of each tenant group, and the proof
// refuses a lab without one, or one whose tenant user also reads every
// namespace (a viewer), since that user would list every tenant's agents.
func TestTenantsTestUsers(t *testing.T) {
	cfg := config.Default()
	if _, err := tenantsTestUsers(cfg); err == nil || !strings.Contains(err.Error(), "agentlab configure --tenants") {
		t.Errorf("without tenant users: %v, want the configure hint", err)
	}
	cfg.EnsureTenantUsers()
	users, err := tenantsTestUsers(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if users[0].Email != "alice@lab.local" || users[1].Email != "bob@lab.local" {
		t.Errorf("tenant users %s, %s; want alice@lab.local and bob@lab.local", users[0].Email, users[1].Email)
	}
	cfg.FindUser("bob@lab.local").Groups = []string{config.GroupTenantB, viewerGroup}
	if _, err := tenantsTestUsers(cfg); err == nil {
		t.Error("a tenant user in viewers accepted")
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

// A Slack roster passes when it lists the person's organization's agent
// and the platform tenant's, each row naming its tenant, and nothing of
// another namespace.
func TestRosterProblem(t *testing.T) {
	own, other := tenantsTestNamespaces[0], tenantsTestNamespaces[1]
	ownRef, otherRef := own+"/"+tenantsTestAgent, other+"/"+tenantsTestAgent
	good := map[string]string{
		tenantsTestPlatformAgent: "*" + tenantsTestPlatformAgent + "* · platform · default",
		ownRef:                   "*" + tenantsTestAgent + "* · organization agentlab-tenants-a",
	}
	if p := rosterProblem(good, own, tenantsTestPlatformAgent); p != "" {
		t.Errorf("a good roster: %s", p)
	}
	withOther := maps.Clone(good)
	withOther[otherRef] = "*" + tenantsTestAgent + "* · organization agentlab-tenants-b"
	unlabelled := maps.Clone(good)
	unlabelled[ownRef] = "*" + tenantsTestAgent + "*"
	// A gateway before giantswarm/klaus-gateway#486 lists the served
	// namespace only, by bare name, with no tenant.
	before486 := map[string]string{tenantsTestPlatformAgent: "*" + tenantsTestPlatformAgent + "*"}
	for name, tc := range map[string]struct {
		rows map[string]string
		want string
	}{
		"another tenant's agent":   {withOther, "another tenant's"},
		"no tenant on the row":     {unlabelled, "does not name its tenant"},
		"a gateway before #486":    {before486, "no row of " + ownRef},
		"no platform tenant's row": {map[string]string{ownRef: good[ownRef]}, "no row of the platform tenant's"},
	} {
		if p := rosterProblem(tc.rows, own, tenantsTestPlatformAgent); !strings.Contains(p, tc.want) {
			t.Errorf("%s: %q, want %q", name, p, tc.want)
		}
	}
}

// The refusal must be the not-permitted note, name the other tenant and say
// nothing was sent: a generic failure note would pass for a refusal
// otherwise.
func TestRefusalProblem(t *testing.T) {
	other := tenantsTestNamespaces[1]
	note := "You cannot use *agentlab-tenants-test* (organization agentlab-tenants-b): it is not one of the agents you have access to. Nothing was sent to it. Ask an admin of the organization agentlab-tenants-b for access."
	if p := refusalProblem(note, other); p != "" {
		t.Errorf("the gateway's note: %s", p)
	}
	for name, text := range map[string]string{
		"a failure note":  "Something went wrong talking to the agent.",
		"another tenant":  strings.ReplaceAll(note, "tenants-b", "tenants-a"),
		"no nothing-sent": strings.ReplaceAll(note, "Nothing was sent to it.", ""),
	} {
		if refusalProblem(text, other) == "" {
			t.Errorf("%s accepted as the refusal", name)
		}
	}
}
