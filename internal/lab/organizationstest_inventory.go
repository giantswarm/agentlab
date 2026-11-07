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
	argument := "--installation-kustomization=" + platformNamespace + "/" + organizationErasureFixture
	arguments := deployment.Spec.Template.Spec.Containers[0].Args
	for _, existing := range arguments {
		if strings.HasPrefix(existing, "--installation-kustomization=") && existing != argument {
			return nil, fmt.Errorf("operator already uses another installation Kustomization")
		}
	}
	crd, err := getObject(ctx, gvrCRDs, "", kustomizationCRDName)
	ownAPI := apierrors.IsNotFound(err) || (err == nil && crd.GetLabels()[organizationsTestLabel] == organizationsTestMark)
	if err != nil && !ownAPI {
		return nil, err
	}
	if ownAPI {
		apiManifest := strings.Replace(string(kustomizationCRD()), "spec:", fmt.Sprintf("    %s: %s\nspec:", organizationsTestLabel, organizationsTestMark), 1)
		if _, err := applyManifests(ctx, []byte(apiManifest)); err != nil {
			return nil, err
		}
	}
	if err := organizationInventoryAccess(ctx); err != nil {
		return nil, err
	}
	resource := k.resource(kustomizationGVR, platformNamespace)
	fixture, err := resource.Get(ctx, organizationErasureFixture, metav1.GetOptions{})
	if err == nil && fixture.GetLabels()[organizationsTestLabel] != organizationsTestMark {
		return nil, fmt.Errorf("inventory fixture %s is not proof-owned", organizationErasureFixture)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	manifest := strings.ReplaceAll(string(gitopsFixtureKustomization()), gitopsFluxFixture, organizationErasureFixture)
	if _, err := applyManifests(ctx, []byte(manifest)); err != nil {
		return nil, err
	}
	labelPatch, _ := json.Marshal(map[string]any{crMetadata: map[string]any{organizationInventoryLabelsKey: map[string]string{organizationsTestLabel: organizationsTestMark}}})
	if _, err := resource.Patch(ctx, organizationErasureFixture, types.MergePatchType, labelPatch, metav1.PatchOptions{}); err != nil {
		return nil, err
	}
	if err := writeOrganizationInventory(ctx, organization); err != nil {
		return nil, err
	}
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return nil, err
	}
	provenance, _ := json.Marshal(map[string]any{crMetadata: map[string]any{organizationInventoryLabelsKey: map[string]string{"kustomize.toolkit.fluxcd.io/name": organizationErasureFixture, "kustomize.toolkit.fluxcd.io/namespace": platformNamespace}}})
	if _, err := k.resource(organizationGVR, organizationsRootNamespace).Patch(ctx, organization.GetName(), types.MergePatchType, provenance, metav1.PatchOptions{}); err != nil {
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
		{
			current, err := deployments.Get(cleanup, deployment.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			delete(current.Spec.Template.Annotations, organizationsTestLabel)
			current.Spec.Template.Spec.Containers[0].Args = slices.DeleteFunc(current.Spec.Template.Spec.Containers[0].Args, func(value string) bool { return value == argument })
			if _, err := deployments.Update(cleanup, current, metav1.UpdateOptions{}); err != nil {
				return err
			}
			if err := waitDeploymentRolledOut(cleanup, platformNamespace, deployment.Name, 3*time.Minute); err != nil {
				return err
			}
		}
		if err := deleteObject(cleanup, kustomizationGVR, platformNamespace, organizationErasureFixture, gitopsFixtureWait); err != nil {
			return err
		}
		if err := k.clientset.RbacV1().RoleBindings(platformNamespace).Delete(cleanup, organizationErasureFixture, metav1.DeleteOptions{}); err != nil {
			return err
		}
		if err := k.clientset.RbacV1().Roles(platformNamespace).Delete(cleanup, organizationErasureFixture, metav1.DeleteOptions{}); err != nil {
			return err
		}
		if ownAPI {
			remaining, err := listObjects(cleanup, kustomizationGVR, "", "")
			if err != nil {
				return err
			}
			if len(remaining) != 0 {
				return fmt.Errorf("retaining fixture Kustomization API: %d other Kustomizations exist", len(remaining))
			}
			return deleteObject(cleanup, gvrCRDs, "", kustomizationCRDName, gitopsFixtureWait)
		}
		return nil
	}, nil
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
	fixture, err := resource.Get(ctx, organizationErasureFixture, metav1.GetOptions{})
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

func waitOrganizationOrphaned(ctx context.Context, name string, expected bool) error {
	var last organizationState
	err := wait.PollUntilContextTimeout(ctx, pollInterval, organizationReadyTimeout, true, func(context.Context) (bool, error) {
		state, err := readOrganization(organizationsRootNamespace, name)
		if err != nil {
			return false, err
		}
		last = state
		condition, found := state.condition("Orphaned")
		return found && condition.ObservedGeneration >= state.generation && (condition.Status == conditionTrue) == expected, nil
	})
	if err != nil {
		return errors.Join(err, fmt.Errorf("organization %s has no current Orphaned=%t: %+v", name, expected, last.conditions))
	}
	return nil
}

func markProofOrganization(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return err
	}
	organization, err := getObject(ctx, organizationGVR, organizationsRootNamespace, name)
	if err != nil {
		return err
	}
	if organization.GetLabels()[managedByLabel] != managedByAgentlabValue {
		return fmt.Errorf("organization %s is not managed by agentlab", name)
	}
	patch, err := json.Marshal(map[string]any{crMetadata: map[string]any{organizationInventoryLabelsKey: map[string]string{organizationsTestLabel: organizationsTestMark}}})
	if err != nil {
		return err
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	_, err = k.resource(organizationGVR, organizationsRootNamespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func checkProofOrganization(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	organizationGVR, err := gvrFor(organizationResource)
	if err != nil {
		return err
	}
	organization, err := getObject(ctx, organizationGVR, organizationsRootNamespace, name)
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
	role, err := clients.clientset.RbacV1().Roles(platformNamespace).Get(ctx, organizationErasureFixture, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && role.Labels[organizationsTestLabel] != organizationsTestMark {
		return fmt.Errorf("inventory Role is not proof-owned")
	}
	binding, err := clients.clientset.RbacV1().RoleBindings(platformNamespace).Get(ctx, organizationErasureFixture, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && binding.Labels[organizationsTestLabel] != organizationsTestMark {
		return fmt.Errorf("inventory RoleBinding is not proof-owned")
	}
	metadata := metav1.ObjectMeta{Name: organizationErasureFixture, Namespace: platformNamespace, Labels: map[string]string{organizationsTestLabel: organizationsTestMark}}
	if _, err := applyTyped(ctx, &rbacv1.Role{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: organizationInventoryRoleKind}, ObjectMeta: metadata,
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{kustomizationGVR.Group}, Resources: []string{kustomizationGVR.Resource}, Verbs: []string{"get", "watch"}}}}); err != nil {
		return err
	}
	_, err = applyTyped(ctx, &rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: organizationInventoryBindingKind}, ObjectMeta: metadata,
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: organizationInventoryRoleKind, Name: organizationErasureFixture},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: platformNamespace, Name: "agent-platform-operator"}}})
	return err
}
