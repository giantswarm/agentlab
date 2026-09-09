package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The toolset proof.
//
// A toolset is the selector list an agent declares — chart value `toolset`,
// header `X-Muster-Toolset`, agent-manager argument `toolset` — that bounds
// which of the gateway's tools its meta-tools can see and call. muster
// evaluates it per request, statelessly, on the caller's own catalogue, so
// the invoking human's identity and the backends' authorization stay the
// boundary; the toolset is composition. This proof drives the released
// components in the lab through every seam the plan names: agent-manager's
// contract, the manifests it renders, muster's resolution and refusal under
// the header the agent's runtime sends, the runtime actually sending it, the
// per-server sign-in the toolset resolution depends on (the one uncertain
// claim of the plan), and the portal's Tools step and composer path.

// Names of what the proof creates; every one of them is deleted by the same
// run (and a leftover of an aborted run is removed first).
const (
	toolsetsTestPrefix    = "agentlab-toolset"
	toolsetsAgentReadOnly = toolsetsTestPrefix + "-ro"
	toolsetsAgentNone     = toolsetsTestPrefix + "-none"
	toolsetsAgentFull     = toolsetsTestPrefix + "-full"
	toolsetsAgentOAuth    = toolsetsTestPrefix + "-oauth"
	toolsetsAgentLegacy   = toolsetsTestPrefix + "-legacy"
	// The tenant identity every agent HelmRelease runs as (the connectivity
	// chart renders the ServiceAccount + RoleBinding from
	// kagent.fluxServiceAccountName; agent-manager and the portal's template
	// name it). The chart's engine is multitenant: a HelmRelease that names
	// no ServiceAccount runs as the namespace's `default`, which holds no
	// RBAC, and never renders.
	kagentFluxServiceAccount  = "kagent-flux"
	toolsetsAgentPortal       = toolsetsTestPrefix + "-portal"
	toolsetsWorkflowQuery     = toolsetsTestPrefix + "-query"
	toolsetsWorkflowMutating  = toolsetsTestPrefix + "-mutating"
	toolsetHeader             = "X-Muster-Toolset"
	presetReadOnly            = "preset:read-only"
	presetNone                = "preset:none"
	presetFull                = "preset:full"
	presetInfrastructure      = "preset:infrastructure"
	presetAgentPlatform       = "preset:agent-platform"
	toolsetFixtureSelector    = "server:" + oauthFixtureServer
	toolsetTestDefaultModel   = "default-model-config"
	toolsetTestAgentSystemMsg = "You are a test agent of the agentlab toolset proof. Do exactly what the message asks, tersely."
)

// kagentAgentResource is kagent's Agent as a resource argument, fully
// qualified so a same-named kind in another group can never be meant (the
// Flux HelmRelease every agent is rendered from is fluxHelmReleaseResource).
const kagentAgentResource = "agents.kagent.dev"

// ToolsetsTestOptions tunes the proof.
type ToolsetsTestOptions struct {
	// ModelConfig is the kagent ModelConfig the throwaway agents run on
	// (default: default-model-config, the Anthropic one the lab renders).
	ModelConfig string
	// SkipChat skips the turns that need the model to answer (the runtime
	// path and the chat-only agent); the manifests, muster's resolution and
	// the sign-in claim are proven regardless.
	SkipChat bool
}

// toolsetsAgent is one throwaway agent the proof creates through agent-manager.
type toolsetsAgent struct {
	name    string
	toolset []string
}

var toolsetsAgents = []toolsetsAgent{
	{toolsetsAgentReadOnly, []string{presetReadOnly}},
	{toolsetsAgentNone, []string{presetNone}},
	{toolsetsAgentFull, []string{presetFull}},
	{toolsetsAgentOAuth, []string{toolsetFixtureSelector}},
}

