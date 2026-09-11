package lab

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The A2A proof: turns over native gRPC through the edge, as the surfaces
// drive them.
//
// Swarmgeist and the Dev Portal's backend speak native gRPC (HTTP/2) to the
// controller through the agentgateway edge — A2A v1 for the turns, kagent's
// services for the roster and the conversations — as the signed-in person.
// This proof drives the same calls with the same metadata on the lab's
// public TLS hostname and asserts what the surfaces rely on: the route and
// its JWT policy exist and are Accepted; a call without a token is refused at
// the edge (Unauthenticated, HTTP 401); the identity is the verified token's
// — a forged x-user-id beside it is replaced; ListAgentTemplates lists the
// proof's agent with its readiness and the display-name and icon-url
// annotations; CreateAgentInstance is idempotent on request_id;
// SendStreamingMessage streams an answer and muster logs the turn's tool
// calls under the person; a tool bound with requireApproval pauses the task
// at input-required with a tool_approval_request, the person's approval
// resumes it to completed and a rejection ends it without the call;
// CancelTask on a running turn ends it server-side (GetTask reports it
// canceled) and the instance takes a following turn. The fixture is one
// agent of the Generic chart (the direct writer, muster.requireApproval on
// its read-only toolset). Leaves nothing behind: the instances over gRPC,
// the agent's release and render on the cluster.

// Names of what the proof creates; deleted by the same run.
const (
	a2aTestAgent       = "agentlab-a2a-test"
	a2aTestDisplayName = "agentlab a2a-test"
	a2aTestIconURL     = "https://avatars.127.0.0.1.nip.io/agentlab-a2a-test.svg"
	a2aTestPrompt      = "You are a terse assistant of the agentlab A2A proof. Use your tools when asked about the cluster. Answer in one short line."
	// a2aTestForgedUser is the x-user-id the identity step sends beside a
	// valid token: the edge must replace it.
	a2aTestForgedUser = "mallory@lab.local"
)

// The turns the proof drives.
const (
	// pongPrompt is a turn no tool is needed for.
	pongPrompt = "Reply with exactly the word pong and nothing else."
	// a2aToolPrompt makes the agent call a tool through muster — the call
	// the HITL binding pauses on.
	a2aToolPrompt = "How many namespaces does the cluster have? Use your tools once and answer with the count."
	// a2aLongPrompt keeps the model writing for long enough to cancel the
	// task while it is working, without any tool call (the binding would
	// pause one).
	a2aLongPrompt = "Write a detailed essay of at least 1500 words on the history of container networking in Kubernetes: CNI, kube-proxy, network policies, service meshes, gateway API. Section by section, no summary, do not stop early."
	// hitlDecisionRounds bounds the decisions one task may ask for (the Go
	// ADK pauses on filter_tools first, then on call_tool).
	hitlDecisionRounds = 4
	// cancelAfterWorking is how long after the task reports working the
	// proof cancels it when no artifact arrived earlier.
	cancelAfterWorking = 3 * time.Second
	// a2aDeclineReason is what the person says when rejecting the tool call.
	a2aDeclineReason = "declined by the agentlab A2A proof"
)

// The Gateway API and agentgateway resources the controller's route is made of.
const (
	grpcRouteResource          = "grpcroutes.gateway.networking.k8s.io"
	agentgatewayPolicyResource = "agentgatewaypolicies.agentgateway.dev"
)

// A2ATestOptions are the flags of `agentlab a2a-test`.
type A2ATestOptions struct {
	// ReadyTimeout bounds the fixture agent's golden boot.
	ReadyTimeout time.Duration
}

