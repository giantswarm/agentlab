package lab

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"google.golang.org/protobuf/proto"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/kagentpb"
)

// The Swarmgeist proof: klaus-gateway on kagent API v2, headless.
//
// klaus-gateway (Swarmgeist, the Slack bridge of the fleet) runs its
// conversations on the kagent controller: A2A v1 over gRPC through the
// agentgateway edge, the roster from ListAgentTemplates, one AgentInstance
// per channel thread kept in the gateway's routing store, human-in-the-loop
// as kagent's HITL extension, a stop as CancelTask — every call made as the
// person behind the turn, whose Dex id_token the gateway forwards and
// validates nowhere itself. Slack cannot be driven headlessly, but the
// gateway's web channel (`/web/*`) is the same facade one adapter down, so
// this proof runs the gateway against the lab — out of cluster, on the host,
// as the released image or a local build, its a2a target the lab's public
// gRPC hostname with JWT validation at the edge — and drives the web channel
// with a lab user's id_token: discovery lists the proof's AgentTemplate (and
// hides one no Harness admits), a streamed turn is attributed to the person
// at muster, a tool call bound with requireApproval pauses the task at
// input-required and the decision resumes it, a client that closes its
// stream has the task cancelled at the controller and the thread goes on,
// and a gateway restart on the same bolt store continues the same
// AgentInstance. docs/platform.md "The Swarmgeist proof".

// KlausGatewayImageDefault is the released gateway the proof runs when no
// image or binary is named: the klaus-gateway the 4.x meta chart's
// `components.klaus-gateway` range (1.x) resolves at the time of writing.
const KlausGatewayImageDefault = "gsoci.azurecr.io/giantswarm/klaus-gateway:1.0.2"

// Names of what the proof creates in the kagent namespace; all are deleted by
// the same run, and a leftover of an aborted run is removed first.
const (
	klausGatewayTestAgent = "agentlab-klaus-gateway-test"
	// klausGatewayTestUnadmitted is the template no Harness admits: it lacks
	// the admission label, so discovery must hide it and a turn naming it
	// must be refused with the reason.
	klausGatewayTestUnadmitted = klausGatewayTestAgent + "-unadmitted"
	klausGatewayTestDisplay    = "agentlab Swarmgeist proof"
	klausGatewayTestIcon       = "https://icons.agentlab.invalid/swarmgeist.png"
	// klausGatewayTestToolset is the toolset the fixture's muster carrier
	// declares (X-Muster-Toolset), the fleet's read-only preset.
	klausGatewayTestToolset = "preset:read-only"
	klausGatewayTestPrompt  = "You are a terse assistant of the agentlab Swarmgeist proof. Answer in one short line. " +
		"When the user asks you to reply with a word, reply with exactly that word and nothing else. " +
		"When the user asks about namespaces of the cluster, use your tools to list them once and answer with their count."
	// klausGatewayMusterURL is muster's in-cluster MCP endpoint, the
	// RemoteMCPServer's url exactly as the Generic chart 1.x renders it.
	klausGatewayMusterURL = "http://muster.agent-platform.svc.cluster.local:8090/mcp"
)

// The web channel: paths, the SSE event names, and the gateway's records.
const (
	webAgentsPath   = "/web/agents"
	webMessagesPath = "/web/messages"
	webHealthPath   = "/web/healthz"
	sseEventDone    = "done"
	sseEventPrompt  = "prompt"
	sseEventError   = "error"
	// instanceBoundRecord is the gateway's log record for a thread bound to
	// a freshly created AgentInstance; a restart on the store writes none.
	instanceBoundRecord = `"record":"instance_bound"`
	// gatewayCAPath is where the image shape mounts the lab CA (the chart's
	// own mount path for a2a.caSecret).
	gatewayCAPath   = "/etc/klaus-gateway/a2a/ca.crt"
	gatewayDataPath = "/var/lib/klaus-gateway"
	gatewayBoltFile = "routes.bolt"
	gatewayLogFile  = "klaus-gateway.log"
	webChannelID    = "agentlab"
	// gatewayContainer names the proof's container in the image shape.
	gatewayContainer = "agentlab-klaus-gateway-test"
)

// The turns and their words: the first turn's word, recalled after the
// restart; the tool-using question that pauses on approval; the long answer
// the stop interrupts.
const (
	klausGatewayWord          = "pong"
	klausGatewayWordPrompt    = "Reply with exactly the word " + klausGatewayWord + "."
	klausGatewayRecallPrompt  = "Which single word did I ask you to reply with earlier in this conversation? Answer with just that word."
	klausGatewayToolPrompt    = "How many namespaces does the cluster have? Use your tools to list them."
	klausGatewayEssayPrompt   = "Write a long essay of at least 1500 words about the history of container orchestration, without using any tools."
	klausGatewayTurnTimeout   = 4 * time.Minute
	klausGatewayStopAfter     = 6 * time.Second
	klausGatewayCancelWait    = 90 * time.Second
	klausGatewayStartWait     = 30 * time.Second
	klausGatewayStopWait      = 20 * time.Second
	klausGatewayHITLRounds    = 6
	klausGatewayLogSince      = 30 * time.Minute
	klausGatewayReadyTimeout  = 10 * time.Minute
	klausGatewayMusterAttempt = 15
)

// Muster's audit and protocol records the attribution reads.
const (
	musterTokenAccepted = "forwarded_id_token_accepted"
	musterToolCall      = `"msg":"tools/call request"`
)

// KlausGatewayTestOptions tunes the proof.
type KlausGatewayTestOptions struct {
	// GatewayImage is the klaus-gateway image run on the host's network
	// (default KlausGatewayImageDefault); GatewayBinary, a local build, takes
	// precedence — the proof of a branch.
	GatewayImage  string
	GatewayBinary string
	// Port is the host port the web channel listens on; the admin endpoints
	// take Port+1. Default 18090.
	Port int
	// ModelConfig is the kagent ModelConfig the fixture runs on (default
	// default-model-config, the Anthropic one the lab renders).
	ModelConfig string
	// ReadyTimeout bounds the fixture's golden boot (default 10 min).
	ReadyTimeout time.Duration
	// RunDir holds the bolt store and the gateway's log; empty picks a
	// temporary directory removed at the end.
	RunDir string
}

