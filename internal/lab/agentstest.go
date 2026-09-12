package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// agentsTestAgent is the throwaway agent the proof creates, updates and
// deletes through agent-manager; agentsTestUnadmitted the template it places
// on no Harness (the direct writer, user story 14).
const (
	agentsTestAgent      = "agentlab-agents-test"
	agentsTestUnadmitted = agentsTestAgent + "-unadmitted"
	agentsTestViewer     = agentsTestAgent + "-viewer"
	// agentsTestNoHarness is the admission-label value nothing admits.
	agentsTestNoHarness = "agentlab-no-such-harness"
	// agentsTestIconURL is the avatar the proof declares, asserted as the
	// icon-url annotation on the render.
	agentsTestIconURL = "https://avatars.127.0.0.1.nip.io/agentlab-agents-test.svg"
)

// agentsTestToolset is the toolset the proof declares: the read-only preset,
// the smallest one that still gives the agent tools.
const agentsTestToolset = "preset:read-only"

// agentsTestReadyTimeout bounds the golden boot of the proof's agent: it
// carries a git skill, which the Go ADK fetches before it serves readyz.
const agentsTestReadyTimeout = 5 * time.Minute

// agentDescriptionValuePath is the changed path update_agent reports for a
// new description (the chart value).
const agentDescriptionValuePath = "agent.description"

// agentManagerServiceAccount is the ServiceAccount the umbrella's agent-manager
// Deployment runs with (fullnameOverride: agent-manager).
const agentManagerServiceAccount = "system:serviceaccount:" + platformNamespace + ":" + agentManagerMCPServer

// baselineServiceAccount is a ServiceAccount no binding names: what it may do
// is what every ServiceAccount may — discovery, the self-subject reviews, and
// whatever a cluster's bootstrap policy hands system:authenticated (the
// trust-bundle discovery once the ClusterTrustBundle gate is on). The
// agent-manager ServiceAccount's permissions of its own are what it holds
// beyond that.
const baselineServiceAccount = "system:serviceaccount:" + platformNamespace + ":agentlab-nobody"

