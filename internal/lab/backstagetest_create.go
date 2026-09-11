package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The create half of the portal proof: the wizard's path on kagent API v2.
// The portal composes no manifest — skill discovery (the gs backend's
// GET /api/gs/agent-skills) resolves a repository's skills at its head
// commit, the review page is agent-manager's validate_agent dry run and
// Deploy is its create_agent, both called through the muster plugin's
// backend (POST /api/muster/call) with the person's own forwarded token, so
// agent-manager writes as the person. What lands is asserted against what the
// dry run rendered: the OCIRepository at the chart's 1.x range, the
// HelmRelease as the tenant ServiceAccount with the skills pinned to the
// commits discovery showed, its render on the platform Harness.

// The agent the proof creates through the portal, its avatar, and the
// namespace argument the portal always sends (the ModelConfig's).
const (
	backstageTestAgent       = "agentlab-backstage-test"
	backstageTestDisplayName = "agentlab backstage-test"
	backstageTestIconURL     = "https://avatars.127.0.0.1.nip.io/" + backstageTestAgent + ".svg"
	backstageTestToolset     = "preset:read-only"
	backstageTestDescription = "Throwaway agent of `agentlab backstage-test`, created through the Dev Portal's path (agent-manager over muster as the person); deleted by the same run."
	// backstageTestReadyTimeout bounds the golden boot: the agent carries a
	// git skill the Go ADK fetches before it serves readyz.
	backstageTestReadyTimeout = 5 * time.Minute
	// namespaceKey is the argument the portal sends with every agent-manager
	// call (the ModelConfig's namespace).
	namespaceKey = "namespace"
	// The dry run's mode and the agent-manager refusals the portal tells apart
	// (lib/agentManager.ts classifyAgentManagerError): `<code>: <message>`.
	validateModeCreate = "create"
	validateModeUpdate = "update"
	refusalConflict    = "conflict:"
	refusalForbidden   = "forbidden:"
)

// portalToolCall runs one agent-manager tool the way the portal does — the
// muster plugin's backend, POST /api/muster/call {name, arguments}, with the
// person's forwarded token — and decodes the tool's JSON payload into out.
// The backend (MusterMcpClient.callTool) has already looked through
// call_tool's `{isError, content}` envelope: a 200 carries the tool's payload
// itself, and a tool-level refusal is thrown, arriving as a non-200 whose
// Backstage error body carries the refusal as the tool worded it
// (`<code>: <message>`); that message is the error, so callers judge the code
// by its marker.
func portalToolCall(ps *portalSession, tool string, args map[string]any, out any) error {
	if args == nil {
		args = map[string]any{}
	}
	status, raw, err := ps.musterPost("/call"+installationQuery, map[string]any{nameKey: agentManagerToolPrefix + tool, argumentsKey: args})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("POST /api/muster/call %s%s answered %d: %s", agentManagerToolPrefix, tool, status, excerpt(strings.TrimSpace(backstageErrorMessage(raw)), 300))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s%s through the portal: payload is not the expected JSON: %w\n%.300s", agentManagerToolPrefix, tool, err, raw)
	}
	return nil
}

// discoveredSkill is one entry of the skill discovery route: the skill's
// frontmatter name, its directory in the repository, and the commit the
// listing was read at — what a selected skill becomes in the create request.
type discoveredSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	RepoURL     string `json:"repoUrl"`
	Path        string `json:"path"`
	Ref         string `json:"ref"`
	Commit      string `json:"commit"`
}

// skillDiscovery is the route's answer: the skills, the ref read and the
// commit it resolved to, and whether some may be missing.
type skillDiscovery struct {
	Skills    []discoveredSkill `json:"skills"`
	Ref       string            `json:"ref"`
	Commit    string            `json:"commit"`
	Truncated bool              `json:"truncated"`
}

