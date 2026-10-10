package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The platform proof's stages, in order, as the summary names them.
const (
	stageDexToken             = "Dex token"
	stageMusterSession        = "muster session"
	stageKubernetesTools      = "Kubernetes tools"
	stageMCPToolCall          = "MCP tool call"
	stageProvidedAPIs         = "provided APIs"
	stageFleetAdmission       = "fleet admission"
	stageDexReach             = "Dex reach"
	stageDownstreamIdentity   = "downstream identity"
	stageControllerIdentity   = "controller identity"
	stageAgentManagerIdentity = "agent-manager identity"
	stageAteletPolicy         = "atelet image-cache policy"
	stageSubstrateLine        = "Substrate line"
	stageWorkerPools          = "WorkerPool workers"
	stageWorkspaceManager     = "workspace-manager providers"
	stageOAuthSignIn          = "per-server OAuth sign-in"
	stageFamilies             = "infrastructure families"
	stagePrometheusTools      = "Prometheus tools"
	stagePromQL               = "PromQL through muster"
	stageScraped              = "platform targets scraped"
	stageMetricsEndpoint      = "Backstage metrics endpoint"
)

// PlatformTest is the headless end-to-end proof: Dex login -> muster
// (OAuth-protected) -> the Kubernetes MCP -> the kind apiserver.
//
// Uses the Dex password grant with client_id=muster, which yields an id_token
// with aud=muster. muster accepts it directly because `muster` is listed under
// oauth.server.trustedAudiences. Claude Code instead does the full browser
// authorization-code flow; this is the CI-friendly shortcut.
//
// The proof runs in stages (proofStages): each named, timed and judged, the
// summary before the verdict. Nothing the lab's state cannot prove is left
// out silently — a missing fixture user, a release that is not there, a
// read that failed fail the stage — and the stages agentlab.yaml leaves
// without a subject (agents or observability off, the 3.x line) are listed
// as skipped with that reason.
func PlatformTest(cfg *config.Config, email string) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}
	p := newProofStages()
	verdict, err := platformTest(cfg, user, p)
	if err := p.close(err); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println(verdict)
	return nil
}

