package lab

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	a2agrpc "github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// kagent API v2 on the wire, as the surfaces speak it.
//
// The controller serves one gRPC API — kagent.api.v1alpha1 for the control
// plane, lf.a2a.v1 for the turns — and no REST. Swarmgeist (klaus-gateway)
// and the Dev Portal's backend reach it as native gRPC over HTTP/2 through
// the agentgateway edge: the connectivity chart's GRPCRoute
// `kagent-controller` matches the five services, its AgentgatewayPolicy
// validates the person's Dex id_token (JWT Strict) and rewrites `x-user-id`
// from the token's email claim before the controller — in trusted-proxy mode
// — reads it; whatever `x-user-id` the caller sent is replaced. The proofs
// take the same path on the lab's TLS transport (the public hostname, the lab
// CA), so a turn proves the route and the policy as well as the controller.
//
// The metadata contract every call carries: `authorization: Bearer <Dex
// id_token>`; on the A2A calls exactly one `x-kagent-agent-instance-id`
// naming the AgentInstance that holds the conversation, and the
// human-in-the-loop extension requested (`A2A-Extensions`, gRPC metadata
// `a2a-extensions`) so a tool that needs approval pauses the task at
// input-required with a decidable request instead of a plain notice. The
// Harness re-emits the person's bearer on every MCP call, so muster logs the
// tool calls under the person. The messages are kagent's own protos
// (internal/kagent/gen, generated from the line's proto tree) and the A2A v1
// package the controller itself is built with.

const (
	// kagentHarness is the platform's Go ADK Harness: every AgentTemplate the
	// proofs create is labelled for it (harnessLabel) and every turn runs on it.
	kagentHarness = "kagent"
	// kagentTurnTimeout bounds one turn end to end: the instance create, a
	// cold resume from the golden snapshot, the model's answer with its tool
	// calls, the instance delete.
	kagentTurnTimeout = 180 * time.Second
	// templateRevisionTimeout bounds CreateAgentInstance's wait for a
	// template whose golden snapshot is still being taken (the controller
	// answers FailedPrecondition meanwhile; kagent's own e2e polls through it
	// the same way).
	templateRevisionTimeout = 60 * time.Second
	// instanceReadyTimeout bounds a freshly created instance's way to READY:
	// the controller converges it synchronously, so the poll only covers a
	// create that was interrupted and retried.
	instanceReadyTimeout = 90 * time.Second

	// The gRPC metadata of the contract (keys lower-case on the wire).
	authorizationMetadata = "authorization"
	instanceIDMetadata    = "x-kagent-agent-instance-id"
	// userIDHeader is the header the edge sets from the verified token's
	// email claim for the controller's trusted-proxy authenticator. The
	// identity proof forges it to show the edge replaces it.
	userIDHeader = "x-user-id"

	// hitlExtensionURI identifies kagent's human-in-the-loop A2A extension;
	// the payload types are the `type` field of the message metadata it
	// carries under that URI.
	hitlExtensionURI             = "https://kagent.dev/extensions/hitl/v1"
	hitlTypeToolApprovalRequest  = "tool_approval_request"
	hitlTypeToolApprovalResponse = "tool_approval_response"

	// The keys of the CR the controller hands back whole (StructuredObject)
	// and of the route objects' status, read as maps.
	crMetadata   = "metadata"
	crStatus     = "status"
	crConditions = "conditions"

	// kagentControllerRoute and kagentControllerJWTPolicy are the
	// connectivity chart's route objects in front of the controller: the
	// GRPCRoute on the edge and the policy that validates the token and
	// rewrites x-user-id.
	kagentControllerRoute     = "kagent-controller"
	kagentControllerJWTPolicy = "kagent-controller-jwt"
	// a2aService is the A2A v1 service the route matches next to the kagent
	// ones (lf.a2a.v1.A2AService).
	a2aService = "lf.a2a.v1.A2AService"
)

// kagentAPI is one person's client of the controller through the edge: one
// gRPC connection, the person's token on every call, the surfaces' metadata
// on the A2A ones. Zero-value token: no `authorization` at all — the way the
// refusal proof calls.
type kagentAPI struct {
	a2a       *a2aclient.Client
	templates apiv1alpha1.AgentTemplateServiceClient
	instances apiv1alpha1.AgentInstanceServiceClient
	system    apiv1alpha1.SystemServiceClient
	token     string
	// extra is metadata sent beside the token on every call — the identity
	// proof's forged x-user-id; nil for everyone else.
	extra     metadata.MD
	closeConn func() error
}

