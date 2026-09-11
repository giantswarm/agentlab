package lab

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// The portal proof's readers and assertions against a fake Backstage backend:
// the routes the proof drives, answering recorded shapes (the controller's
// proto3 JSON as the agent-platform backend relays it, agent-manager's tool
// payloads as the muster plugin's backend hands them back, the gs backend's
// skill discovery, the Kubernetes proxy's lists).

const (
	testPortalUser    = "admin@lab.local"
	testPortalViewer  = "viewer@lab.local"
	testBSToken       = "bs-token"
	testDexToken      = "dex-token"
	testSessionID     = "01a08e6b-11fd-7689-b9c3-0b5ed46d960a"
	testTaskID        = "01a08e6b-122c-7f07-9986-04757ec3e601"
	testContextID     = "3e878c9d-e9d8-494d-8081-cce681e03fe0"
	testSkillCommit   = "cb1fb768bbbbcaa035b884a99ad308b14f846468"
	testShortCommit   = "cb1fb768"
	testHarnessName   = "kagent"
	testRosterRelease = "probe"
	testOtherHarness  = "claude"
	testRefMain       = "main"
	testArtifactID    = "a-1"
	fieldParts        = "parts"
	fieldTaskID       = "taskId"
	fieldContextID    = "contextId"
	fieldState        = "state"
	fieldError        = "error"
	fieldArtifact     = "artifact"
	fieldMessageID    = "messageId"
	fieldArtifactID   = "artifactId"
	fieldTools        = "tools"
	fieldRole         = "role"
	frameArtifact     = "artifactUpdate"
)

// fakePortal is the fake Backstage backend: a handler per route, the muster
// call_tool payloads per tool, and what it captured.
type fakePortal struct {
	t        *testing.T
	mux      *http.ServeMux
	srv      *httptest.Server
	tools    map[string]func(args map[string]any) (string, bool)
	captured map[string][]map[string]any
	headers  map[string]http.Header
}

