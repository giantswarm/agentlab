package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"

	"github.com/giantswarm/agentlab/internal/config"
)

// tenantsTestNamespaces are the two organization namespaces tenants-test
// creates, labels and removes again. The values template names them too:
// until the chart selects the organizations' namespaces by label
// (giantswarm/agent-platform#1041), the credential provider reads a
// namespace only where the lab lists it.
var tenantsTestNamespaces = [2]string{"org-agentlab-tenants-a", "org-agentlab-tenants-b"}

const (
	// tenantsTestAgent is the agent tenants-test creates in each
	// organization namespace, one person's in each.
	tenantsTestAgent = "agentlab-tenants-test"
	// organizationLabel marks a namespace as a tenant (agent-manager's
	// tenants.OrganizationLabel); its value is the organization's name.
	organizationLabel = "giantswarm.io/organization"
	// tenantObjectLabel marks what the reconciler writes, its value when
	// the object is kept: tenantScopeNamespace always,
	// tenantScopeAgents while the namespace holds an agent.
	tenantObjectLabel    = "agent-manager.giantswarm.io/tenant-object"
	tenantScopeNamespace = "namespace"
	tenantScopeAgents    = "agents"
	// tenantEditorRole is the chart's ClusterRole a person is bound to in
	// an organization namespace to write agents there.
	tenantEditorRole = "agent-platform:editor"
	// tenantAdmissionPolicyPrefix names the chart's ValidatingAdmissionPolicies
	// that hold an organization namespace's workers
	// (agent-platform-tenant-workers, agent-platform-tenant-workerpool-copy).
	tenantAdmissionPolicyPrefix = "agent-platform-tenant-"
	// workerPoolLabel marks a worker pod (and its Deployment and ReplicaSet)
	// with the WorkerPool it belongs to.
	workerPoolLabel = "ate.dev/worker-pool"
	// apiserverUserPrefix is the apiserver's --oidc-username-prefix: an
	// RBAC subject names a lab person as oidc:<email>.
	apiserverUserPrefix = "oidc:"
	// agentManagerHTTPPort serves agent-manager's REST API (/api/v1) and MCP.
	agentManagerHTTPPort = 8080
	// The Substrate credential provider: its Deployment, the ConfigMap of
	// its namespace policy and the port of its /statusz page.
	credentialProvider              = "k8s-credential-provider" // #nosec G101 -- a Deployment name, not a credential
	credentialProviderPolicy        = credentialProvider + "-namespace-policy"
	credentialProviderPolicyKey     = "namespace-policy.yaml"
	credentialProviderStatusPort    = 4040
	credentialProviderGoldenSpace   = "ate-golden"
	credentialProviderResolvingLine = "resolving credential"
	// tenantsTestPrompt is the turn each tenant's agent answers: any reply
	// proves the model call, which needs the key read from the namespace.
	tenantsTestPrompt = "Reply with the single word: ready"
	// tenantsTestCopiesTimeout bounds the reconciler's work after a create
	// or a delete; tenantsTestWorkerTimeout the worker's start.
	tenantsTestCopiesTimeout = 2 * time.Minute
	tenantsTestWorkerTimeout = 5 * time.Minute
	// tenantsTestNamespaceTimeout bounds a namespace's removal at cleanup.
	tenantsTestNamespaceTimeout = 3 * time.Minute
)

// tenantReconciler is agent-manager's tenant reconciler as its Deployment
// configures it (the flags of `agent-manager serve`).
type tenantReconciler struct {
	multiTenant bool
	harness     string
	modelConfig string
	modelSecret string
	replicas    int64
	// fluxServiceAccount is the ServiceAccount the agents' HelmReleases run
	// as, kept in every tenant with a RoleBinding of the same name.
	fluxServiceAccount string
	// networkPolicyKind and networkPolicies are the network policies copied
	// into every tenant; none while the lab's network policies are off.
	networkPolicyKind string
	networkPolicies   []string
}

// tenantReconcilerFrom reads the reconciler's settings off agent-manager's
// arguments, with agent-manager's defaults for a flag not given.
func tenantReconcilerFrom(args []string) (tenantReconciler, error) {
	r := tenantReconciler{harness: kagentHarness, modelConfig: defaultModelConfig, modelSecret: anthropicSecret, replicas: 1}
	// The chart writes every flag as --name=value; a bare boolean flag is
	// true. The AGENT_MANAGER_* environment variables are not read: the
	// chart sets none of the reconciler's.
	for _, arg := range args {
		name, value, ok := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if !ok {
			if name == "multi-tenant" {
				r.multiTenant = true
			}
			continue
		}
		switch name {
		case "multi-tenant":
			r.multiTenant, _ = strconv.ParseBool(value)
		case "harness-name":
			r.harness = value
		case "tenant-model-config":
			r.modelConfig = value
		case "tenant-model-secret":
			r.modelSecret = value
		case "tenant-worker-pool-replicas":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return r, fmt.Errorf("--tenant-worker-pool-replicas=%q: %w", value, err)
			}
			r.replicas = n
		case "helmrelease-service-account":
			r.fluxServiceAccount = value
		case "tenant-network-policy-flavor":
			switch value {
			case "":
			case "kubernetes":
				r.networkPolicyKind = "NetworkPolicy"
			case "cilium":
				r.networkPolicyKind = "CiliumNetworkPolicy"
			default:
				return r, fmt.Errorf("--tenant-network-policy-flavor=%q: kubernetes, cilium or empty", value)
			}
		case "tenant-network-policies":
			for _, n := range strings.Split(value, ",") {
				if n = strings.TrimSpace(n); n != "" {
					r.networkPolicies = append(r.networkPolicies, n)
				}
			}
		}
	}
	return r, nil
}