// KlausGatewayTest is the headless Swarmgeist proof: klaus-gateway against
// the lab's kagent API v2 controller through the edge, driven on its web
// channel as the signed-in user.
func KlausGatewayTest(cfg *config.Config, email string, opts KlausGatewayTestOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled || !cfg.Platform.Agents {
		return fmt.Errorf("platform.agents is off in %s — enable it and run `agentlab platform` first", config.File)
	}
	if kagentLegacy() {
		return fmt.Errorf("the Swarmgeist proof needs kagent API v2 (klaus-gateway 1.x speaks A2A v1 over gRPC to it); the released 0.x kagent serves the REST surface the 0.x gateway used")
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}
	opts = opts.withDefaults()
	if err := portsFree(opts.Port, opts.Port+1); err != nil {
		return err
	}
	runDir, cleanRunDir, err := klausGatewayRunDir(opts.RunDir)
	if err != nil {
		return err
	}
	defer cleanRunDir()

	step("Logging in to Dex as %s", user.Email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	claims, err := decodeJWTClaims(token)
	if err != nil {
		return fmt.Errorf("the id_token of %s: %w", user.Email, err)
	}
	subject, _ := claims["sub"].(string)
	api, err := newKagentAPI(cfg, user.Email, token)
	if err != nil {
		return err
	}

	// Leftovers of an aborted run first, and everything this run creates on
	// every exit path.
	klausGatewayCleanup(api)
	defer klausGatewayCleanup(api)

	shape := harnessAdmissionLabels()
	step("Applying the fixtures in namespace %s: AgentTemplate %s (labelled %v, muster carrier %s with %s, requireApproval on the binding) and AgentTemplate %s (no admission label)",
		kagentNamespace, klausGatewayTestAgent, shape, klausGatewayTestAgent, klausGatewayTestToolset, klausGatewayTestUnadmitted)
	if _, err := applyManifests(context.Background(), []byte(klausGatewayFixtures(opts.ModelConfig, shape))); err != nil {
		return err
	}
	step("Waiting up to %s for %s Ready on Harness %s (the golden boot)", opts.ReadyTimeout, klausGatewayTestAgent, kagentHarness)
	boot, err := waitGoldenBoot(klausGatewayTestAgent, kagentHarness, opts.ReadyTimeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return fmt.Errorf("AgentTemplate %s never became Ready on Harness %s within %s: %s", klausGatewayTestAgent, kagentHarness, opts.ReadyTimeout, harnessReadySummary(boot.template, kagentHarness))
	}
	note("Ready after %s", boot.elapsed.Round(time.Second))

	target := grpcsTarget(cfg.AgentgatewayBaseURL())
	caFile, err := filepath.Abs(caCertPath)
	if err != nil {
		return err
	}
	gw := newGatewayProcess(opts, runDir, caFile, target)
	defer func() { _ = gw.stop() }()
	step("Starting klaus-gateway %s on the host: web channel at %s, a2a target %s (TLS with the lab CA, the person's token forwarded), bolt store %s", gw.describe(), gw.baseURL(), target, filepath.Join(runDir, gatewayBoltFile))
	if err := gw.start(); err != nil {
		return err
	}
	note("healthy: %s", gw.version())
	web := &webClient{base: gw.baseURL(), token: token, user: user.Email, thread: "thread-" + randomSuffix()}

	step("1. Discovery: GET %s as %s lists %s with its display name and icon, hides %s, and a turn naming the hidden one is refused", webAgentsPath, user.Email, klausGatewayTestAgent, klausGatewayTestUnadmitted)
	agents, err := web.agents(context.Background())
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	if err := assertRoster(agents); err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	refusal, err := web.send(context.Background(), webMessage{Text: klausGatewayWordPrompt, AgentRef: klausGatewayTestUnadmitted}, klausGatewayTurnTimeout)
	if err != nil {
		return fmt.Errorf("a turn on %s: %w", klausGatewayTestUnadmitted, err)
	}
	if err := assertUnadmittedRefused(refusal); err != nil {
		return err
	}
	note("roster: %s; %s refused: HTTP %d %s", rosterLine(agents), klausGatewayTestUnadmitted, refusal.Status, excerpt(refusal.Body, 160))

	step("2. One streamed turn as %s: the thread's first turn creates the AgentInstance", user.Email)
	turn, err := web.firstTurn(klausGatewayWordPrompt)
	if err != nil {
		return err
	}
	if err := assertTurnSaid(turn, klausGatewayWord); err != nil {
		return err
	}
	instanceID, err := gw.boundInstance()
	if err != nil {
		return err
	}
	instances, err := api.listInstances(context.Background(), klausGatewayTestAgent)
	if err != nil {
		return err
	}
	if len(instances) != 1 || instances[0].GetId() != instanceID {
		return fmt.Errorf("the controller lists %d AgentInstance(s) of %s (%s), wanted exactly the bound one %s", len(instances), klausGatewayTestAgent, instanceIDs(instances), instanceID)
	}
	note("answered %q; thread bound to AgentInstance %s (creator %s), the only instance of the template", excerpt(turn.Text, 60), instanceID, instances[0].GetCreator())

	step("3. Human in the loop: a tool call pauses the task at input-required; the decision on the web channel resumes it in place")
	toolTurnStart := time.Now()
	turn, err = web.send(context.Background(), webMessage{Text: klausGatewayToolPrompt}, klausGatewayTurnTimeout)
	if err != nil {
		return fmt.Errorf("the tool-using turn: %w", err)
	}
	hitl, err := web.approveUntilDone(api, instanceID, turn)
	if err != nil {
		return err
	}
	note("%d approval(s) on task %s (%s); GetTask=%s; no task of the instance left at input-required; answered %q",
		hitl.rounds, hitl.taskID, strings.Join(hitl.tools, ", "), hitl.finalState, excerpt(hitl.text, 80))

	step("The turn is attributed to %s at muster (the forwarded id_token accepted, the tool calls under that subject)", user.Email)
	if err := assertMusterAttribution(user.Email, subject, toolTurnStart); err != nil {
		return err
	}
	note("muster's audit log: %s email=%s; tools/call requests under subject %.8s…", musterTokenAccepted, user.Email, subject)

	step("4. Stop: the client closes its stream after %s; the gateway cancels the task at the controller and the thread takes a following turn", klausGatewayStopAfter)
	tasksBefore, err := api.taskIDs(context.Background(), instanceID)
	if err != nil {
		return err
	}
	cut, err := web.sendAndCut(klausGatewayEssayPrompt, klausGatewayStopAfter)
	if err != nil {
		return err
	}
	canceled, err := api.waitCanceledTask(instanceID, tasksBefore, klausGatewayCancelWait)
	if err != nil {
		return err
	}
	turn, err = web.send(context.Background(), webMessage{Text: klausGatewayWordPrompt}, klausGatewayTurnTimeout)
	if err != nil {
		return fmt.Errorf("the turn after the stop: %w", err)
	}
	if err := assertTurnSaid(turn, klausGatewayWord); err != nil {
		return fmt.Errorf("the turn after the stop: %w", err)
	}
	note("stream cut after %d bytes; task %s TASK_STATE_CANCELED at the controller; the following turn answered %q", len(cut.Text), canceled, excerpt(turn.Text, 40))

	step("5. Restart on the bolt store: the thread → AgentInstance mapping survives, the next turn continues %s", instanceID)
	boundBefore := gw.boundCount()
	if err := gw.stop(); err != nil {
		return err
	}
	if err := gw.start(); err != nil {
		return fmt.Errorf("restarting the gateway: %w", err)
	}
	turn, err = web.send(context.Background(), webMessage{Text: klausGatewayRecallPrompt}, klausGatewayTurnTimeout)
	if err != nil {
		return fmt.Errorf("the turn after the restart: %w", err)
	}
	if err := assertTurnSaid(turn, klausGatewayWord); err != nil {
		return fmt.Errorf("the turn after the restart did not continue the conversation: %w", err)
	}
	if after := gw.boundCount(); after != boundBefore {
		return fmt.Errorf("the restarted gateway bound the thread anew (%d instance_bound records before, %d after): the bolt store did not carry the mapping", boundBefore, after)
	}
	instances, err = api.listInstances(context.Background(), klausGatewayTestAgent)
	if err != nil {
		return err
	}
	if len(instances) != 1 || instances[0].GetId() != instanceID {
		return fmt.Errorf("after the restart the controller lists %d AgentInstance(s) of %s (%s), wanted only %s", len(instances), klausGatewayTestAgent, instanceIDs(instances), instanceID)
	}
	note("no new binding, still the one AgentInstance %s; the agent recalled %q", instanceID, excerpt(turn.Text, 40))

	step("Deleting the fixtures, the AgentInstance and the gateway")
	if err := gw.stop(); err != nil {
		return err
	}
	klausGatewayCleanup(api)
	if left := klausGatewayLeftovers(api); len(left) > 0 {
		return fmt.Errorf("left behind: %s", strings.Join(left, "; "))
	}
	runDirFate := "removed"
	if opts.RunDir != "" {
		runDirFate = "kept at " + runDir
	}
	note("nothing left in the kagent namespace; gateway stopped, run directory %s", runDirFate)

	fmt.Println()
	fmt.Printf("PASS: klaus-gateway %s ran on the host against %s (TLS with the lab CA, JWT validated at the edge) and listed AgentTemplate %s with its display name and icon on GET %s as %s; %s (no admission label) was hidden and refused with the reason\n",
		gw.describe(), target, klausGatewayTestAgent, webAgentsPath, user.Email, klausGatewayTestUnadmitted)
	fmt.Printf("PASS: one streamed turn on the web channel bound the thread to AgentInstance %s, the only instance of the template; muster accepted %s's forwarded id_token and ran the agent's tool calls under that subject\n", instanceID, user.Email)
	fmt.Printf("PASS: the requireApproval binding paused task %s at input-required; %d approval decision(s) carrying the task id resumed it to %s with nothing left waiting\n", hitl.taskID, hitl.rounds, hitl.finalState)
	fmt.Printf("PASS: closing the stream mid-turn had the gateway cancel task %s at the controller (TASK_STATE_CANCELED); the thread took a following turn\n", canceled)
	fmt.Printf("PASS: a gateway restart on the bolt store kept the thread → AgentInstance mapping: the next turn continued %s and recalled the earlier word\n", instanceID)
	fmt.Printf("PASS: nothing left behind — the AgentTemplates, the RemoteMCPServer, the AgentInstance, the gateway process and its store are gone\n")
	return nil
}

// withDefaults fills the zero options.
func (o KlausGatewayTestOptions) withDefaults() KlausGatewayTestOptions {
	if o.GatewayImage == "" {
		o.GatewayImage = KlausGatewayImageDefault
	}
	if o.Port == 0 {
		o.Port = 18090
	}
	if o.ModelConfig == "" {
		o.ModelConfig = defaultModelConfig
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = klausGatewayReadyTimeout
	}
	return o
}

// portsFree refuses to start on a port something else already listens on.
func portsFree(ports ...int) error {
	for _, port := range ports {
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return fmt.Errorf("host port %d is taken (pick another with --gateway-port): %w", port, err)
		}
		_ = l.Close()
	}
	return nil
}

