package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The organizations proof (giantswarm/agentlab#1149, the
// agent-platform-organizations plan's testing decisions): two
// AgentPlatformOrganizations in the lab's root namespace, one member each,
// bound by the group in their token as an editor of their organization
// (alice in tenant-a, bob in tenant-b). Each member creates an agent in
// their own organization as themselves (a HelmRelease of the agent chart
// through the Kubernetes API, which the chart's field lock admits) and runs
// one turn on it. Then:
//
//   - the same answer on every surface: the agents each member sees through
//     the Kubernetes API, the kagent API through the edge and Slack are the
//     same set, their own organization's agent and nothing of the other;
//     the Dev Portal's is skipped until its organization pages land
//     (giantswarm/backstage#3363);
//   - the negative cross-organization tests, each a verdict naming its
//     surface and its boundary, each with a twin in the member's own
//     organization that is admitted, so a refusal for any other reason
//     fails: Kubernetes, the kagent API and A2A, muster, the other
//     organization's model, in-cluster endpoints on 443 through the egress
//     gateway;
//   - the management-cluster profile tests that fit the lab: the
//     object-count ResourceQuota, a Kyverno PolicyException that does not
//     lift the sandbox admission policies, cleanup of an Orphaned
//     organization with a member-owned running session.
//
// Successful runs delete both proof organizations. Failed cleanup retains
// its fixtures for diagnosis.
const (
	// organizationsTestNames are the proof's two organizations.
	organizationsTestA = "agentlab-a"
	organizationsTestB = "agentlab-b"
	// organizationsTestAgent is the agent each member creates in their own
	// organization.
	organizationsTestAgent = "agentlab-organizations"
	// organizationsTestCrossAgent is the name of a create in the other
	// organization; it never exists there.
	organizationsTestCrossAgent = "agentlab-organizations-cross"
	// organizationsTestBinding names the members' RoleBinding.
	organizationsTestBinding = "agentlab-members"
	// organizationsTestPrompt is the turn each member's agent answers: any
	// reply proves the model call, which needs the organization's own key.
	organizationsTestPrompt = "Reply with the single word: ready"
	// organizationsTestReadyTimeout bounds an agent's boot in an
	// organization (the golden boot of the organization's Harness copy).
	organizationsTestReadyTimeout = 10 * time.Minute
	// organizationsTestWorkerTimeout bounds the organization's worker.
	organizationsTestWorkerTimeout = 5 * time.Minute
	// organizationsTestLabel marks what the proof writes, for its cleanup,
	// with organizationsTestMark as its value.
	organizationsTestLabel = "agentlab.giantswarm.io/organizations-test"
	organizationsTestMark  = "proof"
	// organizationQuota is the operator's ResourceQuota of object counts.
	organizationQuota = "agent-platform-organization-objects"
	// organizationQuotaResource is the counted resource the quota test
	// fills: ServiceAccounts, the smallest default count (100) of a kind
	// nothing of the platform's watches, under the key the quota counts
	// them by (organizationQuotaKey; HACKS.md U42).
	organizationQuotaResource = "serviceaccounts"
	organizationQuotaKey      = "count/" + organizationQuotaResource
	// organizationQuotaRefusal is the API server's refusal of a write over
	// a ResourceQuota.
	organizationQuotaRefusal = "exceeded quota"
	// organizationPolicyPrefix and tenantPolicyPrefix start the chart's
	// ValidatingAdmissionPolicies that hold an organization namespace (the
	// workers' pair is rendered per set of namespaces under the second).
	organizationPolicyPrefix = "agent-platform-organization-"
	tenantPolicyPrefix       = "agent-platform-tenant-"
	// policyExceptionsNamespace is the fleet's namespace of Kyverno
	// PolicyExceptions, where every Organization's automation
	// ServiceAccount may write them.
	policyExceptionsNamespace = "policy-exceptions"
	// organizationsTestPolicyException lifts every Kyverno rule for every
	// agents-* namespace.
	organizationsTestPolicyException = "agentlab-organizations-lift-all"
	// workerPoolLabel marks a worker pod with the WorkerPool it belongs to.
	workerPoolLabel = "ate.dev/worker-pool"
	// apiserverUserPrefix is the apiserver's --oidc-username-prefix (and
	// group prefix): an RBAC subject names a lab person as oidc:<email>.
	apiserverUserPrefix = "oidc:"
	// The Substrate credential provider: its Deployment, the port of its
	// /statusz page, and the line it logs per read.
	credentialProvider              = "k8s-credential-provider" // #nosec G101 -- a Deployment name, not a credential
	credentialProviderStatusPort    = 4040
	credentialProviderGoldenSpace   = "ate-golden"
	credentialProviderResolvingLine = "resolving credential"
	// ateAPIHost is Substrate's API server, an in-cluster endpoint on 443
	// no agent may reach.
	ateAPIHost = "api." + substrateNamespace + ".svc"
	// kubernetesAPIHost is the API server's in-cluster name, on 443.
	kubernetesAPIHost = "kubernetes.default.svc"
)

// Surfaces, as the verdicts name them.
const (
	surfaceKubernetes = "Kubernetes API"
	surfaceKagent     = "kagent API and A2A"
	surfaceMuster     = "muster"
	surfaceLLM        = "models and the llm listener"
	surfaceEgress     = "egress gateway"
	surfaceSlack      = "Slack"
	surfacePortal     = "Dev Portal"
	surfaceProfile    = "management-cluster profile"
)

// organizationVerdict is one line of the proof's report: the surface it
// was judged on, the boundary it proves, and what was seen, or why it was
// skipped. A verdict without a surface or a boundary is refused (lint).
type organizationVerdict struct {
	surface, boundary string
	detail            string
	skip              string
}

// lint fails a verdict that does not name what it proves.
func (v organizationVerdict) lint() error {
	switch {
	case strings.TrimSpace(v.surface) == "":
		return fmt.Errorf("a verdict without a surface: %q", v.boundary)
	case strings.TrimSpace(v.boundary) == "":
		return fmt.Errorf("a verdict on %s without a boundary", v.surface)
	case v.skip == "" && strings.TrimSpace(v.detail) == "":
		return fmt.Errorf("a passing verdict on %s (%s) without what was seen", v.surface, v.boundary)
	}
	return nil
}

// line is the verdict as the report prints it.
func (v organizationVerdict) line() string {
	if v.skip != "" {
		return fmt.Sprintf("SKIP [%s] %s — %s", v.surface, v.boundary, v.skip)
	}
	return fmt.Sprintf("PASS [%s] %s — %s", v.surface, v.boundary, v.detail)
}

// organizationCheck is one negative or profile test: its surface and
// boundary, and the run that answers what was seen or the failure; a skip
// reason instead of a run.
type organizationCheck struct {
	surface, boundary string
	skip              string
	run               func() (string, error)
}

// verdict runs the check, or skips it, and fails naming the surface and the
// boundary.
func (c organizationCheck) verdict() (organizationVerdict, error) {
	v := organizationVerdict{surface: c.surface, boundary: c.boundary, skip: c.skip}
	switch {
	case c.skip != "":
		return v, v.lint()
	case c.run == nil:
		return v, fmt.Errorf("the check on %s (%s) has neither a run nor a skip reason", c.surface, c.boundary)
	}
	step("[%s] %s", c.surface, c.boundary)
	detail, err := c.run()
	if err != nil {
		return v, fmt.Errorf("FAIL [%s] %s: %w", c.surface, c.boundary, err)
	}
	v.detail = detail
	return v, v.lint()
}

// OrganizationsTestOptions tunes the proof's Slack surface: the
// klaus-gateway it runs on the host (klaus-gateway-test's GatewayImage and
// GatewayBinary), or none.
type OrganizationsTestOptions struct {
	GatewayImage, GatewayBinary string
	Part                        string
	SkipSlack                   bool
}

// organizationsProof is one run's state.
type organizationsProof struct {
	cfg  *config.Config
	opts OrganizationsTestOptions
	// names are the two organizations, namespaces their namespaces.
	names, namespaces [2]string
	// members are alice and bob, tokens their Dex tokens (the platform
	// client's audiences), apis their kagent API clients.
	members [2]*config.User
	tokens  [2]string
	apis    [2]*kagentAPI
	admin   *config.User
	// adminAPI is the admin's (cluster-admin) kagent API client.
	adminAPI *kagentAPI
	// sessions are the member-owned Sessions cleanup must remove.
	sessions                 [2][]string
	verdicts                 []organizationVerdict
	retainCleanupDiagnostics bool
}

// OrganizationsTest is the organizations proof (the steps above).
func OrganizationsTest(cfg *config.Config, opts OrganizationsTestOptions) error {
	part, err := organizationProofPart(opts.Part)
	if err != nil {
		return err
	}
	opts.Part = part
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.OrganizationsEnabled() {
		if why := cfg.OrganizationsInactive(); why != "" {
			return errors.New(why)
		}
		return fmt.Errorf("platform.organizations is off in %s — `agentlab configure --organizations`, then `agentlab platform`", config.File)
	}
	admin := cfg.AdminUser()
	if admin == nil {
		return fmt.Errorf("no platform-admins user in %s", config.File)
	}
	if err := checkApiserverOIDC(cfg); err != nil {
		return err
	}
	if part == organizationCredentialsPart {
		return organizationCredentialsTest(cfg, admin)
	}
	members, err := organizationsTestMembers(cfg)
	if err != nil {
		return err
	}
	p := &organizationsProof{cfg: cfg, opts: opts, names: [2]string{organizationsTestA, organizationsTestB}, members: members, admin: admin}
	defer p.cleanup()
	if err := p.run(); err != nil {
		return err
	}
	fmt.Println()
	for _, v := range p.verdicts {
		fmt.Println(v.line())
	}
	return nil
}

func (p *organizationsProof) run() error {
	if err := p.preconditions(); err != nil {
		return err
	}
	if err := p.setUp(); err != nil {
		return err
	}
	p.cleanupAgents()

	for i, ns := range p.namespaces {
		member := p.members[i]
		step("%s (editor of %s by group %s) creates %s/%s as a HelmRelease of the agent chart, as themselves", member.Email, p.names[i], member.Groups[0], ns, organizationsTestAgent)
		if err := p.createAgentAs(i, ns, organizationsTestAgent); err != nil {
			return err
		}
	}
	for i, ns := range p.namespaces {
		step("%s/%s Ready on the organization's Harness copy; its WorkerPool runs a worker", ns, organizationsTestAgent)
		boot, err := waitOrganizationAgentReady(ns, organizationsTestAgent, organizationsTestReadyTimeout)
		if err != nil {
			return err
		}
		note("Ready in %s", boot.Round(time.Second))
		if p.opts.Part == organizationCleanupPart {
			continue
		}
		since := time.Now()
		reply, session, err := agentSessionTurnOf(p.cfg, &apiv1alpha1.ResourceReference{Namespace: ns, Name: organizationsTestAgent}, p.tokens[i], organizationsTestPrompt)
		if session != "" {
			p.sessions[i] = append(p.sessions[i], session)
		}
		if err != nil {
			return fmt.Errorf("%s's turn on %s/%s: %w", p.members[i].Email, ns, organizationsTestAgent, err)
		}
		note("%s's turn answered: %s", p.members[i].Email, excerpt(reply, 120))
		read, err := credentialReadsSince(ns, since)
		if err != nil {
			return err
		}
		if len(read) == 0 || slices.ContainsFunc(read, func(r string) bool { return r != ns }) {
			return fmt.Errorf("the credential provider read %v for atespace %s during the turn, want %s and nothing else", read, ns, ns)
		}
	}
	if p.opts.Part == organizationCleanupPart {
		verdict, err := p.cleanupCheck().verdict()
		if err != nil {
			return err
		}
		p.add(verdict)
		return nil
	}
	p.add(organizationVerdict{surface: surfaceKagent, boundary: "each member runs an agent in their own organization",
		detail: fmt.Sprintf("%s and %s each created %s in their own organization as themselves, it became Ready and a turn as its member answered on credentials read from its own namespace only", p.members[0].Email, p.members[1].Email, organizationsTestAgent)})

	// The edge before Slack: Slack's refusals are judged against the
	// controller, which the edge's checks have proven first.
	edge := &organizationsEdge{namespaces: p.namespaces, members: p.members, apis: p.apis, admin: p.adminAPI}
	edgeVerdicts, kagentRosters, err := edge.run()
	if err != nil {
		return err
	}
	p.verdicts = append(p.verdicts, edgeVerdicts...)
	kubeRosters, err := p.kubernetesRosters()
	if err != nil {
		return err
	}
	rosters := map[string][2]refSet{surfaceKubernetes: kubeRosters, surfaceKagent: kagentRosters}
	if p.opts.SkipSlack {
		p.add(organizationVerdict{surface: surfaceSlack, boundary: "each member's roster is their own organization's", skip: "--skip-slack"})
	} else {
		slack := &organizationsSlack{cfg: p.cfg, opts: KlausGatewayTestOptions{GatewayImage: p.opts.GatewayImage, GatewayBinary: p.opts.GatewayBinary},
			namespaces: p.namespaces, users: p.members, admin: p.adminAPI}
		slackVerdicts, slackRosters, err := slack.run()
		if err != nil {
			return err
		}
		p.verdicts = append(p.verdicts, slackVerdicts...)
		rosters[surfaceSlack] = slackRosters
	}
	p.add(organizationVerdict{surface: surfacePortal, boundary: "each member's roster and New agent picker are their own organization's",
		skip: "the Dev Portal's organization page and New agent picker are giantswarm/backstage#3363 (slice 15), not in the lab's portal yet"})
	same, err := sameAnswer(rosters, p.members)
	if err != nil {
		return err
	}
	p.add(organizationVerdict{surface: strings.Join(slices.Sorted(maps.Keys(rosters)), ", "), boundary: "every surface gives each member the same agents", detail: same})

	muster := &organizationsMuster{cfg: p.cfg, namespaces: p.namespaces, users: p.members}
	for _, c := range slices.Concat(p.kubernetesChecks(), muster.checks(), p.modelChecks(), p.egressChecks(), p.profileChecks()) {
		v, err := c.verdict()
		if err != nil {
			return err
		}
		p.verdicts = append(p.verdicts, v)
	}
	return nil
}

// add appends a verdict.
func (p *organizationsProof) add(v organizationVerdict) {
	p.verdicts = append(p.verdicts, v)
}

// preconditions checks what the proof stands on: the CRD and its operator,
// the organizations' roles, and the kagent authorizer judging people as the
// API server does.
func (p *organizationsProof) preconditions() error {
	step("agent-platform-operator and the organizations' roles (agentPlatformOrganizations on the installed chart)")
	if _, err := gvrFor(organizationResource); err != nil {
		return fmt.Errorf("the apiserver serves no %s: the installed chart does not run agent-platform-operator — `agentlab platform` with platform.organizations on and agent-platform %s or newer: %w", organizationResource, config.OrganizationsChartFloor, err)
	}
	for _, role := range []string{organizationEditorRole, organizationUserRole, organizationAdminRole} {
		if err := checkOrganizationRole(role); err != nil {
			return err
		}
	}
	step("The kagent controller's RBAC authorizer (ConfigMap %s/%s)", kagentNamespace, kagentControllerConfigMap)
	authz, err := readControllerAuthorization()
	if err != nil {
		return err
	}
	if problem := authz.problem(); problem != "" {
		return errors.New(problem)
	}
	note("mode %s, groups from claim %q, user prefix %q, group prefix %q", authz.mode, authz.groupsClaim, authz.usernamePrefix, authz.groupsPrefix)
	return nil
}

// setUp keeps the two organizations Ready, binds each member's group as
// an editor of their own, gives each the lab's model, and logs everyone in.
func (p *organizationsProof) setUp() error {
	for i, name := range p.names {
		if err := checkProofOrganization(name); err != nil {
			return err
		}
		step("Organization %s/%s Ready; group %s bound to %s there; its model %s", organizationsRootNamespace, name, p.members[i].Groups[0], organizationEditorRole, organizationModel)
		ns, err := ensureOrganization(organizationsRootNamespace, name, name)
		if err != nil {
			return err
		}
		p.namespaces[i] = ns
		if err := markProofOrganization(name); err != nil {
			return err
		}
		if err := bindOrganizationRole(ns, organizationAdminsBinding, organizationAdminRole, []rbacv1.Subject{groupSubject(platformAdminsGroup)}); err != nil {
			return err
		}
		if err := bindOrganizationRole(ns, organizationsTestBinding, organizationEditorRole, []rbacv1.Subject{groupSubject(p.members[i].Groups[0])}); err != nil {
			return err
		}
		if err := ensureOrganizationModel(p.cfg, ns); err != nil {
			return err
		}
		note("%s runs in namespace %s", name, ns)
	}
	for i, m := range p.members {
		step("Logging in to Dex as %s", m.Email)
		token, err := passwordGrant(p.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, m.Email, m.Password, musterLoginScopes)
		if err != nil {
			return err
		}
		p.tokens[i] = token
		if p.apis[i], err = dialKagentAPI(p.cfg, token); err != nil {
			return err
		}
	}
	adminToken, err := passwordGrant(p.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, p.admin.Email, p.admin.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	p.adminAPI, err = dialKagentAPI(p.cfg, adminToken)
	return err
}

// organizationsTestMembers are the two members, one per organization: the
// first user in each member group. Neither may hold a cluster-wide read (a
// platform admin or a viewer), which would show them every organization.
func organizationsTestMembers(cfg *config.Config) ([2]*config.User, error) {
	var users [2]*config.User
	for i, group := range []string{config.GroupTenantA, config.GroupTenantB} {
		u := cfg.FindUserInGroup(group)
		if u == nil {
			return users, fmt.Errorf("no user in group %s in %s: organizations-test binds that group as an editor of one organization — `agentlab configure --organizations` adds %s and %s, then `agentlab reload`",
				group, config.File, config.TenantUsers[0].Email, config.TenantUsers[1].Email)
		}
		if u.HasGroup(platformAdminsGroup) || u.HasGroup(viewerGroup) || u.HasGroup(developersGroup) {
			return users, fmt.Errorf("%s (group %s) is also in platform-admins, viewers or developers, which reach agents outside their organization (cluster-admin, view, the default organization's editors): the proof needs a person whose only access is the member group", u.Email, group)
		}
		users[i] = u
	}
	if users[0].Email == users[1].Email {
		return users, fmt.Errorf("%s is in both %s and %s: the proof needs one person per organization", users[0].Email, config.GroupTenantA, config.GroupTenantB)
	}
	// The member group comes first in Groups for the bindings.
	for i, group := range []string{config.GroupTenantA, config.GroupTenantB} {
		u := *users[i]
		u.Groups = append([]string{group}, slices.DeleteFunc(slices.Clone(u.Groups), func(g string) bool { return g == group })...)
		users[i] = &u
	}
	return users, nil
}

// checkOrganizationRole fails, naming the role, when the chart renders no
// such ClusterRole: a RoleBinding to a missing ClusterRole is accepted and
// grants nothing, so the calls would fail as a bare 403.
func checkOrganizationRole(role string) error {
	gvr, err := gvrFor(clusterRolesResource)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	exists, err := objectExists(ctx, gvr, "", role)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("no ClusterRole %s: the installed agent-platform chart does not render the organizations' roles", role)
	}
	return nil
}

// personClients are a person's Kubernetes clients, on their own token.
func personClients(token string) (dynamic.Interface, kubernetes.Interface, error) {
	cfg, err := tokenConfig(token)
	if err != nil {
		return nil, nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	return dyn, cs, err
}

// organizationChartSource is the name of the agent chart's OCIRepository
// the operator keeps in the organization namespace, the one an agent's
// HelmRelease must name (the chart's field lock).
func organizationChartSource(ns string) (string, error) {
	gvr, err := gvrFor(fluxOCIRepositoryResource)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	items, err := listObjects(ctx, gvr, ns, "")
	if err != nil {
		return "", err
	}
	if len(items) != 1 {
		names := make([]string, 0, len(items))
		for _, item := range items {
			names = append(names, item.GetName())
		}
		return "", fmt.Errorf("namespace %s holds %d OCIRepositories (%v), want the operator's one of the agent chart", ns, len(items), names)
	}
	return items[0].GetName(), nil
}

// organizationAgentRelease is an agent's HelmRelease in an organization
// namespace: the operator's chart source and Helm ServiceAccount, and the
// organization's model.
func organizationAgentRelease(ns, name, source, modelConfig string) *unstructured.Unstructured {
	spec := agentSpec{Name: name, ModelConfig: modelConfig, Toolset: []string{presetNone}, SystemMessage: egressTestSystemPrompt,
		Egress:      []string{DefaultEgressOrigin},
		Description: "Throwaway agent of `agentlab organizations-test`; deleted by the same run."}
	u := &unstructured.Unstructured{Object: map[string]any{
		crSpec: map[string]any{
			"interval":               helmReleaseInterval,
			podServiceAccountNameKey: kagentFluxServiceAccount,
			"chartRef":               map[string]any{kindKey: kindOCIRepository, nameKey: source, namespaceKey: ns},
			"values":                 agentValues(spec),
		},
	}}
	u.SetAPIVersion(fluxHelmReleaseAPIVersion)
	u.SetKind(kindHelmRelease)
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetLabels(map[string]string{organizationsTestLabel: organizationsTestMark, managedByLabel: managedByAgentlabValue})
	return u
}

// createAgentAs writes the member's agent in the namespace with their own
// token.
func (p *organizationsProof) createAgentAs(i int, ns, name string) error {
	source, err := organizationChartSource(ns)
	if err != nil {
		return err
	}
	dyn, _, err := personClients(p.tokens[i])
	if err != nil {
		return err
	}
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if _, err := dyn.Resource(gvr).Namespace(ns).Create(ctx, organizationAgentRelease(ns, name, source, organizationModel), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("%s's create of HelmRelease %s/%s: %w", p.members[i].Email, ns, name, err)
	}
	return nil
}

// waitOrganizationAgentReady waits until the agent's HelmRelease is Ready
// and its Agent reports Ready on its desired revision, bounded; a release
// that failed or an Agent that failed for good ends the wait.
func waitOrganizationAgentReady(ns, name string, timeout time.Duration) (time.Duration, error) {
	hr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return 0, err
	}
	ag, err := gvrFor(agentResource)
	if err != nil {
		return 0, err
	}
	started := time.Now()
	var last string
	var failed error
	ready := waitFor(int(timeout/pollInterval), pollInterval, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		release, err := getObject(ctx, hr, ns, name)
		if err != nil {
			last = err.Error()
			return false
		}
		if status, reason, message := releaseReady(release); status == condFalseStatus && (reason == helmInstallFailed || reason == helmUpgradeFailed) {
			failed = fmt.Errorf("HelmRelease %s/%s: Ready=False %s: %s", ns, name, reason, message)
			return true
		} else if status != conditionTrue {
			last = fmt.Sprintf("HelmRelease Ready=%q %s %s", status, reason, excerpt(message, 160))
			return false
		}
		obj, err := getObject(ctx, ag, ns, name)
		if err != nil {
			last = "no Agent yet: " + err.Error()
			return false
		}
		agent, err := agentFrom(obj)
		if err != nil {
			failed = err
			return true
		}
		if text, terminal := terminalAgentFailure(&agent.Status); terminal {
			failed = fmt.Errorf("the Agent %s/%s: %s", ns, name, text)
			return true
		}
		status, message := agent.Status.condition(conditionReady)
		if status == conditionTrue && agent.Status.DesiredRevision == agent.Status.LatestSuccessfulRevision && agent.Status.ObservedGeneration >= agent.Metadata.Generation {
			return true
		}
		last = fmt.Sprintf("Agent Ready=%q %s", status, excerpt(message, 160))
		return false
	})
	switch {
	case failed != nil:
		return 0, failed
	case !ready:
		return 0, fmt.Errorf("agent %s/%s not Ready within %s (last: %s) — `KUBECONFIG=state/kubeconfig kubectl -n %s get %s,%s %s -o yaml`", ns, name, timeout, last, ns, fluxHelmReleaseResource, agentResource, name)
	}
	return time.Since(started), nil
}