// AgentsTest is the headless proof that agent-manager acts as the signed-in
// user and writes the Generic chart 1.x contract, through the platform path
// only (Dex id_token -> muster -> call_tool x_agent-manager_*): get_info
// reports identity caller on the 1.x chart and the platform Harness; the
// admin's create -> ready -> turn -> update -> refreshSkills -> delete round
// trip succeeds with requestedBy set — the HelmRelease written by
// agent-manager (its field manager, values.toolset the anchor, the skill
// pinned to a commit) next to the OCIRepository at 1.x, the render an
// AgentTemplate carrying the Harness label and the display-name and icon-url
// annotations, Ready on the platform Harness with its skill, the per-agent
// RemoteMCPServer carrying the toolset header; get_agent_status agrees with
// status.harnesses[] for ready and for a template no Harness admits; a
// viewers-group user's create is refused by the kind apiserver as
// User "oidc:viewer@lab.local" (the view role writes no HelmReleases); and the
// agent-manager ServiceAccount holds nothing beyond API discovery. Leaves
// nothing behind.
func AgentsTest(cfg *config.Config, email string) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled || !cfg.Platform.Agents {
		return fmt.Errorf("platform.agents is off in %s — enable it and run `agentlab platform` first", config.File)
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}
	fixture, err := SkillsFixture{}.resolve()
	if err != nil {
		return err
	}

	step("Logging in to Dex as %s", email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	note("got an id_token")

	step("MCP tools through muster (%s*)", agentManagerToolPrefix)
	session, err := openMusterSession(cfg, token, "agents-test")
	if err != nil {
		return err
	}
	tools, err := session.listTools()
	if err != nil {
		return err
	}
	var amTools []string
	for _, t := range tools {
		if strings.HasPrefix(t, agentManagerToolPrefix) {
			amTools = append(amTools, strings.TrimPrefix(t, agentManagerToolPrefix))
		}
	}
	if len(amTools) == 0 {
		return fmt.Errorf("muster aggregates no %s tools; check `kubectl -n %s get mcpservers.muster.giantswarm.io %s` and `agentlab logs muster`", agentManagerToolPrefix, platformNamespace, agentManagerMCPServer)
	}
	slices.Sort(amTools)
	note("%d tools: %s", len(amTools), strings.Join(amTools, ", "))

	step("%sget_info — expecting identity caller on the agent chart %s and Harness %s", agentManagerToolPrefix, agentChartRange, kagentHarness)
	info, err := readAgentManagerInfo(session)
	if err != nil {
		return err
	}
	if info.Identity != "caller" || !info.Capabilities["writesAsCaller"] {
		return fmt.Errorf("agent-manager reports identity=%q writesAsCaller=%v: it is not running with downstream OAuth (umbrella agent-manager.oauth.downstream)", info.Identity, info.Capabilities["writesAsCaller"])
	}
	if got := info.APIVersions.AgentTemplate; got != agentTemplateAPIVersion {
		return fmt.Errorf("agent-manager composes apiVersions.agentTemplate=%q, the platform's agents are %s AgentTemplates — this agent-manager does not speak kagent API v2", got, agentTemplateAPIVersion)
	}
	if info.Chart.Semver != agentChartRange || info.Chart.OCIURL != agentChartURL {
		return fmt.Errorf("agent-manager tracks the agent chart %s at %q, wanted %s at %s (Generic chart 1.x)", info.Chart.OCIURL, info.Chart.Semver, agentChartURL, agentChartRange)
	}
	if info.Harness.Name != kagentHarness {
		return fmt.Errorf("agent-manager places agents on Harness %q, the platform Harness is %s", info.Harness.Name, kagentHarness)
	}
	musterURL := firstNonEmpty(info.Muster.URL, defaultMusterMCPURL)
	note("version %s, identity %s, chart %s@%s (latest %s, schema %s from %s), Harness %s, muster.url %s, flux.serviceAccountName %q, default namespace %s",
		info.Version, info.Identity, info.Chart.OCIURL, info.Chart.Semver, firstNonEmpty(info.Chart.LatestVersion, "unread"), info.Chart.SchemaVersion, info.Chart.SchemaSource,
		info.Harness.Name, musterURL, info.Flux.ServiceAccountName, info.Namespaces.Default)

	step("%slist_model_configs", agentManagerToolPrefix)
	modelConfig, err := agentManagerModelConfig(session)
	if err != nil {
		return err
	}
	note("using ModelConfig %s", modelConfig)

	// Leftovers of an aborted run first, and everything this run creates on
	// every exit path.
	cleanup := func() {
		for _, name := range []string{agentsTestAgent, agentsTestUnadmitted, agentsTestViewer} {
			if !agentExists(name) {
				continue
			}
			note("cleanup: removing %s", name)
			if err := removeAgent(name); err != nil {
				note("cleanup: %v", err)
			}
		}
	}
	cleanup()
	defer cleanup()

	step("%slist_skills — the head commit of %s (the commit refreshSkills re-pins to)", agentManagerToolPrefix, fixture.Repo)
	// agent-manager reads GitHub for this and for the create_agent pin
	// below; its window is this machine's (githubwindow.go) — printed, and
	// waited for once when exhausted.
	if err := awaitGitHubWindow("agent-manager's skill resolution"); err != nil {
		return err
	}
	head, err := agentManagerSkillHead(session, fixture)
	if err != nil {
		return err
	}
	note("%s @ %.12s lists skill %s (path %s)", fixture.Repo, head, fixture.name(), fixture.Skill)

	// Every agent declares a toolset (agent-manager refuses a create without
	// one, naming the shipped presets); agents-test is the platform path, so
	// it declares the read-only preset and asserts the refusal.
	spec := agentSpec{
		Name: agentsTestAgent, ModelConfig: modelConfig, DisplayName: agentsTestDisplayName, IconURL: agentsTestIconURL,
		Description:   "Throwaway agent of `agentlab agents-test`; deleted by the same run.",
		SystemMessage: skillsTestSystemPrompt,
		Skills:        []agentSkill{{Name: fixture.name(), Path: fixture.Skill, Git: &gitSkill{URL: fixture.Repo, Commit: fixture.Commit}}},
	}
	step("%screate_agent %s without a toolset — expecting the refusal naming the presets", agentManagerToolPrefix, agentsTestAgent)
	if _, err := (agentManagerWriter{session}).createAgent(spec); err == nil {
		return fmt.Errorf("create_agent without a toolset was accepted; agent-manager requires one")
	} else if !strings.Contains(err.Error(), toolsetKey) || !strings.Contains(err.Error(), presetNone) {
		return fmt.Errorf("the refusal does not name the toolset contract: %w", err)
	} else {
		note("refused: %s", excerpt(err.Error(), 200))
	}
	if agentExists(agentsTestAgent) {
		return fmt.Errorf("something of %s exists although the create was refused", agentsTestAgent)
	}

	step("%screate_agent %s as %s: toolset [%s], skill %s @ %.12s", agentManagerToolPrefix, agentsTestAgent, user.Email, agentsTestToolset, fixture.name(), fixture.Commit)
	spec.Toolset = []string{agentsTestToolset}
	written, err := (agentManagerWriter{session}).createAgent(spec)
	if err != nil {
		return err
	}
	if !written.HelmRelease {
		return fmt.Errorf("create_agent reported no HelmRelease written")
	}
	if written.RequestedBy != user.Email {
		return fmt.Errorf("create_agent carries requestedBy=%q, wanted %q: agent-manager did not learn the caller from the forwarded token", written.RequestedBy, user.Email)
	}
	note("HelmRelease written (OCIRepository created: %v), requestedBy=%s", written.OCIRepository, written.RequestedBy)

	step("The HelmRelease is agent-manager's write (field manager %s) on the chart at %s; values.toolset is the anchor, the skill is pinned", agentManagerFieldManager, agentChartRange)
	release, err := readAgentRelease(agentsTestAgent)
	if err != nil {
		return err
	}
	if !slices.Contains(release.managers, agentManagerFieldManager) {
		return fmt.Errorf("HelmRelease %s has no %s field manager: %q", agentsTestAgent, agentManagerFieldManager, release.managers)
	}
	if release.Spec.ChartRef.Kind != kindOCIRepository || release.Spec.ChartRef.Name != agentChartOCIRepository {
		return fmt.Errorf("HelmRelease %s renders from chartRef %+v, wanted the OCIRepository %s of the namespace", agentsTestAgent, release.Spec.ChartRef, agentChartOCIRepository)
	}
	toolset, declared, err := release.toolset()
	if err != nil {
		return err
	}
	if !declared || !slices.Equal(toolset, spec.Toolset) {
		return fmt.Errorf("HelmRelease %s carries values.toolset=%v (declared %v), wanted %v", agentsTestAgent, toolset, declared, spec.Toolset)
	}
	if got := release.value("agent", "harness"); got != kagentHarness {
		return fmt.Errorf("HelmRelease %s carries values.agent.harness=%q, wanted %s (the admission label's value)", agentsTestAgent, got, kagentHarness)
	}
	if got := release.skillCommits()[fixture.name()]; got != fixture.Commit {
		return fmt.Errorf("HelmRelease %s pins skill %s at %q, wanted %s", agentsTestAgent, fixture.name(), got, fixture.Commit)
	}
	chartURL, chartRange, sourceManagers, err := agentChartSource()
	if err != nil {
		return fmt.Errorf("the namespace's OCIRepository %s: %w", agentChartOCIRepository, err)
	}
	if chartURL != agentChartURL || chartRange != agentChartRange {
		return fmt.Errorf("OCIRepository %s tracks %s at %q, wanted %s at %s", agentChartOCIRepository, chartURL, chartRange, agentChartURL, agentChartRange)
	}
	logCtx, cancelLogs := context.WithTimeout(context.Background(), 60*time.Second)
	events, _ := podLogs(logCtx, platformNamespace, "deploy/"+agentManagerMCPServer, agentManagerMCPServer, 5*time.Minute)
	cancelLogs()
	if !strings.Contains(events, "caller="+user.Email) {
		return fmt.Errorf("agent-manager's log carries no `caller=%s` line for the create", user.Email)
	}
	note("HelmRelease managers %q, chartRef OCIRepository/%s, serviceAccountName %q, values.toolset %v, agent.harness %s, skill %s @ %.12s; OCIRepository %s -> %s @ %s (managers %q); agent-manager logged the write with caller=%s",
		release.managers, release.Spec.ChartRef.Name, release.Spec.ServiceAccountName, toolset, kagentHarness, fixture.name(), fixture.Commit, agentChartOCIRepository, chartURL, chartRange, sourceManagers, user.Email)

	step("The render: AgentTemplate %s with the Harness label and the display-name and icon-url annotations, its RemoteMCPServer %s carrying %s", agentsTestAgent, agentsTestAgent, toolsetHeader)
	template, err := waitAgentTemplate(agentsTestAgent)
	if err != nil {
		return err
	}
	if err := assertAgentRender(template, spec, musterURL); err != nil {
		return err
	}
	skill := template.skill(fixture.name())
	if skill == nil || skill.Source.Git == nil || skill.Source.Git.URL != fixture.Repo || skill.Source.Git.Commit != fixture.Commit || skill.Source.Path != fixture.Skill {
		return fmt.Errorf("AgentTemplate %s carries skill %s as %+v, wanted {git: {%s, %s}, path %s}", agentsTestAgent, fixture.name(), skill, fixture.Repo, fixture.Commit, fixture.Skill)
	}
	note("%s=%s, %s=%q, %s=%s, ModelConfig %s, skill %s @ %.12s; RemoteMCPServer %s -> %s with %s=%s, %s=%s; both rendered by HelmRelease %s",
		harnessLabel, kagentHarness, displayNameAnnotation, agentsTestDisplayName, iconURLAnnotation, agentsTestIconURL, modelConfig, fixture.name(), fixture.Commit,
		agentsTestAgent, musterURL, toolsetHeader, agentsTestToolset, discoveryLabel, discoveryDisabledValue, agentsTestAgent)

	step("Waiting for %s to be Ready on Harness %s (the golden boot fetches the skill)", agentsTestAgent, kagentHarness)
	readiness, err := waitAgentReady(agentsTestAgent, agentsTestReadyTimeout)
	if err != nil {
		return err
	}
	if !readiness.ready {
		return readiness.failure(agentsTestAgent, agentsTestReadyTimeout)
	}
	harness := readiness.template.harness(kagentHarness)
	note("Ready after %s: revision %.12s%s", readiness.elapsed.Round(time.Second), harness.LatestSuccessfulRevision, warningsNote(harness.Warnings))

	step("%sget_agent_status agrees with status.harnesses[]: ready on revision %.12s", agentManagerToolPrefix, harness.LatestSuccessfulRevision)
	status, err := waitAgentVerdict(session, agentsTestAgent, verdictReady)
	if err != nil {
		return err
	}
	if !strings.Contains(status.Summary, harness.LatestSuccessfulRevision) || len(status.Template.Harnesses) == 0 || status.Template.Harnesses[0].Harness != kagentHarness || status.Template.Harnesses[0].Ready == nil || !*status.Template.Harnesses[0].Ready {
		return fmt.Errorf("get_agent_status says %s — %s (template %+v), which is not the cluster's Ready revision %s on Harness %s", status.Verdict, status.Summary, status.Template, harness.LatestSuccessfulRevision, kagentHarness)
	}
	note("%s: %s", status.Verdict, excerpt(status.Summary, 160))

	step("%sget_agent reports the contract back", agentManagerToolPrefix)
	got, err := readAgentManagerAgent(session, agentsTestAgent)
	if err != nil {
		return err
	}
	if !slices.Equal(got.Toolset, spec.Toolset) || got.ImplicitFullAccess {
		return fmt.Errorf("get_agent reports toolset=%v implicitFullAccess=%v, wanted %v/false", got.Toolset, got.ImplicitFullAccess, spec.Toolset)
	}
	if len(got.Tools) != 1 || got.Tools[0].Server != agentsTestAgent {
		return fmt.Errorf("get_agent reports tools=%+v, wanted the one binding of RemoteMCPServer %s", got.Tools, agentsTestAgent)
	}
	if got.Ready == nil || !*got.Ready || len(got.Harnesses) != 1 || got.Harnesses[0].Harness != kagentHarness {
		return fmt.Errorf("get_agent reports ready=%v harnesses=%+v, wanted Ready on %s", got.Ready, got.Harnesses, kagentHarness)
	}
	if len(got.Skills) != 1 || got.Skills[0].Git == nil || got.Skills[0].Git.Commit != fixture.Commit {
		return fmt.Errorf("get_agent reports skills=%+v, wanted %s pinned at %s", got.Skills, fixture.name(), fixture.Commit)
	}
	if got.Managed != "helmrelease" || got.HelmRelease == nil || !strings.HasPrefix(got.HelmRelease.ChartVersion, "1.") {
		return fmt.Errorf("get_agent reports managed=%q helmRelease=%+v, wanted helmrelease on a 1.x chart", got.Managed, got.HelmRelease)
	}
	note("toolset %v on Harness %s (ready), binding %s, skill %s @ %.12s, managed %s, chart %s", got.Toolset, kagentHarness, got.Tools[0].Server, got.Skills[0].Name, got.Skills[0].Git.Commit, got.Managed, got.HelmRelease.ChartVersion)

	step("One turn through the edge as %s: the agent names its skill and answers from the skill's text", user.Email)
	reply, err := firstTurnAs(cfg, agentsTestAgent, token, fixture.prompt())
	if err != nil {
		return err
	}
	if err := skillReplyProves(reply, fixture.name(), fixture.Expect); err != nil {
		return fmt.Errorf("the skill was not in effect on the turn: %w", err)
	}
	note("answered: %s", excerpt(reply, 200))

	step("%supdate_agent as %s: a new description", agentManagerToolPrefix, user.Email)
	updated, err := agentManagerUpdate(session, map[string]any{nameKey: agentsTestAgent, descriptionKey: "Updated by agentlab agents-test."})
	if err != nil {
		return err
	}
	if updated.RequestedBy != user.Email || !slices.Contains(updated.Changed, agentDescriptionValuePath) {
		return fmt.Errorf("update_agent: requestedBy=%q changed=%v, wanted %s changed by %s", updated.RequestedBy, updated.Changed, agentDescriptionValuePath, user.Email)
	}
	note("changed %v, requestedBy=%s", updated.Changed, updated.RequestedBy)

	step("%supdate_agent refreshSkills — the skill re-pins to %s's head %.12s", agentManagerToolPrefix, fixture.Repo, head)
	refreshed, err := agentManagerUpdate(session, map[string]any{nameKey: agentsTestAgent, refreshSkillsKey: true})
	if err != nil {
		return err
	}
	if got := skillCommits(refreshed.After)[fixture.name()]; got != head {
		return fmt.Errorf("update_agent refreshSkills pinned %s at %q, wanted the head %s", fixture.name(), got, head)
	}
	skillsChanged := slices.ContainsFunc(refreshed.Changed, func(p string) bool { return strings.HasPrefix(p, skillsKey) })
	if moved := head != fixture.Commit; moved != skillsChanged {
		return fmt.Errorf("update_agent refreshSkills reported changed=%v although the head %.12s %s the pin %.12s", refreshed.Changed, head, map[bool]string{true: "differs from", false: "equals"}[moved], fixture.Commit)
	}
	if release, err = readAgentRelease(agentsTestAgent); err != nil {
		return err
	}
	if got := release.skillCommits()[fixture.name()]; got != head {
		return fmt.Errorf("HelmRelease %s pins skill %s at %q after refreshSkills, wanted %s", agentsTestAgent, fixture.name(), got, head)
	}
	repinned := waitFor(int(time.Minute/pollInterval), pollInterval, func() bool {
		t, err := readAgentTemplate(agentsTestAgent)
		if err != nil {
			return false
		}
		s := t.skill(fixture.name())
		return s != nil && s.Source.Git != nil && s.Source.Git.Commit == head
	})
	if !repinned {
		return fmt.Errorf("AgentTemplate %s does not carry the re-pinned commit %.12s a minute after refreshSkills", agentsTestAgent, head)
	}
	if head == fixture.Commit {
		note("the head is the pin: nothing changed (changed %v), the commit stays %.12s", refreshed.Changed, head)
	} else {
		note("re-pinned %.12s -> %.12s (changed %v); waiting for the new revision's golden boot", fixture.Commit, head, refreshed.Changed)
		if readiness, err = waitAgentReady(agentsTestAgent, agentsTestReadyTimeout); err != nil {
			return err
		}
		if !readiness.ready {
			return readiness.failure(agentsTestAgent, agentsTestReadyTimeout)
		}
		note("Ready again after %s: revision %.12s", readiness.elapsed.Round(time.Second), readiness.template.harness(kagentHarness).LatestSuccessfulRevision)
	}

	// The user's identity, not a ServiceAccount: a viewer (the view
	// ClusterRole, no HelmRelease writes anywhere) is refused by the kind
	// apiserver under the user's own name. A shared ServiceAccount would let
	// both users through alike.
	viewer := cfg.FindUserInGroup("viewers")
	if viewer == nil {
		note("skipping the viewer proof: %s has no viewers-group user", config.File)
	} else {
		step("%screate_agent as %s — expecting the apiserver's Forbidden for User \"oidc:%s\"", agentManagerToolPrefix, viewer.Email, viewer.Email)
		viewerToken, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
			viewer.Email, viewer.Password, musterLoginScopes)
		if err != nil {
			return err
		}
		viewerSession, err := openMusterSession(cfg, viewerToken, "agents-test-viewer")
		if err != nil {
			return err
		}
		_, err = (agentManagerWriter{viewerSession}).createAgent(agentSpec{Name: agentsTestViewer, ModelConfig: modelConfig, Toolset: []string{agentsTestToolset}})
		switch {
		case err == nil:
			return fmt.Errorf("%s created an agent through agent-manager although the view role cannot write HelmReleases — agent-manager is not acting as the caller (ServiceAccount fallback?)", viewer.Email)
		case !strings.Contains(strings.ToLower(err.Error()), "forbidden"):
			return fmt.Errorf("%s: wanted the apiserver's Forbidden, got: %w", viewer.Email, err)
		case !strings.Contains(err.Error(), `User "oidc:`+viewer.Email+`"`):
			return fmt.Errorf("%s: Forbidden, but not under the user's own name (the apiserver saw someone else): %w", viewer.Email, err)
		}
		note("%s: %s", viewer.Email, excerpt(err.Error(), 200))
		if agentExists(agentsTestViewer) {
			return fmt.Errorf("something of %s exists although the create was refused", agentsTestViewer)
		}
	}

	step("The agent-manager ServiceAccount holds no RBAC of its own (auth can-i --list --as=%s, against a ServiceAccount no binding names)", agentManagerServiceAccount)
	rules, err := serviceAccountRules(agentManagerServiceAccount, kagentNamespace)
	if err != nil {
		return err
	}
	baseline, err := serviceAccountRules(baselineServiceAccount, kagentNamespace)
	if err != nil {
		return err
	}
	granted := rulesBeyond(rules, baseline)
	if len(granted) > 0 {
		return fmt.Errorf("the agent-manager ServiceAccount still holds permissions in %s beyond every ServiceAccount's:\n%s", kagentNamespace, strings.Join(granted, "\n"))
	}
	note("nothing beyond what every ServiceAccount holds (%d rows: discovery, self-subject reviews%s)", len(baseline), bootstrapExtras(baseline))

	step("A template no Harness admits: HelmRelease %s with agent.harness %s applied directly — get_agent_status says failed with the reason", agentsTestUnadmitted, agentsTestNoHarness)
	unadmittedSpec := agentSpec{
		Name: agentsTestUnadmitted, ModelConfig: modelConfig, Toolset: []string{presetNone}, Harness: agentsTestNoHarness,
		Description: "Throwaway agent of `agentlab agents-test` on a Harness that does not exist; deleted by the same run.",
	}
	if _, err := (helmReleaseWriter{}).createAgent(unadmittedSpec); err != nil {
		return err
	}
	unadmitted, err := waitAgentReady(agentsTestUnadmitted, 2*time.Minute)
	if err != nil {
		return err
	}
	if unadmitted.ready || !unadmitted.terminal || !strings.Contains(unadmitted.reason, "no Harness admits") {
		return fmt.Errorf("the template on Harness %q: ready=%v terminal=%v (%s); wanted the not-admitted verdict from status.harnesses[] (empty, observedGeneration caught up)", agentsTestNoHarness, unadmitted.ready, unadmitted.terminal, unadmitted.reason)
	}
	failed, err := waitAgentVerdict(session, agentsTestUnadmitted, verdictFailed)
	if err != nil {
		return err
	}
	if !strings.Contains(failed.Summary, "no Harness admits") || !strings.Contains(failed.Summary, kagentHarness) {
		return fmt.Errorf("get_agent_status says %s — %s; wanted the not-admitted reason naming the Harnesses of the namespace (%s)", failed.Verdict, failed.Summary, kagentHarness)
	}
	note("cluster: %s", unadmitted.reason)
	note("get_agent_status: %s — %s", failed.Verdict, excerpt(failed.Summary, 240))
	if err := removeAgent(agentsTestUnadmitted); err != nil {
		return err
	}

	step("%sdelete_agent %s as %s — the HelmRelease goes and the render with it", agentManagerToolPrefix, agentsTestAgent, user.Email)
	var deleted struct {
		RequestedBy          string `json:"requestedBy"`
		HelmReleaseDeleted   bool   `json:"helmReleaseDeleted"`
		OCIRepositoryDeleted bool   `json:"ociRepositoryDeleted"`
		OCIRepositoryKept    string `json:"ociRepositoryKept"`
	}
	if err := session.callServerJSON(agentManagerToolPrefix+"delete_agent", map[string]any{nameKey: agentsTestAgent}, &deleted); err != nil {
		return err
	}
	if !deleted.HelmReleaseDeleted || deleted.RequestedBy != user.Email {
		return fmt.Errorf("delete_agent: helmReleaseDeleted=%v requestedBy=%q, wanted the release deleted by %s", deleted.HelmReleaseDeleted, deleted.RequestedBy, user.Email)
	}
	if !deleted.OCIRepositoryDeleted && deleted.OCIRepositoryKept == "" {
		return fmt.Errorf("delete_agent neither deleted the OCIRepository %s nor said why it stays", agentChartOCIRepository)
	}
	if err := waitAgentRemoved(agentsTestAgent); err != nil {
		return err
	}
	if err := waitAgentUnlisted(session, agentsTestAgent); err != nil {
		return err
	}
	source := "OCIRepository " + agentChartOCIRepository + " deleted"
	if !deleted.OCIRepositoryDeleted {
		source = "OCIRepository " + agentChartOCIRepository + " kept (" + deleted.OCIRepositoryKept + ")"
	}
	note("HelmRelease, AgentTemplate and RemoteMCPServer %s are gone, %s, requestedBy=%s; list_agents agrees", agentsTestAgent, source, deleted.RequestedBy)

	fmt.Println()
	fmt.Printf("PASS: muster aggregates %s* and agent-manager reports identity caller on the agent chart %s (Harness %s)\n", agentManagerToolPrefix, agentChartRange, kagentHarness)
	fmt.Printf("PASS: create_agent without a toolset is refused naming the presets; with [%s] %s created -> Ready on Harness %s -> a turn as the person -> updated -> refreshSkills -> deleted %s through call_tool, every write requestedBy=%s and logged with caller=\n", agentsTestToolset, user.Email, kagentHarness, agentsTestAgent, user.Email)
	fmt.Printf("PASS: the HelmRelease carries the %s field manager, values.toolset [%s], agent.harness %s and the skill %s pinned to a commit next to OCIRepository %s at %s; its render is the AgentTemplate with %s=%s, %s and %s, the skill entry, and RemoteMCPServer %s carrying %s=%s (%s=%s); delete_agent removes the release and the render\n",
		agentManagerFieldManager, agentsTestToolset, kagentHarness, fixture.name(), agentChartOCIRepository, agentChartRange, harnessLabel, kagentHarness, displayNameAnnotation, iconURLAnnotation, agentsTestAgent, toolsetHeader, agentsTestToolset, discoveryLabel, discoveryDisabledValue)
	fmt.Printf("PASS: get_agent_status agrees with status.harnesses[] — ready on revision %.12s, and failed with the not-admitted reason for a template on Harness %q; refreshSkills re-pins %s to %s's head %.12s\n", harness.LatestSuccessfulRevision, agentsTestNoHarness, fixture.name(), fixture.Repo, head)
	if viewer != nil {
		fmt.Printf("PASS: %s's create is Forbidden by the apiserver as User \"oidc:%s\" (user RBAC, not the ServiceAccount's)\n", viewer.Email, viewer.Email)
	}
	fmt.Printf("PASS: %s holds no permissions beyond discovery\n", agentManagerServiceAccount)
	return nil
}

