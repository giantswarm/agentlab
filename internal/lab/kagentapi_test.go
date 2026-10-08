package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/a2aproject/a2a-go/v2/a2apb/v1/pbconv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"

	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
	"github.com/giantswarm/agentlab/internal/kagent/gen/agentlab/kagentv10"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

const (
	fakeSessionID     = "0192f1c2-7d1e-7a3b-9c4d-000000000001"
	fakeToken         = "user-jwt"
	fakeTaskID        = "task-1"
	fakeContextID     = "ctx-1"
	fakeTool          = "filter_tools"
	kindAgentTemplate = "AgentTemplate"
	kindAgent         = "Agent"
	fakeCodingAgent   = "coding"
	fakeNarrowAgent   = "narrow"
	testWorkerPool    = "kagent-default"
	testWorkerPod     = "kagent-default-abc"
	testWorkerIP      = "10.0.0.7"
	testTemplateName  = "t-kagent-0"
	testAteomImage    = "ateom:v0"
)

// fakeKagent is an in-process kagent API v2 controller behind a fake edge:
// the A2A v1 service and the AgentTemplate, Agent, Session and System
// services on a bufconn. It records the metadata of every call so the tests
// assert the wire contract, refuses a call without a bearer the way the
// edge's JWT policy does, and implements the controller's idempotent create
// and FailedPrecondition while an Agent compiles.
type fakeKagent struct {
	a2apb.UnimplementedA2AServiceServer
	apiv1alpha1.UnimplementedAgentTemplateServiceServer
	apiv1alpha1.UnimplementedAgentServiceServer
	apiv1alpha1.UnimplementedSessionServiceServer
	apiv1alpha1.UnimplementedSystemServiceServer

	mu sync.Mutex

	templates []*apiv1alpha1.AgentTemplate
	agents    []*apiv1alpha1.Agent
	sessions  map[string]*apiv1alpha1.Session
	byRequest map[string]string
	tasks     map[a2a.TaskID]*a2a.Task
	// events are played back by SendStreamingMessage; eventsByTask when the
	// message resumes a task.
	events       []a2a.Event
	eventsByTask map[a2a.TaskID][]a2a.Event
	// compiling makes CreateSession answer FailedPrecondition that many
	// times first.
	compiling int

	// substrate is the summary GetSubstrateSummary answers; actorPages the
	// pages ListSubstrateActors walks, token "p<n>" naming page n.
	substrate  *apiv1alpha1.GetSubstrateSummaryResponse
	actorPages []*apiv1alpha1.ListSubstrateActorsResponse
	// legacy, when set, makes the fake a kagent 1.0 controller: the 1.1
	// Substrate RPCs answer Unimplemented and GetSubstrateStatus answers it.
	legacy *kagentv10.GetSubstrateStatusResponse

	calls    map[string][]metadata.MD
	sent     []*a2a.Message
	tenants  []string
	canceled []a2a.TaskID
	deleted  []string
}

func newFakeKagent() *fakeKagent {
	return &fakeKagent{
		sessions:     map[string]*apiv1alpha1.Session{},
		byRequest:    map[string]string{},
		tasks:        map[a2a.TaskID]*a2a.Task{},
		eventsByTask: map[a2a.TaskID][]a2a.Event{},
		calls:        map[string][]metadata.MD{},
	}
}

// serve starts the fake on a bufconn and returns a client dialled to it as
// the person whose token is given.
func (f *fakeKagent) serve(t *testing.T, token string) *kagentAPI {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(f.legacySubstrateStatus))
	a2apb.RegisterA2AServiceServer(srv, f)
	apiv1alpha1.RegisterAgentTemplateServiceServer(srv, f)
	apiv1alpha1.RegisterAgentServiceServer(srv, f)
	apiv1alpha1.RegisterSessionServiceServer(srv, f)
	apiv1alpha1.RegisterSystemServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	api, err := newKagentAPI(conn, token)
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func (f *fakeKagent) record(ctx context.Context, method string) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method] = append(f.calls[method], md)
}

func (f *fakeKagent) lastMD(method string) metadata.MD {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls[method]
	if len(calls) == 0 {
		return nil
	}
	return calls[len(calls)-1]
}

// edge mirrors the JWT policy: no bearer, no call; the person is whoever the
// bearer says (the token itself, for the tests).
func edge(ctx context.Context) (string, error) {
	auth := metadata.ValueFromIncomingContext(ctx, authorizationMetadata)
	if len(auth) != 1 || !strings.HasPrefix(auth[0], "Bearer ") {
		return "", status.Error(codes.Unauthenticated, "no bearer token found")
	}
	return strings.TrimPrefix(auth[0], "Bearer "), nil
}

