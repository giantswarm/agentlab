package lab

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The chat half of the portal proof: the Sessions pages through the routes
// the agent-platform backend serves the browser (plugins/agent-platform-backend
// router.ts), every call carrying the user's forwarded Dex id_token in
// backstage-kagent-authorization, which the backend forwards to the
// controller as the person's bearer. A session is the person's AgentInstance
// of the agent's template on the platform Harness: created on the first
// message with the browser's requestId (idempotent on (creator, requestId)),
// listed for its creator only, renamed, deleted. A turn is A2A v1
// SendStreamingMessage relayed as SSE frames of proto3 JSON (one
// lf.a2a.v1.StreamResponse each); every quiescent turn suspends the actor to
// a snapshot, and the next message resumes it with the conversation intact.
// The agent's own tool call reaches muster with the person's token, which
// muster records as a forwarded_id_token_accepted audit event naming the
// person. A confirmation the agent pauses on (the HITL extension the backend
// requests on every turn) is answered through …/answer naming the task; Stop
// is …/tasks/:taskId/cancel.

// The kagent routes, relative to portalKagentAPI (the installation is
// appended by kagentPath).
const (
	kagentInstallationsPath = "/installations"
	kagentMePath            = "/me"
	kagentSessionsPath      = "/sessions"
	kagentSessionStatesPath = "/session-states"
	kagentSessionUsagePath  = "/session-usage"
)

// The AgentInstance states the controller reports (proto3 JSON enum names)
// and the A2A task states, as the frames spell them.
const (
	instanceStateReady     = "AGENT_INSTANCE_STATE_READY"
	instanceStateSuspended = "AGENT_INSTANCE_STATE_SUSPENDED"
	taskStateCompleted     = "TASK_STATE_COMPLETED"
	taskStateCanceled      = "TASK_STATE_CANCELED"
	taskStateInputRequired = "TASK_STATE_INPUT_REQUIRED"
	taskStateFailedA2A     = "TASK_STATE_FAILED"
	taskStateRejected      = "TASK_STATE_REJECTED"
	taskStateAuthRequired  = "TASK_STATE_AUTH_REQUIRED"
	taskStateWorking       = "TASK_STATE_WORKING"
)

// The HITL extension the backend negotiates on every turn and the request
// type a paused tool call carries in the task's status message.
const (
	hitlExtensionURI       = "https://kagent.dev/extensions/hitl/v1"
	hitlToolApprovalType   = "tool_approval_request"
	hitlDecisionApprove    = "approve"
	musterTokenAcceptedLog = "forwarded_id_token_accepted"
)

// The prompts of the chat proof and their bounds.
const (
	portalSessionName        = "agentlab backstage-test"
	portalSessionRenamed     = portalSessionName + " (renamed)"
	portalPongPrompt         = "Reply with exactly the word pong and nothing else."
	portalToolPrompt         = "Use your tools to count the namespaces of the Kubernetes cluster you can reach. Reply with exactly one line of the form '<count> namespaces'."
	portalRecallPrompt       = "Reply with the codeword of this conversation only, nothing else."
	portalStopPrompt         = "Count slowly from 1 to 400, one number per line, no other text, and do not stop early."
	portalTurnTimeout        = 4 * time.Minute
	portalQuiescentTimeout   = 3 * time.Minute
	portalHITLRounds         = 5
	portalHITLResumeTimeout  = 2 * time.Minute
	portalStopSettleTimeout  = time.Minute
	portalSessionGoneTimeout = 30 * time.Second
)

// A2A v1 on the wire, as the portal relays it (proto3 JSON): the parts of
// Task, Message, TaskStatusUpdateEvent and TaskArtifactUpdateEvent the proof
// reads.
type (
	a2aPart struct {
		Text     string         `json:"text"`
		Data     map[string]any `json:"data"`
		Metadata map[string]any `json:"metadata"`
	}
	a2aMessage struct {
		MessageID  string         `json:"messageId"`
		ContextID  string         `json:"contextId"`
		TaskID     string         `json:"taskId"`
		Role       string         `json:"role"`
		Parts      []a2aPart      `json:"parts"`
		Metadata   map[string]any `json:"metadata"`
		Extensions []string       `json:"extensions"`
	}
	a2aTaskStatus struct {
		State     string      `json:"state"`
		Message   *a2aMessage `json:"message"`
		Timestamp string      `json:"timestamp"`
	}
	a2aArtifact struct {
		ArtifactID string         `json:"artifactId"`
		Parts      []a2aPart      `json:"parts"`
		Metadata   map[string]any `json:"metadata"`
	}
	a2aTask struct {
		ID        string        `json:"id"`
		ContextID string        `json:"contextId"`
		Status    a2aTaskStatus `json:"status"`
		History   []a2aMessage  `json:"history"`
		Artifacts []a2aArtifact `json:"artifacts"`
	}
	a2aStatusUpdate struct {
		TaskID    string        `json:"taskId"`
		ContextID string        `json:"contextId"`
		Status    a2aTaskStatus `json:"status"`
		Final     bool          `json:"final"`
	}
	a2aArtifactUpdate struct {
		TaskID    string      `json:"taskId"`
		Artifact  a2aArtifact `json:"artifact"`
		Append    bool        `json:"append"`
		LastChunk bool        `json:"lastChunk"`
	}
)

