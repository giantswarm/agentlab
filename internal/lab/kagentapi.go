package lab

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/kagentpb"
)

// kagent API v2 (kagent main) on the wire, as the proofs speak it.
//
// The controller serves one gRPC API — kagent.api.v1alpha1 for the control
// plane, lf.a2a.v1 for the turns — and no REST. The portal's backend reaches
// it through the agentgateway edge as gRPC-Web (application/grpc-web+proto
// over HTTP/1.1 POST) at <AgentgatewayBaseURL>/kagent/<service>/<method>, the
// connectivity chart's kagent.controllerRoute: the route's URLRewrite strips
// the prefix and the request reaches the controller's grpcweb wrapper. The
// proofs take the same path on the lab's TLS transport, so a turn proves the
// route as well as the controller.
//
// Two headers carry the person: x-user-id, the identity the controller keeps
// AgentInstances for (creator-scoped: nobody else reads or drives them), and
// authorization: Bearer <Dex id_token>, forwarded unchanged to the actor and
// re-emitted by the Go ADK on every MCP call (KAGENT_PROPAGATE_TOKEN), so
// muster sees the person. A turn is routed to its instance by
// x-kagent-agent-instance-id. The messages are the controller's own protos:
// internal/kagentpb (generated from kagent/api/v1alpha1/{common,agent_instances}.proto)
// and the A2A v1 package the controller itself is built with.

const (
	// kagentRoutePrefix is the path prefix the connectivity chart mounts the
	// kagent API at on the agentgateway hostname (kagent.controllerRoute.pathPrefix).
	kagentRoutePrefix = "/kagent"
	// kagentHarness is the platform's Go ADK Harness: every AgentTemplate the
	// proofs create is labelled for it (harnessLabel) and every turn runs on it.
	kagentHarness = "kagent"
	// kagentTurnTimeout bounds one turn end to end: the instance create, a
	// cold resume from the golden snapshot, the model's answer with its tool
	// calls, the instance delete.
	kagentTurnTimeout = 180 * time.Second

	grpcWebContentType  = "application/grpc-web+proto"
	userIDHeader        = "x-user-id"
	agentInstanceHeader = "x-kagent-agent-instance-id"
	grpcStatusHeader    = "Grpc-Status"
	grpcMessageHeader   = "Grpc-Message"

	agentInstanceService = "kagent.api.v1alpha1.AgentInstanceService"
	a2aService           = "lf.a2a.v1.A2AService"

	// grpcWebTrailerFlag marks the frame that carries the trailers
	// (grpc-status, grpc-message) at the end of a gRPC-Web response body.
	grpcWebTrailerFlag = 0x80
	// grpcWebFrameHeader is the length-prefix of every gRPC-Web frame: one
	// flag byte and the payload length as a big-endian uint32.
	grpcWebFrameHeader = 5
	// grpcFailedPrecondition is the status CreateAgentInstance answers while
	// the template has no successful revision yet (its golden snapshot is
	// still being taken); kagent's own e2e polls through it the same way.
	grpcFailedPrecondition = 9
)

// kagentAPI is one person's view of the kagent controller through the edge.
type kagentAPI struct {
	client *http.Client
	// base is <AgentgatewayBaseURL>/kagent.
	base string
	// user is the x-user-id: the person the controller attributes instances to.
	user string
	// token is the person's Dex id_token, sent as Bearer on every call.
	token string
}

// newKagentAPI is the controller's API through the edge for one person: the
// same base URL Backstage's app-config carries for the installation
// (agentPlatform.kagent.installations.<i>.apiBaseUrl), on the lab transport.
func newKagentAPI(cfg *config.Config, user, token string) (*kagentAPI, error) {
	client, err := labHTTPClient(kagentTurnTimeout)
	if err != nil {
		return nil, err
	}
	return &kagentAPI{client: client, base: cfg.AgentgatewayBaseURL() + kagentRoutePrefix, user: user, token: token}, nil
}

// grpcStatus is a non-OK gRPC status the controller answered with.
type grpcStatus struct {
	code    int
	message string
}

func (s *grpcStatus) Error() string {
	return fmt.Sprintf("grpc status %d: %s", s.code, s.message)
}

