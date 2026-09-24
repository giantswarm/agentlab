package lab

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// Fixed names of the fakes.
const (
	testUser     = "admin@lab.local"
	testInstance = "inst-1"
	testTask     = "task-1"
	testOldTask  = "old"
	testThread   = "t-1"
	toolCallTool = "call_tool"
	// recordKey names a gateway log record's kind; testSubject is the fake
	// person's Dex subject.
	recordKey   = "record"
	testSubject = "CiQ"
	// testCardTitle opens an approval card; testApproved is its rewrite.
	testCardTitle = "*Approval required*"
	testApproved  = "Approved by <@UP>"
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
	if url := nestedString(carrier.Object, "spec", "url"); url != klausGatewayMusterURL {
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
	if model := nestedString(admitted.Object, "spec", "modelConfig", "name"); model != "my-model" {
		t.Errorf("admitted modelConfig = %q", model)
	}
	tools, _ := nestedValue(admitted.Object, "spec", "tools").([]any)
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

// proofInstance is the fixture's one AgentInstance as the fake serves it to
// the person whose bearer is testToken.
func proofInstance(id, template string) *apiv1alpha1.AgentInstance {
	return &apiv1alpha1.AgentInstance{Id: id, Creator: testToken,
		Harness:       &apiv1alpha1.ResourceReference{Namespace: kagentNamespace, Name: kagentHarness},
		AgentTemplate: &apiv1alpha1.ResourceReference{Namespace: kagentNamespace, Name: template}}
}

// TestControllerReads: the listing is narrowed to the fixture's template, taskIDs
// is the set of the instance's tasks, and waitCanceledTask finds the task new
// since the snapshot that reached TASK_STATE_CANCELED — or says what the new
// tasks are.
func TestControllerReads(t *testing.T) {
	ctrl := newFakeKagent()
	ctrl.instances[testInstance] = proofInstance(testInstance, klausGatewayTestAgent)
	ctrl.instances["other"] = proofInstance("other", "another-template")
	ctrl.setTaskState(testOldTask, a2a.TaskStateCompleted)
	ctrl.setTaskState("new", a2a.TaskStateCanceled)
	api := ctrl.serve(t, testToken)

	instances, err := templateInstances(context.Background(), api)
	if err != nil || len(instances) != 1 || instances[0].GetId() != testInstance {
		t.Errorf("templateInstances = %v %v (the other template's instance must not be listed)", instanceIDs(instances), err)
	}
	ids, err := api.taskIDs(context.Background(), testInstance)
	if err != nil || !ids[testOldTask] || !ids["new"] {
		t.Errorf("taskIDs = %v %v", ids, err)
	}
	canceled, err := api.waitCanceledTask(testInstance, map[a2a.TaskID]bool{testOldTask: true}, 3*time.Second)
	if err != nil || canceled != "new" {
		t.Errorf("waitCanceledTask = %q %v", canceled, err)
	}
	ctrl.setTaskState("new", a2a.TaskStateWorking)
	if _, err := api.waitCanceledTask(testInstance, map[a2a.TaskID]bool{testOldTask: true}, 2*pollInterval); err == nil || !strings.Contains(err.Error(), "new TASK_STATE_WORKING") {
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
	accepted, calls, subjects := musterAttribution(logs, testUser, "CiRjNGNlZGFmNS0...", since)
	if accepted != 1 || calls != 1 || len(subjects) != 2 {
		t.Errorf("accepted=%d calls=%d subjects=%v", accepted, calls, subjects)
	}
}

// TestGatewayArgs: the binary shape's flags name the loopback ports, the bolt
// routing store, Slack in events mode on the fake with the run's secrets,
// the grpcs target with the CA and the fixture as the default agent, OBO on
// the seeded bolt link store; the image shape wraps the same flags in docker
// run on the host network with the mounts and the caller's uid.
func TestGatewayArgs(t *testing.T) {
	args := gatewayArgs(gatewayFlags{port: 18090, dataDir: "/run/x", caFile: "/run/ca.crt", target: "grpcs://agentgateway.127.0.0.1.nip.io:8445",
		slackAPI: "http://127.0.0.1:18092/api", musterURL: "https://muster.127.0.0.1.nip.io:8445"})
	want := []string{
		"--listen-address=127.0.0.1:18090", "--admin-address=127.0.0.1:18091", "--log-level=info",
		"--store=bolt", "--bolt-path=/run/x/routes.bolt",
		"--slack-enabled=true", "--slack-mode=events", "--slack-secrets-file=/run/x/slack-secrets.yaml", "--slack-api-base=http://127.0.0.1:18092/api",
		"--a2a-enabled=true", "--a2a-url=grpcs://agentgateway.127.0.0.1.nip.io:8445", "--a2a-ca-file=/run/ca.crt",
		"--a2a-namespace=kagent", "--a2a-default-agent=" + klausGatewayTestAgent,
		"--obo-enabled=true", "--obo-muster-url=https://muster.127.0.0.1.nip.io:8445", "--obo-callback-base-url=http://127.0.0.1:18090",
		"--obo-state-key-file=/run/x/obo-state.key", "--obo-store=bolt", "--obo-store-path=/run/x/links.bolt", "--obo-store-key-file=/run/x/obo-store.key",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("gatewayArgs =\n%q\nwant\n%q", args, want)
	}
	docker := dockerRunArgs("img:1", "ctr", "/run/x", "/lab/certs/ca.crt", 1000, 100, []string{"--flag"})
	wantDocker := []string{"run", "--name", "ctr", "--network", "host", "--rm", "--user", "1000:100",
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

// TestGatewayProcessExitsBeforeReady: a gateway that dies before it is ready
// (a build that rejects a flag) fails start() promptly with its exit, and the
// stop() on that path returns — the process's end must satisfy every later
// wait.
func TestGatewayProcessExitsBeforeReady(t *testing.T) {
	bin, err := exec.LookPath("false")
	if err != nil {
		t.Skip("no false binary")
	}
	g := newGatewayProcess(KlausGatewayTestOptions{GatewayBinary: bin, Port: 18099}.withDefaults(), t.TempDir(), "/ca", "grpcs://x:1", "http://s/api", "https://m")
	done := make(chan error, 1)
	go func() { done <- g.start() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "exited before serving "+gatewayReadyPath) {
			t.Errorf("start() = %v, want the exit before serving", err)
		}
	case <-time.After(klausGatewayStartWait):
		t.Fatal("start() hung after the gateway exited")
	}
	if g.cmd != nil || g.exited != nil {
		t.Error("a failed start left the process fields set")
	}
	if err := g.stop(); err != nil {
		t.Errorf("stop() after the failed start: %v", err)
	}
}

// TestGatewayRecords: the records of one kind come off the log in order,
// pod prefix or not, narrowed to a thread by thread_id or (instance_bound)
// thread; the version comes from the starting record.
func TestGatewayRecords(t *testing.T) {
	logs := strings.Join([]string{
		`{"time":"t","level":"INFO","msg":"klaus-gateway starting","version":"3.2.0","git_sha":"b1005ed"}`,
		`{"time":"t","msg":"channels: thread bound to agent instance","record":"instance_bound","thread":"1.1","instance":"inst-a"}`,
		`[pod/x] {"time":"t","msg":"slack: dispatching turn","record":"turn_dispatch","thread_id":"1.1","agent":"kagent/agentlab-klaus-gateway-test","subject":"admin@lab.local","sub":"CiQ"}`,
		`{"time":"t","msg":"slack: turn complete","record":"turn_complete","thread_id":"1.1","outcome":"completed","task_id":"t1"}`,
		`{"time":"t","msg":"slack: turn complete","record":"turn_complete","thread_id":"2.2","outcome":"input_required","task_id":"t2"}`,
		`{"time":"t","msg":"not ours","record":"turn_completed"}`,
		`not json "record":"turn_complete"`,
	}, "\n")
	done := gatewayRecords(logs, recordTurnDone)
	if len(done) != 2 || done[0].Outcome != outcomeCompleted || done[1].TaskID != "t2" {
		t.Fatalf("turn_complete = %+v", done)
	}
	if got := threadRecords(done, "2.2"); len(got) != 1 || got[0].Outcome != outcomeInputReq {
		t.Errorf("thread 2.2 = %+v", got)
	}
	bound := threadRecords(gatewayRecords(logs, recordBound), "1.1")
	if len(bound) != 1 || bound[0].Instance != "inst-a" {
		t.Errorf("instance_bound = %+v", bound)
	}
	if d := gatewayRecords(logs, recordDispatch); len(d) != 1 || d[0].Subject != testUser || d[0].thread() != "1.1" {
		t.Errorf("turn_dispatch = %+v", d)
	}
	if v := gatewayVersion(logs); v != "klaus-gateway 3.2.0 (b1005ed)" {
		t.Errorf("version = %q", v)
	}
	g := newGatewayProcess(KlausGatewayTestOptions{GatewayBinary: "/bin/x"}.withDefaults(), t.TempDir(), "", "", "", "")
	if g.describe() != "binary /bin/x" || newGatewayProcess(KlausGatewayTestOptions{}.withDefaults(), "", "", "", "", "").describe() != "image "+KlausGatewayImageDefault {
		t.Error("describe")
	}
	if err := g.stop(); err != nil {
		t.Errorf("stopping a gateway that never ran: %v", err)
	}
	if err := portsFree(0); err != nil {
		t.Errorf("a free port: %v", err)
	}
}

// testJWT is an unsigned JWT with the claims given.
func testJWT(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// TestTokenIdentity: the subject and expiry come off the id_token, a token
// that would not outlive the run is refused, and the link caches the token
// with its expiry next to a placeholder refresh token.
func TestTokenIdentity(t *testing.T) {
	now := time.Now()
	tok := testJWT(map[string]any{claimSubject: testSubject, claimExpiry: now.Add(24 * time.Hour).Unix()})
	id, err := tokenIdentity(tok, now)
	if err != nil || id.subject != testSubject || !id.expiry.Equal(time.Unix(now.Add(24*time.Hour).Unix(), 0)) {
		t.Fatalf("tokenIdentity = %+v %v", id, err)
	}
	link := id.link(testUser, tok)
	if link.Sub != testSubject || link.Email != testUser || link.IDToken != tok || !link.Expiry.Equal(id.expiry) || link.RefreshToken != linkRefreshMarker {
		t.Errorf("link = %+v", link)
	}
	if _, err := tokenIdentity(testJWT(map[string]any{claimSubject: testSubject, claimExpiry: now.Add(10 * time.Minute).Unix()}), now); err == nil || !strings.Contains(err.Error(), "expires in") {
		t.Errorf("a short-lived token: %v", err)
	}
	if _, err := tokenIdentity(testJWT(map[string]any{claimExpiry: now.Add(24 * time.Hour).Unix()}), now); err == nil {
		t.Error("a token without sub")
	}
}

// TestGatewayFiles: the run's secrets land in the run directory readable by
// the caller alone, the signing secret in the Slack secrets file; a link
// seeded into the bolt store reads back through the gateway's package with
// the store key.
func TestGatewayFiles(t *testing.T) {
	dir := t.TempDir()
	keys, err := writeGatewayFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{gatewaySecrets, gatewayStateKey, gatewayStoreKey} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", name, info, err)
		}
	}
	secrets, _ := os.ReadFile(filepath.Clean(filepath.Join(dir, gatewaySecrets)))
	if !strings.Contains(string(secrets), "signing_secret: "+keys.signing) || !strings.Contains(string(secrets), "bot_token: ") {
		t.Errorf("secrets file = %q", secrets)
	}
	id := linkedIdentity{subject: testSubject, expiry: time.Now().Add(time.Hour).Truncate(time.Second)}
	link := id.link(testUser, "id-token")
	path := filepath.Join(dir, gatewayLinksFile)
	if err := seedBoltLink(path, keys.store, "UP", link); err != nil {
		t.Fatal(err)
	}
	store, err := musterlink.OpenBoltStore(path, keys.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	got, err := store.Get("UP")
	if err != nil || !sameLink(got, link) {
		t.Errorf("read back %+v %v, wrote %+v", got, err, link)
	}
}

// scriptedGateway plays klaus-gateway's Slack adapter for the proof's
// driving code: it checks every request's signature, answers messages and
// clicks the way the adapter does — through the fake Slack Web API — writes
// the adapter's records into its log, and moves the fake controller's tasks
// the way the turns would.
type scriptedGateway struct {
	t        *testing.T
	secret   string
	fake     *fakeSlack
	ctrl     *fakeKagent
	stranger string

	mu        sync.Mutex
	log       strings.Builder
	bound     map[string]string
	approvals int
	running   map[string]chan struct{}
}

func (g *scriptedGateway) logs() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.log.String(), nil
}

func (g *scriptedGateway) record(fields map[string]any) {
	line, _ := json.Marshal(fields)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.log.Write(append(line, '\n'))
}

// api calls the fake Web API as the adapter does, JSON.
func (g *scriptedGateway) api(method string, params map[string]any) map[string]any {
	raw, _ := json.Marshal(params)
	resp, err := http.Post(g.fake.baseURL()+"/"+method, "application/json", bytes.NewReader(raw))
	if err != nil {
		g.t.Errorf("%s: %v", method, err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func (g *scriptedGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Header.Get("X-Slack-Signature") != slackSignature(g.secret, r.Header.Get("X-Slack-Request-Timestamp"), body) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case slackEventsPath:
		var cb struct {
			Event map[string]string `json:"event"`
		}
		_ = json.Unmarshal(body, &cb)
		go g.onMessage(cb.Event)
	case slackInteractionsPath:
		form, _ := url.ParseQuery(string(body))
		var p struct {
			User      struct{ ID string }
			Channel   struct{ ID string }
			Container struct {
				MessageTS string `json:"message_ts"`
				ThreadTS  string `json:"thread_ts"`
			}
			Actions []struct {
				ActionID string `json:"action_id"`
				Value    string `json:"value"`
			}
		}
		_ = json.Unmarshal([]byte(form.Get("payload")), &p)
		go g.onClick(p.User.ID, p.Channel.ID, p.Container.ThreadTS, p.Container.MessageTS, p.Actions[0].ActionID, p.Actions[0].Value)
	}
}

func (g *scriptedGateway) post(channel, thread, text string, blocks ...any) map[string]any {
	params := map[string]any{slackKeyChannel: channel, slackKeyThreadTS: thread, slackKeyText: text}
	if len(blocks) > 0 {
		params[slackKeyBlocks] = blocks
	}
	return g.api(slackPostMessage, params)
}

func (g *scriptedGateway) answer(channel, thread, text string) {
	s := g.api(slackStartStream, map[string]any{slackKeyChannel: channel, slackKeyThreadTS: thread, "username": klausGatewayTestDisplay, "icon_url": klausGatewayTestIcon,
		slackKeyChunks: []any{map[string]any{fieldTypeKey: slackMarkdownChunk, slackKeyText: text}}})
	g.api(slackStopStream, map[string]any{slackKeyChannel: channel, slackKeyTS: s[slackKeyTS]})
}

func (g *scriptedGateway) done(thread, outcome, task string) {
	g.record(map[string]any{recordKey: recordTurnDone, "thread_id": thread, "outcome": outcome, "task_id": task})
}

func (g *scriptedGateway) card(channel, thread, task string) {
	value := `{"t":"` + thread + `","id":"` + task + `"}`
	g.ctrl.setTaskState(a2a.TaskID(task), a2a.TaskStateInputRequired)
	g.post(channel, thread, testCardTitle,
		map[string]any{fieldTypeKey: slackBlockSection, slackKeyText: map[string]any{fieldTypeKey: slackMrkdwn, slackKeyText: "*Approval required* · list namespaces"}},
		map[string]any{fieldTypeKey: slackBlockActions, slackKeyElements: []any{
			map[string]any{fieldTypeKey: slackButton, slackKeyActionID: slackActionApprove, slackKeyValue: value},
			map[string]any{fieldTypeKey: slackButton, slackKeyActionID: slackActionDeny, slackKeyValue: value},
		}})
	g.done(thread, outcomeInputReq, task)
}

func (g *scriptedGateway) onMessage(ev map[string]string) {
	channel, user := ev[slackKeyChannel], ev[slackKeyUser]
	thread := ev[slackKeyThreadTS]
	if thread == "" {
		thread = ev[slackKeyTS]
	}
	text := strings.TrimPrefix(ev[slackKeyText], "<@"+slackFakeBotUser+"> ")
	switch {
	case user == g.stranger:
		g.post(channel, thread, "Waiting for <@"+user+"> to sign in")
		g.api(slackPostEphemeral, map[string]any{slackKeyChannel: channel, slackKeyThreadTS: thread, slackKeyUser: user, slackKeyText: "Sign in",
			slackKeyBlocks: []any{map[string]any{fieldTypeKey: slackBlockActions, slackKeyElements: []any{map[string]any{fieldTypeKey: slackButton, slackKeyActionID: slackActionSignIn, slackKeyURL: "http://gw" + musterlink.LinkPath + "?u=x"}}}}})
		return
	case text == slackAgentCommand:
		g.post(channel, thread, slackRosterHeading+" — start a new conversation with `/agent \"<name>\" <question>`:\n• *"+klausGatewayTestDisplay+"* — Throwaway")
		return
	case strings.HasPrefix(text, slackAgentCommand+" "+klausGatewayTestUnadmitted):
		g.post(channel, thread, "⚠️ *"+klausGatewayTestUnadmittedDisplay+"* is installed but "+slackNotRunnable+": "+unadmittedReason+" this AgentTemplate. "+slackNotStarted+".\n\n"+slackRosterHeading+":\n• *"+klausGatewayTestDisplay+"*")
		return
	case text == slackStopCommand:
		g.mu.Lock()
		stop, running := g.running[thread]
		delete(g.running, thread)
		g.mu.Unlock()
		if !running {
			g.post(channel, thread, "_Nothing is running in this thread._")
			return
		}
		close(stop)
		return
	}
	g.mu.Lock()
	instance, known := g.bound[thread]
	if !known {
		instance = "inst-" + thread
		g.bound[thread] = instance
	}
	g.mu.Unlock()
	if !known {
		g.ctrl.mu.Lock()
		g.ctrl.instances[instance] = proofInstance(instance, klausGatewayTestAgent)
		g.ctrl.mu.Unlock()
		g.record(map[string]any{recordKey: recordBound, "thread": thread, "instance": instance})
	}
	g.record(map[string]any{recordKey: recordDispatch, "thread_id": thread, "agent": kagentNamespace + "/" + klausGatewayTestAgent, "agent_source": "default",
		"slack_user": user, "subject": testUser, claimSubject: testSubject})
	switch text {
	case klausGatewayToolPrompt:
		g.card(channel, thread, "task-"+thread)
	case klausGatewayEssayPrompt:
		stop := make(chan struct{})
		g.mu.Lock()
		g.running[thread] = stop
		g.mu.Unlock()
		g.ctrl.setTaskState(a2a.TaskID("essay-"+thread), a2a.TaskStateWorking)
		g.api(slackStartStream, map[string]any{slackKeyChannel: channel, slackKeyThreadTS: thread, slackKeyChunks: []any{map[string]any{fieldTypeKey: slackMarkdownChunk, slackKeyText: "Once upon a time"}}})
		<-stop
		g.ctrl.setTaskState(a2a.TaskID("essay-"+thread), a2a.TaskStateCanceled)
		g.post(channel, thread, slackStopped)
		g.done(thread, outcomeCanceled, "essay-"+thread)
	default:
		g.answer(channel, thread, klausGatewayWord)
		g.done(thread, outcomeCompleted, "word-"+thread)
	}
}

func (g *scriptedGateway) onClick(user, channel, thread, cardTS, action, value string) {
	var v struct {
		Thread string `json:"t"`
		Task   string `json:"id"`
	}
	_ = json.Unmarshal([]byte(value), &v)
	verdict := "Denied by <@" + user + ">"
	if action == slackActionApprove {
		verdict = "Approved by <@" + user + ">"
	}
	g.api(slackUpdate, map[string]any{slackKeyChannel: channel, slackKeyTS: cardTS, slackKeyText: verdict,
		slackKeyBlocks: []any{map[string]any{fieldTypeKey: "context", slackKeyElements: []any{map[string]any{fieldTypeKey: slackMrkdwn, slackKeyText: verdict}}}}})
	if action == slackActionDeny {
		g.ctrl.setTaskState(a2a.TaskID(v.Task), a2a.TaskStateRejected)
		g.post(channel, v.Thread, "_(the turn failed; please try again)_")
		g.done(v.Thread, "failed", v.Task)
		return
	}
	g.mu.Lock()
	g.approvals++
	first := g.approvals == 1
	g.mu.Unlock()
	if first {
		g.card(channel, v.Thread, v.Task) // the meta-tool call's second pause
		return
	}
	g.ctrl.setTaskState(a2a.TaskID(v.Task), a2a.TaskStateCompleted)
	g.answer(channel, v.Thread, "There are 12 namespaces.")
	g.done(v.Thread, outcomeCompleted, v.Task)
}

// TestSlackProof: the proof's Slack steps against a gateway that answers the
// way the adapter does — the roster, the refusal, the sign-in prompt, a
// branded turn bound to one instance with its dispatch record, two
// approvals on the same task, a denial, /stop, a following turn — and the
// ways they fail: a turn that never pauses, a turn that ends before /stop.
func TestSlackProof(t *testing.T) {
	ctrl := newFakeKagent()
	api := ctrl.serve(t, testToken)
	fake := startTestFake(t, map[string]string{"UP": testUser})
	gw := &scriptedGateway{t: t, secret: "sekrit", fake: fake, ctrl: ctrl, stranger: "US", bound: map[string]string{}, running: map[string]chan struct{}{}}
	srv := httptest.NewServer(gw)
	defer srv.Close()
	p := &slackProof{driver: newSlackDriver(srv.URL, "sekrit", fake, "C1"), fake: fake, channel: "C1", logs: gw.logs}

	names, err := p.roster("UP")
	if err != nil || assertSlackRoster(names) != nil {
		t.Fatalf("roster = %q %v", names, err)
	}
	refusal, err := p.refusal("UP", slackAgentCommand+" "+klausGatewayTestUnadmitted+" hi")
	if err != nil || strings.Contains(refusal, "\n") || !strings.Contains(refusal, unadmittedReason) {
		t.Fatalf("refusal = %q %v", refusal, err)
	}
	if link, err := p.signInPrompt("US", "hi"); err != nil || !strings.Contains(link, musterlink.LinkPath) {
		t.Fatalf("signInPrompt = %q %v", link, err)
	}
	if err := assertNoInstances(api, klausGatewayTestAgent); err != nil {
		t.Fatal(err)
	}

	main := &slackThread{user: "UP"}
	turn, err := p.firstTurn(main, klausGatewayWordPrompt)
	if err != nil {
		t.Fatal(err)
	}
	if err := assertSlackTurnSaid(turn, klausGatewayWord); err != nil {
		t.Error(err)
	}
	if err := assertBranded(turn); err != nil {
		t.Error(err)
	}
	instance, err := p.boundInstance(main)
	if err != nil || instance != "inst-"+main.ts {
		t.Fatalf("boundInstance = %q %v", instance, err)
	}
	if err := assertOnlyInstances(api, instance); err != nil {
		t.Error(err)
	}
	if _, err := p.dispatch(main, testSubject, testUser); err != nil {
		t.Error(err)
	}
	if _, err := p.dispatch(main, "other-sub", testUser); err == nil {
		t.Error("a dispatch under another subject passed")
	}

	turn, err = p.say(main, klausGatewayToolPrompt)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := p.decideUntilSettled(api, instance, main, turn, true)
	if err != nil {
		t.Fatal(err)
	}
	if approved.rounds != 2 || approved.finalState != a2a.TaskStateCompleted || approved.taskID != "task-"+main.ts || !strings.Contains(approved.answer, "12 namespaces") {
		t.Errorf("approved = %+v", approved)
	}
	if err := assertNothingWaiting(api, instance); err != nil {
		t.Error(err)
	}

	denied := &slackThread{user: "UP"}
	turn, err = p.firstTurn(denied, klausGatewayToolPrompt)
	if err != nil {
		t.Fatal(err)
	}
	deniedInstance, _ := p.boundInstance(denied)
	declined, err := p.decideUntilSettled(api, deniedInstance, denied, turn, false)
	if err != nil {
		t.Fatal(err)
	}
	if declined.rounds != 1 || !declined.finalState.Terminal() || declined.outcome != "failed" {
		t.Errorf("declined = %+v", declined)
	}
	if err := assertOnlyInstances(api, instance, deniedInstance); err != nil {
		t.Error(err)
	}

	before, _ := api.taskIDs(context.Background(), instance)
	stopped, err := p.stop(main, klausGatewayEssayPrompt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stopped.answer, "Once upon") {
		t.Errorf("stopped answer = %q", stopped.answer)
	}
	if canceled, err := api.waitCanceledTask(instance, before, 3*time.Second); err != nil || canceled != "essay-"+main.ts {
		t.Errorf("waitCanceledTask = %q %v", canceled, err)
	}
	turn, err = p.say(main, klausGatewayRecallPrompt)
	if err != nil || assertSlackTurnSaid(turn, klausGatewayWord) != nil {
		t.Errorf("the turn after /stop: %+v %v", turn, err)
	}

	// A turn that does not pause cannot be decided; one that ends before
	// /stop is not a stop.
	if _, err := p.decideUntilSettled(api, instance, main, turn, true); err == nil || !strings.Contains(err.Error(), "did not pause") {
		t.Errorf("deciding a completed turn: %v", err)
	}
	if _, err := p.stop(main, klausGatewayWordPrompt); err == nil || !strings.Contains(err.Error(), "not canceled") {
		t.Errorf("stopping a turn that completes: %v", err)
	}
}
