package lab

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
	"github.com/giantswarm/agentlab/internal/kagent/gen/agentlab/kagentv10"
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
// id_token>`, and on the A2A calls the human-in-the-loop extension requested
// (`A2A-Extensions`, gRPC metadata `a2a-extensions`) so a tool that needs
// approval pauses the task at input-required with a decidable request
// instead of a plain notice. An A2A call names the Agent as its tenant
// (`<namespace>/<name>`, the request's tenant field) and the Session that
// holds the conversation through the message's context id, which is the
// Session's id; a message without one starts a new Session of the Agent. The
// Harness re-emits the person's bearer on every MCP call, so muster logs the
// tool calls under the person. The messages are kagent's own protos
// (internal/kagent/gen, generated from the line's proto tree) and the A2A v1
// package the controller itself is built with.

const (
	// kagentHarness is the platform's Go ADK Harness: every Agent the proofs
	// create references it (spec.harnessRef) and every turn runs on it.
	kagentHarness = "kagent"
	// kagentTurnTimeout bounds one turn end to end: the session create, a
	// cold resume from the golden snapshot, the model's answer with its tool
	// calls, the session delete.
	kagentTurnTimeout = 180 * time.Second
	// agentRevisionTimeout bounds CreateSession's wait for an Agent whose
	// golden snapshot is still being taken (the controller answers
	// FailedPrecondition meanwhile; kagent's own e2e polls through it the
	// same way).
	agentRevisionTimeout = 60 * time.Second
	// sessionReadyTimeout bounds a freshly created session's way to READY:
	// the controller converges it synchronously, so the poll only covers a
	// create that was interrupted and retried.
	sessionReadyTimeout = 90 * time.Second
	// substratePageSize is common.proto's cap on a page; substrateMaxPages
	// bounds a walk the controller's tokens never end.
	substratePageSize = 100
	substrateMaxPages = 100

	// The gRPC metadata of the contract (keys lower-case on the wire).
	authorizationMetadata = "authorization"
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
	crSpec       = "spec"
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
	agents    apiv1alpha1.AgentServiceClient
	sessions  apiv1alpha1.SessionServiceClient
	system    apiv1alpha1.SystemServiceClient
	conn      grpc.ClientConnInterface
	token     string
	// extra is metadata sent beside the token on every call — the identity
	// proof's forged x-user-id; nil for everyone else.
	extra metadata.MD
	// events, when set, receives every streamed A2A event as one JSON line.
	events    io.Writer
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
		agents:    apiv1alpha1.NewAgentServiceClient(conn),
		sessions:  apiv1alpha1.NewSessionServiceClient(conn),
		system:    apiv1alpha1.NewSystemServiceClient(conn),
		conn:      conn,
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

// a2aCtx attaches the A2A service parameters of a call: the person's bearer,
// the HITL extension request (and the extra metadata). The gRPC transport
// carries them as metadata; the Agent and the Session are named in the
// request itself (agentTenant, the message's context id).
func (a *kagentAPI) a2aCtx(ctx context.Context) context.Context {
	params := a2aclient.ServiceParams{
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

// agentTenant is the A2A tenant of an Agent, the way the controller's
// gateway routes a call: `<namespace>/<name>`.
func agentTenant(ref *apiv1alpha1.ResourceReference) string {
	return ref.GetNamespace() + "/" + ref.GetName()
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

// substrateState is the controller's view of Substrate for one namespace:
// the WorkerPools, every ActorTemplate with its phase and golden snapshot,
// every actor with its state and worker pod. ateAPIErrors are the reads the
// controller could not make of ate-api, which it answers beside the data
// rather than as a failed call. What the portal's Substrate page shows; the
// person needs get on Substrate.
type substrateState struct {
	pools        []substratePool
	templates    []substrateTemplate
	actors       []substrateActor
	ateAPIErrors []string
}

type substratePool struct {
	namespace, name string
	replicas        int
	image           string
}

// substrateTemplate's golden is the golden snapshot worded ("golden tag
// <atespace>/<name>" on 1.1, "golden snapshot <uri>" on 1.0, empty before
// one exists); failure is Substrate's error when the phase is Failed.
type substrateTemplate struct {
	namespace, name, phase, golden, failure string
}

type substrateActor struct {
	id, templateNamespace, templateName, state string
	workerNamespace, workerPod, workerIP       string
}

// legacySubstrateStatusMethod is the kagent 1.0 line's one Substrate read.
const legacySubstrateStatusMethod = "/kagent.api.v1alpha1.SystemService/GetSubstrateStatus"

// substrateState reads Substrate through SystemService/GetSubstrateSummary
// (pools, templates) and ListSubstrateActors (every actor, all pages), in
// every atespace: an actor's atespace need not be its template's, so the
// filter is the caller's, on the template ref. A controller of the 1.0 line
// answers Unimplemented and is read through GetSubstrateStatus instead.
func (a *kagentAPI) substrateState(ctx context.Context, namespace string) (substrateState, error) {
	summary, err := a.system.GetSubstrateSummary(a.callCtx(ctx), &apiv1alpha1.GetSubstrateSummaryRequest{Namespace: namespace})
	if status.Code(err) == codes.Unimplemented {
		return a.legacySubstrateState(ctx, namespace)
	}
	if err != nil {
		return substrateState{}, fmt.Errorf("GetSubstrateSummary: %w", err)
	}
	var state substrateState
	for _, p := range summary.GetWorkerPools() {
		state.pools = append(state.pools, poolOf(p))
	}
	for _, t := range summary.GetActorTemplates() {
		state.templates = append(state.templates, templateOf(t))
	}
	if e := summary.GetAteApiError(); e != "" {
		state.ateAPIErrors = append(state.ateAPIErrors, e)
	}
	token := ""
	for range substrateMaxPages {
		page, err := a.system.ListSubstrateActors(a.callCtx(ctx), &apiv1alpha1.ListSubstrateActorsRequest{
			Page: &apiv1alpha1.PageRequest{Limit: substratePageSize, PageToken: token},
		})
		if err != nil {
			return substrateState{}, fmt.Errorf("ListSubstrateActors: %w", err)
		}
		if e := page.GetAteApiError(); e != "" {
			state.ateAPIErrors = append(state.ateAPIErrors, e)
			return state, nil
		}
		for _, actor := range page.GetActors() {
			state.actors = append(state.actors, actorOf(actor))
		}
		next := page.GetPage().GetNextPageToken()
		if next == "" {
			return state, nil
		}
		if next == token {
			return substrateState{}, fmt.Errorf("ListSubstrateActors: the controller answered page token %q with itself", token)
		}
		token = next
	}
	return substrateState{}, fmt.Errorf("ListSubstrateActors: still paging after %d pages", substrateMaxPages)
}

// legacySubstrateState is substrateState on the kagent 1.0 line.
func (a *kagentAPI) legacySubstrateState(ctx context.Context, namespace string) (substrateState, error) {
	resp := &kagentv10.GetSubstrateStatusResponse{}
	if err := a.conn.Invoke(a.callCtx(ctx), legacySubstrateStatusMethod, &kagentv10.GetSubstrateStatusRequest{Namespace: namespace}, resp); err != nil {
		return substrateState{}, fmt.Errorf("GetSubstrateStatus: %w", err)
	}
	var state substrateState
	for _, p := range resp.GetWorkerPools() {
		state.pools = append(state.pools, substratePool{namespace: p.GetNamespace(), name: p.GetName(), replicas: int(p.GetReplicas()), image: p.GetAteomImage()})
	}
	for _, t := range resp.GetActorTemplates() {
		template := substrateTemplate{namespace: t.GetNamespace(), name: t.GetName(), phase: t.GetPhase()}
		if snapshot := t.GetGoldenSnapshot(); snapshot != "" {
			template.golden = "golden snapshot " + snapshot
		}
		state.templates = append(state.templates, template)
	}
	for _, actor := range resp.GetActors() {
		state.actors = append(state.actors, substrateActor{
			id: actor.GetActorId(), templateNamespace: actor.GetActorTemplateNamespace(), templateName: actor.GetActorTemplateName(), state: actor.GetStatus(),
			workerNamespace: actor.GetAteomPodNamespace(), workerPod: actor.GetAteomPodName(), workerIP: actor.GetAteomPodIp(),
		})
	}
	if e := resp.GetAteApiError(); e != "" {
		state.ateAPIErrors = append(state.ateAPIErrors, e)
	}
	return state, nil
}

// poolOf reads a WorkerPool's replicas and worker image off the CR the
// controller hands back whole.
func poolOf(p *apiv1alpha1.SubstrateWorkerPool) substratePool {
	spec, _ := p.GetResource().GetValue().AsMap()["spec"].(map[string]any)
	replicas, _ := spec["replicas"].(float64)
	image, _ := spec["workerImage"].(string)
	return substratePool{namespace: p.GetRef().GetNamespace(), name: p.GetRef().GetName(), replicas: int(replicas), image: image}
}

// templatePhaseFailed is the phase of an ActorTemplate whose golden snapshot
// reported an error.
const templatePhaseFailed = "Failed"

// templateOf words an ActorTemplate's golden snapshot status as a phase:
// Failed on an error, Ready once the golden tag is set, Pending before.
func templateOf(t *ateapi.ActorTemplate) substrateTemplate {
	template := substrateTemplate{namespace: t.GetMetadata().GetAtespace(), name: t.GetMetadata().GetName(), phase: "Pending"}
	golden := t.GetStatus().GetGoldenSnapshotStatus()
	if tag := golden.GetGoldenTag(); tag != nil {
		template.phase = conditionReady
		template.golden = "golden tag " + tag.GetAtespace() + "/" + tag.GetName()
	}
	if e := golden.GetErrorMessage(); e != "" {
		template.phase = templatePhaseFailed
		template.failure = e
	}
	return template
}

// actorOf reads an actor's template, state (the ActorState without its enum
// prefix: RUNNING) and worker assignment.
func actorOf(actor *ateapi.Actor) substrateActor {
	worker := actor.GetStatus().GetWorkerAssignment()
	return substrateActor{
		id: actor.GetMetadata().GetName(), templateNamespace: actor.GetActorTemplate().GetAtespace(), templateName: actor.GetActorTemplate().GetName(),
		state:           strings.TrimPrefix(actor.GetStatus().GetState().String(), "ACTOR_STATE_"),
		workerNamespace: worker.GetWorkerNamespace(), workerPod: worker.GetWorkerPod(), workerIP: worker.GetWorkerPodIp(),
	}
}

// listTemplates is AgentTemplateService/ListAgentTemplates of the kagent
// namespace: the portable halves the Agents of the roster pair with a Harness.
func (a *kagentAPI) listTemplates(ctx context.Context) ([]*apiv1alpha1.AgentTemplate, error) {
	resp, err := a.templates.ListAgentTemplates(a.callCtx(ctx), &apiv1alpha1.ListAgentTemplatesRequest{Namespace: kagentNamespace})
	if err != nil {
		return nil, fmt.Errorf("ListAgentTemplates in %s: %w", kagentNamespace, err)
	}
	return resp.GetAgentTemplates(), nil
}

// listAgents is AgentService/ListAgents of the kagent namespace, the roster
// call: every runnable definition (an AgentTemplate paired with a Harness) a
// Session can be created of.
func (a *kagentAPI) listAgents(ctx context.Context) ([]*apiv1alpha1.Agent, error) {
	resp, err := a.agents.ListAgents(a.callCtx(ctx), &apiv1alpha1.ListAgentsRequest{Namespace: kagentNamespace})
	if err != nil {
		return nil, fmt.Errorf("ListAgents in %s: %w", kagentNamespace, err)
	}
	return resp.GetAgents(), nil
}

// roster is the two lists a surface joins for its roster: the Agents of the
// kagent namespace and the AgentTemplates they reference.
func (a *kagentAPI) roster(ctx context.Context) ([]*apiv1alpha1.Agent, []*apiv1alpha1.AgentTemplate, error) {
	agents, err := a.listAgents(ctx)
	if err != nil {
		return nil, nil, err
	}
	templates, err := a.listTemplates(ctx)
	if err != nil {
		return nil, nil, err
	}
	return agents, templates, nil
}

// agentListing is what a surface reads off one listed Agent: the technical
// name, the AgentTemplate and the Harness it pairs (empty for one written
// inline), the display name and icon from the chart's annotations, and why
// the Agent cannot start a conversation (empty for a selectable one), the
// roster entry of klaus-gateway and the portal.
type agentListing struct {
	Name, Namespace, Template, Harness, DisplayName, IconURL, Description, Unavailable string
}

// listingOf derives the roster entry from a listed Agent: the references
// from its spec, the annotations from its metadata (the AgentTemplate's when
// the Agent carries none, templates being the namespace's by name), the
// readiness from status.conditions. An Agent is selectable when it reports
// Ready=True.
func listingOf(agent *apiv1alpha1.Agent, templates map[string]*apiv1alpha1.AgentTemplate) agentListing {
	resource := agent.GetResource().GetValue().AsMap()
	spec := nestedMap(resource, crSpec)
	l := agentListing{
		Name:      agent.GetRef().GetName(),
		Namespace: agent.GetRef().GetNamespace(),
		Template:  stringOf(nestedMap(spec, "templateRef")[nameKey]),
		Harness:   stringOf(nestedMap(spec, "harnessRef")[nameKey]),
	}
	annotations, _ := nestedMap(resource, crMetadata)["annotations"].(map[string]any)
	l.DisplayName = stringOf(annotations[displayNameAnnotation])
	l.IconURL = stringOf(annotations[iconURLAnnotation])
	if template := templates[l.Template]; template != nil {
		l.Description = template.GetDescription()
		templateAnnotations := nestedMap(nestedMap(template.GetResource().GetValue().AsMap(), crMetadata), "annotations")
		if l.DisplayName == "" {
			l.DisplayName = stringOf(templateAnnotations[displayNameAnnotation])
		}
		if l.IconURL == "" {
			l.IconURL = stringOf(templateAnnotations[iconURLAnnotation])
		}
	}
	if l.Description == "" {
		l.Description = stringOf(nestedMap(spec, "template")[descriptionKey])
	}
	conditions, _ := nestedMap(resource, crStatus)[crConditions].([]any)
	if ready, reason := readyConditionOf(conditions); !ready {
		l.Unavailable = fmt.Sprintf("Agent %s has not compiled a ready revision: %s", l.Name, reason)
	}
	return l
}

// templatesByName indexes listed AgentTemplates by name.
func templatesByName(templates []*apiv1alpha1.AgentTemplate) map[string]*apiv1alpha1.AgentTemplate {
	byName := make(map[string]*apiv1alpha1.AgentTemplate, len(templates))
	for _, t := range templates {
		byName[t.GetRef().GetName()] = t
	}
	return byName
}

// readyConditionOf reads the Ready condition of an Agent's
// status.conditions: true, or false with the reason.
func readyConditionOf(conditions []any) (bool, string) {
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

func nestedMap(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// agentRef is the reference of an Agent of the kagent namespace.
func agentRef(name string) *apiv1alpha1.ResourceReference {
	return &apiv1alpha1.ResourceReference{Namespace: kagentNamespace, Name: name}
}

// createSession is createSessionOf for an Agent of the kagent namespace.
func (a *kagentAPI) createSession(ctx context.Context, agent, requestID string) (*apiv1alpha1.Session, error) {
	return a.createSessionOf(ctx, agentRef(agent), requestID)
}

// createSessionOf is SessionService/CreateSession for the person: one
// conversation of the Agent, keyed by requestID. The controller's create is
// idempotent per (creator, request_id), so a retried first turn gets the
// same session back. An Agent whose golden snapshot is still being taken
// answers FailedPrecondition; that is waited through, bounded. Returns once
// the session is READY (or SUSPENDED: a resumable conversation).
func (a *kagentAPI) createSessionOf(ctx context.Context, agent *apiv1alpha1.ResourceReference, requestID string) (*apiv1alpha1.Session, error) {
	req := &apiv1alpha1.CreateSessionRequest{Agent: agent, RequestId: requestID}
	var resp *apiv1alpha1.CreateSessionResponse
	var err error
	created := waitFor(int(agentRevisionTimeout/pollInterval), pollInterval, func() bool {
		resp, err = a.sessions.CreateSession(a.callCtx(ctx), req)
		return status.Code(err) != codes.FailedPrecondition
	})
	if err != nil {
		return nil, fmt.Errorf("creating a Session of Agent %s: %w", agentTenant(agent), err)
	}
	if !created {
		return nil, fmt.Errorf("Agent %s has no successful revision after %s (the controller keeps answering FailedPrecondition)", agent.GetName(), agentRevisionTimeout)
	}
	session := resp.GetSession()
	if session.GetId() == "" {
		return nil, fmt.Errorf("CreateSession of %s answered without an id", agent.GetName())
	}
	return a.awaitSessionReady(ctx, session)
}

// awaitSessionReady polls the session until it is READY or SUSPENDED (a
// conversation gives its worker back between turns; the next send resumes
// it), FAILED, or the deadline passes. The common case returns at once.
func (a *kagentAPI) awaitSessionReady(ctx context.Context, session *apiv1alpha1.Session) (*apiv1alpha1.Session, error) {
	deadline := time.Now().Add(sessionReadyTimeout)
	for {
		switch session.GetState() {
		case apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED:
			return session, nil
		case apiv1alpha1.RuntimeState_RUNTIME_STATE_FAILED:
			return session, fmt.Errorf("Session %s failed: %s %s", session.GetId(), session.GetFailure().GetReason(), session.GetFailure().GetMessage())
		case apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETING, apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED:
			return session, fmt.Errorf("Session %s is %s", session.GetId(), sessionState(session))
		}
		if time.Now().After(deadline) {
			return session, fmt.Errorf("Session %s is still %s after %s", session.GetId(), sessionState(session), sessionReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return session, ctx.Err()
		case <-time.After(pollInterval):
		}
		next, err := a.getSession(ctx, session.GetId())
		if err != nil {
			return session, err
		}
		session = next
	}
}

// sessionState is the state the way the evidence quotes it (READY, not
// RUNTIME_STATE_READY).
func sessionState(session *apiv1alpha1.Session) string {
	return strings.TrimPrefix(session.GetState().String(), "RUNTIME_STATE_")
}

// getSession is SessionService/GetSession.
func (a *kagentAPI) getSession(ctx context.Context, id string) (*apiv1alpha1.Session, error) {
	resp, err := a.sessions.GetSession(a.callCtx(ctx), &apiv1alpha1.GetSessionRequest{SessionId: id})
	if err != nil {
		return nil, fmt.Errorf("GetSession %s: %w", id, err)
	}
	return resp.GetSession(), nil
}

// listPageLimit is the largest page the controller's list calls validate
// (page.limit 0..100).
const listPageLimit = 100

// listSessions is SessionService/ListSessions for the person: the sessions
// the controller keeps for them (creator-scoped), every page.
func (a *kagentAPI) listSessions(ctx context.Context) ([]*apiv1alpha1.Session, error) {
	return a.listSessionsOf(ctx, nil)
}

// listSessionsOf is the listing narrowed to one Agent's conversations of the
// caller (nil: all of them), every page.
func (a *kagentAPI) listSessionsOf(ctx context.Context, agent *apiv1alpha1.ResourceReference) ([]*apiv1alpha1.Session, error) {
	var all []*apiv1alpha1.Session
	page := &apiv1alpha1.PageRequest{Limit: listPageLimit}
	for {
		resp, err := a.sessions.ListSessions(a.callCtx(ctx), &apiv1alpha1.ListSessionsRequest{Page: page, Agent: agent})
		if err != nil {
			return nil, fmt.Errorf("ListSessions: %w", err)
		}
		all = append(all, resp.GetSessions()...)
		if resp.GetPage().GetNextPageToken() == "" {
			return all, nil
		}
		page = &apiv1alpha1.PageRequest{Limit: listPageLimit, PageToken: resp.GetPage().GetNextPageToken()}
	}
}

// suspendSession is SessionService/SuspendSession: the Actor is snapshotted
// to the Harness's store and the session reported SUSPENDED; the next turn
// restores it. Returns the session as the controller reports it after the
// suspend.
func (a *kagentAPI) suspendSession(ctx context.Context, id string) (*apiv1alpha1.Session, error) {
	resp, err := a.sessions.SuspendSession(a.callCtx(ctx), &apiv1alpha1.SuspendSessionRequest{SessionId: id})
	if err != nil {
		return nil, fmt.Errorf("SuspendSession %s: %w", id, err)
	}
	return resp.GetSession(), nil
}

// resumeSession is SessionService/ResumeSession on a SUSPENDED session: the
// Actor is restored from its snapshot; returns once the session is READY
// again. A suspended session accepts no task until then, which is what the
// surfaces do before a turn.
func (a *kagentAPI) resumeSession(ctx context.Context, id string) (*apiv1alpha1.Session, error) {
	resp, err := a.sessions.ResumeSession(a.callCtx(ctx), &apiv1alpha1.ResumeSessionRequest{SessionId: id})
	if err != nil {
		return nil, fmt.Errorf("ResumeSession %s: %w", id, err)
	}
	session := resp.GetSession()
	deadline := time.Now().Add(sessionReadyTimeout)
	for session.GetState() != apiv1alpha1.RuntimeState_RUNTIME_STATE_READY {
		if time.Now().After(deadline) {
			return session, fmt.Errorf("Session %s is still %s after %s", id, sessionState(session), sessionReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return session, ctx.Err()
		case <-time.After(pollInterval):
		}
		got, err := a.getSession(ctx, id)
		if err != nil {
			return session, err
		}
		session = got
	}
	return session, nil
}

// deleteSession is SessionService/DeleteSession; a session that is already
// gone is success.
func (a *kagentAPI) deleteSession(ctx context.Context, id string) error {
	_, err := a.sessions.DeleteSession(a.callCtx(ctx), &apiv1alpha1.DeleteSessionRequest{SessionId: id})
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("DeleteSession %s: %w", id, err)
	}
	return nil
}

// stream is lf.a2a.v1.A2AService/SendStreamingMessage on the session and
// yields the task's events as the SDK types. A message without a TaskID
// starts a new task in the session (its context id is the session's; the
// controller rejects any other value); one carrying the id of a paused task
// resumes it and names no context of its own.
func (a *kagentAPI) stream(ctx context.Context, session *apiv1alpha1.Session, msg *a2a.Message) iter.Seq2[a2a.Event, error] {
	if msg.TaskID == "" {
		msg.ContextID = session.GetContextId()
	}
	return a.a2a.SendStreamingMessage(a.a2aCtx(ctx), &a2a.SendMessageRequest{Tenant: agentTenant(session.GetAgent()), Message: msg})
}

// getTask is A2AService/GetTask on the session's Agent.
func (a *kagentAPI) getTask(ctx context.Context, session *apiv1alpha1.Session, taskID a2a.TaskID) (*a2a.Task, error) {
	task, err := a.a2a.GetTask(a.a2aCtx(ctx), &a2a.GetTaskRequest{Tenant: agentTenant(session.GetAgent()), ID: taskID})
	if err != nil {
		return nil, fmt.Errorf("GetTask %s: %w", taskID, err)
	}
	return task, nil
}

// listTasks is A2AService/ListTasks narrowed to the session (its context
// id): its tasks, one page of up to 100 (a proof's session has a handful).
func (a *kagentAPI) listTasks(ctx context.Context, session *apiv1alpha1.Session) ([]*a2a.Task, error) {
	resp, err := a.a2a.ListTasks(a.a2aCtx(ctx), &a2a.ListTasksRequest{Tenant: agentTenant(session.GetAgent()), ContextID: session.GetContextId(), PageSize: listPageLimit})
	if err != nil {
		return nil, fmt.Errorf("ListTasks: %w", err)
	}
	return resp.Tasks, nil
}

// cancelTask is A2AService/CancelTask on the session's Agent: the controller
// stops the task server-side and answers its final state.
func (a *kagentAPI) cancelTask(ctx context.Context, session *apiv1alpha1.Session, taskID a2a.TaskID) (*a2a.Task, error) {
	task, err := a.a2a.CancelTask(a.a2aCtx(ctx), &a2a.CancelTaskRequest{Tenant: agentTenant(session.GetAgent()), ID: taskID})
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
	// agent's hint on a pause, its words on a failure; statusMeta is that
	// message's metadata (a harness's usage of the turn, for one).
	statusText string
	statusMeta map[string]any
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
		if s.Message != nil {
			t.statusMeta = s.Message.Metadata
		}
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

// turn drives one streamed turn on the session to the end of its stream
// and folds what it said. The state it ended on is the caller's to judge: a
// completed task is an answer, input-required a pause the caller decides on.
func (a *kagentAPI) turn(ctx context.Context, session *apiv1alpha1.Session, msg *a2a.Message) (*turn, error) {
	t := &turn{}
	for ev, err := range a.stream(ctx, session, msg) {
		if err != nil {
			return t, fmt.Errorf("SendStreamingMessage on %s: %w", session.GetId(), err)
		}
		if a.events != nil {
			if line, err := json.Marshal(ev); err == nil {
				_, _ = fmt.Fprintf(a.events, "%s\n", line)
			}
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
func (a *kagentAPI) decide(ctx context.Context, session *apiv1alpha1.Session, paused *turn, approve bool, reason string) (*turn, error) {
	if paused.approval == nil {
		return nil, fmt.Errorf("task %s carries no tool_approval_request to decide on (state %s)", paused.taskID, stateName(paused.state()))
	}
	msg, err := decisionMessage(paused.taskID, paused.approval, approve, reason)
	if err != nil {
		return nil, err
	}
	return a.turn(ctx, session, msg)
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
// given — the way Swarmgeist and the portal drive a message: a Session of the
// Agent created for the person and deleted afterwards, one
// SendStreamingMessage carrying the person's bearer and the HITL extension
// request, the answer consumed as a stream — and returns the agent's text.
// Shared steps of the proofs call this one; a turn that pauses for a
// decision is an error (their agents bind nothing that requires approval).
func agentTurnAs(cfg *config.Config, name, token, prompt string) (string, error) {
	api, err := dialKagentAPI(cfg, token)
	if err != nil {
		return "", err
	}
	defer api.close()
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	defer cancel()
	session, err := api.createSession(ctx, name, uuid.NewString())
	if err != nil {
		return "", err
	}
	defer api.removeSession(session.GetId())
	note("Session %s of %s (creator %s, %s)", session.GetId(), name, session.GetCreator(), sessionState(session))
	t, err := api.turn(ctx, session, userMessage(prompt))
	if err != nil {
		return "", fmt.Errorf("A2A turn on %s: %w", name, err)
	}
	reply, err := t.completedText()
	if err != nil {
		return "", fmt.Errorf("A2A turn on %s: %w", name, err)
	}
	return reply, nil
}

// removeSession deletes a session on the cleanup paths, bounded on its own
// context, and says when it could not.
func (a *kagentAPI) removeSession(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.deleteSession(ctx, id); err != nil {
		note("deleting Session %s: %v", id, err)
	}
}