// klausGatewayRunDir is where the bolt store and the gateway's log live for
// one run: the caller's directory (kept), or a temporary one (removed). The
// image shape's container writes the store as the caller's uid, so the
// directory needs no wider mode.
func klausGatewayRunDir(dir string) (string, func(), error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", nil, err
		}
		return dir, func() {}, nil
	}
	tmp, err := os.MkdirTemp("", "agentlab-klaus-gateway-test-")
	if err != nil {
		return "", nil, err
	}
	return tmp, func() { _ = os.RemoveAll(tmp) }, nil
}

// grpcsTarget turns the edge's https base URL into the gateway's grpcs://
// target (the same host and port: the GRPCRoute shares the public listener).
func grpcsTarget(httpsBase string) string {
	return "grpcs://" + strings.TrimPrefix(httpsBase, "https://")
}

// randomSuffix is a short per-run id for the thread.
func randomSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano()%1_000_000, 36)
}

// klausGatewayFixtures renders the proof's objects: the admitted AgentTemplate
// with the display-name and icon annotations the Generic chart 1.x writes,
// its muster carrier (the per-agent RemoteMCPServer the chart renders:
// muster's in-cluster URL, X-Muster-Toolset, discovery off, never an
// Authorization header) bound with requireApproval so every muster call pauses
// for a decision — and the template no Harness admits.
func klausGatewayFixtures(modelConfig string, admission map[string]string) string {
	labels := []string{managedByLabel + ": " + managedByAgentlabValue}
	for _, key := range slices.Sorted(maps.Keys(admission)) {
		labels = append(labels, key+": "+admission[key])
	}
	return fmt.Sprintf(`apiVersion: %[1]s
kind: RemoteMCPServer
metadata:
  name: %[2]s
  namespace: %[3]s
  labels:
    %[4]s: %[5]s
    kagent.dev/discovery: disabled
spec:
  description: "muster MCP gateway of agent %[2]s (toolset %[6]s); throwaway of agentlab klaus-gateway-test"
  url: %[7]s
  protocol: STREAMABLE_HTTP
  headersFrom:
    - name: X-Muster-Toolset
      value: %[6]q
---
apiVersion: %[1]s
kind: AgentTemplate
metadata:
  name: %[2]s
  namespace: %[3]s
  labels:
    %[8]s
  annotations:
    ui.giantswarm.io/display-name: %[9]q
    ui.giantswarm.io/icon-url: %[10]q
spec:
  description: "Throwaway agent of agentlab klaus-gateway-test (the Swarmgeist proof); deleted by the same run."
  modelConfig:
    name: %[11]s
  systemPrompt: %[12]q
  tools:
    - mcp:
        server:
          kind: %[13]s
          name: %[2]s
        requireApproval: true
---
apiVersion: %[1]s
kind: AgentTemplate
metadata:
  name: %[14]s
  namespace: %[3]s
  labels:
    %[4]s: %[5]s
  annotations:
    ui.giantswarm.io/display-name: "agentlab Swarmgeist proof (not admitted)"
spec:
  description: "Throwaway agent of agentlab klaus-gateway-test that no Harness admits; deleted by the same run."
  modelConfig:
    name: %[11]s
  systemPrompt: "Never runs."
`, agentTemplateAPIVersion, klausGatewayTestAgent, kagentNamespace, managedByLabel, managedByAgentlabValue,
		klausGatewayTestToolset, klausGatewayMusterURL, strings.Join(labels, "\n    "), klausGatewayTestDisplay, klausGatewayTestIcon,
		modelConfig, klausGatewayTestPrompt, remoteMCPServerKind, klausGatewayTestUnadmitted)
}

