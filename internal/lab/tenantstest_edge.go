package lab

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The edge half of tenants-test (giantswarm/agentlab#166): the kagent
// controller's Kubernetes RBAC authorizer (giantswarm/kagent-upstream#571 and
// #633, agent-platform's kagent.controller.authorization) judged through the
// agentgateway edge, as the Dev Portal and Slack call it, by people whose
// only access to an organization is a RoleBinding of their token's group.
const (
	// tenantUserRole is the chart's ClusterRole that lets a person see a
	// tenant's agents and start conversations with them.
	tenantUserRole = "agent-platform:user"
	// tenantUsersBinding names the RoleBinding of a tenant group to
	// tenantUserRole in an organization namespace.
	tenantUsersBinding = "agentlab-tenants-test-users"
	// tenantsTestPlatformAgent is the platform tenant's agent tenants-test
	// creates in the kagent namespace, so the platform tenant lists one.
	tenantsTestPlatformAgent = "agentlab-tenants-platform"
	// tenantsTestEdgeAgent is the Agent the editor writes through the edge:
	// on a Harness that does not exist, so it never boots.
	tenantsTestEdgeAgent = "agentlab-tenants-edge"
	// tenantsTestEdgeLabel is the label the editor's change adds.
	tenantsTestEdgeLabel = "agentlab.giantswarm.io/tenants-test-change"
	// tenantRBACSettle bounds how long a new RoleBinding takes to reach the
	// controller's answers: its decisions are cached for a few seconds.
	tenantRBACSettle = 45 * time.Second
	// authorizationModeKubernetes is the controller's RBAC authorizer mode.
	authorizationModeKubernetes = "kubernetes"
	// tenantsTestGroupsClaim is the claim the lab's Dex tokens carry the
	// groups in, as the API server reads it (--oidc-groups-claim).
	tenantsTestGroupsClaim = "groups"
	// tenantsTestEdgeChange is the change label's value.
	tenantsTestEdgeChange = "changed"
	// agentKind is the kind of a kagent Agent.
	agentKind = "Agent"
)

// The kagent controller's ConfigMap keys of its authorizer.
const (
	authorizationModeKey           = "KAGENT_AUTHORIZATION_MODE"
	authorizationUsernamePrefixKey = "KAGENT_AUTHORIZATION_USERNAME_PREFIX"
	authorizationGroupsClaimKey    = "KAGENT_AUTHORIZATION_GROUPS_CLAIM"
	authorizationGroupsPrefixKey   = "KAGENT_AUTHORIZATION_GROUPS_PREFIX"
)

// controllerAuthorization is the kagent controller's authorizer as its
// ConfigMap configures it.
type controllerAuthorization struct {
	mode, usernamePrefix, groupsClaim, groupsPrefix string
}

// controllerAuthorizationFrom reads the KAGENT_AUTHORIZATION_* keys; a key
// that is not there is the controller's default (noop, no claim, no prefix).
func controllerAuthorizationFrom(data map[string]string) controllerAuthorization {
	return controllerAuthorization{
		mode:           data[authorizationModeKey],
		usernamePrefix: data[authorizationUsernamePrefixKey],
		groupsClaim:    data[authorizationGroupsClaimKey],
		groupsPrefix:   data[authorizationGroupsPrefixKey],
	}
}

// problem words what keeps the authorizer from judging the lab's people as
// the API server does, "" when nothing does: the kubernetes mode, the groups
// read from the lab's claim, and the lab API server's oidc: prefixes.
func (a controllerAuthorization) problem() string {
	switch {
	case a.mode != authorizationModeKubernetes:
		return fmt.Sprintf("the kagent controller runs authorization mode %q, not %s: every signed-in person sees and runs every tenant's agents. It needs kagent 1.19 or later with kagent.controller.authorization.mode kubernetes — agent-platform 4.151 or later (`agentlab configure --chart-version <version>`, then `agentlab platform`)",
			cmp.Or(a.mode, "noop"), authorizationModeKubernetes)
	case a.groupsClaim != tenantsTestGroupsClaim:
		return fmt.Sprintf("the kagent controller reads the groups from claim %q, not %q: a tenant bound by group is refused (kagent.controller.authorization.groupsClaim)", a.groupsClaim, tenantsTestGroupsClaim)
	case a.usernamePrefix != apiserverUserPrefix || a.groupsPrefix != apiserverUserPrefix:
		return fmt.Sprintf("the kagent controller reviews users with prefix %q and groups with %q, the lab's API server binds both with %q: `agentlab platform` with this agentlab renders kagent.controller.authorization.usernamePrefix and groupsPrefix",
			a.usernamePrefix, a.groupsPrefix, apiserverUserPrefix)
	}
	return ""
}

// readControllerAuthorization reads the authorizer's settings off the kagent
// controller's ConfigMap.
func readControllerAuthorization() (controllerAuthorization, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	cm, err := getObject(ctx, gvrConfigMaps, kagentNamespace, kagentControllerConfigMap)
	if err != nil {
		return controllerAuthorization{}, fmt.Errorf("the kagent controller's ConfigMap %s/%s: %w", kagentNamespace, kagentControllerConfigMap, err)
	}
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	return controllerAuthorizationFrom(data), nil
}

// tenantsTestUsers are the two tenant users, one per organization: the
// first user in each tenant group. Neither may hold a cluster-wide read (a
// platform admin or a viewer), which would show them every tenant.
func tenantsTestUsers(cfg *config.Config) ([2]*config.User, error) {
	var users [2]*config.User
	for i, group := range []string{config.GroupTenantA, config.GroupTenantB} {
		u := cfg.FindUserInGroup(group)
		if u == nil {
			return users, fmt.Errorf("no user in group %s in %s: tenants-test binds that group to %s in one organization — `agentlab configure --tenants` adds %s and %s, then `agentlab reload`",
				group, config.File, tenantUserRole, config.TenantUsers[0].Email, config.TenantUsers[1].Email)
		}
		if u.HasGroup(platformAdminsGroup) || u.HasGroup(viewerGroup) {
			return users, fmt.Errorf("%s (group %s) is also in platform-admins or viewers, which read every namespace: the proof needs a person whose only access is the tenant group", u.Email, group)
		}
		users[i] = u
	}
	if users[0].Email == users[1].Email {
		return users, fmt.Errorf("%s is in both %s and %s: the proof needs one person per organization", users[0].Email, config.GroupTenantA, config.GroupTenantB)
	}
	return users, nil
}

// bindTenantGroup binds the tenant group to agent-platform:user in the
// organization namespace: the RoleBinding names the group as the API server
// spells it (oidc:<group>), never the person.
func bindTenantGroup(ns, group string) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	_, err := applyTyped(ctx, &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: tenantUsersBinding, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: tenantUserRole},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: apiserverUserPrefix + group}},
	})
	return err
}