// releaseReady is a HelmRelease's Ready condition.
func releaseReady(obj *unstructured.Unstructured) (status, reason, message string) {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conditions {
		m, _ := c.(map[string]any)
		if m["type"] == conditionReady {
			status, _ = m["status"].(string)
			reason, _ = m["reason"].(string)
			message, _ = m["message"].(string)
		}
	}
	return status, reason, message
}

// kubernetesRosters are the agents each member lists through the
// Kubernetes API with their own token, in every organization namespace: a
// refused list is nothing.
func (p *organizationsProof) kubernetesRosters() ([2]refSet, error) {
	var out [2]refSet
	all, err := organizationNamespaces()
	if err != nil {
		return out, err
	}
	gvr, err := gvrFor(agentResource)
	if err != nil {
		return out, err
	}
	for i := range p.members {
		dyn, _, err := personClients(p.tokens[i])
		if err != nil {
			return out, err
		}
		out[i] = refSet{}
		for _, ns := range all {
			ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
			list, err := dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
			cancel()
			switch {
			case apierrors.IsForbidden(err):
				continue
			case err != nil:
				return out, fmt.Errorf("%s's list of agents in %s: %w", p.members[i].Email, ns, err)
			}
			for _, item := range list.Items {
				out[i][ns+"/"+item.GetName()] = true
			}
		}
	}
	return out, nil
}