// ToolsetsTest is the headless end-to-end proof of declared toolsets against
// the released components in the lab. See the file comment for what it
// covers; it prints one PASS line per claim and leaves nothing behind.
func ToolsetsTest(cfg *config.Config, email string, opts ToolsetsTestOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled || !cfg.Platform.Agents {
		return fmt.Errorf("platform.agents is off in %s — enable it and run `agentlab platform` first", config.File)
	}
	if !cfg.Backstage.Enabled {
		return fmt.Errorf("backstage.enabled is off in %s — the portal half of the proof needs it", config.File)
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}
	// The second person of the sign-in proof: anyone but the first, a
	// developer preferred (the lab admin is a developer too).
	var other *config.User
	for i := range cfg.Users {
		u := &cfg.Users[i]
		if u.Email == user.Email {
			continue
		}
		if other == nil || (slices.Contains(u.Groups, "developers") && !slices.Contains(other.Groups, "developers")) {
			other = u
		}
	}
	if other == nil {
		return fmt.Errorf("%s needs a second user for the per-user sign-in proof", config.File)
	}
	if opts.ModelConfig == "" {
		opts.ModelConfig = toolsetTestDefaultModel
	}
	toolPrefix := "x_" + agentManagerMCPServer + "_"
	var verdicts []string
	pass := func(format string, a ...any) { verdicts = append(verdicts, fmt.Sprintf("PASS: "+format, a...)) }

	step("Logging in to Dex as %s (with the audience muster's own writes need)", user.Email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterWriterScopes)
	if err != nil {
		return err
	}
	admin, err := openMusterSession(cfg, token, "toolsets-test")
	if err != nil {
		return err
	}
	if err := admin.callServerJSON(toolPrefix+"get_info", nil, &struct{}{}); err != nil {
		return fmt.Errorf("agent-manager is not reachable through muster (%w); check `kubectl -n %s get mcpservers.muster.giantswarm.io %s`", err, platformNamespace, agentManagerMCPServer)
	}

	// Leftovers of an aborted run first, and everything this run creates on
	// every exit path.
	cleanup := func() { toolsetsCleanup(admin, toolPrefix) }
	cleanup()
	defer cleanup()

	// 1. agent-manager: the toolset is the contract's required argument.
	if err := proveAgentManagerToolset(admin, toolPrefix, opts.ModelConfig); err != nil {
		return err
	}
	pass("agent-manager refuses create_agent without a toolset (naming the presets and preset:none), refuses toolNames, and accepts a toolset")

	// 2. What the platform rendered: the header on the Agent CR, the value on
	//    the HelmRelease, agent-manager's own report — and the agent that
	//    declares none.
	if err := proveRenderedToolsets(admin, toolPrefix, opts.ModelConfig); err != nil {
		return err
	}
	pass("%s carries headersFrom X-Muster-Toolset=%s on spec.declarative.tools[0] and toolset on its HelmRelease; %s has no muster tool entry; an agent without a toolset reports implicitFullAccess", toolsetsAgentReadOnly, presetReadOnly, toolsetsAgentNone)

	// 3. muster resolves and refuses per request, under the header the
	//    agents' runtimes send, in a session of the same user.
	res, err := proveMusterToolsets(cfg, token)
	if err != nil {
		return err
	}
	pass("%s resolves to exactly the %d tools annotated read-only, whatever their kind — the query-only workflows %s and %s included, workflow_%s excluded, core tools by their own readOnlyHint (reads such as core_config_get in, writes such as core_workflow_delete out); the read-only Kubernetes call succeeds and the destructive call (x_%s_delete_agent) is refused naming the toolset",
		presetReadOnly, res.readOnlyCount, "workflow_"+toolsetsWorkflowQuery, "workflow_lab-cluster-overview", toolsetsWorkflowMutating, agentManagerMCPServer)
	pass("two toolsets on one token — in one session request by request and from two sessions in parallel — each see their own tools; %s sees nothing; %s equals the unscoped catalogue (%d tools) like an agent without a toolset", presetNone, presetFull, res.fullCount)
	if res.presetsVerdict != "" {
		pass("%s", res.presetsVerdict)
	} else {
		note("presets %s/%s are not configured on this muster (the fleet presets arrive with the platform chart that ships them); their resolution was skipped", presetInfrastructure, presetAgentPlatform)
	}

	// 4. The runtime actually sends the header: the read-only agent, asked
	//    what tools it has, lists read-only tools only; the chat-only agent
	//    answers without any tool entry.
	if opts.SkipChat {
		note("skipping the model turns (--skip-chat): the runtime path and the chat-only turn were not exercised")
	} else {
		if err := proveAgentRuntimeToolsets(cfg, token, opts.ModelConfig, toolPrefix, admin, res); err != nil {
			return err
		}
		pass("through kagent (A2A as %s) %s lists nothing outside %s — read-only core tools may appear, no writer does; the runtime sends the header and the user's token — and %s answers a chat turn with no tool entry", user.Email, toolsetsAgentReadOnly, presetReadOnly, toolsetsAgentNone)
	}

	// 5. The OAuth fixture: the sign-in completed in the portal path is what
	//    makes the server's tools resolve for the agent under the same
	//    forwarded token (ground-truth v3, G6), and only for that token.
	g6, err := proveSignInScopedToolset(cfg, user, other, toolPrefix, admin, opts)
	if err != nil {
		return err
	}
	verdicts = append(verdicts, g6...)

	// 6. The portal: the Tools step's endpoints and the composer's apply path.
	portal, err := proveToolsetPortal(cfg, user)
	if err != nil {
		return err
	}
	verdicts = append(verdicts, portal...)

	fmt.Println()
	fmt.Println(strings.Join(verdicts, "\n"))
	return nil
}

// toolsetsCleanup removes everything the proof creates: the agents through
// agent-manager (force: the legacy and portal releases have no agent-manager
// provenance), the HelmReleases directly when agent-manager does not list
// them, the workflows, and the fixture sign-in of the run.
func toolsetsCleanup(s *musterSession, toolPrefix string) {
	names := []string{toolsetsAgentReadOnly, toolsetsAgentNone, toolsetsAgentFull, toolsetsAgentOAuth, toolsetsAgentLegacy, toolsetsAgentPortal}
	removed := false
	for _, name := range names {
		if _, err := s.callServerTool(toolPrefix+"get_agent", map[string]any{nameKey: name}); err != nil {
			// Not known to agent-manager: a HelmRelease may still exist.
			deleteAgentHelmRelease(name)
			continue
		}
		if _, err := s.callServerTool(toolPrefix+"delete_agent", map[string]any{nameKey: name, "force": true}); err != nil {
			note("cleanup: delete_agent %s: %v", name, err)
			deleteAgentHelmRelease(name)
		}
		removed = true
	}
	for _, wf := range []string{toolsetsWorkflowQuery, toolsetsWorkflowMutating} {
		env, err := s.callToolEnvelope("core_workflow_delete", map[string]any{nameKey: wf})
		if err == nil && !env.IsError {
			removed = true
		}
	}
	if removed {
		for _, name := range names {
			_ = waitAgentGone(s, toolPrefix, name)
		}
		note("cleanup: %s* agents and workflows removed", toolsetsTestPrefix)
	}
}

