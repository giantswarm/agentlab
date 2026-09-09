package lab

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"google.golang.org/protobuf/proto"

	"github.com/giantswarm/agentlab/internal/kagentpb"
)

// TestGRPCWebFrames: a frame round-trips; a body of two messages and a
// trailers frame splits into both and the parsed trailers; a body that ends
// inside a frame is refused.
func TestGRPCWebFrames(t *testing.T) {
	one, two := []byte("first"), []byte("second message")
	trailers := []byte("grpc-status: 0\r\ngrpc-message: \r\n")
	body := bytes.Join([][]byte{grpcWebFrame(0, one), grpcWebFrame(0, two), grpcWebFrame(grpcWebTrailerFlag, trailers)}, nil)
	messages, got, err := parseGRPCWebBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(messages, [][]byte{one, two}) {
		t.Errorf("messages = %q", messages)
	}
	if got.Get(grpcStatusHeader) != "0" || got.Get(grpcMessageHeader) != "" {
		t.Errorf("trailers = %v", got)
	}
	if _, _, err := parseGRPCWebBody(body[:len(body)-3]); err == nil || !strings.Contains(err.Error(), "announces") {
		t.Errorf("a truncated frame: %v", err)
	}
	if _, _, err := parseGRPCWebBody([]byte{0, 0}); err == nil || !strings.Contains(err.Error(), "frame header") {
		t.Errorf("a truncated header: %v", err)
	}
	if messages, trailers, err := parseGRPCWebBody(nil); err != nil || messages != nil || trailers != nil {
		t.Errorf("an empty body: %v %v %v", messages, trailers, err)
	}
}

// TestGRPCStatusFrom: OK is nil; a failure carries its code and the
// percent-decoded message; a trailers-only answer reads the response headers;
// no status anywhere is not gRPC-Web.
func TestGRPCStatusFrom(t *testing.T) {
	ok := http.Header{grpcStatusHeader: {"0"}}
	if status, err := grpcStatusFrom(http.Header{}, ok); err != nil || status != nil {
		t.Errorf("OK: %v %v", status, err)
	}
	failed := http.Header{grpcStatusHeader: {"9"}, grpcMessageHeader: {"template%20has%20no%20revision"}}
	status, err := grpcStatusFrom(http.Header{}, failed)
	if err != nil || status == nil || status.code != grpcFailedPrecondition || status.message != "template has no revision" {
		t.Errorf("failed: %v %v", status, err)
	}
	status, err = grpcStatusFrom(http.Header{grpcStatusHeader: {"16"}, grpcMessageHeader: {"unauthenticated"}}, nil)
	if err != nil || status == nil || status.code != 16 || !strings.Contains(status.Error(), "unauthenticated") {
		t.Errorf("trailers-only: %v %v", status, err)
	}
	if _, err := grpcStatusFrom(http.Header{"Content-Type": {"text/html"}}, nil); err == nil || !strings.Contains(err.Error(), "text/html") {
		t.Errorf("no status: %v", err)
	}
	if _, err := grpcStatusFrom(http.Header{}, http.Header{grpcStatusHeader: {"nine"}}); err == nil {
		t.Error("a non-numeric status must fail")
	}
}

// TestTaskText: the status message's text parts, then the artifacts', one
// per line; a completed task is the reply, any other state an error naming it.
func TestTaskText(t *testing.T) {
	task := &a2apb.Task{
		Status: &a2apb.TaskStatus{State: a2apb.TaskState_TASK_STATE_COMPLETED, Message: &a2apb.Message{Parts: []*a2apb.Part{
			{Content: &a2apb.Part_Text{Text: testPong}},
			{Content: &a2apb.Part_Raw{Raw: []byte{1}}},
		}}},
		Artifacts: []*a2apb.Artifact{{Parts: []*a2apb.Part{{Content: &a2apb.Part_Text{Text: "artifact"}}}}},
	}
	if got := taskText(task); got != "pong\nartifact" {
		t.Errorf("taskText = %q", got)
	}
	if got := taskText(&a2apb.Task{}); got != "" {
		t.Errorf("an empty task = %q", got)
	}
}

