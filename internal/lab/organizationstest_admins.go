package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agentlab/internal/config"
)

// The admins part of the organizations proof (giantswarm/agentlab#1225,
// giantswarm/giantswarm#39501, docs/agents.md "The admins part"): an
// organization's admins exist only in its Organization's spec.admins.
// agent-platform-operator binds them to agent-platform:admin in the
// organization namespace (RoleBinding organization-admins), the chart's
// admission policy refuses every other writer of a binding to that role, and
// agent-manager's member tools refuse the admin role. The right to create an
// Organization is one RBAC rule on the kind, which the lab's admin holds.
//
// The lab's admin creates two organizations through create_organization:
// one whose admin is alice's group, one whose admin is bob by name. Neither
// alice, bob nor the outsider holds a right cluster-wide. Everything is
// judged on what the cluster and the tools answer, never on a copy of the
// operator's or the chart's rule.
const (
	organizationAdminsPart = "admins"
	// adminsOrgA and adminsOrgB are the organizations of the part.
	adminsOrgA = "agentlab-admins-a"
	adminsOrgB = "agentlab-admins-b"
	// adminsOrgRefused is the organization every refused create names; it
	// never exists.
	adminsOrgRefused = "agentlab-admins-refused"
	// adminsDryRunBinding names the RoleBindings the part writes as dry runs.
	adminsDryRunBinding = "agentlab-admins-proof"
	// organizationsAdminClusterRole is the role of the people who manage
	// Organizations, bound cluster-wide.
	organizationsAdminClusterRole = "agent-platform:organizations-admin"
	clusterRoleKind               = "ClusterRole"
	// validatingAdmissionPolicy is the API server's word in a refusal by a
	// ValidatingAdmissionPolicy: a refusal without it may be RBAC's.
	validatingAdmissionPolicy = "ValidatingAdmissionPolicy"
	// adminsCallTimeout bounds one organization tool call.
	adminsCallTimeout = 4 * time.Minute
	// The arguments of agent-manager's organization tools the part sends:
	// the admins of create_organization and update_organization, and the
	// role of add_organization_member, whose person argument is "user" too.
	// organizationArg is the organization every tool of an organization's
	// contents takes: the name of its Organization, never its namespace.
	organizationArg = "organization"
	argAdminGroups  = "adminGroups"
	argAdminUsers   = "adminUsers"
	argRole         = "role"
	argRoleUser     = "user"
	argRoleAdmin    = "admin"
	// memberToolsAdminRefusal is agent-manager's refusal of the admin role
	// in its member tools (internal/organizations/members.go, roleOf).
	memberToolsAdminRefusal = "the member tools do not give or remove the admin role"
	// organizationSwitchOffPolicy is the chart's ValidatingAdmissionPolicy
	// that keeps an organization's Agent Platform on once it is on, for every
	// writer (decision D10 of giantswarm/giantswarm#39492): an update that
	// sets spec.capabilities.agentPlatform.enabled to false or removes it is
	// refused. organizationSwitchOffChart is the chart pull request that adds
	// it.
	organizationSwitchOffPolicy = "agent-platform-organization-switch-off"
	organizationSwitchOffChart  = organizationKindChart
)

// createOrganizationAnswer is the part of create_organization's answer the
// part reads.
type createOrganizationAnswer struct {
	Created      bool `json:"created"`
	Organization struct {
		Admins []organizationAdmin `json:"admins"`
	} `json:"organization"`
	Next string `json:"next"`
}

// adminsProof is one run of the part.
type adminsProof struct {
	cfg *config.Config
	// admin is the lab's admin (platform-admins), who creates the
	// organizations; alice and bob are their admins; outsider is neither.
	admin, alice, bob, outsider *config.User
	// musterTokens are their tokens for muster, kubeTokens for the API server.
	musterTokens map[string]string
	kubeTokens   map[string]string
	// namespaces are the organizations' namespaces, by organization.
	namespaces map[string]string
	verdicts   []organizationVerdict
	failed     bool
}

