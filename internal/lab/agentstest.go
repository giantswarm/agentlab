package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// agentsTestAgent is the throwaway agent the proof creates, updates and
// deletes through agent-manager.
const agentsTestAgent = "agentlab-agents-test"

// agentsTestToolset is the toolset the proof declares: the read-only preset,
// the smallest one that still gives the agent tools.
const agentsTestToolset = "preset:read-only"

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
// user, through the platform path only (Dex id_token -> muster -> call_tool
// x_agent-manager_*): get_info reports identity caller; the admin's create ->
// ready -> update -> delete round trip succeeds with requestedBy set, the
// AgentTemplate it writes carrying the user's field manager, the Go ADK
// Harness's label and its toolset on the per-agent muster carrier; a
// viewers-group user's create is refused by the kind apiserver as
// User "oidc:viewer@lab.local" (the view role writes no AgentTemplates); and
// the agent-manager ServiceAccount holds nothing beyond API discovery. Leaves
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
	toolPrefix := "x_" + agentManagerMCPServer + "_"

	step("Logging in to Dex as %s", email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	note("got an id_token")

	step("MCP tools through muster (%s*)", toolPrefix)
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
		if strings.HasPrefix(t, toolPrefix) {
			amTools = append(amTools, strings.TrimPrefix(t, toolPrefix))
		}
	}
	if len(amTools) == 0 {
		return fmt.Errorf("muster aggregates no %s tools; check `kubectl -n %s get mcpservers.muster.giantswarm.io %s` and `agentlab logs muster`", toolPrefix, platformNamespace, agentManagerMCPServer)
	}
	slices.Sort(amTools)
	note("%d tools: %s", len(amTools), strings.Join(amTools, ", "))

	step("%sget_info — expecting identity caller (every Kubernetes call as the user)", toolPrefix)
	var info struct {
		Version      string            `json:"version"`
		Identity     string            `json:"identity"`
		Capabilities map[string]bool   `json:"capabilities"`
		APIVersions  map[string]string `json:"apiVersions"`
		Namespaces   struct {
			Default string `json:"default"`
		} `json:"namespaces"`
	}
	if err := session.callServerJSON(toolPrefix+"get_info", nil, &info); err != nil {
		return err
	}
	if info.Identity != "caller" || !info.Capabilities["writesAsCaller"] {
		return fmt.Errorf("agent-manager reports identity=%q writesAsCaller=%v: it is not running with downstream OAuth (umbrella agent-manager.oauth.downstream)", info.Identity, info.Capabilities["writesAsCaller"])
	}
	if got := info.APIVersions["agentTemplate"]; got != agentTemplateAPIVersion {
		return fmt.Errorf("agent-manager composes apiVersions.agentTemplate=%q, the platform's agents are %s AgentTemplates (apiVersions: %v) — this agent-manager does not speak kagent main", got, agentTemplateAPIVersion, info.APIVersions)
	}
	note("version %s, identity %s, default namespace %s, apiVersions %v", info.Version, info.Identity, info.Namespaces.Default, info.APIVersions)

	step("%slist_model_configs", toolPrefix)
	var configs struct {
		ModelConfigs []struct {
			Name string `json:"name"`
		} `json:"modelConfigs"`
	}
	if err := session.callServerJSON(toolPrefix+"list_model_configs", nil, &configs); err != nil {
		return err
	}
	if len(configs.ModelConfigs) == 0 {
		return fmt.Errorf("no ModelConfig in %s — the kagent chart's default one is missing", kagentNamespace)
	}
	modelConfig := configs.ModelConfigs[0].Name
	for _, mc := range configs.ModelConfigs {
		if mc.Name == "default-model-config" {
			modelConfig = mc.Name
		}
	}
	note("using ModelConfig %s", modelConfig)

	// A leftover from an aborted run would make the create a conflict.
	if _, err := session.callServerTool(toolPrefix+"get_agent", map[string]any{nameKey: agentsTestAgent}); err == nil {
		note("removing the leftover agent %s from an earlier run", agentsTestAgent)
		if _, err := session.callServerTool(toolPrefix+"delete_agent", map[string]any{nameKey: agentsTestAgent, "force": true}); err != nil {
			return err
		}
		if err := waitAgentGone(session, toolPrefix, agentsTestAgent); err != nil {
			return err
		}
	}

	// Every agent declares a toolset (agent-manager ≥ 0.4.0 refuses a create
	// without one, naming the shipped presets); agents-test is the platform
	// path, so it declares the read-only preset and asserts the refusal.
	createArgs := map[string]any{
		nameKey: agentsTestAgent, modelConfigKey: modelConfig, "displayName": "agentlab agents-test",
		descriptionKey:  "Throwaway agent of `agentlab agents-test`; deleted by the same run.",
		"systemMessage": "Reply with exactly the word pong and nothing else.",
	}
	step("%screate_agent %s without a toolset — expecting the refusal naming the presets", toolPrefix, agentsTestAgent)
	if text, err := session.callServerTool(toolPrefix+"create_agent", createArgs); err == nil {
		return fmt.Errorf("create_agent without a toolset was accepted (%.200s); agent-manager ≥ 0.4.0 requires one", text)
	} else if !strings.Contains(err.Error(), "toolset") || !strings.Contains(err.Error(), "preset:none") {
		return fmt.Errorf("the refusal does not name the toolset contract: %w", err)
	} else {
		note("refused: %s", excerpt(err.Error(), 200))
	}

	step("%screate_agent %s as %s with toolset [%s]", toolPrefix, agentsTestAgent, user.Email, agentsTestToolset)
	createArgs["toolset"] = []string{agentsTestToolset}
	var created struct {
		RequestedBy string          `json:"requestedBy"`
		Created     map[string]bool `json:"created"`
	}
	if err := session.callServerJSON(toolPrefix+"create_agent", createArgs, &created); err != nil {
		return err
	}
	if !created.Created[createdAgentTemplateKey] || !created.Created[createdToolsetCarrierKey] {
		return fmt.Errorf("create_agent reported created=%v, wanted the AgentTemplate and its toolset carrier written", created.Created)
	}
	if created.RequestedBy != user.Email {
		return fmt.Errorf("create_agent carries requestedBy=%q, wanted %q: agent-manager did not learn the caller from the forwarded token", created.RequestedBy, user.Email)
	}
	note("AgentTemplate written (created: %v), requestedBy=%s", created.Created, created.RequestedBy)

	step("The AgentTemplate belongs to the user, not the ServiceAccount (managedFields), and is admitted by Harness %s", kagentHarness)
	managers, err := agentTemplateManagers(agentsTestAgent)
	if err != nil {
		return err
	}
	if !slices.Contains(managers, agentManagerMCPServer) {
		return fmt.Errorf("AgentTemplate %s has no %s field manager: %q", agentsTestAgent, agentManagerMCPServer, managers)
	}
	template, err := readAgentTemplate(agentsTestAgent)
	if err != nil {
		return err
	}
	if got := template.Metadata.Labels[harnessLabel]; got != kagentHarness {
		return fmt.Errorf("AgentTemplate %s carries %s=%q, wanted %q (no Harness admits it otherwise)", agentsTestAgent, harnessLabel, got, kagentHarness)
	}
	if template.Spec.ModelConfig == nil || template.Spec.ModelConfig.Name != modelConfig {
		return fmt.Errorf("AgentTemplate %s names ModelConfig %v, wanted %s", agentsTestAgent, template.Spec.ModelConfig, modelConfig)
	}
	logCtx, cancelLogs := context.WithTimeout(context.Background(), 60*time.Second)
	events, _ := podLogs(logCtx, platformNamespace, "deploy/"+agentManagerMCPServer, agentManagerMCPServer, 5*time.Minute)
	cancelLogs()
	if !strings.Contains(events, "caller="+user.Email) {
		return fmt.Errorf("agent-manager's log carries no `caller=%s` line for the create", user.Email)
	}
	note("field managers %q, %s=%s, ModelConfig %s; agent-manager logged the write with caller=%s", managers, harnessLabel, kagentHarness, modelConfig, user.Email)

	step("The toolset rides on the per-agent muster carrier %s (headersFrom %s), bound by the template", toolsetCarrierName(agentsTestAgent), toolsetHeader)
	if bound := template.mcpServer(); bound != toolsetCarrierName(agentsTestAgent) {
		return fmt.Errorf("AgentTemplate %s binds RemoteMCPServer %q, wanted its carrier %s", agentsTestAgent, bound, toolsetCarrierName(agentsTestAgent))
	}
	header, err := toolsetHeaderOf(template)
	if err != nil {
		return fmt.Errorf("agent %s: %w", agentsTestAgent, err)
	}
	if header != agentsTestToolset {
		return fmt.Errorf("carrier %s sends %s=%q, wanted %q", toolsetCarrierName(agentsTestAgent), toolsetHeader, header, agentsTestToolset)
	}
	note("%s: %s=%s", toolsetCarrierName(agentsTestAgent), toolsetHeader, header)

	step("Waiting for %s to be Ready on Harness %s (the golden snapshot), and for get_agent_status to agree", agentsTestAgent, kagentHarness)
	template, err = waitAgentTemplateReady(agentsTestAgent, kagentHarness, 120*time.Second)
	if err != nil {
		return err
	}
	for _, h := range template.Status.Harnesses {
		if h.Harness == kagentHarness {
			note("Ready on %s: revision %.12s (warnings %v)", h.Harness, h.LatestSuccessfulRevision, h.Warnings)
		}
	}
	var status struct {
		Verdict string `json:"verdict"`
		Summary string `json:"summary"`
	}
	ready := waitFor(20, 3*time.Second, func() bool {
		status.Verdict, status.Summary = "", ""
		if err := session.callServerJSON(toolPrefix+"get_agent_status", map[string]any{nameKey: agentsTestAgent}, &status); err != nil {
			return false
		}
		return status.Verdict == "ready"
	})
	if !ready {
		return fmt.Errorf("%s is Ready on the cluster but get_agent_status says %s — %s (agent-manager does not read status.harnesses[]?)", agentsTestAgent, status.Verdict, status.Summary)
	}
	note("get_agent_status: %s", excerpt(status.Summary, 120))
	var got struct {
		Toolset            []string `json:"toolset"`
		ImplicitFullAccess bool     `json:"implicitFullAccess"`
		Harness            string   `json:"harness"`
		ToolsetCarrier     struct {
			Name   string `json:"name"`
			Exists bool   `json:"exists"`
			Header string `json:"header"`
		} `json:"toolsetCarrier"`
	}
	if err := session.callServerJSON(toolPrefix+"get_agent", map[string]any{nameKey: agentsTestAgent}, &got); err != nil {
		return err
	}
	if len(got.Toolset) != 1 || got.Toolset[0] != agentsTestToolset || got.ImplicitFullAccess {
		return fmt.Errorf("get_agent reports toolset=%v implicitFullAccess=%v, wanted [%s]/false", got.Toolset, got.ImplicitFullAccess, agentsTestToolset)
	}
	if got.Harness != kagentHarness || got.ToolsetCarrier.Name != toolsetCarrierName(agentsTestAgent) || !got.ToolsetCarrier.Exists || got.ToolsetCarrier.Header != agentsTestToolset {
		return fmt.Errorf("get_agent reports harness=%q toolsetCarrier=%+v, wanted %s and %s carrying %s", got.Harness, got.ToolsetCarrier, kagentHarness, toolsetCarrierName(agentsTestAgent), agentsTestToolset)
	}
	note("get_agent reports toolset %v on Harness %s, carrier %s (%s=%s)", got.Toolset, got.Harness, got.ToolsetCarrier.Name, toolsetHeader, got.ToolsetCarrier.Header)

	step("%supdate_agent as %s", toolPrefix, user.Email)
	var updated struct {
		RequestedBy string   `json:"requestedBy"`
		Changed     []string `json:"changed"`
	}
	if err := session.callServerJSON(toolPrefix+"update_agent", map[string]any{nameKey: agentsTestAgent, descriptionKey: "Updated by agentlab agents-test."}, &updated); err != nil {
		return err
	}
	if updated.RequestedBy != user.Email || !slices.Contains(updated.Changed, descriptionKey) {
		return fmt.Errorf("update_agent: requestedBy=%q changed=%v", updated.RequestedBy, updated.Changed)
	}
	note("changed %v, requestedBy=%s", updated.Changed, updated.RequestedBy)

	// The user's identity, not a ServiceAccount: a viewer (the view
	// ClusterRole, no AgentTemplate writes anywhere) is refused by the kind
	// apiserver under the user's own name. A shared ServiceAccount would let
	// both users through alike.
	viewer := cfg.FindUserInGroup("viewers")
	if viewer == nil {
		note("skipping the viewer proof: %s has no viewers-group user", config.File)
	} else {
		step("%screate_agent as %s — expecting the apiserver's Forbidden for User \"oidc:%s\"", toolPrefix, viewer.Email, viewer.Email)
		viewerToken, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
			viewer.Email, viewer.Password, musterLoginScopes)
		if err != nil {
			return err
		}
		viewerSession, err := openMusterSession(cfg, viewerToken, "agents-test-viewer")
		if err != nil {
			return err
		}
		text, err := viewerSession.callServerTool(toolPrefix+"create_agent", map[string]any{nameKey: agentsTestAgent + "-viewer", modelConfigKey: modelConfig, "toolset": []string{agentsTestToolset}})
		switch {
		case err == nil:
			return fmt.Errorf("%s created an agent through agent-manager although the view role cannot write AgentTemplates — agent-manager is not acting as the caller (ServiceAccount fallback?): %.200s", viewer.Email, text)
		case !strings.Contains(strings.ToLower(err.Error()), "forbidden"):
			return fmt.Errorf("%s: wanted the apiserver's Forbidden, got: %w", viewer.Email, err)
		case !strings.Contains(err.Error(), `User "oidc:`+viewer.Email+`"`):
			return fmt.Errorf("%s: Forbidden, but not under the user's own name (the apiserver saw someone else): %w", viewer.Email, err)
		}
		note("%s: %s", viewer.Email, excerpt(err.Error(), 200))
		if agentTemplateExists(agentsTestAgent + "-viewer") {
			return fmt.Errorf("AgentTemplate %s-viewer exists although the create was refused", agentsTestAgent)
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

	step("%sdelete_agent %s as %s — the template and its carrier go", toolPrefix, agentsTestAgent, user.Email)
	var deleted struct {
		RequestedBy          string `json:"requestedBy"`
		AgentTemplateDeleted bool   `json:"agentTemplateDeleted"`
		ToolsetCarrierDelete bool   `json:"toolsetCarrierDeleted"`
		ToolsetCarrierKept   string `json:"toolsetCarrierKept"`
	}
	if err := session.callServerJSON(toolPrefix+"delete_agent", map[string]any{nameKey: agentsTestAgent}, &deleted); err != nil {
		return err
	}
	if deleted.RequestedBy != user.Email || !deleted.AgentTemplateDeleted || !deleted.ToolsetCarrierDelete {
		return fmt.Errorf("delete_agent: requestedBy=%q agentTemplateDeleted=%v toolsetCarrierDeleted=%v (kept: %q), wanted %s and both deleted", deleted.RequestedBy, deleted.AgentTemplateDeleted, deleted.ToolsetCarrierDelete, deleted.ToolsetCarrierKept, user.Email)
	}
	note("AgentTemplate and carrier deleted, requestedBy=%s", deleted.RequestedBy)
	if err := waitAgentTemplateGone(agentsTestAgent); err != nil {
		return err
	}
	if err := waitAgentGone(session, toolPrefix, agentsTestAgent); err != nil {
		return err
	}
	note("%s and %s are gone; list_agents agrees", agentsTestAgent, toolsetCarrierName(agentsTestAgent))

	fmt.Println()
	fmt.Printf("PASS: muster aggregates %s* and agent-manager reports identity caller\n", toolPrefix)
	fmt.Printf("PASS: create_agent without a toolset is refused naming the presets; with [%s] %s created -> Ready on Harness %s -> updated -> deleted %s through call_tool, every write requestedBy=%s and logged with caller=\n", agentsTestToolset, user.Email, kagentHarness, agentsTestAgent, user.Email)
	fmt.Printf("PASS: the AgentTemplate carries the %s field manager and %s=%s; its toolset rides on RemoteMCPServer %s as %s=%s; delete removes both\n", agentManagerMCPServer, harnessLabel, kagentHarness, toolsetCarrierName(agentsTestAgent), toolsetHeader, agentsTestToolset)
	if viewer != nil {
		fmt.Printf("PASS: %s's create is Forbidden by the apiserver as User \"oidc:%s\" (user RBAC, not the ServiceAccount's)\n", viewer.Email, viewer.Email)
	}
	fmt.Printf("PASS: %s holds no permissions beyond discovery\n", agentManagerServiceAccount)
	return nil
}

// createdAgentTemplateKey and createdToolsetCarrierKey are the flags in
// create_agent's `created` report: the AgentTemplate and the per-agent muster
// carrier were written (the HelmRelease flag of the 0.x line).
const (
	createdAgentTemplateKey  = "agentTemplate"
	createdToolsetCarrierKey = "toolsetCarrier"
)

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

// waitAgentGone polls list_agents until name is no longer listed.
func waitAgentGone(session *musterSession, toolPrefix, name string) error {
	gone := waitFor(30, 3*time.Second, func() bool {
		var list struct {
			Agents []struct {
				Name string `json:"name"`
			} `json:"agents"`
		}
		if err := session.callServerJSON(toolPrefix+"list_agents", nil, &list); err != nil {
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