// isPermissionDenied reports whether the controller refused a call for want
// of RBAC: PermissionDenied on a kagent service call, the A2A error the SDK
// maps it to on an A2A call.
func isPermissionDenied(err error) bool {
	return status.Code(err) == codes.PermissionDenied || errors.Is(err, a2a.ErrUnauthorized)
}

// wantDenied reads a call's answer as the authorizer's refusal: nil when the
// controller refused it with PermissionDenied, an error naming what happened
// otherwise.
func wantDenied(what string, err error) error {
	switch {
	case err == nil:
		return fmt.Errorf("%s was accepted, want PermissionDenied", what)
	case isPermissionDenied(err):
		return nil
	default:
		return fmt.Errorf("%s failed, but not with PermissionDenied: %w", what, err)
	}
}

// refSet is a set of agents as namespace/name.
type refSet map[string]bool

func (s refSet) String() string {
	if len(s) == 0 {
		return "no agent"
	}
	return strings.Join(slices.Sorted(maps.Keys(s)), ", ")
}

// refsOf is the set of the listed agents.
func refsOf(agents []*apiv1alpha1.Agent) refSet {
	set := refSet{}
	for _, a := range agents {
		set[agentTenant(a.GetRef())] = true
	}
	return set
}

// rosterDiff words how a listed roster differs from the wanted one, "" when
// they are the same.
func rosterDiff(got, want refSet) string {
	var missing, extra []string
	for ref := range want {
		if !got[ref] {
			missing = append(missing, ref)
		}
	}
	for ref := range got {
		if !want[ref] {
			extra = append(extra, ref)
		}
	}
	var parts []string
	if len(missing) > 0 {
		slices.Sort(missing)
		parts = append(parts, "missing "+strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		slices.Sort(extra)
		parts = append(parts, "not theirs "+strings.Join(extra, ", "))
	}
	return strings.Join(parts, "; ")
}

// agentsIn reads the Agents of the namespaces with the lab's own kubeconfig,
// as namespace/name: what a person bound to them should list.
func agentsIn(namespaces ...string) (refSet, error) {
	gvr, err := gvrFor(agentResource)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	set := refSet{}
	for _, ns := range namespaces {
		items, err := listObjects(ctx, gvr, ns, "")
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			set[ns+"/"+item.GetName()] = true
		}
	}
	return set, nil
}

