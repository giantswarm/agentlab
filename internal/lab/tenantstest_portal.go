package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The portal half of tenants-test (giantswarm/agentlab#166, slice 6's portal
// part): the Dev Portal's backend routes as each person, in the same two
// organizations, while their agents and the platform tenant's run — the
// roster per tenant, the create flow's tenants, the access reviews behind
// Edit and Delete, a refused session and the creator
// (giantswarm/backstage#3161). What only a browser shows (the empty state's
// words, the picked tenant, the note on the agent page) is the Playwright
// suite's (docs/backstage.md "The portal's browser suite").
const (
	// portalAgentNamespacesPath is the portal's route that names the
	// namespaces of the Agents the person may list
	// (AgentService/ListAgents with no namespace, as the person).
	portalAgentNamespacesPath = "/agent-namespaces"
	// createdByAnnotation records an agent's creator on its HelmRelease and
	// Agent (agent-manager writes the caller's email on create).
	createdByAnnotation = "agent-platform.giantswarm.io/created-by"
	// platformTenant is what the portal calls the kagent namespace's tenant,
	// organizationTenantPrefix what it puts before an organization's name.
	platformTenant           = "platform"
	organizationTenantPrefix = "organization "
	// orgNamespacePrefix starts an organization's namespace.
	orgNamespacePrefix = "org-"
	// refusalKagentForbidden starts agent-manager's answer when the
	// controller refuses a session (PermissionDenied): the answer the portal
	// words as a refusal naming the agent's tenant.
	refusalKagentForbidden = refusalForbidden + " kagent:"
	// verbDelete is the Kubernetes verb.
	verbDelete = "delete"
)

// releaseWriteVerbs are the writes the agent page asks the API server about
// on an agent's HelmRelease: Edit and Update skills, Delete.
var releaseWriteVerbs = []string{"update", verbDelete}

// portalTenantOf is the tenant the portal names for a namespace
// (plugins/agent-platform/src/lib/tenants.ts tenantOf): platform for kagent,
// organization <name> for org-<name>, namespace <name> otherwise.
func portalTenantOf(ns string) string {
	switch {
	case ns == kagentNamespace:
		return platformTenant
	case strings.HasPrefix(ns, orgNamespacePrefix) && len(ns) > len(orgNamespacePrefix):
		return organizationTenantPrefix + strings.TrimPrefix(ns, orgNamespacePrefix)
	default:
		return "namespace " + ns
	}
}

// rosterNamespaces are the namespaces the portal reads Agents in when the
// person may not list them cluster-wide (useNamespacedResources'
// agentNamespacesOf): the platform tenant, agent-manager's default and
// managed namespaces, and the ones the controller lists for the person.
func rosterNamespaces(defaultNS string, managed, listed []string) []string {
	var out []string
	for _, ns := range slices.Concat([]string{kagentNamespace, defaultNS}, managed, listed) {
		if ns != "" && !slices.Contains(out, ns) {
			out = append(out, ns)
		}
	}
	return out
}

// tenantRow is one agent of the person's roster as the portal shows it: its
// tenant label and its creator.
type tenantRow struct {
	Tenant, CreatedBy string
}

// tenantRoster is a person's roster: namespace/name -> row, and how the
// portal got it.
type tenantRoster struct {
	rows map[string]tenantRow
	// listed is what GET /kagent/agent-namespaces answered, routeErr why it
	// answered nothing (the portal reads past it).
	listed   []string
	routeErr error
	// refused are the namespaces whose list the apiserver refused (403):
	// skipped by the portal.
	refused []string
}

// refs is the roster as a set of namespace/name.
func (r *tenantRoster) refs() refSet {
	set := refSet{}
	for ref := range r.rows {
		set[ref] = true
	}
	return set
}

// in is the set narrowed to the namespaces, the whole set without any.
func (s refSet) in(namespaces []string) refSet {
	if len(namespaces) == 0 {
		return s
	}
	out := refSet{}
	for ref := range s {
		if ns, _, _ := strings.Cut(ref, "/"); slices.Contains(namespaces, ns) {
			out[ref] = true
		}
	}
	return out
}

// createdBy are the rows whose recorded creator is the email, compared
// without case: what the portal's "Created by: Me" keeps for that person.
func (r *tenantRoster) createdBy(email string) refSet {
	set := refSet{}
	for ref, row := range r.rows {
		if row.CreatedBy != "" && strings.EqualFold(row.CreatedBy, email) {
			set[ref] = true
		}
	}
	return set
}

// agentNamespaces is GET /kagent/agent-namespaces as the person.
func (ps *portalSession) agentNamespaces() ([]string, error) {
	var out struct {
		Namespaces []string `json:"namespaces"`
	}
	status, raw, err := ps.kagentJSON(http.MethodGet, portalAgentNamespacesPath, nil, &out)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET %s%s answered %d: %s", portalKagentAPI, portalAgentNamespacesPath, status, excerpt(strings.TrimSpace(backstageErrorMessage(raw)), 200))
	}
	return out.Namespaces, nil
}

