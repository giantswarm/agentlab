package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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

// The tenant identity every agent HelmRelease runs as (the connectivity
// chart renders the ServiceAccount + RoleBinding from
// kagent.fluxServiceAccountName; agent-manager and the portal's template
// name it). The chart's engine is multitenant: a HelmRelease that names
// no ServiceAccount runs as the namespace's `default`, which holds no
// RBAC, and never renders.
const kagentFluxServiceAccount = "kagent-flux"

// kagentAgentResource is kagent's Agent as a resource argument, fully
// qualified so a same-named kind in another group can never be meant (the
// Flux HelmRelease every agent is rendered from is fluxHelmReleaseResource).
const kagentAgentResource = "agents.kagent.dev"

// toolsetsTestV1 is the headless end-to-end proof of declared toolsets against
// the released components in the lab. See the file comment for what it
// covers; it prints one PASS line per claim and leaves nothing behind.
func toolsetsTestV1(cfg *config.Config, email string, opts ToolsetsTestOptions) error {
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
	cleanup := func() { toolsetsCleanupV1(admin, toolPrefix) }
	cleanup()
	defer cleanup()

	// 1. agent-manager: the toolset is the contract's required argument.
	if err := proveAgentManagerToolsetV1(admin, toolPrefix, opts.ModelConfig); err != nil {
		return err
	}
	pass("agent-manager refuses create_agent without a toolset (naming the presets and preset:none), refuses toolNames, and accepts a toolset")

	// 2. What the platform rendered: the header on the Agent CR, the value on
	//    the HelmRelease, agent-manager's own report — and the agent that
	//    declares none.
	if err := proveRenderedToolsetsV1(admin, toolPrefix, opts.ModelConfig); err != nil {
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
		if err := proveAgentRuntimeToolsetsV1(cfg, token, opts.ModelConfig, toolPrefix, admin, res); err != nil {
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
	portal, err := proveToolsetPortalV1(cfg, user)
	if err != nil {
		return err
	}
	verdicts = append(verdicts, portal...)

	fmt.Println()
	fmt.Println(strings.Join(verdicts, "\n"))
	return nil
}

// toolsetsCleanupV1 removes everything the proof creates: the agents through
// agent-manager (force: the legacy and portal releases have no agent-manager
// provenance), the HelmReleases directly when agent-manager does not list
// them, the workflows, and the fixture sign-in of the run.
func toolsetsCleanupV1(s *musterSession, toolPrefix string) {
	names := []string{toolsetsAgentReadOnly, toolsetsAgentNone, toolsetsAgentFull, toolsetsAgentOAuth, toolsetsAgentLegacy, toolsetsAgentPortal}
	removed := false
	for _, name := range names {
		if _, err := s.callServerTool(toolPrefix+"get_agent", map[string]any{nameKey: name}); err != nil {
			// Not known to agent-manager: a HelmRelease may still exist.
			deleteAgentHelmRelease(name)
			continue
		}
		if _, err := s.callServerTool(toolPrefix+"delete_agent", map[string]any{nameKey: name, forceKey: true}); err != nil {
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

// proveAgentManagerToolsetV1 drives agent-manager's contract through muster:
// create_agent without a toolset is refused naming the shipped presets and
// pointing at preset:none; the removed toolNames argument is refused with the
// explaining error; the four agents of the proof are created with theirs.
func proveAgentManagerToolsetV1(s *musterSession, toolPrefix, modelConfig string) error {
	step("%screate_agent without a toolset — expecting the refusal", toolPrefix)
	base := func(name string) map[string]any {
		return map[string]any{
			nameKey: name, modelConfigKey: modelConfig, displayNameKey: "agentlab toolset proof: " + strings.TrimPrefix(name, toolsetsTestPrefix+"-"),
			descriptionKey:   "Throwaway agent of `agentlab toolsets-test`; deleted by the same run.",
			systemMessageKey: toolsetTestAgentSystemMsg,
		}
	}
	text, err := s.callServerTool(toolPrefix+"create_agent", base(toolsetsAgentReadOnly))
	if err == nil {
		return fmt.Errorf("create_agent without a toolset was accepted (%.200s) — agent-manager predates the toolset contract (needs ≥ 0.4.0)", text)
	}
	for _, want := range []string{toolsetKey, presetReadOnlyName, presetNoneName, "infrastructure", "agent-platform", presetFullName, presetNone} {
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
	args[toolsetKey] = []string{presetReadOnly}
	args["toolNames"] = []string{"list_tools"}
	if text, err := s.callServerTool(toolPrefix+"create_agent", args); err == nil {
		return fmt.Errorf("create_agent accepted toolNames (%.200s); the inert knob must be refused", text)
	} else if !strings.Contains(err.Error(), "toolNames") || !strings.Contains(err.Error(), toolsetKey) {
		return fmt.Errorf("the toolNames refusal does not explain itself: %v", err)
	} else {
		note("refused: %s", excerpt(err.Error(), 200))
	}

	for _, a := range toolsetsAgents {
		step("%screate_agent %s with toolset %v", toolPrefix, a.name, a.toolset)
		args := base(a.name)
		args[toolsetKey] = a.toolset
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

// toolsetHeaderOfCR returns the X-Muster-Toolset value on the agent's muster
// tool entry, "" when the entry carries none, and an error when the Agent has
// no muster tool entry at all.
func toolsetHeaderOfCR(cr *agentCR) (string, error) {
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
	value, found, err := unstructured.NestedFieldNoCopy(hr.Object, "spec", "values", toolsetKey)
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

// proveRenderedToolsetsV1 asserts the manifests behind the created agents and
// agent-manager's report of them, and the shape of an agent that declares no
// toolset (a HelmRelease applied without the value, like every agent that
// predates toolsets).
func proveRenderedToolsetsV1(s *musterSession, toolPrefix, modelConfig string) error {
	for _, a := range toolsetsAgents {
		step("Agent %s: the rendered header and the HelmRelease value", a.name)
		cr, err := waitAgentCR(a.name)
		if err != nil {
			return err
		}
		want := strings.Join(a.toolset, ",")
		header, err := toolsetHeaderOfCR(cr)
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
	header, err := toolsetHeaderOfCR(cr)
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

// a2aURL is the agent's A2A endpoint the portal's backend posts to: kagent's
// API behind the agentgateway edge (agentPlatform.kagent.installations.*.apiBaseUrl).
func a2aURL(cfg *config.Config, name string) string {
	return cfg.AgentgatewayBaseURL() + "/kagent/api/a2a/" + kagentNamespace + "/" + name
}

// agentTurnAsV1 sends one A2A message/send to an agent as the user whose Dex
// id_token is given — the way the portal's session chat does — and returns
// the agent's text. The Ready condition can precede the runtime listening by
// a moment, hence the retry.
func agentTurnAsV1(cfg *config.Config, name, token, prompt string) (string, error) {
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

// proveAgentRuntimeToolsetsV1 is the runtime path: the agents answer real A2A
// turns as the user, and what they report seeing is what their header
// resolves to — so kagent sends the header and the token.
func proveAgentRuntimeToolsetsV1(cfg *config.Config, token, modelConfig, toolPrefix string, s *musterSession, res *musterToolsetResults) error {
	for _, name := range []string{toolsetsAgentReadOnly, toolsetsAgentNone} {
		step("Waiting for %s to be ready (ModelConfig %s)", name, modelConfig)
		if err := waitAgentReady(s, toolPrefix, name); err != nil {
			return err
		}
	}
	step("A2A turn as the user: %s reports its tools", toolsetsAgentReadOnly)
	reply, err := agentTurnAsV1(cfg, toolsetsAgentReadOnly, token, toolListingPrompt)
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
	reply, err = agentTurnAsV1(cfg, toolsetsAgentNone, token, "Reply with exactly the word pong and nothing else.")
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(reply), "pong") {
		return fmt.Errorf("%s answered %q, not pong", toolsetsAgentNone, excerpt(reply, 200))
	}
	note("answered: %s", excerpt(reply, 60))
	return nil
}