// A2ATest is the headless proof described at the top of this file.
func A2ATest(cfg *config.Config, email string, opts A2ATestOptions) error {
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
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = agentReadyTimeout
	}
	hostPort, _, err := kagentTarget(cfg)
	if err != nil {
		return err
	}

	step("The controller's route on the edge: GRPCRoute %s and its JWT policy %s", kagentControllerRoute, kagentControllerJWTPolicy)
	route, err := readControllerRoute()
	if err != nil {
		return err
	}
	for _, line := range route.lines() {
		note("%s", line)
	}

	step("Logging in to Dex as %s", user.Email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	claims, err := decodeJWTClaims(token)
	if err != nil {
		return err
	}
	subject, _ := claims["sub"].(string)
	note("got an id_token (sub %.12s…)", subject)

	step("No token: the edge refuses the gRPC call at %s", hostPort)
	if err := proveEdgeRefusesWithoutToken(cfg); err != nil {
		return err
	}

	step("The identity is the verified token's: a forged %s=%s beside it is replaced by the edge", userIDHeader, a2aTestForgedUser)
	forged, err := dialKagentAPI(cfg, token)
	if err != nil {
		return err
	}
	defer forged.close()
	forged.extra = metadata.Pairs(userIDHeader, a2aTestForgedUser)
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	who, err := forged.currentUser(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("GetCurrentUser with a valid token and a forged %s: %w", userIDHeader, err)
	}
	if principal := principalOf(who); principal != user.Email {
		return fmt.Errorf("GetCurrentUser attributes the call to %q (claims %v), wanted the token's %s — the edge did not replace the forged %s", principal, who, user.Email, userIDHeader)
	}
	note("GetCurrentUser → %s (claims %v)", principalOf(who), who)

	api, err := dialKagentAPI(cfg, token)
	if err != nil {
		return err
	}
	defer api.close()
	instances := &instanceSet{api: api}
	defer instances.removeAll()

	step("The fixture: agent %s of the Generic chart — toolset [%s], muster.requireApproval — Ready on Harness %s", a2aTestAgent, presetReadOnly, kagentHarness)
	if agentExists(a2aTestAgent) {
		note("removing the leftover agent %s from an earlier run", a2aTestAgent)
		if err := removeAgent(a2aTestAgent); err != nil {
			return err
		}
	}
	defer func() {
		if err := removeAgent(a2aTestAgent); err != nil {
			note("cleanup: %v", err)
		}
	}()
	spec := agentSpec{
		Name: a2aTestAgent, ModelConfig: defaultModelConfig, DisplayName: a2aTestDisplayName, IconURL: a2aTestIconURL,
		Description:   "Throwaway agent of `agentlab a2a-test`; deleted by the same run.",
		SystemMessage: a2aTestPrompt, Toolset: []string{presetReadOnly}, RequireApproval: true,
	}
	bootStarted := time.Now()
	template, _, err := readyAgent(helmReleaseWriter{}, spec, opts.ReadyTimeout)
	if err != nil {
		return err
	}
	if h := template.harness(kagentHarness); h != nil {
		note("Ready after %s: revision %.12s", time.Since(bootStarted).Round(time.Second), h.LatestSuccessfulRevision)
	}
	if !template.requiresApproval(a2aTestAgent) {
		return fmt.Errorf("AgentTemplate %s binds RemoteMCPServer %s without requireApproval: the chart did not render muster.requireApproval (chart %s?)", a2aTestAgent, a2aTestAgent, template.chartLabel())
	}
	note("spec.tools[0].mcp = {server %s, requireApproval true}", a2aTestAgent)

	step("ListAgentTemplates through the edge lists %s Ready on %s with its display name and icon", a2aTestAgent, kagentHarness)
	ctx, cancel = context.WithTimeout(context.Background(), kubeReadTimeout)
	templates, err := api.listTemplates(ctx)
	cancel()
	if err != nil {
		return err
	}
	listing, err := findListing(templates, a2aTestAgent)
	if err != nil {
		return err
	}
	if listing.Unavailable != "" || listing.Harness != kagentHarness {
		return fmt.Errorf("ListAgentTemplates lists %s on Harness %q as unavailable: %s", a2aTestAgent, listing.Harness, listing.Unavailable)
	}
	if listing.DisplayName != a2aTestDisplayName || listing.IconURL != a2aTestIconURL {
		return fmt.Errorf("ListAgentTemplates lists %s as %q with icon %q, wanted %q and %q (the annotations %s / %s)", a2aTestAgent, listing.DisplayName, listing.IconURL, a2aTestDisplayName, a2aTestIconURL, displayNameAnnotation, iconURLAnnotation)
	}
	note("%d templates; %s: display name %q, icon %s, Harness %s, selectable", len(templates), listing.Name, listing.DisplayName, listing.IconURL, listing.Harness)

	step("CreateAgentInstance as %s, idempotent on request_id", user.Email)
	requestID := uuid.NewString()
	ctx, cancel = context.WithTimeout(context.Background(), kagentTurnTimeout)
	instance, err := api.createInstance(ctx, a2aTestAgent, requestID)
	if err != nil {
		cancel()
		return err
	}
	instances.add(instance.GetId())
	again, err := api.createInstance(ctx, a2aTestAgent, requestID)
	cancel()
	if err != nil {
		return fmt.Errorf("the second CreateAgentInstance with request_id %s: %w", requestID, err)
	}
	if again.GetId() != instance.GetId() {
		instances.add(again.GetId())
		return fmt.Errorf("CreateAgentInstance with the same request_id created a second instance %s next to %s", again.GetId(), instance.GetId())
	}
	if instance.GetCreator() != user.Email {
		return fmt.Errorf("AgentInstance %s has creator %q, wanted %s", instance.GetId(), instance.GetCreator(), user.Email)
	}
	note("AgentInstance %s (creator %s, %s); the same request_id answers the same instance", instance.GetId(), instance.GetCreator(), instanceState(instance))

	step("SendStreamingMessage as %s (the HITL extension requested): a turn without tools streams its answer", user.Email)
	pong, err := api.completedTurnOnce(instance.GetId(), pongPrompt)
	if err != nil {
		// The first resume of a cold worker can run into Substrate's
		// un-retried ResumeActor deadline and wedge the instance; said and
		// tried once more on a fresh instance, never silently.
		note("the first turn failed (%s) — one retry on a fresh instance, a cold worker's ResumeActor deadline is not retried by Substrate", excerpt(err.Error(), 200))
		ctx, cancel = context.WithTimeout(context.Background(), kagentTurnTimeout)
		instance, err = api.createInstance(ctx, a2aTestAgent, uuid.NewString())
		cancel()
		if err != nil {
			return err
		}
		instances.add(instance.GetId())
		if pong, err = api.completedTurnOnce(instance.GetId(), pongPrompt); err != nil {
			return err
		}
	}
	if !strings.Contains(strings.ToLower(pong.text()), "pong") {
		return fmt.Errorf("the agent answered %q, not pong", excerpt(pong.text(), 200))
	}
	note("task %s: %s in %s, %d events, answered %q", pong.taskID, pong.statesString(), pong.elapsed.Round(time.Millisecond), pong.events, excerpt(pong.text(), 60))

	step("HITL: a tool call pauses the task at input-required with a tool_approval_request; the approval resumes it; muster logs the call under %s", user.Email)
	hitlStarted := time.Now()
	paused, err := api.turnOn(instance.GetId(), userMessage(a2aToolPrompt))
	if err != nil {
		return err
	}
	if paused.state() != a2a.TaskStateInputRequired || paused.approval == nil {
		return fmt.Errorf("the tool turn did not pause for approval: task %s ended %s (%s) with no tool_approval_request: %s", paused.taskID, stateName(paused.state()), paused.statesString(), excerpt(paused.text(), 300))
	}
	note("task %s paused: %s; tool_approval_request for %s (hint %q)", paused.taskID, paused.statesString(), strings.Join(paused.approval.toolNames(), ", "), excerpt(paused.approval.Hint, 100))
	approved, rounds, err := api.decideUntilSettled(instance.GetId(), paused, true, "")
	if err != nil {
		return err
	}
	if approved.state() != a2a.TaskStateCompleted {
		return fmt.Errorf("after %d approval(s) task %s ended %s (%s): %s", rounds, approved.taskID, stateName(approved.state()), approved.statesString(), excerpt(approved.text(), 300))
	}
	note("%d approval(s) → %s in %s; answered %q", rounds, stateName(approved.state()), time.Since(hitlStarted).Round(time.Millisecond), excerpt(approved.text(), 100))
	calls, err := musterCallsSince(hitlStarted, user.Email, subject)
	if err != nil {
		return err
	}
	if calls.accepted == 0 || calls.calls == 0 {
		return fmt.Errorf("muster's log since %s carries %d forwarded_id_token_accepted for %s and %d tools/call by that subject — the Harness did not reach muster as the person", hitlStarted.UTC().Format(time.RFC3339), calls.accepted, user.Email, calls.calls)
	}
	note("muster: %d × forwarded_id_token_accepted email=%s, %d × tools/call by the token's subject", calls.accepted, user.Email, calls.calls)

	step("HITL: a rejected tool_approval_request ends the task without the tool call")
	declineStarted := time.Now()
	paused, err = api.turnOn(instance.GetId(), userMessage(a2aToolPrompt))
	if err != nil {
		return err
	}
	if paused.state() != a2a.TaskStateInputRequired || paused.approval == nil {
		return fmt.Errorf("the second tool turn did not pause for approval: task %s ended %s (%s)", paused.taskID, stateName(paused.state()), paused.statesString())
	}
	declined, rounds, err := api.decideUntilSettled(instance.GetId(), paused, false, a2aDeclineReason)
	if err != nil {
		return err
	}
	if !declined.state().Terminal() {
		return fmt.Errorf("after %d rejection(s) task %s is still %s (%s)", rounds, declined.taskID, stateName(declined.state()), declined.statesString())
	}
	calls, err = musterCallsSince(declineStarted, user.Email, subject)
	if err != nil {
		return err
	}
	if calls.calls != 0 {
		return fmt.Errorf("muster logged %d tools/call by %s during the declined task — the rejected tool ran anyway", calls.calls, user.Email)
	}
	note("%d rejection(s) → %s (%s); no tools/call by %s reached muster; the agent said %q", rounds, stateName(declined.state()), declined.statesString(), user.Email, excerpt(declined.text(), 100))

	step("CancelTask on a running turn ends it server-side; the instance takes a following turn")
	canceled, err := api.cancelRunningTurn(instance.GetId(), a2aLongPrompt)
	if err != nil {
		return err
	}
	note("task %s: %s; CancelTask after %s answered %s; the stream ended %s later; GetTask → %s", canceled.taskID, canceled.statesString(), canceled.canceledAfter.Round(time.Millisecond), stateName(canceled.cancelState), canceled.streamEndedAfter.Round(time.Millisecond), stateName(canceled.finalState))
	after, err := api.completedTurnOnce(instance.GetId(), pongPrompt)
	if err != nil {
		return fmt.Errorf("the turn after the cancel: %w", err)
	}
	note("the following turn: task %s %s, answered %q", after.taskID, after.statesString(), excerpt(after.text(), 60))

	step("DeleteAgentInstance; ListAgentInstances lists none of %s's", a2aTestAgent)
	if err := instances.removeAllNow(); err != nil {
		return err
	}
	ctx, cancel = context.WithTimeout(context.Background(), kubeReadTimeout)
	left, err := api.listInstances(ctx)
	cancel()
	if err != nil {
		return err
	}
	var mine []string
	for _, inst := range left {
		if inst.GetAgentTemplate().GetName() == a2aTestAgent {
			mine = append(mine, inst.GetId())
		}
	}
	if len(mine) > 0 {
		return fmt.Errorf("ListAgentInstances still lists %v of %s after the delete", mine, a2aTestAgent)
	}
	note("%d instances of %s left (of %d the controller keeps for %s)", len(mine), a2aTestAgent, len(left), user.Email)

	fmt.Println()
	fmt.Printf("PASS: GRPCRoute %s (Accepted, ResolvedRefs, %d services) and AgentgatewayPolicy %s (Accepted) carry the controller on the edge; a call without a token is refused there\n", kagentControllerRoute, len(route.services), kagentControllerJWTPolicy)
	fmt.Printf("PASS: native gRPC through %s as %s — the identity is the verified token's (a forged %s replaced), CreateAgentInstance idempotent on request_id, SendStreamingMessage with the HITL extension streamed %s\n", hostPort, user.Email, userIDHeader, excerpt(pong.text(), 30))
	fmt.Printf("PASS: ListAgentTemplates lists %s Ready on %s with %s and %s as Swarmgeist reads them\n", a2aTestAgent, kagentHarness, displayNameAnnotation, iconURLAnnotation)
	fmt.Printf("PASS: HITL — the muster binding with requireApproval paused the task at input-required (tool_approval_request), the approval resumed it to completed with muster logging the call under %s, a rejection ended it without the call\n", user.Email)
	fmt.Printf("PASS: CancelTask ended the running task server-side (GetTask → %s) and the instance answered a following turn; nothing left behind\n", stateName(canceled.finalState))
	return nil
}