// organizationAdminsTest runs the admins part and prints its verdicts.
func organizationAdminsTest(cfg *config.Config) error {
	members, err := organizationsTestMembers(cfg)
	if err != nil {
		return err
	}
	outsider := adminsOutsider(cfg, members)
	if outsider == nil {
		return fmt.Errorf("the admins part needs a lab user outside %s besides %s and %s, whom the admins add as a member; %s has none", platformAdminsGroup, members[0].Email, members[1].Email, config.File)
	}
	admin := cfg.AdminUser()
	if admin == nil {
		return fmt.Errorf("no platform-admins user in %s", config.File)
	}
	p := &adminsProof{cfg: cfg, admin: admin, alice: members[0], bob: members[1], outsider: outsider,
		musterTokens: map[string]string{}, kubeTokens: map[string]string{}, namespaces: map[string]string{}}
	defer p.cleanup()
	if err := p.run(); err != nil {
		p.failed = true
		return err
	}
	fmt.Println()
	for _, v := range p.verdicts {
		fmt.Println(v.line())
	}
	return nil
}

// adminsOutsider is the first lab user outside platform-admins who is
// neither member.
func adminsOutsider(cfg *config.Config, members [2]*config.User) *config.User {
	for i := range cfg.Users {
		u := &cfg.Users[i]
		if u.HasGroup(platformAdminsGroup) || u.HasGroup(config.GroupTenantA) || u.HasGroup(config.GroupTenantB) || u.Email == members[0].Email || u.Email == members[1].Email {
			continue
		}
		return u
	}
	return nil
}

func (p *adminsProof) run() error {
	if err := removeAdminsFixtures(); err != nil {
		return err
	}
	if err := p.setUp(); err != nil {
		return err
	}
	for _, c := range p.checks() {
		v, err := c.verdict()
		if err != nil {
			return err
		}
		p.verdicts = append(p.verdicts, v)
	}
	return nil
}

// checks are the part's verdicts in the order they run: the creates first,
// the refusals while both organizations hold their first admins, then the
// changes of spec.admins.
func (p *adminsProof) checks() []organizationCheck {
	return []organizationCheck{
		{surface: surfaceKubernetes, boundary: "the organizations' admins hold no right cluster-wide to create RoleBindings, bind agent-platform:admin or create Organizations", run: p.noRightClusterWide},
		{surface: surfaceAgentManager, boundary: "create_organization writes an organization's admin group into spec.admins and no RoleBinding; agent-platform-operator makes it Ready and binds the group alone to agent-platform:admin", run: p.created(adminsOrgA)},
		{surface: surfaceAgentManager, boundary: "create_organization writes an organization's admin user into spec.admins and no RoleBinding; agent-platform-operator makes it Ready and binds the user alone to agent-platform:admin", run: p.created(adminsOrgB)},
		{surface: surfaceKubernetes, boundary: "nobody but agent-platform-operator binds agent-platform:admin: the organization's admin and the lab's admin (cluster-admin) are refused at admission, while the admin binds agent-platform:editor", run: p.adminBindingRefused},
		{surface: surfaceAgentManager, boundary: "add_organization_member refuses the admin role, nothing written, while the admin adds a user", run: p.memberToolRefusesAdmin},
		{surface: surfaceAgentManager, boundary: "a person without create on Organizations is refused create_organization, nothing written", run: p.createRefused},
		{surface: surfaceKubernetes, boundary: "nobody switches an organization's Agent Platform off (decision D10): the lab's admin (cluster-admin) is refused at admission an update that sets spec.capabilities.agentPlatform.enabled to false, while an update that keeps it on is admitted", run: p.switchOffRefused},
		{surface: surfaceKubernetes, boundary: "a change of spec.admins (update_organization) changes organization-admins: the group replaced by a user", run: p.adminsChanged},
		{surface: surfaceKubernetes, boundary: "an empty spec.admins (update_organization) removes organization-admins, and the organization stays Ready", run: p.adminsEmptied},
	}
}

