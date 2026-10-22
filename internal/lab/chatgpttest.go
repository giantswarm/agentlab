package lab

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The ChatGPT subscription proof: codex on a person's own ChatGPT login
// (platform.chatgptSubscription), from the chart's values, with a real login
// the person connects through muster's device-code sign-in. Every verdict on
// the ChatGPT backend is read from Substrate's egress gateway's record of the
// Session's actor (egressrecord.go), never from a reply.
//
//  1. The values: the ModelConfig ChatGPTSubscriptionModelConfig Accepted,
//     chatgptOAuth without apiKeySecret and no login Secret of its own;
//     muster lists the provider App chatgpt for the person.
//  2. The connection: the admin holds the App's grant. --connect starts the
//     device-code sign-in first and prints the code and OpenAI's page, which
//     the person enters there; the proof waits --connect-wait for it.
//  3. A codex agent created through agent-manager as the admin on the
//     ChatGPT model; a Session of it started through start_session, which
//     binds it to the admin in muster. Its turn completes, and the gateway
//     forwarded the actor's requests to chatgpt.com/backend-api/codex/.
//  4. The sandbox of that turn: no token-shaped value in its environment,
//     /data or /workspace, no refresh_token in a file, the model key kagent's
//     placeholder. The refresh token stays in muster.
//  5. A second person (a developer) without a connection: their Session of
//     the same agent is refused at the egress gateway, 403 with the
//     credential provider's denial, nothing forwarded to the backend.
//  6. A non-codex agent of the admin (the platform Harness): its bash tool's
//     request to the backend answered 403 by the egress gate with the actor
//     egress policy's denial, nothing forwarded.
//  7. The disconnect: core_auth_logout app=chatgpt drops the grant, the
//     status reads auth_required, and the admin's next Session's turn is
//     refused like the second person's.
//
// --unconnected runs what needs no connection (1, 5, 6) and leaves 2–4 and 7
// out. The agents and Sessions are removed on every exit path; the admin
// ends disconnected after a full run.

const (
	// chatgptTestAgent is the codex agent, chatgptTestPlainAgent the
	// platform Harness agent; both deleted by the same run.
	chatgptTestAgent      = "agentlab-chatgpt"
	chatgptTestPlainAgent = "agentlab-chatgpt-plain"
	// chatgptProviderApp is muster's provider App of the person's login.
	chatgptProviderApp = "chatgpt"
	// chatgptBackendHost and chatgptBackendPath are the ChatGPT Codex
	// backend the provider binds the person's bearer on.
	chatgptBackendHost = "chatgpt.com"
	chatgptBackendPath = "/backend-api/codex/"
	// chatgptBackendURL is the backend URL the plain agent's probe requests.
	chatgptBackendURL = "https://" + chatgptBackendHost + chatgptBackendPath + "responses"
	// chatgptCredentialDenied is the gateway's reason for a request whose
	// credential the provider refused.
	chatgptCredentialDenied = "credential provider" // #nosec G101 -- the gateway's words for a refusal, no credential
	// The auth status of a provider App.
	appConnected    = "connected"
	appAuthRequired = "auth_required"
	// chatgptConnectPoll is the cadence the connection is read at while the
	// person enters the code.
	chatgptConnectPoll = 5 * time.Second
	// chatgptTestSystemPrompt keeps the turns to the probes.
	chatgptTestSystemPrompt = "You are a probe in a test lab. Run exactly the commands you are given with your shell tool, once, and reply with the output lines you are asked for, verbatim."
)

// DefaultChatGPTConnectWait is how long --connect waits for the code to be
// entered: the code's own lifetime at OpenAI.
const DefaultChatGPTConnectWait = 15 * time.Minute

// ChatGPTTestOptions tunes the ChatGPT subscription proof.
type ChatGPTTestOptions struct {
	// Connect starts the admin's device-code sign-in when they hold no
	// grant, prints the code and OpenAI's page, and waits ConnectWait for
	// the person to enter it; zero prints and ends the run there.
	Connect     bool
	ConnectWait time.Duration
	// Unconnected runs the stages that need no connection alone.
	Unconnected bool
	// ReadyTimeout bounds each agent's boot; TurnTimeout each turn.
	ReadyTimeout, TurnTimeout time.Duration
}

