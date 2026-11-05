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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The kagent API surface of organizations-test (the edge half of the old
// tenants proof, giantswarm/agentlab#166): the kagent controller's
// Kubernetes RBAC authorizer (giantswarm/kagent-upstream#571 and #633,
// agent-platform's kagent.controller.authorization, which selects the
// organization namespaces by their label) judged through the agentgateway
// edge, as the Dev Portal and Slack call it, by members whose only access to
// their organization is a RoleBinding of their token's group.
const (
	// organizationsTestEdgeAgent is the Agent a member writes through the
	// edge: on a Harness that does not exist, so it never boots.
	organizationsTestEdgeAgent = "agentlab-organizations-edge"
	// organizationsTestEdgeLabel is the label the member's change adds.
	organizationsTestEdgeLabel = "agentlab.giantswarm.io/organizations-test-change"
	// authorizationModeKubernetes is the controller's RBAC authorizer mode.
	authorizationModeKubernetes = "kubernetes"
	// tenantsTestGroupsClaim is the claim the lab's Dex tokens carry the
	// groups in, as the API server reads it (--oidc-groups-claim).
	tenantsTestGroupsClaim = "groups"
	// edgeUpdateConflictWait bounds the member's update retries while the
	// controller's cached read of a new Agent is stale (HACKS.md U37).
	edgeUpdateConflictWait = 30 * time.Second
	// organizationsTestEdgeChange is the change label's value.
	organizationsTestEdgeChange = "changed"
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
		return fmt.Sprintf("the kagent controller runs authorization mode %q, not %s: every signed-in person sees and runs every organization's agents. It needs kagent 1.19 or later with kagent.controller.authorization.mode kubernetes — agent-platform 4.151 or later (`agentlab configure --chart-version <version>`, then `agentlab platform`)",
			cmp.Or(a.mode, "noop"), authorizationModeKubernetes)
	case a.groupsClaim != tenantsTestGroupsClaim:
		return fmt.Sprintf("the kagent controller reads the groups from claim %q, not %q: a member bound by group is refused (kagent.controller.authorization.groupsClaim)", a.groupsClaim, tenantsTestGroupsClaim)
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
	u.SetName(organizationsTestEdgeAgent)
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

// organizationsEdge is what the kagent API surface works with: the two
// organization namespaces, their members with their kagent API clients, and
// the admin's (cluster-admin) client.
type organizationsEdge struct {
	namespaces [2]string
	members    [2]*config.User
	apis       [2]*kagentAPI
	admin      *kagentAPI
}

// outsidePlatform drops the agents of the kagent namespace from a roster:
// the platform tenant every signed-in person may list until
// giantswarm/agent-manager#166 retires it, which this proof does not judge.
func outsidePlatform(set refSet) refSet {
	out := refSet{}
	for ref := range set {
		if ns, _, _ := strings.Cut(ref, "/"); ns != kagentNamespace {
			out[ref] = true
		}
	}
	return out
}

// waitOwnRoster lists the member's agents across namespaces until, outside
// the platform tenant, the list is the wanted one, bounded by
// organizationRBACSettle (the controller caches its decisions).
func waitOwnRoster(api *kagentAPI, want refSet) (refSet, string, error) {
	var got refSet
	var diff string
	var callErr error
	waitFor(int(organizationRBACSettle/pollInterval), pollInterval, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		agents, err := api.listAgentsIn(ctx, "")
		if callErr = err; err != nil {
			return false
		}
		got = outsidePlatform(refsOf(agents))
		diff = rosterDiff(got, want)
		return diff == ""
	})
	if callErr != nil {
		return nil, "", callErr
	}
	return got, diff, nil
}

// run is the kagent API surface: each member's roster, then the negative
// tests on the other organization's agent. It returns the verdicts and the
// rosters for the same-answer comparison.
func (e *organizationsEdge) run() ([]organizationVerdict, [2]refSet, error) {
	var out []organizationVerdict
	var rosters [2]refSet
	a, b := e.namespaces[0], e.namespaces[1]
	alice, bob := e.members[0], e.members[1]
	aliceAPI := e.apis[0]
	agentA := &apiv1alpha1.ResourceReference{Namespace: a, Name: organizationsTestAgent}
	agentB := &apiv1alpha1.ResourceReference{Namespace: b, Name: organizationsTestAgent}

	step("[%s] ListAgents across namespaces as each member: their own organization's agent, nothing of the other's (the platform tenant in %s not judged)", surfaceKagent, kagentNamespace)
	for i, api := range e.apis {
		own, err := agentsIn(e.namespaces[i])
		if err != nil {
			return nil, rosters, err
		}
		got, diff, err := waitOwnRoster(api, own)
		if err != nil {
			return nil, rosters, fmt.Errorf("%s's ListAgents: %w", e.members[i].Email, err)
		}
		if diff != "" {
			return nil, rosters, fmt.Errorf("FAIL [%s] %s (group %s, bound in %s) lists %s after %s; want %s (%s)", surfaceKagent, e.members[i].Email, e.members[i].Groups[0], e.namespaces[i], got, organizationRBACSettle, own, diff)
		}
		rosters[i] = got
		note("%s lists %s", e.members[i].Email, got)
	}
	out = append(out, organizationVerdict{surface: surfaceKagent, boundary: "kagent's authorizer lists a member the agents of their own organization only",
		detail: fmt.Sprintf("ListAgents through the edge lists %s %s and %s %s, by the group in their token alone", alice.Email, rosters[0], bob.Email, rosters[1])})

	step("[%s] %s on %s's agent: CreateSession and a first A2A message are PermissionDenied, no Session is made and no turn reaches the model; on their own organization's agent CreateSession succeeds", surfaceKagent, alice.Email, b)
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	defer cancel()
	before, err := e.admin.allCreatorsSessions(ctx, agentB)
	if err != nil {
		return nil, rosters, fmt.Errorf("ListSessions all_creators on %s as the admin (cluster-admin): %w", agentTenant(agentB), err)
	}
	since := time.Now()
	_, err = aliceAPI.sessions.CreateSession(aliceAPI.callCtx(ctx), &apiv1alpha1.CreateSessionRequest{Agent: agentB, RequestId: uuid.NewString()})
	if err := wantDenied(fmt.Sprintf("%s's CreateSession on %s", alice.Email, agentTenant(agentB)), err); err != nil {
		return nil, rosters, fmt.Errorf("FAIL [%s] %w", surfaceKagent, err)
	}
	if err := wantDenied(fmt.Sprintf("%s's first A2A message to %s", alice.Email, agentTenant(agentB)), aliceAPI.firstMessageTo(ctx, agentB, organizationsTestPrompt)); err != nil {
		return nil, rosters, fmt.Errorf("FAIL [%s] %w", surfaceKagent, err)
	}
	after, err := e.admin.allCreatorsSessions(ctx, agentB)
	if err != nil {
		return nil, rosters, err
	}
	if len(after) != len(before) {
		return nil, rosters, fmt.Errorf("FAIL [%s] %s holds %d Session(s) after the refused calls, %d before", surfaceKagent, agentTenant(agentB), len(after), len(before))
	}
	read, err := credentialReadsSince(b, since)
	if err != nil {
		return nil, rosters, err
	}
	if len(read) > 0 {
		return nil, rosters, fmt.Errorf("FAIL [%s] the credential provider read %v for atespace %s during the refused calls: a turn reached the model", surfaceKagent, read, b)
	}
	session, err := aliceAPI.createSessionOf(ctx, agentA, uuid.NewString())
	if err != nil {
		return nil, rosters, fmt.Errorf("the twin: %s's CreateSession on their own organization's %s: %w", alice.Email, agentTenant(agentA), err)
	}
	aliceAPI.removeSession(session.GetId())
	out = append(out, organizationVerdict{surface: surfaceKagent, boundary: "kagent's authorizer refuses a member a session or an A2A message with the other organization's agent",
		detail: fmt.Sprintf("%s's CreateSession and first A2A message on %s are PermissionDenied: no Session of it (%d before and after), no credential read for its atespace; their CreateSession on %s succeeds", alice.Email, agentTenant(agentB), len(before), agentTenant(agentA))})

	step("[%s] %s (editor of %s) creates, changes and deletes %s there through the edge; in %s the same writes are PermissionDenied", surfaceKagent, alice.Email, a, organizationsTestEdgeAgent, b)
	if err := aliceAPI.createAgent(ctx, edgeAgent(a, organizationsTestAgent, nil)); err != nil {
		return nil, rosters, fmt.Errorf("the twin: %s's CreateAgent %s/%s: %w", alice.Email, a, organizationsTestEdgeAgent, err)
	}
	// The controller writes the update over its cached read of the Agent,
	// which a create a moment ago leaves stale: the API server's conflict
	// comes back as Internal (kagent's kubecrud Update), so a few tries
	// (HACKS.md U37).
	var updateErr error
	waitFor(int(edgeUpdateConflictWait/pollInterval), pollInterval, func() bool {
		updateErr = aliceAPI.updateAgent(ctx, edgeAgent(a, organizationsTestAgent, map[string]string{organizationsTestEdgeLabel: organizationsTestEdgeChange}))
		return updateErr == nil || status.Code(updateErr) != codes.Internal
	})
	if updateErr != nil {
		return nil, rosters, fmt.Errorf("the twin: %s's UpdateAgent %s/%s: %w", alice.Email, a, organizationsTestEdgeAgent, updateErr)
	}
	changed, err := readEdgeAgentLabel(a)
	if err != nil {
		return nil, rosters, err
	}
	if changed != organizationsTestEdgeChange {
		return nil, rosters, fmt.Errorf("the twin: the update was accepted but Agent %s/%s carries %s=%q", a, organizationsTestEdgeAgent, organizationsTestEdgeLabel, changed)
	}
	if err := aliceAPI.deleteAgent(ctx, &apiv1alpha1.ResourceReference{Namespace: a, Name: organizationsTestEdgeAgent}); err != nil {
		return nil, rosters, fmt.Errorf("the twin: %s's DeleteAgent %s/%s: %w", alice.Email, a, organizationsTestEdgeAgent, err)
	}
	other, err := getAgentObject(b, organizationsTestAgent)
	if err != nil {
		return nil, rosters, err
	}
	otherChanged := other.DeepCopy()
	otherChanged.SetLabels(map[string]string{organizationsTestEdgeLabel: organizationsTestEdgeChange})
	if err := deniedWrites(alice.Email, []agentWrite{
		{"CreateAgent " + b + "/" + organizationsTestEdgeAgent, func() error { return aliceAPI.createAgent(ctx, edgeAgent(b, organizationsTestAgent, nil)) }},
		{"UpdateAgent " + agentTenant(agentB), func() error { return aliceAPI.updateAgent(ctx, otherChanged.Object) }},
		{"DeleteAgent " + agentTenant(agentB), func() error { return aliceAPI.deleteAgent(ctx, agentB) }},
	}); err != nil {
		return nil, rosters, fmt.Errorf("FAIL [%s] %w", surfaceKagent, err)
	}
	if err := unchangedAgent(agentB); err != nil {
		return nil, rosters, fmt.Errorf("FAIL [%s] %w", surfaceKagent, err)
	}
	out = append(out, organizationVerdict{surface: surfaceKagent, boundary: "kagent's authorizer refuses a member every write of the other organization's agents",
		detail: fmt.Sprintf("%s, editor of %s, creates, changes and deletes an Agent there through the edge; CreateAgent, UpdateAgent and DeleteAgent in %s are PermissionDenied and its agent is unchanged", alice.Email, a, b)})

	step("[%s] ListSessions with all_creators: PermissionDenied for %s and %s, answered for the admin (cluster-admin, a cluster-wide list on sessions)", surfaceKagent, alice.Email, bob.Email)
	for i, api := range e.apis {
		_, err := api.allCreatorsSessions(ctx, nil)
		if err := wantDenied(e.members[i].Email+"'s ListSessions all_creators", err); err != nil {
			return nil, rosters, fmt.Errorf("FAIL [%s] %w", surfaceKagent, err)
		}
	}
	if _, err := e.admin.allCreatorsSessions(ctx, nil); err != nil {
		return nil, rosters, fmt.Errorf("the twin: the admin's ListSessions all_creators: %w", err)
	}
	out = append(out, organizationVerdict{surface: surfaceKagent, boundary: "kagent's authorizer refuses a member every creator's sessions (all_creators)",
		detail: fmt.Sprintf("ListSessions all_creators is PermissionDenied for %s and %s and answered for the cluster-admin", alice.Email, bob.Email)})
	return out, rosters, nil
}

// agentWrite is one write a person tries through the edge.
type agentWrite struct {
	what string
	call func() error
}

// deniedWrites checks the controller refuses each of the person's writes.
func deniedWrites(email string, writes []agentWrite) error {
	for _, w := range writes {
		if err := wantDenied(email+"'s "+w.what, w.call()); err != nil {
			return err
		}
	}
	return nil
}

// unchangedAgent checks a refused update and delete left the Agent as it
// was: there, without the change label.
func unchangedAgent(ref *apiv1alpha1.ResourceReference) error {
	obj, err := getAgentObject(ref.GetNamespace(), ref.GetName())
	if err != nil {
		return fmt.Errorf("%s after the refused delete: %w", agentTenant(ref), err)
	}
	if _, ok := obj.GetLabels()[organizationsTestEdgeLabel]; ok {
		return fmt.Errorf("%s carries %s although the update was refused", agentTenant(ref), organizationsTestEdgeLabel)
	}
	return nil
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
	obj, err := getAgentObject(ns, organizationsTestEdgeAgent)
	if err != nil {
		return "", fmt.Errorf("the editor's Agent %s/%s: %w", ns, organizationsTestEdgeAgent, err)
	}
	return obj.GetLabels()[organizationsTestEdgeLabel], nil
}