// routed mirrors the controller's A2A gateway: the bearer, and the tenant
// `<namespace>/<name>` naming an Agent one of the person's sessions is of.
func (f *fakeKagent) routed(ctx context.Context, tenant string) error {
	if _, err := edge(ctx); err != nil {
		return err
	}
	namespace, name, ok := strings.Cut(tenant, "/")
	if !ok {
		return status.Error(codes.InvalidArgument, "Agent tenant must be namespace/name")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants = append(f.tenants, tenant)
	for _, session := range f.sessions {
		if session.GetAgent().GetNamespace() == namespace && session.GetAgent().GetName() == name {
			return nil
		}
	}
	return status.Error(codes.PermissionDenied, "not authorized")
}

// sessionOf mirrors the gateway's resolution of a message: its context id is
// the session's, or a paused task's session.
func (f *fakeKagent) sessionOf(msg *a2a.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if msg.TaskID != "" {
		return nil
	}
	for _, session := range f.sessions {
		if session.GetContextId() == msg.ContextID {
			return nil
		}
	}
	return status.Errorf(codes.PermissionDenied, "no session with context %q", msg.ContextID)
}

func (f *fakeKagent) SendStreamingMessage(req *a2apb.SendMessageRequest, stream grpc.ServerStreamingServer[a2apb.StreamResponse]) error {
	ctx := stream.Context()
	f.record(ctx, "SendStreamingMessage")
	if err := f.routed(ctx, req.GetTenant()); err != nil {
		return err
	}
	msg, err := pbconv.FromProtoMessage(req.GetMessage())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if err := f.sessionOf(msg); err != nil {
		return err
	}
	f.mu.Lock()
	f.sent = append(f.sent, msg)
	events := f.events
	if msg.TaskID != "" {
		events = f.eventsByTask[msg.TaskID]
	}
	f.mu.Unlock()
	for _, ev := range events {
		pb, err := pbconv.ToProtoStreamResponse(ev)
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		if err := stream.Send(pb); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeKagent) GetTask(ctx context.Context, req *a2apb.GetTaskRequest) (*a2apb.Task, error) {
	f.record(ctx, "GetTask")
	if err := f.routed(ctx, req.GetTenant()); err != nil {
		return nil, err
	}
	f.mu.Lock()
	task, ok := f.tasks[a2a.TaskID(req.GetId())]
	f.mu.Unlock()
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	return pbconv.ToProtoTask(task)
}

func (f *fakeKagent) ListTasks(ctx context.Context, req *a2apb.ListTasksRequest) (*a2apb.ListTasksResponse, error) {
	f.record(ctx, "ListTasks")
	if err := f.routed(ctx, req.GetTenant()); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &a2apb.ListTasksResponse{}
	for _, task := range f.tasks {
		pb, err := pbconv.ToProtoTask(task)
		if err != nil {
			return nil, err
		}
		resp.Tasks = append(resp.Tasks, pb)
	}
	return resp, nil
}

// setTaskState scripts the controller's view of one task — under the lock,
// since the web fakes flip states from their handler goroutines.
func (f *fakeKagent) setTaskState(id a2a.TaskID, state a2a.TaskState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[id] = &a2a.Task{ID: id, ContextID: "ctx-" + string(id), Status: a2a.TaskStatus{State: state}}
}

func (f *fakeKagent) CancelTask(ctx context.Context, req *a2apb.CancelTaskRequest) (*a2apb.Task, error) {
	f.record(ctx, "CancelTask")
	if err := f.routed(ctx, req.GetTenant()); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	task, ok := f.tasks[a2a.TaskID(req.GetId())]
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	f.canceled = append(f.canceled, task.ID)
	canceled := *task
	canceled.Status = a2a.TaskStatus{State: a2a.TaskStateCanceled}
	f.tasks[task.ID] = &canceled
	return pbconv.ToProtoTask(&canceled)
}

func (f *fakeKagent) ListAgentTemplates(ctx context.Context, req *apiv1alpha1.ListAgentTemplatesRequest) (*apiv1alpha1.ListAgentTemplatesResponse, error) {
	f.record(ctx, "ListAgentTemplates")
	if _, err := edge(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*apiv1alpha1.AgentTemplate
	for _, t := range f.templates {
		if t.GetRef().GetNamespace() == req.GetNamespace() {
			out = append(out, t)
		}
	}
	return &apiv1alpha1.ListAgentTemplatesResponse{AgentTemplates: out}, nil
}

func (f *fakeKagent) ListAgents(ctx context.Context, req *apiv1alpha1.ListAgentsRequest) (*apiv1alpha1.ListAgentsResponse, error) {
	f.record(ctx, "ListAgents")
	if _, err := edge(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*apiv1alpha1.Agent
	for _, a := range f.agents {
		if a.GetRef().GetNamespace() == req.GetNamespace() {
			out = append(out, a)
		}
	}
	return &apiv1alpha1.ListAgentsResponse{Agents: out}, nil
}

func (f *fakeKagent) GetSubstrateSummary(ctx context.Context, _ *apiv1alpha1.GetSubstrateSummaryRequest) (*apiv1alpha1.GetSubstrateSummaryResponse, error) {
	f.record(ctx, "GetSubstrateSummary")
	if _, err := edge(ctx); err != nil {
		return nil, err
	}
	if f.legacy != nil {
		return nil, status.Error(codes.Unimplemented, "unknown method GetSubstrateSummary for service kagent.api.v1alpha1.SystemService")
	}
	return f.substrate, nil
}

func (f *fakeKagent) ListSubstrateActors(ctx context.Context, req *apiv1alpha1.ListSubstrateActorsRequest) (*apiv1alpha1.ListSubstrateActorsResponse, error) {
	f.record(ctx, "ListSubstrateActors")
	if _, err := edge(ctx); err != nil {
		return nil, err
	}
	if limit := req.GetPage().GetLimit(); limit > 100 {
		return nil, status.Errorf(codes.InvalidArgument, "page.limit %d above 100", limit)
	}
	page := 0
	if token := req.GetPage().GetPageToken(); token != "" {
		if _, err := fmt.Sscanf(token, "p%d", &page); err != nil || page >= len(f.actorPages) {
			return nil, status.Errorf(codes.InvalidArgument, "page token %q", token)
		}
	}
	if len(f.actorPages) == 0 {
		return &apiv1alpha1.ListSubstrateActorsResponse{}, nil
	}
	return f.actorPages[page], nil
}

// legacySubstrateStatus serves the kagent 1.0 line's GetSubstrateStatus,
// which the 1.1 SystemService no longer registers, when the fake is legacy.
func (f *fakeKagent) legacySubstrateStatus(_ any, stream grpc.ServerStream) error {
	method, _ := grpc.MethodFromServerStream(stream)
	if method != legacySubstrateStatusMethod || f.legacy == nil {
		return status.Errorf(codes.Unimplemented, "unknown method %s", method)
	}
	f.record(stream.Context(), "GetSubstrateStatus")
	if _, err := edge(stream.Context()); err != nil {
		return err
	}
	if err := stream.RecvMsg(&kagentv10.GetSubstrateStatusRequest{}); err != nil {
		return err
	}
	return stream.SendMsg(f.legacy)
}

func (f *fakeKagent) GetCurrentUser(ctx context.Context, _ *apiv1alpha1.GetCurrentUserRequest) (*apiv1alpha1.GetCurrentUserResponse, error) {
	f.record(ctx, "GetCurrentUser")
	who, err := edge(ctx)
	if err != nil {
		return nil, err
	}
	// The edge rewrites x-user-id from the token; the fake echoes what it
	// would have set (the token) and what the caller sent, so a test sees
	// both sides of the contract.
	claims := map[string]any{"email": who}
	if sent := metadata.ValueFromIncomingContext(ctx, userIDHeader); len(sent) == 1 {
		claims["sent_x_user_id"] = sent[0]
	}
	value, err := structpb.NewStruct(claims)
	if err != nil {
		return nil, err
	}
	return &apiv1alpha1.GetCurrentUserResponse{Claims: value}, nil
}

func (f *fakeKagent) CreateSession(ctx context.Context, req *apiv1alpha1.CreateSessionRequest) (*apiv1alpha1.CreateSessionResponse, error) {
	f.record(ctx, "CreateSession")
	who, err := edge(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.compiling > 0 {
		f.compiling--
		return nil, status.Error(codes.FailedPrecondition, "Agent does not have a ready prepared revision")
	}
	key := who + "|" + req.GetRequestId()
	if id, ok := f.byRequest[key]; ok {
		return &apiv1alpha1.CreateSessionResponse{Session: f.sessions[id]}, nil
	}
	n := len(f.sessions) + 1
	session := &apiv1alpha1.Session{
		Id:        fmt.Sprintf("0192f1c2-7d1e-7a3b-9c4d-%012d", n),
		Creator:   who,
		Agent:     req.GetAgent(),
		State:     apiv1alpha1.RuntimeState_RUNTIME_STATE_READY,
		Operation: apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE,
	}
	session.ContextId = session.Id
	f.sessions[session.Id] = session
	f.byRequest[key] = session.Id
	return &apiv1alpha1.CreateSessionResponse{Session: session}, nil
}

func (f *fakeKagent) GetSession(ctx context.Context, req *apiv1alpha1.GetSessionRequest) (*apiv1alpha1.GetSessionResponse, error) {
	f.record(ctx, "GetSession")
	if _, err := edge(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	session, ok := f.sessions[req.GetSessionId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "Session not found")
	}
	return &apiv1alpha1.GetSessionResponse{Session: session}, nil
}

func (f *fakeKagent) ListSessions(ctx context.Context, req *apiv1alpha1.ListSessionsRequest) (*apiv1alpha1.ListSessionsResponse, error) {
	f.record(ctx, "ListSessions")
	// The controller's validation of the page.
	if limit := req.GetPage().GetLimit(); limit < 0 || limit > 100 {
		return nil, status.Errorf(codes.InvalidArgument, "validation error: page.limit: must be greater than or equal to 0 and less than or equal to 100")
	}
	who, err := edge(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*apiv1alpha1.Session
	for _, session := range f.sessions {
		if session.GetCreator() != who {
			continue
		}
		if agent := req.GetAgent(); agent != nil && (session.GetAgent().GetNamespace() != agent.GetNamespace() || session.GetAgent().GetName() != agent.GetName()) {
			continue
		}
		out = append(out, session)
	}
	return &apiv1alpha1.ListSessionsResponse{Sessions: out, Page: &apiv1alpha1.PageResponse{}}, nil
}

func (f *fakeKagent) DeleteSession(ctx context.Context, req *apiv1alpha1.DeleteSessionRequest) (*apiv1alpha1.DeleteSessionResponse, error) {
	f.record(ctx, "DeleteSession")
	if _, err := edge(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	session, ok := f.sessions[req.GetSessionId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "Session not found")
	}
	f.deleted = append(f.deleted, req.GetSessionId())
	delete(f.sessions, req.GetSessionId())
	return &apiv1alpha1.DeleteSessionResponse{Session: session}, nil
}

// readyFake is a fake with one session of the proof's Agent, the shape most
// tests start from.
func readyFake(t *testing.T) *fakeKagent {
	t.Helper()
	f := newFakeKagent()
	f.sessions[fakeSessionID] = fakeSession(fakeSessionID, fakeToken, a2aTestAgent)
	return f
}

// fakeSession is one READY session of an Agent of the kagent namespace, as
// the controller serves it to the person whose bearer is given; its context
// id is fakeContextID for fakeSessionID and the id itself otherwise.
func fakeSession(id, creator, agent string) *apiv1alpha1.Session {
	contextID := id
	if id == fakeSessionID {
		contextID = fakeContextID
	}
	return &apiv1alpha1.Session{
		Id: id, Creator: creator, ContextId: contextID,
		Agent:     &apiv1alpha1.ResourceReference{Namespace: kagentNamespace, Name: agent},
		State:     apiv1alpha1.RuntimeState_RUNTIME_STATE_READY,
		Operation: apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE,
	}
}

// fakeTemplate builds an AgentTemplate the way the controller serves it: the
// whole CR as a StructuredObject plus the denormalised fields.
func fakeTemplate(t *testing.T, name string, annotations map[string]any) *apiv1alpha1.AgentTemplate {
	t.Helper()
	meta := map[string]any{nameKey: name, fieldNamespace: kagentNamespace}
	if annotations != nil {
		meta["annotations"] = annotations
	}
	value, err := structpb.NewStruct(map[string]any{
		fieldAPIVersion: agentTemplateAPIVersion, fieldKind: kindAgentTemplate, fieldMetadata: meta,
		fieldSpec: map[string]any{descriptionKey: "Proof agent " + name},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &apiv1alpha1.AgentTemplate{
		Ref:         &apiv1alpha1.ResourceReference{Namespace: kagentNamespace, Name: name},
		Resource:    &apiv1alpha1.StructuredObject{ApiVersion: agentTemplateAPIVersion, Kind: kindAgentTemplate, Value: value},
		Description: "Proof agent " + name,
	}
}

// fakeAgent builds an Agent the way the controller serves it: the whole CR
// as a StructuredObject, referencing the template and the Harness, with the
// annotations and the Ready condition given (nil conditions: no status yet).
func fakeAgent(t *testing.T, name, template, harness string, annotations map[string]any, conditions []any) *apiv1alpha1.Agent {
	t.Helper()
	meta := map[string]any{nameKey: name, fieldNamespace: kagentNamespace}
	if annotations != nil {
		meta["annotations"] = annotations
	}
	status := map[string]any{}
	if conditions != nil {
		status[crConditions] = conditions
	}
	value, err := structpb.NewStruct(map[string]any{
		fieldAPIVersion: kagentAPIVersion, fieldKind: kindAgent, fieldMetadata: meta,
		fieldSpec:   map[string]any{"templateRef": map[string]any{nameKey: template}, "harnessRef": map[string]any{nameKey: harness}},
		fieldStatus: status,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &apiv1alpha1.Agent{
		Ref:      &apiv1alpha1.ResourceReference{Namespace: kagentNamespace, Name: name},
		Resource: &apiv1alpha1.StructuredObject{ApiVersion: kagentAPIVersion, Kind: kindAgent, Value: value},
	}
}

// fakeReady is an Agent's Ready condition as the controller writes it.
func fakeReady(ready bool, message string) []any {
	st := condFalseStatus
	if ready {
		st = conditionTrue
	}
	return []any{map[string]any{fieldType: conditionReady, fieldStatus: st, fieldReason: "Compiled", fieldMessage: message}}
}

// TestKagentAPIWireContract: every A2A call rides the person's bearer and
// the HITL extension request as gRPC metadata and names the Agent as its
// tenant; the message carries the session's context id; the events fold
// into the answer.
func TestKagentAPIWireContract(t *testing.T) {
	f := readyFake(t)
	info := a2a.TaskInfo{TaskID: fakeTaskID, ContextID: fakeContextID}
	f.events = []a2a.Event{
		&a2a.Task{ID: fakeTaskID, ContextID: fakeContextID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}},
		a2a.NewStatusUpdateEvent(info, a2a.TaskStateWorking, nil),
		a2a.NewArtifactEvent(info, a2a.NewTextPart("po")),
		a2a.NewArtifactEvent(info, a2a.NewDataPart(map[string]any{nameKey: fakeTool})),
		a2a.NewStatusUpdateEvent(info, a2a.TaskStateCompleted, nil),
	}
	api := f.serve(t, fakeToken)

	got, err := api.turn(t.Context(), f.sessions[fakeSessionID], userMessage("ping"))
	if err != nil {
		t.Fatal(err)
	}
	if got.state() != a2a.TaskStateCompleted || got.text() != "po" || got.statesString() != "submitted → working → completed" {
		t.Errorf("turn = state %s, text %q, states %s", got.state(), got.text(), got.statesString())
	}
	if reply, err := got.completedText(); err != nil || reply != "po" {
		t.Errorf("completedText = %q, %v", reply, err)
	}
	md := f.lastMD("SendStreamingMessage")
	if want := []string{"Bearer " + fakeToken}; !slices.Equal(md.Get(authorizationMetadata), want) {
		t.Errorf("authorization = %v", md.Get(authorizationMetadata))
	}
	if want := []string{kagentNamespace + "/" + a2aTestAgent}; !slices.Equal(f.tenants, want) {
		t.Errorf("the Agent as the tenant wanted, got %v", f.tenants)
	}
	if !slices.Contains(md.Get("a2a-extensions"), hitlExtensionURI) {
		t.Errorf("the HITL extension is not requested: a2a-extensions = %v", md.Get("a2a-extensions"))
	}
	if len(f.sent) != 1 || f.sent[0].ContextID != fakeContextID || f.sent[0].Parts[0].Text() != "ping" || f.sent[0].TaskID != "" {
		t.Errorf("sent %+v", f.sent)
	}
}

// TestKagentAPIWithoutToken: the edge refuses every call without a bearer
// as Unauthenticated, and the client sends none for nobody.
func TestKagentAPIWithoutToken(t *testing.T) {
	f := readyFake(t)
	api := f.serve(t, "")
	_, err := api.currentUser(t.Context())
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("GetCurrentUser without a token: %v", err)
	}
	if got := f.lastMD("GetCurrentUser").Get(authorizationMetadata); len(got) != 0 {
		t.Errorf("a client for nobody sent authorization %v", got)
	}
	_, err = api.turn(t.Context(), f.sessions[fakeSessionID], userMessage("ping"))
	if !isUnauthenticated(err) {
		t.Errorf("SendStreamingMessage without a token: %v", err)
	}
}

// TestKagentAPIForgedUserID: the extra metadata rides beside the token on
// the kagent services and the A2A calls alike (what the identity proof
// sends; the edge's replacing it is the lab's assertion).
func TestKagentAPIForgedUserID(t *testing.T) {
	f := readyFake(t)
	api := f.serve(t, fakeToken)
	api.extra = metadata.Pairs(userIDHeader, a2aTestForgedUser)
	claims, err := api.currentUser(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if principalOf(claims) != fakeToken || claims["sent_x_user_id"] != a2aTestForgedUser {
		t.Errorf("claims = %v", claims)
	}
	if _, err := api.getTask(t.Context(), f.sessions[fakeSessionID], "nothing"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("GetTask: %v", err)
	}
	if got := f.lastMD("GetTask").Get(userIDHeader); !slices.Equal(got, []string{a2aTestForgedUser}) {
		t.Errorf("the A2A call carried %s=%v", userIDHeader, got)
	}
	if principalOf(map[string]any{"sub": "s"}) != "s" || principalOf(nil) != "" {
		t.Error("principalOf falls back to sub, then to nothing")
	}
}

// TestCreateSession: the create waits through FailedPrecondition while the
// Agent compiles, is idempotent on request_id, lists (all, or one Agent's)
// and deletes.
func TestCreateSession(t *testing.T) {
	f := newFakeKagent()
	f.compiling = 1
	api := f.serve(t, fakeToken)
	first, err := api.createSession(t.Context(), a2aTestAgent, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.GetCreator() != fakeToken || first.GetAgent().GetName() != a2aTestAgent || first.GetAgent().GetNamespace() != kagentNamespace || sessionState(first) != "READY" {
		t.Errorf("created %v", first)
	}
	if calls := len(f.calls["CreateSession"]); calls != 2 {
		t.Errorf("FailedPrecondition was not waited through: %d creates", calls)
	}
	again, err := api.createSession(t.Context(), a2aTestAgent, "req-1")
	if err != nil || again.GetId() != first.GetId() {
		t.Errorf("the same request_id: %v %v", again.GetId(), err)
	}
	other, err := api.createSession(t.Context(), a2aTestAgent, "req-2")
	if err != nil || other.GetId() == first.GetId() {
		t.Errorf("another request_id: %v %v", other.GetId(), err)
	}
	coding, err := api.createSession(t.Context(), fakeCodingAgent, "req-3")
	if err != nil || coding.GetAgent().GetName() != fakeCodingAgent {
		t.Errorf("a session of another Agent: %v %v", coding.GetAgent(), err)
	}
	listed, err := api.listSessions(t.Context())
	if err != nil || len(listed) != 3 {
		t.Errorf("listed %d (%v)", len(listed), err)
	}
	ofAgent, err := api.listSessionsOf(t.Context(), agentRef(a2aTestAgent))
	if err != nil || len(ofAgent) != 2 {
		t.Errorf("listed %d of %s (%v)", len(ofAgent), a2aTestAgent, err)
	}
	got, err := api.getSession(t.Context(), first.GetId())
	if err != nil || got.GetContextId() != first.GetId() {
		t.Errorf("GetSession: %v %v", got, err)
	}
	if err := api.deleteSession(t.Context(), first.GetId()); err != nil {
		t.Error(err)
	}
	if err := api.deleteSession(t.Context(), first.GetId()); err != nil {
		t.Errorf("deleting a deleted session is success: %v", err)
	}
	if !slices.Equal(f.deleted, []string{first.GetId()}) {
		t.Errorf("deleted %v", f.deleted)
	}
}

// TestHITLRoundTrip: a task paused at input-required carries the
// tool_approval_request; the decision resumes the paused task with one
// approval per tool under the extension URI; the rejection carries the
// reason; CancelTask answers the canceled task.
func TestHITLRoundTrip(t *testing.T) {
	f := readyFake(t)
	info := a2a.TaskInfo{TaskID: fakeTaskID, ContextID: fakeContextID}
	prompt := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Approve "+fakeTool+"?"))
	var request map[string]any
	if err := json.Unmarshal([]byte(`{"type":"tool_approval_request","hint":"Approve filter_tools?","tools":[{"id":"adk-1","call_id":"toolu_1","name":"filter_tools","args":{"limit":500}}]}`), &request); err != nil {
		t.Fatal(err)
	}
	prompt.SetMeta(hitlExtensionURI, request)
	prompt.Extensions = []string{hitlExtensionURI}
	f.events = []a2a.Event{
		&a2a.Task{ID: fakeTaskID, ContextID: fakeContextID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}},
		a2a.NewStatusUpdateEvent(info, a2a.TaskStateWorking, nil),
		a2a.NewStatusUpdateEvent(info, a2a.TaskStateInputRequired, prompt),
	}
	f.eventsByTask[fakeTaskID] = []a2a.Event{
		a2a.NewStatusUpdateEvent(info, a2a.TaskStateWorking, nil),
		a2a.NewArtifactEvent(info, a2a.NewTextPart("7 namespaces")),
		a2a.NewStatusUpdateEvent(info, a2a.TaskStateCompleted, nil),
	}
	f.tasks[fakeTaskID] = &a2a.Task{ID: fakeTaskID, ContextID: fakeContextID, Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: prompt}}
	api := f.serve(t, fakeToken)

	session := f.sessions[fakeSessionID]
	paused, err := api.turn(t.Context(), session, userMessage(a2aToolPrompt))
	if err != nil {
		t.Fatal(err)
	}
	if paused.state() != a2a.TaskStateInputRequired || paused.approval == nil || !slices.Equal(paused.approval.toolNames(), []string{fakeTool}) || paused.approval.Hint != "Approve "+fakeTool+"?" {
		t.Fatalf("paused = %s approval %+v", paused.state(), paused.approval)
	}
	if _, err := paused.completedText(); err == nil || !strings.Contains(err.Error(), fakeTool) {
		t.Errorf("a paused turn is no answer: %v", err)
	}

	resumed, err := api.decide(t.Context(), session, paused, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.state() != a2a.TaskStateCompleted || resumed.text() != "7 namespaces" {
		t.Errorf("resumed = %s %q", resumed.state(), resumed.text())
	}
	decision := f.sent[len(f.sent)-1]
	if decision.TaskID != fakeTaskID || decision.ContextID != "" || !slices.Contains(decision.Extensions, hitlExtensionURI) {
		t.Errorf("the decision message = %+v", decision)
	}
	if got := partsText(decision.Parts); got != "Approved: "+fakeTool {
		t.Errorf("the decision's transcript line = %q", got)
	}
	payload, _ := decision.Metadata[hitlExtensionURI].(map[string]any)
	approvals, _ := payload["approvals"].([]any)
	if payload["type"] != hitlTypeToolApprovalResponse || len(approvals) != 1 {
		t.Fatalf("payload = %v", payload)
	}
	if approval, _ := approvals[0].(map[string]any); approval["id"] != "adk-1" || approval["approved"] != true || approval["rejection_reason"] != nil {
		t.Errorf("approval = %v", approval)
	}

	rejection, err := decisionMessage(fakeTaskID, paused.approval, false, a2aDeclineReason)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ = rejection.Metadata[hitlExtensionURI].(map[string]any)
	approvals, _ = payload["approvals"].([]any)
	if approval, _ := approvals[0].(map[string]any); approval["approved"] != false || approval["rejection_reason"] != a2aDeclineReason {
		t.Errorf("rejection = %v", approval)
	}
	if got := partsText(rejection.Parts); got != "Rejected: "+fakeTool+"\nReason: "+a2aDeclineReason {
		t.Errorf("the rejection's transcript line = %q", got)
	}

	canceled, err := api.cancelTask(t.Context(), session, fakeTaskID)
	if err != nil || canceled.Status.State != a2a.TaskStateCanceled || !slices.Equal(f.canceled, []a2a.TaskID{fakeTaskID}) {
		t.Errorf("CancelTask: %v %v %v", canceled, err, f.canceled)
	}
	if got, err := api.getTask(t.Context(), session, fakeTaskID); err != nil || got.Status.State != a2a.TaskStateCanceled {
		t.Errorf("GetTask after the cancel: %v %v", got, err)
	}
	if req := parseToolApprovalRequest(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("plain"))); req != nil {
		t.Errorf("a plain message is no request: %+v", req)
	}
}

// TestListingOf: the roster entry of a listed Agent — the template and the
// Harness it pairs, the annotations (the Agent's, else its template's), Ready
// as selectable, the reason an Agent is not.
func TestListingOf(t *testing.T) {
	f := newFakeKagent()
	annotations := map[string]any{displayNameAnnotation: a2aTestDisplayName, iconURLAnnotation: a2aTestIconURL}
	f.templates = []*apiv1alpha1.AgentTemplate{
		fakeTemplate(t, a2aTestAgent, annotations),
		fakeTemplate(t, "compiling", nil),
	}
	f.agents = []*apiv1alpha1.Agent{
		fakeAgent(t, a2aTestAgent, a2aTestAgent, kagentHarness, nil, fakeReady(true, "")),
		fakeAgent(t, "compiling", "compiling", kagentHarness, nil, fakeReady(false, "waiting for the ActorTemplate golden snapshot")),
		fakeAgent(t, "annotated", "compiling", testOtherHarness, annotations, fakeReady(true, "")),
		fakeAgent(t, "no-status-yet", "compiling", kagentHarness, nil, nil),
	}
	api := f.serve(t, fakeToken)
	agents, templates, err := api.roster(t.Context())
	if err != nil || len(agents) != 4 || len(templates) != 2 {
		t.Fatalf("listed %d agents, %d templates: %v", len(agents), len(templates), err)
	}
	for _, method := range []string{"ListAgents", "ListAgentTemplates"} {
		if got := f.lastMD(method).Get(authorizationMetadata); !slices.Equal(got, []string{"Bearer " + fakeToken}) {
			t.Errorf("%s carried authorization %v", method, got)
		}
	}
	ready, err := findListing(agents, templates, a2aTestAgent)
	if err != nil {
		t.Fatal(err)
	}
	want := agentListing{Name: a2aTestAgent, Namespace: kagentNamespace, Template: a2aTestAgent, Harness: kagentHarness, DisplayName: a2aTestDisplayName, IconURL: a2aTestIconURL, Description: "Proof agent " + a2aTestAgent}
	if ready != want {
		t.Errorf("listing = %+v, want %+v", ready, want)
	}
	annotated, err := findListing(agents, templates, "annotated")
	if err != nil || annotated.Harness != testOtherHarness || annotated.DisplayName != a2aTestDisplayName || annotated.Description != "Proof agent compiling" || annotated.Unavailable != "" {
		t.Errorf("an Agent's own annotations: %+v %v", annotated, err)
	}
	for name, reason := range map[string]string{
		"compiling":     "has not compiled a ready revision: waiting for the ActorTemplate golden snapshot",
		"no-status-yet": "no Ready condition reported yet",
	} {
		l, err := findListing(agents, templates, name)
		if err != nil || !strings.Contains(l.Unavailable, reason) {
			t.Errorf("%s: %+v %v", name, l, err)
		}
	}
	if _, err := findListing(agents, templates, "nobody"); err == nil || !strings.Contains(err.Error(), "does not list nobody") {
		t.Errorf("an unknown Agent: %v", err)
	}
}

// TestTurnFolding: an appended artifact chunk joins its artifact, a
// re-sent one replaces it, a bare Message answer is the text, a failed task
// is an error naming the state and the agent's words.
func TestTurnFolding(t *testing.T) {
	info := a2a.TaskInfo{TaskID: fakeTaskID, ContextID: fakeContextID}
	chunked := &turn{}
	first := a2a.NewArtifactEvent(info, a2a.NewTextPart("Hello"))
	more := a2a.NewArtifactEvent(info, a2a.NewTextPart(", world"))
	more.Artifact.ID, more.Append = first.Artifact.ID, true
	for _, ev := range []a2a.Event{
		&a2a.Task{ID: fakeTaskID, ContextID: fakeContextID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}},
		first, more, a2a.NewArtifactEvent(info, a2a.NewTextPart("second")),
		a2a.NewStatusUpdateEvent(info, a2a.TaskStateCompleted, a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("second"))),
	} {
		chunked.observe(ev)
	}
	if chunked.text() != "Hello, world\nsecond" {
		t.Errorf("chunked text = %q", chunked.text())
	}
	replaced := &turn{}
	replaced.observe(first)
	again := a2a.NewArtifactEvent(info, a2a.NewTextPart("Bye"))
	again.Artifact.ID = first.Artifact.ID
	replaced.observe(again)
	if replaced.text() != "Bye" {
		t.Errorf("replaced text = %q", replaced.text())
	}
	bare := &turn{}
	bare.observe(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("pong")))
	if reply, err := bare.completedText(); err != nil || reply != "pong" {
		t.Errorf("a bare message: %q %v", reply, err)
	}
	failed := &turn{}
	failed.observe(a2a.NewStatusUpdateEvent(info, a2a.TaskStateFailed, a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("the model refused"))))
	if _, err := failed.completedText(); err == nil || !strings.Contains(err.Error(), "ended failed") || !strings.Contains(err.Error(), "the model refused") {
		t.Errorf("a failed task: %v", err)
	}
	if _, err := (&turn{}).completedText(); err == nil || !strings.Contains(err.Error(), "without a task") {
		t.Errorf("an empty stream: %v", err)
	}
}

// TestKagentTarget: the edge's base URL becomes the gRPC authority — the
// port as published, 443 for a bare https.
func TestKagentTarget(t *testing.T) {
	if a2aService != a2apb.A2AService_ServiceDesc.ServiceName {
		t.Errorf("a2aService = %q, the descriptor says %q", a2aService, a2apb.A2AService_ServiceDesc.ServiceName)
	}
	for base, want := range map[string]string{
		"https://agentgateway.127.0.0.1.nip.io:8445": "agentgateway.127.0.0.1.nip.io:8445 tls",
		"https://agentgateway.example.com":           "agentgateway.example.com:443 tls",
		"http://agentgateway.example.com":            "agentgateway.example.com:80 plain",
	} {
		hostPort, useTLS, err := kagentTargetOf(base)
		if err != nil {
			t.Fatal(err)
		}
		got := hostPort + map[bool]string{true: " tls", false: " plain"}[useTLS]
		if got != want {
			t.Errorf("%s → %s, want %s", base, got, want)
		}
	}
	if _, _, err := kagentTargetOf("not a url"); err == nil {
		t.Error("a base URL without a host must fail")
	}
}

// TestSubstrateState: the summary's pools and templates and every page of
// actors ride the person's bearer; an empty page with a next token is walked
// through; an ate-api error on either read is carried beside the data; a
// token answered with itself ends the walk as an error.
func TestSubstrateState(t *testing.T) {
	actor := func(id string) *ateapi.Actor {
		return &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Atespace: kagentNamespace, Name: id}}
	}
	f := newFakeKagent()
	f.substrate = &apiv1alpha1.GetSubstrateSummaryResponse{
		WorkerPools:    []*apiv1alpha1.SubstrateWorkerPool{{Ref: &apiv1alpha1.ResourceReference{Namespace: kagentNamespace, Name: testWorkerPool}}},
		ActorTemplates: []*ateapi.ActorTemplate{{Metadata: &ateapi.ResourceMetadata{Atespace: kagentNamespace, Name: testTemplateName}}},
	}
	f.actorPages = []*apiv1alpha1.ListSubstrateActorsResponse{
		{Actors: []*ateapi.Actor{actor("a1")}, Page: &apiv1alpha1.PageResponse{NextPageToken: "p1"}},
		{Page: &apiv1alpha1.PageResponse{NextPageToken: "p2"}},
		{Actors: []*ateapi.Actor{actor("a2")}},
	}
	api := f.serve(t, fakeToken)

	state, err := api.substrateState(t.Context(), kagentNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.pools) != 1 || len(state.templates) != 1 || len(state.ateAPIErrors) != 0 {
		t.Errorf("state = %+v", state)
	}
	var ids []string
	for _, a := range state.actors {
		ids = append(ids, a.id)
	}
	if !slices.Equal(ids, []string{"a1", "a2"}) {
		t.Errorf("actors = %v, want every page's", ids)
	}
	if got := len(f.calls["ListSubstrateActors"]); got != 3 {
		t.Errorf("ListSubstrateActors called %d times, want 3", got)
	}
	for _, method := range []string{"GetSubstrateSummary", "ListSubstrateActors"} {
		if want := []string{"Bearer " + fakeToken}; !slices.Equal(f.lastMD(method).Get(authorizationMetadata), want) {
			t.Errorf("%s authorization = %v", method, f.lastMD(method).Get(authorizationMetadata))
		}
	}

	f.substrate = &apiv1alpha1.GetSubstrateSummaryResponse{AteApiError: "templates: refused"}
	f.actorPages = []*apiv1alpha1.ListSubstrateActorsResponse{{AteApiError: "actors: refused"}}
	state, err = api.substrateState(t.Context(), kagentNamespace)
	if err != nil || !slices.Equal(state.ateAPIErrors, []string{"templates: refused", "actors: refused"}) {
		t.Errorf("ate-api errors = %v, %v", state.ateAPIErrors, err)
	}

	f.substrate = &apiv1alpha1.GetSubstrateSummaryResponse{}
	f.actorPages = []*apiv1alpha1.ListSubstrateActorsResponse{
		{Page: &apiv1alpha1.PageResponse{NextPageToken: "p1"}},
		{Page: &apiv1alpha1.PageResponse{NextPageToken: "p1"}},
	}
	if _, err := api.substrateState(t.Context(), kagentNamespace); err == nil || !strings.Contains(err.Error(), "with itself") {
		t.Errorf("a token answered with itself: %v", err)
	}
}

// TestSubstrateStateLegacy: a controller of the kagent 1.0 line answers
// GetSubstrateSummary Unimplemented; the state is read through its
// GetSubstrateStatus on the same wire path, with the person's bearer.
func TestSubstrateStateLegacy(t *testing.T) {
	f := newFakeKagent()
	f.legacy = &kagentv10.GetSubstrateStatusResponse{
		WorkerPools:    []*kagentv10.SubstrateWorkerPool{{Namespace: kagentNamespace, Name: testWorkerPool, Replicas: 2, AteomImage: testAteomImage}},
		ActorTemplates: []*kagentv10.SubstrateActorTemplate{{Namespace: kagentNamespace, Name: testTemplateName, Phase: conditionReady, GoldenSnapshot: "s3://ate-snapshots/kagent/x"}},
		Actors: []*kagentv10.SubstrateActor{{
			ActorId: "a1", Status: "Resuming", ActorTemplateNamespace: kagentNamespace, ActorTemplateName: testTemplateName,
			AteomPodNamespace: kagentNamespace, AteomPodName: testWorkerPod, AteomPodIp: testWorkerIP,
		}},
		AteApiError: "workers: refused",
	}
	api := f.serve(t, fakeToken)

	state, err := api.substrateState(t.Context(), kagentNamespace)
	if err != nil {
		t.Fatal(err)
	}
	want := substrateState{
		pools:        []substratePool{{namespace: kagentNamespace, name: testWorkerPool, replicas: 2, image: testAteomImage}},
		templates:    []substrateTemplate{{namespace: kagentNamespace, name: testTemplateName, phase: conditionReady, golden: "golden snapshot s3://ate-snapshots/kagent/x"}},
		actors:       []substrateActor{{id: "a1", templateNamespace: kagentNamespace, templateName: testTemplateName, state: "Resuming", workerNamespace: kagentNamespace, workerPod: testWorkerPod, workerIP: testWorkerIP}},
		ateAPIErrors: []string{"workers: refused"},
	}
	if !reflect.DeepEqual(state, want) {
		t.Errorf("state = %+v\nwant    %+v", state, want)
	}
	if got := f.lastMD("GetSubstrateStatus").Get(authorizationMetadata); !slices.Equal(got, []string{"Bearer " + fakeToken}) {
		t.Errorf("GetSubstrateStatus authorization = %v", got)
	}
	if len(f.calls["ListSubstrateActors"]) != 0 {
		t.Error("the 1.0 path listed actors on the 1.1 RPC")
	}
}

// TestSubstrateConversions: a pool's replicas and image come off the CR; a
// template is Pending without a golden tag, Ready with one, Failed on
// Substrate's error; an actor's state loses its enum prefix and keeps its
// worker.
func TestSubstrateConversions(t *testing.T) {
	spec, err := structpb.NewStruct(map[string]any{"spec": map[string]any{"replicas": 4, "workerImage": "ateom:v1"}})
	if err != nil {
		t.Fatal(err)
	}
	pool := poolOf(&apiv1alpha1.SubstrateWorkerPool{
		Ref:      &apiv1alpha1.ResourceReference{Namespace: kagentNamespace, Name: testWorkerPool},
		Resource: &apiv1alpha1.StructuredObject{Kind: "WorkerPool", Value: spec},
	})
	if want := (substratePool{namespace: kagentNamespace, name: testWorkerPool, replicas: 4, image: "ateom:v1"}); pool != want {
		t.Errorf("pool = %+v", pool)
	}
	template := func(golden *ateapi.GoldenSnapshotStatus) substrateTemplate {
		return templateOf(&ateapi.ActorTemplate{
			Metadata: &ateapi.ResourceMetadata{Atespace: kagentNamespace, Name: "t"},
			Status:   &ateapi.ActorTemplateStatus{GoldenSnapshotStatus: golden},
		})
	}
	for _, c := range []struct {
		golden *ateapi.GoldenSnapshotStatus
		want   substrateTemplate
	}{
		{nil, substrateTemplate{namespace: kagentNamespace, name: "t", phase: "Pending"}},
		{&ateapi.GoldenSnapshotStatus{GoldenTag: &ateapi.ObjectRef{Atespace: "ate-golden", Name: "g"}}, substrateTemplate{namespace: kagentNamespace, name: "t", phase: conditionReady, golden: "golden tag ate-golden/g"}},
		{&ateapi.GoldenSnapshotStatus{ErrorMessage: "git fetch: 401"}, substrateTemplate{namespace: kagentNamespace, name: "t", phase: templatePhaseFailed, failure: "git fetch: 401"}},
	} {
		if got := template(c.golden); got != c.want {
			t.Errorf("templateOf(%v) = %+v, want %+v", c.golden, got, c.want)
		}
	}
	got := actorOf(&ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: kagentNamespace, Name: "a1"},
		ActorTemplate: &ateapi.ObjectRef{Atespace: kagentNamespace, Name: "t"},
		Status: &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_RUNNING, WorkerAssignment: &ateapi.WorkerAssignment{
			WorkerNamespace: kagentNamespace, WorkerPod: testWorkerPod, WorkerPodIp: testWorkerIP,
		}},
	})
	if want := (substrateActor{id: "a1", templateNamespace: kagentNamespace, templateName: "t", state: "RUNNING", workerNamespace: kagentNamespace, workerPod: testWorkerPod, workerIP: testWorkerIP}); got != want {
		t.Errorf("actor = %+v", got)
	}
}