// caseOf is an organization of the part with its first admins.
func (p *adminsProof) caseOf(org string) (args map[string]any, admins []organizationAdmin) {
	if org == adminsOrgA {
		group := adminGroup(config.GroupTenantA)
		return map[string]any{argAdminGroups: []string{group.Name}}, []organizationAdmin{group}
	}
	user := adminUser(p.bob)
	return map[string]any{argAdminUsers: []string{user.Name}}, []organizationAdmin{user}
}

// setUp signs the four people in.
func (p *adminsProof) setUp() error {
	for _, u := range []*config.User{p.admin, p.alice, p.bob, p.outsider} {
		step("Logging in to Dex as %s", u.Email)
		token, err := passwordGrant(p.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, u.Email, u.Password, musterLoginScopes)
		if err != nil {
			return err
		}
		p.musterTokens[u.Email] = token
		if p.kubeTokens[u.Email], err = passwordGrant(p.cfg, config.KubernetesClientID, config.KubernetesClientSecret, u.Email, u.Password, loginScopes); err != nil {
			return err
		}
	}
	return nil
}

// subjectsLine names subjects as a binding lists them.
func subjectsLine(subjects []rbacv1.Subject) string {
	names := make([]string, 0, len(subjects))
	for _, s := range subjects {
		names = append(names, s.Kind+" "+s.Name)
	}
	return strings.Join(names, ", ")
}

// noRightClusterWide asks the API server, as each person, whether they may
// create RoleBindings, bind agent-platform:admin and create Organizations in
// every namespace: a yes would let them pass the part's refusals on that
// right.
func (p *adminsProof) noRightClusterWide() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	for _, u := range []*config.User{p.alice, p.bob, p.outsider} {
		_, cs, err := personClients(p.kubeTokens[u.Email])
		if err != nil {
			return "", err
		}
		for _, attrs := range []authorizationv1.ResourceAttributes{
			{Verb: "create", Group: rbacv1.GroupName, Resource: "rolebindings"},
			{Verb: "bind", Group: rbacv1.GroupName, Resource: "clusterroles", Name: organizationAdminRole},
			{Verb: "create", Group: "security.giantswarm.io", Resource: "organizations"},
		} {
			review, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attrs}}, metav1.CreateOptions{})
			if err != nil {
				return "", fmt.Errorf("access review as %s: %w", u.Email, err)
			}
			if review.Status.Allowed {
				return "", fmt.Errorf("%s may %s %s.%s %s in every namespace: the part needs people with no right outside their organizations", u.Email, attrs.Verb, attrs.Resource, attrs.Group, attrs.Name)
			}
		}
	}
	return fmt.Sprintf("%s, %s and %s may neither create RoleBindings, bind %s nor create Organizations cluster-wide", p.alice.Email, p.bob.Email, p.outsider.Email, organizationAdminRole), nil
}

// callOrganizationTool calls an agent-manager tool through muster as the
// person and returns the tool's envelope.
func (p *adminsProof) callOrganizationTool(person *config.User, tool string, args map[string]any) (*toolEnvelope, error) {
	session, err := openMusterSession(p.cfg, p.musterTokens[person.Email], "organizations-admins")
	if err != nil {
		return nil, err
	}
	session.client.Timeout = adminsCallTimeout
	return session.callToolEnvelope(agentManagerToolPrefix+tool, args)
}

// createOrganization calls create_organization as the person, mode apply.
func (p *adminsProof) createOrganization(person *config.User, name string, extra map[string]any) (*toolEnvelope, error) {
	args := map[string]any{nameKey: name, "displayName": "Admins proof", "harnesses": []string{organizationHarness}, modeArg: modeApply}
	for k, v := range extra {
		args[k] = v
	}
	return p.callOrganizationTool(person, "create_organization", args)
}

// envelopeText is a tool answer's text.
func envelopeText(env *toolEnvelope) string {
	var parts []string
	for _, c := range env.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "\n")
}