// copiesNetworkPolicies reports whether the reconciler keeps network
// policies in a tenant: a flavor and at least one name.
func (r tenantReconciler) copiesNetworkPolicies() bool {
	return r.networkPolicyKind != "" && len(r.networkPolicies) > 0
}

// alwaysKept is what the reconciler keeps in every tenant, as kind/name ->
// scope: the agent chart's OCIRepository, the agents' Flux ServiceAccount
// and its RoleBinding, the network policies.
func (r tenantReconciler) alwaysKept() map[string]string {
	kept := map[string]string{kindOCIRepository + "/" + agentChartOCIRepository: tenantScopeNamespace}
	if r.fluxServiceAccount != "" {
		kept["ServiceAccount/"+r.fluxServiceAccount] = tenantScopeNamespace
		kept["RoleBinding/"+r.fluxServiceAccount] = tenantScopeNamespace
	}
	if r.copiesNetworkPolicies() {
		for _, name := range r.networkPolicies {
			kept[r.networkPolicyKind+"/"+name] = tenantScopeNamespace
		}
	}
	return kept
}

// withAgents is alwaysKept and the copies a tenant holds while it has an
// agent: the platform Harness, the WorkerPool it runs on and the pool's
// ServiceAccount, the default ModelConfig and its key Secret.
func (r tenantReconciler) withAgents(pool, poolServiceAccount string) map[string]string {
	want := r.alwaysKept()
	for _, key := range []string{
		"Harness/" + r.harness,
		"WorkerPool/" + pool,
		"ServiceAccount/" + poolServiceAccount,
		"ModelConfig/" + r.modelConfig,
		kindSecret + "/" + r.modelSecret,
	} {
		want[key] = tenantScopeAgents
	}
	return want
}

// tenantObjectsDiff words how the reconciler's objects in a namespace differ
// from the wanted ones, "" when they agree.
func tenantObjectsDiff(got, want map[string]string) string {
	var missing, extra, scoped []string
	for key, scope := range want {
		switch have, ok := got[key]; {
		case !ok:
			missing = append(missing, key)
		case have != scope:
			scoped = append(scoped, fmt.Sprintf("%s (%s=%s, want %s)", key, tenantObjectLabel, have, scope))
		}
	}
	for key, scope := range got {
		if _, ok := want[key]; !ok {
			extra = append(extra, fmt.Sprintf("%s (%s)", key, scope))
		}
	}
	var parts []string
	for _, p := range []struct {
		what string
		list []string
	}{{"missing", missing}, {"not wanted", extra}, {"wrong scope", scoped}} {
		if len(p.list) > 0 {
			slices.Sort(p.list)
			parts = append(parts, p.what+": "+strings.Join(p.list, ", "))
		}
	}
	return strings.Join(parts, "; ")
}

// tenantObjectKeys lists a kind/name -> scope map, sorted, for a note.
func tenantObjectKeys(objects map[string]string) string {
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return strings.Join(keys, ", ")
}

// credentialPolicy is one entry of the Substrate credential provider's
// namespace policy file.
type credentialPolicy struct {
	Atespace             string         `yaml:"atespace,omitempty"`
	AllowedNamespaces    []string       `yaml:"allowedNamespaces,omitempty"`
	OwnNamespaceSelector map[string]any `yaml:"ownNamespaceSelector,omitempty"`
}

// parseCredentialPolicies reads the policy file the provider loads.
func parseCredentialPolicies(file string) ([]credentialPolicy, error) {
	var doc struct {
		Policies []credentialPolicy `yaml:"policies"`
	}
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil {
		return nil, fmt.Errorf("the credential provider's namespace policy: %w", err)
	}
	return doc.Policies, nil
}

// selectsOwnNamespaces reports whether a policy grants each namespace a
// selector matches to the atespace of its name (Substrate 1.12): the
// organizations' namespaces need no entry of their own then.
func selectsOwnNamespaces(policies []credentialPolicy) bool {
	return slices.ContainsFunc(policies, func(p credentialPolicy) bool { return len(p.OwnNamespaceSelector) > 0 })
}

// uncoveredNamespaces are the namespaces a static policy does not let an
// agent of theirs boot and answer in: an entry whose atespace is the
// namespace and reads it, and ate-golden reading it (the golden actor fetches
// the template's credentials there).
func uncoveredNamespaces(policies []credentialPolicy, namespaces []string) []string {
	var uncovered []string
	for _, ns := range namespaces {
		own := slices.ContainsFunc(policies, func(p credentialPolicy) bool {
			return p.Atespace == ns && slices.Contains(p.AllowedNamespaces, ns)
		})
		golden := slices.ContainsFunc(policies, func(p credentialPolicy) bool {
			return p.Atespace == credentialProviderGoldenSpace && slices.Contains(p.AllowedNamespaces, ns)
		})
		if !own || !golden {
			uncovered = append(uncovered, ns)
		}
	}
	return uncovered
}

// ownGrantsOnly checks the provider's live grants (/statusz policy_grants,
// atespace -> namespaces): each tenant's atespace reads its own namespace
// only, and no other atespace but ate-golden reads a tenant's namespace.
func ownGrantsOnly(grants map[string][]string, namespaces []string) error {
	for _, ns := range namespaces {
		if got := grants[ns]; !slices.Equal(got, []string{ns}) {
			return fmt.Errorf("the credential provider grants atespace %s %v, want its own namespace only", ns, got)
		}
		for atespace, allowed := range grants {
			if atespace != ns && atespace != credentialProviderGoldenSpace && slices.Contains(allowed, ns) {
				return fmt.Errorf("the credential provider lets atespace %s read %s", atespace, ns)
			}
		}
	}
	return nil
}