// controllerRoute is what the proof reads off the connectivity chart's route
// objects in front of the controller.
type controllerRoute struct {
	services         []string
	routeConditions  map[string]string
	policyConditions map[string]string
	userIDClaim      string
}

// readControllerRoute reads the GRPCRoute and the AgentgatewayPolicy and
// asserts they exist, are Accepted and carry what the surfaces need: the
// A2A service and kagent's services matched, the token's claim rewritten
// into x-user-id.
func readControllerRoute() (*controllerRoute, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	routeGVR, err := gvrFor(grpcRouteResource)
	if err != nil {
		return nil, fmt.Errorf("the cluster serves no GRPCRoutes: %w", err)
	}
	route, err := getObject(ctx, routeGVR, platformNamespace, kagentControllerRoute)
	if err != nil {
		return nil, fmt.Errorf("the controller's GRPCRoute is missing (connectivity ≥ 4.0 renders it): %w", err)
	}
	r := &controllerRoute{routeConditions: map[string]string{}, policyConditions: map[string]string{}}
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	for _, rule := range rules {
		matches, _, _ := unstructured.NestedSlice(asMap(rule), "matches")
		for _, m := range matches {
			if svc, _, _ := unstructured.NestedString(asMap(m), "method", "service"); svc != "" && !slices.Contains(r.services, svc) {
				r.services = append(r.services, svc)
			}
		}
	}
	slices.Sort(r.services)
	for _, want := range []string{a2aService, "kagent.api.v1alpha1.AgentInstanceService", "kagent.api.v1alpha1.AgentTemplateService", "kagent.api.v1alpha1.SystemService"} {
		if !slices.Contains(r.services, want) {
			return nil, fmt.Errorf("GRPCRoute %s does not match service %s (it matches %v)", kagentControllerRoute, want, r.services)
		}
	}
	parents, _, _ := unstructured.NestedSlice(route.Object, crStatus, "parents")
	r.routeConditions = ancestorConditions(parents)
	for _, condType := range []string{conditionAccepted, conditionResolvedRefs} {
		if r.routeConditions[condType] != conditionTrue {
			return nil, fmt.Errorf("GRPCRoute %s: %s=%q, wanted True (status.parents: %v)", kagentControllerRoute, condType, r.routeConditions[condType], parents)
		}
	}
	policyGVR, err := gvrFor(agentgatewayPolicyResource)
	if err != nil {
		return nil, fmt.Errorf("the cluster serves no AgentgatewayPolicies: %w", err)
	}
	policy, err := getObject(ctx, policyGVR, platformNamespace, kagentControllerJWTPolicy)
	if err != nil {
		return nil, fmt.Errorf("the controller route's JWT policy is missing (kagent.controllerRoute.jwtAuthentication.enabled): %w", err)
	}
	targets, _, _ := unstructured.NestedSlice(policy.Object, "spec", "targetRefs")
	var targetNames []string
	for _, t := range targets {
		name, _, _ := unstructured.NestedString(asMap(t), nameKey)
		targetNames = append(targetNames, name)
	}
	if !slices.Contains(targetNames, kagentControllerRoute) {
		return nil, fmt.Errorf("AgentgatewayPolicy %s targets %v, not the GRPCRoute %s", kagentControllerJWTPolicy, targetNames, kagentControllerRoute)
	}
	ancestors, _, _ := unstructured.NestedSlice(policy.Object, crStatus, "ancestors")
	r.policyConditions = ancestorConditions(ancestors)
	if r.policyConditions[conditionAccepted] != conditionTrue {
		return nil, fmt.Errorf("AgentgatewayPolicy %s: Accepted=%q, wanted True (status.ancestors: %v)", kagentControllerJWTPolicy, r.policyConditions[conditionAccepted], ancestors)
	}
	set, _, _ := unstructured.NestedSlice(policy.Object, "spec", "traffic", "transformation", "request", "set")
	for _, h := range set {
		m := asMap(h)
		if name, _, _ := unstructured.NestedString(m, nameKey); name == userIDHeader {
			r.userIDClaim, _, _ = unstructured.NestedString(m, "value")
		}
	}
	if r.userIDClaim == "" {
		return nil, fmt.Errorf("AgentgatewayPolicy %s sets no %s from the token (spec.traffic.transformation.request.set %v) — the controller would trust the caller's header", kagentControllerJWTPolicy, userIDHeader, set)
	}
	return r, nil
}