// TestDecideUntilSettled: the decisions loop counts no budget of its own —
// a task resumed to completed settles, one that answers a decision with the
// same request again did not resume and fails.
func TestDecideUntilSettled(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resumed  func(info a2a.TaskInfo, prompt *a2a.Message) []a2a.Event
		wantErr  string
		wantDone bool
	}{
		{
			name: "resumed to completed",
			resumed: func(info a2a.TaskInfo, _ *a2a.Message) []a2a.Event {
				return []a2a.Event{
					a2a.NewStatusUpdateEvent(info, a2a.TaskStateWorking, nil),
					a2a.NewArtifactEvent(info, a2a.NewTextPart("7 namespaces")),
					a2a.NewStatusUpdateEvent(info, a2a.TaskStateCompleted, nil),
				}
			},
			wantDone: true,
		},
		{
			name: "the same request again",
			resumed: func(info a2a.TaskInfo, prompt *a2a.Message) []a2a.Event {
				return []a2a.Event{
					a2a.NewStatusUpdateEvent(info, a2a.TaskStateWorking, nil),
					a2a.NewStatusUpdateEvent(info, a2a.TaskStateInputRequired, prompt),
				}
			},
			wantErr: "did not resume",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := readyFake(t)
			info := a2a.TaskInfo{TaskID: fakeTaskID, ContextID: fakeContextID}
			prompt := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Approve "+fakeTool+"?"))
			var request map[string]any
			if err := json.Unmarshal([]byte(`{"type":"tool_approval_request","tools":[{"id":"adk-1","call_id":"toolu_1","name":"filter_tools","args":{}}]}`), &request); err != nil {
				t.Fatal(err)
			}
			prompt.SetMeta(hitlExtensionURI, request)
			prompt.Extensions = []string{hitlExtensionURI}
			f.events = []a2a.Event{
				&a2a.Task{ID: fakeTaskID, ContextID: fakeContextID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}},
				a2a.NewStatusUpdateEvent(info, a2a.TaskStateInputRequired, prompt),
			}
			f.eventsByTask[fakeTaskID] = tc.resumed(info, prompt)
			f.tasks[fakeTaskID] = &a2a.Task{ID: fakeTaskID, ContextID: fakeContextID, Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: prompt}}
			api := f.serve(t, fakeToken)

			paused, err := api.turnOn(f.sessions[fakeSessionID], userMessage(a2aToolPrompt))
			if err != nil {
				t.Fatal(err)
			}
			settled, decided, err := api.decideUntilSettled(f.sessions[fakeSessionID], paused, true, "")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if settled.state() != a2a.TaskStateCompleted || !slices.Equal(decided, []string{fakeTool}) {
				t.Errorf("settled = %s, decided %v", settled.state(), decided)
			}
		})
	}
}