// credentialReads are the namespaces the provider read a credential from for
// actors of the atespace, in its JSON log: its "resolving credential" lines,
// whose actor is spiffe://<domain>/actor/<atespace>/<actor>.
func credentialReads(logs, atespace string) []string {
	var read []string
	for _, line := range strings.Split(logs, "\n") {
		var entry struct {
			Msg       string `json:"msg"`
			Namespace string `json:"namespace"`
			Actor     string `json:"actor"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Msg != credentialProviderResolvingLine {
			continue
		}
		if strings.Contains(entry.Actor, "/actor/"+atespace+"/") {
			read = append(read, entry.Namespace)
		}
	}
	return read
}

// tenantAdmissionRefusal reads a write's answer as the tenant admission
// policies' refusal: nil when one of them refused it, an error naming what
// happened otherwise — the write went through, or something else refused it.
func tenantAdmissionRefusal(what string, err error) error {
	switch {
	case err == nil:
		return fmt.Errorf("%s was accepted; the chart's %s* admission policies refuse it in an organization namespace", what, tenantAdmissionPolicyPrefix)
	case strings.Contains(err.Error(), "ValidatingAdmissionPolicy '"+tenantAdmissionPolicyPrefix):
		return nil
	case strings.Contains(err.Error(), "Upgrade request required"):
		return fmt.Errorf("%s passed admission (the apiserver only asked for the stream upgrade); the chart's %s* admission policies refuse it in an organization namespace", what, tenantAdmissionPolicyPrefix)
	default:
		return fmt.Errorf("%s was refused, but not by the chart's %s* admission policies: %w", what, tenantAdmissionPolicyPrefix, err)
	}
}

// tenantsTestPeople are the two editors, one per namespace: the emails
// given, else the first two users outside platform-admins. A platform admin
// is cluster-admin, so the other organization's refusal would prove nothing.
func tenantsTestPeople(cfg *config.Config, emails []string) ([2]*config.User, error) {
	var people [2]*config.User
	if len(emails) == 0 {
		n := 0
		for i := range cfg.Users {
			if n < len(people) && !slices.Contains(cfg.Users[i].Groups, "platform-admins") {
				people[n] = &cfg.Users[i]
				n++
			}
		}
		if n < len(people) {
			return people, fmt.Errorf("tenants-test needs two users outside platform-admins in %s (one editor per organization), found %d", config.File, n)
		}
		return people, nil
	}
	if len(emails) != len(people) {
		return people, fmt.Errorf("tenants-test takes no email or two (one editor per organization), got %d", len(emails))
	}
	if emails[0] == emails[1] {
		return people, fmt.Errorf("tenants-test needs two different people, got %s twice", emails[0])
	}
	for i, email := range emails {
		user := cfg.FindUser(email)
		if user == nil {
			return people, fmt.Errorf("no user %q in %s", email, config.File)
		}
		if slices.Contains(user.Groups, "platform-admins") {
			return people, fmt.Errorf("%s is in platform-admins (cluster-admin): the other organization's refusal would prove nothing — name two users outside it", email)
		}
		people[i] = user
	}
	return people, nil
}

// TenantsTest is the headless proof of agent-manager's tenant reconciler
// (giantswarm/agent-manager#71) and the chart's tenants.organizations.enabled
// (platform.tenants): two namespaces labelled giantswarm.io/organization,
// one person bound to agent-platform:editor in each, agent-manager's REST
// API called as each person with their Dex token, and the steps of
// giantswarm/agentlab#739 — only the always-kept objects before an agent;
// the other organization's create refused and the first agent's copies; the
// worker admitted and a turn on credentials read from the agent's own
// namespace; the same in the second organization; the workers held against
// a cluster-admin, the network policy copies and the credential provider's
// grants where the lab has them; the last delete removing the copies.
// Leaves nothing behind.
func TenantsTest(cfg *config.Config, emails []string) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled || !cfg.Platform.Agents {
		return fmt.Errorf("platform.agents is off in %s — enable it, `agentlab configure --tenants`, then `agentlab platform`", config.File)
	}
	people, err := tenantsTestPeople(cfg, emails)
	if err != nil {
		return err
	}
	admin := cfg.AdminUser()
	if admin == nil {
		return fmt.Errorf("no platform-admins user in %s", config.File)
	}
	if err := checkApiserverOIDC(cfg); err != nil {
		return err
	}
	namespaces := tenantsTestNamespaces[:]

	step("agent-manager's tenant reconciler (--multi-tenant on deploy/%s)", agentManagerMCPServer)
	rec, err := readTenantReconciler()
	if err != nil {
		return err
	}
	switch {
	case !rec.multiTenant && !cfg.TenantsEnabled():
		return fmt.Errorf("agent-manager runs without --multi-tenant=true: the tenant reconciler is off — `agentlab configure --tenants` (the chart's tenants.organizations.enabled), then `agentlab platform`")
	case !rec.multiTenant:
		return fmt.Errorf("platform.tenants is on in %s, but agent-manager runs without --multi-tenant=true: the installed chart does not turn the tenant reconciler on with tenants.organizations.enabled — it needs giantswarm/agent-platform#1037 and #1050 (a dev build until a release carries them: `agentlab configure --chart-branch <branch>`, then `agentlab platform`)", config.File)
	}
	pool, poolServiceAccount, err := platformWorkerPool(rec.harness)
	if err != nil {
		return err
	}
	netpols := "none (network policies off)"
	if rec.copiesNetworkPolicies() {
		netpols = rec.networkPolicyKind + " " + strings.Join(rec.networkPolicies, ", ")
	}
	note("Harness %s on WorkerPool %s (ServiceAccount %s), ModelConfig %s with Secret %s, %d worker(s) per tenant, Flux ServiceAccount %q, network policies %s",
		rec.harness, pool, poolServiceAccount, rec.modelConfig, rec.modelSecret, rec.replicas, rec.fluxServiceAccount, netpols)

	step("The credential provider's namespace policy reads %s", strings.Join(namespaces, " and "))
	policies, err := readCredentialPolicies()
	if err != nil {
		return err
	}
	selector := selectsOwnNamespaces(policies)
	if selector {
		note("an ownNamespaceSelector policy: each organization namespace is read by its own atespace")
	} else if missing := uncoveredNamespaces(policies, namespaces); len(missing) > 0 {
		return fmt.Errorf("the credential provider's namespace policy (ConfigMap %s/%s) does not let %s read its own Secrets, so a turn there cannot reach the model: `agentlab configure --tenants`, then `agentlab platform`, list them until the chart selects the organizations' namespaces by label (giantswarm/agent-platform#1041). A platform.valuesFiles overlay that sets substrate.credentialProvider.namespacePolicies replaces the lab's list",
			substrateNamespace, credentialProviderPolicy, strings.Join(missing, " and "))
	} else {
		note("listed by hand (no ownNamespaceSelector policy yet, giantswarm/agent-platform#1041)")
	}

	// Leftovers of an aborted run first, and everything this run creates on
	// every exit path.
	cleanup := func() {
		for _, ns := range namespaces {
			if err := removeTenantsTestNamespace(ns); err != nil {
				note("cleanup: %v", err)
			}
		}
	}
	cleanup()
	defer cleanup()

	step("The chart's ClusterRole %s, which the two people are bound to", tenantEditorRole)
	if err := checkTenantEditorRole(); err != nil {
		return err
	}

	var tokens [2]string
	for i, person := range people {
		step("Logging in to Dex as %s (audiences %s and %s)", person.Email, config.KubernetesClientID, config.AgentPlatformClientID)
		if tokens[i], err = passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, person.Email, person.Password, musterLoginScopes); err != nil {
			return err
		}
	}

	for i, ns := range namespaces {
		step("Namespace %s labelled %s=%s; %s bound to %s there", ns, organizationLabel, strings.TrimPrefix(ns, "org-"), people[i].Email, tenantEditorRole)
		if err := createTenantsTestNamespace(ns, people[i].Email); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api, stopForward, err := dialAgentManagerREST(ctx)
	if err != nil {
		return err
	}
	defer stopForward()

	kept := rec.alwaysKept()
	step("1. Before the first agent: only the always-kept objects (%s) in each namespace", tenantObjectKeys(kept))
	for _, ns := range namespaces {
		if err := waitTenantObjects(ns, rec, kept, "the reconciler's objects before an agent"); err != nil {
			return err
		}
		note("%s: %s", ns, tenantObjectKeys(kept))
	}

	withAgents := rec.withAgents(pool, poolServiceAccount)
	for i, ns := range namespaces {
		person, token, other := people[i], tokens[i], namespaces[1-i]
		// The first organization is steps 2 and 3 of #739, the second
		// both in step 4.
		n, m := 2, 3
		if i == 1 {
			n, m = 4, 4
		}
		step("%d. %s creates %s in %s, the other organization — expecting 403", n, person.Email, tenantsTestAgent, other)
		status, raw, err := api.call(http.MethodPost, "/agents", token, tenantsTestAgentSpec(other, rec.modelConfig))
		if err != nil {
			return err
		}
		if status != http.StatusForbidden {
			return fmt.Errorf("step %d: %s's create in %s answered %d, want 403 (the person has no role there): %s", n, person.Email, other, status, excerpt(string(raw), 300))
		}
		if exists, err := tenantsTestReleaseExists(other); err != nil || exists {
			return fmt.Errorf("step %d: HelmRelease %s/%s exists (or cannot be read: %v) although the create was refused", n, other, tenantsTestAgent, err)
		}
		note("refused: %s", excerpt(string(raw), 200))

		step("%d. %s creates %s in %s — the reconciler copies %s", n, person.Email, tenantsTestAgent, ns, tenantObjectKeys(withAgents))
		status, raw, err = api.call(http.MethodPost, "/agents", token, tenantsTestAgentSpec(ns, rec.modelConfig))
		if err != nil {
			return err
		}
		var created struct {
			RequestedBy string `json:"requestedBy"`
		}
		if status != http.StatusCreated || json.Unmarshal(raw, &created) != nil {
			return fmt.Errorf("step %d: %s's create in %s answered %d, want 201: %s", n, person.Email, ns, status, excerpt(string(raw), 300))
		}
		if created.RequestedBy != person.Email {
			return fmt.Errorf("step %d: the create in %s carries requestedBy=%q, want %s", n, ns, created.RequestedBy, person.Email)
		}
		if err := waitTenantObjects(ns, rec, withAgents, fmt.Sprintf("step %d: the copies of the first agent", n)); err != nil {
			return err
		}
		note("201, requestedBy %s; %s holds %s", created.RequestedBy, ns, tenantObjectKeys(withAgents))

		step("%d. The worker of WorkerPool %s/%s admitted and Ready (%d replica(s)), agent %s ready", m, ns, pool, rec.replicas, tenantsTestAgent)
		workers, err := waitTenantWorkers(ns, pool, rec.replicas)
		if err != nil {
			return fmt.Errorf("step %d: %w", m, err)
		}
		if err := api.waitAgentReady(ns, token); err != nil {
			return fmt.Errorf("step %d: %w", m, err)
		}
		note("workers Running: %s", strings.Join(workers, ", "))

		step("%d. One turn as %s; the credential provider reads %s only for it", m, person.Email, ns)
		since := time.Now()
		reply, err := firstTenantTurn(cfg, ns, token)
		if err != nil {
			return fmt.Errorf("step %d: %w", m, err)
		}
		note("answered: %s", excerpt(reply, 120))
		read, err := tenantCredentialReads(ns, since)
		if err != nil {
			return err
		}
		if len(read) == 0 || slices.ContainsFunc(read, func(r string) bool { return r != ns }) {
			return fmt.Errorf("step %d: the credential provider read %v for atespace %s during the turn, want %s and nothing else", m, read, ns, ns)
		}
		note("credentials read for atespace %s: %d from %s", ns, len(read), ns)
	}

	step("5. As %s (cluster-admin): the workers of both organizations held by the %s* admission policies", admin.Email, tenantAdmissionPolicyPrefix)
	adminToken, err := passwordGrant(cfg, config.KubernetesClientID, config.KubernetesClientSecret, admin.Email, admin.Password, loginScopes)
	if err != nil {
		return err
	}
	for _, ns := range namespaces {
		refusals, err := probeTenantWorkerAdmission(adminToken, ns, pool)
		if err != nil {
			return fmt.Errorf("step 5: %w", err)
		}
		note("%s: refused %s", ns, strings.Join(refusals, ", "))
	}
	netpolVerdict := "SKIP: the network policy copies — the lab runs with network policies off (agent-manager copies none: no --tenant-network-policy-flavor and --tenant-network-policies)"
	if rec.copiesNetworkPolicies() {
		step("5. The network policy copies in each namespace: %s %s", rec.networkPolicyKind, strings.Join(rec.networkPolicies, ", "))
		// Step 1 and the copies' wait held them in the wanted set; read
		// again after the turns, labelled as kept in every tenant.
		for _, ns := range namespaces {
			got, err := tenantObjects(ns, rec)
			if err != nil {
				return err
			}
			for _, name := range rec.networkPolicies {
				if got[rec.networkPolicyKind+"/"+name] != tenantScopeNamespace {
					return fmt.Errorf("step 5: no %s %s/%s labelled %s=%s", rec.networkPolicyKind, ns, name, tenantObjectLabel, tenantScopeNamespace)
				}
			}
		}
		netpolVerdict = fmt.Sprintf("PASS: each organization namespace holds the %s copies %s", rec.networkPolicyKind, strings.Join(rec.networkPolicies, ", "))
	} else {
		step("5. The network policy copies: skipped, the lab runs with network policies off")
	}
	grantsVerdict := "SKIP: the credential provider's grants by label — its policy has no ownNamespaceSelector entry (giantswarm/agent-platform#1041); the lab lists the two namespaces by hand"
	if selector {
		step("5. The credential provider's live grants (/statusz): each organization namespace read by its own atespace only")
		grants, err := credentialProviderGrants(ctx)
		if err != nil {
			return err
		}
		if err := ownGrantsOnly(grants, namespaces); err != nil {
			return fmt.Errorf("step 5: %w", err)
		}
		grantsVerdict = "PASS: the credential provider's live grants give each organization's atespace its own namespace only"
	} else {
		step("5. The credential provider's grants by label: skipped, its policy has no ownNamespaceSelector entry")
	}

	for i, ns := range namespaces {
		person, token := people[i], tokens[i]
		step("6. %s deletes %s in %s, the namespace's last agent — the copies go, the always-kept objects stay", person.Email, tenantsTestAgent, ns)
		status, raw, err := api.call(http.MethodDelete, "/agents/"+ns+"/"+tenantsTestAgent, token, nil)
		if err != nil {
			return err
		}
		var deleted struct {
			HelmReleaseDeleted bool   `json:"helmReleaseDeleted"`
			RequestedBy        string `json:"requestedBy"`
		}
		if status != http.StatusOK || json.Unmarshal(raw, &deleted) != nil || !deleted.HelmReleaseDeleted {
			return fmt.Errorf("step 6: %s's delete in %s answered %d: %s", person.Email, ns, status, excerpt(string(raw), 300))
		}
		// giantswarm/agent-manager#175: today the last delete also removes
		// the namespace's OCIRepository, and the reconciler creates it
		// again at once. The wait accepts either order; it asserts the end
		// state only.
		if err := waitTenantObjects(ns, rec, kept, "step 6: the reconciler's objects after the last agent"); err != nil {
			return err
		}
		note("requestedBy %s; %s holds %s", deleted.RequestedBy, ns, tenantObjectKeys(kept))
	}

	fmt.Println()
	fmt.Printf("PASS: before an agent, %s and %s hold only %s\n", namespaces[0], namespaces[1], tenantObjectKeys(kept))
	fmt.Printf("PASS: %s's and %s's creates in the other organization are refused (403); in their own, the first agent makes the reconciler copy %s (%d worker(s))\n", people[0].Email, people[1].Email, tenantObjectKeys(withAgents), rec.replicas)
	fmt.Printf("PASS: in each organization the worker is admitted and Running, the agent ready, and a turn as the person answers on credentials the provider read from the agent's own namespace only\n")
	fmt.Printf("PASS: as %s (cluster-admin), a WorkerPool patch and delete, a pod labelled %s and an exec into a worker are refused by the %s* admission policies\n", admin.Email, workerPoolLabel, tenantAdmissionPolicyPrefix)
	fmt.Println(netpolVerdict)
	fmt.Println(grantsVerdict)
	fmt.Printf("PASS: deleting each namespace's last agent removes the copies; the always-kept objects stay\n")
	return nil
}

// tenantsTestAgentSpec is the create request: a chat-only agent (no MCP
// binding) on the namespace's own copy of the default ModelConfig.
func tenantsTestAgentSpec(ns, modelConfig string) map[string]any {
	return map[string]any{
		namespaceKey:   ns,
		nameKey:        tenantsTestAgent,
		descriptionKey: "Throwaway agent of `agentlab tenants-test`; deleted by the same run.",
		"modelConfig":  modelConfig,
		toolsetKey:     []string{presetNone},
	}
}

// readTenantReconciler reads the reconciler's settings off agent-manager's
// Deployment.
func readTenantReconciler() (tenantReconciler, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	deploy, err := getObject(ctx, gvrDeployments, platformNamespace, agentManagerMCPServer)
	if err != nil {
		return tenantReconciler{}, fmt.Errorf("the agent-manager Deployment in %s: %w", platformNamespace, err)
	}
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	var args []string
	for _, c := range containers {
		container, _ := c.(map[string]any)
		a, _, _ := unstructured.NestedStringSlice(container, "args")
		args = append(args, a...)
	}
	return tenantReconcilerFrom(args)
}

// platformWorkerPool is the WorkerPool the platform Harness runs on and its
// workers' ServiceAccount: what the reconciler copies into a tenant.
func platformWorkerPool(harness string) (pool, serviceAccount string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	h, err := readKagentObject("harnesses."+kagentAPIGroup, harness)
	if err != nil {
		return "", "", fmt.Errorf("the platform Harness %s/%s: %w", kagentNamespace, harness, err)
	}
	pool, _, _ = unstructured.NestedString(h.Object, "spec", "substrate", "workerPoolRef", "name")
	if pool == "" {
		return "", "", fmt.Errorf("the platform Harness %s/%s names no spec.substrate.workerPoolRef", kagentNamespace, harness)
	}
	gvr, err := gvrFor(workerPoolsResource)
	if err != nil {
		return "", "", err
	}
	p, err := getObject(ctx, gvr, kagentNamespace, pool)
	if err != nil {
		return "", "", err
	}
	serviceAccount, _, _ = unstructured.NestedString(p.Object, "spec", "template", "serviceAccountName")
	if serviceAccount == "" {
		return "", "", fmt.Errorf("WorkerPool %s/%s names no spec.template.serviceAccountName", kagentNamespace, pool)
	}
	return pool, serviceAccount, nil
}

// readCredentialPolicies reads the policy file the credential provider
// loads, from its ConfigMap.
func readCredentialPolicies() ([]credentialPolicy, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	cm, err := getObject(ctx, gvrConfigMaps, substrateNamespace, credentialProviderPolicy)
	if err != nil {
		return nil, fmt.Errorf("the credential provider's namespace policy: %w", err)
	}
	file, _, _ := unstructured.NestedString(cm.Object, "data", credentialProviderPolicyKey)
	return parseCredentialPolicies(file)
}

// tenantObjects lists what the reconciler wrote in a namespace (everything
// labelled tenantObjectLabel) as kind/name -> scope, over the kinds it writes.
func tenantObjects(ns string, rec tenantReconciler) (map[string]string, error) {
	resources := []string{
		"ocirepositories.source.toolkit.fluxcd.io", "serviceaccounts", "rolebindings.rbac.authorization.k8s.io",
		gvrSecrets.Resource, "harnesses." + kagentAPIGroup, workerPoolsResource,
	}
	switch rec.networkPolicyKind {
	case "NetworkPolicy":
		resources = append(resources, "networkpolicies.networking.k8s.io")
	case "CiliumNetworkPolicy":
		resources = append(resources, "ciliumnetworkpolicies.cilium.io")
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	got := map[string]string{}
	add := func(items []unstructured.Unstructured) {
		for _, item := range items {
			got[item.GetKind()+"/"+item.GetName()] = item.GetLabels()[tenantObjectLabel]
		}
	}
	for _, resource := range resources {
		gvr, err := gvrFor(resource)
		if err != nil {
			return nil, err
		}
		items, err := listObjects(ctx, gvr, ns, tenantObjectLabel)
		if err != nil {
			return nil, err
		}
		add(items)
	}
	mc, err := modelConfigGVR()
	if err != nil {
		return nil, err
	}
	items, err := listObjects(ctx, mc, ns, tenantObjectLabel)
	if err != nil {
		return nil, err
	}
	add(items)
	return got, nil
}

// waitTenantObjects waits until the reconciler's objects in the namespace
// are exactly the wanted ones, bounded; the deadline names the difference.
func waitTenantObjects(ns string, rec tenantReconciler, want map[string]string, what string) error {
	var diff string
	var readErr error
	agreed := waitFor(int(tenantsTestCopiesTimeout/pollInterval), pollInterval, func() bool {
		got, err := tenantObjects(ns, rec)
		if readErr = err; err != nil {
			return false
		}
		diff = tenantObjectsDiff(got, want)
		return diff == ""
	})
	switch {
	case agreed:
		return nil
	case readErr != nil:
		return fmt.Errorf("%s in %s: %w", what, ns, readErr)
	default:
		return fmt.Errorf("%s in %s, after %s: %s;\ncheck `KUBECONFIG=state/kubeconfig kubectl -n %s logs deploy/%s` and `kubectl -n %s get all,sa,rolebindings,secrets -l %s`",
			what, ns, tenantsTestCopiesTimeout, diff, platformNamespace, agentManagerMCPServer, ns, tenantObjectLabel)
	}
}

// waitTenantWorkers waits for the tenant's WorkerPool to report its
// replicas ready and returns its Running worker pods.
func waitTenantWorkers(ns, pool string, replicas int64) ([]string, error) {
	gvr, err := gvrFor(workerPoolsResource)
	if err != nil {
		return nil, err
	}
	var running []string
	var last, wrong string
	ready := waitFor(int(tenantsTestWorkerTimeout/pollInterval), pollInterval, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		p, err := getObject(ctx, gvr, ns, pool)
		if err != nil {
			last = err.Error()
			return false
		}
		specReplicas, _, _ := unstructured.NestedInt64(p.Object, "spec", "replicas")
		if specReplicas != replicas {
			wrong = fmt.Sprintf("WorkerPool %s/%s has spec.replicas %d, want agent-manager's --tenant-worker-pool-replicas %d", ns, pool, specReplicas, replicas)
			return true
		}
		readyReplicas, _, _ := unstructured.NestedInt64(p.Object, "status", "readyReplicas")
		pods, err := listObjects(ctx, gvrPods, ns, workerPoolLabel+"="+pool)
		if err != nil {
			last = err.Error()
			return false
		}
		running = running[:0]
		for _, pod := range pods {
			if phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase"); phase == string(corev1.PodRunning) {
				running = append(running, pod.GetName())
			}
		}
		last = fmt.Sprintf("readyReplicas %d of %d, %d worker pod(s) Running of %d", readyReplicas, replicas, len(running), len(pods))
		return readyReplicas == replicas && int64(len(running)) == replicas
	})
	if wrong != "" {
		return nil, errors.New(wrong)
	}
	if !ready {
		return nil, fmt.Errorf("WorkerPool %s/%s not ready within %s (%s): its workers are not admitted or not starting — `kubectl -n %s get events`", ns, pool, tenantsTestWorkerTimeout, last, ns)
	}
	return running, nil
}

// firstTenantTurn is one turn on the tenant's agent as the person, tried
// once more like firstTurnAs: a cold worker's first resume can run into
// Substrate's un-retried ResumeActor deadline.
func firstTenantTurn(cfg *config.Config, ns, token string) (string, error) {
	agent := &apiv1alpha1.ResourceReference{Namespace: ns, Name: tenantsTestAgent}
	reply, err := agentTurnOf(cfg, agent, token, tenantsTestPrompt)
	if err == nil {
		return reply, nil
	}
	note("the first turn on %s/%s failed (%s) — one retry, a cold worker's ResumeActor deadline is not retried by Substrate", ns, tenantsTestAgent, excerpt(err.Error(), 200))
	return agentTurnOf(cfg, agent, token, tenantsTestPrompt)
}

// tenantCredentialReads are the namespaces the credential provider read for
// actors of the namespace's atespace since the given time.
func tenantCredentialReads(ns string, since time.Time) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	logs, err := podLogs(ctx, substrateNamespace, "deploy/"+credentialProvider, credentialProvider, time.Since(since)+5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("the credential provider's log: %w", err)
	}
	return credentialReads(logs, ns), nil
}

// probeTenantWorkerAdmission tries, as the admin's token, every write the
// tenant admission policies hold in the namespace — a WorkerPool patch and
// delete and a worker-labelled pod as server-side dry runs, and an exec into
// a worker as a request without the stream upgrade, which admission judges
// before the apiserver asks for the upgrade — and returns what was refused.
func probeTenantWorkerAdmission(token, ns, pool string) ([]string, error) {
	cfg, err := tokenConfig(token)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	poolGVR, err := gvrFor(workerPoolsResource)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	dryRun := []string{metav1.DryRunAll}
	pods, err := listObjects(ctx, gvrPods, ns, workerPoolLabel+"="+pool)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("no worker pod of WorkerPool %s/%s to exec into", ns, pool)
	}
	worker := pods[0].GetName()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: tenantsTestAgent + "-worker", Namespace: ns, Labels: map[string]string{workerPoolLabel: pool}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: probeContainer, Image: "registry.k8s.io/pause:3.10"}}},
	}
	// Compliant with the restricted Pod Security profile, so only the
	// worker label can be what refuses it.
	restrictPodSecurity(pod)
	probes := []struct {
		what string
		call func() error
	}{
		{"WorkerPool patch", func() error {
			_, err := dyn.Resource(poolGVR).Namespace(ns).Patch(ctx, pool, types.MergePatchType, []byte(`{"spec":{"replicas":2}}`), metav1.PatchOptions{DryRun: dryRun})
			return err
		}},
		{"WorkerPool delete", func() error {
			return dyn.Resource(poolGVR).Namespace(ns).Delete(ctx, pool, metav1.DeleteOptions{DryRun: dryRun})
		}},
		{"pod labelled " + workerPoolLabel, func() error {
			_, err := clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{DryRun: dryRun})
			return err
		}},
		{"exec into " + worker, func() error {
			_, err := clientset.CoreV1().RESTClient().Post().Namespace(ns).Resource("pods").Name(worker).SubResource("exec").
				Param("command", "true").Param("stdout", "true").DoRaw(ctx)
			return err
		}},
	}
	var refused []string
	for _, p := range probes {
		if err := tenantAdmissionRefusal(fmt.Sprintf("%s in %s", p.what, ns), p.call()); err != nil {
			return nil, err
		}
		refused = append(refused, p.what)
	}
	return refused, nil
}

// credentialProviderGrants reads the provider's live grants off its /statusz
// page (JSON), through the apiserver's pod proxy.
func credentialProviderGrants(ctx context.Context) (map[string][]string, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	pods, err := k.targetPods(ctx, substrateNamespace, "deploy/"+credentialProvider)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%d/proxy/statusz", substrateNamespace, pods[0].Name, credentialProviderStatusPort)
	raw, err := k.raw.Get().AbsPath(path).Param("format", "json").DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	var status struct {
		PolicyGrants map[string][]string `json:"policy_grants"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil, fmt.Errorf("the credential provider's /statusz: %w", err)
	}
	return status.PolicyGrants, nil
}

// checkTenantEditorRole fails, naming the role, when the chart renders no
// agent-platform:editor: a RoleBinding to a missing ClusterRole is accepted
// and grants nothing, so the creates would fail as a bare 403.
func checkTenantEditorRole() error {
	gvr, err := gvrFor("clusterroles.rbac.authorization.k8s.io")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	exists, err := objectExists(ctx, gvr, "", tenantEditorRole)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("no ClusterRole %s: the installed agent-platform chart does not render the platform's roles, which a person needs to write agents in an organization namespace", tenantEditorRole)
	}
	return nil
}