// harnessReadySummary words a template's Ready condition on a Harness for a
// failed boot.
func harnessReadySummary(t *agentTemplate, harness string) string {
	if t == nil {
		return "the template could not be read"
	}
	h := t.harness(harness)
	if h == nil {
		return "no status.harnesses[] entry for " + harness + " (the Harness does not admit it)"
	}
	status, message := h.condition(conditionReady)
	return fmt.Sprintf("Ready=%q %s", status, message)
}

// klausGatewayCleanup removes what the proof creates: the AgentInstances of
// the fixture (the controller's, listed as the person), the AgentTemplates and
// the RemoteMCPServer. Best effort; klausGatewayLeftovers reports what stayed.
func klausGatewayCleanup(api *kagentAPI) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if instances, err := api.listInstances(ctx, klausGatewayTestAgent); err == nil {
		for _, inst := range instances {
			api.deleteInstance(inst.GetId())
		}
	}
	for _, obj := range klausGatewayObjects() {
		gvr, err := gvrFor(obj.resource)
		if err != nil {
			continue
		}
		if err := deleteObject(ctx, gvr, kagentNamespace, obj.name, substrateReleaseWait); err != nil {
			note("cleanup: %s %s: %v", obj.resource, obj.name, err)
		}
	}
}

// klausGatewayObjects are the namespaced objects the proof writes, in the
// order they are deleted.
func klausGatewayObjects() []struct{ resource, name string } {
	return []struct{ resource, name string }{
		{agentTemplateResource, klausGatewayTestAgent},
		{agentTemplateResource, klausGatewayTestUnadmitted},
		{remoteMCPServerResource, klausGatewayTestAgent},
	}
}

// klausGatewayLeftovers lists what the cleanup did not remove.
func klausGatewayLeftovers(api *kagentAPI) []string {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	var left []string
	if instances, err := api.listInstances(ctx, klausGatewayTestAgent); err == nil && len(instances) > 0 {
		left = append(left, fmt.Sprintf("%d AgentInstance(s) of %s: %s", len(instances), klausGatewayTestAgent, instanceIDs(instances)))
	}
	for _, obj := range klausGatewayObjects() {
		gvr, err := gvrFor(obj.resource)
		if err != nil {
			continue
		}
		if exists, _ := objectExists(ctx, gvr, kagentNamespace, obj.name); exists {
			left = append(left, obj.resource+" "+obj.name)
		}
	}
	return left
}

// instanceIDs joins the ids of instances for a message.
func instanceIDs(instances []*kagentpb.AgentInstance) string {
	ids := make([]string, 0, len(instances))
	for _, inst := range instances {
		ids = append(ids, inst.GetId())
	}
	return strings.Join(ids, ", ")
}

// --- the controller, read as the person over gRPC-Web through the edge ------

// listInstances is AgentInstanceService/ListAgentInstances narrowed to one
// template's conversations of the caller.
func (a *kagentAPI) listInstances(ctx context.Context, template string) ([]*kagentpb.AgentInstance, error) {
	var resp kagentpb.ListAgentInstancesResponse
	req := &kagentpb.ListAgentInstancesRequest{AgentTemplate: &kagentpb.ResourceReference{Namespace: kagentNamespace, Name: template}}
	if err := a.call(ctx, agentInstanceService, "ListAgentInstances", req, &resp, nil); err != nil {
		return nil, fmt.Errorf("listing the AgentInstances of %s as %s: %w", template, a.user, err)
	}
	return resp.GetAgentInstances(), nil
}

