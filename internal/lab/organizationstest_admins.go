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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agentlab/internal/config"
)

// The admins part of the organizations proof (giantswarm/agentlab#1225,
// docs/agents.md "The admins part"): the admins of a Giant Swarm
// Organization are cluster-admin only inside its org-<name> namespace,
// through a RoleBinding rbac-operator writes (write-all-customer-group, a
// RoleBindingTemplate's copy). The lab runs no rbac-operator, so the part
// writes those bindings itself, with rbac-operator's label, as the lab's
// admin. agent-platform-operator mirrors their Users and Groups into each
// organization namespace as agent-platform:organizations-admin, so such an
// admin creates an organization alone: agent-manager's create_organization
// answers it created with its first admins bound, never pending.
//
// Two Giant Swarm Organizations stand in: alice's, whose binding names her
// group, and bob's, whose binding names him. Neither holds a right
// cluster-wide. Beside them, bindings the operator must not mirror: a
// cluster-admin one written by hand, one whose subjects are a
// ServiceAccount and a system: name, and one held by a finalizer while it
// is deleted. Everything is judged on what the cluster and the tool answer,
// never on a copy of the operator's rule.
const (
	organizationAdminsPart = "admins"
	// adminsParentA and adminsParentB are the two Giant Swarm Organizations'
	// namespaces, the organizations' parents; adminsOrgA and adminsOrgB the
	// organizations their admins create there.
	adminsParentA = "org-agentlab-admins-a"
	adminsParentB = "org-agentlab-admins-b"
	adminsOrgA    = "agentlab-admins-a"
	adminsOrgB    = "agentlab-admins-b"
	// adminsOrgRefused is the organization every refused create names; it
	// never exists.
	adminsOrgRefused = "agentlab-admins-refused"
	// The parent bindings: rbac-operator's to alice's group (with a
	// ServiceAccount beside it) and to bob, one written by hand, one of
	// machines only, and one held by a finalizer.
	adminsGroupBinding    = "agentlab-customer-group"
	adminsUserBinding     = "agentlab-customer-user"
	adminsHandBinding     = "agentlab-hand-written"
	adminsMachinesBinding = "agentlab-machines"
	adminsHeldBinding     = "agentlab-held"
	// adminsHoldFinalizer keeps adminsHeldBinding while it is being deleted.
	adminsHoldFinalizer = "agentlab.giantswarm.io/hold"
	// adminsAutomation is the parent's ServiceAccount, never mirrored.
	adminsAutomation = "automation"
	// adminsSystemUser is a system: name, never mirrored.
	adminsSystemUser = "system:agentlab-admins-proof"
	// rbac-operator's label on every RoleBinding it writes.
	rbacOperatorLabel = "giantswarm.io/managed-by"
	rbacOperatorValue = "rbac-operator"
	// organizationsAdminRole is the ClusterRole every mirror binds;
	// clusterAdminRole the one of rbac-operator's bindings.
	organizationsAdminRole = "agent-platform:organizations-admin"
	clusterAdminRole       = "cluster-admin"
	clusterRoleKind        = "ClusterRole"
	// copiedFromAnnotation names the parent binding a mirror was made from.
	copiedFromAnnotation = "agent-platform.giantswarm.io/copied-from"
	// adminsMirrorWait bounds the operator's reaction to a parent binding.
	adminsMirrorWait = 2 * time.Minute
	// adminsCreateTimeout bounds one create_organization: agent-manager
	// waits up to two minutes for Ready before it answers.
	adminsCreateTimeout = 4 * time.Minute
)