// sameAnswer compares what each surface listed for each member: all equal,
// and each the member's own organization's agent; it returns the line.
func sameAnswer(rosters map[string][2]refSet, members [2]*config.User) (string, error) {
	surfaces := slices.Sorted(maps.Keys(rosters))
	if len(surfaces) < 2 {
		return "", fmt.Errorf("the same answer needs two surfaces, got %v", surfaces)
	}
	var parts []string
	for i, m := range members {
		first := rosters[surfaces[0]][i]
		for _, s := range surfaces[1:] {
			if diff := rosterDiff(rosters[s][i], first); diff != "" {
				return "", fmt.Errorf("%s sees %s on %s and %s on %s (%s)", m.Email, rosters[s][i], s, first, surfaces[0], diff)
			}
		}
		parts = append(parts, fmt.Sprintf("%s sees %s", m.Email, first))
	}
	return strings.Join(parts, "; ") + " on each of " + strings.Join(surfaces, ", "), nil
}

// forbiddenAs reads a call's answer as RBAC's refusal: nil for 403, an
// error naming what happened otherwise.
func forbiddenAs(what string, err error) error {
	switch {
	case err == nil:
		return fmt.Errorf("%s was answered, want 403", what)
	case apierrors.IsForbidden(err):
		return nil
	}
	return fmt.Errorf("%s failed, but not with 403: %w", what, err)
}