func newFakePortal(t *testing.T) *fakePortal {
	t.Helper()
	fp := &fakePortal{t: t, mux: http.NewServeMux(), tools: map[string]func(map[string]any) (string, bool){}, captured: map[string][]map[string]any{}, headers: map[string]http.Header{}}
	fp.mux.HandleFunc(portalMusterAPI+"/call", func(w http.ResponseWriter, r *http.Request) {
		fp.headers[r.URL.Path] = r.Header.Clone()
		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &call); err != nil {
			t.Errorf("call body: %v", err)
		}
		tool := strings.TrimPrefix(call.Name, agentManagerToolPrefix)
		fp.captured[tool] = append(fp.captured[tool], call.Arguments)
		answer, ok := fp.tools[tool]
		if !ok {
			t.Errorf("unexpected tool %s", call.Name)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// As the portal's backend answers: the tool's payload itself on a
		// 200; a tool-level refusal thrown, so Backstage's error body.
		text, isError := answer(call.Arguments)
		if isError {
			writeJSON(w, http.StatusInternalServerError, map[string]any{fieldError: map[string]any{nameKey: "Error", "message": text}, "response": map[string]any{"statusCode": http.StatusInternalServerError}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(text))
	})
	fp.srv = httptest.NewServer(fp.mux)
	t.Cleanup(fp.srv.Close)
	return fp
}

// session is a signed-in session pointed at the fake.
func (fp *fakePortal) session(email string, groups ...string) *portalSession {
	return &portalSession{
		cfg: config.Default(), user: &config.User{Email: email, Groups: groups}, base: fp.srv.URL, client: fp.srv.Client(),
		bsToken: testBSToken, dexIDToken: testDexToken,
	}
}

// handle registers a JSON route answering status and body, recording the
// headers it saw.
func (fp *fakePortal) handle(path string, status int, body any) {
	fp.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		fp.headers[r.URL.Path] = r.Header.Clone()
		writeJSON(w, status, body)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	raw, ok := body.([]byte)
	if !ok {
		raw, _ = json.Marshal(body)
	}
	_, _ = w.Write(raw)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestPortalToolCall: the muster plugin's route carries the person's token in
// backstage-muster-authorization and the tool's arguments; agent-manager's
// refusal comes back worded as the tool answered it, so the code is judged by
// prefix.
func TestPortalToolCall(t *testing.T) {
	fp := newFakePortal(t)
	fp.tools["get_info"] = func(map[string]any) (string, bool) {
		return mustJSON(t, map[string]any{fieldHarness: map[string]any{nameKey: testHarnessName}, "chart": map[string]any{"semver": agentChartRange}}), false
	}
	fp.tools["create_agent"] = func(map[string]any) (string, bool) { return "conflict: an agent named probe exists", true }
	ps := fp.session(testPortalUser, platformAdminsGroup)
	var info agentManagerInfo
	if err := portalToolCall(ps, "get_info", nil, &info); err != nil {
		t.Fatal(err)
	}
	if info.Harness.Name != testHarnessName || info.Chart.Semver != agentChartRange {
		t.Errorf("info = %+v", info)
	}
	if got := fp.headers[portalMusterAPI+"/call"].Get(portalMusterAuthHeader); got != testDexToken {
		t.Errorf("%s = %q, want the Dex id_token", portalMusterAuthHeader, got)
	}
	if got := fp.headers[portalMusterAPI+"/call"].Get("Authorization"); got != "Bearer "+testBSToken {
		t.Errorf("Authorization = %q", got)
	}
	err := portalToolCall(ps, "create_agent", map[string]any{nameKey: "probe"}, nil)
	if err == nil || !strings.Contains(err.Error(), refusalConflict) {
		t.Errorf("a refusal: %v", err)
	}
	if got := fp.captured["create_agent"]; len(got) != 1 || got[0][nameKey] != "probe" {
		t.Errorf("captured = %v", got)
	}
}

// TestDiscoverSkills: the gs backend's discovery answers every skill pinned
// to the one commit the listing was read at, a full id; a listing whose
// entries carry a short or a different commit is refused.
func TestDiscoverSkills(t *testing.T) {
	fp := newFakePortal(t)
	good := skillDiscovery{Ref: testRefMain, Commit: testSkillCommit, Skills: []discoveredSkill{
		{Name: "agent-self-awareness", Path: "agent-self-awareness", RepoURL: skillsTestRepo, Ref: testRefMain, Commit: testSkillCommit},
		{Name: testSkillName, Path: "plugins/gs-base/skills/runbooks", RepoURL: skillsTestRepo, Ref: testRefMain, Commit: testSkillCommit},
	}}
	fp.handle(portalSkillsPath, http.StatusOK, good)
	ps := fp.session(testPortalUser, platformAdminsGroup)
	d, err := discoverSkills(ps, skillsTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Skills) != 2 || d.Commit != testSkillCommit {
		t.Errorf("discovery = %+v", d)
	}
	entry := d.Skills[1].skillEntry()
	if entry.Name != testSkillName || entry.Path != "plugins/gs-base/skills/runbooks" || entry.Git == nil || entry.Git.Commit != testSkillCommit || entry.Git.URL != skillsTestRepo || entry.Git.Ref != "" {
		t.Errorf("skillEntry = %+v", entry)
	}
	if got := fp.headers[portalSkillsPath].Get("Authorization"); got != "Bearer "+testBSToken {
		t.Errorf("Authorization = %q", got)
	}

	short := good
	short.Commit = testShortCommit
	short.Skills = []discoveredSkill{{Name: "x", Path: "x", RepoURL: skillsTestRepo, Commit: testShortCommit}}
	fp2 := newFakePortal(t)
	fp2.handle(portalSkillsPath, http.StatusOK, short)
	if _, err := discoverSkills(fp2.session(testPortalUser), skillsTestRepo); err == nil || !strings.Contains(err.Error(), "full commit id") {
		t.Errorf("a short commit: %v", err)
	}
	fp3 := newFakePortal(t)
	fp3.handle(portalSkillsPath, http.StatusNotFound, map[string]any{fieldError: map[string]any{fieldMessage: "Failed to discover skills: HTTP 404"}})
	if _, err := discoverSkills(fp3.session(testPortalUser), skillsTestRepo); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a missing repository: %v", err)
	}
}

// dryRunReport is a validate_agent answer for the spec — the manifests
// agent-manager renders, as the review page shows them.
func dryRunReport(spec agentSpec, mode string, values map[string]any) validateReport {
	source := fmt.Sprintf("apiVersion: source.toolkit.fluxcd.io/v1\nkind: OCIRepository\nmetadata:\n  name: agent\n  namespace: kagent\nspec:\n  interval: 30m\n  url: %s\n  ref:\n    semver: %s\n", agentChartURL, agentChartRange)
	release := fmt.Sprintf("apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: %s\n  namespace: kagent\nspec:\n  serviceAccountName: %s\n  chartRef:\n    kind: OCIRepository\n    name: agent\n", spec.Name, kagentFluxServiceAccount)
	return validateReport{Valid: true, Mode: mode, SchemaVersion: "1.1.0", SchemaSource: "oci", Manifests: agentManifests{OCIRepository: source, HelmRelease: release, Values: values}}
}

func testInfo() *agentManagerInfo {
	info := &agentManagerInfo{}
	info.Harness.Name = testHarnessName
	info.Chart.Semver = agentChartRange
	info.Flux.ServiceAccountName = kagentFluxServiceAccount
	return info
}

// TestAssertDryRun: the review page's dry run passes when it renders the
// spec — the source at 1.x, the release as the tenant ServiceAccount, the
// values on the platform Harness without a runtime, the skills at the
// discovered commits — and each deviation is named.
func TestAssertDryRun(t *testing.T) {
	spec := testSpec()
	values := jsonRoundTrip(agentValues(spec)).(map[string]any)
	if err := assertDryRun(dryRunReportPtr(spec, validateModeCreate, values), spec, testInfo()); err != nil {
		t.Fatalf("a matching dry run: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(r *validateReport)
		want   string
	}{
		"refused":     {func(r *validateReport) { r.Valid, r.Errors = false, []string{"/toolset: required"} }, "refuses"},
		"update mode": {func(r *validateReport) { r.Mode = validateModeUpdate }, "mode"},
		"x.x.x range": {func(r *validateReport) {
			r.Manifests.OCIRepository = strings.Replace(r.Manifests.OCIRepository, "semver: 1.x", "semver: x.x.x", 1)
		}, "1.x"},
		"other SA": {func(r *validateReport) {
			r.Manifests.HelmRelease = strings.Replace(r.Manifests.HelmRelease, kagentFluxServiceAccount, "default", 1)
		}, "serviceAccountName"},
		"runtime":      {func(r *validateReport) { r.Manifests.Values["agent"].(map[string]any)["runtime"] = "go" }, "runtime"},
		fieldHarness:   {func(r *validateReport) { r.Manifests.Values["agent"].(map[string]any)["harness"] = testOtherHarness }, "harness"},
		"toolset list": {func(r *validateReport) { r.Manifests.Values[toolsetKey] = []any{presetNone} }, toolsetKey},
		"skill pin": {func(r *validateReport) {
			r.Manifests.Values[skillsKey].([]any)[0].(map[string]any)["git"].(map[string]any)["commit"] = testHeadCommit
		}, "discovered commit"},
	} {
		t.Run(name, func(t *testing.T) {
			report := dryRunReportPtr(spec, validateModeCreate, jsonRoundTrip(agentValues(spec)).(map[string]any))
			tc.mutate(report)
			if err := assertDryRun(report, spec, testInfo()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want an error naming %q, got %v", tc.want, err)
			}
		})
	}
}

func dryRunReportPtr(spec agentSpec, mode string, values map[string]any) *validateReport {
	r := dryRunReport(spec, mode, values)
	return &r
}

// TestPortalValidateAgent: the review page's call carries the create
// arguments plus the ModelConfig's namespace.
func TestPortalValidateAgent(t *testing.T) {
	fp := newFakePortal(t)
	spec := testSpec()
	fp.tools["validate_agent"] = func(map[string]any) (string, bool) {
		return mustJSON(t, dryRunReport(spec, validateModeCreate, agentValues(spec))), false
	}
	report, err := portalValidateAgent(fp.session(testPortalUser), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Valid || report.Mode != validateModeCreate {
		t.Errorf("report = %+v", report)
	}
	want, _ := createAgentArgs(spec)
	want[namespaceKey] = kagentNamespace
	if got := fp.captured["validate_agent"]; len(got) != 1 || !reflect.DeepEqual(jsonRoundTrip(got[0]), jsonRoundTrip(want)) {
		t.Errorf("arguments = %v, want %v", got, want)
	}
}

// streamFixture is the recorded stream of one Go ADK turn as the backend
// relays it: the task snapshot, a working status, streamed artifact chunks,
// the complete artifact, the terminal status update — carrying the state,
// not the spec's `final` flag, as kagent's Go path sends it.
func streamFixture(state string) string {
	frames := []map[string]any{
		{"task": map[string]any{"id": testTaskID, fieldContextID: testContextID, fieldStatus: map[string]any{fieldState: "TASK_STATE_SUBMITTED"},
			"history": []map[string]any{{fieldMessageID: "m-1", fieldRole: "ROLE_USER", fieldParts: []map[string]any{{fieldText: "Reply with exactly the word pong."}}}}}},
		{"statusUpdate": map[string]any{fieldTaskID: testTaskID, fieldContextID: testContextID, fieldStatus: map[string]any{fieldState: taskStateWorking}}},
		{frameArtifact: map[string]any{fieldTaskID: testTaskID, fieldArtifact: map[string]any{fieldArtifactID: testArtifactID, fieldParts: []map[string]any{{fieldText: "p"}}}}},
		{frameArtifact: map[string]any{fieldTaskID: testTaskID, fieldArtifact: map[string]any{fieldArtifactID: testArtifactID, fieldParts: []map[string]any{{fieldText: "ong"}}}, "append": true}},
		{frameArtifact: map[string]any{fieldTaskID: testTaskID, fieldArtifact: map[string]any{fieldArtifactID: testArtifactID, fieldParts: []map[string]any{{fieldText: testPong}}}, "lastChunk": true}},
		{"statusUpdate": map[string]any{fieldTaskID: testTaskID, fieldContextID: testContextID,
			fieldStatus: map[string]any{fieldState: state, "message": map[string]any{fieldMessageID: "m-2", fieldRole: "ROLE_AGENT", fieldParts: []map[string]any{{fieldText: testPong}}}}}},
	}
	var b strings.Builder
	for _, f := range frames {
		raw, _ := json.Marshal(f)
		b.WriteString("data: " + string(raw) + "\n\n")
	}
	return b.String()
}

// TestStreamedTurnFinal: the terminal status update is the one flagged
// `final` (the spec) or the one in a terminal or interrupted state (kagent's
// Go path, which omits the flag); a working update is neither.
func TestStreamedTurnFinal(t *testing.T) {
	update := func(state string, final bool) streamFrame {
		return streamFrame{StatusUpdate: &a2aStatusUpdate{TaskID: testTaskID, Status: a2aTaskStatus{State: state}, Final: final}}
	}
	for _, tc := range []struct {
		frame streamFrame
		want  string
	}{
		{update(taskStateWorking, false), ""},
		{update(taskStateWorking, true), taskStateWorking},
		{update(taskStateCompleted, false), taskStateCompleted},
		{update(taskStateInputRequired, false), taskStateInputRequired},
		{update(taskStateCanceled, false), taskStateCanceled},
	} {
		var turn streamedTurn
		turn.absorb(tc.frame)
		if got := turn.finalState(); got != tc.want {
			t.Errorf("%s final=%v: finalState() = %q, want %q", tc.frame.StatusUpdate.Status.State, tc.frame.StatusUpdate.Final, got, tc.want)
		}
	}
}

// TestStreamTurn: the streaming route's SSE frames fold into the turn — the
// task id off the snapshot, the states in order, the reply from the complete
// artifact, the terminal update — with the person's token on the request; a
// route that is not an event stream is refused by name, and an error frame
// after events is kept as the turn's error.
func TestStreamTurn(t *testing.T) {
	fp := newFakePortal(t)
	var body map[string]any
	fp.mux.HandleFunc(portalKagentAPI+kagentSessionsPath+"/"+testSessionID+"/messages/stream", func(w http.ResponseWriter, r *http.Request) {
		fp.headers[r.URL.Path] = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, streamFixture(taskStateCompleted))
	})
	ps := fp.session(testPortalUser, platformAdminsGroup)
	agent := portalAgentRef{Namespace: kagentNamespace, Name: testAgentName}
	var named string
	turn, err := ps.streamTurn(testSessionID, agent, portalPongPrompt, func(taskID string) { named = taskID })
	if err != nil {
		t.Fatal(err)
	}
	if turn.TaskID != testTaskID || named != testTaskID || turn.reply() != testPong || turn.finalState() != taskStateCompleted {
		t.Errorf("turn = %+v (reply %q, final %q)", turn, turn.reply(), turn.finalState())
	}
	if !reflect.DeepEqual(turn.States, []string{"TASK_STATE_SUBMITTED", taskStateWorking, taskStateCompleted}) || turn.Frames["artifactUpdate"] != 3 || turn.Frames["statusUpdate"] != 2 || turn.Frames["task"] != 1 {
		t.Errorf("states %v frames %v", turn.States, turn.Frames)
	}
	path := portalKagentAPI + kagentSessionsPath + "/" + testSessionID + "/messages/stream"
	if got := fp.headers[path].Get(portalKagentAuthHeader); got != testDexToken {
		t.Errorf("%s = %q", portalKagentAuthHeader, got)
	}
	if body["agentName"] != testAgentName || body["agentNamespace"] != kagentNamespace || body["text"] != portalPongPrompt || body["messageId"] == "" {
		t.Errorf("body = %v", body)
	}

	// A refusal before the stream opens, and a stream broken mid-turn.
	fp2 := newFakePortal(t)
	fp2.handle(portalKagentAPI+kagentSessionsPath+"/"+testSessionID+"/messages/stream", http.StatusConflict, map[string]any{fieldError: map[string]any{fieldMessage: "a turn is running"}})
	if _, err := fp2.session(testPortalUser).streamTurn(testSessionID, agent, portalPongPrompt, nil); err == nil || !strings.Contains(err.Error(), "409") {
		t.Errorf("a 409: %v", err)
	}
	var broken streamedTurn
	if err := readSSE(strings.NewReader("data: {\"task\":{\"id\":\"t\"}}\n\ndata: {\"error\":{\"code\":\"Unavailable\",\"message\":\"drained\"}}\n\n"), func(f streamFrame) bool { broken.absorb(f); return true }); err != nil {
		t.Fatal(err)
	}
	if broken.TaskID != "t" || broken.Error != "Unavailable: drained" || broken.finalState() != "" {
		t.Errorf("broken = %+v", broken)
	}
	if err := readSSE(strings.NewReader("data: not json\n\n"), func(streamFrame) bool { return true }); err == nil {
		t.Error("a non-JSON frame must fail")
	}
}

// TestHITLRequestOf: a task paused for approval carries the typed request
// under the extension URI with the tools to approve; a completed task or one
// paused without the extension carries none.
func TestHITLRequestOf(t *testing.T) {
	paused := map[string]any{"id": testTaskID, fieldStatus: map[string]any{fieldState: taskStateInputRequired, fieldMessage: map[string]any{
		fieldMessageID: "m", fieldRole: "ROLE_AGENT",
		fieldParts:    []any{map[string]any{fieldText: "Please approve or reject the tool call filter_tools()"}},
		fieldMetadata: map[string]any{hitlExtensionURI: map[string]any{fieldType: hitlToolApprovalType, "hint": hitlDecisionApprove, fieldTools: []any{map[string]any{nameKey: "filter_tools", "args": map[string]any{"query": "namespaces"}, "id": "adk-1", "call_id": "toolu_1"}}}},
		"extensions":  []any{hitlExtensionURI},
	}}}
	var task a2aTask
	if err := json.Unmarshal([]byte(mustJSON(t, paused)), &task); err != nil {
		t.Fatal(err)
	}
	req := hitlRequestOf(&task)
	if req == nil || req.Type != hitlToolApprovalType || !reflect.DeepEqual(req.Tools, []string{"filter_tools"}) || req.Hint != hitlDecisionApprove {
		t.Errorf("request = %+v", req)
	}
	if got := taskReplyText(&task); !strings.Contains(got, "filter_tools") {
		t.Errorf("taskReplyText = %q", got)
	}
	task.Status.Message.Extensions = nil
	if hitlRequestOf(&task) != nil {
		t.Error("without the extension there is no request")
	}
	if hitlRequestOf(&a2aTask{Status: a2aTaskStatus{State: taskStateCompleted}}) != nil {
		t.Error("a completed task carries no request")
	}
}

// TestSessionsRoutes: the sessions routes as the fake answers them — the
// create's 201 with the instance, the list, the read, a rename, a delete
// followed by the 404 — with the person's token on every call and the
// requestId in the create body.
func TestSessionsRoutes(t *testing.T) {
	fp := newFakePortal(t)
	instance := map[string]any{"id": testSessionID, "creator": testPortalUser, fieldState: instanceStateReady, nameKey: portalSessionName, fieldContextID: testContextID,
		"agentTemplate": map[string]any{fieldNamespace: kagentNamespace, nameKey: testAgentName}}
	var createBody map[string]any
	deleted := false
	fp.mux.HandleFunc(portalKagentAPI+kagentSessionsPath, func(w http.ResponseWriter, r *http.Request) {
		fp.headers[r.URL.Path] = r.Header.Clone()
		switch r.Method {
		case http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &createBody)
			writeJSON(w, http.StatusCreated, map[string]any{"agentInstance": instance})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"agentInstances": []any{instance}})
		}
	})
	fp.mux.HandleFunc(portalKagentAPI+kagentSessionsPath+"/"+testSessionID, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			deleted = true
			writeJSON(w, http.StatusOK, map[string]any{})
		case r.Method == http.MethodPut:
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			instance["name"] = body[nameKey]
			writeJSON(w, http.StatusOK, map[string]any{})
		case deleted:
			writeJSON(w, http.StatusNotFound, map[string]any{fieldError: map[string]any{fieldMessage: "gone"}})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"agentInstance": instance})
		}
	})
	ps := fp.session(testPortalUser, platformAdminsGroup)
	agent := portalAgentRef{Namespace: kagentNamespace, Name: testAgentName}
	status, created, _, err := ps.createSession(agent, portalSessionName, "req-1")
	if err != nil || status != http.StatusCreated || created.ID != testSessionID || created.Creator != testPortalUser || created.AgentTemplate.Name != testAgentName {
		t.Fatalf("create: %d %+v %v", status, created, err)
	}
	if createBody["requestId"] != "req-1" || createBody["agentName"] != testAgentName || createBody[nameKey] != portalSessionName {
		t.Errorf("create body = %v", createBody)
	}
	if got := fp.headers[portalKagentAPI+kagentSessionsPath].Get(portalKagentAuthHeader); got != testDexToken {
		t.Errorf("%s = %q", portalKagentAuthHeader, got)
	}
	list, err := ps.listSessions()
	if err != nil || len(list) != 1 || list[0].ID != testSessionID {
		t.Errorf("list = %v %v", list, err)
	}
	if err := ps.renameSession(testSessionID, portalSessionRenamed); err != nil {
		t.Fatal(err)
	}
	if status, got, err := ps.getSession(testSessionID); err != nil || status != http.StatusOK || got.Name != portalSessionRenamed {
		t.Errorf("after the rename: %d %+v %v", status, got, err)
	}
	if err := ps.deleteSession(testSessionID); err != nil {
		t.Fatal(err)
	}
	if status, _, err := ps.getSession(testSessionID); err != nil || status != http.StatusNotFound {
		t.Errorf("after the delete: %d %v", status, err)
	}
}

