package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

const (
	organizationInventoryLabelsKey   = "labels"
	organizationInventoryRoleKind    = "Role"
	organizationInventoryBindingKind = "RoleBinding"
)

func prepareOrganizationInventory(ctx context.Context, organization *unstructured.Unstructured) (func() error, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	deployments := k.clientset.AppsV1().Deployments(platformNamespace)
	deployment, err := deployments.Get(ctx, "agent-platform-operator", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	argument := cleanupOperatorArgument()
	arguments := deployment.Spec.Template.Spec.Containers[0].Args
	for _, existing := range arguments {
		if strings.HasPrefix(existing, "--installation-kustomization=") && existing != argument {
			return nil, fmt.Errorf("operator already uses another installation Kustomization")
		}
	}
	if err := requireKustomizationAPI(ctx); err != nil {
		return nil, err
	}
	if err := organizationInventoryAccess(ctx); err != nil {
		return nil, err
	}
	resource := k.resource(kustomizationGVR, platformNamespace)
	fixture, err := resource.Get(ctx, organizationCleanupFixture, metav1.GetOptions{})
	if err == nil && fixture.GetLabels()[organizationsTestLabel] != organizationsTestMark {
		return nil, fmt.Errorf("inventory fixture %s is not proof-owned", organizationCleanupFixture)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	manifest := strings.ReplaceAll(string(gitopsFixtureKustomization()), gitopsFluxFixture, organizationCleanupFixture)
	if _, err := applyManifests(ctx, []byte(manifest)); err != nil {
		return nil, err
	}
	labelPatch, _ := json.Marshal(map[string]any{crMetadata: map[string]any{organizationInventoryLabelsKey: map[string]string{organizationsTestLabel: organizationsTestMark}}})
	if _, err := resource.Patch(ctx, organizationCleanupFixture, types.MergePatchType, labelPatch, metav1.PatchOptions{}); err != nil {
		return nil, err
	}
	if err := writeOrganizationInventory(ctx, organization); err != nil {
		return nil, err
	}
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return nil, err
	}
	provenance, _ := json.Marshal(map[string]any{crMetadata: map[string]any{organizationInventoryLabelsKey: map[string]string{"kustomize.toolkit.fluxcd.io/name": organizationCleanupFixture, "kustomize.toolkit.fluxcd.io/namespace": platformNamespace}}})
	if _, err := k.resource(organizationGVR, "").Patch(ctx, organization.GetName(), types.MergePatchType, provenance, metav1.PatchOptions{}); err != nil {
		return nil, err
	}
	addedArgument := !slices.Contains(arguments, argument)
	if addedArgument {
		deployment.Spec.Template.Spec.Containers[0].Args = append(arguments, argument)
	}
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = map[string]string{}
	}
	deployment.Spec.Template.Annotations[organizationsTestLabel] = time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := deployments.Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		return nil, err
	}
	if err := waitDeploymentRolledOut(ctx, platformNamespace, deployment.Name, 3*time.Minute); err != nil {
		return nil, err
	}
	return func() error {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, err := restoreOperatorSetting(cleanup); err != nil {
			return err
		}
		return removeInventoryFixture(cleanup)
	}, nil
}

// cleanupOperatorArgument is the operator's flag that the cleanup proof adds:
// the fixture Kustomization as the installation's.
func cleanupOperatorArgument() string {
	return "--installation-kustomization=" + platformNamespace + "/" + organizationCleanupFixture
}

// withoutCleanupSetting is the operator's pod template without what the
// cleanup proof set: its flag and its restart annotation. changed reports
// whether either was there.
func withoutCleanupSetting(arguments []string, annotations map[string]string) (kept []string, changed bool) {
	argument := cleanupOperatorArgument()
	kept = slices.DeleteFunc(slices.Clone(arguments), func(value string) bool { return value == argument })
	_, annotated := annotations[organizationsTestLabel]
	return kept, annotated || len(kept) != len(arguments)
}

// restoreOperatorSetting gives the operator's Deployment its own setting
// again: the proof's flag and annotation go, and the rollout is waited for.
// It reports whether anything was the proof's.
func restoreOperatorSetting(ctx context.Context) (bool, error) {
	k, err := labKube()
	if err != nil {
		return false, err
	}
	deployments := k.clientset.AppsV1().Deployments(platformNamespace)
	current, err := deployments.Get(ctx, "agent-platform-operator", metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	kept, changed := withoutCleanupSetting(current.Spec.Template.Spec.Containers[0].Args, current.Spec.Template.Annotations)
	if !changed {
		return false, nil
	}
	delete(current.Spec.Template.Annotations, organizationsTestLabel)
	current.Spec.Template.Spec.Containers[0].Args = kept
	if _, err := deployments.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		return true, err
	}
	return true, waitDeploymentRolledOut(ctx, platformNamespace, current.Name, 3*time.Minute)
}

