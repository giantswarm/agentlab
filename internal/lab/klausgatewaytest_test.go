package lab

import (
	"bytes"
	"cmp"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"

	"github.com/giantswarm/agentlab/internal/config"
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
	manifests := klausGatewayFixtures("my-model", testOtherHarness)
	objs, err := decodeManifests([]byte(manifests))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 5 {
		t.Fatalf("%d objects, wanted the carrier, two templates and their Agents", len(objs))
	}
	carrier, admitted, admittedAgent, unadmitted, unadmittedAgent := objs[0], objs[1], objs[2], objs[3], objs[4]
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
	if admitted.GetLabels()[managedByLabel] != managedByAgentlabValue {
		t.Errorf("admitted labels = %v", admitted.GetLabels())
	}
	ann := admitted.GetAnnotations()
	if ann["ui.giantswarm.io/display-name"] != klausGatewayTestDisplay || ann["ui.giantswarm.io/icon-url"] != klausGatewayTestIcon {
		t.Errorf("admitted annotations = %v", ann)
	}
	if admittedAgent.GetKind() != kindAgent || admittedAgent.GetName() != klausGatewayTestAgent || admittedAgent.GetAPIVersion() != kagentAPIVersion {
		t.Errorf("admitted Agent = %s %s %s", admittedAgent.GetAPIVersion(), admittedAgent.GetKind(), admittedAgent.GetName())
	}
	if nestedString(admittedAgent.Object, "spec", "templateRef", "name") != klausGatewayTestAgent || nestedString(admittedAgent.Object, "spec", "harnessRef", "name") != testOtherHarness {
		t.Errorf("admitted Agent spec = %v", admittedAgent.Object["spec"])
	}
	if got := admittedAgent.GetAnnotations(); got["ui.giantswarm.io/display-name"] != klausGatewayTestDisplay || got["ui.giantswarm.io/icon-url"] != klausGatewayTestIcon || admittedAgent.GetLabels()[managedByLabel] != managedByAgentlabValue {
		t.Errorf("admitted Agent metadata = %v %v", admittedAgent.GetLabels(), got)
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

	if unadmitted.GetName() != klausGatewayTestUnadmitted || unadmitted.GetLabels()[managedByLabel] != managedByAgentlabValue {
		t.Errorf("unadmitted = %s %v", unadmitted.GetName(), unadmitted.GetLabels())
	}
	if unadmittedAgent.GetKind() != kindAgent || unadmittedAgent.GetName() != klausGatewayTestUnadmitted || nestedString(unadmittedAgent.Object, "spec", "harnessRef", "name") != klausGatewayNoHarness {
		t.Errorf("the unadmitted Agent = %s %s %v", unadmittedAgent.GetKind(), unadmittedAgent.GetName(), unadmittedAgent.Object["spec"])
	}
	if got := unadmittedAgent.GetAnnotations()["ui.giantswarm.io/display-name"]; got != klausGatewayTestUnadmittedDisplay {
		t.Errorf("the unadmitted Agent's display name = %q", got)
	}
}

// TestControllerReads: the listing is narrowed to the fixture's Agent, taskIDs
// is the set of the session's tasks, and waitCanceledTask finds the task new
// since the snapshot that reached TASK_STATE_CANCELED — or says what the new
// tasks are.
func TestControllerReads(t *testing.T) {
	ctrl := newFakeKagent()
	ctrl.sessions[testInstance] = fakeSession(testInstance, testToken, klausGatewayTestAgent)
	ctrl.sessions["other"] = fakeSession("other", testToken, "another-agent")
	ctrl.setTaskState(testOldTask, a2a.TaskStateCompleted)
	ctrl.setTaskState("new", a2a.TaskStateCanceled)
	api := ctrl.serve(t, testToken)

	sessions, err := fixtureSessions(context.Background(), api)
	if err != nil || len(sessions) != 1 || sessions[0].GetId() != testInstance {
		t.Errorf("fixtureSessions = %v %v (the other Agent's session must not be listed)", sessionIDs(sessions), err)
	}
	session := ctrl.sessions[testInstance]
	ids, err := api.taskIDs(context.Background(), session)
	if err != nil || !ids[testOldTask] || !ids["new"] {
		t.Errorf("taskIDs = %v %v", ids, err)
	}
	canceled, err := api.waitCanceledTask(session, map[a2a.TaskID]bool{testOldTask: true}, 3*time.Second)
	if err != nil || canceled != "new" {
		t.Errorf("waitCanceledTask = %q %v", canceled, err)
	}
	ctrl.setTaskState("new", a2a.TaskStateWorking)
	if _, err := api.waitCanceledTask(session, map[a2a.TaskID]bool{testOldTask: true}, 2*pollInterval); err == nil || !strings.Contains(err.Error(), "new TASK_STATE_WORKING") {
		t.Errorf("no cancel: %v", err)
	}
}

// TestMusterAttribution: only records stamped after the turn began count; the
// accepted-token audit record must name the email, an actor bearer record's
// and a tools/call request's truncated subject must prefix the token's
// subject; a pod prefix before the JSON is tolerated.
func TestMusterAttribution(t *testing.T) {
	since := time.Date(2026, 9, 11, 17, 0, 0, 0, time.UTC)
	logs := strings.Join([]string{
		`{"time":"2026-09-11T16:59:59Z","level":"INFO","msg":"security_audit","audit":{"event_type":"forwarded_id_token_accepted","details":{"email":"admin@lab.local"}}}`,
		`[muster-1] {"time":"2026-09-11T17:00:01Z","level":"INFO","msg":"security_audit","audit":{"event_type":"forwarded_id_token_accepted","details":{"email":"admin@lab.local"}}}`,
		`{"time":"2026-09-11T17:00:02Z","level":"INFO","msg":"security_audit","audit":{"event_type":"forwarded_id_token_accepted","details":{"email":"dev@lab.local"}}}`,
		`{"time":"2026-09-11T17:00:03Z","level":"INFO","msg":"tools/call request","subsystem":"MCP-Protocol","subject":"CiRjNGNl...","tool":"call_tool"}`,
		`{"time":"2026-09-11T17:00:04Z","level":"INFO","msg":"tools/call request","subsystem":"MCP-Protocol","subject":"Cg9vdGhl...","tool":"call_tool"}`,
		`{"time":"2026-09-11T17:00:05Z","level":"INFO","msg":"[AUDIT] action=actor_bearer outcome=success subject=CiRjNGNl... target=muster details=actor=spiffe://substrate-actor.local/actor/kagent/session-x session=kagent/x","subsystem":"AUDIT"}`,
		`{"time":"2026-09-11T17:00:06Z","level":"INFO","msg":"[AUDIT] action=actor_bearer outcome=success subject=Cg9vdGhl... target=muster details=actor=spiffe://substrate-actor.local/actor/kagent/session-y session=kagent/y","subsystem":"AUDIT"}`,
		`{"time":"2026-09-11T17:00:07Z","level":"INFO","msg":"[AUDIT] action=actor_bearer outcome=failure target=muster details=actor=spiffe://substrate-actor.local/actor/kagent/session-z error=actor is not bound","subsystem":"AUDIT"}`,
		`not json at all`,
	}, "\n")
	accepted, calls, subjects := musterAttribution(logs, testUser, "CiRjNGNlZGFmNS0...", since)
	if accepted != 2 || calls != 1 || len(subjects) != 2 {
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
	docker := dockerRunArgs("img:1", "ctr", "", "/run/x", "/lab/certs/ca.crt", 1000, 100, []string{"--flag"})
	wantDocker := []string{"run", "--name", "ctr", "--network", "host", "--security-opt", "label=disable", "--rm", "--user", "1000:100",
		"-v", "/run/x:" + gatewayDataPath, "-v", "/lab/certs/ca.crt:" + gatewayCAPath + ":ro", "img:1", "--flag"}
	if !reflect.DeepEqual(docker, wantDocker) {
		t.Errorf("dockerRunArgs =\n%q\nwant\n%q", docker, wantDocker)
	}
	edge, err := edgeHostPort("https://agentgateway.127.0.0.1.nip.io:8445")
	if err != nil || edge != "agentgateway.127.0.0.1.nip.io:8445" {
		t.Errorf("edgeHostPort = %q, %v", edge, err)
	}
	if edge, _ := edgeHostPort("https://agentgateway.example.io"); edge != "agentgateway.example.io:443" {
		t.Errorf("edgeHostPort without a port = %q", edge)
	}
	if got := gatedTarget(edge, 18093); got != "grpcs://agentgateway.127.0.0.1.nip.io:18093" {
		t.Errorf("gatedTarget = %q", got)
	}
	opts := KlausGatewayTestOptions{}.withDefaults()
	if opts.GatewayImage != "" || opts.Port != 18090 || opts.ModelConfig != defaultModelConfig || opts.ReadyTimeout != klausGatewayReadyTimeout {
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

// fakeDockerScript stands in for the docker CLI in $FAKE_DOCKER_DIR: the
// image is cached once pulled (`image inspect` fails before); `run` pulls an
// uncached image itself, taking $FAKE_PULL seconds, then creates the
// container (the file container), marks it ready unless $FAKE_NEVER_READY,
// and runs until killed; `stop` ends it unless $FAKE_STOP_MISSES (the client
// still pulling when the stop came); `rm -f` removes it.
const fakeDockerScript = `#!/bin/sh
d="$FAKE_DOCKER_DIR"
echo "$*" >> "$d/calls"
case "$1" in
image) [ -e "$d/pulled" ] ;;
pull) sleep "$FAKE_PULL"; touch "$d/pulled" ;;
run)
  [ -e "$d/pulled" ] || { sleep "$FAKE_PULL"; touch "$d/pulled"; }
  echo $$ > "$d/pid"; touch "$d/container"
  [ -n "$FAKE_NEVER_READY" ] || touch "$d/ready"
  exec sleep 600 ;;
stop)
  [ -z "$FAKE_STOP_MISSES" ] && [ -e "$d/container" ] || exit 1
  rm -f "$d/container" "$d/ready"; kill "$(cat "$d/pid")" ;;
rm) rm -f "$d/container" "$d/ready" ;;
esac
`

// fakeDockerGateway puts fakeDockerScript first on PATH and serves the
// gateway's readiness on an admin port of its own, 200 while the fake's
// container is ready; it returns the fake's directory and a gateway on it.
func fakeDockerGateway(t *testing.T, env map[string]string) (string, *gatewayProcess) {
	t.Helper()
	bin, dir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, dockerBin), []byte(fakeDockerScript), 0o700); err != nil { // #nosec G306 -- the fake CLI must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_DIR", dir)
	for k, v := range env {
		t.Setenv(k, v)
	}
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err != nil || r.URL.Path != gatewayReadyPath {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(admin.Close)
	u, _ := url.Parse(admin.URL)
	port, _ := strconv.Atoi(u.Port())
	g := newGatewayProcess(KlausGatewayTestOptions{GatewayImage: "gsoci.azurecr.io/giantswarm/klaus-gateway:9.9.9", Port: port - 1}.withDefaults(), t.TempDir(), "/ca", "grpcs://x:1", "http://s/api", "https://m")
	g.container, g.startWait, g.stopWait = "lab-x-"+gatewayContainerSuffix, time.Second, time.Second
	t.Cleanup(func() { _ = g.stop() })
	return dir, g
}

// TestGatewayImagePullNotCounted: an uncached image whose pull outlasts the
// readiness wait still starts — the pull happens before the wait begins.
func TestGatewayImagePullNotCounted(t *testing.T) {
	dir, g := fakeDockerGateway(t, map[string]string{"FAKE_PULL": "2"})
	if err := g.start(); err != nil {
		t.Fatalf("start() with an uncached image = %v", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls")) // #nosec G304 -- the test's own temporary file
	if !strings.Contains(string(calls), "pull -q "+g.opts.GatewayImage) || !strings.Contains(string(calls), "--name lab-x-"+gatewayContainerSuffix) {
		t.Errorf("docker calls =\n%s\nwant the pull and the run of the lab's container", calls)
	}
}

// TestGatewayTimeoutRemovesContainer: after a readiness timeout no container
// is left, even when `docker stop` missed it and the client had to be killed.
func TestGatewayTimeoutRemovesContainer(t *testing.T) {
	dir, g := fakeDockerGateway(t, map[string]string{"FAKE_PULL": "0", "FAKE_NEVER_READY": "1", "FAKE_STOP_MISSES": "1"})
	err := g.start()
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("start() = %v, want the readiness timeout", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "container")); err == nil {
		t.Error("the container outlived the readiness timeout")
	}
}

// TestGatewayTestPortPerLab: each lab's proofs take a block of their own,
// derived from its API server port; the container carries the lab's name.
func TestGatewayTestPortPerLab(t *testing.T) {
	first := &config.Config{ClusterName: config.DefaultClusterName, APIServerPort: config.DefaultAPIServerPort}
	if got := gatewayTestPort(first); got != 18090 {
		t.Errorf("the first lab's port = %d, want 18090", got)
	}
	if got := gatewayContainerName(first); got != "agentlab-klaus-gateway-test" {
		t.Errorf("the first lab's container = %q", got)
	}
	if got := gatewayContainerName(&config.Config{ClusterName: "lab-b"}); got != "lab-b-klaus-gateway-test" {
		t.Errorf("another lab's container = %q", got)
	}
	seen := map[int]int{}
	for _, api := range []int{1, 6443, 6444, 6445, 6543, 6643, 30000, 65535} {
		p := gatewayTestPort(&config.Config{APIServerPort: api})
		if p < 1024 || p+gatewayTestPortStride > 65536 {
			t.Errorf("apiServerPort %d: block %d..%d out of range", api, p, p+gatewayTestPortStride-1)
		}
		for q, other := range seen {
			if p < q+gatewayTestPortStride && q < p+gatewayTestPortStride {
				t.Errorf("apiServerPort %d and %d share ports (%d, %d)", api, other, p, q)
			}
		}
		seen[p] = api
	}
}

// TestGatewayRecords: the records of one kind come off the log in order,
// pod prefix or not, narrowed to a thread by thread_id or (session_bound)
// thread; the version comes from the starting record.
func TestGatewayRecords(t *testing.T) {
	logs := strings.Join([]string{
		`{"time":"t","level":"INFO","msg":"klaus-gateway starting","version":"3.2.0","git_sha":"b1005ed"}`,
		`{"time":"t","msg":"channels: thread bound to session","record":"session_bound","thread":"1.1","session":"inst-a"}`,
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
	if len(bound) != 1 || bound[0].Session != "inst-a" {
		t.Errorf("session_bound = %+v", bound)
	}
	if d := gatewayRecords(logs, recordDispatch); len(d) != 1 || d[0].Subject != testUser || d[0].thread() != "1.1" {
		t.Errorf("turn_dispatch = %+v", d)
	}
	if v := gatewayVersion(logs); v != "klaus-gateway 3.2.0 (b1005ed)" {
		t.Errorf("version = %q", v)
	}
	g := newGatewayProcess(KlausGatewayTestOptions{GatewayBinary: "/bin/x"}.withDefaults(), t.TempDir(), "", "", "", "")
	if g.describe() != "binary /bin/x" || newGatewayProcess(KlausGatewayTestOptions{GatewayImage: klausGatewayImageRepository + ":" + testGatewayVersion}.withDefaults(), "", "", "", "", "").describe() != "image "+klausGatewayImageRepository+":"+testGatewayVersion {
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
// The real gateway's words on its workspace prompt the fake gateway posts:
// the prompt and its card once No workspace answered it.
const (
	slackWorkspaceAsk    = "Which workspace should this conversation work in?"
	slackWorkspaceNoneIn = "Working without a workspace"
)

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
	// declines is how many more times the model answers the count "No."
	// instead of counting, as a terse model does.
	declines int

	// workspaces has a new thread's first message held behind the
	// workspace prompt, as the adapter does for a person with workspaces;
	// held is each prompted thread's message, until No workspace runs it.
	workspaces bool
	held       map[string]map[string]string

	// answering is every answer still in flight: the adapter answers
	// after the request that asked, so the fake Slack it posts to stays
	// up until settle has seen the last one through.
	answering sync.WaitGroup
}

// settle ends the essay turns nobody stopped (a test that failed before
// its stop) and waits until every answer has landed.
func (g *scriptedGateway) settle() {
	g.mu.Lock()
	for thread, stop := range g.running {
		delete(g.running, thread)
		close(stop)
	}
	g.mu.Unlock()
	g.answering.Wait()
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
		g.answering.Go(func() { g.onMessage(cb.Event) })
	case slackInteractionsPath:
		form, _ := url.ParseQuery(string(body))
		var p struct {
			Type        string
			CallbackID  string `json:"callback_id"`
			ResponseURL string `json:"response_url"`
			User        struct{ ID string }
			Channel     struct{ ID string }
			Message     struct {
				TS       string `json:"ts"`
				ThreadTS string `json:"thread_ts"`
			}
			Container struct {
				MessageTS string `json:"message_ts"`
				ThreadTS  string `json:"thread_ts"`
			}
			Actions []struct {
				ActionID string `json:"action_id"`
				Value    string `json:"value"`
			}
			View struct {
				CallbackID      string `json:"callback_id"`
				PrivateMetadata string `json:"private_metadata"`
				State           struct {
					Values map[string]map[string]struct {
						SelectedOption struct{ Value string } `json:"selected_option"`
						Value          string
					}
				}
			}
		}
		_ = json.Unmarshal([]byte(form.Get("payload")), &p)
		switch {
		case p.Type == "message_action" && p.CallbackID == slackShortcutAskAgent:
			thread := cmp.Or(p.Message.ThreadTS, p.Message.TS)
			g.answering.Go(func() { g.onShortcut(p.User.ID, p.Channel.ID, thread, p.ResponseURL) })
		case p.Type == "view_submission":
			values := p.View.State.Values
			g.answering.Go(func() {
				g.onPick(p.User.ID, p.View.PrivateMetadata, values[slackPickerAgentBlock][slackPickerAgentAction].SelectedOption.Value,
					values[slackPickerQuestionBlock][slackPickerQuestionAction].Value)
			})
		case p.Actions[0].ActionID == slackActionAgentSelect:
			g.answering.Go(func() { g.openPicker(p.Channel.ID, p.Container.ThreadTS, p.Actions[0].Value) })
		default:
			g.answering.Go(func() {
				g.onClick(p.User.ID, p.Channel.ID, p.Container.ThreadTS, p.Container.MessageTS, p.Actions[0].ActionID, p.Actions[0].Value)
			})
		}
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
	g.mu.Lock()
	_, known := g.bound[thread]
	g.mu.Unlock()
	switch {
	case user == g.stranger && (text == slackLoginWord || known):
		if known {
			g.post(channel, thread, "Waiting for <@"+user+"> to sign in")
		}
		g.api(slackPostEphemeral, map[string]any{slackKeyChannel: channel, slackKeyThreadTS: thread, slackKeyUser: user, slackKeyText: "Sign in",
			slackKeyBlocks: []any{map[string]any{fieldTypeKey: slackBlockActions, slackKeyElements: []any{map[string]any{fieldTypeKey: slackButton, slackKeyActionID: slackActionSignIn, slackKeyURL: "http://gw" + musterlink.LinkPath + "?u=x"}}}}})
		return
	case text == slackAgentsWord:
		g.post(channel, thread, "*Available agents*:\n• *"+klausGatewayTestDisplay+"* — Throwaway",
			map[string]any{fieldTypeKey: slackBlockHeader, slackKeyText: map[string]any{fieldTypeKey: slackPlainText, slackKeyText: slackRosterTitle}},
			map[string]any{fieldTypeKey: slackBlockSection, slackKeyText: map[string]any{fieldTypeKey: slackMrkdwn, slackKeyText: "*" + klausGatewayTestDisplay + "*"},
				"accessory": map[string]any{fieldTypeKey: slackButton, slackKeyActionID: slackActionAgentSelect, slackKeyValue: testFixtureRef}})
		return
	case text == slackStopWord:
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
	case !known:
		// klaus-gateway 4.13.0 and later: a conversation opens only through
		// the picker; a message outside one gets the help.
		g.post(channel, thread, "No conversation is open here, so your message did not reach an agent. Here is how to start one.")
		return
	}
	g.turn(channel, thread, user, text)
}

// turn runs one turn of the thread's conversation.
func (g *scriptedGateway) turn(channel, thread, user, text string) {
	g.record(map[string]any{recordKey: recordDispatch, "thread_id": thread, "agent": testFixtureRef, "agent_source": "thread",
		"slack_user": user, "subject": testUser, claimSubject: testSubject})
	switch text {
	case klausGatewayToolPrompt:
		g.card(channel, thread, "task-"+thread)
	case klausGatewayStopPrompt:
		g.mu.Lock()
		declined := g.declines > 0
		if declined {
			g.declines--
		}
		g.mu.Unlock()
		if declined {
			g.answer(channel, thread, "No.")
			g.done(thread, outcomeCompleted, "declined-"+thread)
			return
		}
		stop := make(chan struct{})
		g.mu.Lock()
		g.running[thread] = stop
		g.mu.Unlock()
		g.ctrl.setTaskState(a2a.TaskID("essay-"+thread), a2a.TaskStateWorking)
		g.api(slackStartStream, map[string]any{slackKeyChannel: channel, slackKeyThreadTS: thread, slackKeyChunks: []any{map[string]any{fieldTypeKey: slackMarkdownChunk, slackKeyText: "One\nTwo\nThree\nFour\nFive\n"}}})
		<-stop
		g.ctrl.setTaskState(a2a.TaskID("essay-"+thread), a2a.TaskStateCanceled)
		g.post(channel, thread, slackStopped)
		g.done(thread, outcomeCanceled, "essay-"+thread)
	default:
		g.answer(channel, thread, klausGatewayWord)
		g.done(thread, outcomeCompleted, "word-"+thread)
	}
}

// testFixtureRef is the fixture's agent ref as the adapter's roster and
// picker carry it.
const testFixtureRef = kagentNamespace + "/" + klausGatewayTestAgent

// openPicker answers a roster row's Select button: the picker for the
// roster's thread, offering the fixture alone.
func (g *scriptedGateway) openPicker(channel, thread, ref string) {
	g.api(slackViewsOpen, map[string]any{slackKeyTriggerID: "t", slackKeyView: map[string]any{
		slackKeyCallbackID: slackPickerCallback, "private_metadata": `{"c":"` + channel + `","t":"` + thread + `"}`,
		slackKeyBlocks: []any{map[string]any{slackKeyBlockID: slackPickerAgentBlock, slackKeyElement: map[string]any{
			"initial_option": map[string]any{slackKeyValue: ref},
			slackKeyOptions:  []any{map[string]any{slackKeyValue: testFixtureRef}},
		}}},
	}})
}

// onShortcut answers the Ask an agent here shortcut: the picker for the
// message's thread, or for a person with no link the sign-in notice through
// the response_url.
func (g *scriptedGateway) onShortcut(user, channel, thread, responseURL string) {
	if user == g.stranger {
		raw, _ := json.Marshal(map[string]any{slackKeyText: "The agents are listed with your permissions, so " + slackSignInFirst + ": mention the bot with `login`, then run the command again.", "response_type": "ephemeral"})
		req, err := http.NewRequest(http.MethodPost, responseURL, bytes.NewReader(raw))
		if err != nil {
			g.t.Errorf("response_url: %v", err)
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			g.t.Errorf("response_url: %v", err)
			return
		}
		_ = resp.Body.Close()
		return
	}
	g.openPicker(channel, thread, "")
}

// onPick answers a submitted picker: the fixture binds the thread to a new
// instance, echoes the question and runs it (behind the workspace prompt when
// the person has workspaces); an agent no Harness admits gets the refusal,
// shown to the submitter alone in the thread.
func (g *scriptedGateway) onPick(user, metadata, ref, question string) {
	var pm struct{ C, T string }
	_ = json.Unmarshal([]byte(metadata), &pm)
	if ref != testFixtureRef {
		g.api(slackPostEphemeral, map[string]any{slackKeyChannel: pm.C, slackKeyThreadTS: pm.T, slackKeyUser: user,
			slackKeyText: "*" + klausGatewayTestUnadmittedDisplay + "* is installed but " + slackNotRunnable + ": Agent x " + unadmittedReason + ": blocked by ResolvedRefs. " + slackNotStarted + ".\n\n*Available agents*:\n• *" + klausGatewayTestDisplay + "*"})
		return
	}
	instance := "inst-" + pm.T
	g.mu.Lock()
	g.bound[pm.T] = instance
	g.mu.Unlock()
	g.ctrl.mu.Lock()
	g.ctrl.sessions[instance] = fakeSession(instance, testToken, klausGatewayTestAgent)
	g.ctrl.mu.Unlock()
	g.record(map[string]any{recordKey: recordBound, "thread": pm.T, fieldSession: instance})
	g.post(pm.C, pm.T, question)
	if g.workspaces {
		g.mu.Lock()
		g.held[pm.T] = map[string]string{slackKeyChannel: pm.C, slackKeyUser: user, slackKeyText: question}
		g.mu.Unlock()
		g.post(pm.C, pm.T, slackWorkspaceAsk,
			map[string]any{fieldTypeKey: slackBlockActions, slackKeyElements: []any{
				map[string]any{fieldTypeKey: slackStaticSelect, slackKeyActionID: slackWorkspaceSelect},
				map[string]any{fieldTypeKey: slackButton, slackKeyActionID: slackWorkspaceNone, slackKeyValue: pm.T},
			}})
		return
	}
	g.turn(pm.C, pm.T, user, question)
}

func (g *scriptedGateway) onClick(user, channel, thread, cardTS, action, value string) {
	if action == slackWorkspaceNone {
		g.mu.Lock()
		ev := g.held[thread]
		g.held[thread] = nil
		g.mu.Unlock()
		if ev == nil {
			g.t.Errorf("No workspace clicked in thread %s, which holds no message", thread)
			return
		}
		g.api(slackUpdate, map[string]any{slackKeyChannel: channel, slackKeyTS: cardTS, slackKeyText: slackWorkspaceNoneIn})
		g.turn(ev[slackKeyChannel], thread, ev[slackKeyUser], ev[slackKeyText])
		return
	}
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
// approvals on the same task, a denial, `stop`, a following turn — and the
// ways they fail: a turn that never pauses, a turn that ends before `stop`.
func TestSlackProof(t *testing.T) {
	ctrl := newFakeKagent()
	api := ctrl.serve(t, testToken)
	fake := startTestFake(t, map[string]string{"UP": testUser})
	gw := &scriptedGateway{t: t, secret: "sekrit", fake: fake, ctrl: ctrl, stranger: "US", bound: map[string]string{}, running: map[string]chan struct{}{}}
	srv := httptest.NewServer(gw)
	// Registered after the fake's, so it runs first: the gateway stops
	// taking requests and its last answers land before the fake shuts down.
	// The proof's final steps return before the gateway has answered their
	// reply (`stop` in a thread whose turn is over).
	t.Cleanup(func() {
		srv.Close()
		gw.settle()
	})
	p := &slackProof{driver: newSlackDriver(srv.URL, "sekrit", fake, "C1"), fake: fake, channel: "C1", logs: gw.logs}

	roster, err := p.roster("UP")
	if names := rosterNames(roster); err != nil || assertSlackRoster(names) != nil {
		t.Fatalf("roster = %q %v", names, err)
	}
	refusal, err := p.refusal("UP", roster, "hi")
	if err != nil || strings.Contains(refusal, "\n") || !strings.Contains(refusal, unadmittedReason) {
		t.Fatalf("refusal = %q %v", refusal, err)
	}
	if notice, link, err := p.signInPrompt("US"); err != nil || !strings.Contains(link, musterlink.LinkPath) || !strings.Contains(notice, slackSignInFirst) {
		t.Fatalf("signInPrompt = %q %q %v", notice, link, err)
	}
	if err := assertNoSessions(api, klausGatewayTestAgent); err != nil {
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
	session, err := p.boundSession(api, main)
	if err != nil || session.GetId() != "inst-"+main.ts {
		t.Fatalf("boundSession = %v %v", session, err)
	}
	if err := assertOnlySessions(api, session.GetId()); err != nil {
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
	approved, err := p.decideUntilSettled(api, session, main, turn, true)
	if err != nil {
		t.Fatal(err)
	}
	if approved.rounds != 2 || approved.finalState != a2a.TaskStateCompleted || approved.taskID != "task-"+main.ts || !strings.Contains(approved.answer, "12 namespaces") {
		t.Errorf("approved = %+v", approved)
	}
	if err := assertNothingWaiting(api, session); err != nil {
		t.Error(err)
	}

	denied := &slackThread{user: "UP"}
	turn, err = p.firstTurn(denied, klausGatewayToolPrompt)
	if err != nil {
		t.Fatal(err)
	}
	deniedSession, err := p.boundSession(api, denied)
	if err != nil {
		t.Fatal(err)
	}
	declined, err := p.decideUntilSettled(api, deniedSession, denied, turn, false)
	if err != nil {
		t.Fatal(err)
	}
	if declined.rounds != 1 || !declined.finalState.Terminal() || declined.outcome != "failed" {
		t.Errorf("declined = %+v", declined)
	}
	if err := assertOnlySessions(api, session.GetId(), deniedSession.GetId()); err != nil {
		t.Error(err)
	}

	// The model declines the count once: the proof asks once more and
	// stops the count that follows.
	gw.declines = 1
	before, _ := api.taskIDs(context.Background(), session)
	stopped, err := p.stop(main, klausGatewayStopPrompt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stopped.answer, "Five") {
		t.Errorf("stopped answer = %q", stopped.answer)
	}
	if canceled, err := api.waitCanceledTask(session, before, 3*time.Second); err != nil || canceled != "essay-"+main.ts {
		t.Errorf("waitCanceledTask = %q %v", canceled, err)
	}
	turn, err = p.say(main, klausGatewayRecallPrompt)
	if err != nil || assertSlackTurnSaid(turn, klausGatewayWord) != nil {
		t.Errorf("the turn after stop: %+v %v", turn, err)
	}

	// A turn that does not pause cannot be decided; a model that declines
	// the count twice fails as the model's doing, never as the gateway's.
	if _, err := p.decideUntilSettled(api, session, main, turn, true); err == nil || !strings.Contains(err.Error(), "did not pause") {
		t.Errorf("deciding a completed turn: %v", err)
	}
	gw.declines = 2
	if _, err := p.stop(main, klausGatewayStopPrompt); err == nil || !strings.Contains(err.Error(), "the model's doing") {
		t.Errorf("stopping a count the model declines twice: %v", err)
	}
}

func TestCountReached(t *testing.T) {
	for answer, want := range map[string]bool{
		"No.":                              false,
		"":                                 false,
		"one\ntwo\nthree\nfour\nfive":      true,
		"One\nTwo\nThree\nFour\nFive\nSix": true,
		"1. one\n2. two\n3. three\n4. four\n5. five": true,
		"- one\n- two\n- three\n- four\n- **five**":  true,
		"I won't write out five hundred numbers.":    false,
		"one\ntwo\nthree\nfour":                      false,
	} {
		if got := countReached(answer); got != want {
			t.Errorf("countReached(%q) = %v, want %v", answer, got, want)
		}
	}
}

// TestOpenPastWorkspace: a conversation opened through the shortcut on a
// gateway that lists the person's workspaces in the thread — the question
// held behind the workspace prompt, which No workspace answers, then the
// turn — and on one that asks nothing.
func TestOpenPastWorkspace(t *testing.T) {
	for name, workspaces := range map[string]bool{"prompted": true, "not asked": false} {
		t.Run(name, func(t *testing.T) {
			ctrl := newFakeKagent()
			ctrl.serve(t, testToken)
			fake := startTestFake(t, map[string]string{"UP": testUser})
			gw := &scriptedGateway{t: t, secret: "sekrit", fake: fake, ctrl: ctrl, bound: map[string]string{}, running: map[string]chan struct{}{},
				workspaces: workspaces, held: map[string]map[string]string{}}
			srv := httptest.NewServer(gw)
			t.Cleanup(func() {
				srv.Close()
				gw.settle()
			})
			p := &slackProof{driver: newSlackDriver(srv.URL, "sekrit", fake, "C1"), fake: fake, channel: "C1", logs: gw.logs}
			thread := &slackThread{user: "UP"}
			turn, err := p.firstTurn(thread, klausGatewayWordPrompt)
			if err != nil {
				t.Fatal(err)
			}
			if err := assertSlackTurnSaid(turn, klausGatewayWord); err != nil {
				t.Error(err)
			}
			if _, err := p.dispatch(thread, testSubject, testUser); err != nil {
				t.Error(err)
			}
			answered := slices.ContainsFunc(fake.thread("C1", thread.ts), func(m slackMessage) bool { return m.Text == slackWorkspaceNoneIn })
			if answered != workspaces {
				t.Errorf("the workspace prompt answered with No workspace: %v, want %v", answered, workspaces)
			}
		})
	}
}

// A relative --run-dir is made absolute: docker run -v takes a bare name
// for a named volume, which the gateway's container cannot write.
func TestKlausGatewayRunDirIsAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	dir, cleanup, err := klausGatewayRunDir("run")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !filepath.IsAbs(dir) {
		t.Fatalf("run dir %q is not absolute", dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("run dir not created: %v", err)
	}
}

func TestGatewayKeepsHeld(t *testing.T) {
	for described, want := range map[string]bool{
		"klaus-gateway 3.12.0 (abc123)":      false,
		"klaus-gateway 4.1.0-rc.14 (abc123)": false,
		"klaus-gateway 4.1.0-rc.15 (abc123)": true,
		"klaus-gateway 4.1.0 (abc123)":       true,
		"klaus-gateway (devel) (abc123)":     true,
		"klaus-gateway (version not logged)": true,
	} {
		if got := gatewayKeepsHeld(described); got != want {
			t.Errorf("gatewayKeepsHeld(%q) = %v, want %v", described, got, want)
		}
	}
}

// TestSlackDriverPickerActions: a pick on a message, a pick inside an open
// modal (the view as Slack holds it, with an id and a hash) and the slash
// command reach the gateway's endpoints as Slack sends them; the fake keeps
// a views.update as the modal's latest state.
func TestSlackDriverPickerActions(t *testing.T) {
	var paths []string
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		paths, bodies = append(paths, r.URL.Path), append(bodies, string(body))
	}))
	defer srv.Close()
	f := startTestFake(t, nil)
	d := newSlackDriver(srv.URL, "sekrit", f, "C1")
	msg := slackMessage{TS: "7.1", Channel: "C1", ThreadTS: "9.0"}
	if err := d.pick("UP", msg, slackWorkspaceSelect, `{"t":"9.0","w":"ws"}`); err != nil {
		t.Fatal(err)
	}
	view := map[string]any{slackKeyCallbackID: slackPickerCallback, slackKeyBlocks: []any{}}
	if err := d.pickInView("UP", view, slackPickerAgentBlock, slackPickerAgentAction, "kagent/a"); err != nil {
		t.Fatal(err)
	}
	if err := d.slashCommand("UP", "ask"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{slackInteractionsPath, slackInteractionsPath, slackCommandsPath}) {
		t.Fatalf("paths = %v", paths)
	}
	var action struct {
		Type    string `json:"type"`
		Actions []struct {
			Type           string `json:"type"`
			ActionID       string `json:"action_id"`
			SelectedOption struct {
				Value string `json:"value"`
			} `json:"selected_option"`
		} `json:"actions"`
		View map[string]any `json:"view"`
	}
	for i, want := range []string{`{"t":"9.0","w":"ws"}`, "kagent/a"} {
		form, _ := url.ParseQuery(bodies[i])
		if err := json.Unmarshal([]byte(form.Get("payload")), &action); err != nil {
			t.Fatal(err)
		}
		if action.Type != "block_actions" || len(action.Actions) != 1 || action.Actions[0].Type != slackStaticSelect || action.Actions[0].SelectedOption.Value != want {
			t.Errorf("pick %d = %+v", i, action)
		}
	}
	if action.View[slackKeyCallbackID] != slackPickerCallback || action.View["id"] == "" || action.View["hash"] == "" {
		t.Errorf("the view of the pick in the modal = %v", action.View)
	}
	if _, held := view["hash"]; held {
		t.Error("pickInView changed the caller's view")
	}
	form, _ := url.ParseQuery(bodies[2])
	if form.Get("command") != slackCommand || form.Get("user_id") != "UP" || form.Get("channel_id") != "C1" || form.Get("text") != "ask" || form.Get("trigger_id") == "" || form.Get("response_url") == "" {
		t.Errorf("slash command = %v", form)
	}

	callFake(t, f, slackViewsOpen, nil, map[string]any{slackKeyView: map[string]any{slackKeyCallbackID: "first"}})
	callFake(t, f, slackViewsUpdate, nil, map[string]any{"view_id": "V1", "hash": "h", slackKeyView: map[string]any{slackKeyCallbackID: "redrawn"}})
	if views := f.openedViews(); len(views) != 2 || views[1][slackKeyCallbackID] != "redrawn" {
		t.Errorf("openedViews = %v", views)
	}
}

// TestSlackDriverShortcut: the Ask an agent here shortcut reaches the
// interactions endpoint as Slack's message_action on a lone message (no
// thread_ts) and on a reply; what the gateway posts to its response_url is
// read back, as text, from the fake in process and over HTTP.
func TestSlackDriverShortcut(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
	}))
	defer srv.Close()
	f := startTestFake(t, nil)
	d := newSlackDriver(srv.URL, "sekrit", f, "C1")
	if err := d.shortcut("UP", slackShortcutAskAgent, "5.0", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.shortcut("UP", slackShortcutAskAgent, "5.2", "5.0"); err != nil {
		t.Fatal(err)
	}
	for i, wantThread := range []string{"", "5.0"} {
		form, _ := url.ParseQuery(bodies[i])
		var p struct {
			Type          string
			CallbackID    string `json:"callback_id"`
			TriggerID     string `json:"trigger_id"`
			ResponseURL   string `json:"response_url"`
			User, Channel struct{ ID string }
			Message       map[string]string
		}
		if err := json.Unmarshal([]byte(form.Get("payload")), &p); err != nil {
			t.Fatal(err)
		}
		if p.Type != "message_action" || p.CallbackID != slackShortcutAskAgent || p.User.ID != "UP" || p.Channel.ID != "C1" ||
			p.Message[slackKeyThreadTS] != wantThread || p.Message[slackKeyTS] == "" || p.TriggerID == "" || p.ResponseURL != d.responseURL() {
			t.Errorf("shortcut %d = %+v", i, p)
		}
	}

	resp, err := http.Post(d.responseURL(), "application/json", strings.NewReader(`{"text":"sign in first"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := f.responses(); len(got) != 1 || responseText(got[0]) != "sign in first" {
		t.Errorf("responses = %q", got)
	}
	resp, err = http.Get(strings.TrimSuffix(f.baseURL(), slackAPIPath) + slackResponsesPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var served []string
	if err := json.NewDecoder(resp.Body).Decode(&served); err != nil || len(served) != 1 {
		t.Errorf("served responses = %q %v", served, err)
	}
	if responseText("no JSON") != "no JSON" {
		t.Error("a body that is no JSON is not kept as it came")
	}
}

// TestPickerValues: a submission selects the agent and types the question;
// a picker with the optional Workspace input submits it empty (No
// workspace) under its revisioned block id.
func TestPickerValues(t *testing.T) {
	plain := pickerValues(map[string]any{slackKeyBlocks: []any{map[string]any{slackKeyBlockID: slackPickerAgentBlock}}}, "kagent/a", "hi")
	if len(plain) != 2 {
		t.Errorf("values without a Workspace input = %v", plain)
	}
	agent, _ := plain[slackPickerAgentBlock].(map[string]any)[slackPickerAgentAction].(map[string]any)
	if agent[slackSelectedOption].(map[string]any)[slackKeyValue] != "kagent/a" {
		t.Errorf("agent = %v", agent)
	}
	question, _ := plain[slackPickerQuestionBlock].(map[string]any)[slackPickerQuestionAction].(map[string]any)
	if question[slackKeyValue] != "hi" {
		t.Errorf("question = %v", question)
	}
	withWS := pickerValues(map[string]any{slackKeyBlocks: []any{map[string]any{slackKeyBlockID: slackPickerWorkspaceBlock + ".2"}}}, "kagent/a", "hi")
	ws, ok := withWS[slackPickerWorkspaceBlock+".2"].(map[string]any)[slackPickerWorkspaceAction].(map[string]any)
	if !ok || ws[slackSelectedOption] != nil {
		t.Errorf("the Workspace input = %v, want it present and empty", withWS)
	}
	if ref, ok := pickerRef(map[string]any{slackKeyBlocks: []any{map[string]any{slackKeyBlockID: slackPickerAgentBlock, slackKeyElement: map[string]any{
		slackKeyOptions: []any{map[string]any{slackKeyValue: "kagent/other-" + klausGatewayTestAgent}, map[string]any{slackKeyValue: "kagent/" + klausGatewayTestAgent}},
	}}}}, klausGatewayTestAgent); !ok || ref != "kagent/"+klausGatewayTestAgent {
		t.Errorf("pickerRef = %q %v", ref, ok)
	}
}