// createTenantsTestNamespace creates the organization namespace and binds
// the person to the editor role in it.
func createTenantsTestNamespace(ns, email string) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if _, err := applyTyped(ctx, &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{organizationLabel: strings.TrimPrefix(ns, "org-")}},
	}); err != nil {
		return err
	}
	_, err := applyTyped(ctx, &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: tenantEditorRole, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: tenantEditorRole},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: apiserverUserPrefix + email}},
	})
	return err
}

// tenantsTestReleaseExists reports whether the agent's HelmRelease is in the
// namespace.
func tenantsTestReleaseExists(ns string) (bool, error) {
	gvr, err := gvrFor(helmReleaseResource)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	return objectExists(ctx, gvr, ns, tenantsTestAgent)
}

// removeTenantsTestNamespace removes the agent's HelmRelease first, while
// the namespace's Flux ServiceAccount can still uninstall it, then the
// namespace with everything in it.
func removeTenantsTestNamespace(ns string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*tenantsTestNamespaceTimeout)
	defer cancel()
	exists, err := objectExists(ctx, gvrNamespaces, "", ns)
	if err != nil || !exists {
		return err
	}
	note("cleanup: removing namespace %s", ns)
	gvr, err := gvrFor(helmReleaseResource)
	if err != nil {
		return err
	}
	if err := deleteObject(ctx, gvr, ns, tenantsTestAgent, tenantsTestNamespaceTimeout); err != nil {
		note("cleanup: %v", err)
	}
	return deleteObject(ctx, gvrNamespaces, "", ns, tenantsTestNamespaceTimeout)
}

