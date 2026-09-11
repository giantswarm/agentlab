package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"google.golang.org/protobuf/proto"

	"github.com/giantswarm/agentlab/internal/kagentpb"
)

// Fixed names of the fakes.
const (
	testUser          = "admin@lab.local"
	testInstance      = "inst-1"
	testTask          = "task-1"
	testOldTask       = "old"
	testThread        = "t-1"
	kindAgentTemplate = "AgentTemplate"
	toolCallTool      = "call_tool"
)

// TestKlausGatewayFixtures: the admitted template carries the Harness's
// admission labels, the display-name and icon annotations, the requireApproval
// binding to its own muster carrier; the carrier is the chart's shape (muster's
// in-cluster URL, X-Muster-Toolset, discovery off, no Authorization header);
// the unadmitted template carries no admission label.
func TestKlausGatewayFixtures(t *testing.T) {
	manifests := klausGatewayFixtures("my-model", map[string]string{"agent-platform.giantswarm.io/harness": kagentHarness})
	objs, err := decodeManifests([]byte(manifests))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 {
		t.Fatalf("%d objects, wanted the carrier and two templates", len(objs))
	}
	carrier, admitted, unadmitted := objs[0], objs[1], objs[2]
	if carrier.GetKind() != remoteMCPServerKind || carrier.GetName() != klausGatewayTestAgent || carrier.GetNamespace() != kagentNamespace {
		t.Errorf("carrier = %s %s/%s", carrier.GetKind(), carrier.GetNamespace(), carrier.GetName())
	}
	if url, _, _ := nestedString(carrier.Object, "spec", "url"); url != klausGatewayMusterURL {
		t.Errorf("carrier url = %q", url)
	}
	if carrier.GetLabels()["kagent.dev/discovery"] != "disabled" {
		t.Errorf("carrier labels = %v, wanted discovery disabled", carrier.GetLabels())
	}
	if strings.Contains(manifests, "Authorization") {
		t.Error("the carrier must never carry an Authorization header")
	}
	if !strings.Contains(manifests, "X-Muster-Toolset") || !strings.Contains(manifests, klausGatewayTestToolset) {
		t.Error("the carrier declares no X-Muster-Toolset")
	}

	if admitted.GetKind() != kindAgentTemplate || admitted.GetName() != klausGatewayTestAgent {
		t.Errorf("admitted = %s %s", admitted.GetKind(), admitted.GetName())
	}
	labels := admitted.GetLabels()
	if labels["agent-platform.giantswarm.io/harness"] != kagentHarness || labels[managedByLabel] != managedByAgentlabValue {
		t.Errorf("admitted labels = %v", labels)
	}
	ann := admitted.GetAnnotations()
	if ann["ui.giantswarm.io/display-name"] != klausGatewayTestDisplay || ann["ui.giantswarm.io/icon-url"] != klausGatewayTestIcon {
		t.Errorf("admitted annotations = %v", ann)
	}
	if model, _, _ := nestedString(admitted.Object, "spec", "modelConfig", "name"); model != "my-model" {
		t.Errorf("admitted modelConfig = %q", model)
	}
	tools, _, _ := nestedSlice(admitted.Object, "spec", "tools")
	if len(tools) != 1 {
		t.Fatalf("admitted tools = %v", tools)
	}
	mcp, _ := tools[0].(map[string]any)["mcp"].(map[string]any)
	if mcp["requireApproval"] != true {
		t.Errorf("the binding carries requireApproval=%v, wanted true", mcp["requireApproval"])
	}
	server, _ := mcp["server"].(map[string]any)
	if server["kind"] != remoteMCPServerKind || server["name"] != klausGatewayTestAgent {
		t.Errorf("the binding names %v, wanted the carrier %s", server, klausGatewayTestAgent)
	}

	if unadmitted.GetName() != klausGatewayTestUnadmitted {
		t.Errorf("unadmitted = %s", unadmitted.GetName())
	}
	if _, ok := unadmitted.GetLabels()["agent-platform.giantswarm.io/harness"]; ok {
		t.Error("the unadmitted template carries the admission label")
	}
	if unadmitted.GetLabels()[managedByLabel] != managedByAgentlabValue {
		t.Error("the unadmitted template is not labelled as agentlab's")
	}
}

func nestedString(obj map[string]any, fields ...string) (string, bool, error) {
	v, found := nested(obj, fields...)
	s, ok := v.(string)
	return s, found && ok, nil
}