// ancestorConditions folds the conditions of a route's status.parents[] or a
// policy's status.ancestors[] into type → status (the last wins).
func ancestorConditions(entries []any) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		conditions, _, _ := unstructured.NestedSlice(asMap(e), crConditions)
		for _, c := range conditions {
			m := asMap(c)
			condType, _, _ := unstructured.NestedString(m, fieldTypeKey)
			condStatus, _, _ := unstructured.NestedString(m, crStatus)
			out[condType] = condStatus
		}
	}
	return out
}

// asMap is one element of an unstructured slice as the map it is, or an
// empty map for anything else (a read that never panics on a shape).
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// lines is the route the way the evidence quotes it.
func (r *controllerRoute) lines() []string {
	return []string{
		fmt.Sprintf("GRPCRoute %s: Accepted=%s ResolvedRefs=%s; services %s", kagentControllerRoute, r.routeConditions[conditionAccepted], r.routeConditions[conditionResolvedRefs], strings.Join(r.services, ", ")),
		fmt.Sprintf("AgentgatewayPolicy %s: Accepted=%s; sets %s = %s", kagentControllerJWTPolicy, r.policyConditions[conditionAccepted], userIDHeader, r.userIDClaim),
	}
}

// proveEdgeRefusesWithoutToken makes the no-token call twice: as gRPC (the
// client must see Unauthenticated) and as a raw HTTP/2 POST of the same
// method on the lab's TLS transport (the edge must answer 401 before any
// controller code runs — the HTTP status the gRPC client folds into
// Unauthenticated).
func proveEdgeRefusesWithoutToken(cfg *config.Config) error {
	nobody, err := dialKagentAPI(cfg, "")
	if err != nil {
		return err
	}
	defer nobody.close()
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	_, err = nobody.currentUser(ctx)
	if !isUnauthenticated(err) {
		return fmt.Errorf("GetCurrentUser without a token answered %s (%v), wanted Unauthenticated from the edge", status.Code(err), err)
	}
	note("gRPC without a token: %s — %s", codes.Unauthenticated, excerpt(status.Convert(err).Message(), 120))
	httpStatus, proto, err := edgeHTTPStatusWithoutToken(cfg)
	if err != nil {
		return err
	}
	if httpStatus != http.StatusUnauthorized {
		return fmt.Errorf("a raw %s POST of %s/GetCurrentUser without a token answered HTTP %d, wanted 401 from the edge's JWT policy", proto, apiSystemService, httpStatus)
	}
	note("%s POST %s/GetCurrentUser without a token: HTTP %d", proto, apiSystemService, httpStatus)
	return nil
}