// removeInventoryFixture deletes the fixture Kustomization and its Role and
// RoleBinding. Each must carry the proof's ownership label: one without it is
// refused, by name, and kept.
func removeInventoryFixture(ctx context.Context) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	fixture, err := k.resource(kustomizationGVR, platformNamespace).Get(ctx, organizationCleanupFixture, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case fixture.GetLabels()[organizationsTestLabel] != organizationsTestMark:
		return fmt.Errorf("refusing to delete the Kustomization %s/%s without the proof ownership label %s=%s", platformNamespace, organizationCleanupFixture, organizationsTestLabel, organizationsTestMark)
	default:
		if err := deleteObject(ctx, kustomizationGVR, platformNamespace, organizationCleanupFixture, gitopsFixtureWait); err != nil {
			return err
		}
	}
	rbac := k.clientset.RbacV1()
	binding, err := rbac.RoleBindings(platformNamespace).Get(ctx, organizationCleanupFixture, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case binding.Labels[organizationsTestLabel] != organizationsTestMark:
		return fmt.Errorf("refusing to delete the RoleBinding %s/%s without the proof ownership label %s=%s", platformNamespace, organizationCleanupFixture, organizationsTestLabel, organizationsTestMark)
	default:
		if err := rbac.RoleBindings(platformNamespace).Delete(ctx, organizationCleanupFixture, metav1.DeleteOptions{}); ignoreNotFound(err) != nil {
			return err
		}
	}
	role, err := rbac.Roles(platformNamespace).Get(ctx, organizationCleanupFixture, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case role.Labels[organizationsTestLabel] != organizationsTestMark:
		return fmt.Errorf("refusing to delete the Role %s/%s without the proof ownership label %s=%s", platformNamespace, organizationCleanupFixture, organizationsTestLabel, organizationsTestMark)
	}
	return ignoreNotFound(rbac.Roles(platformNamespace).Delete(ctx, organizationCleanupFixture, metav1.DeleteOptions{}))
}

func settledOrganizationInventory(fixture *unstructured.Unstructured, organizations ...*unstructured.Unstructured) ([]byte, error) {
	patch, err := inventoryStatus(organizations...)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := json.Unmarshal(patch, &document); err != nil {
		return nil, err
	}
	status := document[crStatus].(map[string]any)
	status["observedGeneration"] = fixture.GetGeneration()
	status[crConditions] = []map[string]any{{fieldTypeKey: conditionReady, crStatus: conditionTrue, "reason": "ReconciliationSucceeded", "message": "Suspended proof inventory", "observedGeneration": fixture.GetGeneration(), "lastTransitionTime": time.Now().UTC().Format(time.RFC3339)}}
	return json.Marshal(document)
}

func writeOrganizationInventory(ctx context.Context, organizations ...*unstructured.Unstructured) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	resource := k.resource(kustomizationGVR, platformNamespace)
	fixture, err := resource.Get(ctx, organizationCleanupFixture, metav1.GetOptions{})
	if err != nil {
		return err
	}
	patch, err := settledOrganizationInventory(fixture, organizations...)
	if err != nil {
		return err
	}
	_, err = resource.Patch(ctx, fixture.GetName(), types.MergePatchType, patch, metav1.PatchOptions{}, crStatus)
	return err
}

// waitOrganizationOrphaned waits until the operator reports the organization
// Orphaned=expected with reason for its current generation.
func waitOrganizationOrphaned(ctx context.Context, name string, expected bool, reason string) error {
	var last organizationState
	err := wait.PollUntilContextTimeout(ctx, pollInterval, organizationReadyTimeout, true, func(context.Context) (bool, error) {
		state, err := readOrganization(name)
		if err != nil {
			return false, err
		}
		last = state
		return orphanedAs(state, expected, reason), nil
	})
	if err != nil {
		return errors.Join(err, fmt.Errorf("organization %s has no current Orphaned=%t with reason %s: %+v", name, expected, reason, last.conditions))
	}
	return nil
}

// orphanedAs reports whether state has a current Orphaned condition with
// status expected and reason.
func orphanedAs(state organizationState, expected bool, reason string) bool {
	condition, found := state.condition(orphanedCondition)
	return found && condition.ObservedGeneration >= state.generation &&
		(condition.Status == conditionTrue) == expected && condition.Reason == reason
}

