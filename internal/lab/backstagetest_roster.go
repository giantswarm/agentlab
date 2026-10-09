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
// the AgentTemplates and the RemoteMCPServers of the installation through
// Backstage's Kubernetes proxy with the person's own token
// (AgentsDataProvider, cluster-wide, discovery off), joined per row — the
// display name from the annotation, the readiness from the Agent's
// conditions by the portal's own derivation, the toolset off the carrier the
// template's gateway binding names, the owning release from the Flux
// provenance label. The apiserver decides who reads it: the lab's RBAC
// grants api.kagent.dev to platform-admins only (rbac.yaml.tmpl:
// cluster-admin; viewers hold the view ClusterRole and developers edit in
// `demo`, neither of which aggregates api.kagent.dev), so a non-admin's
// roster read is the apiserver's 403 and the portal shows the installation
// as unreadable — the lab's shape on the 0.10 line already, asserted as such.

// The readiness a row shows (kubernetes-react's deriveAgentReadiness), from
// the Agent's conditions.
const (
	rosterReady       = "ready"
	rosterNotReady    = "notReady"
	rosterNotAccepted = "notAccepted"
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

// listRoster reads the three resources the agents page lists through the
// Kubernetes proxy as the user and joins them into rows, one per Agent; the
// status is the apiserver's on the agents read (403 for a person without
// the read).
func listRoster(ps *portalSession) (int, []rosterRow, error) {
	status, raw, err := ps.kubeProxyGet("/apis/" + kagentAPIVersion + "/agents")
	if err != nil {
		return 0, nil, err
	}
	if status != http.StatusOK {
		return status, nil, nil
	}
	var agentList struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &agentList); err != nil {
		return status, nil, fmt.Errorf("the proxy's agents list is not the expected JSON: %w\n%.300s", err, raw)
	}
	templates, err := ps.kubeProxyList("agenttemplates")
	if err != nil {
		return status, nil, err
	}
	carriers, err := ps.kubeProxyList("remotemcpservers")
	if err != nil {
		return status, nil, err
	}
	rows := make([]rosterRow, 0, len(agentList.Items))
	for _, item := range agentList.Items {
		var a agentObject
		var meta struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(item, &a); err != nil {
			return status, nil, fmt.Errorf("an agents item is not an Agent: %w\n%.300s", err, item)
		}
		_ = json.Unmarshal(item, &meta)
		rows = append(rows, rosterRowOf(&a, meta.Metadata.Namespace, templates, carriers))
	}
	return http.StatusOK, rows, nil
}

// kubeProxyList reads one kagent resource of every namespace through the
// proxy; a status other than 200 is an error, since the agents read passed.
func (ps *portalSession) kubeProxyList(resource string) ([]unstructured.Unstructured, error) {
	status, raw, err := ps.kubeProxyGet("/apis/" + kagentAPIVersion + "/" + resource)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("the proxy lists agents but answers %d for %s: %.300s", status, resource, raw)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("the proxy's %s list is not the expected JSON: %w\n%.300s", resource, err, raw)
	}
	items := make([]unstructured.Unstructured, 0, len(list.Items))
	for _, item := range list.Items {
		items = append(items, unstructured.Unstructured{Object: item})
	}
	return items, nil
}

// rosterRowOf joins one Agent (of the namespace) with the template it
// references and the carrier that template's gateway binding names.
func rosterRowOf(a *agentObject, namespace string, templates, carriers []unstructured.Unstructured) rosterRow {
	row := rosterRow{
		Name: a.Metadata.Name, Namespace: namespace, DisplayName: firstNonEmpty(a.Metadata.Annotations[displayNameAnnotation], a.Metadata.Name),
		Harness: a.harnessName(), OwningRelease: a.Metadata.Labels[fluxHelmReleaseNameLabel],
	}
	row.Readiness = rosterReadiness(a)
	template, _, _ := a.inlineTemplate()
	for i := range templates {
		if templates[i].GetName() != a.templateName() || templates[i].GetNamespace() != namespace {
			continue
		}
		template, _ = agentTemplateFrom(&templates[i])
	}
	if template == nil {
		return row
	}
	if row.DisplayName == a.Metadata.Name {
		row.DisplayName = firstNonEmpty(template.Metadata.Annotations[displayNameAnnotation], a.Metadata.Name)
	}
	if bound := template.mcpServer(); bound != "" {
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
// deriveAgentReadiness) from the Agent's conditions: ready on Ready=True;
// notAccepted on Accepted=False, ResolvedRefs=False or Compatible=False;
// notReady while accepted and not ready; pending otherwise.
func rosterReadiness(a *agentObject) string {
	s := &a.Status
	if status, _ := s.condition(conditionReady); status == conditionTrue {
		return rosterReady
	}
	for _, condType := range []string{conditionAccepted, conditionResolvedRefs, conditionCompatible} {
		if status, _ := s.condition(condType); status == condFalseStatus {
			return rosterNotAccepted
		}
	}
	if accepted, _ := s.condition(conditionAccepted); accepted == conditionTrue {
		return rosterNotReady
	}
	return rosterPending
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
			return nil, fmt.Errorf("%s reads the roster (%d rows) although the lab's RBAC grants %s to %s only — if the lab's grants changed, update this proof and docs/backstage.md", ps.user.Email, len(rows), kagentAPIGroup, platformAdminsGroup)
		case !admin && status != http.StatusForbidden:
			return nil, fmt.Errorf("%s reads the roster as %d, wanted the apiserver's 403 (the lab's RBAC)", ps.user.Email, status)
		case !admin:
			note("403: the apiserver refuses %s (groups %v) the agenttemplates read; the portal shows installation %s as unreadable for this person", ps.user.Email, ps.user.Groups, platformRelease)
			verdicts = append(verdicts, fmt.Sprintf("PASS: %s's roster read is the apiserver's 403 (the lab grants %s to %s only; the portal shows the installation as unreadable)", ps.user.Email, kagentAPIGroup, platformAdminsGroup))
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
		verdicts = append(verdicts, fmt.Sprintf("PASS: %s's roster (agents + agenttemplates + remotemcpservers through %s) shows %s %s on Harness %s with toolset %v, owned by HelmRelease %s", ps.user.Email, portalKubeProxyAPI, spec.Name, row.Readiness, row.Harness, row.Toolset, row.OwningRelease))
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
