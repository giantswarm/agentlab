package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/agentlab/internal/config"
)

// Agent Platform organizations on the lab (platform.organizations;
// giantswarm/giantswarm#38891, #39492). Each organization is an
// Organization (security.giantswarm.io/v1alpha1, cluster-scoped) with
// spec.agentPlatform.enabled, which agent-platform-operator reconciles into
// one organization namespace, agents-<name>, labelled with the organization
// and its UID, and reports in status.agentPlatform.namespace. The lab has no
// KaaS: the agent-platform chart installs the Organization CRD
// (agentPlatformOrganizations.organizationCRD.enabled), and no
// organization-operator runs, so status.namespace (its org-<name>) stays
// empty. The organization's admins are spec.admins: the operator binds them
// to agent-platform:admin in the namespace (RoleBinding organization-admins),
// and the chart's admission policy refuses every other writer of a binding to
// that role, the lab's admin too. `agentlab up` keeps one default
// organization with the lab's admins, its editors and a model, so a person can
// create an agent at once (docs/agents.md "Organizations").
const (
	// defaultOrganization is the organization `up` keeps.
	defaultOrganization = "default"
	// defaultOrganizationDisplayName is its display name.
	defaultOrganizationDisplayName = "Lab"
	// organizationResource is the Organization CRD.
	organizationResource   = "organizations.security.giantswarm.io"
	organizationAPIVersion = "security.giantswarm.io/v1alpha1"
	organizationKind       = "Organization"
	// organizationDisplayNameAnnotation is the organization's display name
	// (a metadata annotation, not a field).
	organizationDisplayNameAnnotation = "agent-platform.giantswarm.io/display-name"
	// organizationLabel marks an organization namespace; its value is the
	// organization's name. organizationUIDLabel holds the organization's UID.
	organizationLabel    = "agent-platform.giantswarm.io/organization"
	organizationUIDLabel = "agent-platform.giantswarm.io/organization-uid"
	// organizationAdminsBinding is the admins' RoleBinding, which
	// agent-platform-operator alone writes from spec.admins.
	organizationAdminsBinding = "organization-admins"
	organizationAdminRole     = "agent-platform:admin"
	// organizationAdminUser and organizationAdminGroup are the kinds of a
	// spec.admins entry.
	organizationAdminUser  = "User"
	organizationAdminGroup = "Group"
	// organizationEditorsBinding binds the people who create agents.
	organizationEditorsBinding = "organization-editors"
	organizationEditorRole     = "agent-platform:editor"
	// organizationUserRole lets a person see an organization's agents and
	// start conversations with them.
	organizationUserRole = "agent-platform:user"
	// organizationModel is the ModelConfig the lab gives an organization,
	// and organizationModelSecret its key: a tenant credential (the chart
	// admits a ModelConfig's key only from a Secret with that prefix), under
	// the key every lab Secret of the Anthropic key holds it (anthropic.go).
	organizationModel          = "anthropic"
	organizationModelSecret    = "tenant-credential-anthropic" // #nosec G101 -- a Secret's NAME, not a credential
	organizationModelSecretKey = anthropicSecretKey
	// The default-ModelConfig marks: the operator release the chart pins
	// reads the first, later releases the second (agent-platform-operator#16,
	// agent-platform#1276). The lab's one model carries both.
	defaultModelConfigAnnotation      = "agent-platform.giantswarm.io/default-model-config"
	isDefaultModelConfigAnnotation    = "agent-platform.giantswarm.io/is-default-model-config"
	organizationReadyTimeout          = 3 * time.Minute
	organizationRBACSettle            = 45 * time.Second
	organizationHarness               = kagentHarness
	organizationWorkerReplicas        = 1
	organizationModelProvider         = "Anthropic"
	organizationModelConfigAPIVersion = kagentAPIVersion
	organizationModelConfigKind       = "ModelConfig"
	organizationModelResolveTimeout   = 2 * time.Minute
	organizationReasonNotObserved     = "NotObserved"
	organizationConditionTypeReady    = conditionReady
	organizationReadyStatusTrue       = conditionTrue
	// The Organization's status: the Agent Platform's part under
	// status.agentPlatform, the conditions generic.
	organizationStatusAgentPlatformKey = "agentPlatform"
	organizationStatusNamespaceKey     = "namespace"
	organizationStatusConditionsKey    = "conditions"
	organizationStatusErasureStepKey   = "erasureStep"
)