// kagentTarget is the controller's gRPC endpoint through the edge, derived
// from the agentgateway hostname the platform publishes (the same base URL
// Backstage's app-config carries): host:port and whether the hop is TLS.
func kagentTarget(cfg *config.Config) (hostPort string, useTLS bool, err error) {
	return kagentTargetOf(cfg.AgentgatewayBaseURL())
}

// kagentTargetOf is kagentTarget for a base URL.
func kagentTargetOf(base string) (hostPort string, useTLS bool, err error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", false, fmt.Errorf("the agentgateway base URL %q is not a URL", base)
	}
	useTLS = u.Scheme == "https"
	port := u.Port()
	if port == "" {
		port = "80"
		if useTLS {
			port = "443"
		}
	}
	return u.Hostname() + ":" + port, useTLS, nil
}

// dialKagentAPI connects to the controller through the edge as the person
// whose Dex id_token is given ("" for nobody): native gRPC over HTTP/2 on the
// lab's TLS transport — the lab CA trusted, the public hostname dialled on
// loopback like every lab client (dialLab) — the way klaus-gateway's
// `a2a.url: grpcs://agentgateway.<domain>:443` does. The connection is made on
// the first call; close releases it.
func dialKagentAPI(cfg *config.Config, token string) (*kagentAPI, error) {
	hostPort, useTLS, err := kagentTarget(cfg)
	if err != nil {
		return nil, err
	}
	creds := insecure.NewCredentials()
	if useTLS {
		pool, err := labCertPool()
		if err != nil {
			return nil, err
		}
		creds = credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	}
	// passthrough: the authority stays the public hostname (the TLS server
	// name and the route's host), the dialer takes it to loopback.
	conn, err := grpc.NewClient("passthrough:///"+hostPort, grpc.WithTransportCredentials(creds), grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
		return dialLab(ctx, "tcp", addr)
	}))
	if err != nil {
		return nil, fmt.Errorf("dialling the kagent controller through %s: %w", hostPort, err)
	}
	api, err := newKagentAPI(conn, token)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	api.closeConn = conn.Close
	return api, nil
}

// newKagentAPI is the client on an existing connection (the tests hand in a
// bufconn to a fake controller).
func newKagentAPI(conn grpc.ClientConnInterface, token string) (*kagentAPI, error) {
	transport := a2agrpc.NewGRPCTransportFromClient(a2apb.NewA2AServiceClient(conn))
	// NewFromEndpoints does no I/O: the factory hands back the transport on
	// the shared connection, so the only error is a configuration mismatch.
	a2aClient, err := a2aclient.NewFromEndpoints(context.Background(),
		[]*a2a.AgentInterface{{URL: "grpc://" + kagentControllerRoute, ProtocolBinding: a2a.TransportProtocolGRPC, ProtocolVersion: a2a.Version}},
		a2aclient.WithDefaultsDisabled(),
		a2aclient.WithTransport(a2a.TransportProtocolGRPC, a2aclient.TransportFactoryFn(
			func(context.Context, *a2a.AgentCard, *a2a.AgentInterface) (a2aclient.Transport, error) {
				return transport, nil
			})),
	)
	if err != nil {
		return nil, fmt.Errorf("building the A2A client: %w", err)
	}
	return &kagentAPI{
		a2a:       a2aClient,
		templates: apiv1alpha1.NewAgentTemplateServiceClient(conn),
		instances: apiv1alpha1.NewAgentInstanceServiceClient(conn),
		system:    apiv1alpha1.NewSystemServiceClient(conn),
		token:     token,
		closeConn: func() error { return nil },
	}, nil
}

// close releases the connection.
func (a *kagentAPI) close() {
	_ = a.closeConn()
}

// callCtx attaches the person's bearer (and the extra metadata) to a kagent
// service call.
func (a *kagentAPI) callCtx(ctx context.Context) context.Context {
	md := metadata.Join(a.extra)
	if a.token != "" {
		md.Set(authorizationMetadata, "Bearer "+a.token)
	}
	return metadata.NewOutgoingContext(ctx, md)
}