// kubeProxyPost sends a JSON body through Backstage's Kubernetes proxy as
// the person, the way kubernetes-react's access review does.
func (ps *portalSession) kubeProxyPost(path string, body any) (int, []byte, error) {
	return ps.request(http.MethodPost, portalKubeProxyAPI+path, body, map[string]string{
		portalKubeClusterHeader: platformRelease, portalKubeAuthHeader: ps.dexIDToken,
	})
}

// mayWriteRelease is the agent page's access review: a
// SelfSubjectAccessReview for the verb on the agent's HelmRelease, named, in
// its namespace, through the portal's Kubernetes proxy as the person.
func (ps *portalSession) mayWriteRelease(verb, ns, name string) (bool, error) {
	review := authorizationv1.SelfSubjectAccessReview{
		TypeMeta: metav1.TypeMeta{APIVersion: authorizationv1.SchemeGroupVersion.String(), Kind: "SelfSubjectAccessReview"},
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
			Group: "helm.toolkit.fluxcd.io", Resource: "helmreleases", Namespace: ns, Name: name, Verb: verb,
		}},
	}
	status, raw, err := ps.kubeProxyPost("/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", review)
	if err != nil {
		return false, err
	}
	var answer authorizationv1.SelfSubjectAccessReview
	if status != http.StatusCreated && status != http.StatusOK || json.Unmarshal(raw, &answer) != nil {
		return false, fmt.Errorf("the access review of %s on HelmRelease %s/%s through %s answered %d: %s", verb, ns, name, portalKubeProxyAPI, status, excerpt(string(raw), 200))
	}
	return answer.Status.Allowed, nil
}