// getTask is A2AService/GetTask on one instance's task.
func (a *kagentAPI) getTask(ctx context.Context, instanceID, taskID string) (*a2apb.Task, error) {
	var task a2apb.Task
	if err := a.call(ctx, a2aService, "GetTask", &a2apb.GetTaskRequest{Id: taskID}, &task, map[string]string{agentInstanceHeader: instanceID}); err != nil {
		return nil, fmt.Errorf("GetTask %s on AgentInstance %s: %w", taskID, instanceID, err)
	}
	return &task, nil
}

// listTasks is A2AService/ListTasks on one instance.
func (a *kagentAPI) listTasks(ctx context.Context, instanceID string) ([]*a2apb.Task, error) {
	var resp a2apb.ListTasksResponse
	if err := a.call(ctx, a2aService, "ListTasks", &a2apb.ListTasksRequest{PageSize: proto.Int32(100)}, &resp, map[string]string{agentInstanceHeader: instanceID}); err != nil {
		return nil, fmt.Errorf("ListTasks on AgentInstance %s: %w", instanceID, err)
	}
	return resp.GetTasks(), nil
}

// taskIDs is the set of an instance's task ids.
func (a *kagentAPI) taskIDs(ctx context.Context, instanceID string) (map[string]bool, error) {
	tasks, err := a.listTasks(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		ids[t.GetId()] = true
	}
	return ids, nil
}

// waitCanceledTask polls the instance's tasks until one not in before is
// TASK_STATE_CANCELED and returns its id; what the new tasks say otherwise is
// the error.
func (a *kagentAPI) waitCanceledTask(instanceID string, before map[string]bool, timeout time.Duration) (string, error) {
	var canceled string
	var seen []string
	waitFor(int(timeout/pollInterval), pollInterval, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		tasks, err := a.listTasks(ctx, instanceID)
		if err != nil {
			return false
		}
		seen = seen[:0]
		for _, t := range tasks {
			if before[t.GetId()] {
				continue
			}
			state := t.GetStatus().GetState()
			seen = append(seen, t.GetId()+" "+state.String())
			if state == a2apb.TaskState_TASK_STATE_CANCELED {
				canceled = t.GetId()
			}
		}
		return canceled != ""
	})
	if canceled == "" {
		return "", fmt.Errorf("no task of AgentInstance %s reached TASK_STATE_CANCELED within %s after the stream was cut (new tasks: %s)", instanceID, timeout, strings.Join(seen, ", "))
	}
	return canceled, nil
}

// --- the gateway on the host -----------------------------------------------

// gatewayProcess runs klaus-gateway on the host: a local binary, or the
// released image on the host network with the run directory and the lab CA
// mounted. Its log (JSON on stderr) accumulates across restarts in the run
// directory; the bolt store lives there too, so a restart resumes on it.
type gatewayProcess struct {
	opts    KlausGatewayTestOptions
	runDir  string
	caFile  string
	target  string
	cmd     *exec.Cmd
	logFile *os.File
	// exited is closed once the process has ended; exitErr is its Wait result.
	// A closed channel satisfies every later wait, so a gateway that died
	// before serving is noticed by the health probe and stop() still returns.
	exited  chan struct{}
	exitErr error
}

func newGatewayProcess(opts KlausGatewayTestOptions, runDir, caFile, target string) *gatewayProcess {
	return &gatewayProcess{opts: opts, runDir: runDir, caFile: caFile, target: target}
}

// describe names what runs.
func (g *gatewayProcess) describe() string {
	if g.opts.GatewayBinary != "" {
		return "binary " + g.opts.GatewayBinary
	}
	return "image " + g.opts.GatewayImage
}

func (g *gatewayProcess) baseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", g.opts.Port)
}

func (g *gatewayProcess) logPath() string { return filepath.Join(g.runDir, gatewayLogFile) }

// gatewayArgs are the gateway's flags for one shape: the web channel on the
// loopback port, the bolt store, the static lifecycle driver (no Klaus
// instances here), the a2a client at the grpcs target with the CA, the
// fixture as the default agent. dataDir and caFile are the paths as the
// process sees them (the container's mounts in the image shape).
func gatewayArgs(port int, dataDir, caFile, target string) []string {
	return []string{
		"--listen-address=" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		"--admin-address=" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port+1)),
		"--log-level=info",
		"--store=bolt",
		"--bolt-path=" + filepath.Join(dataDir, gatewayBoltFile),
		"--driver=static",
		"--web-enabled=true",
		"--a2a-enabled=true",
		"--a2a-url=" + target,
		"--a2a-ca-file=" + caFile,
		"--a2a-namespace=" + kagentNamespace,
		"--a2a-default-agent=" + klausGatewayTestAgent,
	}
}

// dockerRunArgs wraps the gateway's flags into `docker run`: the host network
// (the loopback port and the edge's public hostname as the host sees them),
// the caller's uid so the store is writable in the run directory, the run
// directory and the CA mounted read-write and read-only.
func dockerRunArgs(image, name, runDir, caFile string, uid, gid int, args []string) []string {
	return append([]string{
		"run", "--rm", "--name", name, "--network", "host",
		"--user", fmt.Sprintf("%d:%d", uid, gid),
		"-v", runDir + ":" + gatewayDataPath,
		"-v", caFile + ":" + gatewayCAPath + ":ro",
		image,
	}, args...)
}