// call is one unary gRPC-Web call: the request framed and POSTed to
// <base>/<service>/<method> with the person's headers (and extra ones), the
// answer's message frame decoded into out, its trailers judged. A status
// other than OK is a *grpcStatus; an HTTP status other than 200 is the edge
// or the route talking, reported with the body.
func (a *kagentAPI) call(ctx context.Context, service, method string, in, out proto.Message, headers map[string]string) error {
	payload, err := proto.Marshal(in)
	if err != nil {
		return fmt.Errorf("%s/%s: encoding the request: %w", service, method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/"+service+"/"+method, bytes.NewReader(grpcWebFrame(0, payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", grpcWebContentType)
	req.Header.Set("Accept", grpcWebContentType)
	req.Header.Set("X-Grpc-Web", "1")
	req.Header.Set(userIDHeader, a.user)
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s/%s through %s: %w", service, method, a.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s/%s: reading the answer: %w", service, method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s/%s through %s answered HTTP %d (the edge or the route, not the controller): %.300s", service, method, a.base, resp.StatusCode, body)
	}
	messages, trailers, err := parseGRPCWebBody(body)
	if err != nil {
		return fmt.Errorf("%s/%s: %w", service, method, err)
	}
	status, err := grpcStatusFrom(resp.Header, trailers)
	if err != nil {
		return fmt.Errorf("%s/%s: %w", service, method, err)
	}
	if status != nil {
		return fmt.Errorf("%s/%s: %w", service, method, status)
	}
	if len(messages) == 0 {
		return fmt.Errorf("%s/%s: OK without a message", service, method)
	}
	if err := proto.Unmarshal(messages[0], out); err != nil {
		return fmt.Errorf("%s/%s: decoding the answer: %w", service, method, err)
	}
	return nil
}

// grpcWebFrame is one length-prefixed gRPC-Web frame: the flag byte (0 for a
// message, grpcWebTrailerFlag for trailers), the payload length big-endian,
// the payload.
func grpcWebFrame(flag byte, payload []byte) []byte {
	frame := make([]byte, grpcWebFrameHeader+len(payload))
	frame[0] = flag
	binary.BigEndian.PutUint32(frame[1:grpcWebFrameHeader], uint32(len(payload))) // #nosec G115 -- a proto message never approaches 4 GiB
	copy(frame[grpcWebFrameHeader:], payload)
	return frame
}

// parseGRPCWebBody splits a gRPC-Web response body into its message frames
// and the trailers frame, if the answer carried one ("key: value" lines). A
// body that ends inside a frame is refused: it is not a gRPC-Web answer.
func parseGRPCWebBody(body []byte) (messages [][]byte, trailers http.Header, err error) {
	for len(body) > 0 {
		if len(body) < grpcWebFrameHeader {
			return nil, nil, fmt.Errorf("gRPC-Web answer ends inside a frame header (%d trailing bytes)", len(body))
		}
		flag, length := body[0], binary.BigEndian.Uint32(body[1:grpcWebFrameHeader])
		body = body[grpcWebFrameHeader:]
		if uint64(len(body)) < uint64(length) {
			return nil, nil, fmt.Errorf("gRPC-Web frame announces %d bytes, %d follow", length, len(body))
		}
		frame, rest := body[:length], body[length:]
		body = rest
		if flag&grpcWebTrailerFlag != 0 {
			trailers = parseGRPCWebTrailers(frame)
			continue
		}
		messages = append(messages, frame)
	}
	return messages, trailers, nil
}

// parseGRPCWebTrailers reads a trailers frame: HTTP header lines, CRLF-separated.
func parseGRPCWebTrailers(raw []byte) http.Header {
	trailers := http.Header{}
	for _, line := range strings.Split(string(raw), "\r\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		trailers.Add(textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(key)), strings.TrimSpace(value))
	}
	return trailers
}

// grpcStatusFrom is the call's verdict: grpc-status from the trailers frame,
// or from the response headers of a trailers-only answer (an error before
// any message). nil for OK; an answer without any status is not gRPC-Web.
func grpcStatusFrom(headers, trailers http.Header) (*grpcStatus, error) {
	source := trailers
	if source.Get(grpcStatusHeader) == "" {
		source = headers
	}
	raw := source.Get(grpcStatusHeader)
	if raw == "" {
		return nil, fmt.Errorf("no grpc-status in the trailers or the headers — not a gRPC-Web answer (content-type %q)", headers.Get("Content-Type"))
	}
	code, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("grpc-status %q is not a number", raw)
	}
	if code == 0 {
		return nil, nil
	}
	message := source.Get(grpcMessageHeader)
	if decoded, err := url.PathUnescape(message); err == nil {
		message = decoded
	}
	return &grpcStatus{code: code, message: message}, nil
}