// createOrganizationAnswer is the part of create_organization's answer the
// part reads.
type createOrganizationAnswer struct {
	Created     bool `json:"created"`
	FirstAdmins *struct {
		Name     string `json:"name"`
		Role     string `json:"role"`
		Written  bool   `json:"written"`
		Subjects []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"subjects"`
	} `json:"firstAdmins"`
	Pending string `json:"pending"`
}

// adminsProof is one run of the part.
type adminsProof struct {
	cfg *config.Config
	// alice and bob are each the admin of one Giant Swarm Organization;
	// outsider is named only by the hand-written binding.
	alice, bob, outsider *config.User
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
		return fmt.Errorf("the admins part needs a lab user outside %s besides %s and %s, for the hand-written binding; %s has none", platformAdminsGroup, members[0].Email, members[1].Email, config.File)
	}
	p := &adminsProof{cfg: cfg, alice: members[0], bob: members[1], outsider: outsider,
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
		if u.HasGroup(platformAdminsGroup) || u.Email == members[0].Email || u.Email == members[1].Email {
			continue
		}
		return u
	}
	return nil
}

func (p *adminsProof) run() error {
	if err := p.removeLeftovers(); err != nil {
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
// the refusals while every parent binding is in place, then the changes to
// the parent bindings, which end the admins' rights.
func (p *adminsProof) checks() []organizationCheck {
	return []organizationCheck{
		{surface: surfaceKubernetes, boundary: "the Giant Swarm Organizations' admins hold no right cluster-wide to create or bind RoleBindings", run: p.noRightClusterWide},
		{surface: surfaceAgentManager, boundary: "an admin by group of a Giant Swarm Organization creates an organization alone: created, its first admins bound, not pending", run: p.created(adminsOrgA)},
		{surface: surfaceKubernetes, boundary: "rbac-operator's cluster-admin binding of a group is mirrored as organizations-admin: the group alone, its ServiceAccount left out, the copied-from annotation, nothing binding cluster-admin", run: p.mirrored(adminsOrgA, adminsGroupBinding, groupSubject(config.GroupTenantA))},
		{surface: surfaceAgentManager, boundary: "an admin by name of a Giant Swarm Organization creates an organization alone: created, its first admins bound, not pending", run: p.created(adminsOrgB)},
		{surface: surfaceKubernetes, boundary: "rbac-operator's cluster-admin binding of a user is mirrored as organizations-admin: the user alone, the copied-from annotation, nothing binding cluster-admin", run: p.mirrored(adminsOrgB, adminsUserBinding, userSubject(p.bob))},
		{surface: surfaceKubernetes, boundary: "a cluster-admin binding without rbac-operator's label, and one whose only subjects are a ServiceAccount and a system: name, are not mirrored", run: p.notMirrored},
		{surface: surfaceAgentManager, boundary: "a person whose cluster-admin binding in the parent has no rbac-operator label is refused before any write, naming that binding", run: p.handWrittenRefused},
		{surface: surfaceAgentManager, boundary: "the admin of one Giant Swarm Organization cannot create an organization in the other's namespace, either way", run: p.crossRefused},
		{surface: surfaceKubernetes, boundary: "a parent binding being deleted is not mirrored: its mirror goes while a finalizer still holds the binding", run: p.deletingUnmirrored},
		{surface: surfaceKubernetes, boundary: "a group removed from rbac-operator's binding leaves the organization namespace", run: p.groupRemoved},
	}
}

// adminsBinding is a RoleBinding of a parent namespace to a ClusterRole,
// rbac-operator's when byRBACOperator.
func adminsBinding(ns, name, role string, byRBACOperator bool, subjects ...rbacv1.Subject) *rbacv1.RoleBinding {
	labels := map[string]string{organizationsTestLabel: organizationsTestMark}
	if byRBACOperator {
		labels[rbacOperatorLabel] = rbacOperatorValue
	}
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: organizationInventoryBindingKind},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: clusterRoleKind, Name: role},
		Subjects:   subjects,
	}
}

// serviceAccountSubject is the parent's automation ServiceAccount.
func serviceAccountSubject(ns string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: adminsAutomation, Namespace: ns}
}

// setUp writes the two Giant Swarm Organizations' namespaces and every
// parent binding before any organization exists, so the operator meets them
// all on its first reconcile, and signs the three people in.
func (p *adminsProof) setUp() error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	step("The Giant Swarm Organizations' namespaces %s and %s, and their bindings as rbac-operator writes them (the lab runs no rbac-operator)", adminsParentA, adminsParentB)
	held := adminsBinding(adminsParentA, adminsHeldBinding, clusterAdminRole, true, userSubject(p.alice))
	held.Finalizers = []string{adminsHoldFinalizer}
	bindings := []*rbacv1.RoleBinding{
		adminsBinding(adminsParentA, adminsGroupBinding, clusterAdminRole, true, groupSubject(config.GroupTenantA), serviceAccountSubject(adminsParentA)),
		adminsBinding(adminsParentA, adminsHandBinding, clusterAdminRole, false, userSubject(p.outsider)),
		adminsBinding(adminsParentA, adminsMachinesBinding, clusterAdminRole, true, serviceAccountSubject(adminsParentA), rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: adminsSystemUser}),
		held,
		adminsBinding(adminsParentB, adminsUserBinding, clusterAdminRole, true, userSubject(p.bob)),
	}
	for _, ns := range []string{adminsParentA, adminsParentB} {
		if err := ensureNamespace(ns); err != nil {
			return err
		}
		if err := markNamespace(ctx, ns); err != nil {
			return err
		}
	}
	for _, b := range bindings {
		if _, err := applyTyped(ctx, b); err != nil {
			return fmt.Errorf("RoleBinding %s/%s: %w", b.Namespace, b.Name, err)
		}
		note("%s/%s binds %s to %s (rbac-operator's label: %t)", b.Namespace, b.Name, subjectsLine(b.Subjects), b.RoleRef.Name, b.Labels[rbacOperatorLabel] == rbacOperatorValue)
	}
	for _, u := range []*config.User{p.alice, p.bob, p.outsider} {
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

// markNamespace labels a namespace the part owns, for its cleanup.
func markNamespace(ctx context.Context, ns string) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, organizationsTestLabel, organizationsTestMark)
	_, err = k.clientset.CoreV1().Namespaces().Patch(ctx, ns, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
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
// create RoleBindings and bind agent-platform:admin in every namespace: a
// yes would let create_organization pass on that right instead of the
// mirror.
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
		} {
			review, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attrs}}, metav1.CreateOptions{})
			if err != nil {
				return "", fmt.Errorf("access review as %s: %w", u.Email, err)
			}
			if review.Status.Allowed {
				return "", fmt.Errorf("%s may %s %s.%s %s in every namespace: the part needs people whose right is in their Giant Swarm Organization's namespace only", u.Email, attrs.Verb, attrs.Resource, attrs.Group, attrs.Name)
			}
		}
	}
	return fmt.Sprintf("%s, %s and %s may neither create RoleBindings nor bind %s cluster-wide", p.alice.Email, p.bob.Email, p.outsider.Email, organizationAdminRole), nil
}

// adminsCase is who creates an organization in which parent, with which
// first admins.
type adminsCase struct {
	person *config.User
	parent string
	args   map[string]any
	// admins are the first admins' subject names, as args names them.
	admins []string
}

// caseOf is the create of an organization of the part.
func (p *adminsProof) caseOf(org string) adminsCase {
	if org == adminsOrgA {
		admins := []string{groupSubject(config.GroupTenantA).Name}
		return adminsCase{person: p.alice, parent: adminsParentA, args: map[string]any{"adminGroups": admins}, admins: admins}
	}
	admins := []string{userSubject(p.bob).Name}
	return adminsCase{person: p.bob, parent: adminsParentB, args: map[string]any{"adminUsers": admins}, admins: admins}
}

// createOrganization calls create_organization through muster as the
// person, in mode apply, and returns the tool's envelope.
func (p *adminsProof) createOrganization(person *config.User, parent, name string, extra map[string]any) (*toolEnvelope, error) {
	session, err := openMusterSession(p.cfg, p.musterTokens[person.Email], "organizations-admins")
	if err != nil {
		return nil, err
	}
	session.client.Timeout = adminsCreateTimeout
	args := map[string]any{namespaceKey: parent, nameKey: name, "displayName": "Admins proof", "harnesses": []string{organizationHarness}, "mode": modeApply}
	for k, v := range extra {
		args[k] = v
	}
	if _, ok := args["adminGroups"]; !ok {
		if _, ok := args["adminUsers"]; !ok {
			args["adminUsers"] = []string{userSubject(person).Name}
		}
	}
	return session.callToolEnvelope(agentManagerToolPrefix+"create_organization", args)
}

// envelopeText is a tool answer's text.
func envelopeText(env *toolEnvelope) string {
	var parts []string
	for _, c := range env.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "\n")
}

// created has the organization's Giant Swarm Organization admin create it
// alone: the answer is created with the first admins written and nothing
// pending, the organization is Ready, and its organization-admins binds the
// first admins to agent-platform:admin.
func (p *adminsProof) created(org string) func() (string, error) {
	return func() (string, error) {
		c := p.caseOf(org)
		step("%s calls %screate_organization %s/%s through muster, mode apply", c.person.Email, agentManagerToolPrefix, c.parent, org)
		env, err := p.createOrganization(c.person, c.parent, org, c.args)
		if err != nil {
			return "", err
		}
		text := envelopeText(env)
		if env.IsError {
			return "", fmt.Errorf("create_organization as %s was refused: %s", c.person.Email, text)
		}
		var answer createOrganizationAnswer
		if err := json.Unmarshal([]byte(text), &answer); err != nil {
			return "", fmt.Errorf("create_organization's answer is not the expected JSON: %w\n%.500s", err, text)
		}
		switch {
		case answer.Pending != "":
			return "", fmt.Errorf("create_organization as %s answered pending: %s", c.person.Email, answer.Pending)
		case !answer.Created:
			return "", fmt.Errorf("create_organization as %s answered created=false: %.500s", c.person.Email, text)
		case answer.FirstAdmins == nil || !answer.FirstAdmins.Written:
			return "", fmt.Errorf("create_organization as %s did not write the first admins: %.500s", c.person.Email, text)
		}
		state, err := readOrganization(c.parent, org)
		if err != nil {
			return "", err
		}
		if ok, why := state.ready(); !ok {
			return "", fmt.Errorf("organization %s/%s is not Ready after create_organization: %s", c.parent, org, why)
		}
		p.namespaces[org] = state.namespace
		want := c.admins
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		k, err := labKube()
		if err != nil {
			return "", err
		}
		binding, err := k.clientset.RbacV1().RoleBindings(state.namespace).Get(ctx, organizationAdminsBinding, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("the first admins' RoleBinding %s/%s: %w", state.namespace, organizationAdminsBinding, err)
		}
		var got []string
		for _, s := range binding.Subjects {
			got = append(got, s.Name)
		}
		if binding.RoleRef.Name != organizationAdminRole || !slices.Equal(got, want) {
			return "", fmt.Errorf("RoleBinding %s/%s binds %v to %s, want %v to %s", state.namespace, organizationAdminsBinding, got, binding.RoleRef.Name, want, organizationAdminRole)
		}
		return fmt.Sprintf("%s created %s/%s (namespace %s, Ready); the answer: created, %s written, nothing pending; %s binds %v to %s",
			c.person.Email, c.parent, org, state.namespace, organizationAdminsBinding, organizationAdminsBinding, got, organizationAdminRole), nil
	}
}

// mirrored reads the organization namespace's mirror of a parent binding:
// bound to organizations-admin, the wanted subject alone, annotated with
// the binding it was copied from; and no RoleBinding of the namespace binds
// cluster-admin.
func (p *adminsProof) mirrored(org, name string, want rbacv1.Subject) func() (string, error) {
	return func() (string, error) {
		ns := p.namespaces[org]
		parent := p.caseOf(org).parent
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		k, err := labKube()
		if err != nil {
			return "", err
		}
		mirror, err := k.clientset.RbacV1().RoleBindings(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("the mirror of %s/%s in %s: %w", parent, name, ns, err)
		}
		switch {
		case mirror.RoleRef.Kind != clusterRoleKind || mirror.RoleRef.Name != organizationsAdminRole:
			return "", fmt.Errorf("RoleBinding %s/%s binds %s %s, want ClusterRole %s", ns, name, mirror.RoleRef.Kind, mirror.RoleRef.Name, organizationsAdminRole)
		case len(mirror.Subjects) != 1 || mirror.Subjects[0].Kind != want.Kind || mirror.Subjects[0].Name != want.Name:
			return "", fmt.Errorf("RoleBinding %s/%s binds %s, want %s %s alone", ns, name, subjectsLine(mirror.Subjects), want.Kind, want.Name)
		case mirror.Annotations[copiedFromAnnotation] != parent+"/"+name:
			return "", fmt.Errorf("RoleBinding %s/%s has %s=%q, want %s/%s", ns, name, copiedFromAnnotation, mirror.Annotations[copiedFromAnnotation], parent, name)
		}
		all, err := k.clientset.RbacV1().RoleBindings(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", err
		}
		for _, rb := range all.Items {
			if rb.RoleRef.Name == clusterAdminRole {
				return "", fmt.Errorf("RoleBinding %s/%s binds %s in the organization namespace", ns, rb.Name, clusterAdminRole)
			}
		}
		return fmt.Sprintf("%s/%s binds %s %s to %s, %s=%s; none of the %d RoleBindings of %s binds %s", ns, name, want.Kind, want.Name, organizationsAdminRole, copiedFromAnnotation, parent+"/"+name, len(all.Items), ns, clusterAdminRole), nil
	}
}

// notMirrored checks, once the organization namespace holds the mirror of
// rbac-operator's group binding (the same reconcile), that it holds none of
// the hand-written binding and none of the machines' one.
func (p *adminsProof) notMirrored() (string, error) {
	ns := p.namespaces[adminsOrgA]
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	k, err := labKube()
	if err != nil {
		return "", err
	}
	if _, err := k.clientset.RbacV1().RoleBindings(ns).Get(ctx, adminsGroupBinding, metav1.GetOptions{}); err != nil {
		return "", fmt.Errorf("the mirror of %s/%s, which the negative cases are read beside: %w", adminsParentA, adminsGroupBinding, err)
	}
	for _, name := range []string{adminsHandBinding, adminsMachinesBinding} {
		_, err := k.clientset.RbacV1().RoleBindings(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return "", fmt.Errorf("RoleBinding %s/%s exists: %s/%s was mirrored", ns, name, adminsParentA, name)
		}
		if !apierrors.IsNotFound(err) {
			return "", err
		}
	}
	return fmt.Sprintf("%s holds the mirror of %s and none of %s (cluster-admin to %s, no rbac-operator label) or %s (cluster-admin to ServiceAccount %s/%s and User %s)",
		ns, adminsGroupBinding, adminsHandBinding, userSubject(p.outsider).Name, adminsMachinesBinding, adminsParentA, adminsAutomation, adminsSystemUser), nil
}

// refusedBeforeWrite calls create_organization as the person and wants a
// refusal whose text holds every one of want, and no organization of that
// name in the parent afterwards.
func (p *adminsProof) refusedBeforeWrite(person *config.User, parent string, want ...string) (string, error) {
	step("%s calls %screate_organization %s/%s through muster, mode apply", person.Email, agentManagerToolPrefix, parent, adminsOrgRefused)
	env, err := p.createOrganization(person, parent, adminsOrgRefused, nil)
	if err != nil {
		return "", err
	}
	text := envelopeText(env)
	if !env.IsError {
		return "", fmt.Errorf("create_organization as %s in %s was not refused: %.500s", person.Email, parent, text)
	}
	for _, w := range want {
		if !strings.Contains(text, w) {
			return "", fmt.Errorf("create_organization as %s in %s was refused without %q: %s", person.Email, parent, w, text)
		}
	}
	gvr, err := gvrFor(organizationResource)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	exists, err := objectExists(ctx, gvr, parent, adminsOrgRefused)
	if err != nil {
		return "", err
	}
	if exists {
		return "", fmt.Errorf("create_organization as %s was refused, but %s/%s exists", person.Email, parent, adminsOrgRefused)
	}
	return excerpt(text, 200), nil
}

// handWrittenRefused: the outsider is cluster-admin in parent A by a binding
// without rbac-operator's label, so may write there what create_organization
// asks first; its refusal must come from the check that the right reaches
// the new namespace, which names the binding.
func (p *adminsProof) handWrittenRefused() (string, error) {
	text, err := p.refusedBeforeWrite(p.outsider, adminsParentA, "nothing was written", adminsHandBinding)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s (cluster-admin in %s by %s) refused, no %s/%s afterwards: %s", p.outsider.Email, adminsParentA, adminsHandBinding, adminsParentA, adminsOrgRefused, text), nil
}

// crossRefused: each admin, who created an organization in their own
// Giant Swarm Organization's namespace, is refused in the other's.
func (p *adminsProof) crossRefused() (string, error) {
	var seen []string
	for _, c := range []struct {
		person *config.User
		parent string
	}{{p.bob, adminsParentA}, {p.alice, adminsParentB}} {
		text, err := p.refusedBeforeWrite(c.person, c.parent)
		if err != nil {
			return "", err
		}
		seen = append(seen, fmt.Sprintf("%s in %s: %s", c.person.Email, c.parent, text))
	}
	return "refused, no organization written: " + strings.Join(seen, "; "), nil
}

// waitMirror waits until the organization namespace holds (or no longer
// holds) a RoleBinding of the name.
func waitMirror(ns, name string, present bool) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminsMirrorWait)
	defer cancel()
	err = wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
		_, err := k.clientset.RbacV1().RoleBindings(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return !present, nil
		}
		return present && err == nil, err
	})
	if err != nil {
		return fmt.Errorf("RoleBinding %s/%s present=%t within %s: %w", ns, name, present, adminsMirrorWait, err)
	}
	return nil
}

// deletingUnmirrored deletes the held parent binding, whose finalizer keeps
// it: its mirror must go while the binding still exists, being deleted.
func (p *adminsProof) deletingUnmirrored() (string, error) {
	ns := p.namespaces[adminsOrgA]
	if err := waitMirror(ns, adminsHeldBinding, true); err != nil {
		return "", fmt.Errorf("before the delete: %w", err)
	}
	k, err := labKube()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	step("Deleting %s/%s, which the finalizer %s holds", adminsParentA, adminsHeldBinding, adminsHoldFinalizer)
	if err := k.clientset.RbacV1().RoleBindings(adminsParentA).Delete(ctx, adminsHeldBinding, metav1.DeleteOptions{}); err != nil {
		return "", err
	}
	if err := waitMirror(ns, adminsHeldBinding, false); err != nil {
		return "", err
	}
	held, err := k.clientset.RbacV1().RoleBindings(adminsParentA).Get(ctx, adminsHeldBinding, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("%s/%s is gone, so its mirror's removal does not show a binding being deleted: %w", adminsParentA, adminsHeldBinding, err)
	}
	if held.DeletionTimestamp == nil {
		return "", fmt.Errorf("%s/%s has no deletion timestamp", adminsParentA, adminsHeldBinding)
	}
	if err := releaseHeld(ctx); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s mirrored before the delete; its mirror gone while the binding was still there, deleted since %s", ns, adminsHeldBinding, held.DeletionTimestamp.UTC().Format(time.RFC3339)), nil
}

// releaseHeld removes the finalizer of the held binding, if it is there.
func releaseHeld(ctx context.Context) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	held, err := k.clientset.RbacV1().RoleBindings(adminsParentA).Get(ctx, adminsHeldBinding, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !slices.Contains(held.Finalizers, adminsHoldFinalizer) {
		return nil
	}
	held.Finalizers = slices.DeleteFunc(held.Finalizers, func(f string) bool { return f == adminsHoldFinalizer })
	_, err = k.clientset.RbacV1().RoleBindings(adminsParentA).Update(ctx, held, metav1.UpdateOptions{})
	return err
}

// groupRemoved removes alice's group from rbac-operator's binding, which
// keeps its ServiceAccount: the mirror, left without a subject, must go.
func (p *adminsProof) groupRemoved() (string, error) {
	ns := p.namespaces[adminsOrgA]
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	step("Removing group %s from %s/%s", groupSubject(config.GroupTenantA).Name, adminsParentA, adminsGroupBinding)
	if _, err := applyTyped(ctx, adminsBinding(adminsParentA, adminsGroupBinding, clusterAdminRole, true, serviceAccountSubject(adminsParentA))); err != nil {
		return "", err
	}
	if err := waitMirror(ns, adminsGroupBinding, false); err != nil {
		return "", err
	}
	return fmt.Sprintf("with only ServiceAccount %s/%s left in %s/%s, %s holds no RoleBinding %s", adminsParentA, adminsAutomation, adminsParentA, adminsGroupBinding, ns, adminsGroupBinding), nil
}

// removeLeftovers erases a previous run's organizations and parents.
func (p *adminsProof) removeLeftovers() error {
	return removeAdminsFixtures()
}

// cleanup removes what the part wrote, unless it failed: then the
// fixtures stay for diagnosis, and the next run removes them first.
func (p *adminsProof) cleanup() {
	if p.failed {
		note("the admins part failed: %s and %s, their organizations and bindings stay for diagnosis; the next run removes them first", adminsParentA, adminsParentB)
		return
	}
	if err := removeAdminsFixtures(); err != nil {
		note("cleanup of the admins part: %v", err)
	}
}

// removeAdminsFixtures deletes the part's organizations (with Flux's prune
// marker, which the chart's admission asks of a delete) and waits until
// they and their namespaces are gone, then the parents, which it refuses
// to delete without the part's label.
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
	for _, parent := range []string{adminsParentA, adminsParentB} {
		ns, err := k.clientset.CoreV1().Namespaces().Get(ctx, parent, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ns.Labels[organizationsTestLabel] != organizationsTestMark {
			errs = append(errs, fmt.Errorf("refusing to delete namespace %s without the label %s=%s", parent, organizationsTestLabel, organizationsTestMark))
			continue
		}
		if parent == adminsParentA {
			if err := releaseHeld(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		orgs, err := listObjects(ctx, gvr, parent, "")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, org := range orgs {
			step("Deleting organization %s/%s", parent, org.GetName())
			state, _ := organizationStateFrom(&org)
			resource := k.resource(gvr, parent)
			if _, err := resource.Patch(ctx, org.GetName(), types.MergePatchType, []byte(`{"metadata":{"annotations":{"kustomize.toolkit.fluxcd.io/prune":"enabled"}}}`), metav1.PatchOptions{}); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := resource.Delete(ctx, org.GetName(), metav1.DeleteOptions{}); ignoreNotFound(err) != nil {
				errs = append(errs, err)
				continue
			}
			if err := wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
				_, err := getObject(ctx, gvr, parent, org.GetName())
				if !apierrors.IsNotFound(err) {
					return false, ignoreNotFound(err)
				}
				if state.namespace == "" {
					return true, nil
				}
				_, err = getObject(ctx, gvrNamespaces, "", state.namespace)
				return apierrors.IsNotFound(err), ignoreNotFound(err)
			}); err != nil {
				errs = append(errs, fmt.Errorf("organization %s/%s not gone: %w", parent, org.GetName(), err))
				continue
			}
		}
		if err := deleteNamespace(ctx, parent); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