// answerOrganization is the organization an agent-manager answer names in
// its top-level organization field, "" for none or an answer that is not a
// JSON object.
func answerOrganization(text string) string {
	var answer struct {
		Organization string `json:"organization"`
	}
	if json.Unmarshal([]byte(text), &answer) != nil {
		return ""
	}
	return answer.Organization
}

// readAdminsBinding reads the namespace's organization-admins; nil when
// there is none.
func readAdminsBinding(ns string) (*rbacv1.RoleBinding, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	binding, err := k.clientset.RbacV1().RoleBindings(ns).Get(ctx, organizationAdminsBinding, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return binding, err
}

// created has the lab's admin create the organization through
// create_organization with its first admins: the answer is created with
// those admins in spec.admins, the organization becomes Ready, and the
// operator's organization-admins binds those admins alone to
// agent-platform:admin.
func (p *adminsProof) created(org string) func() (string, error) {
	return func() (string, error) {
		args, admins := p.caseOf(org)
		step("%s calls %screate_organization %s through muster, mode apply", p.admin.Email, agentManagerToolPrefix, org)
		env, err := p.createOrganization(p.admin, org, args)
		if err != nil {
			return "", err
		}
		text := envelopeText(env)
		if env.IsError {
			return "", fmt.Errorf("create_organization as %s was refused: %s", p.admin.Email, text)
		}
		// Marked before the answer is judged, so that a failed verdict
		// leaves an organization the next run removes first.
		if err := labelProofOrganization(org); err != nil {
			return "", err
		}
		var answer createOrganizationAnswer
		if err := json.Unmarshal([]byte(text), &answer); err != nil {
			return "", fmt.Errorf("create_organization's answer is not the expected JSON: %w\n%.500s", err, text)
		}
		switch {
		case !answer.Created:
			return "", fmt.Errorf("create_organization as %s answered created=false: %.500s", p.admin.Email, text)
		case !slices.Equal(answer.Organization.Admins, admins):
			return "", fmt.Errorf("create_organization's organization has admins %v, want %v: %.500s", answer.Organization.Admins, admins, text)
		}
		ns, err := waitOrganizationReady(org)
		if err != nil {
			return "", err
		}
		p.namespaces[org] = ns
		got, err := waitOrganizationAdmins(ns, admins)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s created %s (namespace %s, Ready): spec.admins %v; the operator's %s/%s binds %s to %s", p.admin.Email, org, ns, admins, ns, organizationAdminsBinding, got, organizationAdminRole), nil
	}
}

// labelProofOrganization marks an Organization the part created through
// agent-manager, which writes no agentlab label, for the part's cleanup.
func labelProofOrganization(name string) error {
	gvr, err := gvrFor(organizationResource)
	if err != nil {
		return err
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{crMetadata: map[string]any{organizationInventoryLabelsKey: map[string]string{organizationsTestLabel: organizationsTestMark}}})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	_, err = k.resource(gvr, "").Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// adminRoleBinding is a RoleBinding of the role in the namespace to the
// subject, which the part writes as a dry run.
func adminRoleBinding(ns, role string, subject rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: adminsDryRunBinding, Namespace: ns, Labels: map[string]string{organizationsTestLabel: organizationsTestMark}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: clusterRoleKind, Name: role},
		Subjects:   []rbacv1.Subject{subject},
	}
}

// refusedAtAdmission reads a write's answer as a refusal by a
// ValidatingAdmissionPolicy: nil for one, an error naming what happened
// otherwise. RBAC's refusal does not count: the admin holds the role and may
// bind it as far as RBAC goes, and the lab's admin is cluster-admin.
func refusedAtAdmission(what string, err error) error {
	switch {
	case err == nil:
		return fmt.Errorf("%s was admitted", what)
	case (apierrors.IsForbidden(err) || apierrors.IsInvalid(err)) && strings.Contains(err.Error(), validatingAdmissionPolicy):
		return nil
	}
	return fmt.Errorf("%s failed, but not as a refusal by a %s: %w", what, validatingAdmissionPolicy, err)
}

