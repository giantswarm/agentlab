package lab

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSettledOrganizationInventory(t *testing.T) {
	fixture := &unstructured.Unstructured{}
	fixture.SetGeneration(7)
	organization := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": organizationAPIVersion, beekeeperKeyKind: organizationKind,
		crMetadata: map[string]any{"namespace": organizationsRootNamespace, nameKey: organizationsTestA},
	}}
	for _, present := range []bool{true, false} {
		t.Run(map[bool]string{true: "recorded", false: "removed"}[present], func(t *testing.T) {
			var organizations []*unstructured.Unstructured
			if present {
				organizations = append(organizations, organization)
			}
			patch, err := settledOrganizationInventory(fixture, organizations...)
			require.NoError(t, err)
			var document struct {
				Status struct {
					ObservedGeneration int64 `json:"observedGeneration"`
					Inventory          struct {
						Entries []inventoryEntry `json:"entries"`
					} `json:"inventory"`
					Conditions []organizationCondition `json:"conditions"`
				} `json:"status"`
			}
			require.NoError(t, json.Unmarshal(patch, &document))
			require.Equal(t, int64(7), document.Status.ObservedGeneration)
			require.Len(t, document.Status.Conditions, 1)
			require.Equal(t, conditionReady, document.Status.Conditions[0].Type)
			require.Equal(t, conditionTrue, document.Status.Conditions[0].Status)
			require.Equal(t, int64(7), document.Status.Conditions[0].ObservedGeneration)
			if present {
				require.Equal(t, []inventoryEntry{{ID: organizationsRootNamespace + "_" + organizationsTestA + "_agent-platform.giantswarm.io_AgentPlatformOrganization", Version: organization.GroupVersionKind().Version}}, document.Status.Inventory.Entries)
			} else {
				require.Empty(t, document.Status.Inventory.Entries)
			}
		})
	}
}
