package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The sign-in half of the toolset proof, and the portal half.

// signInChallengeViaPortal asks the portal's muster backend for the per-server
// sign-in challenge (POST /api/muster/auth/login — what the Sign in button of
// the servers page and of the Tools step calls), with the session's own
// forwarded Dex id_token, and returns the challenge URL.
func signInChallengeViaPortal(ps *portalSession, server string) (string, error) {
	status, raw, err := ps.musterPost("/auth/login"+installationQuery, map[string]any{serverKey: server})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("muster /auth/login for %s FAILED %d: %.200s", server, status, raw)
	}
	var login struct {
		Status  string `json:"status"`
		AuthURL string `json:"authUrl"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &login); err != nil {
		return "", fmt.Errorf("parsing /auth/login answer: %w\n%.200s", err, raw)
	}
	if login.Status != "auth_required" || login.AuthURL == "" {
		return "", fmt.Errorf("/auth/login for %s answered status %q, not auth_required: %.300s", server, login.Status, login.Message)
	}
	return login.AuthURL, nil
}

// fixtureToolsFor opens a fresh MCP session with the token and the header an
// agent declaring the fixture would send, and returns what it resolves to
// (the tool names) and the unmatched selectors.
func fixtureToolsFor(cfg *config.Config, token, clientName string) ([]string, []string, error) {
	s, err := openMusterSession(cfg, token, clientName)
	if err != nil {
		return nil, nil, err
	}
	s.setHeader(toolsetHeader, toolsetFixtureSelector)
	r, err := s.filterTools(nil)
	if err != nil {
		return nil, nil, err
	}
	return r.names(), r.ToolsetUnmatched, nil
}

// sessionIDInChallenge decodes the muster session the challenge's state is
// bound to (the state is base64url JSON carrying session_id, the token-derived
// `ext-…` identifier for a forwarded bearer).
func sessionIDInChallenge(challengeURL string) string {
	u, err := url.Parse(challengeURL)
	if err != nil {
		return ""
	}
	state := u.Query().Get("state")
	raw, err := base64URLDecode(state)
	if err != nil {
		return ""
	}
	var payload struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(raw, &payload)
	return payload.SessionID
}

// proveSignInScopedToolset settles ground-truth v3's G6: a sign-in the human
// completes in the portal is what makes that server's tools available to an
// agent request carrying the same forwarded token — because muster keys the
// session of a forwarded bearer by the token itself, and the portal forwards
// one id_token to muster and to kagent alike. Proven against the lab's OAuth
// fixture, pinned to Dex so the sign-in can complete headlessly:
//
//   - before any sign-in, a toolset naming the fixture resolves to nothing for
//     everyone (toolset_unmatched names the selector);
//   - the portal's Sign in (POST /api/muster/auth/login with the portal's own
//     Dex id_token) yields the challenge, completed as the browser would;
//   - an agent-shaped session on the SAME id_token then resolves the fixture's
//     tools and can call them; the real agent, driven through kagent with that
//     token, reports them too;
//   - a second user without the sign-in, and even the same user under a
//     different id_token (a fresh login), still resolve nothing — the grant is
//     the session's, and the session is the token's (grantScope: session).
func proveSignInScopedToolset(cfg *config.Config, user, other *config.User, toolPrefix string, admin *musterSession, opts ToolsetsTestOptions) ([]string, error) {
	var verdicts []string
	step("The OAuth sign-in fixture %s, pinned to Dex so the sign-in can complete", oauthFixtureServer)
	if err := ensureOAuthFixture(cfg); err != nil {
		return nil, err
	}

	step("Before any sign-in: %s resolves to nothing for %s and %s", toolsetFixtureSelector, user.Email, other.Email)
	otherToken, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		other.Email, other.Password, musterLoginScopes)
	if err != nil {
		return nil, err
	}
	ps, err := backstageLogin(cfg, user)
	if err != nil {
		return nil, err
	}
	for _, tc := range []struct {
		who   string
		token string
	}{{user.Email + " (portal token)", ps.dexIDToken}, {other.Email, otherToken}} {
		names, unmatched, err := fixtureToolsFor(cfg, tc.token, "toolsets-test-g6-before")
		if err != nil {
			return nil, err
		}
		if len(names) != 0 || !slices.Contains(unmatched, toolsetFixtureSelector) {
			return nil, fmt.Errorf("%s resolves %s to %d tools (unmatched %v) before any sign-in — a leftover grant? run core_auth_logout for %s", tc.who, toolsetFixtureSelector, len(names), unmatched, oauthFixtureServer)
		}
		note("%s: 0 tools, toolset_unmatched=%v", tc.who, unmatched)
	}

	step("The portal's Sign in for %s as %s, completed headlessly", oauthFixtureServer, user.Email)
	challenge, err := signInChallengeViaPortal(ps, oauthFixtureServer)
	if err != nil {
		return nil, err
	}
	portalSessionID := sessionIDInChallenge(challenge)
	note("challenge bound to muster session %s (the portal's forwarded token)", portalSessionID)
	if err := completeSignIn(challenge, user); err != nil {
		return nil, err
	}
	// Leave the lab as found: the grant is the session's and the sign-out
	// forgets it, but the pooled connection to the fixture lingers until its
	// next use and keeps the CR at Connected — where an older binary's
	// platform-test waits for Auth Required. A one-shot restart request on
	// the CR (spec.restartRequestedAt) drops the connection right away.
	defer func() {
		s, err := openMusterSession(cfg, ps.dexIDToken, "toolsets-test-g6-logout")
		if err == nil {
			_, _ = s.callToolEnvelope("core_auth_logout", map[string]any{serverKey: oauthFixtureServer})
		}
		restartOAuthFixture()
	}()
	note("Dex login completed; muster's proxy callback answered Authentication Successful")

	step("An agent-shaped session on the same token: %s resolves to the fixture's tools and can call them", toolsetFixtureSelector)
	var names []string
	resolved := waitFor(10, 2*time.Second, func() bool {
		var unmatched []string
		names, unmatched, err = fixtureToolsFor(cfg, ps.dexIDToken, "toolsets-test-g6-agent")
		return err == nil && len(names) > 0 && len(unmatched) == 0
	})
	if err != nil {
		return nil, err
	}
	if !resolved {
		return nil, fmt.Errorf("after the sign-in, %s still resolves to nothing on the same token", toolsetFixtureSelector)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, "x_"+oauthFixtureServer+"_") {
			return nil, fmt.Errorf("%s resolved to %s, a tool of another server", toolsetFixtureSelector, n)
		}
	}
	agentSession, err := openMusterSession(cfg, ps.dexIDToken, "toolsets-test-g6-call")
	if err != nil {
		return nil, err
	}
	agentSession.setHeader(toolsetHeader, toolsetFixtureSelector)
	agentChallenge, err := agentSession.callToolEnvelope("core_auth_login", map[string]any{serverKey: oauthFixtureServer})
	if err != nil {
		return nil, err
	}
	if agentChallenge.IsError || agentChallenge.StructuredContent["authUrl"] != nil {
		// core_auth_login of a server the session is signed in to answers
		// no challenge: the two MCP sessions are one muster session.
		if authURL, _ := agentChallenge.StructuredContent["authUrl"].(string); authURL != "" {
			if agentSID := sessionIDInChallenge(authURL); agentSID != portalSessionID {
				return nil, fmt.Errorf("the agent-shaped session is muster session %s, the portal's was %s — the same token gave two sessions", agentSID, portalSessionID)
			}
		}
	}
	fixtureText, err := agentSession.callServerTool("x_"+oauthFixtureServer+"_list_core_tools", nil)
	if err != nil {
		return nil, fmt.Errorf("calling the fixture through the agent-shaped session after the portal's sign-in: %w", err)
	}
	note("%d tools (%s…); x_%s_list_core_tools answered (%s)", len(names), names[0], oauthFixtureServer, excerpt(fixtureText, 60))
	verdicts = append(verdicts, fmt.Sprintf("PASS: G6 — the sign-in completed through the portal (POST /api/muster/auth/login, muster session %s) makes %s resolve to the fixture's %d tools for an agent-shaped request on the same forwarded id_token, callable through it", portalSessionID, toolsetFixtureSelector, len(names)))

	step("The same user under a different id_token, and %s without a sign-in: nothing", other.Email)
	freshToken, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return nil, err
	}
	for _, tc := range []struct {
		who   string
		token string
	}{{user.Email + " (fresh id_token)", freshToken}, {other.Email, otherToken}} {
		names, unmatched, err := fixtureToolsFor(cfg, tc.token, "toolsets-test-g6-other")
		if err != nil {
			return nil, err
		}
		if len(names) != 0 || !slices.Contains(unmatched, toolsetFixtureSelector) {
			return nil, fmt.Errorf("%s resolves %s to %d tools (unmatched %v) although that token never signed in", tc.who, toolsetFixtureSelector, len(names), unmatched)
		}
		note("%s: 0 tools, toolset_unmatched=%v", tc.who, unmatched)
	}
	verdicts = append(verdicts, fmt.Sprintf("PASS: G6 boundary — %s lacks the fixture's tools (toolset_unmatched names %s), and so does %s under a fresh id_token: the grant is the token-derived session's, not the person's (grantScope: session)", other.Email, toolsetFixtureSelector, user.Email))

	if opts.SkipChat {
		note("skipping the model turns (--skip-chat): the real agent's view of the fixture was not exercised")
		return verdicts, nil
	}
	step("The real agent %s through kagent: as %s (the portal's token) it reports the fixture's tools, as %s none", toolsetsAgentOAuth, user.Email, other.Email)
	if err := waitAgentReady(admin, toolPrefix, toolsetsAgentOAuth); err != nil {
		return nil, err
	}
	reply, err := agentTurnAs(cfg, toolsetsAgentOAuth, ps.dexIDToken, toolListingPrompt)
	if err != nil {
		return nil, err
	}
	reported := toolNamesInReply(reply)
	fixtureTools := 0
	for _, n := range reported {
		if strings.HasPrefix(n, "x_"+oauthFixtureServer+"_") {
			fixtureTools++
		} else {
			return nil, fmt.Errorf("%s reported %s, outside %s", toolsetsAgentOAuth, n, toolsetFixtureSelector)
		}
	}
	if fixtureTools == 0 {
		return nil, fmt.Errorf("%s as %s reported no fixture tools (reply: %s)", toolsetsAgentOAuth, user.Email, excerpt(reply, 300))
	}
	note("as %s: %d fixture tools reported", user.Email, fixtureTools)
	reply, err = agentTurnAs(cfg, toolsetsAgentOAuth, otherToken, toolListingPrompt)
	if err != nil {
		return nil, err
	}
	if reported := toolNamesInReply(reply); len(reported) != 0 {
		return nil, fmt.Errorf("%s as %s reported %v although %s never signed in to %s", toolsetsAgentOAuth, other.Email, reported, other.Email, oauthFixtureServer)
	}
	note("as %s: %s", other.Email, excerpt(reply, 80))
	verdicts = append(verdicts, fmt.Sprintf("PASS: G6 end to end — the agent %s (toolset %s), driven through kagent with the portal's token, reports the fixture's tools; driven by %s it reports none", toolsetsAgentOAuth, toolsetFixtureSelector, other.Email))
	return verdicts, nil
}

// portalFilterTools calls the muster backend route the Tools step uses:
// GET /api/muster/tools/filter with one toolset= entry per selector and
// include_presets, with the portal's forwarded token.
func portalFilterTools(ps *portalSession, toolset []string, includePresets bool) (*filterToolsResponse, error) {
	q := url.Values{"installation": {platformRelease}, "limit": {"1000"}}
	for _, sel := range toolset {
		q.Add("toolset", sel)
	}
	if includePresets {
		q.Set("include_presets", "true")
	}
	status, payload, err := ps.musterGet("/tools/filter?" + q.Encode())
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("muster /tools/filter answered %d: %.300v", status, payload)
	}
	// The route relays muster's result; the tool payload sits in its
	// content[0].text like every meta-tool answer, or is already unwrapped.
	raw, _ := json.Marshal(payload)
	var out filterToolsResponse
	if err := json.Unmarshal(raw, &out); err == nil && (len(out.Tools) > 0 || len(out.Presets) > 0 || len(out.ToolsetUnmatched) > 0) {
		return &out, nil
	}
	if text, ok := unwrapKey(payload, "text").(string); ok {
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			return nil, fmt.Errorf("parsing /tools/filter payload: %w\n%.300s", err, text)
		}
		return &out, nil
	}
	if errText, ok := unwrapKey(payload, "error").(string); ok {
		return nil, fmt.Errorf("muster /tools/filter: %s", errText)
	}
	return &out, nil
}

// proveToolsetPortal is the portal half: the Tools step's backend calls
// (presets, live resolution, unmatched selectors) and the composer's apply
// path (the same scaffolder template the wizard's Deploy drives, with the
// manifest the composer emits, the user's own OIDC token as the secret) —
// asserted on what lands: the HelmRelease value and the Agent's header.
func proveToolsetPortal(cfg *config.Config, user *config.User) ([]string, error) {
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
	if _, err := portalFilterTools(ps, []string{"preset:agentlab-no-such-preset"}, false); err == nil || !strings.Contains(err.Error(), "unknown preset") {
		return nil, fmt.Errorf("/tools/filter with an unknown preset should relay muster's error, got %v", err)
	}
	note("%s -> %d read-only tools; an unknown server -> toolset_unmatched; an unknown preset -> muster's error relayed", presetReadOnly, len(ro.Tools))
	verdicts = append(verdicts, fmt.Sprintf("PASS: the Tools step's backend (/api/muster/tools/filter) offers the presets [%s], resolves %s live (%d read-only tools) and reports unmatched selectors and unknown presets as muster does", strings.Join(presetNames, ", "), presetReadOnly, len(ro.Tools)))

	step("The composer's apply path: template:default/agent-deployment with the composed manifest (toolset %s) as %s", presetReadOnly, user.Email)
	manifest := composeAgentManifest(toolsetsAgentPortal, "default-model-config", []string{presetReadOnly})
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
	header, err := toolsetHeaderOf(cr)
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

// composeAgentManifest is the composer's combinedManifest for a new agent
// (plugins/agent-platform composeManifests.ts): the shared OCIRepository of
// the chart, then the HelmRelease with the values — `agent`, `modelConfig`
// and the top-level `toolset` exactly as the Tools step composed it — and
// `spec.serviceAccountName`, which the portal takes from the app-config key
// `agentPlatform.fluxServiceAccountName` the chart renders (composeManifests.ts).
func composeAgentManifest(name, modelConfig string, toolset []string) string {
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

// scaffold drives the hidden agent-deployment template the wizard's Deploy
// button drives (useDeployAgent.ts): scaffolderApi.scaffold() with the
// manifest, the installation and the user's per-installation OIDC token as
// the USER_OIDC_TOKEN secret, then waits for the task to complete.
func scaffold(ps *portalSession, manifest, releaseName string) (string, error) {
	status, raw, err := ps.backstagePostJSON("/api/scaffolder/v2/tasks", map[string]any{
		"templateRef": "template:default/agent-deployment",
		"values": map[string]any{
			"manifest":              manifest,
			"clusterName":           platformRelease,
			"releaseName":           releaseName,
			"namespace":             kagentNamespace,
			"oidcTokenInstallation": platformRelease,
		},
		"secrets": map[string]any{"USER_OIDC_TOKEN": ps.dexIDToken},
	})
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", fmt.Errorf("scaffolder refused the task (%d): %.300s", status, raw)
	}
	var task struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &task); err != nil || task.ID == "" {
		return "", fmt.Errorf("scaffolder answered without a task id: %.200s", raw)
	}
	var state string
	done := waitFor(30, 3*time.Second, func() bool {
		_, raw, err := ps.backstageGet("/api/scaffolder/v2/tasks/" + task.ID)
		if err != nil {
			return false
		}
		var t struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(raw, &t)
		state = t.Status
		return state == "completed" || state == taskStateFailed || state == "cancelled"
	})
	if !done || state != "completed" {
		_, events, _ := ps.backstageGet("/api/scaffolder/v2/tasks/" + task.ID + "/events")
		return "", fmt.Errorf("scaffolder task %s ended %q:\n%s", task.ID, state, excerpt(string(events), 600))
	}
	return task.ID, nil
}

// deploymentImage is the image of a Deployment's first container — what
// `kubectl get deploy -o jsonpath={.spec.template.spec.containers[0].image}`
// printed — or "" when the Deployment cannot be read (a note, not a verdict).
func deploymentImage(ns, name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	d, err := getObject(ctx, gvrDeployments, ns, name)
	if err != nil {
		return ""
	}
	containers, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		return ""
	}
	first, ok := containers[0].(map[string]any)
	if !ok {
		return ""
	}
	image, _, _ := unstructured.NestedString(first, "image")
	return image
}