// listAgentsIn is AgentService/ListAgents of one namespace, or of every
// namespace the caller may list when ns is empty — the roster call of
// klaus-gateway and the portal across tenants.
func (a *kagentAPI) listAgentsIn(ctx context.Context, ns string) ([]*apiv1alpha1.Agent, error) {
	resp, err := a.agents.ListAgents(a.callCtx(ctx), &apiv1alpha1.ListAgentsRequest{Namespace: ns})
	if err != nil {
		return nil, fmt.Errorf("ListAgents: %w", err)
	}
	return resp.GetAgents(), nil
}

// waitRoster lists the caller's agents across namespaces until the list is
// the wanted one, bounded by tenantRBACSettle (the controller caches its
// decisions), and returns the difference left at the deadline.
func waitRoster(api *kagentAPI, want refSet) (refSet, string, error) {
	var got refSet
	var diff string
	var callErr error
	waitFor(int(tenantRBACSettle/pollInterval), pollInterval, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		agents, err := api.listAgentsIn(ctx, "")
		if callErr = err; err != nil {
			return false
		}
		got = refsOf(agents)
		diff = rosterDiff(got, want)
		return diff == ""
	})
	if callErr != nil {
		return nil, "", callErr
	}
	return got, diff, nil
}

// agentResourceOf is an Agent as the controller's write calls take it.
func agentResourceOf(obj map[string]any) (*apiv1alpha1.StructuredObject, error) {
	value, err := structpb.NewStruct(obj)
	if err != nil {
		return nil, err
	}
	return &apiv1alpha1.StructuredObject{ApiVersion: kagentAPIVersion, Kind: agentKind, Value: value}, nil
}

// edgeAgent is the Agent the editor writes: the template of the
// namespace's agent on a Harness that does not exist, so nothing boots, and
// labels when given.
func edgeAgent(ns, template string, labels map[string]string) map[string]any {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion(kagentAPIVersion)
	u.SetKind(agentKind)
	u.SetNamespace(ns)
	u.SetName(tenantsTestEdgeAgent)
	if len(labels) > 0 {
		u.SetLabels(labels)
	}
	_ = unstructured.SetNestedField(u.Object, template, crSpec, "templateRef", nameKey)
	_ = unstructured.SetNestedField(u.Object, klausGatewayNoHarness, crSpec, "harnessRef", nameKey)
	return u.Object
}

// createAgent, updateAgent and deleteAgent are AgentService's writes.
func (a *kagentAPI) createAgent(ctx context.Context, obj map[string]any) error {
	res, err := agentResourceOf(obj)
	if err != nil {
		return err
	}
	ref := refOfObject(obj)
	_, err = a.agents.CreateAgent(a.callCtx(ctx), &apiv1alpha1.CreateAgentRequest{Ref: ref, Resource: res})
	return err
}

func (a *kagentAPI) updateAgent(ctx context.Context, obj map[string]any) error {
	res, err := agentResourceOf(obj)
	if err != nil {
		return err
	}
	_, err = a.agents.UpdateAgent(a.callCtx(ctx), &apiv1alpha1.UpdateAgentRequest{Ref: refOfObject(obj), Resource: res})
	return err
}

func (a *kagentAPI) deleteAgent(ctx context.Context, ref *apiv1alpha1.ResourceReference) error {
	_, err := a.agents.DeleteAgent(a.callCtx(ctx), &apiv1alpha1.DeleteAgentRequest{Ref: ref})
	return err
}