// streamFrame is one SSE data frame of the streaming route: one
// lf.a2a.v1.StreamResponse (exactly one of the four set), or the {error}
// frame the backend writes when the upstream stream broke mid-turn.
type streamFrame struct {
	Task           *a2aTask           `json:"task"`
	StatusUpdate   *a2aStatusUpdate   `json:"statusUpdate"`
	ArtifactUpdate *a2aArtifactUpdate `json:"artifactUpdate"`
	Message        *a2aMessage        `json:"message"`
	Error          *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// wireText is the concatenated text of the parts as the wire carries them.
func wireText(parts []a2aPart) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// hitlRequest is the typed request a paused task's status message carries
// under the HITL extension URI, when it is a tool approval: the tools the
// agent proposes, each with the id an approval answers.
type hitlRequest struct {
	Type  string
	Hint  string
	Tools []string
}

// hitlRequestOf reads the HITL request off a task's status message: nil for a
// task that is not paused on the extension.
func hitlRequestOf(task *a2aTask) *hitlRequest {
	msg := task.Status.Message
	if msg == nil || !slices.Contains(msg.Extensions, hitlExtensionURI) {
		return nil
	}
	payload, ok := msg.Metadata[hitlExtensionURI].(map[string]any)
	if !ok {
		return nil
	}
	req := &hitlRequest{Type: stringAt(payload, "type"), Hint: stringAt(payload, "hint")}
	tools, _ := payload["tools"].([]any)
	for _, t := range tools {
		if m, ok := t.(map[string]any); ok {
			req.Tools = append(req.Tools, firstNonEmpty(stringAt(m, nameKey), stringAt(m, "id")))
		}
	}
	return req
}

// streamedTurn is what the proof keeps of one streamed turn: the task the
// first frame named, the states seen in order, the frames counted by kind,
// the reply text, and the terminal status update.
type streamedTurn struct {
	TaskID  string
	States  []string
	Frames  map[string]int
	Reply   string
	Final   *a2aStatusUpdate
	Error   string
	elapsed time.Duration
	// chunks accumulates the streamed text between complete artifacts.
	chunks strings.Builder
}

// absorb folds one frame into the turn. The reply is the text of the
// complete artifact (the Go ADK's lastChunk with the whole text), else the
// terminal status message's text, else the streamed chunks joined.
func (t *streamedTurn) absorb(f streamFrame) {
	if t.Frames == nil {
		t.Frames = map[string]int{}
	}
	state := func(s string) {
		if s != "" && (len(t.States) == 0 || t.States[len(t.States)-1] != s) {
			t.States = append(t.States, s)
		}
	}
	switch {
	case f.Task != nil:
		t.Frames["task"]++
		t.TaskID = firstNonEmpty(t.TaskID, f.Task.ID)
		state(f.Task.Status.State)
	case f.StatusUpdate != nil:
		t.Frames["statusUpdate"]++
		t.TaskID = firstNonEmpty(t.TaskID, f.StatusUpdate.TaskID)
		state(f.StatusUpdate.Status.State)
		if f.StatusUpdate.Final || streamEndsOn(f.StatusUpdate.Status.State) {
			t.Final = f.StatusUpdate
			if t.Reply == "" && f.StatusUpdate.Status.Message != nil {
				t.Reply = wireText(f.StatusUpdate.Status.Message.Parts)
			}
		}
	case f.ArtifactUpdate != nil:
		t.Frames["artifactUpdate"]++
		text := wireText(f.ArtifactUpdate.Artifact.Parts)
		if f.ArtifactUpdate.LastChunk {
			t.Reply = text
		} else if text != "" {
			t.chunks.WriteString(text)
		}
	case f.Message != nil:
		t.Frames["message"]++
		if t.Reply == "" {
			t.Reply = wireText(f.Message.Parts)
		}
	case f.Error != nil:
		t.Frames["error"]++
		t.Error = f.Error.Code + ": " + f.Error.Message
	}
}

// reply is the reply text, falling back to the streamed chunks.
func (t *streamedTurn) reply() string {
	return strings.TrimSpace(firstNonEmpty(t.Reply, t.chunks.String()))
}

// streamEndsOn reports whether a status update in this state is the turn's
// terminal one: A2A's terminal and interrupted states. The spec flags that
// event `final: true`; kagent's Go executor path omits the flag and closes the
// stream on the state, so the state is what tells.
func streamEndsOn(state string) bool {
	switch state {
	case taskStateCompleted, taskStateCanceled, taskStateFailedA2A, taskStateRejected, taskStateInputRequired, taskStateAuthRequired:
		return true
	}
	return false
}

// finalState is the terminal state the stream ended in, "" when it ended
// without a terminal status update.
func (t *streamedTurn) finalState() string {
	if t.Final == nil {
		return ""
	}
	return t.Final.Status.State
}

// portalInstallation is one entry of GET /kagent/installations: the
// installation and whether its controller is reachable from the portal
// (true, false or "unknown" while no probe has settled).
type portalInstallation struct {
	Name      string `json:"name"`
	Reachable any    `json:"reachable"`
	Reason    string `json:"reason"`
}

// portalInstance is an AgentInstance as the sessions routes return it (the
// controller's AgentInstance in proto3 JSON, under agentInstance).
type portalInstance struct {
	ID            string `json:"id"`
	Creator       string `json:"creator"`
	State         string `json:"state"`
	Name          string `json:"name"`
	ContextID     string `json:"contextId"`
	AgentTemplate struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"agentTemplate"`
}

// portalAgentRef names the agent a session or a message is for, the way the
// routes take it.
type portalAgentRef struct {
	Namespace, Name string
}

func (a portalAgentRef) body() map[string]any {
	return map[string]any{"agentNamespace": a.Namespace, "agentName": a.Name}
}

// createSession is POST /kagent/sessions: the person's AgentInstance of the
// agent, titled, keyed by requestId. Returns the status too, so a 409 for a
// reused requestId with other parameters is judged by the caller.
func (ps *portalSession) createSession(agent portalAgentRef, name, requestID string) (int, *portalInstance, []byte, error) {
	body := agent.body()
	body[nameKey] = name
	body["requestId"] = requestID
	var out struct {
		AgentInstance *portalInstance `json:"agentInstance"`
	}
	status, raw, err := ps.kagentJSON(http.MethodPost, kagentSessionsPath, body, &out)
	if err != nil || status != http.StatusCreated {
		return status, nil, raw, err
	}
	if out.AgentInstance == nil || out.AgentInstance.ID == "" {
		return status, nil, raw, fmt.Errorf("POST %s answered %d without an agentInstance.id: %.300s", kagentSessionsPath, status, raw)
	}
	return status, out.AgentInstance, raw, nil
}

// listSessions is GET /kagent/sessions: the caller's instances.
func (ps *portalSession) listSessions() ([]portalInstance, error) {
	var out struct {
		AgentInstances []portalInstance `json:"agentInstances"`
	}
	status, raw, err := ps.kagentJSON(http.MethodGet, kagentSessionsPath, nil, &out)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET %s answered %d: %.300s", kagentSessionsPath, status, raw)
	}
	return out.AgentInstances, nil
}

// getSession is GET /kagent/sessions/:id: the instance, or the status the
// route answered (404 for another person's or a deleted one).
func (ps *portalSession) getSession(id string) (int, *portalInstance, error) {
	var out struct {
		AgentInstance *portalInstance `json:"agentInstance"`
	}
	status, raw, err := ps.kagentJSON(http.MethodGet, kagentSessionsPath+"/"+id, nil, &out)
	if err != nil {
		return status, nil, err
	}
	if status == http.StatusOK && out.AgentInstance == nil {
		return status, nil, fmt.Errorf("GET %s/%s answered 200 without an agentInstance: %.300s", kagentSessionsPath, id, raw)
	}
	return status, out.AgentInstance, nil
}

// renameSession is PUT /kagent/sessions/:id {name}.
func (ps *portalSession) renameSession(id, name string) error {
	status, raw, err := ps.kagentRequest(http.MethodPut, kagentSessionsPath+"/"+id, map[string]any{nameKey: name})
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("PUT %s/%s answered %d: %.300s", kagentSessionsPath, id, status, raw)
	}
	return nil
}

// deleteSession is DELETE /kagent/sessions/:id, then the read that must 404.
func (ps *portalSession) deleteSession(id string) error {
	status, raw, err := ps.kagentRequest(http.MethodDelete, kagentSessionsPath+"/"+id, nil)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("DELETE %s/%s answered %d: %.300s", kagentSessionsPath, id, status, raw)
	}
	var last int
	gone := waitFor(int(portalSessionGoneTimeout/pollInterval), pollInterval, func() bool {
		last, _, err = ps.getSession(id)
		return err == nil && last == http.StatusNotFound
	})
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("session %s still answers %d %s after the delete", id, last, portalSessionGoneTimeout)
	}
	return nil
}

// listTasks is GET /kagent/sessions/:id/tasks: the conversation's A2A tasks.
func (ps *portalSession) listTasks(id string) ([]a2aTask, error) {
	var out struct {
		Tasks []a2aTask `json:"tasks"`
	}
	status, raw, err := ps.kagentJSON(http.MethodGet, kagentSessionsPath+"/"+id+"/tasks", nil, &out)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET %s/%s/tasks answered %d: %.300s", kagentSessionsPath, id, status, raw)
	}
	return out.Tasks, nil
}

// streamTurn is POST /kagent/sessions/:id/messages/stream: one message,
// the frames folded into a streamedTurn as they arrive. onTask, when given,
// runs once the task frame names the turn (the Stop proof cancels there).
func (ps *portalSession) streamTurn(id string, agent portalAgentRef, text string, onTask func(taskID string)) (*streamedTurn, error) {
	body := agent.body()
	body["messageId"] = uuid.NewString()
	body["text"] = text
	turn := &streamedTurn{}
	started := time.Now()
	err := ps.kagentStream(kagentSessionsPath+"/"+id+"/messages/stream", body, func(f streamFrame) bool {
		named := turn.TaskID
		turn.absorb(f)
		if onTask != nil && named == "" && turn.TaskID != "" {
			onTask(turn.TaskID)
		}
		return true
	})
	turn.elapsed = time.Since(started)
	if err != nil {
		return turn, err
	}
	if turn.TaskID == "" {
		return turn, fmt.Errorf("the stream on session %s ended after %d frames without a task frame", id, frameCount(turn))
	}
	return turn, nil
}

func frameCount(t *streamedTurn) int {
	n := 0
	for _, c := range t.Frames {
		n += c
	}
	return n
}

// answer is POST /kagent/sessions/:id/answer: the decision on the
// confirmation the named task is paused on. A 202 is the turn still running
// after the answer was accepted — fine, the tasks are polled after.
func (ps *portalSession) answer(id string, agent portalAgentRef, taskID, decision string) error {
	body := agent.body()
	body["messageId"] = uuid.NewString()
	body["taskId"] = taskID
	body["decision"] = decision
	status, raw, err := ps.kagentRequest(http.MethodPost, kagentSessionsPath+"/"+id+"/answer", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return fmt.Errorf("POST %s/%s/answer for task %s answered %d: %.300s", kagentSessionsPath, id, taskID, status, raw)
	}
	return nil
}

// cancelTask is POST /kagent/sessions/:id/tasks/:taskId/cancel — the Stop —
// answering the task as the controller left it.
func (ps *portalSession) cancelTask(id, taskID string) (*a2aTask, error) {
	var task a2aTask
	status, raw, err := ps.kagentJSON(http.MethodPost, kagentSessionsPath+"/"+id+"/tasks/"+taskID+"/cancel", nil, &task)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("POST %s/%s/tasks/%s/cancel answered %d: %.300s", kagentSessionsPath, id, taskID, status, raw)
	}
	return &task, nil
}

// waitInstanceState polls the session until the instance reports one of the
// wanted states, bounded; the last state seen comes back either way.
func (ps *portalSession) waitInstanceState(id string, timeout time.Duration, want ...string) (string, bool, error) {
	var last string
	var err error
	reached := waitFor(int(timeout/pollInterval), pollInterval, func() bool {
		var status int
		var inst *portalInstance
		status, inst, err = ps.getSession(id)
		if err != nil || status != http.StatusOK {
			return false
		}
		last = inst.State
		return slices.Contains(want, last)
	})
	return last, reached, err
}

// waitTaskSettled polls the session's tasks until the named task is out of
// the working states (completed, failed, canceled, or paused for input),
// bounded, and returns it.
func (ps *portalSession) waitTaskSettled(id, taskID string, timeout time.Duration) (*a2aTask, error) {
	var found *a2aTask
	var err error
	settled := waitFor(int(timeout/pollInterval), pollInterval, func() bool {
		var tasks []a2aTask
		tasks, err = ps.listTasks(id)
		if err != nil {
			return false
		}
		found = nil
		for i := range tasks {
			if tasks[i].ID == taskID {
				found = &tasks[i]
			}
		}
		return found != nil && found.Status.State != taskStateWorking && found.Status.State != "TASK_STATE_SUBMITTED" && found.Status.State != ""
	})
	if err != nil {
		return nil, err
	}
	if !settled {
		state := "not listed"
		if found != nil {
			state = found.Status.State
		}
		return found, fmt.Errorf("task %s of session %s did not settle within %s (last %s)", taskID, id, timeout, state)
	}
	return found, nil
}

// musterAttributedTurn checks muster's audit log for the person's forwarded
// token being accepted since the turn began — the agent's own tool call
// reaching muster as the person. No portal call touches muster during a turn
// (the kagent routes go to the controller), so the window is the agent's.
func musterAttributedTurn(email string, since time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	logs, err := podLogs(ctx, platformNamespace, "deploy/"+componentMuster, componentMuster, since+5*time.Second)
	if err != nil {
		return fmt.Errorf("reading muster's log: %w", err)
	}
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, musterTokenAcceptedLog) && strings.Contains(line, `"email":"`+email+`"`) {
			return nil
		}
	}
	return fmt.Errorf("muster logged no %s audit event for %s during the turn — the agent's tool call did not reach muster as the person", musterTokenAcceptedLog, email)
}

// codeword is the fact turn one tells and turn two must recall: a word the
// model cannot guess, readable in the notes.
func codeword() string {
	return "amber-" + strings.ToLower(rand.Text()[:6])
}

// proveChat drives the Sessions pages for the primary user on the agent, and
// the boundary for the others: the installation is offered, the identity
// probe names the person, a session is created on the first message with a
// requestId (a repeat answers the same instance), the turn streams and the
// agent's tool call is attributed to the person by muster, the instance
// suspends after the turn and the next message resumes the conversation with
// its context, rename and the tasks/states/usage reads work, another user
// sees none of it, and the delete leaves nothing. Returns the verdict lines.
func proveChat(primary *portalSession, others []*portalSession, agent portalAgentRef) ([]string, error) {
	var verdicts []string
	email := primary.user.Email

	step("The Sessions pages as %s: the installation is offered and the identity probe names the person", email)
	var installations struct {
		Installations []portalInstallation `json:"installations"`
	}
	if status, raw, err := primary.kagentJSON(http.MethodGet, kagentInstallationsPath, nil, &installations); err != nil || status != http.StatusOK {
		return nil, fmt.Errorf("GET %s answered %d %v: %.300s", kagentInstallationsPath, status, err, raw)
	}
	idx := slices.IndexFunc(installations.Installations, func(i portalInstallation) bool { return i.Name == platformRelease })
	if idx < 0 {
		return nil, fmt.Errorf("GET %s does not offer installation %s: %+v", kagentInstallationsPath, platformRelease, installations.Installations)
	}
	if reachable := installations.Installations[idx].Reachable; reachable == false {
		return nil, fmt.Errorf("the portal reports installation %s not reachable from where it runs (%s) — the controller route is not what the portal expects", platformRelease, installations.Installations[idx].Reason)
	}
	var me map[string]any
	if status, raw, err := primary.kagentJSON(http.MethodGet, kagentMePath, nil, &me); err != nil || status != http.StatusOK {
		return nil, fmt.Errorf("GET %s answered %d %v: %.300s", kagentMePath, status, err, raw)
	}
	if !slices.Contains(collectStrings(me, "email"), email) && !slices.Contains(collectStrings(me, "sub"), email) {
		return nil, fmt.Errorf("GET %s resolved %v, not the person %s — the controller attributes the call to someone else", kagentMePath, me, email)
	}
	note("installation %s reachable=%v; %s resolves to %s", platformRelease, installations.Installations[idx].Reachable, kagentMePath, email)

	step("A session on %s/%s is created on the first message with a requestId — the repeat answers the same instance", agent.Namespace, agent.Name)
	requestID := uuid.NewString()
	status, instance, raw, err := primary.createSession(agent, portalSessionName, requestID)
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("POST %s answered %d: %.300s", kagentSessionsPath, status, raw)
	}
	sessionID := instance.ID
	defer func() {
		if status, _, err := primary.getSession(sessionID); err == nil && status != http.StatusNotFound {
			if err := primary.deleteSession(sessionID); err != nil {
				note("cleanup: %v", err)
			}
		}
	}()
	if instance.Creator != email {
		return nil, fmt.Errorf("the instance's creator is %q, not the person %s", instance.Creator, email)
	}
	if instance.AgentTemplate.Name != agent.Name || instance.AgentTemplate.Namespace != agent.Namespace {
		return nil, fmt.Errorf("the instance binds %s/%s, not %s/%s", instance.AgentTemplate.Namespace, instance.AgentTemplate.Name, agent.Namespace, agent.Name)
	}
	status, repeat, raw, err := primary.createSession(agent, portalSessionName, requestID)
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated || repeat.ID != sessionID {
		return nil, fmt.Errorf("the repeat with requestId %s answered %d with instance %q, wanted the same %s (idempotent create): %.200s", requestID, status, repeat.ID, sessionID, raw)
	}
	mine, err := primary.listSessions()
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(mine, func(i portalInstance) bool { return i.ID == sessionID }) {
		return nil, fmt.Errorf("GET %s for %s does not list the session %s just created", kagentSessionsPath, email, sessionID)
	}
	note("session %s (creator %s, state %s, title %q); the repeat answered the same id; listed among %d of %s's", sessionID, instance.Creator, instance.State, instance.Name, len(mine), email)
	verdicts = append(verdicts, fmt.Sprintf("PASS: POST %s creates the person's AgentInstance (%s, creator %s) and answers the same instance for the same requestId", kagentSessionsPath, sessionID, email))

	word := codeword()
	step("Turn 1 streams (SSE) and the agent's tool call reaches muster as %s", email)
	turn1, err := primary.streamTurn(sessionID, agent, fmt.Sprintf("The codeword of this conversation is %s. %s", word, portalToolPrompt), nil)
	if err != nil {
		note("the first turn failed (%s) — one retry, a cold worker's ResumeActor deadline is not retried by Substrate", excerpt(err.Error(), 200))
		if turn1, err = primary.streamTurn(sessionID, agent, fmt.Sprintf("The codeword of this conversation is %s. %s", word, portalToolPrompt), nil); err != nil {
			return nil, err
		}
	}
	if turn1.finalState() != taskStateCompleted {
		return nil, fmt.Errorf("turn 1 ended %q after %d frames (states %v, error %q): %s", turn1.finalState(), frameCount(turn1), turn1.States, turn1.Error, excerpt(turn1.reply(), 200))
	}
	if turn1.reply() == "" {
		return nil, fmt.Errorf("turn 1 completed without a reply text (frames %v)", turn1.Frames)
	}
	if err := musterAttributedTurn(email, turn1.elapsed); err != nil {
		return nil, err
	}
	note("task %s: %d frames (%v), states %v, %s; reply %q; muster: %s for %s", turn1.TaskID, frameCount(turn1), turn1.Frames, turn1.States, turn1.elapsed.Round(time.Second), excerpt(turn1.reply(), 80), musterTokenAcceptedLog, email)
	verdicts = append(verdicts, fmt.Sprintf("PASS: the turn streams through POST %s/:id/messages/stream (%d frames: task, %d status updates, %d artifact updates) and muster attributes the agent's tool call to %s (%s)", kagentSessionsPath, frameCount(turn1), turn1.Frames["statusUpdate"], turn1.Frames["artifactUpdate"], email, musterTokenAcceptedLog))

	// The gateway gives the worker back at the end of every turn — the
	// runtime is quiesced into a snapshot — while the instance's logical
	// state stays READY; SUSPENDED is the explicit suspend. Either is the
	// quiescent instance the next turn resumes from.
	step("The instance is quiescent after the turn (its runtime a snapshot), then turn 2 resumes the conversation from it")
	state, quiescent, err := primary.waitInstanceState(sessionID, portalQuiescentTimeout, instanceStateReady, instanceStateSuspended)
	if err != nil {
		return nil, err
	}
	if !quiescent {
		return nil, fmt.Errorf("the instance %s is still %s %s after turn 1 completed — a quiescent instance reads %s (its runtime a snapshot) or %s; check `kubectl -n %s logs deploy/kagent-controller`", sessionID, state, portalQuiescentTimeout, instanceStateReady, instanceStateSuspended, kagentNamespace)
	}
	turn2, err := primary.streamTurn(sessionID, agent, portalRecallPrompt, nil)
	if err != nil {
		return nil, err
	}
	if turn2.finalState() != taskStateCompleted {
		return nil, fmt.Errorf("turn 2 ended %q (states %v, error %q): %s", turn2.finalState(), turn2.States, turn2.Error, excerpt(turn2.reply(), 200))
	}
	if !strings.Contains(turn2.reply(), word) {
		return nil, fmt.Errorf("turn 2 answered %q — the codeword %s of turn 1 is not in it: the resumed instance lost its context", excerpt(turn2.reply(), 200), word)
	}
	note("instance %s after turn 1; turn 2 (%s) recalled %q", state, turn2.elapsed.Round(time.Second), word)
	verdicts = append(verdicts, fmt.Sprintf("PASS: the instance was %s after turn 1 and turn 2 resumed the conversation intact (the codeword %s recalled)", state, word))

	step("Rename, the conversation's tasks, the derived states and usage")
	if err := primary.renameSession(sessionID, portalSessionRenamed); err != nil {
		return nil, err
	}
	status, renamed, err := primary.getSession(sessionID)
	if err != nil || status != http.StatusOK {
		return nil, fmt.Errorf("GET %s/%s after the rename: %d %v", kagentSessionsPath, sessionID, status, err)
	}
	if renamed.Name != portalSessionRenamed {
		return nil, fmt.Errorf("the session is titled %q after the rename, wanted %q", renamed.Name, portalSessionRenamed)
	}
	tasks, err := primary.listTasks(sessionID)
	if err != nil {
		return nil, err
	}
	completed := 0
	for _, t := range tasks {
		if t.Status.State == taskStateCompleted {
			completed++
		}
	}
	if completed < 2 {
		return nil, fmt.Errorf("GET %s/%s/tasks lists %d completed tasks after two turns", kagentSessionsPath, sessionID, completed)
	}
	for _, path := range []string{kagentSessionStatesPath, kagentSessionUsagePath} {
		if status, raw, err := primary.kagentRequest(http.MethodGet, path, nil); err != nil || status != http.StatusOK {
			return nil, fmt.Errorf("GET %s answered %d %v: %.300s", path, status, err, raw)
		}
	}
	note("renamed to %q; %d tasks (%d completed); %s and %s answer 200", renamed.Name, len(tasks), completed, kagentSessionStatesPath, kagentSessionUsagePath)
	verdicts = append(verdicts, fmt.Sprintf("PASS: rename (PUT), the tasks list (%d completed turns), session-states and session-usage read as the person", completed))

	for _, other := range others {
		step("%s sees none of it", other.user.Email)
		theirs, err := other.listSessions()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", other.user.Email, err)
		}
		if slices.ContainsFunc(theirs, func(i portalInstance) bool { return i.ID == sessionID }) {
			return nil, fmt.Errorf("GET %s for %s lists %s's session %s — sessions are not scoped to their creator", kagentSessionsPath, other.user.Email, email, sessionID)
		}
		if status, _, err := other.getSession(sessionID); err != nil || status != http.StatusNotFound {
			return nil, fmt.Errorf("GET %s/%s as %s answered %d %v, wanted 404 (another person's session reads as absent)", kagentSessionsPath, sessionID, other.user.Email, status, err)
		}
		note("%s: %d own sessions, %s not among them; GET → 404", other.user.Email, len(theirs), sessionID)
		verdicts = append(verdicts, fmt.Sprintf("PASS: %s does not see %s's session (not listed, GET → 404)", other.user.Email, email))
	}

	step("Delete leaves nothing")
	if err := primary.deleteSession(sessionID); err != nil {
		return nil, err
	}
	mine, err = primary.listSessions()
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(mine, func(i portalInstance) bool { return i.ID == sessionID }) {
		return nil, fmt.Errorf("GET %s still lists %s after the delete", kagentSessionsPath, sessionID)
	}
	note("session %s deleted: GET → 404, not listed", sessionID)
	verdicts = append(verdicts, fmt.Sprintf("PASS: DELETE %s/:id removes the session (GET → 404, unlisted)", kagentSessionsPath))
	return verdicts, nil
}

// proveHITLAndStop drives the two interruption paths of the session page on
// an agent whose muster binding requires approval: a tool call pauses the
// task (input-required, a tool_approval_request under the HITL extension),
// the answer route approves it naming the task, rounds until the turn
// completes; then Stop — a long turn cancelled server-side from the task the
// stream named — and a following turn that completes. The session is the
// proof's and deleted on every path.
func proveHITLAndStop(primary *portalSession, agent portalAgentRef) ([]string, error) {
	var verdicts []string
	step("HITL on %s: a session, a tool prompt that pauses on approval, the answer through …/answer", agent.Name)
	status, instance, raw, err := primary.createSession(agent, portalSessionName+" hitl", uuid.NewString())
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("POST %s answered %d: %.300s", kagentSessionsPath, status, raw)
	}
	sessionID := instance.ID
	defer func() {
		if err := primary.deleteSession(sessionID); err != nil {
			note("cleanup: %v", err)
		}
	}()
	turn, err := primary.streamTurn(sessionID, agent, portalToolPrompt, nil)
	if err != nil {
		note("the first turn failed (%s) — one retry, a cold worker's ResumeActor deadline is not retried by Substrate", excerpt(err.Error(), 200))
		if turn, err = primary.streamTurn(sessionID, agent, portalToolPrompt, nil); err != nil {
			return nil, err
		}
	}
	if turn.finalState() != taskStateInputRequired {
		return nil, fmt.Errorf("the tool prompt on %s ended %q (states %v), wanted %s — the binding did not pause the tool call for approval: %s", agent.Name, turn.finalState(), turn.States, taskStateInputRequired, excerpt(turn.reply(), 200))
	}
	task, err := primary.waitTaskSettled(sessionID, turn.TaskID, kubeReadTimeout)
	if err != nil {
		return nil, err
	}
	rounds := 0
	var approved []string
	for task.Status.State == taskStateInputRequired {
		req := hitlRequestOf(task)
		if req == nil || req.Type != hitlToolApprovalType || len(req.Tools) == 0 {
			return nil, fmt.Errorf("task %s is paused (%s) but carries no %s under %s: %+v", task.ID, task.Status.State, hitlToolApprovalType, hitlExtensionURI, task.Status.Message)
		}
		if rounds++; rounds > portalHITLRounds {
			return nil, fmt.Errorf("task %s still asks for approval after %d rounds (%v)", task.ID, portalHITLRounds, approved)
		}
		approved = append(approved, strings.Join(req.Tools, "+"))
		if err := primary.answer(sessionID, agent, task.ID, hitlDecisionApprove); err != nil {
			return nil, err
		}
		if task, err = primary.waitTaskSettled(sessionID, task.ID, portalHITLResumeTimeout); err != nil {
			return nil, err
		}
	}
	if task.Status.State != taskStateCompleted {
		return nil, fmt.Errorf("task %s ended %s after %d approvals (%v): %s", task.ID, task.Status.State, rounds, approved, excerpt(taskReplyText(task), 200))
	}
	note("task %s paused on %s; approved %d round(s) (%s) through …/answer; completed: %q", task.ID, hitlToolApprovalType, rounds, strings.Join(approved, " → "), excerpt(taskReplyText(task), 80))
	verdicts = append(verdicts, fmt.Sprintf("PASS: HITL — the tool call pauses the task (%s, %s) and POST %s/:id/answer approving it (%d round(s): %s) resumes the same task to %s", taskStateInputRequired, hitlToolApprovalType, kagentSessionsPath, rounds, strings.Join(approved, " → "), taskStateCompleted))

	step("Stop: a long turn cancelled server-side from the task the stream named, then a following turn completes")
	var cancelled *a2aTask
	var cancelErr error
	turn, err = primary.streamTurn(sessionID, agent, portalStopPrompt, func(taskID string) {
		cancelled, cancelErr = primary.cancelTask(sessionID, taskID)
	})
	if err != nil {
		return nil, err
	}
	if cancelErr != nil {
		return nil, cancelErr
	}
	settled, err := primary.waitTaskSettled(sessionID, turn.TaskID, portalStopSettleTimeout)
	if err != nil {
		return nil, err
	}
	if settled.Status.State != taskStateCanceled {
		return nil, fmt.Errorf("task %s is %s after the cancel (the cancel answered %s; stream states %v), wanted %s", turn.TaskID, settled.Status.State, cancelled.Status.State, turn.States, taskStateCanceled)
	}
	after, err := primary.streamTurn(sessionID, agent, portalPongPrompt, nil)
	if err != nil {
		return nil, err
	}
	if after.finalState() != taskStateCompleted || !strings.Contains(strings.ToLower(after.reply()), "pong") {
		return nil, fmt.Errorf("the turn after the Stop ended %q with %q, wanted a completed pong", after.finalState(), excerpt(after.reply(), 80))
	}
	note("task %s cancelled (the cancel answered %s, the task settled %s; stream ended %v after %d frames); the next turn completed with %q", turn.TaskID, cancelled.Status.State, settled.Status.State, turn.States, frameCount(turn), excerpt(after.reply(), 40))
	verdicts = append(verdicts, fmt.Sprintf("PASS: Stop — POST %s/:id/tasks/:taskId/cancel leaves the task %s and the session takes the next turn", kagentSessionsPath, taskStateCanceled))
	return verdicts, nil
}

// taskReplyText is the text of a task's last agent message: its status
// message when it carries text, else the last artifact's.
func taskReplyText(task *a2aTask) string {
	if task.Status.Message != nil {
		if text := wireText(task.Status.Message.Parts); text != "" {
			return text
		}
	}
	for i := len(task.Artifacts) - 1; i >= 0; i-- {
		if text := wireText(task.Artifacts[i].Parts); text != "" {
			return text
		}
	}
	return ""
}
