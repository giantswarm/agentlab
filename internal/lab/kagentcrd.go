package lab

import (
	"fmt"
	"strings"
)

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
	const crd = "agents.kagent.dev"
	present, err := outputQuiet("kubectl", "get", "crd", crd, "-o",
		`jsonpath={.spec.versions[?(@.name=="v1alpha2")].schema.openAPIV3Schema.properties.spec.properties.iconUrl}`)
	if err != nil {
		return fmt.Errorf("reading the %s CRD: %w", crd, err)
	}
	if strings.TrimSpace(present) != "" {
		note("Agent CRD already declares spec.iconUrl; the patch retired (HACKS.md U11)")
		return nil
	}
	names, err := outputQuiet("kubectl", "get", "crd", crd, "-o",
		`jsonpath={range .spec.versions[*]}{.name}{"\n"}{end}`)
	if err != nil {
		return fmt.Errorf("listing the %s CRD versions: %w", crd, err)
	}
	idx := -1
	for i, name := range strings.Split(strings.TrimSpace(names), "\n") {
		if name == "v1alpha2" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("the %s CRD serves no v1alpha2 (versions: %q)", crd, strings.TrimSpace(names))
	}
	patch := fmt.Sprintf(`[{"op":"add","path":"/spec/versions/%d/schema/openAPIV3Schema/properties/spec/properties/iconUrl",`+
		`"value":{"description":"IconURL is a URL to an icon representing the agent. It is surfaced on the agent's A2A AgentCard.",`+
		`"format":"uri","type":"string"}}]`, idx)
	if err := runQuiet("kubectl", "patch", "crd", crd, "--type=json", "-p", patch); err != nil {
		return fmt.Errorf("patching %s with spec.iconUrl: %w", crd, err)
	}
	note("Agent CRD patched with spec.iconUrl (until giantswarm/kagent#55)")
	return nil
}