// a2aCtx attaches the A2A service parameters of a call on the instance: the
// person's bearer, exactly one instance route, the HITL extension request
// (and the extra metadata). The gRPC transport carries them as metadata.
func (a *kagentAPI) a2aCtx(ctx context.Context, instanceID string) context.Context {
	params := a2aclient.ServiceParams{
		instanceIDMetadata:     {instanceID},
		a2a.SvcParamExtensions: {hitlExtensionURI},
	}
	if a.token != "" {
		params[authorizationMetadata] = []string{"Bearer " + a.token}
	}
	for key, values := range a.extra {
		params[key] = values
	}
	return a2aclient.AttachServiceParams(ctx, params)
}

// currentUser is SystemService/GetCurrentUser: the claims the controller
// attributes the call to — in trusted-proxy mode what the edge put into
// x-user-id from the verified token.
func (a *kagentAPI) currentUser(ctx context.Context) (map[string]any, error) {
	resp, err := a.system.GetCurrentUser(a.callCtx(ctx), &apiv1alpha1.GetCurrentUserRequest{})
	if err != nil {
		return nil, fmt.Errorf("GetCurrentUser: %w", err)
	}
	return resp.GetClaims().AsMap(), nil
}

// principalOf is the person the controller's claims name: the email claim,
// else the subject.
func principalOf(claims map[string]any) string {
	if email, _ := claims["email"].(string); email != "" {
		return email
	}
	sub, _ := claims["sub"].(string)
	return sub
}

// version is SystemService/GetVersion: the controller's own build identity.
func (a *kagentAPI) version(ctx context.Context) (*apiv1alpha1.GetVersionResponse, error) {
	resp, err := a.system.GetVersion(a.callCtx(ctx), &apiv1alpha1.GetVersionRequest{})
	if err != nil {
		return nil, fmt.Errorf("GetVersion: %w", err)
	}
	return resp, nil
}

// substrateStatus is SystemService/GetSubstrateStatus for one namespace: the
// controller's view of Substrate — the WorkerPools, the ActorTemplates it
// wrote (phase Pending, Ready or Failed, the golden snapshot), the actors
// with their state and worker assignment, the pools' workers. What the
// portal's Substrate page shows; the person needs get on Substrate.
func (a *kagentAPI) substrateStatus(ctx context.Context, namespace string) (*apiv1alpha1.GetSubstrateStatusResponse, error) {
	resp, err := a.system.GetSubstrateStatus(a.callCtx(ctx), &apiv1alpha1.GetSubstrateStatusRequest{Namespace: namespace})
	if err != nil {
		return nil, fmt.Errorf("GetSubstrateStatus: %w", err)
	}
	return resp, nil
}

// listTemplates is AgentTemplateService/ListAgentTemplates of the kagent
// namespace — Swarmgeist's roster call.
func (a *kagentAPI) listTemplates(ctx context.Context) ([]*apiv1alpha1.AgentTemplate, error) {
	resp, err := a.templates.ListAgentTemplates(a.callCtx(ctx), &apiv1alpha1.ListAgentTemplatesRequest{Namespace: kagentNamespace})
	if err != nil {
		return nil, fmt.Errorf("ListAgentTemplates in %s: %w", kagentNamespace, err)
	}
	return resp.GetAgentTemplates(), nil
}

// templateListing is what a surface reads off one listed AgentTemplate: the
// technical name, the display name and icon from the chart's annotations,
// the Harness a conversation is created with, and why the template cannot
// start one (empty for a selectable template) — klaus-gateway's roster entry.
type templateListing struct {
	Name, Namespace, DisplayName, IconURL, Description, Harness, Unavailable string
}

// listingOf derives the roster entry from a listed template: the annotations
// from the CR's metadata, the readiness from status.harnesses[] of the
// admitting Harnesses the controller reports — a template is selectable when
// an admitting Harness reports Ready=True for it.
func listingOf(t *apiv1alpha1.AgentTemplate) templateListing {
	resource := t.GetResource().GetValue().AsMap()
	annotations, _ := nestedMap(resource, crMetadata)["annotations"].(map[string]any)
	l := templateListing{
		Name:        t.GetRef().GetName(),
		Namespace:   t.GetRef().GetNamespace(),
		DisplayName: stringOf(annotations[displayNameAnnotation]),
		IconURL:     stringOf(annotations[iconURLAnnotation]),
		Description: t.GetDescription(),
	}
	admitting := t.GetAdmittingHarnesses()
	if len(admitting) == 0 {
		l.Unavailable = "no Harness admits this AgentTemplate (it carries no admission label a platform Harness selects)"
		return l
	}
	harnesses, _ := nestedMap(resource, crStatus)["harnesses"].([]any)
	var firstReason string
	for _, name := range admitting {
		ready, reason := readyConditionOf(harnesses, name)
		if ready {
			l.Harness = name
			return l
		}
		if firstReason == "" {
			firstReason = reason
		}
	}
	l.Harness = admitting[0]
	l.Unavailable = fmt.Sprintf("Harness %s has not compiled a ready revision: %s", admitting[0], firstReason)
	return l
}