// kubernetesKinds are the organization's objects on the Kubernetes surface,
// with whether an editor reads them in their own organization (an editor
// reads no Secret anywhere, so that twin is a refusal too).
var kubernetesKinds = []struct {
	what, resource string
	editorReads    bool
}{
	{"agents", agentResource, true},
	{"agent templates", agentTemplateResource, true},
	{"connectors (RemoteMCPServers)", remoteMCPServerResource, true},
	{"connectors (muster MCPServers)", musterMCPServerResource, true},
	{"agent releases (HelmReleases)", fluxHelmReleaseResource, true},
	{"Secrets", gvrSecrets.Resource, false},
}

// kubernetesChecks are the Kubernetes surface's negative tests.
func (p *organizationsProof) kubernetesChecks() []organizationCheck {
	a, b := p.namespaces[0], p.namespaces[1]
	alice := p.members[0]
	return []organizationCheck{
		{surface: surfaceKubernetes, boundary: "RBAC: an organization's roles are bound in its own namespace only, so a member lists none of the other's agents, templates, connectors, models, releases or Secrets",
			run: func() (string, error) {
				dyn, _, err := personClients(p.tokens[0])
				if err != nil {
					return "", err
				}
				mc, err := modelConfigGVR()
				if err != nil {
					return "", err
				}
				kinds := slices.Clone(kubernetesKinds)
				kinds = append(kinds, struct {
					what, resource string
					editorReads    bool
				}{"models (ModelConfigs)", modelConfigResourceName(mc), true})
				var refused, twins []string
				for _, k := range kinds {
					gvr, err := gvrFor(k.resource)
					if err != nil {
						return "", err
					}
					ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
					_, err = dyn.Resource(gvr).Namespace(b).List(ctx, metav1.ListOptions{})
					_, ownErr := dyn.Resource(gvr).Namespace(a).List(ctx, metav1.ListOptions{})
					cancel()
					if err := forbiddenAs(fmt.Sprintf("%s's list of %s in %s", alice.Email, k.what, b), err); err != nil {
						return "", err
					}
					refused = append(refused, k.what)
					switch {
					case k.editorReads && ownErr != nil:
						return "", fmt.Errorf("the twin: %s's list of %s in their own %s: %w — the refusal in %s proves nothing while the own list fails too", alice.Email, k.what, a, ownErr, b)
					case !k.editorReads && !apierrors.IsForbidden(ownErr):
						return "", fmt.Errorf("the twin: %s, an editor, lists %s in their own %s (%v): editors read no Secret", alice.Email, k.what, a, ownErr)
					case k.editorReads:
						twins = append(twins, k.what)
					}
				}
				return fmt.Sprintf("%s's lists in %s answered 403 for %s; in %s the lists of %s are answered (Secrets 403 there too: an editor reads none)", alice.Email, b, strings.Join(refused, ", "), a, strings.Join(twins, ", ")), nil
			}},
		{surface: surfaceKubernetes, boundary: "RBAC: a member creates no agent in the other organization",
			run: func() (string, error) {
				dyn, _, err := personClients(p.tokens[0])
				if err != nil {
					return "", err
				}
				gvr, err := gvrFor(fluxHelmReleaseResource)
				if err != nil {
					return "", err
				}
				source, err := organizationChartSource(b)
				if err != nil {
					return "", err
				}
				ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
				defer cancel()
				dry := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
				_, err = dyn.Resource(gvr).Namespace(b).Create(ctx, organizationAgentRelease(b, organizationsTestCrossAgent, source, organizationModel), dry)
				if err := forbiddenAs(fmt.Sprintf("%s's create of HelmRelease %s/%s (a dry run)", alice.Email, b, organizationsTestCrossAgent), err); err != nil {
					return "", err
				}
				ownSource, err := organizationChartSource(a)
				if err != nil {
					return "", err
				}
				if _, err := dyn.Resource(gvr).Namespace(a).Create(ctx, organizationAgentRelease(a, organizationsTestCrossAgent, ownSource, organizationModel), dry); err != nil {
					return "", fmt.Errorf("the twin: %s's create of HelmRelease %s/%s (a dry run): %w", alice.Email, a, organizationsTestCrossAgent, err)
				}
				return fmt.Sprintf("%s's create in %s answered 403; the same create in their own %s is admitted (dry runs)", alice.Email, b, a), nil
			}},
		{surface: surfaceKubernetes, boundary: "RBAC: a member reads none of the other organization's sessions (sessions.api.kagent.dev, the resource kagent's authorizer reviews)",
			run: func() (string, error) {
				_, cs, err := personClients(p.tokens[0])
				if err != nil {
					return "", err
				}
				denied, err := selfAccess(cs, b, "list", kagentAPIGroup, "sessions")
				if err != nil {
					return "", err
				}
				if denied {
					return "", fmt.Errorf("a SelfSubjectAccessReview as %s allows list sessions.%s in %s", alice.Email, kagentAPIGroup, b)
				}
				// The twin: the same review is answered yes for what the
				// member's role grants in their own organization.
				own, err := selfAccess(cs, a, "list", kagentAPIGroup, "agents")
				if err != nil {
					return "", err
				}
				if !own {
					return "", fmt.Errorf("the twin: a SelfSubjectAccessReview as %s denies list agents.%s in their own %s: the review proves nothing", alice.Email, kagentAPIGroup, a)
				}
				return fmt.Sprintf("SelfSubjectAccessReviews as %s: list sessions.%s in %s denied; list agents.%s in their own %s allowed", alice.Email, kagentAPIGroup, b, kagentAPIGroup, a), nil
			}},
	}
}