// deniedByPolicy reports whether a write's answer is a refusal by the named
// ValidatingAdmissionPolicy: the API server names it in the refusal
// ("ValidatingAdmissionPolicy '<name>' with binding '<binding>' denied
// request: ...").
func deniedByPolicy(err error, policy string) bool {
	return (apierrors.IsForbidden(err) || apierrors.IsInvalid(err)) && strings.Contains(err.Error(), validatingAdmissionPolicy+" '"+policy+"'")
}

// switchOffRefusal reads an update's answer as the switch-off policy's
// refusal: nil for it, an error naming what happened otherwise (the update
// was admitted, or RBAC or another policy refused it).
func switchOffRefusal(what string, err error) error {
	switch {
	case err == nil:
		return fmt.Errorf("%s was admitted; the ValidatingAdmissionPolicy %s (%s) refuses it for every writer", what, organizationSwitchOffPolicy, organizationSwitchOffChart)
	case deniedByPolicy(err, organizationSwitchOffPolicy):
		return nil
	}
	return fmt.Errorf("%s failed, but not as the refusal of the ValidatingAdmissionPolicy %s: %w", what, organizationSwitchOffPolicy, err)
}

// switchOffRefused: the lab's admin, on their own token, sends two updates
// of organization A, Ready with its Agent Platform on, as server-side dry
// runs, so nothing is written whatever the answer. The one that sets
// spec.capabilities.agentPlatform.enabled to false must be refused by the
// chart's switch-off policy; its twin, a change of
// spec.capabilities.agentPlatform.workers.replicas that keeps the switch on,
// must be admitted, so a refusal of every update does not pass. A chart
// without the policy fails the check, naming it.
func (p *adminsProof) switchOffRefused() (string, error) {
	org := adminsOrgA
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	k, err := labKube()
	if err != nil {
		return "", err
	}
	_, err = k.clientset.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, organizationSwitchOffPolicy, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return "", fmt.Errorf("the installed chart has no ValidatingAdmissionPolicy %s, which refuses an organization's switch-off (decision D10): it needs the chart of %s — docs/agents.md \"The Organization kind prototype\"", organizationSwitchOffPolicy, organizationSwitchOffChart)
	case err != nil:
		return "", fmt.Errorf("the ValidatingAdmissionPolicy %s: %w", organizationSwitchOffPolicy, err)
	}
	gvr, err := gvrFor(organizationResource)
	if err != nil {
		return "", err
	}
	admin, _, err := personClients(p.kubeTokens[p.admin.Email])
	if err != nil {
		return "", err
	}
	organizations := admin.Resource(gvr)
	current, err := organizations.Get(ctx, org, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("organization %s as %s: %w", org, p.admin.Email, err)
	}
	state, err := organizationStateFrom(current)
	if err != nil {
		return "", err
	}
	if ready, why := state.ready(); !ready {
		return "", fmt.Errorf("organization %s is not Ready: %s", org, why)
	}
	if on, _, _ := unstructured.NestedBool(current.Object, crSpec, organizationSpecCapabilitiesKey, organizationCapabilityAgentPlatform, "enabled"); !on {
		return "", fmt.Errorf("organization %s does not have spec.capabilities.agentPlatform.enabled true: the switch-off policy holds only an organization whose Agent Platform is on", org)
	}
	replicas, found, err := unstructured.NestedInt64(current.Object, crSpec, organizationSpecCapabilitiesKey, organizationCapabilityAgentPlatform, "workers", "replicas")
	if err != nil {
		return "", fmt.Errorf("organization %s: spec.capabilities.agentPlatform.workers.replicas: %w", org, err)
	}
	if !found {
		replicas = organizationWorkerReplicas
	}
	dry := metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}}
	what := fmt.Sprintf("%s's update of organization %s that sets spec.capabilities.agentPlatform.enabled to false (a dry run)", p.admin.Email, org)
	_, err = organizations.Patch(ctx, org, types.MergePatchType, []byte(`{"spec":{"capabilities":{"agentPlatform":{"enabled":false}}}}`), dry)
	if err := switchOffRefusal(what, err); err != nil {
		return "", err
	}
	twin := replicas + 1
	if _, err := organizations.Patch(ctx, org, types.MergePatchType, fmt.Appendf(nil, `{"spec":{"capabilities":{"agentPlatform":{"workers":{"replicas":%d}}}}}`, twin), dry); err != nil {
		return "", fmt.Errorf("the twin: %s's update of organization %s that sets spec.capabilities.agentPlatform.workers.replicas to %d and keeps the switch on (a dry run): %w", p.admin.Email, org, twin, err)
	}
	return fmt.Sprintf("as %s (cluster-admin), on %s (%s): spec.capabilities.agentPlatform.enabled false refused by the %s %s; spec.capabilities.agentPlatform.workers.replicas %d to %d admitted (dry runs, nothing written)",
		p.admin.Email, org, organizationConditionTypeReady, validatingAdmissionPolicy, organizationSwitchOffPolicy, replicas, twin), nil
}