// defaultMusterMCPURL is the agent chart's default muster.url: the platform's
// in-cluster muster, what an agent-manager with no muster.url of its own
// leaves in place.
const defaultMusterMCPURL = "http://muster.agent-platform.svc.cluster.local:8090/mcp"

// assertAgentRender checks the two objects the chart renders for a spec: the
// AgentTemplate (the Harness label, the annotations, the model, the prompt,
// the provenance label) and, unless the spec is chat-only, the agent's
// RemoteMCPServer (muster's URL, the toolset header, discovery off) bound as
// its tools.
func assertAgentRender(t *agentTemplate, spec agentSpec, musterURL string) error {
	name := spec.Name
	if got := t.Metadata.Labels[harnessLabel]; got != spec.harnessValue() {
		return fmt.Errorf("AgentTemplate %s carries %s=%q, wanted %q (the Harness admits by it)", name, harnessLabel, got, spec.harnessValue())
	}
	if got := t.Metadata.Labels[fluxHelmReleaseNameLabel]; got != name {
		return fmt.Errorf("AgentTemplate %s carries %s=%q, wanted %s (rendered by the agent's HelmRelease)", name, fluxHelmReleaseNameLabel, got, name)
	}
	if got := t.Metadata.Annotations[displayNameAnnotation]; got != spec.DisplayName {
		return fmt.Errorf("AgentTemplate %s carries %s=%q, wanted %q", name, displayNameAnnotation, got, spec.DisplayName)
	}
	if got := t.Metadata.Annotations[iconURLAnnotation]; got != spec.IconURL {
		return fmt.Errorf("AgentTemplate %s carries %s=%q, wanted %q", name, iconURLAnnotation, got, spec.IconURL)
	}
	if t.Spec.ModelConfig == nil || t.Spec.ModelConfig.Name != spec.ModelConfig {
		return fmt.Errorf("AgentTemplate %s names ModelConfig %v, wanted %s", name, t.Spec.ModelConfig, spec.ModelConfig)
	}
	if spec.SystemMessage != "" && strings.TrimSpace(t.Spec.SystemPrompt) != strings.TrimSpace(spec.SystemMessage) {
		return fmt.Errorf("AgentTemplate %s carries systemPrompt %q, wanted %q", name, excerpt(t.Spec.SystemPrompt, 80), excerpt(spec.SystemMessage, 80))
	}
	chatOnly := len(spec.Toolset) == 1 && spec.Toolset[0] == presetNone
	if chatOnly {
		if bound := t.mcpServer(); bound != "" {
			return fmt.Errorf("AgentTemplate %s (toolset [%s]) binds RemoteMCPServer %q, wanted no MCP binding at all", name, presetNone, bound)
		}
		if _, err := readKagentObject(remoteMCPServerResource, name); !apierrors.IsNotFound(err) {
			return fmt.Errorf("RemoteMCPServer %s exists (or cannot be read: %v) although the toolset is [%s]: the chart renders none for a chat-only agent", name, err, presetNone)
		}
		return nil
	}
	if bound := t.mcpServer(); bound != name {
		return fmt.Errorf("AgentTemplate %s binds RemoteMCPServer %q, wanted its own %s", name, bound, name)
	}
	rms, err := readKagentObject(remoteMCPServerResource, name)
	if err != nil {
		return fmt.Errorf("the agent's RemoteMCPServer %s: %w", name, err)
	}
	if got, _, _ := unstructured.NestedString(rms.Object, "spec", "url"); got != musterURL {
		return fmt.Errorf("RemoteMCPServer %s points at %q, wanted muster at %s", name, got, musterURL)
	}
	if got := rms.GetLabels()[discoveryLabel]; got != discoveryDisabledValue {
		return fmt.Errorf("RemoteMCPServer %s carries %s=%q, wanted %s (muster is an OAuth resource server; the controller holds no user token)", name, discoveryLabel, got, discoveryDisabledValue)
	}
	if got := rms.GetLabels()[fluxHelmReleaseNameLabel]; got != name {
		return fmt.Errorf("RemoteMCPServer %s carries %s=%q, wanted %s", name, fluxHelmReleaseNameLabel, got, name)
	}
	header, found, err := remoteMCPServerHeader(rms, toolsetHeader)
	if err != nil {
		return err
	}
	if _, authz, _ := remoteMCPServerHeader(rms, "Authorization"); authz {
		return fmt.Errorf("RemoteMCPServer %s carries an Authorization header: a static one would override the person's forwarded token", name)
	}
	switch want := strings.Join(spec.Toolset, ","); {
	case spec.Toolset == nil && found:
		return fmt.Errorf("RemoteMCPServer %s carries %s=%q although the release declares no toolset (implicit full access renders no header)", name, toolsetHeader, header)
	case spec.Toolset != nil && (!found || header != want):
		return fmt.Errorf("RemoteMCPServer %s carries %s=%q (present %v), wanted %q", name, toolsetHeader, header, found, want)
	}
	return nil
}