// proveAgentManagerToolset drives agent-manager's contract through muster:
// create_agent without a toolset is refused naming the shipped presets and
// pointing at preset:none; the removed toolNames argument is refused with the
// explaining error; the four agents of the proof are created with theirs.
func proveAgentManagerToolset(s *musterSession, toolPrefix, modelConfig string) error {
	step("%screate_agent without a toolset — expecting the refusal", toolPrefix)
	base := func(name string) map[string]any {
		return map[string]any{
			nameKey: name, modelConfigKey: modelConfig, "displayName": "agentlab toolset proof: " + strings.TrimPrefix(name, toolsetsTestPrefix+"-"),
			descriptionKey:  "Throwaway agent of `agentlab toolsets-test`; deleted by the same run.",
			"systemMessage": toolsetTestAgentSystemMsg,
		}
	}
	text, err := s.callServerTool(toolPrefix+"create_agent", base(toolsetsAgentReadOnly))
	if err == nil {
		return fmt.Errorf("create_agent without a toolset was accepted (%.200s) — agent-manager predates the toolset contract (needs ≥ 0.4.0)", text)
	}
	for _, want := range []string{"toolset", presetReadOnlyName, presetNoneName, "infrastructure", "agent-platform", presetFullName, presetNone} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	note("refused: %s", excerpt(err.Error(), 220))
	if agentHelmReleaseExists(toolsetsAgentReadOnly) {
		return fmt.Errorf("HelmRelease %s exists although the create was refused", toolsetsAgentReadOnly)
	}

	step("%screate_agent with the removed toolNames argument — expecting the explaining refusal", toolPrefix)
	args := base(toolsetsAgentReadOnly)
	args["toolset"] = []string{presetReadOnly}
	args["toolNames"] = []string{"list_tools"}
	if text, err := s.callServerTool(toolPrefix+"create_agent", args); err == nil {
		return fmt.Errorf("create_agent accepted toolNames (%.200s); the inert knob must be refused", text)
	} else if !strings.Contains(err.Error(), "toolNames") || !strings.Contains(err.Error(), "toolset") {
		return fmt.Errorf("the toolNames refusal does not explain itself: %v", err)
	} else {
		note("refused: %s", excerpt(err.Error(), 200))
	}

	for _, a := range toolsetsAgents {
		step("%screate_agent %s with toolset %v", toolPrefix, a.name, a.toolset)
		args := base(a.name)
		args["toolset"] = a.toolset
		var created struct {
			RequestedBy string `json:"requestedBy"`
			Created     struct {
				HelmRelease bool `json:"helmRelease"`
			} `json:"created"`
		}
		if err := s.callServerJSON(toolPrefix+"create_agent", args, &created); err != nil {
			return err
		}
		if !created.Created.HelmRelease {
			return fmt.Errorf("create_agent %s reported no HelmRelease written", a.name)
		}
		note("HelmRelease written, requestedBy=%s", created.RequestedBy)
	}
	return nil
}