// TestKagentAPICall drives the gRPC-Web hop against a fake controller behind
// a fake edge: the POST lands on <base>/<service>/<method> with the person's
// headers and the instance header, the framed request decodes, the framed
// answer plus trailers decodes into the response; a failing trailer is the
// status, an edge error is the HTTP status.
func TestKagentAPICall(t *testing.T) {
	var seen *http.Request
	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", grpcWebContentType)
		switch {
		case strings.HasSuffix(r.URL.Path, "/CreateAgentInstance"):
			resp, _ := proto.Marshal(&kagentpb.CreateAgentInstanceResponse{AgentInstance: &kagentpb.AgentInstance{Id: testIDPrefix, Creator: r.Header.Get(userIDHeader), State: kagentpb.AgentInstanceState_AGENT_INSTANCE_STATE_READY}})
			_, _ = w.Write(grpcWebFrame(0, resp))
			_, _ = w.Write(grpcWebFrame(grpcWebTrailerFlag, []byte("grpc-status: 0\r\n")))
		case strings.HasSuffix(r.URL.Path, "/SendMessage"):
			w.Header().Set(grpcStatusHeader, "7")
			w.Header().Set(grpcMessageHeader, "creator%20mismatch")
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("no route"))
		}
	}))
	defer srv.Close()
	api := &kagentAPI{client: srv.Client(), base: srv.URL + kagentRoutePrefix, user: testDevUser, token: "tok"}

	var resp kagentpb.CreateAgentInstanceResponse
	req := &kagentpb.CreateAgentInstanceRequest{Harness: &kagentpb.ResourceReference{Namespace: kagentNamespace, Name: kagentHarness}, AgentTemplate: &kagentpb.ResourceReference{Namespace: kagentNamespace, Name: testSmoke}, RequestId: "r1"}
	if err := api.call(context.Background(), agentInstanceService, "CreateAgentInstance", req, &resp, map[string]string{agentInstanceHeader: testIDPrefix}); err != nil {
		t.Fatal(err)
	}
	if resp.GetAgentInstance().GetId() != testIDPrefix || resp.GetAgentInstance().GetCreator() != testDevUser {
		t.Errorf("decoded %v", resp.GetAgentInstance())
	}
	if seen.URL.Path != kagentRoutePrefix+"/"+agentInstanceService+"/CreateAgentInstance" || seen.Method != http.MethodPost {
		t.Errorf("request went to %s %s", seen.Method, seen.URL.Path)
	}
	for header, want := range map[string]string{
		"Content-Type": grpcWebContentType, userIDHeader: testDevUser, "Authorization": "Bearer tok", agentInstanceHeader: testIDPrefix, "X-Grpc-Web": "1",
	} {
		if got := seen.Header.Get(header); got != want {
			t.Errorf("header %s = %q, want %q", header, got, want)
		}
	}
	messages, _, err := parseGRPCWebBody(seenBody)
	if err != nil || len(messages) != 1 {
		t.Fatalf("request body: %v %d frames", err, len(messages))
	}
	var sent kagentpb.CreateAgentInstanceRequest
	if err := proto.Unmarshal(messages[0], &sent); err != nil || sent.GetAgentTemplate().GetName() != testSmoke || sent.GetRequestId() != "r1" {
		t.Errorf("sent %v (%v)", &sent, err)
	}

	var a2aResp a2apb.SendMessageResponse
	err = api.call(context.Background(), a2aService, "SendMessage", &a2apb.SendMessageRequest{}, &a2aResp, nil)
	var status *grpcStatus
	if !errors.As(err, &status) || status.code != 7 || status.message != "creator mismatch" {
		t.Errorf("a trailers-only failure: %v", err)
	}
	if err := api.call(context.Background(), "nowhere", "Nothing", &a2apb.SendMessageRequest{}, &a2aResp, nil); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("an edge 404: %v", err)
	}
}