// platformTest is the proof's body, stage by stage on p; the verdict it
// returns is printed once every stage has passed.
func platformTest(cfg *config.Config, user *config.User, p *proofStages) (string, error) {
	client, err := labHTTPClient(30 * time.Second)
	if err != nil {
		return "", err
	}

	p.begin(stageDexToken, "Logging in to Dex as %s", user.Email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return "", err
	}
	note("got an id_token")

	p.begin(stageMusterSession, "MCP initialize against %s", cfg.MusterBaseURL())
	if !httpUp(client, cfg.MusterBaseURL()+"/.well-known/oauth-authorization-server") {
		return "", fmt.Errorf("muster is not reachable at %s — run `agentlab platform` first", cfg.MusterBaseURL())
	}
	mcpURL := cfg.MusterBaseURL() + "/mcp"
	post := func(sessionID, payload string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPost, mcpURL, strings.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
		}
		return client.Do(req)
	}
	resp, err := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"platform-test","version":"1"}}}`)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		return "", fmt.Errorf("no session id — muster rejected the token:\n%s", strings.TrimSpace(string(body)))
	}
	note("session %s", sessionID)
	if resp, err := post(sessionID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	call := func(payload string) (map[string]any, error) {
		resp, err := post(sessionID, payload)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		parsed, err := parseMCPResponse(raw)
		if err != nil {
			return nil, fmt.Errorf("parsing muster response: %w\n%s", err, strings.TrimSpace(string(raw)))
		}
		return parsed, nil
	}

	p.begin(stageKubernetesTools, "Kubernetes tools muster is aggregating")
	// One page with every tool: list_tools pages at 50 by default, and the
	// family tools sort after the platform servers'.
	res, err := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_tools","arguments":{"limit":1000}}}`)
	if err != nil {
		return "", err
	}
	var toolList struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(innerText(res)), &toolList); err != nil {
		return "", fmt.Errorf("parsing list_tools payload: %w", err)
	}
	// The lab's mcp-kubernetes is the kubernetes family's member, so muster
	// exposes the family's tools: x_kubernetes_<tool>, management_cluster
	// selecting the lab's member by its name (kubernetesTool: the server's
	// own tools on the 3.x line).
	toolPrefix := kubernetesTool(cfg, "")
	shown := 0
	for _, t := range toolList.Tools {
		if strings.HasPrefix(t.Name, toolPrefix) && shown < 8 {
			note("%s", t.Name)
			shown++
		}
	}
	if shown == 0 {
		names := make([]string, 0, len(toolList.Tools))
		for _, t := range toolList.Tools {
			names = append(names, t.Name)
		}
		return "", fmt.Errorf("muster aggregates no %s tools (it lists %s)", toolPrefix, strings.Join(names, ", "))
	}

	p.begin(stageMCPToolCall, "Calling %slist namespaces on %s through muster", toolPrefix, cfg.MCPServerName())
	listArgs, err := json.Marshal(kubernetesArgs(cfg, map[string]any{"resourceType": "namespaces"}))
	if err != nil {
		return "", err
	}
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"call_tool","arguments":{"name":%q,"arguments":%s}}}`, toolPrefix+"list", listArgs)
	res, err = call(payload)
	if err != nil {
		return "", err
	}
	// Tool results are double-wrapped: result.content[0].text is JSON whose
	// content[0].text is the actual payload — two decode hops.
	var wrapped struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(innerText(res)), &wrapped); err != nil || len(wrapped.Content) == 0 {
		return "", fmt.Errorf("unexpected call_tool payload shape")
	}
	var nsList struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(wrapped.Content[0].Text), &nsList); err != nil {
		return "", fmt.Errorf("parsing namespace list: %w", err)
	}
	names := make([]string, 0, len(nsList.Items))
	for _, item := range nsList.Items {
		names = append(names, item.Name)
	}
	note("namespaces: %s", strings.Join(names, ", "))

	verdict := "PASS: Claude Code -> muster (Dex) -> mcp-kubernetes -> kind apiserver"

	// The APIs the lab provides itself (providedapis.go), as `kubectl
	// api-resources` would list them: a component chart that renders a
	// Gateway API route or a Cilium policy installs only while they are served.
	p.begin(stageProvidedAPIs, "Verifying the apiserver serves the APIs the lab provides itself")
	served, err := proveProvidedAPIs()
	if err != nil {
		return "", err
	}
	var groupVersions []string
	kindCount := 0
	for _, s := range served {
		note("%s", s)
		groupVersions = append(groupVersions, s.gv.String())
		kindCount += len(s.kinds)
	}
	verdict += fmt.Sprintf("\nPASS: the apiserver serves the %d kinds the lab provides itself (%s) — Gateway API routes and Cilium policies render, the policies enforce nothing", kindCount, strings.Join(groupVersions, ", "))

	// The fleet's admission (admission.go): the flux-multi-tenancy policy in
	// Enforce on the lab's Kyverno, answered for the four HelmRelease shapes
	// as server-side dry runs in the org namespace — denied with the fleet's
	// message where an installation denies, admitted where it admits.
	p.begin(stageFleetAdmission, "Verifying the fleet's %s policy enforces on HelmReleases in %s", fleetPolicyName, orgNamespace)
	admissionCtx, cancelAdmission := context.WithTimeout(context.Background(), kubeReadTimeout)
	cases, err := proveFleetAdmission(admissionCtx)
	cancelAdmission()
	if err != nil {
		return "", err
	}
	var caseLines []string
	for _, c := range cases {
		note("%s", c)
		caseLines = append(caseLines, c.String())
	}
	verdict += fmt.Sprintf("\nPASS: the fleet's %s policy (Enforce, Kyverno %s) answers as an installation does for %d HelmRelease shapes in %s — %s", fleetPolicyName, kyvernoChartVersion, len(cases), orgNamespace, strings.Join(caseLines, "; "))

	// The lab's Dex bridge: every OAuth resource server the rule selects on
	// the live Deployments carries the dex-localhost sidecar, runs without a
	// restart and reads Connected in muster (platformtest_sidecar.go) — the
	// chart's default-on managers and an overlay's included, not only the
	// servers the lab turns on itself.
	if dexLocalhostBridged(cfg) {
		p.begin(stageDexReach, "Verifying the %s sidecar on every server told the lab Dex address", dexLocalhostContainer)
	} else {
		p.begin(stageDexReach, "Verifying every server told the issuer %s reaches it without a sidecar", cfg.Issuer())
	}
	sidecarCtx, cancelSidecar := context.WithTimeout(context.Background(), kubeReadTimeout)
	servers, err := proveDexLocalhostSidecars(sidecarCtx, cfg)
	cancelSidecar()
	if err != nil {
		return "", err
	}
	var serverNames []string
	for _, s := range servers {
		serverNames = append(serverNames, s.String())
	}
	if dexLocalhostBridged(cfg) {
		verdict += fmt.Sprintf("\nPASS: the %s bridge on all %d servers told the lab Dex address through a name they dial it by — sidecar present, 0 restarts since the release, MCPServer Connected: %s", dexLocalhostContainer, len(servers), strings.Join(serverNames, ", "))
	} else {
		verdict += fmt.Sprintf("\nPASS: all %d servers told the issuer %s reach it through cluster DNS (the %s Service) — no sidecar, MCPServer Connected: %s", len(servers), cfg.Issuer(), dexIssuerService, strings.Join(serverNames, ", "))
	}

	// The user's identity, not a ServiceAccount: the same tool as two users
	// with different RBAC must answer differently — and a forged identity
	// header changes nothing, the bearer decides.
	p.begin(stageDownstreamIdentity, "Downstream identity: the forwarded Dex id_token decides at the apiserver, not a ServiceAccount")
	if err := proveDownstreamIdentity(cfg, toolPrefix); err != nil {
		return "", err
	}
	verdict += "\nPASS: the forwarded Dex id_token reaches the apiserver — kube-system Secrets: platform-admin allowed, viewer forbidden (user RBAC, not the ServiceAccount's; a forged x-user-id changes nothing)"
	if cfg.Platform.Agents {
		// The kagent controller behind the JWT policy on its route, and
		// agent-manager writing as the caller: the same forged header.
		p.begin(stageControllerIdentity, "The kagent controller route: the edge's JWT policy, the controller's trusted-proxy identity")
		if reason := controllerIdentitySkip(cfg); reason != "" {
			p.skip("%s", reason)
			verdict += "\nSKIP: the kagent controller route — " + reason
		} else {
			if err := proveControllerIdentity(cfg, user, token); err != nil {
				return "", err
			}
			verdict += "\nPASS: the kagent controller route — no token refused at the edge (JWT Strict); a valid token with a forged x-user-id attributed to the token's subject"
		}
		p.begin(stageAgentManagerIdentity, "agent-manager writes as the caller")
		if err := proveAgentManagerIdentity(cfg); err != nil {
			return "", err
		}
		verdict += "\nPASS: agent-manager writes as the caller — a viewer's create_agent with a forged x-user-id is the apiserver's Forbidden for the viewer"
		// Agent Substrate, the component the 4.x line added: the chart in
		// place renders its HelmRelease or has none to prove.
		substrateCtx, cancelSubstrate := context.WithTimeout(context.Background(), kubeReadTimeout)
		shipped, err := substrateShipped(substrateCtx, cfg)
		cancelSubstrate()
		if err != nil {
			return "", err
		}
		if !shipped {
			p.leaveOut(fmt.Sprintf("%s renders no %s HelmRelease, no Agent Substrate to prove", platformChartFor(cfg), substrateRelease), stageAteletPolicy, stageSubstrateLine, stageWorkerPools)
		} else {
			// The lab's atelet image-cache policy, live on the node agents
			// (ateletImageCacheArgs): without it a laptop above the chart's 85 %
			// watermark loses the Harness image five minutes after every turn.
			p.begin(stageAteletPolicy, "Verifying the atelet DaemonSet carries the lab's image-cache policy %s", strings.Join(ateletImageCacheArgs, " "))
			version, err := helmReleaseVersion(substrateNamespace, substrateRelease)
			if err != nil {
				return "", fmt.Errorf("reading the %s release in %s: %w", substrateRelease, substrateNamespace, err)
			}
			if version == "" {
				return "", fmt.Errorf("no %s release in %s although the chart renders its HelmRelease — Agent Substrate did not install (`agentlab status`, `agentlab platform`)", substrateRelease, substrateNamespace)
			}
			note("Substrate %s (release %s/%s)", version, substrateNamespace, substrateRelease)
			ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
			ready, err := proveAteletImageCachePolicy(ctx)
			cancel()
			if err != nil {
				return "", err
			}
			note("atelet %s/%s: the policy flags on all %d ready pods", substrateNamespace, ateletDaemonSet, ready)
			verdict += "\nPASS: atelet carries the lab's image-cache policy (the host disk cannot evict the Harness image; a 4 GiB cap bounds the cache)"
			// The two halves of Substrate on one release (proveSubstrateLine):
			// a skew installs green, boots no golden actor and shows only in
			// the atelet log — the first agents proof would burn its timeout.
			p.begin(stageSubstrateLine, "Verifying the atelet and the WorkerPool's workers are one Substrate release")
			ctx, cancel = context.WithTimeout(context.Background(), kubeReadTimeout)
			substrate, err := proveSubstrateLine(ctx, substrateSkewRemedy(cfg))
			cancel()
			if err != nil {
				return "", err
			}
			for _, s := range substrate {
				note("%s", s)
			}
			verdict += fmt.Sprintf("\nPASS: Agent Substrate is one release (%s) on the atelet and the WorkerPool's workers — golden actors can boot", substrate[0].release)
			// The workers themselves (proveWorkerPoolsRunning): images that
			// agree say nothing about pods that never schedule.
			p.begin(stageWorkerPools, "Verifying the WorkerPool's workers are Running on a node that carries the pool's node selector")
			ctx, cancel = context.WithTimeout(context.Background(), workerPoolRunningTimeout+kubeReadTimeout)
			pools, err := proveWorkerPoolsRunning(ctx, workerPoolRunningTimeout)
			cancel()
			if err != nil {
				return "", err
			}
			for _, pool := range pools {
				note("%s", pool)
				verdict += fmt.Sprintf("\nPASS: WorkerPool %s has %d workers Running on %s", pool.pool, pool.running, strings.Join(pool.nodes, ", "))
			}
		}
	} else {
		p.leaveOut("platform.agents is off in "+config.File, stageControllerIdentity, stageAgentManagerIdentity, stageAteletPolicy, stageSubstrateLine, stageWorkerPools)
	}

	// The workspace-manager (workspacemanagertest.go): registered with muster
	// and Connected, rolled out, and listing every configured provider
	// instance to the admin through muster.
	if reason := workspaceManagerSkip(cfg); reason != "" {
		p.leaveOut(reason, stageWorkspaceManager)
	} else {
		p.begin(stageWorkspaceManager, "The workspace-manager: registered with muster, Ready, list_providers as the admin")
		providers, err := proveWorkspaceManager(cfg)
		if err != nil {
			return "", err
		}
		verdict += fmt.Sprintf("\nPASS: the workspace-manager is registered with muster (MCPServer %s Connected) and lists its %d provider instances to the admin: %s", workspaceManagerMCPServer, len(providers), strings.Join(providers, ", "))
	}

	// The per-server sign-in path: muster as OAuth client, challenged by the
	// lab's Auth Required fixture (oauthfixture.go).
	p.begin(stageOAuthSignIn, "Per-server OAuth sign-in: core_auth_login for the %s fixture", oauthFixtureServer)
	if err := proveOAuthSignIn(cfg, token); err != nil {
		return "", err
	}
	verdict += "\nPASS: per-server OAuth sign-in -> muster (OAuth client) -> challenge on " + oauthProxyStartPath +
		" (fixture " + oauthFixtureServer + ")"
	// The infrastructure families (infrastructure.go): the lab's servers are
	// their members, labelled infrastructure, and nothing family-less or
	// lab-created is. The OAuth fixture stays a Registered server.
	p.begin(stageFamilies, "Infrastructure families: %s is the member of %s, %s=%s", cfg.ClusterName, strings.Join(labFamilies(cfg), ", "), toolGroupLabel, toolGroupInfrastructure)
	if reason := familiesSkip(cfg); reason != "" {
		p.skip("%s", reason)
		verdict += "\nSKIP: the infrastructure families — " + reason
	} else {
		if err := proveToolGroupLabels(cfg); err != nil {
			return "", err
		}
		verdict += fmt.Sprintf("\nPASS: %s is the member of %s (%s=%s, %s=%s), no family-less mcp-kubernetes; %s unlabelled (Registered servers)",
			cfg.ClusterName, strings.Join(labFamilies(cfg), ", "), toolGroupLabel, toolGroupInfrastructure, managementClusterLabel, cfg.ClusterName, oauthFixtureServer)
	}
	if !cfg.Platform.Observability {
		p.leaveOut("platform.observability is off in "+config.File, stagePrometheusTools, stagePromQL, stageScraped, stageMetricsEndpoint)
		return verdict, nil
	}

	// The prometheus family's tools, as for mcp-kubernetes: the lab's
	// mcpServers entry registers the server as the family's member (see
	// agent-platform-values.yaml.tmpl).
	promPrefix := familyTool(familyPrometheus, "")
	p.begin(stagePrometheusTools, "Prometheus tools muster is aggregating")
	shown = 0
	for _, t := range toolList.Tools {
		if strings.HasPrefix(t.Name, promPrefix) && shown < 8 {
			note("%s", t.Name)
			shown++
		}
	}
	if shown == 0 {
		return "", fmt.Errorf("muster aggregates no %s tools", promPrefix)
	}

	// promQL runs one instant query through muster and returns the tool's
	// rendered answer (result.content[0].text is JSON whose own
	// content[0].text is the actual payload — the same two decode hops as
	// call_tool above).
	promQL := func(query string) (string, error) {
		q, _ := json.Marshal(query)
		payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"call_tool","arguments":{"name":%q,"arguments":{%q:%q,"query":%s}}}}`, promPrefix+"execute_query", familyInstanceArg, cfg.PrometheusMCPServerName(), q)
		res, err := call(payload)
		if err != nil {
			return "", err
		}
		var wrapped struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(innerText(res)), &wrapped); err != nil || len(wrapped.Content) == 0 {
			return "", fmt.Errorf("unexpected execute_query payload shape")
		}
		return wrapped.Content[0].Text, nil
	}

	// `up` is non-empty as soon as Prometheus completes its first scrape,
	// so a short retry absorbs a just-booted lab.
	p.begin(stagePromQL, "Calling %sexecute_query (PromQL: up) through muster", promPrefix)
	var inner string
	queried := waitFor(15, 4*time.Second, func() bool {
		var err error
		if inner, err = promQL("up"); err != nil {
			return false
		}
		// The tool renders the Prometheus query result as text; an answer
		// with scrape targets in it proves collection AND the query path.
		return strings.Contains(inner, "up")
	})
	if !queried {
		return "", fmt.Errorf("execute_query never returned scrape targets (last payload: %.200s)", inner)
	}
	if len(inner) > 160 {
		inner = inner[:160] + "..."
	}
	note("query result: %s", strings.ReplaceAll(inner, "\n", " "))

	// The platform's own monitors: muster ServiceMonitor, valkey
	// PodMonitor and mcp-prometheus's ServiceMonitor (plus kagent's when
	// agents run) are enabled with observability, and the lab Prometheus
	// selects monitors from every release
	// (…NilUsesHelmValues: false, kube-prometheus-stack-values.yaml.tmpl).
	// A fresh install needs the operator to reload targets plus one 30s
	// scrape interval, hence the generous retry.
	expected := []string{componentMuster, "valkey", mcpPrometheusRelease}
	p.begin(stageScraped, "Verifying Prometheus scrapes the platform itself")
	if cfg.Platform.Agents {
		monitored, err := kagentControllerMonitored()
		if err != nil {
			return "", err
		}
		if monitored {
			expected = append(expected, "kagent")
		} else {
			note("no ServiceMonitor for the kagent controller in %s (kagent main serves no metrics listener): not expecting a kagent target", kagentNamespace)
		}
	}
	note("expecting the targets %s", strings.Join(expected, ", "))
	var missing []string
	scraped := waitFor(30, 5*time.Second, func() bool {
		series, err := promQL(`up{namespace=~"agent-platform|kagent|monitoring"} == 1`)
		if err != nil {
			return false
		}
		missing = missing[:0]
		for _, want := range expected {
			if !strings.Contains(series, want) {
				missing = append(missing, want)
			}
		}
		return len(missing) == 0
	})
	if !scraped {
		return "", fmt.Errorf("prometheus is not scraping %s;\n"+
			"check `kubectl -n %s get servicemonitors,podmonitors -A` and the targets via %sget_targets",
			strings.Join(missing, ", "), platformNamespace, promPrefix)
	}
	note("all platform targets are up: %s", strings.Join(expected, ", "))
	verdict += "\nPASS: Claude Code -> muster (Dex) -> mcp-prometheus -> Prometheus (platform targets scraped)"

	// The Backstage metrics path: gs-backend's MimirService queries
	// https://observability.<domain>/prometheus/api/v1/query — the lab
	// serves it via observability-route.yaml.tmpl. Run the exact
	// workload query shape the Deployments page uses and expect the
	// muster deployment in the answer (present whenever the platform
	// runs, unlike backstage's own).
	obsQueryURL := cfg.ObservabilityBaseURL() + "/api/v1/query?query=" +
		url.QueryEscape(`max without(app, container, customer, endpoint, instance, job, pipeline, pod, provider, region, service, service_priority) (kube_deployment_spec_replicas)`)
	p.begin(stageMetricsEndpoint, "Querying the Backstage metrics endpoint on the edge (%s)", cfg.ObservabilityBaseURL())
	body = nil
	answered := waitFor(10, 3*time.Second, func() bool {
		resp, err := client.Get(obsQueryURL)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ = io.ReadAll(resp.Body)
		return resp.StatusCode == http.StatusOK &&
			strings.Contains(string(body), `"deployment":"muster"`)
	})
	if !answered {
		return "", fmt.Errorf("the edge observability endpoint never answered the Deployments-page query "+
			"(last body: %.200s);\ncheck `kubectl -n monitoring get httproute observability` and the edge",
			string(body))
	}
	note("the Deployments-page query answers through the edge (deployment=muster found)")
	verdict += "\nPASS: Backstage metrics path -> edge -> Prometheus (Mimir-shaped /prometheus API)"
	return verdict, nil
}

// substrateShipped reports whether the agent-platform chart in place renders
// the Agent Substrate component — its HelmRelease in the platform namespace.
// The 3.x line has none (legacyChartDir tells the lines apart by it), so a
// 3.x lab has no Substrate to prove without a read; a 4.x chart that renders
// it must have installed the release. A read that fails is an error, never
// "no Substrate".
func substrateShipped(ctx context.Context, cfg *config.Config) (bool, error) {
	if cfg.LegacyChart() {
		return false, nil
	}
	gvr, err := gvrFor(helmReleaseResource)
	if err != nil {
		return false, err
	}
	releases, err := listObjects(ctx, gvr, platformNamespace, "")
	if err != nil {
		return false, fmt.Errorf("listing the platform's HelmReleases in %s: %w", platformNamespace, err)
	}
	for _, r := range releases {
		if r.GetName() == substrateRelease {
			return true, nil
		}
	}
	return false, nil
}

// musterTokenProbe is the smallest end-to-end auth check: a Dex password
// grant for the admin user, then an MCP initialize with the id_token as
// Bearer. A session id back means muster's whole token-validation chain
// (JWKS / userinfo over the lab CA) works. PlatformTest proves the same and
// more; this exists so `up` can verify and heal cheaply (see
// ensureMusterValidatesTokens).
func musterTokenProbe(cfg *config.Config) error {
	admin := cfg.AdminUser()
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		admin.Email, admin.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	client, err := labHTTPClient(10 * time.Second)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, cfg.MusterBaseURL()+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agentlab-up","version":"1"}}}`))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Mcp-Session-Id") == "" {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200] + "..."
		}
		return fmt.Errorf("muster rejected the token: %s", msg)
	}
	return nil
}