// start launches the gateway and waits for its web channel's health.
func (g *gatewayProcess) start() error {
	logFile, err := os.OpenFile(g.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	g.logFile = logFile
	if g.opts.GatewayBinary != "" {
		g.cmd = command(g.opts.GatewayBinary, gatewayArgs(g.opts.Port, g.runDir, g.caFile, g.target)...)
	} else {
		_ = command(dockerBin, "rm", "-f", gatewayContainer).Run()
		g.cmd = command(dockerBin, dockerRunArgs(g.opts.GatewayImage, gatewayContainer, g.runDir, g.caFile, os.Getuid(), os.Getgid(),
			gatewayArgs(g.opts.Port, gatewayDataPath, gatewayCAPath, g.target))...)
	}
	g.cmd.Stdout, g.cmd.Stderr = logFile, logFile
	if err := g.cmd.Start(); err != nil {
		_ = logFile.Close()
		g.cmd, g.logFile = nil, nil
		return fmt.Errorf("starting klaus-gateway (%s): %w", g.describe(), err)
	}
	g.exited = make(chan struct{})
	go func(cmd *exec.Cmd, exited chan<- struct{}) { g.exitErr = cmd.Wait(); close(exited) }(g.cmd, g.exited)

	client := &http.Client{Timeout: 2 * time.Second}
	healthy := waitFor(int(klausGatewayStartWait/(500*time.Millisecond)), 500*time.Millisecond, func() bool {
		if g.ended() {
			return true
		}
		resp, err := client.Get(g.baseURL() + webHealthPath)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	if g.ended() || !healthy {
		why := fmt.Sprintf("did not become healthy at %s%s within %s", g.baseURL(), webHealthPath, klausGatewayStartWait)
		if g.ended() {
			why = fmt.Sprintf("exited before serving %s (%v)", webHealthPath, g.exitErr)
		}
		log := tailLines(g.logs(), 8)
		_ = g.stop()
		return fmt.Errorf("klaus-gateway (%s) %s; its log ends:\n%s", g.describe(), why, log)
	}
	return nil
}

// ended reports whether the gateway process has exited (exitErr says how).
func (g *gatewayProcess) ended() bool {
	select {
	case <-g.exited:
		return true
	default:
		return false
	}
}

// stop ends the gateway: SIGTERM (docker stop for the container) and a
// bounded wait, SIGKILL after it. Idempotent; a gateway that never started
// is nothing to stop.
func (g *gatewayProcess) stop() error {
	if g.cmd == nil {
		return nil
	}
	if g.opts.GatewayBinary == "" {
		_ = command(dockerBin, "stop", "-t", "15", gatewayContainer).Run()
	} else {
		_ = g.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-g.exited:
	case <-time.After(klausGatewayStopWait):
		_ = g.cmd.Process.Kill()
		<-g.exited
	}
	g.cmd, g.exited = nil, nil
	if g.logFile != nil {
		_ = g.logFile.Close()
		g.logFile = nil
	}
	return nil
}

// version is the gateway's own "starting" record, for the notes.
func (g *gatewayProcess) version() string {
	for _, line := range strings.Split(g.logs(), "\n") {
		if strings.Contains(line, `"klaus-gateway starting"`) {
			var rec struct {
				Version string `json:"version"`
				GitSHA  string `json:"git_sha"`
			}
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Version != "" {
				return fmt.Sprintf("klaus-gateway %s (%s)", rec.Version, rec.GitSHA)
			}
		}
	}
	return "klaus-gateway (version not logged)"
}

// logs is everything the gateway wrote so far.
func (g *gatewayProcess) logs() string {
	b, _ := os.ReadFile(g.logPath())
	return string(b)
}

// boundCount counts the instance_bound records: one per thread bound to a
// new AgentInstance.
func (g *gatewayProcess) boundCount() int {
	return strings.Count(g.logs(), instanceBoundRecord)
}

// boundInstance is the AgentInstance id of the last instance_bound record.
func (g *gatewayProcess) boundInstance() (string, error) {
	lines := strings.Split(g.logs(), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.Contains(lines[i], instanceBoundRecord) {
			continue
		}
		var rec struct {
			Instance string `json:"instance"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &rec); err == nil && rec.Instance != "" {
			return rec.Instance, nil
		}
	}
	return "", fmt.Errorf("the gateway's log carries no instance_bound record (the turn ran without binding the thread to an AgentInstance); its log ends:\n%s", tailLines(g.logs(), 8))
}

// tailLines is the last n non-empty lines of s, each cut short.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, line := range lines {
		lines[i] = excerpt(line, 240)
	}
	return strings.Join(lines, "\n")
}

// --- the web channel, driven as the person -----------------------------------

// webClient drives one thread of klaus-gateway's web channel as one person:
// every request carries the id_token as the bearer the gateway forwards.
type webClient struct {
	base, token, user, thread string
}

// webMessage is POST /web/messages: a user message, or a decision on the task
// a previous turn paused on.
type webMessage struct {
	Text     string
	AgentRef string
	TaskID   string
	Decision *webDecision
}

type webDecision struct {
	Type string `json:"type"`
}

// webAgent is one entry of GET /web/agents.
type webAgent struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	DisplayName string `json:"displayName"`
	IconURL     string `json:"iconUrl"`
	Description string `json:"description"`
}

// webPrompt is the `prompt` event: the paused task and what it asks.
type webPrompt struct {
	TaskID string `json:"taskId"`
	Text   string `json:"text"`
	Prompt struct {
		ToolName string `json:"toolName"`
		Hint     string `json:"hint"`
		Tools    []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"tools"`
	} `json:"prompt"`
}

// webTurn is what one POST /web/messages answered: the HTTP status and body
// of a refusal, or the stream's outcome — the text, whether it ended with
// done, the prompt it paused on, the error event, and whether the client cut
// it short.
type webTurn struct {
	Status int
	Body   string
	Text   string
	Done   bool
	Prompt *webPrompt
	Err    string
	Cut    bool
}

func (w *webClient) request(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// agents is GET /web/agents.
func (w *webClient) agents(ctx context.Context) ([]webAgent, error) {
	req, err := w.request(ctx, http.MethodGet, webAgentsPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s answered HTTP %d: %s", webAgentsPath, resp.StatusCode, excerpt(string(body), 300))
	}
	var out struct {
		Agents []webAgent `json:"agents"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("GET %s: %w: %s", webAgentsPath, err, excerpt(string(body), 200))
	}
	return out.Agents, nil
}

// send posts one message on the thread and reads the stream to its end (or
// until ctx ends, which the gateway takes as a stop: Cut).
func (w *webClient) send(ctx context.Context, msg webMessage, timeout time.Duration) (*webTurn, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	payload := map[string]any{"channelId": webChannelID, "userId": w.user, "threadId": w.thread}
	if msg.Text != "" {
		payload["text"] = msg.Text
	}
	if msg.AgentRef != "" {
		payload["agentRef"] = msg.AgentRef
	}
	if msg.TaskID != "" {
		payload["taskId"] = msg.TaskID
	}
	if msg.Decision != nil {
		payload["decision"] = msg.Decision
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := w.request(ctx, http.MethodPost, webMessagesPath, body)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// The deadline fired before the stream's headers arrived: the
			// gateway had the message and sees the connection close — a
			// stop all the same.
			return &webTurn{Status: http.StatusOK, Cut: true}, nil
		}
		return nil, fmt.Errorf("POST %s: %w", webMessagesPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	turn := &webTurn{Status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		turn.Body = string(b)
		return turn, nil
	}
	if err := readSSE(resp.Body, turn); err != nil {
		if ctx.Err() != nil {
			turn.Cut = true
			return turn, nil
		}
		return turn, fmt.Errorf("reading the stream of POST %s: %w", webMessagesPath, err)
	}
	return turn, nil
}

// readSSE folds the stream's events into the turn: `data:` lines of the
// default event carry {"content"} deltas, `event: done` ends the turn,
// `event: prompt` pauses it on a decision, `event: error` fails it.
func readSSE(r io.Reader, turn *webTurn) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	event := ""
	var text strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			event = ""
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			switch event {
			case sseEventDone:
				turn.Done = true
			case sseEventPrompt:
				var p webPrompt
				if err := json.Unmarshal([]byte(data), &p); err != nil {
					return fmt.Errorf("prompt event %q: %w", excerpt(data, 200), err)
				}
				turn.Prompt = &p
			case sseEventError:
				var msg string
				if json.Unmarshal([]byte(data), &msg) != nil {
					msg = data
				}
				turn.Err = msg
			default:
				var delta struct {
					Content string `json:"content"`
				}
				if json.Unmarshal([]byte(data), &delta) == nil {
					text.WriteString(delta.Content)
				}
			}
		}
	}
	turn.Text = text.String()
	return scanner.Err()
}

// firstTurn is the thread's first message with one visible retry: a cold
// worker's first resume can hit Substrate's ResumeActor deadline once.
func (w *webClient) firstTurn(text string) (*webTurn, error) {
	turn, err := w.send(context.Background(), webMessage{Text: text}, klausGatewayTurnTimeout)
	if err == nil && turn.Status == http.StatusOK && turn.Err == "" {
		return turn, nil
	}
	reason := "error: " + fmt.Sprint(err)
	if err == nil {
		reason = turnFailure(turn)
	}
	note("the first turn failed (%s); retrying once — a cold worker's first resume may hit the ResumeActor deadline", excerpt(reason, 200))
	return w.send(context.Background(), webMessage{Text: text}, klausGatewayTurnTimeout)
}

// turnFailure words a turn that did not complete.
func turnFailure(turn *webTurn) string {
	switch {
	case turn.Status != http.StatusOK:
		return fmt.Sprintf("HTTP %d: %s", turn.Status, excerpt(turn.Body, 300))
	case turn.Err != "":
		return "error event: " + excerpt(turn.Err, 300)
	case turn.Prompt != nil:
		return "paused on a prompt for " + turn.Prompt.Prompt.ToolName
	case turn.Cut:
		return "the stream was cut"
	case !turn.Done:
		return "the stream ended without done"
	}
	return "ok"
}

// assertTurnSaid checks a turn completed and its text carries the word.
func assertTurnSaid(turn *webTurn, word string) error {
	if failure := turnFailure(turn); failure != "ok" {
		return fmt.Errorf("the turn did not complete: %s", failure)
	}
	if !strings.Contains(strings.ToLower(turn.Text), strings.ToLower(word)) {
		return fmt.Errorf("the turn completed but its text %q does not carry %q", excerpt(turn.Text, 200), word)
	}
	return nil
}

// sendAndCut posts a message and closes the stream after the given time, the
// web channel's stop: the gateway cancels the task at the controller.
func (w *webClient) sendAndCut(text string, after time.Duration) (*webTurn, error) {
	turn, err := w.send(context.Background(), webMessage{Text: text}, after)
	if err != nil {
		return nil, fmt.Errorf("the turn to stop: %w", err)
	}
	if turn.Status != http.StatusOK {
		return nil, fmt.Errorf("the turn to stop was refused: %s", turnFailure(turn))
	}
	if !turn.Cut {
		return nil, fmt.Errorf("the turn to stop ended on its own within %s (%s) — nothing to cancel; a longer answer is needed", after, turnFailure(turn))
	}
	return turn, nil
}

// hitlOutcome is how the approval round trip ended.
type hitlOutcome struct {
	taskID     string
	rounds     int
	tools      []string
	finalState string
	text       string
}

// approveUntilDone takes the tool-using turn's prompt, checks the task is
// paused at input-required at the controller, approves on the web channel
// with the task id, and repeats while the resumed turn pauses again (a
// meta-tool call follows the first), up to klausGatewayHITLRounds; the
// resumed task must end completed with nothing of the instance left waiting.
func (w *webClient) approveUntilDone(api *kagentAPI, instanceID string, turn *webTurn) (*hitlOutcome, error) {
	if turn.Prompt == nil {
		return nil, fmt.Errorf("the tool-using turn did not pause on a prompt (%s): the requireApproval binding did not gate the tool call", turnFailure(turn))
	}
	out := &hitlOutcome{taskID: turn.Prompt.TaskID}
	for turn.Prompt != nil {
		if out.rounds == klausGatewayHITLRounds {
			return nil, fmt.Errorf("still paused after %d approvals (last on %s)", out.rounds, turn.Prompt.Prompt.ToolName)
		}
		if turn.Prompt.TaskID != out.taskID {
			return nil, fmt.Errorf("the resumed turn paused on task %s, not the one it resumed (%s): the decision did not resume in place", turn.Prompt.TaskID, out.taskID)
		}
		out.rounds++
		out.tools = append(out.tools, turn.Prompt.Prompt.ToolName)
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		task, err := api.getTask(ctx, instanceID, out.taskID)
		cancel()
		if err != nil {
			return nil, err
		}
		if state := task.GetStatus().GetState(); state != a2apb.TaskState_TASK_STATE_INPUT_REQUIRED {
			return nil, fmt.Errorf("the gateway reports a prompt on task %s but the controller has it %s, not TASK_STATE_INPUT_REQUIRED", out.taskID, state)
		}
		turn, err = w.send(context.Background(), webMessage{Text: "approve", TaskID: out.taskID, Decision: &webDecision{Type: "approve"}}, klausGatewayTurnTimeout)
		if err != nil {
			return nil, fmt.Errorf("approval %d: %w", out.rounds, err)
		}
		if failure := turnFailure(turn); failure != "ok" && turn.Prompt == nil {
			return nil, fmt.Errorf("approval %d did not resume the task to completion: %s", out.rounds, failure)
		}
	}
	out.text = turn.Text
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	task, err := api.getTask(ctx, instanceID, out.taskID)
	if err != nil {
		return nil, err
	}
	out.finalState = task.GetStatus().GetState().String()
	if task.GetStatus().GetState() != a2apb.TaskState_TASK_STATE_COMPLETED {
		return nil, fmt.Errorf("the resumed task %s ended %s at the controller, not completed", out.taskID, out.finalState)
	}
	tasks, err := api.listTasks(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if t.GetStatus().GetState() == a2apb.TaskState_TASK_STATE_INPUT_REQUIRED {
			return nil, fmt.Errorf("task %s of the instance is still at input-required after the decisions", t.GetId())
		}
	}
	return out, nil
}

// assertRoster checks the roster carries the admitted fixture with its
// annotations and not the unadmitted one.
func assertRoster(agents []webAgent) error {
	var found *webAgent
	for i := range agents {
		switch agents[i].Name {
		case klausGatewayTestAgent:
			found = &agents[i]
		case klausGatewayTestUnadmitted:
			return fmt.Errorf("the roster lists %s, which no Harness admits", klausGatewayTestUnadmitted)
		}
	}
	if found == nil {
		return fmt.Errorf("the roster does not list %s (%s)", klausGatewayTestAgent, rosterLine(agents))
	}
	if found.Namespace != kagentNamespace || found.DisplayName != klausGatewayTestDisplay || found.IconURL != klausGatewayTestIcon {
		return fmt.Errorf("the roster lists %s as namespace %q, displayName %q, iconUrl %q; wanted %q, %q, %q from the template's annotations",
			found.Name, found.Namespace, found.DisplayName, found.IconURL, kagentNamespace, klausGatewayTestDisplay, klausGatewayTestIcon)
	}
	return nil
}

// rosterLine words the roster.
func rosterLine(agents []webAgent) string {
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.Name)
	}
	return fmt.Sprintf("%d agent(s): %s", len(agents), strings.Join(names, ", "))
}

// unadmittedReason is the gateway's reason for refusing a template no Harness
// admits (pkg/a2a's harnessReadiness).
const unadmittedReason = "no Harness admits"

// assertUnadmittedRefused checks a turn naming the unadmitted template was
// refused synchronously with the reason, not run.
func assertUnadmittedRefused(turn *webTurn) error {
	if turn.Status == http.StatusOK {
		return fmt.Errorf("a turn on %s was accepted (%s); the gateway must refuse a template no Harness admits", klausGatewayTestUnadmitted, turnFailure(turn))
	}
	if !strings.Contains(turn.Body, unadmittedReason) {
		return fmt.Errorf("a turn on %s was refused with HTTP %d but without the reason %q: %s", klausGatewayTestUnadmitted, turn.Status, unadmittedReason, excerpt(turn.Body, 300))
	}
	return nil
}

// assertMusterAttribution reads muster's log since the tool-using turn began:
// the forwarded id_token of the person accepted (the audit record with the
// email), and tools/call requests under the token's subject — the agent's
// muster calls ran as the person, not as the gateway or the Harness.
func assertMusterAttribution(email, subject string, since time.Time) error {
	var accepted, calls int
	var lines []string
	waitFor(klausGatewayMusterAttempt, pollInterval, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		logs, err := podLogs(ctx, platformNamespace, "deploy/"+componentMuster, componentMuster, klausGatewayLogSince)
		if err != nil {
			return false
		}
		accepted, calls, lines = musterAttribution(logs, email, subject, since)
		return accepted > 0 && calls > 0
	})
	if accepted == 0 {
		return fmt.Errorf("muster's log since %s carries no %s record for %s: the agent's tool calls did not ride the person's forwarded id_token", since.UTC().Format(time.RFC3339), musterTokenAccepted, email)
	}
	if calls == 0 {
		return fmt.Errorf("muster's log since %s carries %d %s record(s) for %s but no tools/call request under subject %.8s…: %s", since.UTC().Format(time.RFC3339), accepted, musterTokenAccepted, email, subject, strings.Join(lines, " | "))
	}
	return nil
}

// musterAttribution counts, in muster's log lines stamped after since, the
// accepted-token audit records naming the email and the tools/call requests
// whose (truncated) subject is a prefix of the token's subject.
func musterAttribution(logs, email, subject string, since time.Time) (accepted, calls int, subjects []string) {
	for _, line := range strings.Split(logs, "\n") {
		// A line may carry a pod prefix before its JSON record.
		start := strings.IndexByte(line, '{')
		if start < 0 {
			continue
		}
		record := []byte(line[start:])
		var rec struct {
			Time    string `json:"time"`
			Subject string `json:"subject"`
		}
		if json.Unmarshal(record, &rec) != nil {
			continue
		}
		stamp, err := time.Parse(time.RFC3339Nano, rec.Time)
		if err != nil || stamp.Before(since) {
			continue
		}
		switch {
		case strings.Contains(line, musterTokenAccepted) && strings.Contains(line, `"email":"`+email+`"`):
			accepted++
		case strings.Contains(line, musterToolCall):
			call := rec
			logged := strings.TrimSuffix(call.Subject, "...")
			subjects = append(subjects, call.Subject)
			if logged != "" && strings.HasPrefix(subject, logged) {
				calls++
			}
		}
	}
	return accepted, calls, subjects
}
