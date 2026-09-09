package lab

import (
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

// The sign-in half of the toolset proof, and the portal half.

// proveToolsetPortalV1 is the portal half: the Tools step's backend calls
// (presets, live resolution, unmatched selectors) and the composer's apply
// path (the same scaffolder template the wizard's Deploy drives, with the
// manifest the composer emits, the user's own OIDC token as the secret) —
// asserted on what lands: the HelmRelease value and the Agent's header.
func proveToolsetPortalV1(cfg *config.Config, user *config.User) ([]string, error) {
	var verdicts []string
	ps, err := backstageLogin(cfg, user)
	if err != nil {
		return nil, err
	}
	note("portal image %s", deploymentImage(platformNamespace, componentBackstage))

	step("The Tools step's backend calls: presets, live resolution, unmatched selectors (/api/muster/tools/filter)")
	presets, err := portalFilterTools(ps, nil, true)
	if err != nil {
		return nil, err
	}
	presetNames := make([]string, 0, len(presets.Presets))
	for _, p := range presets.Presets {
		presetNames = append(presetNames, p.Name)
	}
	for _, want := range []string{presetReadOnlyName, presetNoneName, presetFullName} {
		if !slices.Contains(presetNames, want) {
			return nil, fmt.Errorf("/tools/filter?include_presets=true lacks the built-in preset %s: %v", want, presetNames)
		}
	}
	note("presets offered: %s", strings.Join(presetNames, ", "))
	ro, err := portalFilterTools(ps, []string{presetReadOnly}, false)
	if err != nil {
		return nil, err
	}
	if len(ro.Tools) == 0 {
		return nil, fmt.Errorf("/tools/filter?toolset=%s resolved to nothing", presetReadOnly)
	}
	for _, t := range ro.Tools {
		if !t.Annotations.readOnly() {
			return nil, fmt.Errorf("/tools/filter?toolset=%s includes %s without readOnlyHint", presetReadOnly, t.Name)
		}
	}
	unmatched, err := portalFilterTools(ps, []string{"server:agentlab-no-such-server"}, false)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(unmatched.ToolsetUnmatched, "server:agentlab-no-such-server") || len(unmatched.Tools) != 0 {
		return nil, fmt.Errorf("/tools/filter?toolset=server:agentlab-no-such-server: tools=%d unmatched=%v", len(unmatched.Tools), unmatched.ToolsetUnmatched)
	}
	if _, err := portalFilterTools(ps, []string{presetNoSuch}, false); err == nil || !strings.Contains(err.Error(), "unknown preset") {
		return nil, fmt.Errorf("/tools/filter with an unknown preset should relay muster's error, got %v", err)
	}
	note("%s -> %d read-only tools; an unknown server -> toolset_unmatched; an unknown preset -> muster's error relayed", presetReadOnly, len(ro.Tools))
	verdicts = append(verdicts, fmt.Sprintf("PASS: the Tools step's backend (/api/muster/tools/filter) offers the presets [%s], resolves %s live (%d read-only tools) and reports unmatched selectors and unknown presets as muster does", strings.Join(presetNames, ", "), presetReadOnly, len(ro.Tools)))

	step("The composer's apply path: template:default/agent-deployment with the composed manifest (toolset %s) as %s", presetReadOnly, user.Email)
	manifest := composeAgentManifestV1(toolsetsAgentPortal, "default-model-config", []string{presetReadOnly})
	taskID, err := scaffold(ps, manifest, toolsetsAgentPortal)
	if err != nil {
		return nil, err
	}
	note("scaffolder task %s completed (kube:apply as %s)", taskID, user.Email)
	toolset, set, err := helmReleaseToolset(toolsetsAgentPortal)
	if err != nil {
		return nil, err
	}
	if !set || !slices.Equal(toolset, []string{presetReadOnly}) {
		return nil, fmt.Errorf("HelmRelease %s applied by the portal path carries values.toolset=%v (set %v), wanted [%s]", toolsetsAgentPortal, toolset, set, presetReadOnly)
	}
	cr, err := waitAgentCR(toolsetsAgentPortal)
	if err != nil {
		return nil, err
	}
	header, err := toolsetHeaderOfCR(cr)
	if err != nil {
		return nil, fmt.Errorf("agent %s: %w", toolsetsAgentPortal, err)
	}
	if header != presetReadOnly {
		return nil, fmt.Errorf("agent %s carries %s=%q, wanted %s", toolsetsAgentPortal, toolsetHeader, header, presetReadOnly)
	}
	note("HelmRelease values.toolset %v; Agent spec.declarative.tools[0].headersFrom %s=%s", toolset, toolsetHeader, header)
	verdicts = append(verdicts, fmt.Sprintf("PASS: the portal's apply path (scaffolder template agent-deployment, kube:apply with the user's token) lands the composer's toolset on the HelmRelease and the header on the Agent (%s)", toolsetsAgentPortal))
	return verdicts, nil
}

// composeAgentManifestV1 is the composer's combinedManifest for a new agent
// (plugins/agent-platform composeManifests.ts): the shared OCIRepository of
// the chart, then the HelmRelease with the values — `agent`, `modelConfig`
// and the top-level `toolset` exactly as the Tools step composed it — and
// `spec.serviceAccountName`, which the portal takes from the app-config key
// `agentPlatform.fluxServiceAccountName` the chart renders (composeManifests.ts).
func composeAgentManifestV1(name, modelConfig string, toolset []string) string {
	quoted := make([]string, 0, len(toolset))
	for _, sel := range toolset {
		quoted = append(quoted, fmt.Sprintf("%q", sel))
	}
	return fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: agent
  namespace: %[1]s
spec:
  interval: 30m
  url: oci://gsoci.azurecr.io/charts/giantswarm/agent
  ref:
    semver: x.x.x
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: %[2]s
  namespace: %[1]s
spec:
  interval: 10m
  serviceAccountName: %[6]s
  chartRef:
    kind: OCIRepository
    name: agent
    namespace: %[1]s
  values:
    agent:
      name: %[2]s
      displayName: "agentlab toolset proof: portal"
      description: "Throwaway agent of agentlab toolsets-test, applied through the portal's deploy path; deleted by the same run."
      systemMessage: %[3]q
    modelConfig:
      name: %[4]s
    toolset: [%[5]s]
`, kagentNamespace, name, toolsetTestAgentSystemMsg, modelConfig, strings.Join(quoted, ", "), kagentFluxServiceAccount)
}
