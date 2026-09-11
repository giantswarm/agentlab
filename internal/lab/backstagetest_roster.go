package lab

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The agents list (the roster) as the Dev Portal reads it: the AgentTemplates
// and the RemoteMCPServers of the installation through Backstage's Kubernetes
// proxy with the person's own token (AgentsDataProvider: useResources(Agent)
// and useResources(RemoteMCPServer), cluster-wide, discovery off), joined per
// row — the display name from the annotation, the readiness from
// status.harnesses[] by the portal's own derivation, the toolset off the
// carrier the template's gateway binding names, the owning release from the
// Flux provenance label. The apiserver decides who reads it: the lab's RBAC
// grants kagent.dev to platform-admins only (rbac.yaml.tmpl: cluster-admin;
// viewers hold the view ClusterRole and developers edit in `demo`, neither
// of which aggregates kagent.dev), so a non-admin's roster read is the
// apiserver's 403 and the portal shows the installation as unreadable — the
// lab's shape on the 0.10 line already, asserted as such.

// The readiness a row shows (kubernetes-react's deriveAgentReadiness): the
// deciding Harness is the platform one named by the admission label.
const (
	rosterReady       = "ready"
	rosterNotReady    = "notReady"
	rosterNotAccepted = "notAccepted"
	rosterNotAdmitted = "notAdmitted"
	rosterPending     = "pending"
)

// platformAdminsGroup is the lab group whose ClusterRoleBinding
// (cluster-admin) reads kagent.dev — the one group the roster renders for.
const platformAdminsGroup = "platform-admins"

// rosterRow is one agent of the list as the proof derives it from the same
// two reads the portal makes.
type rosterRow struct {
	Name, Namespace, DisplayName string
	Readiness, Harness           string
	// Toolset is the carrier's header split, Declared whether the template
	// binds a carrier that carries one (`No tools` for a chat-only agent,
	// `Full gateway access` for a carrier without the header).
	Toolset  []string
	Declared bool
	// OwningRelease is the HelmRelease the template was rendered by.
	OwningRelease string
}

// listRoster reads the two resources the agents page lists through the
// Kubernetes proxy as the user and joins them into rows; the status is the
// apiserver's on the templates read (403 for a person without the read).
func listRoster(ps *portalSession) (int, []rosterRow, error) {
	status, raw, err := ps.kubeProxyGet("/apis/" + agentTemplateAPIVersion + "/agenttemplates")
	if err != nil {
		return 0, nil, err
	}
	if status != http.StatusOK {
		return status, nil, nil
	}
	var templates struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &templates); err != nil {
		return status, nil, fmt.Errorf("the proxy's agenttemplates list is not the expected JSON: %w\n%.300s", err, raw)
	}
	status, raw, err = ps.kubeProxyGet("/apis/" + agentTemplateAPIVersion + "/remotemcpservers")
	if err != nil {
		return 0, nil, err
	}
	if status != http.StatusOK {
		return status, nil, fmt.Errorf("the proxy lists agenttemplates but answers %d for remotemcpservers: %.300s", status, raw)
	}
	var carrierList struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &carrierList); err != nil {
		return status, nil, fmt.Errorf("the proxy's remotemcpservers list is not the expected JSON: %w\n%.300s", err, raw)
	}
	carriers := make([]unstructured.Unstructured, 0, len(carrierList.Items))
	for _, item := range carrierList.Items {
		carriers = append(carriers, unstructured.Unstructured{Object: item})
	}
	rows := make([]rosterRow, 0, len(templates.Items))
	for _, item := range templates.Items {
		var t agentTemplate
		var meta struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(item, &t); err != nil {
			return status, nil, fmt.Errorf("an agenttemplates item is not an AgentTemplate: %w\n%.300s", err, item)
		}
		_ = json.Unmarshal(item, &meta)
		rows = append(rows, rosterRowOf(&t, meta.Metadata.Namespace, carriers))
	}
	return http.StatusOK, rows, nil
}