// agentManagerREST is agent-manager's REST API (/api/v1) on a port-forward
// to its pod, called with a person's Dex token, as the portal backend does.
type agentManagerREST struct {
	base   string
	client *http.Client
}

// dialAgentManagerREST forwards a local port to a running agent-manager
// pod's HTTP port. The stop ends the forward.
func dialAgentManagerREST(ctx context.Context) (*agentManagerREST, func(), error) {
	k, err := labKube()
	if err != nil {
		return nil, nil, err
	}
	pods, err := k.targetPods(ctx, platformNamespace, "deploy/"+agentManagerMCPServer)
	if err != nil {
		return nil, nil, err
	}
	i := slices.IndexFunc(pods, func(p corev1.Pod) bool { return p.Status.Phase == corev1.PodRunning })
	if i < 0 {
		return nil, nil, fmt.Errorf("no Running agent-manager pod in %s", platformNamespace)
	}
	port, stop, err := portForwardPod(ctx, platformNamespace, pods[i].Name, agentManagerHTTPPort)
	if err != nil {
		return nil, nil, err
	}
	return &agentManagerREST{base: fmt.Sprintf("http://127.0.0.1:%d/api/v1", port), client: &http.Client{Timeout: time.Minute}}, stop, nil
}

// call sends one request as the token's person and returns the status and
// the body.
func (a *agentManagerREST) call(method, path, token string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, a.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("agent-manager %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

// waitAgentReady polls the agent's status as the person until
// agent-manager's verdict is ready, bounded.
func (a *agentManagerREST) waitAgentReady(ns, token string) error {
	var last string
	ready := waitFor(int(agentsTestReadyTimeout/pollInterval), pollInterval, func() bool {
		status, raw, err := a.call(http.MethodGet, "/agents/"+ns+"/"+tenantsTestAgent+"/status", token, nil)
		if err != nil {
			last = err.Error()
			return false
		}
		var st agentManagerStatus
		if status != http.StatusOK || json.Unmarshal(raw, &st) != nil {
			last = fmt.Sprintf("%d %s", status, excerpt(string(raw), 200))
			return false
		}
		last = st.Verdict + " — " + st.Summary
		return st.Verdict == verdictReady
	})
	if !ready {
		return fmt.Errorf("agent %s/%s not ready within %s (last: %s)", ns, tenantsTestAgent, agentsTestReadyTimeout, last)
	}
	return nil
}