func nestedSlice(obj map[string]any, fields ...string) ([]any, bool, error) {
	v, found := nested(obj, fields...)
	s, ok := v.([]any)
	return s, found && ok, nil
}

func nested(obj map[string]any, fields ...string) (any, bool) {
	var cur any = obj
	for _, f := range fields {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[f]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// TestGatewayArgs: the binary shape's flags name the loopback ports, the bolt
// store in the run directory, the static driver, the grpcs target with the CA
// and the fixture as the default agent; the image shape wraps the same flags
// in docker run on the host network with the mounts and the caller's uid.
func TestGatewayArgs(t *testing.T) {
	args := gatewayArgs(18090, "/run/x", "/run/ca.crt", "grpcs://agentgateway.127.0.0.1.nip.io:8445")
	want := []string{
		"--listen-address=127.0.0.1:18090", "--admin-address=127.0.0.1:18091", "--log-level=info",
		"--store=bolt", "--bolt-path=/run/x/routes.bolt", "--driver=static", "--web-enabled=true",
		"--a2a-enabled=true", "--a2a-url=grpcs://agentgateway.127.0.0.1.nip.io:8445", "--a2a-ca-file=/run/ca.crt",
		"--a2a-namespace=kagent", "--a2a-default-agent=" + klausGatewayTestAgent,
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("gatewayArgs =\n%q\nwant\n%q", args, want)
	}
	docker := dockerRunArgs("img:1", "ctr", "/run/x", "/lab/certs/ca.crt", 1000, 100, []string{"--flag"})
	wantDocker := []string{"run", "--rm", "--name", "ctr", "--network", "host", "--user", "1000:100",
		"-v", "/run/x:" + gatewayDataPath, "-v", "/lab/certs/ca.crt:" + gatewayCAPath + ":ro", "img:1", "--flag"}
	if !reflect.DeepEqual(docker, wantDocker) {
		t.Errorf("dockerRunArgs =\n%q\nwant\n%q", docker, wantDocker)
	}
	if got := grpcsTarget("https://agentgateway.127.0.0.1.nip.io:8445"); got != "grpcs://agentgateway.127.0.0.1.nip.io:8445" {
		t.Errorf("grpcsTarget = %q", got)
	}
	opts := KlausGatewayTestOptions{}.withDefaults()
	if opts.GatewayImage != KlausGatewayImageDefault || opts.Port != 18090 || opts.ModelConfig != defaultModelConfig || opts.ReadyTimeout != klausGatewayReadyTimeout {
		t.Errorf("defaults = %+v", opts)
	}
}

// TestReadSSE: content deltas concatenate and done ends the turn; a prompt
// event carries the paused task and its tool; an error event fails the turn.
func TestReadSSE(t *testing.T) {
	turn := webTurn{Status: http.StatusOK}
	stream := "data: {\"content\":\"po\"}\n\ndata: {\"content\":\"ng\"}\n\nevent: done\ndata: {}\n\n"
	if err := readSSE(strings.NewReader(stream), &turn); err != nil {
		t.Fatal(err)
	}
	if turn.Text != "pong" || !turn.Done || turn.Prompt != nil || turn.Err != "" {
		t.Errorf("done turn = %+v", turn)
	}
	if err := assertTurnSaid(&turn, "PONG"); err != nil {
		t.Errorf("assertTurnSaid: %v", err)
	}

	turn = webTurn{Status: http.StatusOK}
	stream = "data: {\"content\":\"Let me check.\"}\n\nevent: prompt\ndata: {\"taskId\":\"t-1\",\"text\":\"Run call_tool?\",\"prompt\":{\"toolName\":\"call_tool\",\"hint\":\"Run call_tool?\",\"tools\":[{\"id\":\"a-1\",\"name\":\"call_tool\"}]}}\n\n"
	if err := readSSE(strings.NewReader(stream), &turn); err != nil {
		t.Fatal(err)
	}
	if turn.Done || turn.Prompt == nil || turn.Prompt.TaskID != testThread || turn.Prompt.Prompt.ToolName != toolCallTool || len(turn.Prompt.Prompt.Tools) != 1 {
		t.Errorf("prompt turn = %+v", turn)
	}
	if err := assertTurnSaid(&turn, "x"); err == nil || !strings.Contains(err.Error(), "paused on a prompt for call_tool") {
		t.Errorf("a paused turn is not complete: %v", err)
	}

	turn = webTurn{Status: http.StatusOK}
	stream = "event: error\ndata: \"upstream timed out\"\n\n"
	if err := readSSE(strings.NewReader(stream), &turn); err != nil {
		t.Fatal(err)
	}
	if turn.Err != "upstream timed out" || turn.Done {
		t.Errorf("error turn = %+v", turn)
	}
	if err := assertTurnSaid(&turn, "x"); err == nil || !strings.Contains(err.Error(), "error event") {
		t.Errorf("an errored turn is not complete: %v", err)
	}
	turn = webTurn{Status: http.StatusBadGateway, Body: `{"error":{"message":"send completion: x"}}`}
	if err := assertTurnSaid(&turn, "x"); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("a refused turn is not complete: %v", err)
	}
}

// TestAssertRoster: the admitted fixture with its annotations passes; a
// missing fixture, wrong annotations, or the unadmitted template listed fail.
func TestAssertRoster(t *testing.T) {
	good := webAgent{Name: klausGatewayTestAgent, Namespace: kagentNamespace, DisplayName: klausGatewayTestDisplay, IconURL: klausGatewayTestIcon}
	if err := assertRoster([]webAgent{{Name: "other"}, good}); err != nil {
		t.Errorf("good roster: %v", err)
	}
	if err := assertRoster([]webAgent{{Name: "other"}}); err == nil || !strings.Contains(err.Error(), "does not list") {
		t.Errorf("missing: %v", err)
	}
	bad := good
	bad.IconURL = ""
	if err := assertRoster([]webAgent{bad}); err == nil || !strings.Contains(err.Error(), "iconUrl") {
		t.Errorf("wrong annotations: %v", err)
	}
	if err := assertRoster([]webAgent{good, {Name: klausGatewayTestUnadmitted}}); err == nil || !strings.Contains(err.Error(), "no Harness admits") {
		t.Errorf("unadmitted listed: %v", err)
	}
}

// TestAssertUnadmittedRefused: the gateway's synchronous refusal with the
// reason passes; an accepted turn or a refusal without the reason fails.
func TestAssertUnadmittedRefused(t *testing.T) {
	refused := &webTurn{Status: http.StatusBadGateway, Body: `{"error":{"message":"send completion: agent unavailable: kagent/x: no Harness admits this AgentTemplate (it carries no admission label a platform Harness selects)","type":"Bad Gateway"}}`}
	if err := assertUnadmittedRefused(refused); err != nil {
		t.Errorf("refused: %v", err)
	}
	if err := assertUnadmittedRefused(&webTurn{Status: http.StatusOK, Done: true, Text: "hi"}); err == nil || !strings.Contains(err.Error(), "was accepted") {
		t.Errorf("accepted: %v", err)
	}
	if err := assertUnadmittedRefused(&webTurn{Status: http.StatusBadGateway, Body: "resolve: boom"}); err == nil || !strings.Contains(err.Error(), "without the reason") {
		t.Errorf("no reason: %v", err)
	}
}

// fakeWebGateway is a web channel that answers a scripted stream per message
// text and records what it received.
type fakeWebGateway struct {
	t        *testing.T
	agents   []webAgent
	script   map[string]string // text (or "decision:<taskId>") -> SSE body
	received []map[string]any
	bearer   string
}

func (f *fakeWebGateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(webAgentsPath, func(w http.ResponseWriter, r *http.Request) {
		f.bearer = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"agents": f.agents})
	})
	mux.HandleFunc(webMessagesPath, func(w http.ResponseWriter, r *http.Request) {
		f.bearer = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var in map[string]any
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.received = append(f.received, in)
		key, _ := in["text"].(string)
		if task, ok := in["taskId"].(string); ok && in["decision"] != nil {
			key = "decision:" + task
		}
		stream, ok := f.script[key]
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"send completion: unscripted %q","type":"Bad Gateway"}}`, key)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, stream)
	})
	return mux
}

// TestWebClientTurns: the client sends the thread's identity and the bearer,
// reads the roster, a done turn and a refusal, and a stream the client cuts
// short is reported as Cut rather than failed.
func TestWebClientTurns(t *testing.T) {
	fake := &fakeWebGateway{t: t,
		agents: []webAgent{{Name: klausGatewayTestAgent, Namespace: kagentNamespace, DisplayName: klausGatewayTestDisplay, IconURL: klausGatewayTestIcon}},
		script: map[string]string{klausGatewayWordPrompt: "data: {\"content\":\"pong\"}\n\nevent: done\ndata: {}\n\n"},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	web := &webClient{base: srv.URL, token: testToken, user: testUser, thread: testThread}

	agents, err := web.agents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := assertRoster(agents); err != nil {
		t.Error(err)
	}
	if fake.bearer != "Bearer tok" {
		t.Errorf("bearer = %q", fake.bearer)
	}
	turn, err := web.send(context.Background(), webMessage{Text: klausGatewayWordPrompt}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := assertTurnSaid(turn, klausGatewayWord); err != nil {
		t.Error(err)
	}
	got := fake.received[0]
	if got["channelId"] != webChannelID || got["userId"] != testUser || got["threadId"] != testThread || got["text"] != klausGatewayWordPrompt {
		t.Errorf("request = %v", got)
	}
	refused, err := web.send(context.Background(), webMessage{Text: "nope", AgentRef: klausGatewayTestUnadmitted}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if refused.Status != http.StatusBadGateway || !strings.Contains(refused.Body, "unscripted") || fake.received[1]["agentRef"] != klausGatewayTestUnadmitted {
		t.Errorf("refusal = %+v, request %v", refused, fake.received[1])
	}

	// A stream that never ends: the client's deadline cuts it.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: {\"content\":\"once upon\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer slow.Close()
	cut, err := (&webClient{base: slow.URL, token: testToken, user: "u", thread: "t"}).sendAndCut("essay", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !cut.Cut || cut.Done || !strings.Contains(cut.Text, "once upon") {
		t.Errorf("cut turn = %+v", cut)
	}
	if _, err := web.sendAndCut(klausGatewayWordPrompt, time.Minute); err == nil || !strings.Contains(err.Error(), "ended on its own") {
		t.Errorf("a turn that completes is nothing to stop: %v", err)
	}
}

// fakeController answers the gRPC-Web reads the proof makes on the
// controller: GetTask by id with a scripted state, ListTasks, and
// ListAgentInstances.
type fakeController struct {
	tasks     map[string]a2apb.TaskState
	instances []*kagentpb.AgentInstance
}

func (f *fakeController) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		messages, _, err := parseGRPCWebBody(body)
		if err != nil || len(messages) != 1 {
			t.Errorf("request body: %v (%d messages)", err, len(messages))
		}
		var out proto.Message
		switch r.URL.Path {
		case "/" + a2aService + "/GetTask":
			var req a2apb.GetTaskRequest
			_ = proto.Unmarshal(messages[0], &req)
			state, ok := f.tasks[req.GetId()]
			if !ok {
				w.Header().Set(grpcStatusHeader, "5")
				w.Header().Set(grpcMessageHeader, "task not found")
				w.WriteHeader(http.StatusOK)
				return
			}
			out = &a2apb.Task{Id: req.GetId(), Status: &a2apb.TaskStatus{State: state}}
		case "/" + a2aService + "/ListTasks":
			resp := &a2apb.ListTasksResponse{}
			for id, state := range f.tasks {
				resp.Tasks = append(resp.Tasks, &a2apb.Task{Id: id, Status: &a2apb.TaskStatus{State: state}})
			}
			out = resp
		case "/" + agentInstanceService + "/ListAgentInstances":
			var req kagentpb.ListAgentInstancesRequest
			_ = proto.Unmarshal(messages[0], &req)
			if req.GetAgentTemplate().GetName() != klausGatewayTestAgent {
				t.Errorf("ListAgentInstances narrowed to %v, wanted %s", req.GetAgentTemplate(), klausGatewayTestAgent)
			}
			out = &kagentpb.ListAgentInstancesResponse{AgentInstances: f.instances}
		default:
			t.Errorf("unexpected call %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path != "/"+agentInstanceService+"/ListAgentInstances" && r.Header.Get(agentInstanceHeader) != testInstance {
			t.Errorf("%s without %s=inst-1: %q", r.URL.Path, agentInstanceHeader, r.Header.Get(agentInstanceHeader))
		}
		payload, _ := proto.Marshal(out)
		w.Header().Set("Content-Type", grpcWebContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(grpcWebFrame(0, payload))
		_, _ = w.Write(grpcWebFrame(grpcWebTrailerFlag, []byte("grpc-status: 0\r\n")))
	})
}

// TestApproveUntilDone: the tool-using turn pauses twice (filter_tools, then
// call_tool) on the same task, each pause is input-required at the controller,
// each approval carries the task id, and the resumed task ends completed with
// nothing left waiting; a pause on another task, or a task the controller does
// not have paused, fails.
func TestApproveUntilDone(t *testing.T) {
	ctrl := &fakeController{tasks: map[string]a2apb.TaskState{testTask: a2apb.TaskState_TASK_STATE_INPUT_REQUIRED}}
	ctrlSrv := httptest.NewServer(ctrl.handler(t))
	defer ctrlSrv.Close()
	api := &kagentAPI{client: ctrlSrv.Client(), base: ctrlSrv.URL, user: testUser, token: testToken}

	prompt := func(tool string) string {
		return fmt.Sprintf("event: prompt\ndata: {\"taskId\":\"task-1\",\"text\":\"%s?\",\"prompt\":{\"toolName\":\"%s\",\"tools\":[{\"id\":\"a\",\"name\":\"%s\"}]}}\n\n", tool, tool, tool)
	}
	approvals := 0
	fake := &fakeWebGateway{t: t, script: map[string]string{klausGatewayToolPrompt: prompt("filter_tools")}}
	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		if in["decision"] != nil {
			approvals++
			if in["taskId"] != testTask {
				t.Errorf("approval %d names task %v", approvals, in["taskId"])
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if approvals == 1 {
				_, _ = io.WriteString(w, prompt(toolCallTool))
				return
			}
			ctrl.tasks[testTask] = a2apb.TaskState_TASK_STATE_COMPLETED
			_, _ = io.WriteString(w, "data: {\"content\":\"There are 12 namespaces.\"}\n\nevent: done\ndata: {}\n\n")
			return
		}
		fake.handler().ServeHTTP(w, r)
	}))
	defer webSrv.Close()
	web := &webClient{base: webSrv.URL, token: testToken, user: testUser, thread: testThread}

	turn, err := web.send(context.Background(), webMessage{Text: klausGatewayToolPrompt}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	out, err := web.approveUntilDone(api, testInstance, turn)
	if err != nil {
		t.Fatal(err)
	}
	if out.taskID != testTask || out.rounds != 2 || !reflect.DeepEqual(out.tools, []string{"filter_tools", toolCallTool}) || out.finalState != "TASK_STATE_COMPLETED" || !strings.Contains(out.text, "12 namespaces") {
		t.Errorf("outcome = %+v", out)
	}

	// The controller does not have the task paused: the gateway's prompt is not trusted alone.
	ctrl.tasks["task-2"] = a2apb.TaskState_TASK_STATE_WORKING
	other := &webTurn{Status: http.StatusOK, Prompt: &webPrompt{TaskID: "task-2"}}
	other.Prompt.Prompt.ToolName = toolCallTool
	if _, err := web.approveUntilDone(api, testInstance, other); err == nil || !strings.Contains(err.Error(), "not TASK_STATE_INPUT_REQUIRED") {
		t.Errorf("a task the controller has working: %v", err)
	}
	if _, err := web.approveUntilDone(api, testInstance, &webTurn{Status: http.StatusOK, Done: true, Text: "12"}); err == nil || !strings.Contains(err.Error(), "did not pause") {
		t.Errorf("a turn without a prompt: %v", err)
	}
}

// TestControllerReads: ListAgentInstances is narrowed to the fixture, taskIDs
// is the set of the instance's tasks, and waitCanceledTask finds the task new
// since the snapshot that reached TASK_STATE_CANCELED — or says what the new
// tasks are.
func TestControllerReads(t *testing.T) {
	ctrl := &fakeController{
		tasks:     map[string]a2apb.TaskState{testOldTask: a2apb.TaskState_TASK_STATE_COMPLETED, "new": a2apb.TaskState_TASK_STATE_CANCELED},
		instances: []*kagentpb.AgentInstance{{Id: testInstance, Creator: testUser}},
	}
	srv := httptest.NewServer(ctrl.handler(t))
	defer srv.Close()
	api := &kagentAPI{client: srv.Client(), base: srv.URL, user: testUser, token: testToken}

	instances, err := api.listInstances(context.Background(), klausGatewayTestAgent)
	if err != nil || len(instances) != 1 || instances[0].GetId() != testInstance {
		t.Errorf("listInstances = %v %v", instanceIDs(instances), err)
	}
	ids, err := api.taskIDs(context.Background(), testInstance)
	if err != nil || !ids[testOldTask] || !ids["new"] {
		t.Errorf("taskIDs = %v %v", ids, err)
	}
	canceled, err := api.waitCanceledTask(testInstance, map[string]bool{testOldTask: true}, 3*time.Second)
	if err != nil || canceled != "new" {
		t.Errorf("waitCanceledTask = %q %v", canceled, err)
	}
	ctrl.tasks["new"] = a2apb.TaskState_TASK_STATE_WORKING
	if _, err := api.waitCanceledTask(testInstance, map[string]bool{testOldTask: true}, 2*pollInterval); err == nil || !strings.Contains(err.Error(), "new TASK_STATE_WORKING") {
		t.Errorf("no cancel: %v", err)
	}
}

// TestMusterAttribution: only records stamped after the turn began count; the
// accepted-token audit record must name the email and a tools/call request's
// truncated subject must prefix the token's subject; a pod prefix before the
// JSON is tolerated.
func TestMusterAttribution(t *testing.T) {
	since := time.Date(2026, 9, 11, 17, 0, 0, 0, time.UTC)
	logs := strings.Join([]string{
		`{"time":"2026-09-11T16:59:59Z","level":"INFO","msg":"security_audit","audit":{"event_type":"forwarded_id_token_accepted","details":{"email":"admin@lab.local"}}}`,
		`[muster-1] {"time":"2026-09-11T17:00:01Z","level":"INFO","msg":"security_audit","audit":{"event_type":"forwarded_id_token_accepted","details":{"email":"admin@lab.local"}}}`,
		`{"time":"2026-09-11T17:00:02Z","level":"INFO","msg":"security_audit","audit":{"event_type":"forwarded_id_token_accepted","details":{"email":"dev@lab.local"}}}`,
		`{"time":"2026-09-11T17:00:03Z","level":"INFO","msg":"tools/call request","subsystem":"MCP-Protocol","subject":"CiRjNGNl...","tool":"call_tool"}`,
		`{"time":"2026-09-11T17:00:04Z","level":"INFO","msg":"tools/call request","subsystem":"MCP-Protocol","subject":"Cg9vdGhl...","tool":"call_tool"}`,
		`not json at all`,
	}, "\n")
	accepted, calls, subjects := musterAttribution(logs, "admin@lab.local", "CiRjNGNlZGFmNS0...", since)
	if accepted != 1 || calls != 1 || len(subjects) != 2 {
		t.Errorf("accepted=%d calls=%d subjects=%v", accepted, calls, subjects)
	}
}

// TestGatewayProcessLog: the bound instance is read from the last
// instance_bound record, the count follows the records, the version from the
// starting record; a log without a binding says so.
func TestGatewayProcessLog(t *testing.T) {
	dir := t.TempDir()
	g := newGatewayProcess(KlausGatewayTestOptions{GatewayBinary: "/bin/x"}.withDefaults(), dir, "/ca", "grpcs://x:1")
	if _, err := g.boundInstance(); err == nil || !strings.Contains(err.Error(), "no instance_bound record") {
		t.Errorf("empty log: %v", err)
	}
	log := strings.Join([]string{
		`{"time":"t","level":"INFO","msg":"klaus-gateway starting","version":"1.0.2","git_sha":"b1005ed"}`,
		`{"time":"t","level":"INFO","msg":"channels: thread bound to agent instance","record":"instance_bound","instance":"inst-a"}`,
		`{"time":"t","level":"INFO","msg":"channels: thread bound to agent instance","record":"instance_bound","instance":"inst-b"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, gatewayLogFile), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, err := g.boundInstance(); err != nil || id != "inst-b" {
		t.Errorf("boundInstance = %q %v", id, err)
	}
	if g.boundCount() != 2 {
		t.Errorf("boundCount = %d", g.boundCount())
	}
	if g.version() != "klaus-gateway 1.0.2 (b1005ed)" {
		t.Errorf("version = %q", g.version())
	}
	if g.describe() != "binary /bin/x" || newGatewayProcess(KlausGatewayTestOptions{}.withDefaults(), dir, "", "").describe() != "image "+KlausGatewayImageDefault {
		t.Error("describe")
	}
	if err := g.stop(); err != nil {
		t.Errorf("stopping a gateway that never ran: %v", err)
	}
	if err := portsFree(0); err != nil {
		t.Errorf("a free port: %v", err)
	}
}