// innerText pulls result.content[0].text out of a JSON-RPC tool response.
func innerText(rpc map[string]any) string {
	result, _ := rpc["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

// parseMCPResponse handles both plain-JSON and SSE-framed (`data: {...}`)
// MCP responses. A streamed answer can carry notifications (progress,
// logging) ahead of the response to the request; the frame with a `result`
// or `error` is the answer, the last frame the fallback.
func parseMCPResponse(raw []byte) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	var out map[string]any
	if json.Unmarshal(trimmed, &out) == nil {
		return out, nil
	}
	var last map[string]any
	for line := range strings.SplitSeq(string(trimmed), "\n") {
		line = strings.TrimSpace(line)
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var frame map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &frame) != nil {
			continue
		}
		if _, isResult := frame["result"]; isResult {
			return frame, nil
		}
		if _, isError := frame["error"]; isError {
			return frame, nil
		}
		last = frame
	}
	if last != nil {
		return last, nil
	}
	return nil, fmt.Errorf("neither JSON nor SSE data frame")
}

// proveDownstreamIdentity shows that mcp-kubernetes acts as the signed-in
// user: muster forwards each session's Dex id_token (auth.forwardToken), the
// server validates it and presents it to the kind apiserver
// (enableDownstreamOAuth), so the user's RBAC — not a ServiceAccount's —
// decides. Listing kube-system Secrets separates the lab's groups cleanly: the
// `view` ClusterRole bound to oidc:viewers excludes Secrets, cluster-admin
// (oidc:platform-admins) reads them. A shared ServiceAccount would answer both
// users alike.
func proveDownstreamIdentity(cfg *config.Config, toolPrefix string) error {
	admin, viewer, err := identityProofUsers(cfg)
	if err != nil {
		return err
	}
	args := kubernetesArgs(cfg, map[string]any{"resourceType": "secrets", "namespace": "kube-system"})
	for _, tc := range []struct {
		user      *config.User
		allowed   bool
		expecting string
	}{
		{admin, true, "allowed (cluster-admin via oidc:platform-admins)"},
		{viewer, false, "forbidden (view role via oidc:viewers excludes Secrets)"},
	} {
		step("Listing kube-system Secrets through muster as %s with x-user-id forged to %s — expecting %s", tc.user.Email, forgedIdentity, tc.expecting)
		token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
			tc.user.Email, tc.user.Password, musterLoginScopes)
		if err != nil {
			return err
		}
		session, err := openMusterSession(cfg, token, "platform-test-identity")
		if err != nil {
			return err
		}
		// muster and mcp-kubernetes read the bearer, never this header: the
		// apiserver must still see the token's user.
		session.setHeader(userIDHeader, forgedIdentity)
		text, err := session.callServerTool(toolPrefix+"list", args)
		switch {
		case tc.allowed && err != nil:
			return fmt.Errorf("%s could not list kube-system Secrets through %slist: %w", tc.user.Email, toolPrefix, err)
		case tc.allowed:
			note("%s: listed (%s)", tc.user.Email, excerpt(text, 80))
		case err == nil:
			return fmt.Errorf("%s listed kube-system Secrets through %slist although the view role excludes them — mcp-kubernetes is not acting as the caller (ServiceAccount fallback?): %.200s", tc.user.Email, toolPrefix, text)
		case !strings.Contains(strings.ToLower(err.Error()), "forbidden"):
			return fmt.Errorf("%s: wanted the apiserver's Forbidden for kube-system Secrets, got: %w", tc.user.Email, err)
		case !strings.Contains(err.Error(), `User "oidc:`+tc.user.Email+`"`):
			return fmt.Errorf("%s: Forbidden, but not under the user's own name — the apiserver saw someone else (the forged header?): %w", tc.user.Email, err)
		default:
			note("%s: %s", tc.user.Email, excerpt(err.Error(), 160))
		}
	}
	return nil
}