// readyConditionOf reads the Ready condition of one Harness's entry in
// status.harnesses[]: true, or false with the reason.
func readyConditionOf(harnesses []any, harness string) (bool, string) {
	for _, h := range harnesses {
		entry, ok := h.(map[string]any)
		if !ok || stringOf(entry["harness"]) != harness {
			continue
		}
		conditions, _ := entry[crConditions].([]any)
		for _, c := range conditions {
			cond, ok := c.(map[string]any)
			if !ok || stringOf(cond[fieldTypeKey]) != conditionReady {
				continue
			}
			if stringOf(cond[crStatus]) == conditionTrue {
				return true, ""
			}
			reason := stringOf(cond["message"])
			if reason == "" {
				reason = stringOf(cond["reason"])
			}
			return false, reason
		}
		return false, "no Ready condition reported yet"
	}
	return false, "no status reported yet"
}

func nestedMap(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// createInstance is AgentInstanceService/CreateAgentInstance for the person:
// one conversation of the AgentTemplate on the Go ADK Harness, both in the
// kagent namespace, keyed by requestID — the controller's create is
// idempotent per (creator, request_id), so a retried first turn gets the
// same instance back. A template whose golden snapshot is still being taken
// answers FailedPrecondition; that is waited through, bounded. Returns once
// the instance is READY (or SUSPENDED: a resumable conversation).
func (a *kagentAPI) createInstance(ctx context.Context, template, requestID string) (*apiv1alpha1.AgentInstance, error) {
	req := &apiv1alpha1.CreateAgentInstanceRequest{
		Harness:       &apiv1alpha1.ResourceReference{Namespace: kagentNamespace, Name: kagentHarness},
		AgentTemplate: &apiv1alpha1.ResourceReference{Namespace: kagentNamespace, Name: template},
		RequestId:     requestID,
	}
	var resp *apiv1alpha1.CreateAgentInstanceResponse
	var err error
	created := waitFor(int(templateRevisionTimeout/pollInterval), pollInterval, func() bool {
		resp, err = a.instances.CreateAgentInstance(a.callCtx(ctx), req)
		return status.Code(err) != codes.FailedPrecondition
	})
	if err != nil {
		return nil, fmt.Errorf("creating an AgentInstance of %s/%s on Harness %s: %w", kagentNamespace, template, kagentHarness, err)
	}
	if !created {
		return nil, fmt.Errorf("AgentTemplate %s has no successful revision after %s (the controller keeps answering FailedPrecondition)", template, templateRevisionTimeout)
	}
	instance := resp.GetAgentInstance()
	if instance.GetId() == "" {
		return nil, fmt.Errorf("CreateAgentInstance of %s answered without an id", template)
	}
	return a.awaitInstanceReady(ctx, instance)
}

// awaitInstanceReady polls the instance until it is READY or SUSPENDED (a
// conversation gives its worker back between turns; the next send resumes
// it), FAILED, or the deadline passes. The common case returns at once.
func (a *kagentAPI) awaitInstanceReady(ctx context.Context, instance *apiv1alpha1.AgentInstance) (*apiv1alpha1.AgentInstance, error) {
	deadline := time.Now().Add(instanceReadyTimeout)
	for {
		switch instance.GetState() {
		case apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY, apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_SUSPENDED:
			return instance, nil
		case apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_FAILED:
			return instance, fmt.Errorf("AgentInstance %s failed: %s %s", instance.GetId(), instance.GetFailure().GetReason(), instance.GetFailure().GetMessage())
		case apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_DELETING, apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_DELETED:
			return instance, fmt.Errorf("AgentInstance %s is %s", instance.GetId(), instanceState(instance))
		}
		if time.Now().After(deadline) {
			return instance, fmt.Errorf("AgentInstance %s is still %s after %s", instance.GetId(), instanceState(instance), instanceReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return instance, ctx.Err()
		case <-time.After(pollInterval):
		}
		next, err := a.getInstance(ctx, instance.GetId())
		if err != nil {
			return instance, err
		}
		instance = next
	}
}

// instanceState is the state the way the evidence quotes it (READY, not
// AGENT_INSTANCE_STATE_READY).
func instanceState(instance *apiv1alpha1.AgentInstance) string {
	return strings.TrimPrefix(instance.GetState().String(), "AGENT_INSTANCE_STATE_")
}

// getInstance is AgentInstanceService/GetAgentInstance.
func (a *kagentAPI) getInstance(ctx context.Context, id string) (*apiv1alpha1.AgentInstance, error) {
	resp, err := a.instances.GetAgentInstance(a.callCtx(ctx), &apiv1alpha1.GetAgentInstanceRequest{AgentInstanceId: id})
	if err != nil {
		return nil, fmt.Errorf("GetAgentInstance %s: %w", id, err)
	}
	return resp.GetAgentInstance(), nil
}

// listPageLimit is the largest page the controller's list calls validate
// (page.limit 0..100).
const listPageLimit = 100

// listInstances is AgentInstanceService/ListAgentInstances for the person:
// the instances the controller keeps for them (creator-scoped), every page.
func (a *kagentAPI) listInstances(ctx context.Context) ([]*apiv1alpha1.AgentInstance, error) {
	return a.listInstancesOf(ctx, nil)
}

// listInstancesOf is the listing narrowed to one template's conversations of
// the caller (nil: all of them), every page.
func (a *kagentAPI) listInstancesOf(ctx context.Context, template *apiv1alpha1.ResourceReference) ([]*apiv1alpha1.AgentInstance, error) {
	var all []*apiv1alpha1.AgentInstance
	page := &apiv1alpha1.PageRequest{Limit: listPageLimit}
	for {
		resp, err := a.instances.ListAgentInstances(a.callCtx(ctx), &apiv1alpha1.ListAgentInstancesRequest{Page: page, AgentTemplate: template})
		if err != nil {
			return nil, fmt.Errorf("ListAgentInstances: %w", err)
		}
		all = append(all, resp.GetAgentInstances()...)
		if resp.GetPage().GetNextPageToken() == "" {
			return all, nil
		}
		page = &apiv1alpha1.PageRequest{Limit: listPageLimit, PageToken: resp.GetPage().GetNextPageToken()}
	}
}

// deleteInstance is AgentInstanceService/DeleteAgentInstance; an instance
// that is already gone is success.
func (a *kagentAPI) deleteInstance(ctx context.Context, id string) error {
	_, err := a.instances.DeleteAgentInstance(a.callCtx(ctx), &apiv1alpha1.DeleteAgentInstanceRequest{AgentInstanceId: id})
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("DeleteAgentInstance %s: %w", id, err)
	}
	return nil
}

// stream is lf.a2a.v1.A2AService/SendStreamingMessage on the instance and
// yields the task's events as the SDK types. A message without a TaskID
// starts a new task; one carrying the id of a paused task resumes it. The
// message's ContextID stays empty: the controller owns the conversation's
// context id and rejects any other value.
func (a *kagentAPI) stream(ctx context.Context, instanceID string, msg *a2a.Message) iter.Seq2[a2a.Event, error] {
	return a.a2a.SendStreamingMessage(a.a2aCtx(ctx, instanceID), &a2a.SendMessageRequest{Message: msg})
}

// getTask is A2AService/GetTask on the instance.
func (a *kagentAPI) getTask(ctx context.Context, instanceID string, taskID a2a.TaskID) (*a2a.Task, error) {
	task, err := a.a2a.GetTask(a.a2aCtx(ctx, instanceID), &a2a.GetTaskRequest{ID: taskID})
	if err != nil {
		return nil, fmt.Errorf("GetTask %s: %w", taskID, err)
	}
	return task, nil
}

// listTasks is A2AService/ListTasks on the instance: its tasks, one page of
// up to 100 (a proof's instance has a handful).
func (a *kagentAPI) listTasks(ctx context.Context, instanceID string) ([]*a2a.Task, error) {
	resp, err := a.a2a.ListTasks(a.a2aCtx(ctx, instanceID), &a2a.ListTasksRequest{PageSize: listPageLimit})
	if err != nil {
		return nil, fmt.Errorf("ListTasks: %w", err)
	}
	return resp.Tasks, nil
}

// cancelTask is A2AService/CancelTask on the instance: the controller stops
// the task server-side and answers its final state.
func (a *kagentAPI) cancelTask(ctx context.Context, instanceID string, taskID a2a.TaskID) (*a2a.Task, error) {
	task, err := a.a2a.CancelTask(a.a2aCtx(ctx, instanceID), &a2a.CancelTaskRequest{ID: taskID})
	if err != nil {
		return nil, fmt.Errorf("CancelTask %s: %w", taskID, err)
	}
	return task, nil
}

// userMessage is one user turn: a text part, no context id (the controller's).
func userMessage(prompt string) *a2a.Message {
	return a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(prompt))
}