// refOfObject is an object's namespace and name.
func refOfObject(obj map[string]any) *apiv1alpha1.ResourceReference {
	u := unstructured.Unstructured{Object: obj}
	return &apiv1alpha1.ResourceReference{Namespace: u.GetNamespace(), Name: u.GetName()}
}

// allCreatorsSessions is ListSessions with all_creators narrowed to an
// Agent: every creator's conversations of it, which only a holder of a
// cluster-wide list on sessions may read.
func (a *kagentAPI) allCreatorsSessions(ctx context.Context, agent *apiv1alpha1.ResourceReference) ([]*apiv1alpha1.Session, error) {
	resp, err := a.sessions.ListSessions(a.callCtx(ctx), &apiv1alpha1.ListSessionsRequest{
		Page: &apiv1alpha1.PageRequest{Limit: listPageLimit}, Agent: agent, AllCreators: true,
	})
	if err != nil {
		return nil, err
	}
	return resp.GetSessions(), nil
}

// firstMessageTo sends a first A2A message to an Agent with no Session — a
// conversation the controller starts for the caller — and returns the
// stream's first error.
func (a *kagentAPI) firstMessageTo(ctx context.Context, agent *apiv1alpha1.ResourceReference, prompt string) error {
	for _, err := range a.a2a.SendStreamingMessage(a.a2aCtx(ctx), &a2a.SendMessageRequest{Tenant: agentTenant(agent), Message: userMessage(prompt)}) {
		if err != nil {
			return err
		}
	}
	return nil
}

// tenantsEdge is what the edge half works with: the two organization
// namespaces, their tenant users and editors with their kagent API clients,
// and the admin's (cluster-admin) client.
type tenantsEdge struct {
	namespaces [2]string
	users      [2]*config.User
	userAPIs   [2]*kagentAPI
	editors    [2]*config.User
	editorAPIs [2]*kagentAPI
	admin      *kagentAPI
}

// edgeVerdicts are the PASS lines of the edge half.
type edgeVerdicts []string