// ChatGPTTest is the ChatGPT subscription proof (the stages above).
func ChatGPTTest(cfg *config.Config, opts ChatGPTTestOptions) error {
	if !cfg.ChatGPTSubscriptionEnabled() {
		return fmt.Errorf("platform.chatgptSubscription is off in %s — `agentlab configure --chatgpt-subscription`, then `agentlab platform`", config.File)
	}
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if opts.Connect && opts.Unconnected {
		return fmt.Errorf("--connect and --unconnected exclude each other")
	}
	opts.ReadyTimeout = cmp.Or(opts.ReadyTimeout, DefaultCodingReadyTimeout)
	opts.TurnTimeout = cmp.Or(opts.TurnTimeout, hostedModelTurnTimeout)
	admin, second := cfg.AdminUser(), cfg.FindUserInGroup("developers")
	if admin == nil || second == nil {
		return fmt.Errorf("the proof needs an admin and a developer among the lab users in %s", config.File)
	}
	p := &chatgptProof{cfg: cfg, opts: opts, stages: newProofStages()}
	defer p.cleanup()
	var err error
	if p.admin, err = openChatGPTPerson(cfg, admin); err != nil {
		return err
	}
	if p.second, err = openChatGPTPerson(cfg, second); err != nil {
		return err
	}
	return p.stages.close(p.run())
}

// chatgptProof is one run's state.
type chatgptProof struct {
	cfg           *config.Config
	opts          ChatGPTTestOptions
	stages        *proofStages
	admin, second *chatgptPerson
	agents        []string
}

// chatgptPerson is a lab user signed in to muster and to the kagent API,
// with the Sessions started as them.
type chatgptPerson struct {
	user     *config.User
	token    string
	muster   *musterSession
	api      *kagentAPI
	sessions *sessionSet
}

func openChatGPTPerson(cfg *config.Config, user *config.User) (*chatgptPerson, error) {
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return nil, err
	}
	login, _, _ := strings.Cut(user.Email, "@")
	ms, err := openMusterSession(cfg, token, "chatgpt-test-"+login)
	if err != nil {
		return nil, err
	}
	api, err := dialKagentAPI(cfg, token)
	if err != nil {
		return nil, err
	}
	return &chatgptPerson{user: user, token: token, muster: ms, api: api, sessions: &sessionSet{api: api, manager: ms}}, nil
}

func (p *chatgptProof) run() error {
	p.stages.begin("values", "The ModelConfig %s from the chart's values: chatgptOAuth without apiKeySecret, Accepted, no login Secret; muster lists the provider App %s", config.ChatGPTSubscriptionModelConfig, chatgptProviderApp)
	if err := p.proveValues(); err != nil {
		return err
	}

	connectStages := []string{"connection", "connected turn", "sandbox", "disconnect"}
	connected := false
	if p.opts.Unconnected {
		p.stages.leaveOut("--unconnected: the stages that need the admin's ChatGPT connection are not run", connectStages...)
	} else {
		p.stages.begin("connection", "%s holds the grant of muster's provider App %s (a real ChatGPT login through OpenAI's device-code sign-in)", p.admin.user.Email, chatgptProviderApp)
		done, err := p.connect()
		if err != nil || !done {
			return err
		}
		connected = true
	}

	p.removeAgents()
	if err := p.createAgent(chatgptTestAgent, config.CodingRuntimeCodex, config.ChatGPTSubscriptionModelConfig, nil); err != nil {
		return err
	}

	if connected {
		p.stages.begin("connected turn", "A Session of %s on Harness codex started as %s: the turn completes, the egress gateway forwarded its requests to %s%s", chatgptTestAgent, p.admin.user.Email, chatgptBackendHost, chatgptBackendPath)
		reply, err := p.proveConnectedTurn()
		if err != nil {
			return err
		}
		p.stages.begin("sandbox", "The sandbox of that turn holds no login: no token-shaped value, no refresh_token in a file, the model key kagent's placeholder")
		if err := judgeChatGPTSandbox(parseCodingProbe(reply)); err != nil {
			return fmt.Errorf("%w; the reply: %q", err, excerpt(reply, 800))
		}
	}

	p.stages.begin("second person refused", "A Session of %s started as %s, who has not connected ChatGPT: refused at the egress gateway with the credential provider's denial, nothing forwarded to the backend", chatgptTestAgent, p.second.user.Email)
	if err := p.proveRefused(p.second); err != nil {
		return err
	}

	p.stages.begin("non-codex agent refused", "%s on the platform Harness as %s: its bash tool's request to %s answered %s by the egress gate (%s), nothing forwarded", chatgptTestPlainAgent, p.admin.user.Email, chatgptBackendURL, egressGateStatus, egressPolicyDenied)
	if err := p.provePlainAgentRefused(); err != nil {
		return err
	}

	if connected {
		p.stages.begin("disconnect", "core_auth_logout app=%s as %s drops the grant; the next Session's turn is refused like the second person's", chatgptProviderApp, p.admin.user.Email)
		if err := p.proveDisconnect(); err != nil {
			return err
		}
	}
	return nil
}