// rosterTemplate is one AgentTemplate as the Kubernetes proxy lists it.
func rosterTemplate(name, harness string, ready bool, binds string) map[string]any {
	status := map[string]any{"observedGeneration": 1, "harnesses": []any{}}
	if harness != "" {
		conditions := []map[string]any{{fieldType: conditionAccepted, fieldStatus: conditionTrue}, {fieldType: conditionReady, fieldStatus: condFalseStatus, "reason": readyReasonPending}}
		if ready {
			conditions[1] = map[string]any{fieldType: conditionReady, fieldStatus: conditionTrue}
		}
		status["harnesses"] = []any{map[string]any{fieldHarness: harness, "desiredRevision": "r1", "latestSuccessfulRevision": "r1", fieldConditions: conditions}}
	}
	spec := map[string]any{"modelConfig": map[string]any{nameKey: defaultModelConfig}}
	if binds != "" {
		spec["tools"] = []any{map[string]any{"mcp": map[string]any{"server": map[string]any{fieldKind: remoteMCPServerKind, nameKey: binds}}}}
	}
	return map[string]any{
		fieldAPIVersion: agentTemplateAPIVersion, fieldKind: "AgentTemplate",
		fieldMetadata: map[string]any{nameKey: name, fieldNamespace: kagentNamespace, "generation": 1,
			"labels":      map[string]any{harnessLabel: testHarnessName, fluxHelmReleaseNameLabel: name},
			"annotations": map[string]any{displayNameAnnotation: "Display " + name}},
		"spec": spec, fieldStatus: status,
	}
}

