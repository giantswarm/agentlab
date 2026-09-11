package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

const (
	fakeInstanceID    = "0192f1c2-7d1e-7a3b-9c4d-000000000001"
	fakeToken         = "user-jwt"
	fakeTaskID        = "task-1"
	fakeContextID     = "ctx-1"
	fakeTool          = "filter_tools"
	kindAgentTemplate = "AgentTemplate"
)

// fakeKagent is an in-process kagent API v2 controller behind a fake edge:
// the A2A v1 service and the AgentTemplate, AgentInstance and System
// services on a bufconn. It records the metadata of every call so the tests
// assert the wire contract, refuses a call without a bearer the way the
// edge's JWT policy does, and implements the controller's idempotent create
// and FailedPrecondition while a template compiles.
type fakeKagent struct {
	a2apb.UnimplementedA2AServiceServer
	apiv1alpha1.UnimplementedAgentTemplateServiceServer
	apiv1alpha1.UnimplementedAgentInstanceServiceServer
	apiv1alpha1.UnimplementedSystemServiceServer

	mu sync.Mutex

	templates []*apiv1alpha1.AgentTemplate
	instances map[string]*apiv1alpha1.AgentInstance
	byRequest map[string]string
	tasks     map[a2a.TaskID]*a2a.Task
	// events are played back by SendStreamingMessage; eventsByTask when the
	// message resumes a task.
	events       []a2a.Event
	eventsByTask map[a2a.TaskID][]a2a.Event
	// compiling makes CreateAgentInstance answer FailedPrecondition that
	// many times first.
	compiling int

	calls    map[string][]metadata.MD
	sent     []*a2a.Message
	canceled []a2a.TaskID
	deleted  []string
}