func markProofOrganization(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return err
	}
	organization, err := getObject(ctx, organizationGVR, "", name)
	if err != nil {
		return err
	}
	if organization.GetLabels()[managedByLabel] != managedByAgentlabValue {
		return fmt.Errorf("organization %s is not managed by agentlab", name)
	}
	return labelProofOrganization(name)
}

func checkProofOrganization(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return err
	}
	organization, err := getObject(ctx, organizationGVR, "", name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if organization.GetLabels()[managedByLabel] != managedByAgentlabValue {
		return fmt.Errorf("refusing to adopt organization %s without agentlab ownership", name)
	}
	return nil
}

func organizationInventoryAccess(ctx context.Context) error {
	clients, err := labKube()
	if err != nil {
		return err
	}
	role, err := clients.clientset.RbacV1().Roles(platformNamespace).Get(ctx, organizationCleanupFixture, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && role.Labels[organizationsTestLabel] != organizationsTestMark {
		return fmt.Errorf("inventory Role is not proof-owned")
	}
	binding, err := clients.clientset.RbacV1().RoleBindings(platformNamespace).Get(ctx, organizationCleanupFixture, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && binding.Labels[organizationsTestLabel] != organizationsTestMark {
		return fmt.Errorf("inventory RoleBinding is not proof-owned")
	}
	metadata := metav1.ObjectMeta{Name: organizationCleanupFixture, Namespace: platformNamespace, Labels: map[string]string{organizationsTestLabel: organizationsTestMark}}
	if _, err := applyTyped(ctx, &rbacv1.Role{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: organizationInventoryRoleKind}, ObjectMeta: metadata,
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{kustomizationGVR.Group}, Resources: []string{kustomizationGVR.Resource}, Verbs: []string{"get", "watch"}}}}); err != nil {
		return err
	}
	_, err = applyTyped(ctx, &rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: organizationInventoryBindingKind}, ObjectMeta: metadata,
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: organizationInventoryRoleKind, Name: organizationCleanupFixture},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: platformNamespace, Name: "agent-platform-operator"}}})
	return err
}

// leftoverAction is the next step of erasing a proof organization that an
// earlier failed run of the cleanup proof left.
type leftoverAction int

const (
	// leftoverWait: the operator has not judged the organization yet (its
	// condition is stale, or still InInventory while the emptied inventory
	// reaches it), or not yet answered the delete pull request annotation.
	leftoverWait leftoverAction = iota
	// leftoverMarkDeletePullRequest: Orphaned=True, MissingFromGit; the
	// delete pull request annotation turns it into DeleteMerged.
	leftoverMarkDeletePullRequest
	// leftoverDelete: the chart's delete-intent policy admits the delete,
	// or the organization's deletion already runs.
	leftoverDelete
)

// nextLeftoverAction is the next step for a leftover organization in state,
// annotated when it carries the delete pull request annotation. An
// organization without an Orphaned condition is not judged applied from git
// and is deleted with Flux's prune marker alone; one Orphaned from git goes
// the path of a merged delete pull request, as the proof itself does.
func nextLeftoverAction(state organizationState, annotated bool) leftoverAction {
	if state.deleting {
		return leftoverDelete
	}
	condition, found := state.condition(orphanedCondition)
	switch {
	case !found:
		return leftoverDelete
	case condition.ObservedGeneration < state.generation, condition.Status != conditionTrue:
		return leftoverWait
	case condition.Reason == orphanedDeleteMerged:
		return leftoverDelete
	case annotated:
		return leftoverWait
	}
	return leftoverMarkDeletePullRequest
}

// cleanupLeftoversFound reports whether an earlier run of the cleanup proof
// left something: the fixture Kustomization, its Role or RoleBinding, the
// operator's flag or annotation, or an organization labelled as applied by
// the fixture.
func cleanupLeftoversFound(ctx context.Context, names []string) (bool, error) {
	k, err := labKube()
	if err != nil {
		return false, err
	}
	if err := requireKustomizationAPI(ctx); err == nil {
		if found, err := objectExists(ctx, kustomizationGVR, platformNamespace, organizationCleanupFixture); err != nil || found {
			return found, err
		}
	}
	if _, err := k.clientset.RbacV1().Roles(platformNamespace).Get(ctx, organizationCleanupFixture, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		return err == nil, err
	}
	if _, err := k.clientset.RbacV1().RoleBindings(platformNamespace).Get(ctx, organizationCleanupFixture, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		return err == nil, err
	}
	deployment, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, "agent-platform-operator", metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if _, changed := withoutCleanupSetting(deployment.Spec.Template.Spec.Containers[0].Args, deployment.Spec.Template.Annotations); changed {
		return true, nil
	}
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return false, err
	}
	for _, name := range names {
		organization, err := getObject(ctx, organizationGVR, "", name)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if organization.GetLabels()[gitopsFluxName] == organizationCleanupFixture {
			return true, nil
		}
	}
	return false, nil
}