// rosterRowOf joins one template (of the namespace) with the carrier its
// gateway binding names.
func rosterRowOf(t *agentTemplate, namespace string, carriers []unstructured.Unstructured) rosterRow {
	row := rosterRow{
		Name: t.Metadata.Name, Namespace: namespace, DisplayName: firstNonEmpty(t.Metadata.Annotations[displayNameAnnotation], t.Metadata.Name),
		Harness: t.Metadata.Labels[harnessLabel], OwningRelease: t.Metadata.Labels[fluxHelmReleaseNameLabel],
	}
	row.Readiness = rosterReadiness(t)
	if bound := t.mcpServer(); bound != "" {
		for i := range carriers {
			c := &carriers[i]
			if c.GetName() != bound || c.GetNamespace() != namespace {
				continue
			}
			if header, found, _ := remoteMCPServerHeader(c, toolsetHeader); found {
				row.Toolset, row.Declared = splitToolset(header), true
			}
		}
	}
	return row
}

// rosterReadiness is the portal's derivation (kubernetes-react Agent.ts,
// deriveAgentReadiness) from the deciding Harness — the platform one named
// by the admission label when it reports, else the readiest of the others:
// ready on Ready=True; notAccepted on Accepted=False or Compatible=False;
// notReady while accepted and not ready; notAdmitted when harnesses[] is
// empty with observedGeneration caught up; pending otherwise.
func rosterReadiness(t *agentTemplate) string {
	if len(t.Status.Harnesses) == 0 {
		if t.Status.ObservedGeneration > 0 && t.Status.ObservedGeneration >= t.Metadata.Generation {
			return rosterNotAdmitted
		}
		return rosterPending
	}
	deciding := t.harness(t.Metadata.Labels[harnessLabel])
	if deciding == nil {
		verdicts := make([]string, 0, len(t.Status.Harnesses))
		for i := range t.Status.Harnesses {
			verdicts = append(verdicts, harnessVerdict(&t.Status.Harnesses[i]))
		}
		slices.SortFunc(verdicts, func(a, b string) int { return readinessRank(a) - readinessRank(b) })
		return verdicts[0]
	}
	return harnessVerdict(deciding)
}

func harnessVerdict(h *harnessStatus) string {
	if status, _ := h.condition(conditionReady); status == conditionTrue {
		return rosterReady
	}
	if accepted, _ := h.condition(conditionAccepted); accepted == condFalseStatus {
		return rosterNotAccepted
	}
	if compatible, _ := h.condition(conditionCompatible); compatible == condFalseStatus {
		return rosterNotAccepted
	}
	if accepted, _ := h.condition(conditionAccepted); accepted == conditionTrue {
		return rosterNotReady
	}
	return rosterPending
}

// readinessRank orders verdicts readiest first.
func readinessRank(v string) int {
	return slices.Index([]string{rosterReady, rosterNotReady, rosterPending, rosterNotAccepted, rosterNotAdmitted}, v)
}

// splitToolset splits the header back into selectors.
func splitToolset(header string) []string {
	var out []string
	for _, part := range strings.Split(header, ",") {
		if sel := strings.TrimSpace(part); sel != "" {
			out = append(out, sel)
		}
	}
	return out
}