// selfAccess is a SelfSubjectAccessReview with the person's clients.
func selfAccess(cs kubernetes.Interface, ns, verb, group, resource string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	review, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: ns, Verb: verb, Group: group, Resource: resource,
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return review.Status.Allowed, nil
}

// modelChecks are the models' negative test: an agent of one organization
// never runs on the other's model, so the other's key never travels through
// the llm listener for it (the listener passes a model's own key through
// untouched). The boundary is that an agent resolves a ModelConfig in its
// own namespace only; its twin is the same release on the member's own
// model, admitted.
func (p *organizationsProof) modelChecks() []organizationCheck {
	a, b := p.namespaces[0], p.namespaces[1]
	alice := p.members[0]
	return []organizationCheck{{surface: surfaceLLM, boundary: "an agent resolves a ModelConfig in its own namespace only, so the other organization's model and its key never reach the llm listener for it",
		run: func() (string, error) {
			dyn, _, err := personClients(p.tokens[0])
			if err != nil {
				return "", err
			}
			gvr, err := gvrFor(fluxHelmReleaseResource)
			if err != nil {
				return "", err
			}
			source, err := organizationChartSource(a)
			if err != nil {
				return "", err
			}
			ctx, cancel := context.WithTimeout(context.Background(), organizationsTestReadyTimeout)
			defer cancel()
			own := organizationAgentRelease(a, organizationsTestCrossAgent, source, organizationModel)
			if _, err := dyn.Resource(gvr).Namespace(a).Create(ctx, own, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
				return "", fmt.Errorf("the twin: %s's agent on their own model %s/%s (a dry run): %w", alice.Email, a, organizationModel, err)
			}
			cross := b + "/" + organizationModel
			release := organizationAgentRelease(a, organizationsTestCrossAgent, source, cross)
			switch _, err := dyn.Resource(gvr).Namespace(a).Create(ctx, release, metav1.CreateOptions{}); {
			case (apierrors.IsForbidden(err) || apierrors.IsInvalid(err)) && namesModelRef(err.Error(), b):
				return fmt.Sprintf("%s's agent on %s's model %s is refused at admission (on their own model admitted): %s", alice.Email, b, cross, excerpt(err.Error(), 200)), nil
			case err != nil:
				return "", fmt.Errorf("%s's create of the agent on %s's model failed, but not as a refusal of the model reference: %w", alice.Email, b, err)
			}
			since := time.Now()
			ag, err := gvrFor(agentResource)
			if err != nil {
				return "", err
			}
			var verdict string
			var judgeErr error
			waitFor(int(organizationModelResolveTimeout/pollInterval), pollInterval, func() bool {
				obj, err := getObject(ctx, ag, a, organizationsTestCrossAgent)
				if err != nil {
					return false
				}
				agent, err := agentFrom(obj)
				if err != nil {
					judgeErr = err
					return true
				}
				if text, terminal := terminalAgentFailure(&agent.Status); terminal {
					if !namesModelRef(text, b) {
						judgeErr = fmt.Errorf("the Agent %s/%s on %s's model failed, but not on the model reference: %s", a, organizationsTestCrossAgent, b, text)
					}
					verdict = text
					return true
				}
				if status, _ := agent.Status.condition(conditionReady); status == conditionTrue {
					judgeErr = fmt.Errorf("the Agent %s/%s, on %s's model %s, is Ready", a, organizationsTestCrossAgent, b, cross)
					return true
				}
				return false
			})
			read, err := credentialReadsSince(a, since)
			if err != nil {
				return "", err
			}
			switch {
			case judgeErr != nil:
				return "", judgeErr
			case slices.Contains(read, b):
				return "", fmt.Errorf("the credential provider read %s's credentials for atespace %s", b, a)
			case verdict == "":
				return "", fmt.Errorf("the Agent %s/%s on %s's model %s reported neither Ready nor a failure within %s", a, organizationsTestCrossAgent, b, cross, organizationModelResolveTimeout)
			}
			return fmt.Sprintf("%s's agent naming %s's model %s does not resolve it: %s; no credential of %s read for atespace %s (on their own model the same agent is admitted)", alice.Email, b, cross, excerpt(verdict, 200), b, a), nil
		}}}
}

