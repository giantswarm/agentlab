package lab

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestAgentHelmReleaseManagers: the field managers of the agent's HelmRelease
// are read off metadata.managedFields — who wrote it; a missing HelmRelease
// is the apiserver's NotFound.
func TestAgentHelmReleaseManagers(t *testing.T) {
	hr := customObject(fluxHelmReleaseGVK, kagentNamespace, agentsTestAgent, nil)
	_ = unstructured.SetNestedSlice(hr.Object, []any{
		map[string]any{managerField: agentManagerMCPServer, operationField: "Apply"},
		map[string]any{managerField: "helm-controller", operationField: "Update", "subresource": fieldStatus},
	}, "metadata", "managedFields")
	newFakeLab(t, hr)
	managers, err := agentHelmReleaseManagers(agentsTestAgent)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(managers, []string{agentManagerMCPServer, "helm-controller"}) {
		t.Errorf("managers = %v", managers)
	}
	if _, err := agentHelmReleaseManagers("absent"); err == nil {
		t.Error("a missing HelmRelease must fail the read")
	}
	if !agentHelmReleaseExists(agentsTestAgent) || agentHelmReleaseExists("absent") {
		t.Error("agentHelmReleaseExists disagrees with the store")
	}
}