// organizationAdmin is one entry of an Organization's spec.admins: a User or
// a Group by the name the API server authenticates it with.
type organizationAdmin struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// adminGroup is a lab group as an admin.
func adminGroup(group string) organizationAdmin {
	return organizationAdmin{Kind: organizationAdminGroup, Name: apiserverUserPrefix + group}
}

// adminUser is a lab person as an admin.
func adminUser(u *config.User) organizationAdmin {
	return organizationAdmin{Kind: organizationAdminUser, Name: apiserverUserPrefix + u.Email}
}

// subjectLine is the admin as subjectsLine names a binding's subject.
func (a organizationAdmin) subjectLine() string {
	return a.Kind + " " + a.Name
}

// organizationCondition is one condition of an organization's status.
type organizationCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	ObservedGeneration int64  `json:"observedGeneration"`
}

// organizationState is what the proofs read of an organization: its
// namespace and whether the operator reports it Ready for its current
// generation.
type organizationState struct {
	name, namespace string
	generation      int64
	conditions      []organizationCondition
	deleting        bool
	erasureStep     string
}

// organizationStateFrom reads an Organization: the Agent Platform's
// namespace and erasure step from status.agentPlatform, the conditions from
// status.conditions.
func organizationStateFrom(obj *unstructured.Unstructured) (organizationState, error) {
	s := organizationState{name: obj.GetName(), generation: obj.GetGeneration(), deleting: obj.GetDeletionTimestamp() != nil}
	s.namespace, _, _ = unstructured.NestedString(obj.Object, "status", organizationStatusAgentPlatformKey, organizationStatusNamespaceKey)
	s.erasureStep, _, _ = unstructured.NestedString(obj.Object, "status", organizationStatusAgentPlatformKey, organizationStatusErasureStepKey)
	raw, found, _ := unstructured.NestedSlice(obj.Object, "status", organizationStatusConditionsKey)
	if !found {
		return s, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s.conditions); err != nil {
		return s, fmt.Errorf("organization %s: status.conditions: %w", s.name, err)
	}
	return s, nil
}

// condition is the named condition, and whether the status has it.
func (s organizationState) condition(kind string) (organizationCondition, bool) {
	for _, c := range s.conditions {
		if c.Type == kind {
			return c, true
		}
	}
	return organizationCondition{}, false
}

// ready reports whether the operator reports the organization Ready for
// its current generation with a namespace; otherwise why not.
func (s organizationState) ready() (bool, string) {
	c, ok := s.condition(organizationConditionTypeReady)
	switch {
	case s.deleting:
		return false, fmt.Sprintf("being deleted (cleanup step %q)", s.erasureStep)
	case !ok:
		return false, "no Ready condition yet: is agent-platform-operator running?"
	case c.ObservedGeneration < s.generation:
		return false, fmt.Sprintf("Ready reports generation %d, the organization is at %d (%s)", c.ObservedGeneration, s.generation, organizationReasonNotObserved)
	case c.Status != organizationReadyStatusTrue:
		return false, fmt.Sprintf("Ready=%s %s: %s", c.Status, c.Reason, c.Message)
	case s.namespace == "":
		return false, "Ready without status.agentPlatform.namespace"
	}
	return true, ""
}