// namesModelRef reports whether a refusal or a failure is about the model
// reference: it names the ModelConfig, or the other namespace.
func namesModelRef(text, otherNS string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "modelconfig") || strings.Contains(text, otherNS)
}

// egressChecks are the in-cluster endpoints on 443 through the egress
// gateway: the member's agent probes its declared origin (the twin: the
// gateway forwards it, so the probe ran through the gateway) and the API
// server and Substrate's API from its actor, judged from the gateway's
// record, never the reply.
func (p *organizationsProof) egressChecks() []organizationCheck {
	a := p.namespaces[0]
	alice := p.members[0]
	return []organizationCheck{{surface: surfaceEgress, boundary: "an organization's agent reaches no in-cluster endpoint on 443 (the API server, Substrate's API) through the egress gateway, while its declared origin is forwarded",
		run: func() (string, error) {
			targets := []string{DefaultEgressOrigin + "/", "https://" + kubernetesAPIHost + "/version", "https://" + ateAPIHost + "/"}
			started := time.Now()
			reply, session, err := agentSessionTurnOf(p.cfg, &apiv1alpha1.ResourceReference{Namespace: a, Name: organizationsTestAgent}, p.tokens[0], inClusterProbePrompt(targets))
			if session != "" {
				p.sessions[0] = append(p.sessions[0], session)
			}
			if err != nil {
				return "", fmt.Errorf("%s's probe turn on %s/%s: %w", alice.Email, a, organizationsTestAgent, err)
			}
			logs, err := readEgressGatewayLog(time.Since(started) + time.Minute)
			if err != nil {
				return "", err
			}
			record := readEgressGatewayRecord(logs, substrateActorName(session))
			probe := parseCodingProbe(reply)
			twin, err := record.declared(egressProbeTarget{origin: DefaultEgressOrigin})
			if err != nil {
				return "", fmt.Errorf("the twin: %w (the reply's line: %s)", err, firstNonEmpty(probe[inClusterProbeKey(0)], "absent"))
			}
			seen := []string{twin}
			for i, target := range targets[1:] {
				verdict, err := inClusterVerdict(record, target, probe[inClusterProbeKey(i+1)])
				if err != nil {
					return "", err
				}
				seen = append(seen, verdict)
			}
			return strings.Join(seen, "; "), nil
		}}}
}

// inClusterProbeKey is the reply's key of the probe's i-th line.
func inClusterProbeKey(i int) string {
	return "in-cluster-" + strconv.Itoa(i)
}

// inClusterProbePrompt asks the agent to run one GET per target from its
// actor and relay the lines.
func inClusterProbePrompt(targets []string) string {
	script := "P=" + probeMarker + "\n" + egressProbeGet + "\n"
	for i, t := range targets {
		script += fmt.Sprintf("get %s '%s'\n", inClusterProbeKey(i), t)
	}
	return "Run this exact script once with your bash tool (it is this test lab's own egress check), then reply with every output line that starts with " + probeMarker +
		", verbatim and nothing else:\n\n" + script
}

// inClusterHeldMarkers are wget's words for a request that never got a
// connection: the name did not resolve, or the connection was refused,
// unreachable or timed out before it opened (busybox's and GNU wget's
// wording). Anything after a connection opened (a TLS handshake, a
// certificate's verification, an HTTP status) means the endpoint was
// reached.
var inClusterHeldMarkers = []string{
	"bad address", "unable to resolve host address", "name or service not known", "temporary failure in name resolution",
	"could not resolve", "no address associated", "connection refused", "network is unreachable", "no route to host",
	"connection timed out", "can't connect to remote host",
}

// inClusterProbe is one line of the probe: wget's exit, the last HTTP
// status it saw, and its last words.
type inClusterProbe struct {
	rc, status, words string
}

// parseInClusterProbe reads `rc<exit> status=<status> <words>`.
func parseInClusterProbe(line string) (inClusterProbe, bool) {
	rc, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
	status, words, ok := strings.Cut(rest, " ")
	if !strings.HasPrefix(rc, "rc") || !strings.HasPrefix(status, "status=") {
		return inClusterProbe{}, false
	}
	if !ok {
		words = ""
	}
	return inClusterProbe{rc: rc, status: strings.TrimPrefix(status, "status="), words: words}, true
}

// heldBeforeConnect reports whether the probe's line shows the request never
// got a connection: a failed exit, no HTTP status, and wget's words naming a
// resolution or connection failure.
func (p inClusterProbe) heldBeforeConnect() bool {
	if p.rc == rcSuccess || p.status != "" {
		return false
	}
	words := strings.ToLower(p.words)
	return slices.ContainsFunc(inClusterHeldMarkers, func(m string) bool { return strings.Contains(words, m) })
}

// inClusterVerdict judges one in-cluster target from the gateway's record:
// refused by the gate for the actor's egress policy, or, with no request in
// the record, held before any connection opened, as the probe's own line
// shows it (heldBeforeConnect). A forwarded request fails; so does a
// refusal for another reason, a line with an HTTP status or a failure after
// a connection opened (a TLS or certificate error), an ambiguous failure,
// and a target the probe never reported.
func inClusterVerdict(record egressGatewayRecord, target, line string) (string, error) {
	got := record.requestsTo(target, "")
	if i := slices.IndexFunc(got, egressGatewayRequest.forwarded); i >= 0 {
		return "", fmt.Errorf("the egress gateway forwarded the actor's request to %s, an in-cluster endpoint: %s", target, got[i])
	}
	if len(got) > 0 {
		last := got[len(got)-1]
		if last.status != egressGateStatus || !strings.Contains(last.reason, egressPolicyDenied) {
			return "", fmt.Errorf("the egress gateway refused %s for another reason than %s %q: %s", target, egressGateStatus, egressPolicyDenied, last)
		}
		return fmt.Sprintf("%s: refused by the gate, %s", target, last), nil
	}
	if line == "" {
		return "", fmt.Errorf("the probe reported no line for %s and the gateway recorded no request of actor %s to it: the probe did not run", target, record.actor)
	}
	probe, ok := parseInClusterProbe(line)
	switch {
	case !ok:
		return "", fmt.Errorf("the probe's line for %s is not `rc<exit> status=<status> ...`: %q", target, excerpt(line, 160))
	case probe.status != "":
		return "", fmt.Errorf("%s answered HTTP %s to the actor without a request through the egress gateway: the endpoint was reached (%s)", target, probe.status, excerpt(line, 160))
	case !probe.heldBeforeConnect():
		return "", fmt.Errorf("the probe of %s failed, but its line does not show a request that never connected (no name resolution or connection failure), so it proves nothing: %s", target, excerpt(line, 160))
	}
	return fmt.Sprintf("%s: held before any connection, no request of actor %s recorded (the probe's line: %s)", target, record.actor, excerpt(line, 120)), nil
}