// turn is what one streamed A2A turn produced, folded from its events.
type turn struct {
	taskID    a2a.TaskID
	contextID string
	// states are the task states the stream reported, in order — SUBMITTED,
	// WORKING, …, the one the stream ended on.
	states []a2a.TaskState
	// artifacts are the artifacts' text so far, in the order first seen
	// (an appended chunk joins its artifact's text).
	artifactOrder []a2a.ArtifactID
	artifactText  map[a2a.ArtifactID]string
	// statusText is the text of the status message the stream ended on: the
	// agent's hint on a pause, its words on a failure.
	statusText string
	// approval is the tool_approval_request the task paused on at
	// input-required, nil otherwise.
	approval *toolApprovalRequest
	// message is set for a bare Message answer (no task).
	message *a2a.Message
	events  int
}

// state is the task state the stream ended on (unspecified before any).
func (t *turn) state() a2a.TaskState {
	if len(t.states) == 0 {
		return a2a.TaskStateUnspecified
	}
	return t.states[len(t.states)-1]
}

// text is the agent's answer: the artifacts' text parts in order, then the
// terminal status message's, or a bare Message's parts.
func (t *turn) text() string {
	if t.message != nil {
		return partsText(t.message.Parts)
	}
	texts := make([]string, 0, len(t.artifactOrder)+1)
	for _, id := range t.artifactOrder {
		if s := t.artifactText[id]; s != "" {
			texts = append(texts, s)
		}
	}
	if t.statusText != "" && !slices.Contains(texts, t.statusText) {
		texts = append(texts, t.statusText)
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}

// observe folds one event in.
func (t *turn) observe(ev a2a.Event) {
	t.events++
	switch e := ev.(type) {
	case *a2a.Task:
		t.taskID, t.contextID = e.ID, e.ContextID
		for _, artifact := range e.Artifacts {
			t.addArtifact(artifact, false)
		}
		t.setStatus(e.Status)
	case *a2a.TaskStatusUpdateEvent:
		t.taskID, t.contextID = e.TaskID, e.ContextID
		t.setStatus(e.Status)
	case *a2a.TaskArtifactUpdateEvent:
		t.taskID, t.contextID = e.TaskID, e.ContextID
		t.addArtifact(e.Artifact, e.Append)
	case *a2a.Message:
		t.message = e
	}
}

func (t *turn) setStatus(s a2a.TaskStatus) {
	t.states = append(t.states, s.State)
	t.approval = nil
	switch {
	case s.State == a2a.TaskStateInputRequired:
		t.approval = parseToolApprovalRequest(s.Message)
		t.statusText = messageText(s.Message)
	case s.State.Terminal():
		t.statusText = messageText(s.Message)
	}
}

func (t *turn) addArtifact(artifact *a2a.Artifact, appendChunk bool) {
	if artifact == nil {
		return
	}
	if t.artifactText == nil {
		t.artifactText = map[a2a.ArtifactID]string{}
	}
	if _, seen := t.artifactText[artifact.ID]; !seen {
		t.artifactOrder = append(t.artifactOrder, artifact.ID)
	}
	text := partsText(artifact.Parts)
	if appendChunk {
		t.artifactText[artifact.ID] += text
		return
	}
	t.artifactText[artifact.ID] = text
}

// statesString is the states the way the evidence quotes them:
// `submitted → working → completed`.
func (t *turn) statesString() string {
	names := make([]string, 0, len(t.states))
	for _, s := range t.states {
		names = append(names, stateName(s))
	}
	return strings.Join(names, " → ")
}

// stateName is a task state the way the evidence quotes it (canceled, not
// TASK_STATE_CANCELED).
func stateName(s a2a.TaskState) string {
	return strings.ToLower(strings.TrimPrefix(string(s), "TASK_STATE_"))
}

// messageText is the text of a message's text parts, "" for no message.
func messageText(msg *a2a.Message) string {
	if msg == nil {
		return ""
	}
	return partsText(msg.Parts)
}

// partsText joins the text parts of a message or artifact (a data part — a
// tool call, a tool result — carries no text and is skipped).
func partsText(parts a2a.ContentParts) string {
	var texts []string
	for _, part := range parts {
		if text := part.Text(); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "")
}

// turn drives one streamed turn on the instance to the end of its stream
// and folds what it said. The state it ended on is the caller's to judge: a
// completed task is an answer, input-required a pause the caller decides on.
func (a *kagentAPI) turn(ctx context.Context, instanceID string, msg *a2a.Message) (*turn, error) {
	t := &turn{}
	for ev, err := range a.stream(ctx, instanceID, msg) {
		if err != nil {
			return t, fmt.Errorf("SendStreamingMessage on %s: %w", instanceID, err)
		}
		t.observe(ev)
	}
	return t, nil
}

// hitlTool is one tool invocation awaiting the person's decision.
type hitlTool struct {
	ID     string         `json:"id"`
	CallID string         `json:"call_id"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
}

// toolApprovalRequest is the HITL payload of a task paused at
// input-required on one or more tool calls that need approval.
type toolApprovalRequest struct {
	Type   string     `json:"type"`
	Hint   string     `json:"hint,omitempty"`
	Tools  []hitlTool `json:"tools"`
	Nested *struct {
		Tools []hitlTool `json:"tools"`
	} `json:"nested,omitempty"`
}

// decidedTools are the tools a decision covers: a nested child's when the
// request was propagated from a sub-agent, the request's own otherwise.
func (r *toolApprovalRequest) decidedTools() []hitlTool {
	if r.Nested != nil {
		return r.Nested.Tools
	}
	return r.Tools
}

// toolNames names the tools awaiting the decision.
func (r *toolApprovalRequest) toolNames() []string {
	var names []string
	for _, tool := range r.decidedTools() {
		names = append(names, tool.Name)
	}
	return names
}

// toolApproval is one decision, toolApprovalResponse the answer to a request:
// exactly one decision per requested tool.
type toolApproval struct {
	ID              string `json:"id"`
	Approved        bool   `json:"approved"`
	RejectionReason string `json:"rejection_reason,omitempty"`
}

type toolApprovalResponse struct {
	Type      string         `json:"type"`
	Approvals []toolApproval `json:"approvals"`
}

// parseToolApprovalRequest reads the tool_approval_request a paused task's
// status message carries under the extension URI in its metadata; nil for a
// message without one (a plain-text prompt, an ask_user question, a runtime
// that did not activate the extension).
func parseToolApprovalRequest(msg *a2a.Message) *toolApprovalRequest {
	if msg == nil || !slices.Contains(msg.Extensions, hitlExtensionURI) {
		return nil
	}
	raw, ok := msg.Metadata[hitlExtensionURI].(map[string]any)
	if !ok || stringOf(raw[fieldTypeKey]) != hitlTypeToolApprovalRequest {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var req toolApprovalRequest
	if err := json.Unmarshal(encoded, &req); err != nil || len(req.decidedTools()) == 0 {
		return nil
	}
	return &req
}

// decisionMessage answers a paused task's tool_approval_request the way
// kagent's UI does: a user message on the paused task, the extension
// declared, under its URI one approval per requested tool — all approved,
// or all rejected with the reason — so the task resumes in place, and as
// its one text part the transcript line of the same choices ("Approved:
// <tool>", "Rejected: <tool>" with a "Reason:" line): the A2A server
// requires a part, and the conversation reads as what happened.
func decisionMessage(taskID a2a.TaskID, req *toolApprovalRequest, approve bool, reason string) (*a2a.Message, error) {
	response := toolApprovalResponse{Type: hitlTypeToolApprovalResponse}
	var lines []string
	for _, tool := range req.decidedTools() {
		approval := toolApproval{ID: tool.ID, Approved: approve}
		line := "Approved: " + tool.Name
		if !approve {
			approval.RejectionReason = reason
			line = "Rejected: " + tool.Name
			if reason != "" {
				line += "\nReason: " + reason
			}
		}
		response.Approvals = append(response.Approvals, approval)
		lines = append(lines, line)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return nil, err
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(strings.Join(lines, "\n")))
	msg.TaskID = taskID
	msg.SetMeta(hitlExtensionURI, raw)
	msg.Extensions = append(msg.Extensions, hitlExtensionURI)
	return msg, nil
}

// decide resumes a task paused on a tool_approval_request with the decision
// and folds the resumed stream.
func (a *kagentAPI) decide(ctx context.Context, instanceID string, paused *turn, approve bool, reason string) (*turn, error) {
	if paused.approval == nil {
		return nil, fmt.Errorf("task %s carries no tool_approval_request to decide on (state %s)", paused.taskID, stateName(paused.state()))
	}
	msg, err := decisionMessage(paused.taskID, paused.approval, approve, reason)
	if err != nil {
		return nil, err
	}
	return a.turn(ctx, instanceID, msg)
}

// isUnauthenticated reports whether the edge refused the call for want of a
// token: the gRPC status on a kagent service call, the A2A error the SDK
// maps it to on an A2A call.
func isUnauthenticated(err error) bool {
	return status.Code(err) == codes.Unauthenticated || errors.Is(err, a2a.ErrUnauthenticated)
}

// errTurnPaused says a shared proof's turn paused for a decision it does not
// take: its agents bind no tool that requires approval, so a pause is a
// finding.
var errTurnPaused = errors.New("the turn paused at input-required")

// completedText is the answer of a turn that ended completed; any other end
// is an error naming the state and what the agent said.
func (t *turn) completedText() (string, error) {
	switch {
	case t.message != nil:
		return t.text(), nil
	case t.state() == a2a.TaskStateCompleted:
		return t.text(), nil
	case t.state() == a2a.TaskStateInputRequired:
		tools := "no tools named"
		if t.approval != nil {
			tools = strings.Join(t.approval.toolNames(), ", ")
		}
		return "", fmt.Errorf("%w on task %s (%s): %s", errTurnPaused, t.taskID, tools, excerpt(t.statusText, 200))
	case len(t.states) == 0:
		return "", fmt.Errorf("the stream ended without a task or a message (%d events)", t.events)
	default:
		return "", fmt.Errorf("the task ended %s (%s): %s", stateName(t.state()), t.statesString(), excerpt(t.text(), 300))
	}
}

// agentTurnAs sends one turn to the agent as the person whose Dex id_token is
// given — the way Swarmgeist and the portal drive a message: an AgentInstance
// of the AgentTemplate on the Go ADK Harness created for the person and
// deleted afterwards, one SendStreamingMessage carrying the person's bearer,
// the instance route and the HITL extension request, the answer consumed as
// a stream — and returns the agent's text. Shared steps of the proofs call
// this one; a turn that pauses for a decision is an error (their agents bind
// nothing that requires approval).
func agentTurnAs(cfg *config.Config, name, token, prompt string) (string, error) {
	api, err := dialKagentAPI(cfg, token)
	if err != nil {
		return "", err
	}
	defer api.close()
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	defer cancel()
	instance, err := api.createInstance(ctx, name, uuid.NewString())
	if err != nil {
		return "", err
	}
	defer api.removeInstance(instance.GetId())
	note("AgentInstance %s of %s (creator %s, %s)", instance.GetId(), name, instance.GetCreator(), instanceState(instance))
	t, err := api.turn(ctx, instance.GetId(), userMessage(prompt))
	if err != nil {
		return "", fmt.Errorf("A2A turn on %s: %w", name, err)
	}
	reply, err := t.completedText()
	if err != nil {
		return "", fmt.Errorf("A2A turn on %s: %w", name, err)
	}
	return reply, nil
}

// removeInstance deletes an instance on the cleanup paths, bounded on its own
// context, and says when it could not.
func (a *kagentAPI) removeInstance(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.deleteInstance(ctx, id); err != nil {
		note("deleting AgentInstance %s: %v", id, err)
	}
}