// portalInfo is agent-manager's get_info through the portal as the person.
func portalInfo(ps *portalSession) (*agentManagerInfo, error) {
	var info agentManagerInfo
	if err := portalToolCall(ps, "get_info", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// readTenantRoster reads the person's roster the way the Agents tab does
// with #3161: the cluster-wide list first (a tenant's person is refused it),
// then each of rosterNamespaces, a refused namespace skipped.
func readTenantRoster(ps *portalSession) (*tenantRoster, error) {
	status, raw, err := ps.kubeProxyGet("/apis/" + kagentAPIVersion + "/agents")
	if err != nil {
		return nil, err
	}
	if status == http.StatusOK {
		return nil, fmt.Errorf("%s lists Agents cluster-wide through %s: a tenant's person must be refused that (the lab binds them per namespace only)", ps.user.Email, portalKubeProxyAPI)
	}
	if status != http.StatusForbidden {
		return nil, fmt.Errorf("%s's cluster-wide Agents list through %s answered %d, want 403: %s", ps.user.Email, portalKubeProxyAPI, status, excerpt(string(raw), 200))
	}
	info, err := portalInfo(ps)
	if err != nil {
		return nil, err
	}
	roster := &tenantRoster{rows: map[string]tenantRow{}}
	roster.listed, roster.routeErr = ps.agentNamespaces()
	for _, ns := range rosterNamespaces(info.Namespaces.Default, info.Namespaces.Managed, roster.listed) {
		status, raw, err := ps.kubeProxyGet("/apis/" + kagentAPIVersion + "/namespaces/" + ns + "/agents")
		if err != nil {
			return nil, err
		}
		switch status {
		case http.StatusOK:
		case http.StatusForbidden:
			roster.refused = append(roster.refused, ns)
			continue
		default:
			return nil, fmt.Errorf("%s's Agents list in %s through %s answered %d — the portal would show the environment as unreadable: %s", ps.user.Email, ns, portalKubeProxyAPI, status, excerpt(string(raw), 200))
		}
		var list struct {
			Items []agentObject `json:"items"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("the Agents of %s through %s: %w", ns, portalKubeProxyAPI, err)
		}
		for _, a := range list.Items {
			roster.rows[ns+"/"+a.Metadata.Name] = tenantRow{Tenant: portalTenantOf(ns), CreatedBy: a.Metadata.Annotations[createdByAnnotation]}
		}
	}
	return roster, nil
}

// waitTenantRoster reads the person's roster until it lists exactly want
// and, when listed is not nil, GET /kagent/agent-namespaces names exactly
// listed — bounded by tenantRBACSettle (the controller caches its decisions,
// the API server does not, so the two can disagree for a while) — and
// returns the last read with the difference left.
//
// With only, the roster is compared in those namespaces alone: a person may
// read namespaces outside the run (an earlier hand-made tenant).
func waitTenantRoster(ps *portalSession, want refSet, listed []string, only ...string) (*tenantRoster, string, error) {
	var roster *tenantRoster
	var diff string
	var readErr error
	waitFor(int(tenantRBACSettle/pollInterval), pollInterval, func() bool {
		roster, readErr = readTenantRoster(ps)
		if readErr != nil {
			return false
		}
		diff = rosterDiff(roster.refs().in(only), want)
		if listed != nil && !sameNamespaces(roster.listed, listed) {
			diff = strings.TrimPrefix(diff+fmt.Sprintf("; GET %s%s names %v, want %v", portalKagentAPI, portalAgentNamespacesPath, roster.listed, listed), "; ")
		}
		return diff == ""
	})
	return roster, diff, readErr
}

// sameNamespaces reports whether two namespace lists hold the same names, in
// any order.
func sameNamespaces(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// rosterMiss words a roster that is not the wanted one, with what the portal
// read it from.
func rosterMiss(email string, roster *tenantRoster, diff string) string {
	why := fmt.Sprintf("GET %s%s named %v", portalKagentAPI, portalAgentNamespacesPath, roster.listed)
	if roster.routeErr != nil {
		why = fmt.Sprintf("the portal read no tenant of theirs: %v (a Backstage without giantswarm/backstage#3161 has no such route)", roster.routeErr)
	}
	return fmt.Sprintf("%s's roster through the portal lists %s (%s); %s", email, roster.refs(), diff, why)
}

// tenantsPortal is what the portal half works with: the two organization
// namespaces, their tenant users, the editors and the admin.
type tenantsPortal struct {
	cfg        *config.Config
	namespaces [2]string
	users      [2]*config.User
	editor     *config.User
	admin      *config.User
}

// run is the portal half, steps P1 to P6 (P2 last: it takes the first
// organization from its user and removes the platform tenant's agent), and
// returns its PASS lines.
func (p *tenantsPortal) run() ([]string, error) {
	var out []string
	a, b := p.namespaces[0], p.namespaces[1]
	alice, bob := p.users[0], p.users[1]
	agentA, agentB := a+"/"+tenantsTestAgent, b+"/"+tenantsTestAgent

	var sessions [2]*portalSession
	for i, u := range p.users {
		step("Signing in to the portal as %s (group %v)", u.Email, u.Groups)
		ps, err := backstageLogin(p.cfg, u)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u.Email, err)
		}
		sessions[i] = ps
	}
	step("Signing in to the portal as %s (editor of %s)", p.editor.Email, a)
	dev, err := backstageLogin(p.cfg, p.editor)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.editor.Email, err)
	}

	// M5's agent goes with its HelmRelease's uninstall, which M5 does not
	// wait for: what the rosters must list is read once it is gone.
	if err := waitAgentGone(a, tenantsTestMusterAgent); err != nil {
		return nil, err
	}
	step("P1. %s and %s read their rosters through the portal at the same time: their organization's agent and the platform tenant's, nothing of the other organization", alice.Email, bob.Email)
	var wg sync.WaitGroup
	errs := make([]error, len(sessions))
	rosters := make([]*tenantRoster, len(sessions))
	for i, ps := range sessions {
		want, err := agentsIn(kagentNamespace, p.namespaces[i])
		if err != nil {
			return nil, err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			roster, diff, err := waitTenantRoster(ps, want, []string{kagentNamespace, p.namespaces[i]})
			switch {
			case err != nil:
				errs[i] = fmt.Errorf("%s: %w", ps.user.Email, err)
			case diff != "":
				errs[i] = errors.New(rosterMiss(ps.user.Email, roster, diff))
			}
			rosters[i] = roster
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("P1: %w", err)
	}
	for i, roster := range rosters {
		note("%s: %s (tenants: %s; refused %v)", sessions[i].user.Email, roster.refs(), tenantLabels(roster), roster.refused)
	}
	out = append(out, fmt.Sprintf("PASS: through the portal, at the same time, %s's roster is %s's agent and the platform tenant's, %s's is %s's and the platform tenant's: GET %s%s names each person's tenants, nothing of the other organization (the rows' tenant labels are the browser suite's)",
		alice.Email, a, bob.Email, b, portalKagentAPI, portalAgentNamespacesPath))

	step("P3. %s (editor of %s only): the create flow's tenants (get_info namespaces.managed) name %s, not %s, and a create through the portal into %s is refused", p.editor.Email, a, a, b, b)
	info, err := portalInfo(dev)
	if err != nil {
		return nil, fmt.Errorf("P3: %w", err)
	}
	// Of the two organizations, the editor's one only. A namespace outside
	// the run where the lab binds them too (an earlier hand-made tenant) is
	// theirs as well, so the list is not compared whole.
	if managed := info.Namespaces.Managed; !slices.Contains(managed, a) || slices.Contains(managed, b) {
		return nil, fmt.Errorf("P3: %s's get_info through the portal names namespaces.managed %v, want %s and not %s (they are an editor of the first only)", p.editor.Email, managed, a, b)
	}
	created := portalToolCall(dev, "create_agent", tenantsTestAgentSpec(b, tenantsTestCrossAgent, defaultModelConfig), nil)
	if created == nil || !strings.Contains(created.Error(), refusalForbidden) {
		return nil, fmt.Errorf("P3: %s's create_agent through the portal in %s: %v, want agent-manager's %s", p.editor.Email, b, created, refusalForbidden)
	}
	if exists, rerr := tenantsTestReleaseExists(b, tenantsTestCrossAgent); rerr != nil || exists {
		return nil, fmt.Errorf("P3: HelmRelease %s/%s exists (or cannot be read: %v) although the create was refused", b, tenantsTestCrossAgent, rerr)
	}
	note("managed %v; refused: %s", info.Namespaces.Managed, excerpt(created.Error(), 200))
	out = append(out, fmt.Sprintf("PASS: of the two organizations, %s's create flow offers %s only (get_info namespaces.managed through the portal), and their create in %s through the portal is refused (forbidden), no HelmRelease written", p.editor.Email, portalTenantOf(a), b))

	step("P4. The agent page's access reviews (update, delete on the agent's HelmRelease): no for %s on %s; yes for %s on %s, no on %s", alice.Email, agentA, p.editor.Email, agentA, agentB)
	for _, c := range []struct {
		ps    *portalSession
		ns    string
		allow bool
	}{{sessions[0], a, false}, {dev, a, true}, {dev, b, false}} {
		for _, verb := range releaseWriteVerbs {
			allowed, err := c.ps.mayWriteRelease(verb, c.ns, tenantsTestAgent)
			if err != nil {
				return nil, fmt.Errorf("P4: %w", err)
			}
			if allowed != c.allow {
				return nil, fmt.Errorf("P4: the access review of %s on HelmRelease %s/%s answers %v for %s, want %v", verb, c.ns, tenantsTestAgent, allowed, c.ps.user.Email, c.allow)
			}
		}
	}
	out = append(out, fmt.Sprintf("PASS: through the portal's Kubernetes proxy, the access review for update and delete on the agent's HelmRelease answers no for %s (a user) in %s, yes for %s (its editor) there and no in %s: the page hides Edit, Update skills and Delete where they would be refused", alice.Email, a, p.editor.Email, b))

	step("P5. %s starts a session on %s through the portal: agent-manager answers the controller's refusal (%s …), no Session of it, no credential read", alice.Email, agentB, refusalKagentForbidden)
	adminToken, err := passwordGrant(p.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, p.admin.Email, p.admin.Password, musterLoginScopes)
	if err != nil {
		return nil, err
	}
	adminAPI, err := dialKagentAPI(p.cfg, adminToken)
	if err != nil {
		return nil, err
	}
	defer adminAPI.close()
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	defer cancel()
	ref := &apiv1alpha1.ResourceReference{Namespace: b, Name: tenantsTestAgent}
	before, err := adminAPI.allCreatorsSessions(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("P5: ListSessions all_creators on %s as the admin: %w", agentB, err)
	}
	since := time.Now()
	_, _, refused := sessions[0].startSession(portalAgentRef{Namespace: b, Name: tenantsTestAgent}, "agentlab tenants-test refused", uuid.NewString(), tenantsTestPrompt)
	if refused == nil || !strings.Contains(refused.Error(), refusalKagentForbidden) {
		return nil, fmt.Errorf("P5: %s's start_session on %s through the portal: %v, want agent-manager's %s … (the controller's PermissionDenied), the answer the portal words as a refusal naming the tenant", alice.Email, agentB, refused, refusalKagentForbidden)
	}
	after, err := adminAPI.allCreatorsSessions(ctx, ref)
	if err != nil {
		return nil, err
	}
	if len(after) != len(before) {
		return nil, fmt.Errorf("P5: %s holds %d Session(s) after the refused start, %d before", agentB, len(after), len(before))
	}
	read, err := tenantCredentialReads(b, since)
	if err != nil {
		return nil, err
	}
	if len(read) > 0 {
		return nil, fmt.Errorf("P5: the credential provider read %v for atespace %s during the refused start: a turn reached the model", read, b)
	}
	note("refused: %s", excerpt(refused.Error(), 200))
	out = append(out, fmt.Sprintf("PASS: %s's session start on %s through the portal is the controller's refusal (%s …), no Session of it (%d before and after), no credential read for its atespace", alice.Email, agentB, refusalKagentForbidden, len(before)))

	step("P6. The creator: %s's Agent carries %s=%s as %s reads it through the portal, and of their roster only that agent is theirs", agentA, createdByAnnotation, p.editor.Email, p.editor.Email)
	// The person as kagent knows them (GET /kagent/me, the claims the
	// controller resolved): what "Created by: Me" is compared with. The
	// creator agent-manager records is the email; kagent 1.19 answers the
	// token's claims, whose sub is Dex's, not the email.
	var me struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if status, raw, err := dev.kagentJSON(http.MethodGet, kagentMePath, nil, &me); err != nil || status != http.StatusOK {
		return nil, fmt.Errorf("P6: GET %s%s as %s answered %d (%v): %s", portalKagentAPI, kagentMePath, p.editor.Email, status, err, excerpt(string(raw), 200))
	}
	if !strings.EqualFold(me.Email, p.editor.Email) {
		return nil, fmt.Errorf("P6: GET %s%s as %s names email %q (sub %q)", portalKagentAPI, kagentMePath, p.editor.Email, me.Email, me.Sub)
	}
	want, err := agentsIn(kagentNamespace, a)
	if err != nil {
		return nil, err
	}
	roster, diff, err := waitTenantRoster(dev, want, nil, kagentNamespace, a, b)
	if err != nil {
		return nil, fmt.Errorf("P6: %s: %w", p.editor.Email, err)
	}
	if diff != "" {
		return nil, fmt.Errorf("P6: %s", rosterMiss(p.editor.Email, roster, diff))
	}
	if mine := roster.createdBy(p.editor.Email).in([]string{kagentNamespace, a, b}); rosterDiff(mine, refSet{agentA: true}) != "" {
		return nil, fmt.Errorf("P6: of %s's roster %s, %s names %s as the creator of %s, want %s alone", p.editor.Email, roster.refs(), createdByAnnotation, p.editor.Email, mine, agentA)
	}
	note("roster %s; created by %s: %s; GET %s%s: sub %q, email %q", roster.refs(), p.editor.Email, agentA, portalKagentAPI, kagentMePath, me.Sub, me.Email)
	out = append(out, fmt.Sprintf("PASS: of %s's roster through the portal (%s), %s names them as the creator of %s alone; GET %s%s names them by email (sub %s)", p.editor.Email, roster.refs(), createdByAnnotation, agentA, portalKagentAPI, kagentMePath, excerpt(me.Sub, 12)))

	step("P2. %s in no organization (RoleBinding %s in %s removed) and the platform tenant's agent gone: an empty roster, not an error", alice.Email, tenantUsersBinding, a)
	if err := unbindTenantGroup(a); err != nil {
		return nil, err
	}
	if err := removeTenantsTestPlatformAgent(); err != nil {
		return nil, err
	}
	if err := waitAgentGone(kagentNamespace, tenantsTestPlatformAgent); err != nil {
		return nil, err
	}
	platform, err := agentsIn(kagentNamespace)
	if err != nil {
		return nil, err
	}
	if len(platform) > 0 {
		return nil, fmt.Errorf("P2: the platform tenant still holds %s, which the lab did not create: the empty roster cannot be shown", platform)
	}
	roster, diff, err = waitTenantRoster(sessions[0], refSet{}, []string{})
	if err != nil {
		return nil, fmt.Errorf("P2: %s: %w", alice.Email, err)
	}
	if diff != "" {
		return nil, fmt.Errorf("P2: %s", rosterMiss(alice.Email, roster, diff))
	}
	if roster.routeErr != nil {
		return nil, fmt.Errorf("P2: GET %s%s as %s, in no organization: %w, want an answer naming no namespace", portalKagentAPI, portalAgentNamespacesPath, alice.Email, roster.routeErr)
	}
	// Read, and empty: a refused platform tenant would leave every namespace
	// refused, which the portal shows as no access, not as an empty roster.
	if slices.Contains(roster.refused, kagentNamespace) {
		return nil, fmt.Errorf("P2: %s's Agents list in %s through %s is refused (403): the chart binds agent-platform:platform-user to every signed-in person there", alice.Email, kagentNamespace, portalKubeProxyAPI)
	}
	note("no agent; GET %s%s names none; refused %v", portalKagentAPI, portalAgentNamespacesPath, roster.refused)
	out = append(out, fmt.Sprintf("PASS: %s, in no organization and with the platform tenant empty, reads an empty roster through the portal, no environment unreadable (GET %s%s names no namespace; refused %v)", alice.Email, portalKagentAPI, portalAgentNamespacesPath, roster.refused))
	return out, nil
}

// tenantLabels lists the tenants a roster names, sorted, once each.
func tenantLabels(r *tenantRoster) string {
	var labels []string
	for _, row := range r.rows {
		if !slices.Contains(labels, row.Tenant) {
			labels = append(labels, row.Tenant)
		}
	}
	slices.Sort(labels)
	return strings.Join(labels, ", ")
}

// waitAgentGone waits until the Agent is gone, bounded: it goes with its
// HelmRelease's uninstall, after the release itself.
func waitAgentGone(ns, name string) error {
	gvr, err := gvrFor(agentResource)
	if err != nil {
		return err
	}
	var readErr error
	gone := waitFor(int(tenantsTestCopiesTimeout/pollInterval), pollInterval, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		exists, err := objectExists(ctx, gvr, ns, name)
		readErr = err
		return err == nil && !exists
	})
	if !gone {
		return fmt.Errorf("the Agent %s/%s is still there after %s (%v): its HelmRelease's uninstall did not remove it", ns, name, tenantsTestCopiesTimeout, readErr)
	}
	return nil
}