// proveRoster reads the agents list as every user: a platform-admin sees
// the agent with its readiness, toolset and owning release; a developer or
// viewer is refused by the apiserver (the lab's RBAC), which the portal shows
// as the installation being unreadable — asserted exactly, so a change of
// the lab's grants is noticed here.
func proveRoster(sessions []*portalSession, spec agentSpec) ([]string, error) {
	var verdicts []string
	for _, ps := range sessions {
		step("The agents list through %s as %s", portalKubeProxyAPI, ps.user.Email)
		status, rows, err := listRoster(ps)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ps.user.Email, err)
		}
		admin := ps.user.HasGroup(platformAdminsGroup)
		switch {
		case admin && status != http.StatusOK:
			return nil, fmt.Errorf("%s (%s) reads the roster as %d, wanted 200", ps.user.Email, platformAdminsGroup, status)
		case !admin && status == http.StatusOK:
			return nil, fmt.Errorf("%s reads the roster (%d rows) although the lab's RBAC grants kagent.dev to %s only — if the lab's grants changed, update this proof and docs/backstage.md", ps.user.Email, len(rows), platformAdminsGroup)
		case !admin && status != http.StatusForbidden:
			return nil, fmt.Errorf("%s reads the roster as %d, wanted the apiserver's 403 (the lab's RBAC)", ps.user.Email, status)
		case !admin:
			note("403: the apiserver refuses %s (groups %v) the agenttemplates read; the portal shows installation %s as unreadable for this person", ps.user.Email, ps.user.Groups, platformRelease)
			verdicts = append(verdicts, fmt.Sprintf("PASS: %s's roster read is the apiserver's 403 (the lab grants kagent.dev to %s only; the portal shows the installation as unreadable)", ps.user.Email, platformAdminsGroup))
			continue
		}
		idx := slices.IndexFunc(rows, func(r rosterRow) bool { return r.Name == spec.Name && r.Namespace == kagentNamespace })
		if idx < 0 {
			return nil, fmt.Errorf("the roster for %s lists %d agents but not %s/%s", ps.user.Email, len(rows), kagentNamespace, spec.Name)
		}
		row := rows[idx]
		if row.Readiness != rosterReady || row.Harness != spec.harnessValue() {
			return nil, fmt.Errorf("the roster shows %s as %s on Harness %q, wanted %s on %s", spec.Name, row.Readiness, row.Harness, rosterReady, spec.harnessValue())
		}
		if !row.Declared || !slices.Equal(row.Toolset, spec.Toolset) {
			return nil, fmt.Errorf("the roster shows %s's toolset as %v (declared %v), wanted %v off carrier %s", spec.Name, row.Toolset, row.Declared, spec.Toolset, spec.Name)
		}
		if row.OwningRelease != spec.Name || row.DisplayName != spec.DisplayName {
			return nil, fmt.Errorf("the roster shows %s as %q owned by HelmRelease %q, wanted %q owned by %s", spec.Name, row.DisplayName, row.OwningRelease, spec.DisplayName, spec.Name)
		}
		ready := 0
		for _, r := range rows {
			if r.Readiness == rosterReady {
				ready++
			}
		}
		note("%d agents (%d ready): %s is %q, %s on Harness %s, toolset %v, rendered by HelmRelease %s", len(rows), ready, spec.Name, row.DisplayName, row.Readiness, row.Harness, row.Toolset, row.OwningRelease)
		verdicts = append(verdicts, fmt.Sprintf("PASS: %s's roster (agenttemplates + remotemcpservers through %s) shows %s %s on Harness %s with toolset %v, owned by HelmRelease %s", ps.user.Email, portalKubeProxyAPI, spec.Name, row.Readiness, row.Harness, row.Toolset, row.OwningRelease))
	}
	return verdicts, nil
}

// adminSession is the first session of a platform-admin, nil when none is
// among them; the create path needs one.
func adminSession(sessions []*portalSession) *portalSession {
	for _, ps := range sessions {
		if ps.user.HasGroup(platformAdminsGroup) {
			return ps
		}
	}
	return nil
}

// sessionInGroup is the first session of a user in the group, nil when none.
func sessionInGroup(sessions []*portalSession, group string) *portalSession {
	for _, ps := range sessions {
		if ps.user.HasGroup(group) {
			return ps
		}
	}
	return nil
}

// otherSessions are the sessions that are not the given one.
func otherSessions(sessions []*portalSession, than *portalSession) []*portalSession {
	var out []*portalSession
	for _, ps := range sessions {
		if ps != than {
			out = append(out, ps)
		}
	}
	return out
}

// viewerGroup is the lab group whose users hold the view ClusterRole — the
// refused writer of the proofs.
const viewerGroup = "viewers"