// apiSystemService is the kagent service the raw probe posts to.
const apiSystemService = "kagent.api.v1alpha1.SystemService"

// edgeHTTPStatusWithoutToken POSTs one empty gRPC frame to the controller's
// GetCurrentUser through the edge over HTTP/2 without a token and returns
// the HTTP status and protocol the edge answered with.
func edgeHTTPStatusWithoutToken(cfg *config.Config) (int, string, error) {
	pool, err := labCertPool()
	if err != nil {
		return 0, "", err
	}
	client := &http.Client{Timeout: kubeReadTimeout, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true,
		DialContext:       dialLab,
	}}
	// One empty length-prefixed gRPC message: the request has no fields.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, cfg.AgentgatewayBaseURL()+"/"+apiSystemService+"/GetCurrentUser", bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("the raw HTTP/2 probe through the edge: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Proto, nil
}

// findListing is the roster entry of one template among the listed ones.
func findListing(templates []*apiv1alpha1.AgentTemplate, name string) (templateListing, error) {
	var names []string
	for _, t := range templates {
		l := listingOf(t)
		if l.Name == name {
			return l, nil
		}
		names = append(names, l.Name)
	}
	return templateListing{}, fmt.Errorf("ListAgentTemplates does not list %s (it lists %v)", name, names)
}