// agentCR is the part of a kagent Agent the proof reads.
type agentCR struct {
	Spec struct {
		Declarative struct {
			Tools []struct {
				Type        string `json:"type"`
				HeadersFrom []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"headersFrom"`
				MCPServer *struct {
					Name string `json:"name"`
					Kind string `json:"kind"`
				} `json:"mcpServer"`
			} `json:"tools"`
		} `json:"declarative"`
	} `json:"spec"`
}

// waitAgentCR waits for helm-controller to render the Agent behind a
// HelmRelease and returns it.
func waitAgentCR(name string) (*agentCR, error) {
	var agent *unstructured.Unstructured
	found := waitFor(40, 3*time.Second, func() bool {
		obj, err := readKagentObject(kagentAgentResource, name)
		if err != nil {
			return false
		}
		agent = obj
		return true
	})
	if !found {
		status := ""
		if hr, err := readKagentObject(fluxHelmReleaseResource, name); err == nil {
			status = conditionMessage(hr, "Ready")
		} else {
			status = err.Error()
		}
		return nil, fmt.Errorf("no Agent %s rendered from its HelmRelease within 2 min (Ready: %s)", name, excerpt(status, 200))
	}
	cr, err := agentCRFrom(agent)
	if err != nil {
		return nil, fmt.Errorf("parsing agent %s: %w", name, err)
	}
	return cr, nil
}

// agentCRFrom reads the part of a kagent Agent the proof looks at off the
// object as the apiserver returned it.
func agentCRFrom(obj *unstructured.Unstructured) (*agentCR, error) {
	raw, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, err
	}
	var cr agentCR
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, err
	}
	return &cr, nil
}

// readKagentObject reads one object of the given resource (a kubectl resource
// argument) in the kagent namespace, bounded by kubeReadTimeout.
func readKagentObject(resourceArg, name string) (*unstructured.Unstructured, error) {
	gvr, err := gvrFor(resourceArg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	return getObject(ctx, gvr, kagentNamespace, name)
}

// agentHelmReleaseExists reports whether an agent's HelmRelease is there; a
// read that fails counts as absent, as the CLI probe's non-zero exit did.
func agentHelmReleaseExists(name string) bool {
	_, err := readKagentObject(fluxHelmReleaseResource, name)
	return err == nil
}

// deleteAgentHelmRelease removes an agent's HelmRelease without waiting for
// helm-controller's uninstall (`--ignore-not-found --wait=false`); best
// effort, for the cleanup paths.
func deleteAgentHelmRelease(name string) {
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	_ = deleteObject(ctx, gvr, kagentNamespace, name, 0)
}

// toolsetHeaderOf returns the X-Muster-Toolset value on the agent's muster
// tool entry, "" when the entry carries none, and an error when the Agent has
// no muster tool entry at all.
func toolsetHeaderOf(cr *agentCR) (string, error) {
	for _, t := range cr.Spec.Declarative.Tools {
		if t.MCPServer == nil {
			continue
		}
		for _, h := range t.HeadersFrom {
			if h.Name == toolsetHeader {
				return h.Value, nil
			}
		}
		return "", nil
	}
	return "", fmt.Errorf("no McpServer tool entry")
}

// helmReleaseToolset reads the top-level toolset value of a HelmRelease.
func helmReleaseToolset(name string) ([]string, bool, error) {
	hr, err := readKagentObject(fluxHelmReleaseResource, name)
	if err != nil {
		return nil, false, err
	}
	return toolsetValue(hr)
}

// toolsetValue is `{.spec.values.toolset}` of a HelmRelease: the list and
// whether the value is set at all; anything but a list of strings is refused.
func toolsetValue(hr *unstructured.Unstructured) ([]string, bool, error) {
	value, found, err := unstructured.NestedFieldNoCopy(hr.Object, "spec", "values", "toolset")
	if err != nil || !found || value == nil {
		return nil, false, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, false, fmt.Errorf("HelmRelease %s values.toolset is not a string list: %v", hr.GetName(), value)
	}
	toolset := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, false, fmt.Errorf("HelmRelease %s values.toolset is not a string list: %v", hr.GetName(), value)
		}
		toolset = append(toolset, s)
	}
	return toolset, true, nil
}

// proveRenderedToolsets asserts the manifests behind the created agents and
// agent-manager's report of them, and the shape of an agent that declares no
// toolset (a HelmRelease applied without the value, like every agent that
// predates toolsets).
func proveRenderedToolsets(s *musterSession, toolPrefix, modelConfig string) error {
	for _, a := range toolsetsAgents {
		step("Agent %s: the rendered header and the HelmRelease value", a.name)
		cr, err := waitAgentCR(a.name)
		if err != nil {
			return err
		}
		want := strings.Join(a.toolset, ",")
		header, err := toolsetHeaderOf(cr)
		switch {
		case a.name == toolsetsAgentNone:
			if err == nil {
				return fmt.Errorf("agent %s (toolset %v) still has a muster tool entry (header %q); the chart must omit it", a.name, a.toolset, header)
			}
			if len(cr.Spec.Declarative.Tools) != 0 {
				return fmt.Errorf("agent %s has %d tool entries, wanted none", a.name, len(cr.Spec.Declarative.Tools))
			}
			note("no tool entry at all (spec.declarative.tools empty)")
		case err != nil:
			return fmt.Errorf("agent %s: %w", a.name, err)
		case header != want:
			return fmt.Errorf("agent %s carries %s=%q on spec.declarative.tools[0].headersFrom, wanted %q", a.name, toolsetHeader, header, want)
		default:
			note("spec.declarative.tools[0].headersFrom: %s=%s (mcpServer %s/%s)", toolsetHeader, header, cr.Spec.Declarative.Tools[0].MCPServer.Kind, cr.Spec.Declarative.Tools[0].MCPServer.Name)
		}
		values, set, err := helmReleaseToolset(a.name)
		if err != nil {
			return err
		}
		if !set || !slices.Equal(values, a.toolset) {
			return fmt.Errorf("HelmRelease %s values.toolset = %v (set: %v), wanted %v", a.name, values, set, a.toolset)
		}
		var got struct {
			Toolset            []string `json:"toolset"`
			ImplicitFullAccess bool     `json:"implicitFullAccess"`
		}
		if err := s.callServerJSON(toolPrefix+"get_agent", map[string]any{nameKey: a.name}, &got); err != nil {
			return err
		}
		if !slices.Equal(got.Toolset, a.toolset) || got.ImplicitFullAccess {
			return fmt.Errorf("get_agent %s reports toolset=%v implicitFullAccess=%v, wanted %v/false", a.name, got.Toolset, got.ImplicitFullAccess, a.toolset)
		}
		note("HelmRelease values.toolset %v; get_agent reports the same", values)
	}

	step("An agent without a toolset (HelmRelease applied without the value): no header, implicit full access")
	hr := fmt.Sprintf(`apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/managed-by: agentlab
spec:
  interval: 10m
  serviceAccountName: %s
  chartRef:
    kind: OCIRepository
    name: agent
    namespace: %s
  values:
    agent:
      name: %s
      displayName: "agentlab toolset proof: legacy"
      description: "Throwaway agent of agentlab toolsets-test without a toolset; deleted by the same run."
      systemMessage: %q
    modelConfig:
      name: %s
`, toolsetsAgentLegacy, kagentNamespace, kagentFluxServiceAccount, kagentNamespace, toolsetsAgentLegacy, toolsetTestAgentSystemMsg, modelConfig)
	if _, err := applyManifests(context.Background(), []byte(hr)); err != nil {
		return err
	}
	cr, err := waitAgentCR(toolsetsAgentLegacy)
	if err != nil {
		return err
	}
	header, err := toolsetHeaderOf(cr)
	if err != nil {
		return fmt.Errorf("agent %s: %w", toolsetsAgentLegacy, err)
	}
	if header != "" {
		return fmt.Errorf("agent %s declares no toolset but carries %s=%q", toolsetsAgentLegacy, toolsetHeader, header)
	}
	var list struct {
		Agents []struct {
			Name               string   `json:"name"`
			Toolset            []string `json:"toolset"`
			ImplicitFullAccess bool     `json:"implicitFullAccess"`
		} `json:"agents"`
	}
	if err := s.callServerJSON(toolPrefix+"list_agents", nil, &list); err != nil {
		return err
	}
	seen := false
	for _, a := range list.Agents {
		if a.Name != toolsetsAgentLegacy {
			continue
		}
		seen = true
		if !a.ImplicitFullAccess || len(a.Toolset) != 0 {
			return fmt.Errorf("list_agents reports %s with toolset=%v implicitFullAccess=%v, wanted none/true", a.Name, a.Toolset, a.ImplicitFullAccess)
		}
	}
	if !seen {
		return fmt.Errorf("list_agents does not list %s", toolsetsAgentLegacy)
	}
	note("muster tool entry without a header; list_agents: implicitFullAccess=true")
	return nil
}

// musterToolsetResults is what proveMusterToolsets learned, for the verdicts.
type musterToolsetResults struct {
	readOnlyCount int
	// readOnlyNames is what preset:read-only resolved to for the user,
	// sorted; the runtime path checks the agent's report against it.
	readOnlyNames []string
	// fullNames is the unscoped catalogue of the same user, sorted.
	fullNames      []string
	fullCount      int
	presetsVerdict string
}

// proveMusterToolsets is the request-level proof: sessions of the user
// carrying the header an agent's runtime would send.
func proveMusterToolsets(cfg *config.Config, token string) (*musterToolsetResults, error) {
	out := &musterToolsetResults{}
	k8sPrefix := "x_" + cfg.MCPServerName() + "_"
	// The lab's mcp-kubernetes runs non-destructive, so its writers do not
	// exist as tools; the destructive call of the proof is agent-manager's
	// delete_agent, a platform writer annotated as such. Under the read-only
	// toolset muster refuses it before agent-manager ever sees it.
	listTool, deleteTool := k8sPrefix+"list", "x_"+agentManagerMCPServer+"_delete_agent"
	deleteArgs := map[string]any{nameKey: toolsetsTestPrefix + "-nothing"}
	queryTool, mutatingTool := "workflow_"+toolsetsWorkflowQuery, "workflow_"+toolsetsWorkflowMutating
	demoTool := "workflow_lab-cluster-overview"

	s, err := openMusterSession(cfg, token, "toolsets-test-muster")
	if err != nil {
		return nil, err
	}

	step("Workflows: a query-only one and one with a destructive step (core_workflow_create)")
	for _, wf := range []map[string]any{
		{
			nameKey:        toolsetsWorkflowQuery,
			descriptionKey: "agentlab toolsets-test: read-only steps only (deleted by the same run)",
			"steps": []map[string]any{
				{"id": resourceNamespaces, toolKey: listTool, argsKey: map[string]any{resourceTypeKey: resourceNamespaces}, "store": true},
			},
		},
		{
			nameKey:        toolsetsWorkflowMutating,
			descriptionKey: "agentlab toolsets-test: a destructive step, never executed (deleted by the same run)",
			"steps": []map[string]any{
				{"id": resourceNamespaces, toolKey: listTool, argsKey: map[string]any{resourceTypeKey: resourceNamespaces}},
				{"id": "purge", toolKey: deleteTool, argsKey: deleteArgs},
			},
		},
	} {
		env, err := s.callToolEnvelope("core_workflow_create", wf)
		if err != nil {
			return nil, err
		}
		if env.IsError {
			return nil, fmt.Errorf("core_workflow_create %s: %s", wf[nameKey], excerpt(env.Content[0].Text, 300))
		}
	}
	for _, tc := range []struct {
		tool     string
		readOnly bool
	}{{queryTool, true}, {mutatingTool, false}, {demoTool, true}} {
		var desc *describeToolResponse
		ready := waitFor(10, 2*time.Second, func() bool {
			d, err := s.describeTool(tc.tool)
			if err != nil {
				return false
			}
			desc = d
			return true
		})
		if !ready {
			return nil, fmt.Errorf("describe_tool %s never answered after the workflow was created", tc.tool)
		}
		if desc.Kind != "workflow" || desc.Annotations.readOnly() != tc.readOnly {
			return nil, fmt.Errorf("describe_tool %s: kind=%q readOnlyHint=%v, wanted workflow/%v (the derived hint)", tc.tool, desc.Kind, desc.Annotations.readOnly(), tc.readOnly)
		}
		note("%s: kind %s, derived readOnlyHint=%v", tc.tool, desc.Kind, tc.readOnly)
	}

	step("Unscoped catalogue (no header) and %s", presetFull)
	unscoped, err := s.filterTools(nil)
	if err != nil {
		return nil, err
	}
	s.setHeader(toolsetHeader, presetFull)
	full, err := s.filterTools(nil)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(unscoped.names(), full.names()) {
		return nil, fmt.Errorf("%s (%d tools) differs from the unscoped catalogue (%d tools)", presetFull, len(full.Tools), len(unscoped.Tools))
	}
	if !slices.Contains(unscoped.names(), deleteTool) || !slices.Contains(unscoped.names(), mutatingTool) {
		return nil, fmt.Errorf("the unscoped catalogue lacks %s or %s — agent-manager or the workflows are not aggregated", deleteTool, mutatingTool)
	}
	out.fullCount = len(full.Tools)
	out.fullNames = full.names()
	note("%d tools either way", out.fullCount)

	step("%s: exactly the tools annotated read-only, whatever their kind — the query-only workflows and muster's read-only core tools included, the mutating workflow and the core writes excluded", presetReadOnly)
	s.setHeader(toolsetHeader, presetReadOnly)
	ro, err := s.filterTools(nil)
	if err != nil {
		return nil, err
	}
	if len(ro.Tools) == 0 || len(ro.Tools) >= out.fullCount {
		return nil, fmt.Errorf("%s resolved to %d of %d tools", presetReadOnly, len(ro.Tools), out.fullCount)
	}
	// The preset is the readOnlyHint predicate over the caller's catalogue,
	// kind-agnostic: muster's core tools are in it exactly when they declare
	// the annotation — every read since muster 5.13.0 (giantswarm/muster#1172),
	// never a write; a muster whose core tools carry no annotations puts none
	// in the preset.
	if missing, extra := readOnlySetMismatch(unscoped.Tools, ro.Tools); len(missing)+len(extra) > 0 {
		return nil, fmt.Errorf("%s is not the read-only annotated catalogue: missing %v, extra %v", presetReadOnly, missing, extra)
	}
	names := ro.names()
	for _, want := range []string{listTool, queryTool, demoTool} {
		if !slices.Contains(names, want) {
			return nil, fmt.Errorf("%s lacks %s", presetReadOnly, want)
		}
	}
	for _, unwanted := range []string{deleteTool, mutatingTool} {
		if slices.Contains(names, unwanted) {
			return nil, fmt.Errorf("%s includes %s", presetReadOnly, unwanted)
		}
	}
	// Named spot checks on top of the set equality: the core reads are in
	// whenever the core tools carry annotations at all, the core writes are
	// out on every muster.
	coreCount := countKind(ro.Tools, "core")
	if coreCount > 0 {
		for _, want := range []string{"core_config_get", "core_workflow_list", "core_mcpserver_list", "core_auth_login"} {
			if !slices.Contains(names, want) {
				return nil, fmt.Errorf("%s carries %d core tools but lacks the read-only %s", presetReadOnly, coreCount, want)
			}
		}
	}
	for _, unwanted := range []string{"core_workflow_create", "core_workflow_delete", "core_mcpserver_delete", "core_service_stop", "core_config_save", "core_auth_logout"} {
		if slices.Contains(names, unwanted) {
			return nil, fmt.Errorf("%s includes the core write %s", presetReadOnly, unwanted)
		}
	}
	out.readOnlyCount = len(ro.Tools)
	out.readOnlyNames = names
	note("%d tools, exactly the readOnlyHint ones of the catalogue (%d workflows, %d read-only core tools, no core write)", len(ro.Tools), countKind(ro.Tools, "workflow"), coreCount)

	step("Under %s: the read-only Kubernetes call succeeds, the destructive call and the mutating workflow are refused naming the toolset", presetReadOnly)
	text, err := s.callServerTool(listTool, map[string]any{resourceTypeKey: resourceNamespaces})
	if err != nil {
		return nil, fmt.Errorf("%s under %s: %w", listTool, presetReadOnly, err)
	}
	if !strings.Contains(text, platformNamespace) {
		return nil, fmt.Errorf("%s answered without the %s namespace: %.200s", listTool, platformNamespace, text)
	}
	note("%s listed the namespaces", listTool)
	for _, refused := range []struct {
		tool string
		args map[string]any
	}{
		{deleteTool, deleteArgs},
		{mutatingTool, map[string]any{}},
	} {
		env, err := s.callToolEnvelope(refused.tool, refused.args)
		if err != nil {
			return nil, err
		}
		want := fmt.Sprintf("tool %q is outside the toolset [%s]", refused.tool, presetReadOnly)
		if !env.IsError || !strings.Contains(env.Content[0].Text, want) {
			return nil, fmt.Errorf("%s under %s: wanted the refusal %q, got isError=%v %.200s", refused.tool, presetReadOnly, want, env.IsError, env.Content[0].Text)
		}
		note("%s", excerpt(env.Content[0].Text, 120))
	}
	if _, err := s.describeTool(deleteTool); err == nil || !strings.Contains(err.Error(), "outside the toolset") {
		return nil, fmt.Errorf("describe_tool %s under %s should say outside the toolset, got %v", deleteTool, presetReadOnly, err)
	}

	step("%s: nothing visible, everything refused", presetNone)
	s.setHeader(toolsetHeader, presetNone)
	none, err := s.filterTools(nil)
	if err != nil {
		return nil, err
	}
	if len(none.Tools) != 0 {
		return nil, fmt.Errorf("%s resolved to %d tools", presetNone, len(none.Tools))
	}
	env, err := s.callToolEnvelope(listTool, map[string]any{resourceTypeKey: resourceNamespaces})
	if err != nil {
		return nil, err
	}
	if !env.IsError || !strings.Contains(env.Content[0].Text, "outside the toolset ["+presetNone+"]") {
		return nil, fmt.Errorf("%s under %s was not refused: %.200s", listTool, presetNone, env.Content[0].Text)
	}
	note("0 tools; %s", excerpt(env.Content[0].Text, 100))

	step("Header errors are error results, never a fall-back")
	for _, tc := range []struct{ header, want string }{
		{"preset:agentlab-no-such-preset", `unknown preset "agentlab-no-such-preset"`},
		{"toolset:shared", "reserved"},
		{"label:" + toolGroupLabel + "=" + toolGroupInfrastructure, "presets only"},
	} {
		s.setHeader(toolsetHeader, tc.header)
		if _, err := s.filterTools(nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			return nil, fmt.Errorf("header %q: wanted an error containing %q, got %v", tc.header, tc.want, err)
		}
		note("%s -> %s", tc.header, tc.want)
	}

	step("Two toolsets on one token: request by request in one session, and two sessions in parallel")
	narrow := "tool:" + listTool + ",workflow:" + toolsetsWorkflowQuery
	narrowWant := []string{listTool, queryTool}
	slices.Sort(narrowWant)
	for i := 0; i < 3; i++ {
		s.setHeader(toolsetHeader, presetReadOnly)
		a, err := s.filterTools(nil)
		if err != nil {
			return nil, err
		}
		s.setHeader(toolsetHeader, narrow)
		b, err := s.filterTools(nil)
		if err != nil {
			return nil, err
		}
		s.setHeader(toolsetHeader, "")
		c, err := s.filterTools(nil)
		if err != nil {
			return nil, err
		}
		if len(a.Tools) != out.readOnlyCount || !slices.Equal(b.names(), narrowWant) || len(c.Tools) != out.fullCount {
			return nil, fmt.Errorf("round %d: %s=%d tools (want %d), narrow=%v (want %v), unscoped=%d (want %d) — the header leaked between requests", i, presetReadOnly, len(a.Tools), out.readOnlyCount, b.names(), narrowWant, len(c.Tools), out.fullCount)
		}
	}
	note("three rounds of read-only / narrow / unscoped on one session: each request got its own catalogue")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, agent := range []struct {
		header string
		check  func(*filterToolsResponse) error
	}{
		{presetReadOnly, func(r *filterToolsResponse) error {
			if len(r.Tools) != out.readOnlyCount || slices.Contains(r.names(), deleteTool) {
				return fmt.Errorf("%s saw %d tools (delete: %v)", presetReadOnly, len(r.Tools), slices.Contains(r.names(), deleteTool))
			}
			return nil
		}},
		{narrow, func(r *filterToolsResponse) error {
			if !slices.Equal(r.names(), narrowWant) {
				return fmt.Errorf("narrow toolset saw %v", r.names())
			}
			return nil
		}},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess, err := openMusterSession(cfg, token, "toolsets-test-parallel")
			if err != nil {
				errs <- err
				return
			}
			sess.setHeader(toolsetHeader, agent.header)
			for i := 0; i < 5; i++ {
				r, err := sess.filterTools(nil)
				if err != nil {
					errs <- err
					return
				}
				if err := agent.check(r); err != nil {
					errs <- fmt.Errorf("parallel round %d: %w", i, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return nil, err
	}
	note("two sessions on the same token, five interleaved rounds each: every request saw its own toolset")

	// The shipped presets, when the platform chart configured them.
	s.setHeader(toolsetHeader, "")
	withPresets, err := s.filterTools(map[string]any{"include_presets": true, "limit": 1})
	if err != nil {
		return nil, err
	}
	presetNames := make([]string, 0, len(withPresets.Presets))
	for _, p := range withPresets.Presets {
		presetNames = append(presetNames, p.Name)
	}
	for _, builtIn := range []string{presetReadOnlyName, presetNoneName, presetFullName} {
		if !slices.Contains(presetNames, builtIn) {
			return nil, fmt.Errorf("filter_tools presets lack the built-in %s: %v", builtIn, presetNames)
		}
	}
	note("presets: %s", strings.Join(presetNames, ", "))
	if slices.Contains(presetNames, "infrastructure") && slices.Contains(presetNames, "agent-platform") {
		step("The shipped presets resolve by the tool-group label: %s and %s", presetInfrastructure, presetAgentPlatform)
		s.setHeader(toolsetHeader, presetInfrastructure)
		infra, err := s.filterTools(nil)
		if err != nil {
			return nil, err
		}
		s.setHeader(toolsetHeader, presetAgentPlatform)
		platform, err := s.filterTools(nil)
		if err != nil {
			return nil, err
		}
		s.setHeader(toolsetHeader, "")
		labels, err := mcpServerToolGroups()
		if err != nil {
			return nil, err
		}
		for _, t := range infra.Tools {
			if labels[t.Server] != toolGroupInfrastructure {
				return nil, fmt.Errorf("%s includes %s of server %q (label %q)", presetInfrastructure, t.Name, t.Server, labels[t.Server])
			}
		}
		coreSeen := false
		for _, t := range platform.Tools {
			if t.Kind == "core" {
				coreSeen = true
				continue
			}
			if labels[t.Server] != toolGroupAgentPlatform {
				return nil, fmt.Errorf("%s includes %s of server %q (label %q)", presetAgentPlatform, t.Name, t.Server, labels[t.Server])
			}
		}
		if !coreSeen {
			return nil, fmt.Errorf("%s lacks muster's core tools", presetAgentPlatform)
		}
		if labels[cfg.MCPServerName()] == toolGroupInfrastructure && !slices.Contains(infra.names(), listTool) {
			return nil, fmt.Errorf("%s lacks %s although %s carries the infrastructure label", presetInfrastructure, listTool, cfg.MCPServerName())
		}
		if labels[agentManagerMCPServer] == toolGroupAgentPlatform && !slices.Contains(platform.names(), "x_"+agentManagerMCPServer+"_create_agent") {
			return nil, fmt.Errorf("%s lacks agent-manager's tools although its CR carries the agent-platform label", presetAgentPlatform)
		}
		note("%s: %d tools, all from infrastructure-labelled servers; %s: %d tools, all from agent-platform-labelled servers plus core_*", presetInfrastructure, len(infra.Tools), presetAgentPlatform, len(platform.Tools))
		out.presetsVerdict = fmt.Sprintf("the shipped presets resolve by the tool-group label: %s -> %d tools of infrastructure servers, %s -> %d tools of agent-platform servers plus core tools", presetInfrastructure, len(infra.Tools), presetAgentPlatform, len(platform.Tools))
	}
	return out, nil
}

func countKind(tools []toolInfo, kind string) int {
	n := 0
	for _, t := range tools {
		if t.Kind == kind {
			n++
		}
	}
	return n
}

// readOnlySetMismatch compares what preset:read-only resolved to with the
// tools of the unscoped catalogue that carry readOnlyHint, whatever their
// kind: missing are annotated read-only but absent from the preset, extra are
// in the preset without the annotation. Both empty means the preset is
// exactly the annotation predicate.
func readOnlySetMismatch(catalogue, resolved []toolInfo) (missing, extra []string) {
	want := make(map[string]bool, len(catalogue))
	for _, t := range catalogue {
		if t.Annotations.readOnly() {
			want[t.Name] = true
		}
	}
	got := make(map[string]bool, len(resolved))
	for _, t := range resolved {
		got[t.Name] = true
		if !want[t.Name] {
			extra = append(extra, t.Name)
		}
	}
	for name := range want {
		if !got[name] {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	return missing, extra
}

// namesOutside splits what an agent reported into the catalogue's tools
// outside its toolset — the ones it must never have seen — and names the
// catalogue does not know at all (the model's noise: reported, not fatal).
func namesOutside(reported, toolset, catalogue []string) (outside, unknown []string) {
	for _, n := range reported {
		switch {
		case slices.Contains(toolset, n):
		case slices.Contains(catalogue, n):
			outside = append(outside, n)
		default:
			unknown = append(unknown, n)
		}
	}
	return outside, unknown
}

// mcpServerToolGroups maps every MCPServer of the platform namespace to its
// tool-group label ("" when unlabelled).
func mcpServerToolGroups() (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	servers, err := listMCPServers(ctx, platformNamespace, "")
	if err != nil {
		return nil, err
	}
	labels := make(map[string]string, len(servers))
	for _, s := range servers {
		labels[s.Name] = s.Labels[toolGroupLabel]
	}
	return labels, nil
}

// waitAgentReady polls agent-manager's get_agent_status until the verdict is
// ready.
func waitAgentReady(s *musterSession, toolPrefix, name string) error {
	var status struct {
		Verdict string `json:"verdict"`
		Summary string `json:"summary"`
	}
	ready := waitFor(60, 3*time.Second, func() bool {
		status.Verdict, status.Summary = "", ""
		if err := s.callServerJSON(toolPrefix+"get_agent_status", map[string]any{nameKey: name}, &status); err != nil {
			return false
		}
		return status.Verdict == "ready"
	})
	if !ready {
		return fmt.Errorf("%s never reached ready (last: %s — %s);\ncheck `kubectl -n %s get helmrelease,agents.kagent.dev,pods`", name, status.Verdict, status.Summary, kagentNamespace)
	}
	return nil
}

// a2aURL is the agent's A2A endpoint the portal's backend posts to: kagent's
// API behind the agentgateway edge (agentPlatform.kagent.installations.*.apiBaseUrl).
func a2aURL(cfg *config.Config, name string) string {
	return cfg.AgentgatewayBaseURL() + "/kagent/api/a2a/" + kagentNamespace + "/" + name
}

// agentTurnAs sends one A2A message/send to an agent as the user whose Dex
// id_token is given — the way the portal's session chat does — and returns
// the agent's text. The Ready condition can precede the runtime listening by
// a moment, hence the retry.
func agentTurnAs(cfg *config.Config, name, token, prompt string) (string, error) {
	client, err := labHTTPClient(180 * time.Second)
	if err != nil {
		return "", err
	}
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"message":{"kind":"message","role":"user","messageId":%q,"parts":[{"kind":"text","text":%q}]}}}`,
		randHex(8), prompt)
	var reply string
	var lastErr error
	answered := waitFor(4, 5*time.Second, func() bool {
		reply, lastErr = a2aSend(client, a2aURL(cfg, name), token, payload, prompt)
		return lastErr == nil
	})
	if !answered {
		return "", fmt.Errorf("A2A turn against %s failed: %w", name, lastErr)
	}
	return reply, nil
}

// toolListingPrompt asks an agent to report what filter_tools returns, in a
// shape the proof can parse: one tool name per line, or NO_TOOLS.
const toolListingPrompt = "Call your filter_tools tool exactly once with the arguments {\"limit\": 500}. " +
	"Then reply with ONLY the tool names from its result, one per line, no other words. " +
	"If the result has no tools, or you have no tools to call at all, reply with exactly NO_TOOLS."

// toolNamesInReply extracts the tool names an agent reported.
func toolNamesInReply(reply string) []string {
	var names []string
	for _, field := range strings.FieldsFunc(reply, func(r rune) bool { return r == '\n' || r == ',' || r == ' ' || r == '`' || r == '*' || r == '\t' }) {
		// List bullets and trailing punctuation around a name, not the
		// dashes inside one (x_mcp-kubernetes_list).
		field = strings.Trim(field, "-.;:()[]\"'")
		if strings.HasPrefix(field, "x_") || strings.HasPrefix(field, "workflow_") || strings.HasPrefix(field, "core_") {
			names = append(names, field)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// proveAgentRuntimeToolsets is the runtime path: the agents answer real A2A
// turns as the user, and what they report seeing is what their header
// resolves to — so kagent sends the header and the token.
func proveAgentRuntimeToolsets(cfg *config.Config, token, modelConfig, toolPrefix string, s *musterSession, res *musterToolsetResults) error {
	for _, name := range []string{toolsetsAgentReadOnly, toolsetsAgentNone} {
		step("Waiting for %s to be ready (ModelConfig %s)", name, modelConfig)
		if err := waitAgentReady(s, toolPrefix, name); err != nil {
			return err
		}
	}
	step("A2A turn as the user: %s reports its tools", toolsetsAgentReadOnly)
	reply, err := agentTurnAs(cfg, toolsetsAgentReadOnly, token, toolListingPrompt)
	if err != nil {
		return err
	}
	names := toolNamesInReply(reply)
	k8sPrefix := "x_" + cfg.MCPServerName() + "_"
	if len(names) == 0 {
		return fmt.Errorf("%s reported no tools (reply: %s) — the runtime did not reach muster with the user's token, or the model did not follow the instruction", toolsetsAgentReadOnly, excerpt(reply, 300))
	}
	if !slices.Contains(names, k8sPrefix+"list") {
		return fmt.Errorf("%s did not report %slist among %d tools: %v", toolsetsAgentReadOnly, k8sPrefix, len(names), names)
	}
	// What the agent sees is what its header resolves to: nothing of the
	// catalogue outside preset:read-only — not agent-manager's writer, not the
	// mutating workflow, not a core write. muster's read-only core tools are
	// legitimately among them (annotated since muster 5.13.0).
	outside, unknown := namesOutside(names, res.readOnlyNames, res.fullNames)
	if len(outside) > 0 {
		return fmt.Errorf("%s reported %v, outside %s: the runtime did not send the header (names: %v)", toolsetsAgentReadOnly, outside, presetReadOnly, names)
	}
	if len(unknown) > 0 {
		note("%d reported names the catalogue does not know, ignored: %v", len(unknown), unknown)
	}
	coreReported := 0
	for _, n := range names {
		if strings.HasPrefix(n, "core_") {
			coreReported++
		}
	}
	note("%d tools reported, %slist among them, every one within %s (%d read-only core tools, no writer)", len(names), k8sPrefix, presetReadOnly, coreReported)

	step("A2A turn as the user: %s (no tool entry) answers a chat turn", toolsetsAgentNone)
	reply, err = agentTurnAs(cfg, toolsetsAgentNone, token, "Reply with exactly the word pong and nothing else.")
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(reply), "pong") {
		return fmt.Errorf("%s answered %q, not pong", toolsetsAgentNone, excerpt(reply, 200))
	}
	note("answered: %s", excerpt(reply, 60))
	return nil
}