// agentManagerInfo is get_info as the proofs read it.
type agentManagerInfo struct {
	Version      string          `json:"version"`
	Identity     string          `json:"identity"`
	Capabilities map[string]bool `json:"capabilities"`
	Chart        struct {
		OCIURL        string `json:"ociUrl"`
		Semver        string `json:"semver"`
		LatestVersion string `json:"latestVersion"`
		SchemaVersion string `json:"schemaVersion"`
		SchemaSource  string `json:"schemaSource"`
	} `json:"chart"`
	APIVersions struct {
		AgentTemplate string `json:"agentTemplate"`
	} `json:"apiVersions"`
	Flux struct {
		ServiceAccountName string `json:"serviceAccountName"`
	} `json:"flux"`
	Harness struct {
		Name string `json:"name"`
	} `json:"harness"`
	Muster struct {
		URL string `json:"url"`
	} `json:"muster"`
	Namespaces struct {
		Default string `json:"default"`
	} `json:"namespaces"`
}

func readAgentManagerInfo(s *musterSession) (*agentManagerInfo, error) {
	var info agentManagerInfo
	if err := s.callServerJSON(agentManagerToolPrefix+"get_info", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// agentManagerModelConfig is the ModelConfig the throwaway agents run on:
// the lab's default when list_model_configs has it, else the first.
func agentManagerModelConfig(s *musterSession) (string, error) {
	var configs struct {
		ModelConfigs []struct {
			Name string `json:"name"`
		} `json:"modelConfigs"`
	}
	if err := s.callServerJSON(agentManagerToolPrefix+"list_model_configs", nil, &configs); err != nil {
		return "", err
	}
	if len(configs.ModelConfigs) == 0 {
		return "", fmt.Errorf("no ModelConfig in %s — the platform's default one is missing", kagentNamespace)
	}
	for _, mc := range configs.ModelConfigs {
		if mc.Name == defaultModelConfig {
			return mc.Name, nil
		}
	}
	return configs.ModelConfigs[0].Name, nil
}

// listedSkill is one skill of list_skills' answer: its frontmatter name and
// its directory within the repository.
type listedSkill struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// agentManagerSkillHead is list_skills for the fixture's repository: the head
// commit of its default branch, which the repository must list the fixture's
// skill under.
func agentManagerSkillHead(s *musterSession, fixture SkillsFixture) (string, error) {
	var listed struct {
		Repositories []struct {
			RepoURL string        `json:"repoUrl"`
			Ref     string        `json:"ref"`
			Commit  string        `json:"commit"`
			Error   string        `json:"error"`
			Skills  []listedSkill `json:"skills"`
		} `json:"repositories"`
	}
	if err := s.callServerJSON(agentManagerToolPrefix+"list_skills", map[string]any{"repository": fixture.Repo}, &listed); err != nil {
		return "", err
	}
	for _, repo := range listed.Repositories {
		if strings.TrimSuffix(repo.RepoURL, "/") != strings.TrimSuffix(fixture.Repo, "/") {
			continue
		}
		if repo.Error != "" {
			return "", fmt.Errorf("list_skills could not read %s: %s", fixture.Repo, repo.Error)
		}
		if !fullCommitID.MatchString(repo.Commit) {
			return "", fmt.Errorf("list_skills reports %q as %s's head, not a full commit id", repo.Commit, fixture.Repo)
		}
		if !slices.ContainsFunc(repo.Skills, func(s listedSkill) bool { return strings.Trim(s.Path, "/") == fixture.Skill }) {
			return "", fmt.Errorf("list_skills lists no skill at %s in %s @ %.12s", fixture.Skill, fixture.Repo, repo.Commit)
		}
		return repo.Commit, nil
	}
	return "", fmt.Errorf("list_skills lists nothing for %s (is it among agent-manager's skills repositories?)", fixture.Repo)
}

// agentManagerAgent is get_agent as the proofs read it.
type agentManagerAgent struct {
	Name               string   `json:"name"`
	Toolset            []string `json:"toolset"`
	ImplicitFullAccess bool     `json:"implicitFullAccess"`
	Ready              *bool    `json:"ready"`
	Managed            string   `json:"managed"`
	Tools              []struct {
		Server string `json:"server"`
	} `json:"tools"`
	Harnesses []struct {
		Harness string `json:"harness"`
		Ready   *bool  `json:"ready"`
	} `json:"harnesses"`
	Skills      []agentSkill `json:"skills"`
	HelmRelease *struct {
		Name         string `json:"name"`
		ChartVersion string `json:"chartVersion"`
	} `json:"helmRelease"`
}

func readAgentManagerAgent(s *musterSession, name string) (*agentManagerAgent, error) {
	var got agentManagerAgent
	if err := s.callServerJSON(agentManagerToolPrefix+"get_agent", map[string]any{nameKey: name}, &got); err != nil {
		return nil, err
	}
	return &got, nil
}

// agentManagerUpdateResult is update_agent's result: the values after, the
// changed paths, who the write ran as.
type agentManagerUpdateResult struct {
	RequestedBy string         `json:"requestedBy"`
	Changed     []string       `json:"changed"`
	After       map[string]any `json:"after"`
}

func agentManagerUpdate(s *musterSession, args map[string]any) (*agentManagerUpdateResult, error) {
	var updated agentManagerUpdateResult
	if err := s.callServerJSON(agentManagerToolPrefix+"update_agent", args, &updated); err != nil {
		return nil, err
	}
	return &updated, nil
}

// agentManagerStatus is get_agent_status as the proofs read it.
type agentManagerStatus struct {
	Verdict  string `json:"verdict"`
	Summary  string `json:"summary"`
	Template struct {
		Harnesses []struct {
			Harness string `json:"harness"`
			Ready   *bool  `json:"ready"`
		} `json:"harnesses"`
	} `json:"template"`
}

// waitAgentVerdict polls get_agent_status until the verdict is the wanted
// one, bounded; the deadline reports the last verdict and summary.
func waitAgentVerdict(s *musterSession, name, want string) (*agentManagerStatus, error) {
	var status agentManagerStatus
	reached := waitFor(20, 3*time.Second, func() bool {
		status = agentManagerStatus{}
		if err := s.callServerJSON(agentManagerToolPrefix+"get_agent_status", map[string]any{nameKey: name}, &status); err != nil {
			return false
		}
		return status.Verdict == want
	})
	if !reached {
		return nil, fmt.Errorf("get_agent_status for %s never said %s (last: %s — %s)", name, want, status.Verdict, status.Summary)
	}
	return &status, nil
}

// serviceAccountRules is `kubectl auth can-i --list --as=<principal> -n <ns>`:
// the rules the apiserver grants the impersonated principal in the namespace,
// one row per resource the way the CLI prints them.
func serviceAccountRules(principal, ns string) ([]string, error) {
	as, err := asUserConfig(principal)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	status, err := canIList(ctx, as, ns)
	if err != nil {
		return nil, err
	}
	return ruleRows(status), nil
}

// rulesBeyond drops from rows every row of baseline — what a ServiceAccount no
// binding names may do on this cluster — and the CLI's header row, and returns
// the rest: permissions of the principal's own.
func rulesBeyond(rows, baseline []string) []string {
	var granted []string
	for _, row := range rows {
		fields := strings.Fields(row)
		if len(fields) == 0 || fields[0] == "Resources" || slices.Contains(baseline, row) {
			continue
		}
		granted = append(granted, row)
	}
	return granted
}

// bootstrapExtras names the resource rows of the baseline beyond the
// self-subject reviews — what the cluster's bootstrap policy hands every
// principal (the ClusterTrustBundle discovery on a 1.36 cluster with the gate
// on) — for the note.
func bootstrapExtras(baseline []string) string {
	var extras []string
	for _, row := range baseline {
		fields := strings.Fields(row)
		if len(fields) > 0 && !strings.HasPrefix(fields[0], "selfsubject") && !strings.HasPrefix(fields[0], "[") {
			extras = append(extras, fields[0])
		}
	}
	if len(extras) == 0 {
		return ""
	}
	return ", " + strings.Join(extras, ", ")
}

// waitAgentUnlisted polls list_agents until name is no longer listed.
func waitAgentUnlisted(session *musterSession, name string) error {
	gone := waitFor(30, 3*time.Second, func() bool {
		var list struct {
			Agents []struct {
				Name string `json:"name"`
			} `json:"agents"`
		}
		if err := session.callServerJSON(agentManagerToolPrefix+"list_agents", nil, &list); err != nil {
			return false
		}
		for _, a := range list.Agents {
			if a.Name == name {
				return false
			}
		}
		return true
	})
	if !gone {
		return fmt.Errorf("%s is still listed after the delete", name)
	}
	return nil
}

// callServerJSON runs an aggregated server tool and decodes its JSON payload.
func (s *musterSession) callServerJSON(name string, args map[string]any, into any) error {
	text, err := s.callServerTool(name, args)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(text), into); err != nil {
		return fmt.Errorf("%s: payload is not the expected JSON: %w\n%.300s", name, err, text)
	}
	return nil
}