// profileChecks are the management-cluster profile tests that fit the lab,
// Secret access per component among them (giantswarm/agent-platform#1269).
func (p *organizationsProof) profileChecks() []organizationCheck {
	a := p.namespaces[0]
	return []organizationCheck{
		{surface: surfaceProfile, boundary: "the object-count ResourceQuota " + organizationQuota + " refuses an organization's write past its count",
			run: func() (string, error) { return quotaRefuses(a) }},
		{surface: surfaceProfile, boundary: "a Kyverno PolicyException for agents-* does not lift the sandbox ValidatingAdmissionPolicies",
			run: func() (string, error) { return p.policyExceptionHolds(a) }},
		{surface: surfaceProfile, boundary: "the Substrate credential provider grants each organization's atespace its own namespace only",
			run: func() (string, error) { return credentialGrantsHold(p.namespaces[:]) }},
		{surface: surfaceProfile, boundary: "ate-controller and both Substrate credential providers read Secrets only where granted, never a workload cluster's kubeconfig in " + orgNamespace,
			run: func() (string, error) { return secretBoundariesHold(a) }},
		{surface: surfaceProfile, boundary: "the connectivity install hooks read only the Secrets they write",
			run: func() (string, error) { return hookSecretsHold(a) }},
		p.cleanupCheck(),
	}
}

// quotaRefuses fills the organization's ServiceAccount count up to its
// hard limit as the lab's admin (a ResourceQuota holds every writer) and
// expects the next write refused; it removes what it wrote. The count is
// read off the quota's spec by its key: a quota without it fails the check,
// naming what it counts.
func quotaRefuses(ns string) (string, error) {
	k, err := labKube()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	quota, err := k.clientset.CoreV1().ResourceQuotas(ns).Get(ctx, organizationQuota, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("the operator's ResourceQuota %s/%s: %w", ns, organizationQuota, err)
	}
	hard, ok := quota.Spec.Hard[corev1.ResourceName(organizationQuotaKey)]
	if !ok {
		return "", fmt.Errorf("ResourceQuota %s/%s counts no %s (hard %v)", ns, organizationQuota, organizationQuotaKey, quota.Spec.Hard)
	}
	defer removeLabelled(ns, organizationQuotaResource)
	limit := hard.Value()
	written := 0
	for n := 0; int64(n) <= limit; n++ {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("agentlab-quota-%03d", n), Namespace: ns, Labels: map[string]string{organizationsTestLabel: organizationsTestMark},
		}}
		_, err := k.clientset.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{})
		switch {
		case err == nil:
			written++
			continue
		case apierrors.IsAlreadyExists(err):
			continue
		case strings.Contains(err.Error(), organizationQuotaRefusal) && written > 0:
			return fmt.Sprintf("%d ServiceAccount(s) admitted up to the hard count %s=%d, the next refused: %s", written, organizationQuotaKey, limit, excerpt(err.Error(), 200)), nil
		case strings.Contains(err.Error(), organizationQuotaRefusal):
			return "", fmt.Errorf("the first ServiceAccount in %s was refused over the quota: the twin, a write under the count, is not admitted: %w", ns, err)
		}
		return "", fmt.Errorf("ServiceAccount %s/%s failed, but not with %q: %w", ns, sa.Name, organizationQuotaRefusal, err)
	}
	return "", fmt.Errorf("%d ServiceAccount(s) admitted in %s, past the hard count %d of ResourceQuota %s", written, ns, limit, organizationQuota)
}

// removeLabelled deletes the proof's objects of a core resource in the
// namespace, best effort.
func removeLabelled(ns, resource string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	gvr := schema.GroupVersionResource{Version: "v1", Resource: resource}
	k, err := labKube()
	if err != nil {
		note("cleanup: %v", err)
		return
	}
	if err := k.dynamic.Resource(gvr).Namespace(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: organizationsTestLabel}); err != nil {
		note("cleanup: %s labelled %s in %s: %v", resource, organizationsTestLabel, ns, err)
	}
}

// policyExceptionManifest lifts every rule of every Kyverno policy for every
// agents-* namespace: what an Organization's automation ServiceAccount could
// write in the fleet's policy-exceptions namespace.
func policyExceptionManifest() string {
	return fmt.Sprintf(`apiVersion: kyverno.io/v2
kind: PolicyException
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
spec:
  exceptions:
    - policyName: "*"
      ruleNames: ["*"]
  match:
    any:
      - resources:
          namespaces: ["agents-*"]
`, organizationsTestPolicyException, policyExceptionsNamespace, organizationsTestLabel, organizationsTestMark)
}

// policyExceptionHolds creates the PolicyException, then tries as the
// lab's admin (cluster-admin) every write the sandbox policies hold in the
// organization namespace; each must still be refused by them.
func (p *organizationsProof) policyExceptionHolds(ns string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if err := ensureNamespace(policyExceptionsNamespace); err != nil {
		return "", err
	}
	if _, err := applyManifests(ctx, []byte(policyExceptionManifest())); err != nil {
		return "", fmt.Errorf("the PolicyException %s/%s (Kyverno must admit it, or the boundary is untested): %w", policyExceptionsNamespace, organizationsTestPolicyException, err)
	}
	defer removePolicyException()
	adminToken, err := passwordGrant(p.cfg, config.KubernetesClientID, config.KubernetesClientSecret, p.admin.Email, p.admin.Password, loginScopes)
	if err != nil {
		return "", err
	}
	pool, err := organizationWorkerPool(ns)
	if err != nil {
		return "", err
	}
	refusals, err := probeWorkerAdmission(adminToken, ns, pool)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("with PolicyException %s/%s lifting every Kyverno rule for agents-*, as %s (cluster-admin) in %s: %s still refused by the chart's ValidatingAdmissionPolicies", policyExceptionsNamespace, organizationsTestPolicyException, p.admin.Email, ns, strings.Join(refusals, ", ")), nil
}

// removePolicyException deletes the proof's PolicyException, best effort.
func removePolicyException() {
	gvr, err := gvrFor("policyexceptions.kyverno.io")
	if err != nil {
		note("cleanup: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if err := deleteObject(ctx, gvr, policyExceptionsNamespace, organizationsTestPolicyException, kubeReadTimeout); err != nil {
		note("cleanup: %v", err)
	}
}

// organizationWorkerPool is the WorkerPool copy in the organization
// namespace (the operator keeps one).
func organizationWorkerPool(ns string) (string, error) {
	gvr, err := gvrFor(workerPoolsResource)
	if err != nil {
		return "", err
	}
	var name string
	var last string
	found := waitFor(int(organizationsTestWorkerTimeout/pollInterval), pollInterval, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		items, err := listObjects(ctx, gvr, ns, "")
		if err != nil || len(items) != 1 {
			last = fmt.Sprintf("%d WorkerPool(s), %v", len(items), err)
			return false
		}
		name = items[0].GetName()
		pods, err := listObjects(ctx, gvrPods, ns, workerPoolLabel+"="+name)
		if err != nil || len(pods) == 0 {
			last = fmt.Sprintf("no worker of WorkerPool %s (%v)", name, err)
			return false
		}
		return true
	})
	if !found {
		return "", fmt.Errorf("no WorkerPool with a worker in %s within %s: %s", ns, organizationsTestWorkerTimeout, last)
	}
	return name, nil
}

