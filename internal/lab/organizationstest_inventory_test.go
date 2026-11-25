package lab

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSettledOrganizationInventory(t *testing.T) {
	fixture := &unstructured.Unstructured{}
	fixture.SetGeneration(7)
	organization := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": organizationAPIVersion, beekeeperKeyKind: organizationKind,
		crMetadata: map[string]any{nameKey: organizationsTestA},
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
				require.Equal(t, []inventoryEntry{{ID: "_agentlab-a_security.giantswarm.io_Organization", Version: organization.GroupVersionKind().Version}}, document.Status.Inventory.Entries)
			} else {
				require.Empty(t, document.Status.Inventory.Entries)
			}
		})
	}
}

func TestOrphanedAs(t *testing.T) {
	state := func(status, reason string, observed int64) organizationState {
		return organizationState{generation: 3, conditions: []organizationCondition{{Type: orphanedCondition, Status: status, Reason: reason, ObservedGeneration: observed}}}
	}
	require.True(t, orphanedAs(state(conditionTrue, orphanedDeleteMerged, 3), true, orphanedDeleteMerged))
	require.False(t, orphanedAs(state(conditionTrue, orphanedMissingInGit, 3), true, orphanedDeleteMerged), "another reason")
	require.False(t, orphanedAs(state(conditionTrue, orphanedDeleteMerged, 2), true, orphanedDeleteMerged), "an earlier generation")
	require.False(t, orphanedAs(state("False", orphanedInInventory, 3), true, orphanedInInventory), "another status")
	require.False(t, orphanedAs(organizationState{generation: 3}, false, orphanedInInventory), "no condition")
}

func TestNextLeftoverAction(t *testing.T) {
	orphaned := func(status, reason string, observed int64) organizationState {
		return organizationState{generation: 2, conditions: []organizationCondition{{Type: orphanedCondition, Status: status, Reason: reason, ObservedGeneration: observed}}}
	}
	for _, test := range []struct {
		name      string
		state     organizationState
		annotated bool
		want      leftoverAction
	}{
		{name: "not judged applied from git", state: organizationState{generation: 2}, want: leftoverDelete},
		{name: "its deletion runs", state: organizationState{generation: 2, deleting: true, conditions: orphaned(conditionTrue, orphanedMissingInGit, 2).conditions}, want: leftoverDelete},
		{name: "still in the inventory", state: orphaned("False", orphanedInInventory, 2), want: leftoverWait},
		{name: "a stale condition", state: orphaned(conditionTrue, orphanedMissingInGit, 1), want: leftoverWait},
		{name: "missing from git", state: orphaned(conditionTrue, orphanedMissingInGit, 2), want: leftoverMarkDeletePullRequest},
		{name: "annotated, not yet answered", state: orphaned(conditionTrue, orphanedMissingInGit, 2), annotated: true, want: leftoverWait},
		{name: "delete merged", state: orphaned(conditionTrue, orphanedDeleteMerged, 2), annotated: true, want: leftoverDelete},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, nextLeftoverAction(test.state, test.annotated))
		})
	}
}

func TestWithoutCleanupSetting(t *testing.T) {
	own := []string{"--leader-elect", "--installation-kustomization=flux-giantswarm/installation"}
	kept, changed := withoutCleanupSetting(append(slices.Clone(own), cleanupOperatorArgument()), map[string]string{organizationsTestLabel: "2026-11-25T00:00:00Z"})
	require.True(t, changed)
	require.Equal(t, own, kept)
	kept, changed = withoutCleanupSetting(own, map[string]string{"unrelated": "x"})
	require.False(t, changed)
	require.Equal(t, own, kept)
	_, changed = withoutCleanupSetting(own, map[string]string{organizationsTestLabel: "2026-11-25T00:00:00Z"})
	require.True(t, changed, "the proof's restart annotation alone")
}