// timedTurn is a turn with how long its stream took.
type timedTurn struct {
	*turn
	elapsed time.Duration
}

// turnOn drives one turn on the instance, bounded by kagentTurnTimeout.
func (a *kagentAPI) turnOn(instanceID string, msg *a2a.Message) (*timedTurn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	defer cancel()
	started := time.Now()
	t, err := a.turn(ctx, instanceID, msg)
	if err != nil {
		return nil, err
	}
	return &timedTurn{turn: t, elapsed: time.Since(started)}, nil
}

// completedTurnOnce drives one turn that must end completed.
func (a *kagentAPI) completedTurnOnce(instanceID, prompt string) (*timedTurn, error) {
	t, err := a.turnOn(instanceID, userMessage(prompt))
	if err != nil {
		return nil, err
	}
	if _, err := t.completedText(); err != nil {
		return nil, err
	}
	return t, nil
}

// decideUntilSettled answers a paused task's requests with the same decision
// until the task leaves input-required (the Go ADK may pause once per tool
// call), bounded by hitlDecisionRounds; returns the settled turn and the
// number of decisions.
func (a *kagentAPI) decideUntilSettled(instanceID string, paused *timedTurn, approve bool, reason string) (*turn, int, error) {
	current := paused.turn
	rounds := 0
	for current.state() == a2a.TaskStateInputRequired {
		if current.approval == nil {
			return current, rounds, fmt.Errorf("task %s paused at input-required without a tool_approval_request (%s): %s", current.taskID, current.statesString(), excerpt(current.statusText, 200))
		}
		if rounds == hitlDecisionRounds {
			return current, rounds, fmt.Errorf("task %s still asks for a decision after %d (%s)", current.taskID, rounds, strings.Join(current.approval.toolNames(), ", "))
		}
		rounds++
		note("decision %d: %s %s", rounds, map[bool]string{true: "approve", false: "reject"}[approve], strings.Join(current.approval.toolNames(), ", "))
		ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
		next, err := a.decide(ctx, instanceID, current, approve, reason)
		cancel()
		if err != nil {
			return current, rounds, err
		}
		current = next
	}
	return current, rounds, nil
}