// createInstance is AgentInstanceService/CreateAgentInstance for the person:
// one conversation of the AgentTemplate on the Go ADK Harness, both in the
// kagent namespace. A template whose golden snapshot is still being taken
// answers FailedPrecondition; that is waited through, bounded.
func (a *kagentAPI) createInstance(ctx context.Context, template string) (*kagentpb.AgentInstance, error) {
	req := &kagentpb.CreateAgentInstanceRequest{
		Harness:       &kagentpb.ResourceReference{Namespace: kagentNamespace, Name: kagentHarness},
		AgentTemplate: &kagentpb.ResourceReference{Namespace: kagentNamespace, Name: template},
		RequestId:     uuid.NewString(),
	}
	var resp kagentpb.CreateAgentInstanceResponse
	var err error
	created := waitFor(20, 3*time.Second, func() bool {
		err = a.call(ctx, agentInstanceService, "CreateAgentInstance", req, &resp, nil)
		var status *grpcStatus
		if errors.As(err, &status) && status.code == grpcFailedPrecondition {
			return false
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("creating an AgentInstance of %s/%s on Harness %s as %s: %w", kagentNamespace, template, kagentHarness, a.user, err)
	}
	if !created {
		return nil, fmt.Errorf("AgentTemplate %s has no successful revision after 60 s (the controller keeps answering FailedPrecondition)", template)
	}
	instance := resp.GetAgentInstance()
	if instance.GetId() == "" {
		return nil, fmt.Errorf("CreateAgentInstance of %s answered without an id", template)
	}
	return instance, nil
}

// deleteInstance is AgentInstanceService/DeleteAgentInstance: best effort,
// on the cleanup paths.
func (a *kagentAPI) deleteInstance(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var resp kagentpb.DeleteAgentInstanceResponse
	if err := a.call(ctx, agentInstanceService, "DeleteAgentInstance", &kagentpb.DeleteAgentInstanceRequest{AgentInstanceId: id}, &resp, nil); err != nil {
		note("deleting AgentInstance %s: %v", id, err)
	}
}

// sendMessage is one A2A turn on the instance — lf.a2a.v1.A2AService/SendMessage
// with the person's headers and x-kagent-agent-instance-id — and returns the
// text of the terminal Task (or of a bare Message answer). A task that ends
// anywhere but completed is an error carrying what it said.
func (a *kagentAPI) sendMessage(ctx context.Context, instanceID, prompt string) (string, error) {
	req := &a2apb.SendMessageRequest{Message: &a2apb.Message{
		MessageId: uuid.NewString(),
		Role:      a2apb.Role_ROLE_USER,
		Parts:     []*a2apb.Part{{Content: &a2apb.Part_Text{Text: prompt}}},
	}}
	var resp a2apb.SendMessageResponse
	if err := a.call(ctx, a2aService, "SendMessage", req, &resp, map[string]string{agentInstanceHeader: instanceID}); err != nil {
		return "", err
	}
	if msg := resp.GetMessage(); msg != nil {
		return partsText(msg.GetParts()), nil
	}
	task := resp.GetTask()
	if task == nil {
		return "", fmt.Errorf("SendMessage answered neither a Task nor a Message")
	}
	text := taskText(task)
	if state := task.GetStatus().GetState(); state != a2apb.TaskState_TASK_STATE_COMPLETED {
		return "", fmt.Errorf("the task ended %s: %s", strings.TrimPrefix(state.String(), "TASK_STATE_"), excerpt(text, 300))
	}
	return text, nil
}

// taskText is what a Task says: its status message's text parts, then its
// artifacts' — the shape kagent's own e2e reads.
func taskText(task *a2apb.Task) string {
	texts := []string{partsText(task.GetStatus().GetMessage().GetParts())}
	for _, artifact := range task.GetArtifacts() {
		texts = append(texts, partsText(artifact.GetParts()))
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}

// partsText joins the text parts of a message or artifact, one per line.
func partsText(parts []*a2apb.Part) string {
	var texts []string
	for _, part := range parts {
		if text := part.GetText(); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

// kagentTurn is one conversation turn on kagent main as a person — what the
// portal does for a chat message: an AgentInstance of the AgentTemplate on the
// Go ADK Harness, created for the person and deleted afterwards, and one A2A
// SendMessage carrying the person's bearer, answered by the terminal Task's
// text. user is the person's email (the controller's x-user-id), token the
// same person's Dex id_token.
func kagentTurn(cfg *config.Config, user, token, template, prompt string) (string, error) {
	api, err := newKagentAPI(cfg, user, token)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	defer cancel()
	instance, err := api.createInstance(ctx, template)
	if err != nil {
		return "", err
	}
	defer api.deleteInstance(instance.GetId())
	note("AgentInstance %s of %s for %s (creator %s, %s)", instance.GetId(), template, user, instance.GetCreator(), strings.TrimPrefix(instance.GetState().String(), "AGENT_INSTANCE_STATE_"))
	reply, err := api.sendMessage(ctx, instance.GetId(), prompt)
	if err != nil {
		return "", fmt.Errorf("A2A turn on %s as %s: %w", template, user, err)
	}
	return reply, nil
}

// agentTurnAsV2 sends one turn to the agent as the person whose Dex id_token is
// given — the way the portal's session chat does, x-user-id being the token's
// email — and returns the agent's text.
func agentTurnAsV2(cfg *config.Config, name, token, prompt string) (string, error) {
	claims, err := decodeJWTClaims(token)
	if err != nil {
		return "", fmt.Errorf("the token for the turn on %s: %w", name, err)
	}
	email, _ := claims["email"].(string)
	if email == "" {
		return "", fmt.Errorf("the token for the turn on %s carries no email claim to send as %s", name, userIDHeader)
	}
	return kagentTurn(cfg, email, token, name, prompt)
}