// adminBindingRefused writes, as dry runs, a RoleBinding to
// agent-platform:admin in organization A's namespace as alice (its admin by
// group) and as the lab's admin: both must be refused by an admission
// policy. The twin: alice's RoleBinding to agent-platform:editor there is
// admitted.
func (p *adminsProof) adminBindingRefused() (string, error) {
	ns := p.namespaces[adminsOrgA]
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	dry := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	outsider := userSubject(p.outsider)
	_, alice, err := personClients(p.kubeTokens[p.alice.Email])
	if err != nil {
		return "", err
	}
	_, err = alice.RbacV1().RoleBindings(ns).Create(ctx, adminRoleBinding(ns, organizationAdminRole, outsider), dry)
	if err := refusedAtAdmission(fmt.Sprintf("%s's RoleBinding %s/%s to %s (a dry run)", p.alice.Email, ns, adminsDryRunBinding, organizationAdminRole), err); err != nil {
		return "", err
	}
	k, err := labKube()
	if err != nil {
		return "", err
	}
	_, err = k.clientset.RbacV1().RoleBindings(ns).Create(ctx, adminRoleBinding(ns, organizationAdminRole, outsider), dry)
	if err := refusedAtAdmission(fmt.Sprintf("the lab admin's RoleBinding %s/%s to %s (a dry run)", ns, adminsDryRunBinding, organizationAdminRole), err); err != nil {
		return "", err
	}
	if _, err := alice.RbacV1().RoleBindings(ns).Create(ctx, adminRoleBinding(ns, organizationEditorRole, outsider), dry); err != nil {
		return "", fmt.Errorf("the twin: %s's RoleBinding %s/%s to %s (a dry run): %w", p.alice.Email, ns, adminsDryRunBinding, organizationEditorRole, err)
	}
	return fmt.Sprintf("in %s, the RoleBinding of %s to %s is refused by a %s as %s (its admin by group) and as the lab's admin; %s's binding to %s is admitted (dry runs)",
		ns, outsider.Name, organizationAdminRole, validatingAdmissionPolicy, p.alice.Email, p.alice.Email, organizationEditorRole), nil
}