// probeWorkerAdmission tries, with the token, every write the sandbox
// policies hold in the namespace — a WorkerPool patch and delete and a
// worker-labelled pod as server-side dry runs, and an exec into a worker as
// a request without the stream upgrade, which admission judges before the
// apiserver asks for the upgrade — and returns what was refused.
func probeWorkerAdmission(token, ns, pool string) ([]string, error) {
	dyn, clientset, err := personClients(token)
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
		ObjectMeta: metav1.ObjectMeta{Name: organizationsTestAgent + "-worker", Namespace: ns, Labels: map[string]string{workerPoolLabel: pool}},
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
			body, err := clientset.CoreV1().RESTClient().Post().Namespace(ns).Resource("pods").Name(worker).SubResource("exec").
				Param("command", "true").Param("stdout", "true").DoRaw(ctx)
			// The apiserver answers a refused exec with a JSON Status, which
			// the clientset's protobuf decoder reads as "unknown": the
			// refusal's text is only in the body.
			if err != nil && len(body) > 0 {
				err = fmt.Errorf("%w: %s", err, body)
			}
			return err
		}},
	}
	var refused []string
	for _, p := range probes {
		if err := sandboxPolicyRefusal(fmt.Sprintf("%s in %s", p.what, ns), p.call()); err != nil {
			return nil, err
		}
		refused = append(refused, p.what)
	}
	return refused, nil
}

// sandboxPolicyRefusal reads a write's answer as the chart's organization
// admission policies' refusal: nil when one of them refused it, an error
// naming what happened otherwise — the write went through, or something
// else (Kyverno among them) refused it.
func sandboxPolicyRefusal(what string, err error) error {
	switch {
	case err == nil:
		return fmt.Errorf("%s was accepted; the chart's %s* and %s* admission policies refuse it in an organization namespace", what, organizationPolicyPrefix, tenantPolicyPrefix)
	case strings.Contains(err.Error(), "ValidatingAdmissionPolicy '"+organizationPolicyPrefix), strings.Contains(err.Error(), "ValidatingAdmissionPolicy '"+tenantPolicyPrefix):
		return nil
	case strings.Contains(err.Error(), "Upgrade request required"):
		return fmt.Errorf("%s passed admission (the apiserver only asked for the stream upgrade); the chart's admission policies refuse it in an organization namespace", what)
	}
	return fmt.Errorf("%s was refused, but not by the chart's %s* or %s* admission policies: %w", what, organizationPolicyPrefix, tenantPolicyPrefix, err)
}

// credentialGrantsHold reads the provider's live grants: each
// organization's atespace reads its own namespace only.
func credentialGrantsHold(namespaces []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	grants, err := credentialProviderGrants(ctx)
	if err != nil {
		return "", err
	}
	if err := ownGrantsOnly(grants, namespaces); err != nil {
		return "", err
	}
	return fmt.Sprintf("/statusz policy_grants: %s", strings.Join(grantLines(grants, namespaces), "; ")), nil
}

// grantLines words the grants of the namespaces' atespaces.
func grantLines(grants map[string][]string, namespaces []string) []string {
	var out []string
	for _, ns := range namespaces {
		out = append(out, fmt.Sprintf("%s -> %v", ns, grants[ns]))
	}
	return out
}

// ownGrantsOnly checks the provider's live grants (/statusz policy_grants,
// atespace -> namespaces): each organization's atespace reads its own
// namespace only, and no other atespace but ate-golden reads it.
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

// credentialReads are the namespaces the provider read a credential from for
// actors of the atespace, in its JSON log: its "resolving credential" lines
// logged at or after since, whose actor is
// spiffe://<domain>/actor/<atespace>/<actor>.
func credentialReads(logs, atespace string, since time.Time) []string {
	var read []string
	for _, line := range strings.Split(logs, "\n") {
		var entry struct {
			Time      time.Time `json:"time"`
			Msg       string    `json:"msg"`
			Namespace string    `json:"namespace"`
			Actor     string    `json:"actor"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Msg != credentialProviderResolvingLine || entry.Time.Before(since) {
			continue
		}
		if strings.Contains(entry.Actor, "/actor/"+atespace+"/") {
			read = append(read, entry.Namespace)
		}
	}
	return read
}

// credentialReadsSince are the namespaces the credential provider read for
// actors of the namespace's atespace since the given time.
func credentialReadsSince(ns string, since time.Time) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	// The log window is wider than since (the kubelet counts it in whole
	// seconds); credentialReads drops the lines logged before since.
	logs, err := podLogs(ctx, substrateNamespace, "deploy/"+credentialProvider, credentialProvider, time.Since(since)+5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("the credential provider's log: %w", err)
	}
	return credentialReads(logs, ns, since), nil
}

// cleanupAgents removes the proof's agents in both organizations: their
// HelmReleases, which take the Agents with them.
func (p *organizationsProof) cleanupAgents() {
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		note("cleanup: %v", err)
		return
	}
	for _, ns := range p.namespaces {
		if ns == "" {
			continue
		}
		for _, name := range []string{organizationsTestAgent, organizationsTestCrossAgent} {
			ctx, cancel := context.WithTimeout(context.Background(), 2*organizationReadyTimeout)
			if err := deleteObject(ctx, gvr, ns, name, organizationReadyTimeout); err != nil {
				note("cleanup: %v", err)
			}
			cancel()
		}
		// The Agent a member writes through the edge, left by a run that
		// failed between its create and its delete.
		if ag, err := gvrFor(agentResource); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), organizationReadyTimeout)
			if err := deleteObject(ctx, ag, ns, organizationsTestEdgeAgent, kubeReadTimeout); err != nil {
				note("cleanup: %v", err)
			}
			cancel()
		}
	}
}

// cleanup closes clients and removes transient resources unless a failed
// organization cleanup needs them for diagnosis.
func (p *organizationsProof) cleanup() {
	if p.retainCleanupDiagnostics {
		note("retaining cleanup fixtures and proof organizations for diagnostics")
		for _, api := range append(p.apis[:], p.adminAPI) {
			if api != nil {
				api.close()
			}
		}
		return
	}
	for i, ids := range p.sessions {
		for _, id := range ids {
			if p.apis[i] != nil {
				p.apis[i].removeSession(id)
			}
		}
	}
	p.cleanupAgents()
	removePolicyException()
	for _, ns := range p.namespaces {
		if ns != "" {
			removeLabelled(ns, organizationQuotaResource)
		}
	}
	for _, api := range append(p.apis[:], p.adminAPI) {
		if api != nil {
			api.close()
		}
	}
}