// proveValues reads the chart's model and muster's App: the ModelConfig
// without a Secret of its own and Accepted, and the App in the person's
// auth status.
func (p *chatgptProof) proveValues() error {
	model, err := readModelConfig(config.ChatGPTSubscriptionModelConfig)
	if err != nil {
		return fmt.Errorf("the chart's ModelConfig %s: %w", config.ChatGPTSubscriptionModelConfig, err)
	}
	if verdict := judgeChatGPTModel(model); verdict != "" {
		return fmt.Errorf("ModelConfig %s: %s", config.ChatGPTSubscriptionModelConfig, verdict)
	}
	secret := "kagent-" + config.ChatGPTSubscriptionModelConfig
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if exists, err := objectExists(ctx, gvrSecrets, kagentNamespace, secret); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("a Secret %s/%s exists beside the ModelConfig: the person's login must live in muster alone", kagentNamespace, secret)
	}
	name, _, _ := unstructured.NestedString(model.Object, crSpec, modelField)
	status, err := p.admin.appStatus()
	if err != nil {
		return err
	}
	note("ModelConfig %s: %s, chatgptOAuth, no apiKeySecret, Accepted; no Secret %s; muster's App %s reads %s for %s", config.ChatGPTSubscriptionModelConfig, name, secret, chatgptProviderApp, status.Status, p.admin.user.Email)
	return nil
}

// judgeChatGPTModel is the verdict on the chart's ModelConfig: what is wrong
// with it, "" for a person's own login that kagent accepted.
func judgeChatGPTModel(model *unstructured.Unstructured) string {
	credential, _, _ := unstructured.NestedString(model.Object, crSpec, "openAI", "credentialType")
	if credential != config.CredentialTypeChatGPT {
		return fmt.Sprintf("spec.openAI.credentialType %q, want %s", credential, config.CredentialTypeChatGPT)
	}
	if secret, _, _ := unstructured.NestedString(model.Object, crSpec, "apiKeySecret"); secret != "" {
		return fmt.Sprintf("spec.apiKeySecret %q: the installation's login, not the person's own", secret)
	}
	c := condition(model, conditionAccepted)
	switch {
	case c == nil:
		return "no Accepted condition"
	case conditionStatus(model, conditionAccepted) != conditionTrue:
		return fmt.Sprintf("Accepted=%v %v: %v", c[crStatus], c["reason"], c[frameKindMessage])
	}
	return ""
}