// memberToolRefusesAdmin: alice, the admin of organization A, adds the
// outsider with the admin role through add_organization_member: refused,
// and organization-admins does not change. The twin: the same call with the
// user role, a dry run, is admitted.
func (p *adminsProof) memberToolRefusesAdmin() (string, error) {
	ns := p.namespaces[adminsOrgA]
	_, admins := p.caseOf(adminsOrgA)
	user := userSubject(p.outsider).Name
	twin, err := p.callOrganizationTool(p.alice, "add_organization_member", map[string]any{organizationArg: adminsOrgA, argRoleUser: user, argRole: argRoleUser, dryRunArg: true})
	if err != nil {
		return "", err
	}
	if twin.IsError {
		return "", fmt.Errorf("the twin: %s's add_organization_member %s as user in %s (a dry run) was refused: %s", p.alice.Email, user, ns, envelopeText(twin))
	}
	if got := answerOrganization(envelopeText(twin)); got != adminsOrgA {
		return "", fmt.Errorf("the twin: %s's add_organization_member %s as user (a dry run) answered for organization %q, want %s: %.300s", p.alice.Email, user, got, adminsOrgA, envelopeText(twin))
	}
	env, err := p.callOrganizationTool(p.alice, "add_organization_member", map[string]any{organizationArg: adminsOrgA, argRoleUser: user, argRole: argRoleAdmin, modeArg: modeApply})
	if err != nil {
		return "", err
	}
	text := envelopeText(env)
	switch {
	case !env.IsError:
		return "", fmt.Errorf("%s's add_organization_member %s as admin in %s was not refused: %.500s", p.alice.Email, user, ns, text)
	case !strings.Contains(text, memberToolsAdminRefusal):
		// agent-manager's own refusal of the role, before any write; an
		// admission refusal of a tool that tried to write the admin binding
		// carries agent-manager's hint on spec.admins too, but not this.
		return "", fmt.Errorf("%s's add_organization_member %s as admin in %s was refused, but not by agent-manager's refusal of the role (%q): %.500s", p.alice.Email, user, ns, memberToolsAdminRefusal, text)
	}
	binding, err := readAdminsBinding(ns)
	if err != nil {
		return "", err
	}
	if problem := adminsBindingProblem(binding, admins); problem != "" {
		return "", fmt.Errorf("after the refused add of %s as admin in %s: %s", user, ns, problem)
	}
	return fmt.Sprintf("%s's add of %s as admin in %s refused (%s), %s unchanged; as user (a dry run) admitted", p.alice.Email, user, ns, excerpt(text, 200), organizationAdminsBinding), nil
}

// createRefused: alice, an organization's admin without create on
// Organizations, calls create_organization: refused, and no Organization of
// that name exists afterwards.
func (p *adminsProof) createRefused() (string, error) {
	step("%s calls %screate_organization %s through muster, mode apply", p.alice.Email, agentManagerToolPrefix, adminsOrgRefused)
	env, err := p.createOrganization(p.alice, adminsOrgRefused, map[string]any{argAdminUsers: []string{userSubject(p.alice).Name}})
	if err != nil {
		return "", err
	}
	text := envelopeText(env)
	if !env.IsError {
		// alice's create was answered: this run wrote the Organization, so
		// the next run's cleanup may remove it.
		if err := labelProofOrganization(adminsOrgRefused); err != nil {
			note("labelling Organization %s for the next run's cleanup: %v", adminsOrgRefused, err)
		}
		return "", fmt.Errorf("create_organization as %s was not refused: %.500s", p.alice.Email, text)
	}
	gvr, err := gvrFor(organizationResource)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	exists, err := objectExists(ctx, gvr, "", adminsOrgRefused)
	if err != nil {
		return "", err
	}
	if exists {
		// Not labelled for cleanup: the refused call did not create it, so
		// nothing shows this run owns it.
		return "", fmt.Errorf("create_organization as %s was refused, but Organization %s exists", p.alice.Email, adminsOrgRefused)
	}
	return fmt.Sprintf("%s refused, no Organization %s afterwards: %s", p.alice.Email, adminsOrgRefused, excerpt(text, 200)), nil
}

// updateAdmins calls update_organization as the lab's admin with the
// admins arguments.
func (p *adminsProof) updateAdmins(org string, args map[string]any) error {
	args = withArgs(map[string]any{nameKey: org, modeArg: modeApply}, args)
	env, err := p.callOrganizationTool(p.admin, "update_organization", args)
	if err != nil {
		return err
	}
	if env.IsError {
		return fmt.Errorf("update_organization %s as %s was refused: %s", org, p.admin.Email, envelopeText(env))
	}
	return nil
}