// run is the edge half, steps E1 to E6.
func (e *tenantsEdge) run() (edgeVerdicts, error) {
	var out edgeVerdicts
	a, b := e.namespaces[0], e.namespaces[1]
	alice, bob := e.users[0], e.users[1]
	aliceAPI := e.userAPIs[0]
	agentA := &apiv1alpha1.ResourceReference{Namespace: a, Name: tenantsTestAgent}
	agentB := &apiv1alpha1.ResourceReference{Namespace: b, Name: tenantsTestAgent}

	platform, err := agentsIn(kagentNamespace)
	if err != nil {
		return nil, err
	}
	step("E1. %s, in no organization yet (group %s bound nowhere): ListAgents across namespaces lists the platform tenant only (%s)", alice.Email, config.GroupTenantA, platform)
	got, diff, err := waitRoster(aliceAPI, platform)
	if err != nil {
		return nil, fmt.Errorf("E1: %s's ListAgents: %w", alice.Email, err)
	}
	if diff != "" {
		return nil, fmt.Errorf("E1: %s, bound to no organization, lists %s; want the platform tenant's %s only (%s)", alice.Email, got, platform, diff)
	}
	out = append(out, fmt.Sprintf("PASS: %s, in no organization, lists the platform tenant only (%s)", alice.Email, got))

	step("E2. Group %s bound to %s in %s and %s in %s: each person lists their organization's agents and the platform tenant's, nothing of another organization", config.GroupTenantA, tenantUserRole, a, config.GroupTenantB, b)
	for i, group := range []string{config.GroupTenantA, config.GroupTenantB} {
		if err := bindTenantGroup(e.namespaces[i], group); err != nil {
			return nil, err
		}
	}
	for i, api := range e.userAPIs {
		own, err := agentsIn(kagentNamespace, e.namespaces[i])
		if err != nil {
			return nil, err
		}
		got, diff, err := waitRoster(api, own)
		if err != nil {
			return nil, fmt.Errorf("E2: %s's ListAgents: %w", e.users[i].Email, err)
		}
		if diff != "" {
			return nil, fmt.Errorf("E2: %s (group %s, bound in %s) lists %s after %s; want %s (%s)", e.users[i].Email, e.users[i].Groups, e.namespaces[i], got, tenantRBACSettle, own, diff)
		}
		note("%s lists %s", e.users[i].Email, got)
	}
	out = append(out, fmt.Sprintf("PASS: ListAgents through the edge lists %s exactly %s's agents and the platform tenant's, %s exactly %s's and the platform tenant's — by the group in their token alone", alice.Email, a, bob.Email, b))

	step("E3. %s on %s's agent: CreateSession and a first A2A message are PermissionDenied, no Session is made and no turn reaches the model; on their own organization's agent CreateSession succeeds", alice.Email, b)
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	defer cancel()
	before, err := e.admin.allCreatorsSessions(ctx, agentB)
	if err != nil {
		return nil, fmt.Errorf("E3: ListSessions all_creators on %s as the admin (cluster-admin): %w", agentTenant(agentB), err)
	}
	since := time.Now()
	_, err = aliceAPI.sessions.CreateSession(aliceAPI.callCtx(ctx), &apiv1alpha1.CreateSessionRequest{Agent: agentB, RequestId: uuid.NewString()})
	if err := wantDenied(fmt.Sprintf("%s's CreateSession on %s", alice.Email, agentTenant(agentB)), err); err != nil {
		return nil, fmt.Errorf("E3: %w", err)
	}
	if err := wantDenied(fmt.Sprintf("%s's first A2A message to %s", alice.Email, agentTenant(agentB)), aliceAPI.firstMessageTo(ctx, agentB, tenantsTestPrompt)); err != nil {
		return nil, fmt.Errorf("E3: %w", err)
	}
	after, err := e.admin.allCreatorsSessions(ctx, agentB)
	if err != nil {
		return nil, err
	}
	if len(after) != len(before) {
		return nil, fmt.Errorf("E3: %s holds %d Session(s) after the refused calls, %d before", agentTenant(agentB), len(after), len(before))
	}
	read, err := tenantCredentialReads(b, since)
	if err != nil {
		return nil, err
	}
	if len(read) > 0 {
		return nil, fmt.Errorf("E3: the credential provider read %v for atespace %s during the refused calls: a turn reached the model", read, b)
	}
	session, err := aliceAPI.createSessionOf(ctx, agentA, uuid.NewString())
	if err != nil {
		return nil, fmt.Errorf("E3: %s's CreateSession on their own organization's %s: %w", alice.Email, agentTenant(agentA), err)
	}
	aliceAPI.removeSession(session.GetId())
	out = append(out, fmt.Sprintf("PASS: %s's CreateSession and first A2A message on %s are PermissionDenied: no Session of it (%d before and after), no credential read for its atespace; their CreateSession on %s succeeds", alice.Email, agentTenant(agentB), len(before), agentTenant(agentA)))

	step("E4. %s (a user) writes nothing: CreateAgent in their own organization %s is PermissionDenied", alice.Email, a)
	if err := wantDenied(fmt.Sprintf("%s's CreateAgent %s/%s", alice.Email, a, tenantsTestEdgeAgent), aliceAPI.createAgent(ctx, edgeAgent(a, tenantsTestAgent, nil))); err != nil {
		return nil, fmt.Errorf("E4: %w", err)
	}
	out = append(out, fmt.Sprintf("PASS: %s, a user of %s, cannot create an Agent there (PermissionDenied)", alice.Email, a))

	editor, editorAPI := e.editors[0], e.editorAPIs[0]
	step("E5. %s (editor of %s) creates, changes and deletes %s there through the edge; in %s the same writes are PermissionDenied", editor.Email, a, tenantsTestEdgeAgent, b)
	if err := editorAPI.createAgent(ctx, edgeAgent(a, tenantsTestAgent, nil)); err != nil {
		return nil, fmt.Errorf("E5: %s's CreateAgent %s/%s: %w", editor.Email, a, tenantsTestEdgeAgent, err)
	}
	// The controller writes the update over its cached read of the Agent,
	// which a create a moment ago leaves stale: the API server's conflict
	// comes back as Internal (kagent's kubecrud Update), so a few tries.
	var updateErr error
	waitFor(int(tenantRBACSettle/pollInterval), pollInterval, func() bool {
		updateErr = editorAPI.updateAgent(ctx, edgeAgent(a, tenantsTestAgent, map[string]string{tenantsTestEdgeLabel: tenantsTestEdgeChange}))
		return updateErr == nil || status.Code(updateErr) != codes.Internal
	})
	if updateErr != nil {
		return nil, fmt.Errorf("E5: %s's UpdateAgent %s/%s: %w", editor.Email, a, tenantsTestEdgeAgent, updateErr)
	}
	changed, err := readEdgeAgentLabel(a)
	if err != nil {
		return nil, err
	}
	if changed != tenantsTestEdgeChange {
		return nil, fmt.Errorf("E5: the update was accepted but Agent %s/%s carries %s=%q", a, tenantsTestEdgeAgent, tenantsTestEdgeLabel, changed)
	}
	if err := editorAPI.deleteAgent(ctx, &apiv1alpha1.ResourceReference{Namespace: a, Name: tenantsTestEdgeAgent}); err != nil {
		return nil, fmt.Errorf("E5: %s's DeleteAgent %s/%s: %w", editor.Email, a, tenantsTestEdgeAgent, err)
	}
	other, err := getAgentObject(b, tenantsTestAgent)
	if err != nil {
		return nil, err
	}
	otherChanged := other.DeepCopy()
	otherChanged.SetLabels(map[string]string{tenantsTestEdgeLabel: tenantsTestEdgeChange})
	for _, w := range []struct {
		what string
		call func() error
	}{
		{"CreateAgent " + b + "/" + tenantsTestEdgeAgent, func() error { return editorAPI.createAgent(ctx, edgeAgent(b, tenantsTestAgent, nil)) }},
		{"UpdateAgent " + agentTenant(agentB), func() error { return editorAPI.updateAgent(ctx, otherChanged.Object) }},
		{"DeleteAgent " + agentTenant(agentB), func() error { return editorAPI.deleteAgent(ctx, agentB) }},
	} {
		if err := wantDenied(editor.Email+"'s "+w.what, w.call()); err != nil {
			return nil, fmt.Errorf("E5: %w", err)
		}
	}
	still, err := getAgentObject(b, tenantsTestAgent)
	if err != nil {
		return nil, fmt.Errorf("E5: %s after the refused delete: %w", agentTenant(agentB), err)
	}
	if _, ok := still.GetLabels()[tenantsTestEdgeLabel]; ok {
		return nil, fmt.Errorf("E5: %s carries %s although the update was refused", agentTenant(agentB), tenantsTestEdgeLabel)
	}
	out = append(out, fmt.Sprintf("PASS: %s, editor of %s, creates, changes and deletes an Agent there through the edge; CreateAgent, UpdateAgent and DeleteAgent in %s are PermissionDenied and its agent is unchanged", editor.Email, a, b))

	step("E6. ListSessions with all_creators: PermissionDenied for %s and %s, answered for the admin (cluster-admin, a cluster-wide list on sessions)", alice.Email, bob.Email)
	for i, api := range e.userAPIs {
		_, err := api.allCreatorsSessions(ctx, nil)
		if err := wantDenied(e.users[i].Email+"'s ListSessions all_creators", err); err != nil {
			return nil, fmt.Errorf("E6: %w", err)
		}
	}
	if _, err := e.admin.allCreatorsSessions(ctx, nil); err != nil {
		return nil, fmt.Errorf("E6: the admin's ListSessions all_creators: %w", err)
	}
	out = append(out, fmt.Sprintf("PASS: ListSessions all_creators is PermissionDenied for %s and %s and answered for the cluster-admin", alice.Email, bob.Email))
	return out, nil
}

// getAgentObject reads an Agent with the lab's own kubeconfig.
func getAgentObject(ns, name string) (*unstructured.Unstructured, error) {
	gvr, err := gvrFor(agentResource)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	return getObject(ctx, gvr, ns, name)
}

// readEdgeAgentLabel is the change label on the editor's Agent.
func readEdgeAgentLabel(ns string) (string, error) {
	obj, err := getAgentObject(ns, tenantsTestEdgeAgent)
	if err != nil {
		return "", fmt.Errorf("the editor's Agent %s/%s: %w", ns, tenantsTestEdgeAgent, err)
	}
	return obj.GetLabels()[tenantsTestEdgeLabel], nil
}