// identityProofUsers are the two users the identity proofs act as: one of
// platform-admins (cluster-admin) and one of viewers (the view role), whom
// the same tool must answer differently. A configuration without both has
// no proof to run and fails it, naming the need: `agentlab configure
// --defaults` writes the lab's three users.
func identityProofUsers(cfg *config.Config) (admin, viewer *config.User, err error) {
	admin, viewer = cfg.FindUserInGroup("platform-admins"), cfg.FindUserInGroup("viewers")
	if admin == nil || viewer == nil {
		return nil, nil, fmt.Errorf("the identity proof needs one platform-admins and one viewers user in %s (`agentlab configure --defaults` writes them)", config.File)
	}
	return admin, viewer, nil
}

// serviceMonitorResource is the Prometheus operator's ServiceMonitor as a
// resource argument.
const serviceMonitorResource = "servicemonitors.monitoring.coreos.com"

// kagentControllerMonitored reports whether a ServiceMonitor for the kagent
// controller exists in the kagent namespace — the connectivity chart's
// (agent-platform-connectivity-kagent-controller) or the kagent chart's own.
// kagent main's controller serves no Prometheus listener; a lab that renders
// no monitor for it has no kagent target to expect, one that does expects it
// scraped. A read that fails is an error, never "no monitor".
func kagentControllerMonitored() (bool, error) {
	gvr, err := gvrFor(serviceMonitorResource)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	monitors, err := listObjects(ctx, gvr, kagentNamespace, "")
	if err != nil {
		return false, fmt.Errorf("listing the ServiceMonitors in %s: %w", kagentNamespace, err)
	}
	for _, m := range monitors {
		if strings.Contains(m.GetName(), "kagent-controller") {
			return true, nil
		}
	}
	return false, nil
}