// adminsChanged: organization A's admin group is replaced by alice by name;
// the operator's binding follows.
func (p *adminsProof) adminsChanged() (string, error) {
	ns := p.namespaces[adminsOrgA]
	want := []organizationAdmin{adminUser(p.alice)}
	step("%s replaces the admins of %s: no group, user %s", p.admin.Email, adminsOrgA, want[0].Name)
	if err := p.updateAdmins(adminsOrgA, map[string]any{argAdminGroups: []string{}, argAdminUsers: []string{want[0].Name}}); err != nil {
		return "", err
	}
	got, err := waitOrganizationAdmins(ns, want)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("after update_organization, %s/%s binds %s alone to %s; group %s left it", ns, organizationAdminsBinding, got, organizationAdminRole, adminGroup(config.GroupTenantA).Name), nil
}

// adminsEmptied: organization B's admins are emptied; the operator removes
// its binding and the organization stays Ready.
func (p *adminsProof) adminsEmptied() (string, error) {
	ns := p.namespaces[adminsOrgB]
	step("%s removes every admin of %s", p.admin.Email, adminsOrgB)
	if err := p.updateAdmins(adminsOrgB, map[string]any{argAdminUsers: []string{}}); err != nil {
		return "", err
	}
	if _, err := waitOrganizationAdmins(ns, nil); err != nil {
		return "", err
	}
	if _, err := waitOrganizationReady(adminsOrgB); err != nil {
		return "", fmt.Errorf("organization %s without admins: %w", adminsOrgB, err)
	}
	return fmt.Sprintf("with spec.admins empty, %s holds no RoleBinding %s and %s stays Ready", ns, organizationAdminsBinding, adminsOrgB), nil
}

// cleanup removes what the part wrote, unless it failed: then the
// fixtures stay for diagnosis, and the next run removes them first.
func (p *adminsProof) cleanup() {
	if p.failed {
		note("the admins part failed: %s and %s stay for diagnosis; the next run removes them first", adminsOrgA, adminsOrgB)
		return
	}
	if err := removeAdminsFixtures(); err != nil {
		note("cleanup of the admins part: %v", err)
	}
}

// removeAdminsFixtures deletes the part's organizations (with Flux's prune
// marker, which the chart's admission asks of a delete) and waits until
// they and their namespaces are gone. It refuses an organization without the
// part's label.
func removeAdminsFixtures() error {
	gvr, err := gvrFor(organizationResource)
	if err != nil {
		return err
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), organizationCleanupWait+namespaceDeleteTimeout)
	defer cancel()
	var errs []error
	for _, name := range []string{adminsOrgA, adminsOrgB, adminsOrgRefused} {
		org, err := getObject(ctx, gvr, "", name)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if org.GetLabels()[organizationsTestLabel] != organizationsTestMark {
			errs = append(errs, fmt.Errorf("refusing to delete Organization %s without the label %s=%s", name, organizationsTestLabel, organizationsTestMark))
			continue
		}
		step("Deleting organization %s", name)
		state, _ := organizationStateFrom(org)
		resource := k.resource(gvr, "")
		if _, err := resource.Patch(ctx, name, types.MergePatchType, []byte(`{"metadata":{"annotations":{"kustomize.toolkit.fluxcd.io/prune":"enabled"}}}`), metav1.PatchOptions{}); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := resource.Delete(ctx, name, metav1.DeleteOptions{}); ignoreNotFound(err) != nil {
			errs = append(errs, err)
			continue
		}
		if err := wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
			_, err := getObject(ctx, gvr, "", name)
			if !apierrors.IsNotFound(err) {
				return false, ignoreNotFound(err)
			}
			if state.namespace == "" {
				return true, nil
			}
			_, err = getObject(ctx, gvrNamespaces, "", state.namespace)
			return apierrors.IsNotFound(err), ignoreNotFound(err)
		}); err != nil {
			errs = append(errs, fmt.Errorf("organization %s not gone: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