// TestRoster: the roster joins the templates with their carriers the way
// the portal does — readiness by the deciding Harness, the toolset off the
// carrier's header, the owning release off the provenance label — and the
// per-user rule: a platform-admin reads it, anyone else meets the
// apiserver's 403.
func TestRoster(t *testing.T) {
	fp := newFakePortal(t)
	templates := map[string]any{"items": []any{
		rosterTemplate(testRosterRelease, testHarnessName, true, testRosterRelease),
		rosterTemplate("compiling", testHarnessName, false, "compiling"),
		rosterTemplate("orphan", "", false, ""),
	}}
	carriers := map[string]any{"items": []any{map[string]any{
		fieldAPIVersion: agentTemplateAPIVersion, fieldKind: remoteMCPServerKind,
		fieldMetadata: map[string]any{nameKey: testRosterRelease, fieldNamespace: kagentNamespace},
		"spec":        map[string]any{"url": testMusterURL, "headersFrom": []any{map[string]any{nameKey: toolsetHeader, "value": presetReadOnly + "," + workflowIncidentTriage}}},
	}}}
	fp.mux.HandleFunc(portalKubeProxyAPI+"/apis/"+agentTemplateAPIVersion+"/agenttemplates", func(w http.ResponseWriter, r *http.Request) {
		fp.headers[r.URL.Path] = r.Header.Clone()
		if r.Header.Get(portalKubeAuthHeader) == "viewer-token" {
			writeJSON(w, http.StatusForbidden, map[string]any{fieldMessage: "forbidden"})
			return
		}
		writeJSON(w, http.StatusOK, templates)
	})
	fp.handle(portalKubeProxyAPI+"/apis/"+agentTemplateAPIVersion+"/remotemcpservers", http.StatusOK, carriers)
	admin := fp.session(testPortalUser, platformAdminsGroup)
	status, rows, err := listRoster(admin)
	if err != nil || status != http.StatusOK || len(rows) != 3 {
		t.Fatalf("roster: %d %v %v", status, rows, err)
	}
	if got := fp.headers[portalKubeProxyAPI+"/apis/"+agentTemplateAPIVersion+"/agenttemplates"]; got.Get(portalKubeClusterHeader) != platformRelease || got.Get(portalKubeAuthHeader) != testDexToken {
		t.Errorf("proxy headers = %v", got)
	}
	want := rosterRow{Name: testRosterRelease, Namespace: kagentNamespace, DisplayName: "Display " + testRosterRelease, Readiness: rosterReady, Harness: testHarnessName,
		Toolset: []string{presetReadOnly, workflowIncidentTriage}, Declared: true, OwningRelease: testRosterRelease}
	if !reflect.DeepEqual(rows[0], want) {
		t.Errorf("row = %+v, want %+v", rows[0], want)
	}
	if rows[1].Readiness != rosterNotReady || rows[1].Declared || rows[2].Readiness != rosterNotAdmitted {
		t.Errorf("rows = %+v", rows[1:])
	}
	spec := agentSpec{Name: testRosterRelease, DisplayName: "Display " + testRosterRelease, Toolset: []string{presetReadOnly, workflowIncidentTriage}}
	viewer := fp.session(testPortalViewer, viewerGroup)
	viewer.dexIDToken = "viewer-token"
	verdicts, err := proveRoster([]*portalSession{admin, viewer}, spec)
	if err != nil || len(verdicts) != 2 || !strings.Contains(verdicts[1], "403") {
		t.Errorf("proveRoster: %v %v", verdicts, err)
	}
	// A developer who can read is a change of the lab's grants, reported.
	dev := fp.session("dev@lab.local", "developers")
	if _, err := proveRoster([]*portalSession{dev}, spec); err == nil || !strings.Contains(err.Error(), "grants kagent.dev") {
		t.Errorf("a readable roster for a non-admin: %v", err)
	}
}