// chatgptAppStatus is the person's auth status of a provider App
// (core_auth_status' providerApps entry).
type chatgptAppStatus struct {
	Name            string `json:"name"`
	Status          string `json:"status"`
	Error           string `json:"error"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	CodeExpiresAt   string `json:"code_expires_at"`
	LastFailure     string `json:"last_failure"`
}

// appStatus is the person's auth status of the App.
func (cp *chatgptPerson) appStatus() (chatgptAppStatus, error) {
	env, err := cp.muster.callToolEnvelope("core_auth_status", nil)
	if err != nil {
		return chatgptAppStatus{}, err
	}
	if env.IsError {
		return chatgptAppStatus{}, fmt.Errorf("core_auth_status as %s: %.300s", cp.user.Email, env.Content[0].Text)
	}
	var status struct {
		ProviderApps []chatgptAppStatus `json:"providerApps"`
	}
	if err := json.Unmarshal([]byte(env.Content[0].Text), &status); err != nil {
		return chatgptAppStatus{}, fmt.Errorf("core_auth_status as %s: %w: %.300s", cp.user.Email, err, env.Content[0].Text)
	}
	i := slices.IndexFunc(status.ProviderApps, func(a chatgptAppStatus) bool { return a.Name == chatgptProviderApp })
	if i < 0 {
		return chatgptAppStatus{}, fmt.Errorf("muster lists no provider App %s for %s (its Apps: %v): the chart derived none from the model entry", chatgptProviderApp, cp.user.Email, status.ProviderApps)
	}
	return status.ProviderApps[i], nil
}

// connect is the connection stage: done when the admin holds the grant.
// With --connect and no grant it starts the sign-in and waits; a wait of
// zero prints the code and ends the run there (done false, no error).
func (p *chatgptProof) connect() (bool, error) {
	status, err := p.admin.appStatus()
	if err != nil {
		return false, err
	}
	if status.Status == appConnected {
		note("%s holds the grant of %s", p.admin.user.Email, chatgptProviderApp)
		return true, nil
	}
	if !p.opts.Connect {
		return false, fmt.Errorf("%s holds no grant of muster's App %s (status %s%s): `agentlab chatgpt-test --connect` signs them in, or --unconnected runs what needs no connection", p.admin.user.Email, chatgptProviderApp, status.Status, failureWords(status))
	}
	env, err := p.admin.muster.callToolEnvelope("core_auth_login", map[string]any{musterAppArg: chatgptProviderApp})
	if err != nil {
		return false, err
	}
	code, _ := env.StructuredContent["userCode"].(string)
	page, _ := env.StructuredContent["verificationUrl"].(string)
	expires, _ := env.StructuredContent["expiresAt"].(string)
	if env.IsError || code == "" || page == "" {
		return false, fmt.Errorf("core_auth_login app=%s as %s answered no device code: %.400s", chatgptProviderApp, p.admin.user.Email, env.Content[0].Text)
	}
	note("ChatGPT sign-in for %s: open %s, sign in to ChatGPT and enter the code %s (it expires at %s)", p.admin.user.Email, page, code, expires)
	if p.opts.ConnectWait <= 0 {
		p.stages.skip("--connect-wait 0: the code is printed, muster waits for it until %s; run `agentlab chatgpt-test` once it is entered", expires)
		return false, nil
	}
	deadline := time.Now().Add(p.opts.ConnectWait)
	for time.Now().Before(deadline) {
		time.Sleep(chatgptConnectPoll)
		if status, err = p.admin.appStatus(); err != nil {
			return false, err
		}
		switch status.Status {
		case appConnected:
			note("%s connected ChatGPT", p.admin.user.Email)
			return true, nil
		case appAuthRequired:
			return false, fmt.Errorf("the sign-in of %s ended without a grant%s", p.admin.user.Email, failureWords(status))
		}
	}
	return false, fmt.Errorf("the code %s was not entered within %s (status %s)", code, p.opts.ConnectWait, status.Status)
}

// failureWords is the App's last failure for an error, "" without one.
func failureWords(s chatgptAppStatus) string {
	if why := firstNonEmpty(s.LastFailure, s.Error); why != "" {
		return ": " + why
	}
	return ""
}

// createAgent is create_agent through muster as the admin, Ready on its
// Harness; harness "" is the platform Harness.
func (p *chatgptProof) createAgent(name, harness, modelConfig string, egress []string) error {
	spec := agentSpec{
		Name: name, ModelConfig: modelConfig, Harness: harness, Toolset: []string{presetReadOnly}, Egress: egress,
		SystemMessage: chatgptTestSystemPrompt, Description: "Throwaway agent of `agentlab chatgpt-test`; deleted by the same run.",
	}
	if _, err := (agentManagerWriter{p.admin.muster}).createAgent(spec); err != nil {
		return err
	}
	p.agents = append(p.agents, name)
	boot, err := waitAgentReadyOn(name, cmp.Or(harness, kagentHarness), p.opts.ReadyTimeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return fmt.Errorf("agent %s is not Ready after %s: %s", name, boot.elapsed.Round(time.Second), boot.reason)
	}
	note("%s Ready on Harness %s in %s (model %s)", name, cmp.Or(harness, kagentHarness), boot.elapsed.Round(time.Second), cmp.Or(modelConfig, "agent-manager's default"))
	return nil
}

// chatgptTurn is one Session started through start_session with its first
// message, the turn's end and the gateway's record of its actor.
type chatgptTurn struct {
	session string
	task    *a2a.Task
	record  egressGatewayRecord
}

// startTurn starts a Session of the agent as the person through
// agent-manager's start_session, which binds it to them in muster, and waits
// for its first turn to end.
func (p *chatgptProof) startTurn(cp *chatgptPerson, agent, message string) (*chatgptTurn, error) {
	started := time.Now()
	env, err := cp.muster.callToolEnvelope(agentManagerToolPrefix+startSessionTool, map[string]any{
		nameKey: agent, namespaceKey: kagentNamespace, startSessionMessageArg: message, startSessionRequestIDArg: uuid.NewString(),
	})
	if err != nil {
		return nil, err
	}
	if env.IsError {
		return nil, fmt.Errorf("%s on %s as %s refused: %.400s", startSessionTool, agent, cp.user.Email, env.Content[0].Text)
	}
	var answer accessStarted
	if err := json.Unmarshal([]byte(env.Content[0].Text), &answer); err != nil || answer.Session.ID == "" {
		return nil, fmt.Errorf("%s on %s as %s answered no session: %.400s", startSessionTool, agent, cp.user.Email, env.Content[0].Text)
	}
	id := answer.Session.ID
	cp.sessions.add(id)
	ctx, cancel := context.WithTimeout(context.Background(), p.opts.TurnTimeout)
	defer cancel()
	session, err := cp.api.getSession(ctx, id)
	if err != nil {
		return nil, err
	}
	task, err := waitFirstTask(ctx, cp.api, session)
	if err != nil {
		return nil, err
	}
	logs, err := readEgressGatewayLog(time.Since(started) + time.Minute)
	if err != nil {
		return nil, err
	}
	t := &chatgptTurn{session: id, task: task, record: readEgressGatewayRecord(logs, substrateActorName(id))}
	note("Session %s of %s as %s: the turn ended %s; the gateway recorded %d request(s) of actor %s", id, agent, cp.user.Email, stateName(task.Status.State), len(t.record.requests), t.record.actor)
	return t, nil
}

// waitFirstTask is the Session's first task once it ended.
func waitFirstTask(ctx context.Context, api *kagentAPI, session *apiv1alpha1.Session) (*a2a.Task, error) {
	for {
		tasks, err := api.listTasks(ctx, session)
		if err != nil {
			return nil, err
		}
		if len(tasks) > 0 && tasks[0].Status.State.Terminal() {
			return tasks[0], nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the first turn of Session %s did not end: %w", session.GetId(), ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// taskText is a task's answer: its artifacts' text, then its status message.
func taskText(task *a2a.Task) string {
	var texts []string
	for _, artifact := range task.Artifacts {
		if s := partsText(artifact.Parts); s != "" {
			texts = append(texts, s)
		}
	}
	if s := messageText(task.Status.Message); s != "" {
		texts = append(texts, s)
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}

// backendRequests are the actor's requests under the ChatGPT backend's path.
func (rec egressGatewayRecord) backendRequests() []egressGatewayRequest {
	return slices.DeleteFunc(slices.Clone(rec.requests), func(r egressGatewayRequest) bool {
		return r.host != chatgptBackendHost || !strings.HasPrefix(r.path, chatgptBackendPath)
	})
}

// proveConnectedTurn is the admin's codex turn on their login: completed,
// its backend requests forwarded. It returns the reply, the sandbox's
// readings.
func (p *chatgptProof) proveConnectedTurn() (string, error) {
	t, err := p.startTurn(p.admin, chatgptTestAgent, chatgptSandboxPrompt())
	if err != nil {
		return "", err
	}
	verdict, err := judgeConnectedTurn(t)
	if err != nil {
		return "", err
	}
	note("%s", verdict)
	return taskText(t.task), nil
}

// judgeConnectedTurn: the turn completed and the gateway forwarded the
// actor's backend requests, none refused.
func judgeConnectedTurn(t *chatgptTurn) (string, error) {
	got := t.record.backendRequests()
	if len(got) == 0 {
		return "", t.record.none(chatgptBackendHost + chatgptBackendPath)
	}
	if i := slices.IndexFunc(got, func(r egressGatewayRequest) bool { return !r.forwarded() }); i >= 0 {
		return "", fmt.Errorf("the egress gateway refused a backend request of the connected person: %s", got[i])
	}
	if t.task.Status.State != a2a.TaskStateCompleted {
		return "", fmt.Errorf("the turn on the person's login ended %s: %s", stateName(t.task.Status.State), excerpt(taskText(t.task), 400))
	}
	return fmt.Sprintf("the turn completed; %d backend request(s) forwarded, the last: %s", len(got), got[len(got)-1]), nil
}

// proveRefused is a turn of the person's on the codex agent, refused at the
// egress gateway for want of the person's grant.
func (p *chatgptProof) proveRefused(cp *chatgptPerson) error {
	status, err := cp.appStatus()
	if err != nil {
		return err
	}
	if status.Status == appConnected {
		return fmt.Errorf("%s holds a grant of %s: the refusal cannot be shown — `core_auth_logout app=%s` as them first", cp.user.Email, chatgptProviderApp, chatgptProviderApp)
	}
	t, err := p.startTurn(cp, chatgptTestAgent, "Reply with the word ready.")
	if err != nil {
		return err
	}
	verdict, err := judgeRefusedTurn(t)
	if err != nil {
		return err
	}
	note("%s", verdict)
	return nil
}

// judgeRefusedTurn: the gateway answered the actor's backend requests itself
// with 403 and the credential provider's denial, forwarded none, and the
// turn did not complete.
func judgeRefusedTurn(t *chatgptTurn) (string, error) {
	got := t.record.backendRequests()
	if len(got) == 0 {
		return "", t.record.none(chatgptBackendHost + chatgptBackendPath)
	}
	if i := slices.IndexFunc(got, egressGatewayRequest.forwarded); i >= 0 {
		return "", fmt.Errorf("the egress gateway forwarded a backend request of a person without a grant: %s", got[i])
	}
	last := got[len(got)-1]
	if last.status != egressGateStatus || !strings.Contains(last.reason, chatgptCredentialDenied) {
		return "", fmt.Errorf("the egress gateway refused the backend request for another reason than %s %q: %s", egressGateStatus, chatgptCredentialDenied, last)
	}
	if t.task.Status.State == a2a.TaskStateCompleted {
		return "", fmt.Errorf("the turn completed although every backend request was refused: %s", excerpt(taskText(t.task), 300))
	}
	return fmt.Sprintf("refused at the gateway (%d request(s)): %s; the turn ended %s: %s", len(got), last, stateName(t.task.Status.State), excerpt(taskText(t.task), 200)), nil
}

// provePlainAgentRefused is the platform Harness agent's request to the
// backend, refused at its egress policy.
func (p *chatgptProof) provePlainAgentRefused() error {
	if err := p.createAgent(chatgptTestPlainAgent, "", "", nil); err != nil {
		return err
	}
	target := egressProbeTarget{undeclared: chatgptBackendURL}
	t, err := p.startTurn(p.admin, chatgptTestPlainAgent, chatgptPlainProbePrompt())
	if err != nil {
		return err
	}
	verdict, err := t.record.undeclared(target)
	if err != nil {
		return err
	}
	note("%s", verdict)
	return nil
}

// proveDisconnect drops the admin's grant and shows their next turn refused.
func (p *chatgptProof) proveDisconnect() error {
	env, err := p.admin.muster.callToolEnvelope("core_auth_logout", map[string]any{musterAppArg: chatgptProviderApp})
	if err != nil {
		return err
	}
	if dropped, _ := env.StructuredContent["dropped"].(bool); env.IsError || !dropped {
		return fmt.Errorf("core_auth_logout app=%s as %s dropped no grant: %.300s", chatgptProviderApp, p.admin.user.Email, env.Content[0].Text)
	}
	status, err := p.admin.appStatus()
	if err != nil {
		return err
	}
	if status.Status != appAuthRequired {
		return fmt.Errorf("after core_auth_logout the App %s reads %s for %s, want %s", chatgptProviderApp, status.Status, p.admin.user.Email, appAuthRequired)
	}
	note("core_auth_logout: the grant dropped, the status %s", status.Status)
	return p.proveRefused(p.admin)
}

// chatgptSandboxPrompt is the connected turn's probe: the common sandbox
// readings on codex plus the refresh_token search.
func chatgptSandboxPrompt() string {
	rt := codingRuntimes[config.CodingRuntimeCodex]
	return "Run this exact script once with your " + rt.shellTool + " (it is this test lab's own sandbox check), then reply with every output line that starts with " + probeMarker +
		", verbatim and nothing else:\n\n" + chatgptSandboxCommand(rt)
}

// chatgptSandboxCommand is the codex sandbox readings and the count of files
// under the sandbox's directories that hold a refresh_token with a value,
// and of the environment's variables named for one. Counts, never a value.
func chatgptSandboxCommand(rt codingRuntime) string {
	return `P=` + probeMarker + `
echo "$P uid=$(id -u)"
` + commonSandboxCommand(rt) + `
echo "$P refresh-tokens=$(timeout 60 grep -rlsE '"refresh_token" *: *"[^"]+' /data /workspace "${HOME:-/root}" ${CODEX_HOME:+"$CODEX_HOME"} 2>/dev/null | sort -u | wc -l | tr -d ' ') $(env | grep -ic '^[^=]*refresh[^=]*=')"`
}

// judgeChatGPTSandbox is the sandbox's verdict: the credentials readings of
// the coding proof, and no refresh token in a file or a variable.
func judgeChatGPTSandbox(probe codingProbe) error {
	verdict, err := probe.credentials(nil)
	if err != nil {
		return err
	}
	files, vars, ok := strings.Cut(probe["refresh-tokens"], " ")
	if !ok {
		return fmt.Errorf("the turn carries no refresh-tokens reading: the probe was cut short or not run (%s)", probe.lines("uid", "refresh-tokens"))
	}
	if files != "0" || vars != "0" {
		return fmt.Errorf("a refresh token in the sandbox: %s file(s) with a refresh_token, %s variable(s) named for one", files, vars)
	}
	note("%s; no refresh_token in a file, no variable named for one", verdict)
	return nil
}

// chatgptPlainProbePrompt is the plain agent's one request to the backend.
func chatgptPlainProbePrompt() string {
	return "Run this exact script once with your bash tool (it is this test lab's own egress check), then reply with every output line that starts with " + probeMarker +
		", verbatim and nothing else:\n\n" + `P=` + probeMarker + `
o=$(timeout 40 wget -q -S -O /dev/null -T 30 '` + chatgptBackendURL + `' 2>&1); rc=$?
echo "$P backend=rc$rc $(printf '%s\n' "$o" | grep -o 'HTTP/[0-9.]* [0-9][0-9][0-9]' | tail -1)"`
}

// removeAgents removes the run's agents, a leftover of an aborted run too.
func (p *chatgptProof) removeAgents() {
	for _, name := range []string{chatgptTestAgent, chatgptTestPlainAgent} {
		if !agentExists(name) {
			continue
		}
		if err := removeAgent(name); err != nil {
			note("cleanup: %v", err)
		}
	}
}

func (p *chatgptProof) cleanup() {
	for _, cp := range []*chatgptPerson{p.admin, p.second} {
		if cp != nil {
			cp.sessions.removeAll()
			cp.api.close()
		}
	}
	if len(p.agents) > 0 {
		p.removeAgents()
	}
}