// organizationManifest is an Organization of the lab with the Agent
// Platform on: the platform Harness, one worker and its admins.
func organizationManifest(name, displayName string, admins []organizationAdmin) string {
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: %s
kind: %s
metadata:
  name: %s
  annotations:
    %s: %q
  labels:
    %s: %s
spec:
`, organizationAPIVersion, organizationKind, name, organizationDisplayNameAnnotation, displayName, managedByLabel, managedByAgentlabValue)
	if len(admins) > 0 {
		b.WriteString("  admins:\n")
		for _, a := range admins {
			fmt.Fprintf(&b, "    - kind: %s\n      name: %q\n", a.Kind, a.Name)
		}
	}
	fmt.Fprintf(&b, `  agentPlatform:
    enabled: true
    harnesses: [%s]
    workers:
      replicas: %d
`, organizationHarness, organizationWorkerReplicas)
	return b.String()
}

// ensureOrganization applies the Organization with its admins, as the lab's
// admin (cluster-admin, which holds what agent-platform:organizations-admin
// grants), and waits until the operator reports it Ready; it returns the
// organization namespace.
func ensureOrganization(name, displayName string, admins []organizationAdmin) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if _, err := applyManifests(ctx, []byte(organizationManifest(name, displayName, admins))); err != nil {
		return "", fmt.Errorf("the Organization %s: %w", name, err)
	}
	return waitOrganizationReady(name)
}

// readOrganization reads one organization's state.
func readOrganization(name string) (organizationState, error) {
	gvr, err := gvrFor(organizationResource)
	if err != nil {
		return organizationState{}, fmt.Errorf("the apiserver serves no %s: is the agent-platform chart's Organization CRD installed (platform.organizations)? %w", organizationResource, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	obj, err := getObject(ctx, gvr, "", name)
	if err != nil {
		return organizationState{}, err
	}
	return organizationStateFrom(obj)
}

// waitOrganizationReady waits for the organization's Ready, bounded.
func waitOrganizationReady(name string) (string, error) {
	var state organizationState
	var why string
	var readErr error
	ready := waitFor(int(organizationReadyTimeout/pollInterval), pollInterval, func() bool {
		state, readErr = readOrganization(name)
		if readErr != nil {
			return false
		}
		var ok bool
		ok, why = state.ready()
		return ok
	})
	switch {
	case ready:
		return state.namespace, nil
	case readErr != nil:
		return "", readErr
	}
	return "", fmt.Errorf("organization %s not Ready within %s: %s — `KUBECONFIG=state/kubeconfig kubectl get %s %s -o yaml` and `kubectl -n %s logs deploy/agent-platform-operator`",
		name, organizationReadyTimeout, why, organizationResource, name, platformNamespace)
}

// setOrganizationAdmins replaces the Organization's spec.admins, as the
// lab's admin; agent-platform-operator follows it with organization-admins.
func setOrganizationAdmins(name string, admins []organizationAdmin) error {
	gvr, err := gvrFor(organizationResource)
	if err != nil {
		return err
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	if admins == nil {
		admins = []organizationAdmin{}
	}
	patch, err := json.Marshal(map[string]any{crSpec: map[string]any{"admins": admins}})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if _, err := k.resource(gvr, "").Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("spec.admins of Organization %s: %w", name, err)
	}
	return nil
}

// adminsBindingProblem is why the namespace's organization-admins does not
// bind exactly the admins, in their order, to agent-platform:admin; "" when
// it does. No admins wants no binding.
func adminsBindingProblem(binding *rbacv1.RoleBinding, admins []organizationAdmin) string {
	if binding == nil {
		if len(admins) == 0 {
			return ""
		}
		return fmt.Sprintf("no RoleBinding %s", organizationAdminsBinding)
	}
	want := make([]string, 0, len(admins))
	for _, a := range admins {
		want = append(want, a.subjectLine())
	}
	got := make([]string, 0, len(binding.Subjects))
	for _, s := range binding.Subjects {
		got = append(got, s.Kind+" "+s.Name)
	}
	switch {
	case len(admins) == 0 && len(got) > 0:
		return fmt.Sprintf("%s binds %v, want no admin", organizationAdminsBinding, got)
	case len(admins) > 0 && (binding.RoleRef.Kind != clusterRoleKind || binding.RoleRef.Name != organizationAdminRole):
		return fmt.Sprintf("%s binds %s %s, want ClusterRole %s", organizationAdminsBinding, binding.RoleRef.Kind, binding.RoleRef.Name, organizationAdminRole)
	case !slices.Equal(got, want):
		return fmt.Sprintf("%s binds %v, want %v", organizationAdminsBinding, got, want)
	}
	return ""
}

// waitOrganizationAdmins waits until the operator's organization-admins in
// the namespace binds exactly the admins (none: no binding), bounded, and
// returns its subjects as they read.
func waitOrganizationAdmins(ns string, admins []organizationAdmin) (string, error) {
	k, err := labKube()
	if err != nil {
		return "", err
	}
	var why string
	var readErr error
	var seen []rbacv1.Subject
	ok := waitFor(int(organizationReadyTimeout/pollInterval), pollInterval, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		binding, err := k.clientset.RbacV1().RoleBindings(ns).Get(ctx, organizationAdminsBinding, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			binding, readErr = nil, nil
		case err != nil:
			readErr = err
			return false
		default:
			readErr = nil
		}
		seen = nil
		if binding != nil {
			seen = binding.Subjects
		}
		why = adminsBindingProblem(binding, admins)
		return why == ""
	})
	switch {
	case ok:
		return subjectsLine(seen), nil
	case readErr != nil:
		return "", readErr
	}
	return "", fmt.Errorf("RoleBinding %s/%s did not follow spec.admins within %s: %s — agent-platform-operator writes it from spec.admins (agent-platform-operator 0.5 line, giantswarm/agent-platform-operator#69): `kubectl -n %s logs deploy/agent-platform-operator`",
		ns, organizationAdminsBinding, organizationReadyTimeout, why, platformNamespace)
}

// bindOrganizationRole writes a RoleBinding of a ClusterRole in the
// organization namespace to the subjects.
func bindOrganizationRole(ns, binding, role string, subjects []rbacv1.Subject) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	_, err := applyTyped(ctx, &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: binding, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
		Subjects:   subjects,
	})
	if err != nil {
		return fmt.Errorf("RoleBinding %s/%s to %s: %w", ns, binding, role, err)
	}
	return nil
}

// groupSubject is a lab group as the API server spells it in a binding.
func groupSubject(group string) rbacv1.Subject {
	return rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: apiserverUserPrefix + group}
}

// userSubject is a lab person as the API server spells them in a binding.
func userSubject(u *config.User) rbacv1.Subject {
	return rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: apiserverUserPrefix + u.Email}
}

// organizationModelManifest is the organization's ModelConfig: Anthropic on
// the lab's model with the key from the tenant credential, marked default;
// the chart's admission policy fills in the LLM listener as its baseUrl.
func organizationModelManifest(ns, model string) string {
	return fmt.Sprintf(`apiVersion: %s
kind: %s
metadata:
  name: %s
  namespace: %s
  annotations:
    %s: "true"
    %s: "true"
  labels:
    %s: %s
spec:
  provider: %s
  model: %s
  apiKeySecret: %s
  apiKeySecretKey: %s
`, organizationModelConfigAPIVersion, organizationModelConfigKind, organizationModel, ns,
		defaultModelConfigAnnotation, isDefaultModelConfigAnnotation, managedByLabel, managedByAgentlabValue,
		organizationModelProvider, model, organizationModelSecret, organizationModelSecretKey)
}

// ensureOrganizationModel gives the organization namespace its model: the
// Anthropic key placed in the tenant credential the way the lab places it
// everywhere (anthropic.go: the recorded source, the host environment, else
// the placeholder; agentlab never reads the value), then the ModelConfig.
// The operator runs no worker while the namespace holds no ModelConfig.
func ensureOrganizationModel(cfg *config.Config, ns string) error {
	if _, err := ensureAnthropicKey(cfg, anthropicKeySecret{ns, organizationModelSecret, "the organization's ModelConfig " + ns + "/" + organizationModel}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if _, err := applyManifests(ctx, []byte(organizationModelManifest(ns, cfg.AIModel))); err != nil {
		return fmt.Errorf("the ModelConfig %s/%s: %w", ns, organizationModel, err)
	}
	return nil
}

// ensureDefaultOrganization is `up`'s part: the default organization,
// Ready, with platform-admins in spec.admins (agent-platform-operator binds
// them as its admins), its editors (developers) and its model; then a
// developer's create of an agent there, as a server-side dry run with their
// own token, which RBAC and every admission policy of the namespace (the
// HelmRelease field lock among them) judge.
func ensureDefaultOrganization(cfg *config.Config) (string, error) {
	step("The default organization %s (Organization), its admins, editors and model", defaultOrganization)
	ns, err := ensureOrganization(defaultOrganization, defaultOrganizationDisplayName, []organizationAdmin{adminGroup(platformAdminsGroup)})
	if err != nil {
		return "", err
	}
	if err := bindOrganizationRole(ns, organizationEditorsBinding, organizationEditorRole, []rbacv1.Subject{groupSubject(developersGroup)}); err != nil {
		return "", err
	}
	if err := ensureOrganizationModel(cfg, ns); err != nil {
		return "", err
	}
	editor := cfg.FindUserInGroup(developersGroup)
	if editor == nil {
		note("no user in %s: nobody but the platform admins creates agents in %s", developersGroup, ns)
		return ns, nil
	}
	if err := dryRunAgentCreate(cfg, ns, editor); err != nil {
		return "", fmt.Errorf("the default organization %s is Ready, but %s (group %s) cannot create an agent in %s: %w — check RoleBinding %s/%s and the chart's ClusterRole %s",
			defaultOrganization, editor.Email, developersGroup, ns, err, ns, organizationEditorsBinding, organizationEditorRole)
	}
	note("%s Ready in namespace %s; %s may create agents there (a dry run as them)", defaultOrganization, ns, editor.Email)
	return ns, nil
}

// dryRunAgentCreate creates an agent's HelmRelease in the namespace as the
// person, with their own Dex token, as a server-side dry run.
func dryRunAgentCreate(cfg *config.Config, ns string, person *config.User) error {
	token, err := passwordGrant(cfg, config.KubernetesClientID, config.KubernetesClientSecret, person.Email, person.Password, loginScopes)
	if err != nil {
		return err
	}
	source, err := organizationChartSource(ns)
	if err != nil {
		return err
	}
	dyn, _, err := personClients(token)
	if err != nil {
		return err
	}
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	release := organizationAgentRelease(ns, organizationsTestCrossAgent, source, organizationModel)
	_, err = dyn.Resource(gvr).Namespace(ns).Create(ctx, release, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	return err
}

// organizationsHint is up's summary line on the organizations.
func organizationsHint(cfg *config.Config, ns string) string {
	if why := cfg.OrganizationsInactive(); why != "" {
		return "  Organizations are off: " + why + "."
	}
	if !cfg.OrganizationsEnabled() {
		return "  Organizations are off (platform.organizations in agentlab.yaml)."
	}
	return fmt.Sprintf("  Organizations: the default Organization %s runs its agents in namespace %s (developers create agents there).",
		defaultOrganization, ns)
}

// organizationNamespaces lists the organization namespaces (labelled by the
// operator), as the lab's admin.
func organizationNamespaces() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	items, err := listObjects(ctx, gvrNamespaces, "", organizationLabel)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, item := range items {
		out = append(out, item.GetName())
	}
	return out, nil
}