// TestHITLAgentManifest: the fixture's HelmRelease is the direct writer's
// with muster.requireApproval on top and nothing else changed.
func TestHITLAgentManifest(t *testing.T) {
	spec := testSpec()
	manifest, err := hitlAgentManifest(spec)
	if err != nil {
		t.Fatal(err)
	}
	var got, plain map[string]any
	if err := yaml.Unmarshal([]byte(manifest), &got); err != nil {
		t.Fatalf("%v\n%s", err, manifest)
	}
	if err := yaml.Unmarshal([]byte(agentHelmReleaseManifest(spec)), &plain); err != nil {
		t.Fatal(err)
	}
	values := got["spec"].(map[string]any)["values"].(map[string]any)
	muster, _ := values["muster"].(map[string]any)
	if muster[musterRequireApprovalKey] != true {
		t.Errorf("values.muster = %v", values["muster"])
	}
	delete(values, "muster")
	if !reflect.DeepEqual(jsonRoundTrip(got), jsonRoundTrip(plain)) {
		t.Errorf("beyond muster.requireApproval the manifest differs:\n%s\n---\n%s", manifest, agentHelmReleaseManifest(spec))
	}
	if !chartVersionBelow("1.0.5", agentChartWithApproval) || chartVersionBelow("1.1.0", agentChartWithApproval) || chartVersionBelow("1.2.3", agentChartWithApproval) || !chartVersionBelow("", agentChartWithApproval) {
		t.Error("chartVersionBelow")
	}
}

// TestKagentPath: the installation is appended the way the plugin's client
// builds the routes, whether or not the path carries a query already.
func TestKagentPath(t *testing.T) {
	if got := kagentPath(kagentSessionsPath); got != portalKagentAPI+kagentSessionsPath+"?installation="+platformRelease {
		t.Errorf("kagentPath = %q", got)
	}
	if got := kagentPath("/x?y=1"); got != portalKagentAPI+"/x?y=1&installation="+platformRelease {
		t.Errorf("kagentPath with a query = %q", got)
	}
}