// discoverSkills is GET /api/gs/agent-skills?repoUrl=… as the user: every
// skill of the repository at the head commit of its default branch. Every
// entry must carry that commit as a full id — the pin the portal writes.
func discoverSkills(ps *portalSession, repo string) (*skillDiscovery, error) {
	status, raw, err := ps.backstageGet(portalSkillsPath + "?repoUrl=" + url.QueryEscape(repo))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET %s?repoUrl=%s answered %d: %.300s", portalSkillsPath, repo, status, raw)
	}
	var d skillDiscovery
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("GET %s: not the expected JSON: %w\n%.300s", portalSkillsPath, err, raw)
	}
	if d.Truncated {
		return nil, fmt.Errorf("GET %s?repoUrl=%s is truncated (%d skills listed): the portal could not read every skill — its GitHub API budget (unauthenticated: 60 requests an hour per egress address) is the usual cause; run again once the window has reset", portalSkillsPath, repo, len(d.Skills))
	}
	if len(d.Skills) == 0 {
		return nil, fmt.Errorf("GET %s?repoUrl=%s lists no skills", portalSkillsPath, repo)
	}
	if !fullCommitID.MatchString(d.Commit) {
		return nil, fmt.Errorf("GET %s resolved %s to %q, not a full commit id", portalSkillsPath, repo, d.Commit)
	}
	for _, s := range d.Skills {
		if s.Commit != d.Commit {
			return nil, fmt.Errorf("skill %s (%s) carries commit %q, the listing was read at %s — every entry is pinned to the one commit", s.Name, s.Path, s.Commit, d.Commit)
		}
	}
	return &d, nil
}

// skillEntry is the chart 1.x skills[] entry the portal composes from a
// discovered skill (lib/agentSpec.ts skillEntryOf): the name, the directory,
// the repository at the discovered commit — never a branch.
func (s discoveredSkill) skillEntry() agentSkill {
	return agentSkill{Name: s.Name, Path: s.Path, Git: &gitSkill{URL: s.RepoURL, Commit: s.Commit}}
}

// agentManifests is what validate_agent and create_agent hand back: the two
// Flux objects as YAML and the composed chart values.
type agentManifests struct {
	OCIRepository string         `json:"ociRepository"`
	HelmRelease   string         `json:"helmRelease"`
	Values        map[string]any `json:"values"`
}

// validateReport is validate_agent's answer as the review page renders it.
type validateReport struct {
	Valid         bool           `json:"valid"`
	Mode          string         `json:"mode"`
	Errors        []string       `json:"errors"`
	SchemaVersion string         `json:"schemaVersion"`
	SchemaSource  string         `json:"schemaSource"`
	Manifests     agentManifests `json:"manifests"`
}

// portalAgentArgs is the agent-manager argument shape the portal sends: the
// create arguments plus the namespace (the ModelConfig's).
func portalAgentArgs(spec agentSpec) (map[string]any, error) {
	args, err := createAgentArgs(spec)
	if err != nil {
		return nil, err
	}
	args[namespaceKey] = kagentNamespace
	return args, nil
}