// canceledTurn is the evidence of the cancel step.
type canceledTurn struct {
	*turn
	canceledAfter    time.Duration
	cancelState      a2a.TaskState
	streamEndedAfter time.Duration
	finalState       a2a.TaskState
}

// streamed is one event of a stream consumed on its own goroutine.
type streamed struct {
	event a2a.Event
	err   error
}

// cancelRunningTurn starts a long turn, cancels the task once it is working
// (on its first artifact, or cancelAfterWorking after the working state),
// drains the stream to its end and reads the task back: CancelTask's answer
// and GetTask must both say canceled.
func (a *kagentAPI) cancelRunningTurn(instanceID, prompt string) (*canceledTurn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	defer cancel()
	events := make(chan streamed)
	go func() {
		// The consumer may leave early on an error of its own; the context
		// it cancels on the way out ends the stream and this send alike.
		defer close(events)
		for ev, err := range a.stream(ctx, instanceID, userMessage(prompt)) {
			select {
			case events <- streamed{ev, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	result := &canceledTurn{turn: &turn{}}
	started := time.Now()
	var working <-chan time.Time
	canceled := false
	cancelNow := func() error {
		task, err := a.cancelTask(ctx, instanceID, result.taskID)
		if err != nil {
			return err
		}
		result.canceledAfter, result.cancelState, canceled = time.Since(started), task.Status.State, true
		return nil
	}
	var streamErr error
	for events != nil {
		select {
		case s, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if s.err != nil {
				streamErr = s.err
				continue
			}
			result.observe(s.event)
			if canceled || result.taskID == "" {
				continue
			}
			_, artifact := s.event.(*a2a.TaskArtifactUpdateEvent)
			switch {
			case artifact && slices.Contains(result.states, a2a.TaskStateWorking):
				if err := cancelNow(); err != nil {
					return nil, err
				}
			case working == nil && result.state() == a2a.TaskStateWorking:
				working = time.After(cancelAfterWorking)
			}
		case <-working:
			working = nil
			if !canceled && result.taskID != "" {
				if err := cancelNow(); err != nil {
					return nil, err
				}
			}
		case <-ctx.Done():
			return nil, fmt.Errorf("the long turn's stream did not end within %s after the cancel (%s)", kagentTurnTimeout, result.statesString())
		}
	}
	result.streamEndedAfter = time.Since(started) - result.canceledAfter
	if !canceled {
		return nil, fmt.Errorf("the long turn ended %s (%s) before it could be canceled: %s", stateName(result.state()), result.statesString(), excerpt(result.text(), 200))
	}
	if streamErr != nil {
		note("the stream ended with %s after the cancel: %s", status.Code(streamErr), excerpt(streamErr.Error(), 160))
	}
	if result.cancelState != a2a.TaskStateCanceled {
		return nil, fmt.Errorf("CancelTask answered state %s, wanted canceled", stateName(result.cancelState))
	}
	task, err := a.getTask(ctx, instanceID, result.taskID)
	if err != nil {
		return nil, err
	}
	result.finalState = task.Status.State
	if result.finalState != a2a.TaskStateCanceled {
		return nil, fmt.Errorf("GetTask %s reports %s after the cancel, wanted canceled", result.taskID, stateName(result.finalState))
	}
	return result, nil
}

// instanceSet tracks the instances a run created so every exit path deletes
// them.
type instanceSet struct {
	api *kagentAPI
	ids []string
}

func (s *instanceSet) add(id string) {
	if !slices.Contains(s.ids, id) {
		s.ids = append(s.ids, id)
	}
}

// removeAllNow deletes every tracked instance and reports what refused.
func (s *instanceSet) removeAllNow() error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout*2)
	defer cancel()
	var errs []string
	for _, id := range s.ids {
		if err := s.api.deleteInstance(ctx, id); err != nil {
			errs = append(errs, err.Error())
		}
	}
	s.ids = nil
	if len(errs) > 0 {
		return fmt.Errorf("deleting the instances: %s", strings.Join(errs, "; "))
	}
	return nil
}

// removeAll is removeAllNow on the cleanup paths: said, never fatal.
func (s *instanceSet) removeAll() {
	if err := s.removeAllNow(); err != nil {
		note("cleanup: %v", err)
	}
}

// musterCalls is what muster's log says about one person's tool calls in a
// window: the forwarded id_tokens it accepted for the email, and the
// tools/call requests by the token's subject.
type musterCalls struct {
	accepted, calls int
}

// musterCallsSince reads muster's log lines since the instant: muster's
// security audit `forwarded_id_token_accepted` carries the person's email,
// its `tools/call request` the Dex subject (muster prints a prefix of it).
func musterCallsSince(since time.Time, email, subject string) (musterCalls, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	window := time.Since(since) + time.Minute
	logs, err := podLogs(ctx, platformNamespace, "deploy/"+componentMuster, componentMuster, window)
	if err != nil {
		return musterCalls{}, fmt.Errorf("reading muster's log: %w", err)
	}
	return musterCallsIn(logs, since, email, subject), nil
}

// musterCallsIn counts the lines of a muster log for the person from the
// instant on.
func musterCallsIn(logs string, since time.Time, email, subject string) musterCalls {
	var calls musterCalls
	for _, line := range strings.Split(logs, "\n") {
		var entry struct {
			Time    time.Time `json:"time"`
			Msg     string    `json:"msg"`
			Tool    string    `json:"tool"`
			Subject string    `json:"subject"`
			Audit   struct {
				EventType string `json:"event_type"`
				Details   struct {
					Email string `json:"email"`
				} `json:"details"`
			} `json:"audit"`
		}
		start := strings.IndexByte(line, '{')
		if start < 0 || json.Unmarshal([]byte(line[start:]), &entry) != nil || entry.Time.Before(since) {
			continue
		}
		switch {
		case entry.Msg == "security_audit" && entry.Audit.EventType == "forwarded_id_token_accepted" && entry.Audit.Details.Email == email:
			calls.accepted++
		case entry.Msg == "tools/call request" && entry.Tool == "call_tool" && subjectMatches(entry.Subject, subject):
			calls.calls++
		}
	}
	return calls
}

// subjectMatches compares muster's logged subject — the whole value or a
// prefix ending in an ellipsis — with the token's.
func subjectMatches(logged, subject string) bool {
	if logged == "" || subject == "" {
		return false
	}
	prefix := strings.TrimSuffix(logged, "...")
	return strings.HasPrefix(subject, prefix)
}
