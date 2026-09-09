package lab

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// agentCRD names kagent's Agent CustomResourceDefinition.
const agentCRD = "agents.kagent.dev"

// agentCRDVersion is the served version whose schema the create flow's
// objects are validated against.
const agentCRDVersion = "v1alpha2"

// patchAgentCRDIconURL works around HACKS.md U11: the kagent package pins the
// upstream 0.9.x CRDs, whose v1alpha2 Agent spec predates the A2A-card
// metadata fields. The Backstage create flow composes agent.iconUrl whenever
// the installation has a baseDomain (this lab always sets one), the agent
// chart renders it into Agent.spec.iconUrl, and server-side apply then
// rejects the whole HelmRelease with ".spec.iconUrl: field not declared in
// schema" — the portal's agents list stays empty with no visible error, since
// the scaffolder task only kube-applies the HelmRelease and reports success.
//
// Until giantswarm/kagent#55 ships CRDs that carry the field, add upstream
// main's definition (optional string; the 0.9.x controller stores and ignores
// it) to the installed CRD. Idempotent: a no-op once the field is present, so
// a kagent bump that carries it retires this silently.
func patchAgentCRDIconURL() error {
	ctx := context.Background()
	crd, err := getObject(ctx, gvrCRDs, "", agentCRD)
	if err != nil {
		return fmt.Errorf("reading the %s CRD: %w", agentCRD, err)
	}
	idx, present, names := agentCRDIconURL(crd)
	if present {
		note("Agent CRD already declares spec.iconUrl; the patch retired (HACKS.md U11)")
		return nil
	}
	if idx < 0 {
		return fmt.Errorf("the %s CRD serves no %s (versions: %q)", agentCRD, agentCRDVersion, strings.Join(names, "\n"))
	}
	patch := fmt.Sprintf(`[{"op":"add","path":"/spec/versions/%d/schema/openAPIV3Schema/properties/spec/properties/iconUrl",`+
		`"value":{"description":"IconURL is a URL to an icon representing the agent. It is surfaced on the agent's A2A AgentCard.",`+
		`"format":"uri","type":"string"}}]`, idx)
	if err := patchObject(ctx, gvrCRDs, "", agentCRD, types.JSONPatchType, []byte(patch)); err != nil {
		return fmt.Errorf("patching %s with spec.iconUrl: %w", agentCRD, err)
	}
	note("Agent CRD patched with spec.iconUrl (until giantswarm/kagent#55)")
	return nil
}

// agentCRDIconURL reads the Agent CRD's spec.versions: the index of the
// version the create flow targets (-1 when it is not served), whether that
// version's schema already declares spec.iconUrl, and the version names as
// listed (for the error that names them).
func agentCRDIconURL(crd *unstructured.Unstructured) (idx int, present bool, names []string) {
	idx = -1
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for i, v := range versions {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(m, "name")
		names = append(names, name)
		if name != agentCRDVersion {
			continue
		}
		idx = i
		_, present, _ = unstructured.NestedFieldNoCopy(m, "schema", "openAPIV3Schema", "properties", "spec", "properties", "iconUrl")
	}
	return idx, present, names
}