// portalValidateAgent is the review page: validate_agent with the spec the
// wizard composed, through the portal as the person.
func portalValidateAgent(ps *portalSession, spec agentSpec) (*validateReport, error) {
	args, err := portalAgentArgs(spec)
	if err != nil {
		return nil, err
	}
	var report validateReport
	if err := portalToolCall(ps, "validate_agent", args, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

// assertDryRun checks a create dry run against the spec and what get_info
// says the platform is: valid with no violations, mode create, the
// OCIRepository tracking the chart at 1.x, the values placing the agent on
// the platform Harness with no runtime, the toolset as composed, every git
// skill pinned to the commit discovery showed.
func assertDryRun(report *validateReport, spec agentSpec, info *agentManagerInfo) error {
	if !report.Valid || len(report.Errors) > 0 {
		return fmt.Errorf("validate_agent refuses the wizard's spec: valid=%v errors=%v", report.Valid, report.Errors)
	}
	if report.Mode != validateModeCreate {
		return fmt.Errorf("validate_agent answered mode %q, wanted %s", report.Mode, validateModeCreate)
	}
	return assertManifests(report.Manifests, spec, info)
}

// assertManifests is the shape both the dry run and the create hand back.
func assertManifests(m agentManifests, spec agentSpec, info *agentManagerInfo) error {
	var source struct {
		Spec struct {
			URL string `yaml:"url"`
			Ref struct {
				Semver string `yaml:"semver"`
			} `yaml:"ref"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(m.OCIRepository), &source); err != nil {
		return fmt.Errorf("manifests.ociRepository is not YAML: %w\n%.300s", err, m.OCIRepository)
	}
	if source.Spec.Ref.Semver != agentChartRange || source.Spec.URL != agentChartURL {
		return fmt.Errorf("manifests.ociRepository tracks %s at %q, wanted %s at %s (the wizard writes 1.x, never x.x.x)", source.Spec.URL, source.Spec.Ref.Semver, agentChartURL, agentChartRange)
	}
	var release struct {
		Spec struct {
			ServiceAccountName string `yaml:"serviceAccountName"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(m.HelmRelease), &release); err != nil {
		return fmt.Errorf("manifests.helmRelease is not YAML: %w\n%.300s", err, m.HelmRelease)
	}
	if want := firstNonEmpty(info.Flux.ServiceAccountName, kagentFluxServiceAccount); release.Spec.ServiceAccountName != want {
		return fmt.Errorf("manifests.helmRelease runs as serviceAccountName %q, wanted the tenant identity %s", release.Spec.ServiceAccountName, want)
	}
	agent, _, _ := unstructured.NestedMap(m.Values, "agent")
	if got, _ := agent["harness"].(string); got != info.Harness.Name {
		return fmt.Errorf("manifests.values.agent.harness is %q, wanted get_info's Harness %s", got, info.Harness.Name)
	}
	if runtime, found := agent["runtime"]; found {
		return fmt.Errorf("manifests.values.agent.runtime=%v is set: chart 1.x has no runtime (the platform Harness runs every agent)", runtime)
	}
	if got, _ := agent[nameKey].(string); got != spec.Name {
		return fmt.Errorf("manifests.values.agent.name is %q, wanted %s", got, spec.Name)
	}
	if got, _, _ := unstructured.NestedString(m.Values, modelConfigKey, nameKey); got != spec.ModelConfig {
		return fmt.Errorf("manifests.values.modelConfig.name is %q, wanted %s", got, spec.ModelConfig)
	}
	toolset, _, _ := unstructured.NestedStringSlice(m.Values, toolsetKey)
	if !slices.Equal(toolset, spec.Toolset) {
		return fmt.Errorf("manifests.values.toolset is %v, wanted %v", toolset, spec.Toolset)
	}
	pinned := skillCommits(m.Values)
	for _, s := range spec.Skills {
		if s.Git == nil {
			continue
		}
		if got := pinned[s.Name]; got != s.Git.Commit {
			return fmt.Errorf("manifests.values.skills pins %s at %q, wanted the discovered commit %s", s.Name, got, s.Git.Commit)
		}
	}
	return nil
}

// assertReleaseIsDryRun checks the HelmRelease that landed carries exactly the
// values the dry run rendered (the Deploy applies the reviewed manifests, no
// more), runs as the tenant ServiceAccount and renders from the namespace's
// OCIRepository of the chart at 1.x.
func assertReleaseIsDryRun(release *agentRelease, m agentManifests, info *agentManagerInfo) error {
	if !reflect.DeepEqual(jsonRoundTrip(release.Spec.Values), jsonRoundTrip(m.Values)) {
		return fmt.Errorf("HelmRelease %s carries values %v, the dry run rendered %v — Deploy applied something else than the review showed", release.Metadata.Name, release.Spec.Values, m.Values)
	}
	if want := firstNonEmpty(info.Flux.ServiceAccountName, kagentFluxServiceAccount); release.Spec.ServiceAccountName != want {
		return fmt.Errorf("HelmRelease %s runs as serviceAccountName %q, wanted %s", release.Metadata.Name, release.Spec.ServiceAccountName, want)
	}
	if release.Spec.ChartRef.Kind != kindOCIRepository || release.Spec.ChartRef.Name != agentChartOCIRepository {
		return fmt.Errorf("HelmRelease %s renders from chartRef %+v, wanted the OCIRepository %s of the namespace", release.Metadata.Name, release.Spec.ChartRef, agentChartOCIRepository)
	}
	chartURL, chartRange, _, err := agentChartSource()
	if err != nil {
		return fmt.Errorf("the namespace's OCIRepository %s: %w", agentChartOCIRepository, err)
	}
	if chartURL != agentChartURL || chartRange != agentChartRange {
		return fmt.Errorf("OCIRepository %s tracks %s at %q, wanted %s at %s", agentChartOCIRepository, chartURL, chartRange, agentChartURL, agentChartRange)
	}
	return nil
}

// jsonRoundTrip is v as JSON decodes it — one shape whatever Go types
// composed it (a HelmRelease read off the apiserver against a dry run's
// values), so the two compare.
func jsonRoundTrip(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return v
	}
	return out
}

// portalAgentStatus is get_agent_status through the portal, polled until the
// verdict is the wanted one (the detail page's 3 s poll), bounded.
func portalAgentStatus(ps *portalSession, name, want string, timeout time.Duration) (*agentManagerStatus, error) {
	var status agentManagerStatus
	var lastErr error
	reached := waitFor(int(timeout/pollInterval), pollInterval, func() bool {
		status = agentManagerStatus{}
		lastErr = portalToolCall(ps, "get_agent_status", map[string]any{nameKey: name, namespaceKey: kagentNamespace}, &status)
		return lastErr == nil && status.Verdict == want
	})
	if !reached {
		return nil, fmt.Errorf("get_agent_status for %s never said %s within %s (last: %s — %s; %v)", name, want, timeout, status.Verdict, excerpt(status.Summary, 200), lastErr)
	}
	return &status, nil
}

// agentManagerLoggedCaller checks agent-manager's log for a write it attributed
// to the person since the given duration — the identity the muster hop
// forwarded, as the service logs it.
func agentManagerLoggedCaller(email string, since time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	events, err := podLogs(ctx, platformNamespace, "deploy/"+agentManagerMCPServer, agentManagerMCPServer, since+5*time.Second)
	if err != nil {
		return fmt.Errorf("reading agent-manager's log: %w", err)
	}
	if !strings.Contains(events, "caller="+email) {
		return fmt.Errorf("agent-manager's log carries no `caller=%s` line for the write", email)
	}
	return nil
}

// proveCreatePath is the wizard's Deploy driven headlessly as the primary
// user: skill discovery, get_info, the dry run and its assertions, the create
// through the portal (the shared readiness wait), what landed against the dry
// run, the render and the status the detail page polls, a second create of
// the same name refused as a conflict, a viewer's refused as forbidden.
// Returns the spec the agent was created from, its template, and the verdict
// lines; the caller removes the agent on every path.
func proveCreatePath(primary, viewer *portalSession) (agentSpec, *agentTemplate, *agentManagerInfo, []string, error) {
	var verdicts []string
	email := primary.user.Email
	fail := func(err error) (agentSpec, *agentTemplate, *agentManagerInfo, []string, error) {
		return agentSpec{}, nil, nil, verdicts, err
	}

	step("Skill discovery through the portal: GET %s for %s, every skill pinned to the head commit", portalSkillsPath, skillsTestRepo)
	discovery, err := discoverSkills(primary, skillsTestRepo)
	if err != nil {
		return fail(err)
	}
	idx := slices.IndexFunc(discovery.Skills, func(s discoveredSkill) bool { return strings.Trim(s.Path, "/") == skillsTestSkill })
	if idx < 0 {
		return fail(fmt.Errorf("discovery lists no skill at %s in %s @ %.12s (%d skills)", skillsTestSkill, skillsTestRepo, discovery.Commit, len(discovery.Skills)))
	}
	skill := discovery.Skills[idx]
	note("%d skills of %s at %s %.12s (truncated=%v); picked %s (%s) @ %.12s", len(discovery.Skills), skillsTestRepo, firstNonEmpty(discovery.Ref, "default branch"), discovery.Commit, discovery.Truncated, skill.Name, skill.Path, skill.Commit)
	verdicts = append(verdicts, fmt.Sprintf("PASS: skill discovery (%s) returns %d commit-pinned entries for %s (head %.12s), among them %s", portalSkillsPath, len(discovery.Skills), skillsTestRepo, discovery.Commit, skill.Name))

	step("%sget_info and list_model_configs through the portal (the wizard's model step and the review page's chart facts)", agentManagerToolPrefix)
	var info agentManagerInfo
	if err := portalToolCall(primary, "get_info", nil, &info); err != nil {
		return fail(err)
	}
	if info.Chart.Semver != agentChartRange || info.Harness.Name == "" {
		return fail(fmt.Errorf("get_info reports chart %q on Harness %q, wanted %s on the platform Harness", info.Chart.Semver, info.Harness.Name, agentChartRange))
	}
	var configs struct {
		ModelConfigs []struct {
			Name string `json:"name"`
		} `json:"modelConfigs"`
	}
	if err := portalToolCall(primary, "list_model_configs", map[string]any{namespaceKey: kagentNamespace}, &configs); err != nil {
		return fail(err)
	}
	modelConfig := ""
	for _, mc := range configs.ModelConfigs {
		if modelConfig == "" || mc.Name == defaultModelConfig {
			modelConfig = mc.Name
		}
	}
	if modelConfig == "" {
		return fail(fmt.Errorf("list_model_configs lists no ModelConfig in %s", kagentNamespace))
	}
	note("chart %s@%s, Harness %s, muster.url %s, flux.serviceAccountName %q; ModelConfig %s", info.Chart.OCIURL, info.Chart.Semver, info.Harness.Name, firstNonEmpty(info.Muster.URL, defaultMusterMCPURL), info.Flux.ServiceAccountName, modelConfig)

	spec := agentSpec{
		Name: backstageTestAgent, ModelConfig: modelConfig, DisplayName: backstageTestDisplayName, Description: backstageTestDescription,
		SystemMessage: skillsTestSystemPrompt, IconURL: backstageTestIconURL, Toolset: []string{backstageTestToolset},
		Skills: []agentSkill{skill.skillEntry()},
	}
	step("The review page: %svalidate_agent dry run of the wizard's spec (toolset %v, skill %s @ %.12s)", agentManagerToolPrefix, spec.Toolset, skill.Name, skill.Commit)
	report, err := portalValidateAgent(primary, spec)
	if err != nil {
		return fail(err)
	}
	if err := assertDryRun(report, spec, &info); err != nil {
		return fail(err)
	}
	note("valid (mode %s, schema %s from %s): OCIRepository at %s, HelmRelease as %s, values.agent.harness=%s, no runtime, skill pinned at %.12s", report.Mode, report.SchemaVersion, report.SchemaSource, agentChartRange, kagentFluxServiceAccount, info.Harness.Name, skill.Commit)
	verdicts = append(verdicts, fmt.Sprintf("PASS: validate_agent through the portal renders the reviewed manifests — OCIRepository %s at %s, HelmRelease as %s, values on Harness %s without a runtime, skill %s pinned at %.12s", agentChartOCIRepository, agentChartRange, kagentFluxServiceAccount, info.Harness.Name, skill.Name, skill.Commit))

	writer := portalAgentManagerWriter{primary}
	step("Deploy: %s, then the shared readiness wait", writer)
	started := time.Now()
	template, written, err := readyAgent(writer, spec, backstageTestReadyTimeout)
	if err != nil {
		return fail(err)
	}
	if written.RequestedBy != email || !written.HelmRelease {
		return fail(fmt.Errorf("create_agent reported requestedBy=%q helmRelease=%v, wanted the release written by %s", written.RequestedBy, written.HelmRelease, email))
	}
	release, err := readAgentRelease(spec.Name)
	if err != nil {
		return fail(err)
	}
	if err := assertReleaseIsDryRun(release, report.Manifests, &info); err != nil {
		return fail(err)
	}
	if !slices.Contains(release.managers, agentManagerFieldManager) {
		return fail(fmt.Errorf("HelmRelease %s has no %s field manager: %q", spec.Name, agentManagerFieldManager, release.managers))
	}
	if err := agentManagerLoggedCaller(email, time.Since(started)); err != nil {
		return fail(err)
	}
	if err := assertAgentRender(template, spec, firstNonEmpty(info.Muster.URL, defaultMusterMCPURL)); err != nil {
		return fail(err)
	}
	rendered := template.skill(skill.Name)
	if rendered == nil || rendered.Source.Git == nil || rendered.Source.Git.Commit != skill.Commit || rendered.Source.Path != skill.Path {
		return fail(fmt.Errorf("AgentTemplate %s carries skill %s as %+v, wanted {git: {%s, %s}, path %s}", spec.Name, skill.Name, rendered, skill.RepoURL, skill.Commit, skill.Path))
	}
	harness := template.harness(kagentHarness)
	note("requestedBy=%s (OCIRepository created: %v); HelmRelease managers %q, serviceAccountName %s, values == the dry run's; OCIRepository %s at %s; agent-manager logged caller=%s; AgentTemplate Ready on Harness %s at revision %.12s with %s=%s, %s=%q, skill %s @ %.12s; RemoteMCPServer %s carries %s=%s",
		written.RequestedBy, written.OCIRepository, release.managers, release.Spec.ServiceAccountName, agentChartOCIRepository, agentChartRange, email, kagentHarness, harness.LatestSuccessfulRevision, harnessLabel, kagentHarness, displayNameAnnotation, spec.DisplayName, skill.Name, skill.Commit, spec.Name, toolsetHeader, backstageTestToolset)
	verdicts = append(verdicts, fmt.Sprintf("PASS: Deploy through the portal (POST /api/muster/call %screate_agent as %s) lands HelmRelease %s with exactly the dry run's values as %s next to OCIRepository %s at %s (requestedBy=%s, agent-manager logged caller=%s); the render is the AgentTemplate on Harness %s (%s, %s, %s, skill %s @ %.12s) and RemoteMCPServer %s with %s=%s, Ready in %s",
		agentManagerToolPrefix, email, spec.Name, kagentFluxServiceAccount, agentChartOCIRepository, agentChartRange, written.RequestedBy, email, kagentHarness, harnessLabel, displayNameAnnotation, iconURLAnnotation, skill.Name, skill.Commit, spec.Name, toolsetHeader, backstageTestToolset, time.Since(started).Round(time.Second)))

	step("The detail page's poll: %sget_agent_status through the portal says ready on Harness %s", agentManagerToolPrefix, kagentHarness)
	status, err := portalAgentStatus(primary, spec.Name, verdictReady, time.Minute)
	if err != nil {
		return fail(err)
	}
	if len(status.Template.Harnesses) == 0 || status.Template.Harnesses[0].Harness != info.Harness.Name || status.Template.Harnesses[0].Ready == nil || !*status.Template.Harnesses[0].Ready {
		return fail(fmt.Errorf("get_agent_status says %s — %s (template %+v), which is not Ready on Harness %s as the AgentTemplate's status.harnesses[] reports", status.Verdict, status.Summary, status.Template, info.Harness.Name))
	}
	note("%s: %s", status.Verdict, excerpt(status.Summary, 160))
	verdicts = append(verdicts, fmt.Sprintf("PASS: get_agent_status through the portal agrees with status.harnesses[] — %s on Harness %s", status.Verdict, info.Harness.Name))

	step("A second create of %s is refused as a conflict; %s's create as forbidden", spec.Name, viewerEmail(viewer))
	if _, err := writer.createAgent(spec); err == nil {
		return fail(fmt.Errorf("a second create_agent of %s was accepted; agent-manager refuses an existing name", spec.Name))
	} else if !strings.Contains(err.Error(), refusalConflict) {
		return fail(fmt.Errorf("the second create of %s was refused, but not as `%s …`: %w", spec.Name, refusalConflict, err))
	} else {
		note("duplicate: %s", excerpt(err.Error(), 200))
	}
	verdicts = append(verdicts, fmt.Sprintf("PASS: a second create_agent of %s answers `%s …`", spec.Name, refusalConflict))
	if viewer != nil {
		viewerSpec := agentSpec{Name: backstageTestAgent + "-viewer", ModelConfig: modelConfig, Toolset: []string{presetNone}}
		_, err := (portalAgentManagerWriter{viewer}).createAgent(viewerSpec)
		switch {
		case err == nil:
			return fail(fmt.Errorf("%s created an agent through the portal although the view role writes no HelmReleases — agent-manager is not acting as the caller", viewer.user.Email))
		case !strings.Contains(err.Error(), refusalForbidden):
			return fail(fmt.Errorf("%s: wanted `%s …`, got: %w", viewer.user.Email, refusalForbidden, err))
		}
		if agentExists(viewerSpec.Name) {
			return fail(fmt.Errorf("something of %s exists although the create was refused", viewerSpec.Name))
		}
		note("%s: %s", viewer.user.Email, excerpt(err.Error(), 200))
		verdicts = append(verdicts, fmt.Sprintf("PASS: %s's create_agent through the portal answers `%s …` (the person's RBAC, not a ServiceAccount's)", viewer.user.Email, refusalForbidden))
	}
	return spec, template, &info, verdicts, nil
}

// viewerEmail names the viewer for the step line, or says there is none.
func viewerEmail(viewer *portalSession) string {
	if viewer == nil {
		return "(no viewers-group user)"
	}
	return viewer.user.Email
}