// removeCleanupLeftovers removes, before the run, what an earlier failed run
// of the cleanup proof left for diagnosis: the proof's organizations, the
// fixture Kustomization with its Role and RoleBinding, and the operator's
// setting. It says so in its output. It touches only what carries the
// proof's ownership label or the fixture made, and refuses, by name, an
// object of those names without it.
func (p *organizationsProof) removeCleanupLeftovers() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*organizationCleanupWait)
	defer cancel()
	found, err := cleanupLeftoversFound(ctx, p.names[:])
	if err != nil || !found {
		return err
	}
	step("Removing what an earlier failed run of the cleanup proof left: its organizations, the Kustomization %s/%s and the operator's setting", platformNamespace, organizationCleanupFixture)
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return err
	}
	var organizations []*unstructured.Unstructured
	for _, name := range p.names {
		organization, err := getObject(ctx, organizationGVR, "", name)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if organization.GetLabels()[organizationsTestLabel] != organizationsTestMark {
			return fmt.Errorf("refusing to remove the leftover organization %s without the proof ownership label %s=%s", name, organizationsTestLabel, organizationsTestMark)
		}
		organizations = append(organizations, organization)
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	fixture, err := k.resource(kustomizationGVR, platformNamespace).Get(ctx, organizationCleanupFixture, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		// Without its Kustomization the operator keeps an Orphaned condition
		// as it was: its own setting back first lets it drop the condition.
		if restored, err := restoreOperatorSetting(ctx); err != nil {
			return err
		} else if restored {
			note("the operator's Deployment has its own setting again")
		}
	case err != nil:
		return err
	case fixture.GetLabels()[organizationsTestLabel] != organizationsTestMark:
		return fmt.Errorf("refusing to use the Kustomization %s/%s without the proof ownership label %s=%s", platformNamespace, organizationCleanupFixture, organizationsTestLabel, organizationsTestMark)
	default:
		// An empty inventory: every organization the fixture applied is
		// missing from git, and goes the path of a merged delete pull request.
		if err := writeOrganizationInventory(ctx); err != nil {
			return err
		}
	}
	resource, err := p.adminOrganizations()
	if err != nil {
		return err
	}
	for _, organization := range organizations {
		if err := eraseLeftoverOrganization(ctx, resource, organization.GetName()); err != nil {
			return err
		}
		note("removed the leftover organization %s", organization.GetName())
	}
	if restored, err := restoreOperatorSetting(ctx); err != nil {
		return err
	} else if restored {
		note("the operator's Deployment has its own setting again")
	}
	if err := removeInventoryFixture(ctx); err != nil {
		return err
	}
	note("removed the Kustomization %s/%s and its Role and RoleBinding", platformNamespace, organizationCleanupFixture)
	return nil
}

// eraseLeftoverOrganization takes the leftover organization name through
// nextLeftoverAction until its delete is admitted, then waits until it and
// its namespace are gone.
func eraseLeftoverOrganization(ctx context.Context, resource dynamic.ResourceInterface, name string) error {
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return err
	}
	var last organizationState
	var ready *unstructured.Unstructured
	err = wait.PollUntilContextTimeout(ctx, pollInterval, organizationReadyTimeout, true, func(ctx context.Context) (bool, error) {
		organization, err := getObject(ctx, organizationGVR, "", name)
		if err != nil {
			return false, err
		}
		state, err := organizationStateFrom(organization)
		if err != nil {
			return false, err
		}
		last = state
		annotated := organization.GetAnnotations()[organizationDeletePullRequestAnnotation] != ""
		switch nextLeftoverAction(state, annotated) {
		case leftoverMarkDeletePullRequest:
			note("organization %s is Orphaned=True (%s): setting %s", name, orphanedMissingInGit, organizationDeletePullRequestAnnotation)
			return false, patchDeletePullRequest(ctx, resource, name)
		case leftoverDelete:
			ready = organization
			return true, nil
		}
		return false, nil
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return errors.Join(err, fmt.Errorf("leftover organization %s not ready for its delete: %+v", name, last.conditions))
	}
	return eraseOrganization(ctx, resource, ready, last.namespace)
}