func newFakeKagent() *fakeKagent {
	return &fakeKagent{
		instances:    map[string]*apiv1alpha1.AgentInstance{},
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
	srv := grpc.NewServer()
	a2apb.RegisterA2AServiceServer(srv, f)
	apiv1alpha1.RegisterAgentTemplateServiceServer(srv, f)
	apiv1alpha1.RegisterAgentInstanceServiceServer(srv, f)
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

// routed mirrors the controller's A2A gateway: exactly one instance id, the
// bearer, an instance the person owns.
func (f *fakeKagent) routed(ctx context.Context) (*apiv1alpha1.AgentInstance, error) {
	if _, err := edge(ctx); err != nil {
		return nil, err
	}
	ids := metadata.ValueFromIncomingContext(ctx, instanceIDMetadata)
	if len(ids) != 1 {
		return nil, status.Errorf(codes.InvalidArgument, "exactly one %s header is required", instanceIDMetadata)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.instances[ids[0]]
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "not authorized")
	}
	return inst, nil
}

func (f *fakeKagent) SendStreamingMessage(req *a2apb.SendMessageRequest, stream grpc.ServerStreamingServer[a2apb.StreamResponse]) error {
	ctx := stream.Context()
	f.record(ctx, "SendStreamingMessage")
	if _, err := f.routed(ctx); err != nil {
		return err
	}
	msg, err := pbconv.FromProtoMessage(req.GetMessage())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
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
	if _, err := f.routed(ctx); err != nil {
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

func (f *fakeKagent) ListTasks(ctx context.Context, _ *a2apb.ListTasksRequest) (*a2apb.ListTasksResponse, error) {
	f.record(ctx, "ListTasks")
	if _, err := f.routed(ctx); err != nil {
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
	if _, err := f.routed(ctx); err != nil {
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

func (f *fakeKagent) CreateAgentInstance(ctx context.Context, req *apiv1alpha1.CreateAgentInstanceRequest) (*apiv1alpha1.CreateAgentInstanceResponse, error) {
	f.record(ctx, "CreateAgentInstance")
	who, err := edge(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.compiling > 0 {
		f.compiling--
		return nil, status.Error(codes.FailedPrecondition, "AgentTemplate and Harness do not have a ready prepared revision")
	}
	key := who + "|" + req.GetRequestId()
	if id, ok := f.byRequest[key]; ok {
		return &apiv1alpha1.CreateAgentInstanceResponse{AgentInstance: f.instances[id]}, nil
	}
	inst := &apiv1alpha1.AgentInstance{
		Id:            fmt.Sprintf("0192f1c2-7d1e-7a3b-9c4d-%012d", len(f.instances)+1),
		Creator:       who,
		Harness:       req.GetHarness(),
		AgentTemplate: req.GetAgentTemplate(),
		State:         apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY,
		ContextId:     fmt.Sprintf("ctx-%d", len(f.instances)+1),
	}
	f.instances[inst.Id] = inst
	f.byRequest[key] = inst.Id
	return &apiv1alpha1.CreateAgentInstanceResponse{AgentInstance: inst}, nil
}

func (f *fakeKagent) GetAgentInstance(ctx context.Context, req *apiv1alpha1.GetAgentInstanceRequest) (*apiv1alpha1.GetAgentInstanceResponse, error) {
	f.record(ctx, "GetAgentInstance")
	if _, err := edge(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.instances[req.GetAgentInstanceId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "AgentInstance not found")
	}
	return &apiv1alpha1.GetAgentInstanceResponse{AgentInstance: inst}, nil
}

func (f *fakeKagent) ListAgentInstances(ctx context.Context, req *apiv1alpha1.ListAgentInstancesRequest) (*apiv1alpha1.ListAgentInstancesResponse, error) {
	f.record(ctx, "ListAgentInstances")
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
	var out []*apiv1alpha1.AgentInstance
	for _, inst := range f.instances {
		if inst.GetCreator() != who {
			continue
		}
		if tpl := req.GetAgentTemplate(); tpl != nil && (inst.GetAgentTemplate().GetNamespace() != tpl.GetNamespace() || inst.GetAgentTemplate().GetName() != tpl.GetName()) {
			continue
		}
		out = append(out, inst)
	}
	return &apiv1alpha1.ListAgentInstancesResponse{AgentInstances: out, Page: &apiv1alpha1.PageResponse{}}, nil
}

func (f *fakeKagent) DeleteAgentInstance(ctx context.Context, req *apiv1alpha1.DeleteAgentInstanceRequest) (*apiv1alpha1.DeleteAgentInstanceResponse, error) {
	f.record(ctx, "DeleteAgentInstance")
	if _, err := edge(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.instances[req.GetAgentInstanceId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "AgentInstance not found")
	}
	f.deleted = append(f.deleted, req.GetAgentInstanceId())
	delete(f.instances, req.GetAgentInstanceId())
	return &apiv1alpha1.DeleteAgentInstanceResponse{AgentInstance: inst}, nil
}

// readyFake is a fake with one instance of the proof's template, the shape
// most tests start from.
func readyFake(t *testing.T) *fakeKagent {
	t.Helper()
	f := newFakeKagent()
	f.instances[fakeInstanceID] = &apiv1alpha1.AgentInstance{
		Id: fakeInstanceID, Creator: fakeToken, ContextId: fakeContextID,
		Harness:       &apiv1alpha1.ResourceReference{Namespace: kagentNamespace, Name: kagentHarness},
		AgentTemplate: &apiv1alpha1.ResourceReference{Namespace: kagentNamespace, Name: a2aTestAgent},
		State:         apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY,
	}
	return f
}

// fakeTemplate builds an AgentTemplate the way the controller serves it: the
// whole CR as a StructuredObject plus the denormalised fields.
func fakeTemplate(t *testing.T, name string, annotations map[string]any, admitting []string, harnesses []any) *apiv1alpha1.AgentTemplate {
	t.Helper()
	meta := map[string]any{nameKey: name, "namespace": kagentNamespace}
	if annotations != nil {
		meta["annotations"] = annotations
	}
	value, err := structpb.NewStruct(map[string]any{
		"apiVersion": agentTemplateAPIVersion, "kind": kindAgentTemplate, fieldMetadata: meta,
		"spec":      map[string]any{descriptionKey: "Proof agent " + name},
		fieldStatus: map[string]any{"harnesses": harnesses},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &apiv1alpha1.AgentTemplate{
		Ref:                &apiv1alpha1.ResourceReference{Namespace: kagentNamespace, Name: name},
		Resource:           &apiv1alpha1.StructuredObject{ApiVersion: agentTemplateAPIVersion, Kind: kindAgentTemplate, Value: value},
		Description:        "Proof agent " + name,
		AdmittingHarnesses: admitting,
	}
}

func fakeHarnessStatus(harness string, ready bool, message string) map[string]any {
	st := condFalseStatus
	if ready {
		st = conditionTrue
	}
	return map[string]any{"harness": harness, fieldConditions: []any{map[string]any{fieldType: conditionReady, fieldStatus: st, fieldReason: "Compiled", fieldMessage: message}}}
}

// TestKagentAPIWireContract: every A2A call rides the person's bearer,
// exactly one instance route and the HITL extension request as gRPC
// metadata; the message keeps no context id of its own; the events fold
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

	got, err := api.turn(t.Context(), fakeInstanceID, userMessage("ping"))
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
	if want := []string{fakeInstanceID}; !slices.Equal(md.Get(instanceIDMetadata), want) {
		t.Errorf("exactly one instance route wanted, got %v", md.Get(instanceIDMetadata))
	}
	if !slices.Contains(md.Get("a2a-extensions"), hitlExtensionURI) {
		t.Errorf("the HITL extension is not requested: a2a-extensions = %v", md.Get("a2a-extensions"))
	}
	if len(f.sent) != 1 || f.sent[0].ContextID != "" || f.sent[0].Parts[0].Text() != "ping" || f.sent[0].TaskID != "" {
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
	_, err = api.turn(t.Context(), fakeInstanceID, userMessage("ping"))
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
	if _, err := api.getTask(t.Context(), fakeInstanceID, "nothing"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("GetTask: %v", err)
	}
	if got := f.lastMD("GetTask").Get(userIDHeader); !slices.Equal(got, []string{a2aTestForgedUser}) {
		t.Errorf("the A2A call carried %s=%v", userIDHeader, got)
	}
	if principalOf(map[string]any{"sub": "s"}) != "s" || principalOf(nil) != "" {
		t.Error("principalOf falls back to sub, then to nothing")
	}
}

// TestCreateInstance: the create waits through FailedPrecondition while
// the template compiles, is idempotent on request_id, lists and deletes.
func TestCreateInstance(t *testing.T) {
	f := newFakeKagent()
	f.compiling = 1
	api := f.serve(t, fakeToken)
	first, err := api.createInstance(t.Context(), a2aTestAgent, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.GetCreator() != fakeToken || first.GetHarness().GetName() != kagentHarness || first.GetAgentTemplate().GetName() != a2aTestAgent || instanceState(first) != "READY" {
		t.Errorf("created %v", first)
	}
	if calls := len(f.calls["CreateAgentInstance"]); calls != 2 {
		t.Errorf("FailedPrecondition was not waited through: %d creates", calls)
	}
	again, err := api.createInstance(t.Context(), a2aTestAgent, "req-1")
	if err != nil || again.GetId() != first.GetId() {
		t.Errorf("the same request_id: %v %v", again.GetId(), err)
	}
	other, err := api.createInstance(t.Context(), a2aTestAgent, "req-2")
	if err != nil || other.GetId() == first.GetId() {
		t.Errorf("another request_id: %v %v", other.GetId(), err)
	}
	listed, err := api.listInstances(t.Context())
	if err != nil || len(listed) != 2 {
		t.Errorf("listed %d (%v)", len(listed), err)
	}
	if err := api.deleteInstance(t.Context(), first.GetId()); err != nil {
		t.Error(err)
	}
	if err := api.deleteInstance(t.Context(), first.GetId()); err != nil {
		t.Errorf("deleting a deleted instance is success: %v", err)
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

	paused, err := api.turn(t.Context(), fakeInstanceID, userMessage(a2aToolPrompt))
	if err != nil {
		t.Fatal(err)
	}
	if paused.state() != a2a.TaskStateInputRequired || paused.approval == nil || !slices.Equal(paused.approval.toolNames(), []string{fakeTool}) || paused.approval.Hint != "Approve "+fakeTool+"?" {
		t.Fatalf("paused = %s approval %+v", paused.state(), paused.approval)
	}
	if _, err := paused.completedText(); err == nil || !strings.Contains(err.Error(), fakeTool) {
		t.Errorf("a paused turn is no answer: %v", err)
	}

	resumed, err := api.decide(t.Context(), fakeInstanceID, paused, true, "")
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

	canceled, err := api.cancelTask(t.Context(), fakeInstanceID, fakeTaskID)
	if err != nil || canceled.Status.State != a2a.TaskStateCanceled || !slices.Equal(f.canceled, []a2a.TaskID{fakeTaskID}) {
		t.Errorf("CancelTask: %v %v %v", canceled, err, f.canceled)
	}
	if got, err := api.getTask(t.Context(), fakeInstanceID, fakeTaskID); err != nil || got.Status.State != a2a.TaskStateCanceled {
		t.Errorf("GetTask after the cancel: %v %v", got, err)
	}
	if req := parseToolApprovalRequest(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("plain"))); req != nil {
		t.Errorf("a plain message is no request: %+v", req)
	}
}

// TestListingOf: the roster entry of a listed template — the annotations,
// the admitting Harness that is Ready, the reason a template is not
// selectable.
func TestListingOf(t *testing.T) {
	f := newFakeKagent()
	f.templates = []*apiv1alpha1.AgentTemplate{
		fakeTemplate(t, a2aTestAgent, map[string]any{displayNameAnnotation: a2aTestDisplayName, iconURLAnnotation: a2aTestIconURL}, []string{kagentHarness}, []any{fakeHarnessStatus(kagentHarness, true, "")}),
		fakeTemplate(t, "compiling", nil, []string{kagentHarness}, []any{fakeHarnessStatus(kagentHarness, false, "waiting for the ActorTemplate golden snapshot")}),
		fakeTemplate(t, "orphan", nil, nil, nil),
		fakeTemplate(t, "no-status-yet", nil, []string{kagentHarness}, nil),
	}
	api := f.serve(t, fakeToken)
	templates, err := api.listTemplates(t.Context())
	if err != nil || len(templates) != 4 {
		t.Fatalf("listed %d: %v", len(templates), err)
	}
	if got := f.lastMD("ListAgentTemplates").Get(authorizationMetadata); !slices.Equal(got, []string{"Bearer " + fakeToken}) {
		t.Errorf("ListAgentTemplates carried authorization %v", got)
	}
	ready, err := findListing(templates, a2aTestAgent)
	if err != nil {
		t.Fatal(err)
	}
	want := templateListing{Name: a2aTestAgent, Namespace: kagentNamespace, DisplayName: a2aTestDisplayName, IconURL: a2aTestIconURL, Description: "Proof agent " + a2aTestAgent, Harness: kagentHarness}
	if ready != want {
		t.Errorf("listing = %+v, want %+v", ready, want)
	}
	for name, reason := range map[string]string{
		"compiling":     "has not compiled a ready revision: waiting for the ActorTemplate golden snapshot",
		"orphan":        "no Harness admits",
		"no-status-yet": "no status reported yet",
	} {
		l, err := findListing(templates, name)
		if err != nil || !strings.Contains(l.Unavailable, reason) {
			t.Errorf("%s: %+v %v", name, l, err)
		}
	}
	if _, err := findListing(templates, "nobody"); err == nil || !strings.Contains(err.Error(), "does not list nobody") {
		t.Errorf("an unknown template: %v", err)
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
