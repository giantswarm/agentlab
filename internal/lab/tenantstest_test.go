package lab

import (
	"errors"
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
	} {
		if _, err := tenantsTestPeople(cfg, emails); err == nil {
			t.Errorf("%s: %v accepted", name, emails)
		}
	}
}
