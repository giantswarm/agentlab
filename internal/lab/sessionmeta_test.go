//go:build standinproof

package lab

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agentlab/internal/config"
)

// The stand-in application of the session-metadata POC: it holds a client of
// its own at the lab Dex, signs a session reference per request and talks A2A
// to klaus-gateway only. Configuration comes from the environment, set by the
// proof's runner script.
const (
	metaSessionKey = "com.example/session"
	metaAudience   = "session-meta"
	metaServer     = "session-meta"
)

// metaOrgNS is the organization namespace that holds the stand-in servers
// and the proof's agent.
func metaOrgNS(t *testing.T) string { return proofEnv(t, "META_ORG_NAMESPACE") }

func proofEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set", name)
	}
	return v
}

type metaApp struct {
	t       *testing.T
	http    *http.Client
	client  *a2aclient.Client
	tenant  string
	key     *ecdsa.PrivateKey
	token   string
	stateAt string
}

type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(req)
}

func newMetaApp(t *testing.T) *metaApp {
	t.Helper()
	hc, err := labHTTPClient(5 * time.Minute)
	require.NoError(t, err)
	app := &metaApp{t: t, http: hc, tenant: proofEnv(t, "META_TENANT"), stateAt: proofEnv(t, "META_STATE_URL")}
	app.token = app.clientCredentials()
	app.key = loadSessionKey(t, proofEnv(t, "META_SESSION_KEY_FILE"))

	cardURL := strings.TrimSuffix(proofEnv(t, "META_GATEWAY_URL"), "/") + "/.well-known/agent-card.json"
	resp, err := hc.Get(cardURL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var card a2a.AgentCard
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&card))
	app.client, err = a2aclient.NewFromCard(t.Context(), &card, a2aclient.WithJSONRPCTransport(&http.Client{
		Timeout: 10 * time.Minute, Transport: bearerRoundTripper{token: app.token, base: hc.Transport},
	}))
	require.NoError(t, err)
	return app
}

// clientCredentials obtains the application's token for klaus-gateway.
func (a *metaApp) clientCredentials() string {
	t := a.t
	secret, err := os.ReadFile(proofEnv(t, "META_APP_SECRET_FILE"))
	require.NoError(t, err)
	form := url.Values{
		grantTypeParam:    {"client_credentials"},
		consentScopeParam: {"openid profile groups audience:server:client_id:" + proofEnv(t, "META_GATEWAY_AUDIENCE")},
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, proofEnv(t, "META_TOKEN_URL"), strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(proofEnv(t, "META_APP_CLIENT_ID")), url.QueryEscape(strings.TrimSpace(string(secret))))
	resp, err := a.http.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "client_credentials: %s", body)
	var out struct {
		IDToken string `json:"id_token"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	require.NotEmpty(t, out.IDToken)
	t.Logf("app token claims: %s", claimsSummary(t, out.IDToken))
	return out.IDToken
}

func loadSessionKey(t *testing.T, path string) *ecdsa.PrivateKey {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the proof's own key file
	require.NoError(t, err)
	block, _ := pem.Decode(raw)
	require.NotNil(t, block)
	key, err := x509.ParseECPrivateKey(block.Bytes)
	require.NoError(t, err)
	return key
}

// sessionRef signs a session reference for sid, the way the application
// would for every A2A request.
func (a *metaApp) sessionRef(sid, aud string, exp time.Duration, key *ecdsa.PrivateKey) string {
	t := a.t
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{"sid": sid, "aud": aud, "exp": time.Now().Add(exp).Unix(), "jti": uuid.NewString(), claimSubject: "web-user"})
	require.NoError(t, err)
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	require.NoError(t, err)
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// send runs one turn and returns the context id and the agent's text.
func (a *metaApp) send(contextID, text string, metadata map[string]any) (string, string) {
	ctx, out, err := a.trySend(contextID, text, metadata)
	require.NoError(a.t, err)
	return ctx, out
}

// trySend is send for goroutines other than the test's.
func (a *metaApp) trySend(contextID, text string, metadata map[string]any) (string, string, error) {
	t := a.t
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.ContextID = contextID
	result, err := a.client.SendMessage(t.Context(), &a2a.SendMessageRequest{Tenant: a.tenant, Message: msg, Metadata: metadata})
	if err != nil {
		return "", "", err
	}
	task, ok := result.(*a2a.Task)
	if !ok {
		return "", "", fmt.Errorf("result = %T", result)
	}
	var out strings.Builder
	for _, artifact := range task.Artifacts {
		for _, part := range artifact.Parts {
			out.WriteString(part.Text())
		}
	}
	if task.Status.Message != nil {
		for _, part := range task.Status.Message.Parts {
			out.WriteString(part.Text())
		}
	}
	t.Logf("turn %s %s: %s", task.ContextID, task.Status.State, out.String())
	return task.ContextID, out.String(), nil
}

func (a *metaApp) state() map[string]string {
	t := a.t
	resp, err := http.Get(a.stateAt) //nolint:gosec // the proof's port-forward
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out := map[string]string{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func setPrompt(t *testing.T, format string) string {
	return fmt.Sprintf("Use muster's call_tool to call the tool x_%s_%s_set_export_config with arguments {\"format\": %q}, exactly once, then reply with the tool's raw output verbatim and nothing else.", metaOrgNS(t), metaServer, format)
}

func claimsSummary(t *testing.T, token string) string {
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var c map[string]any
	require.NoError(t, json.Unmarshal(raw, &c))
	return fmt.Sprintf("iss=%v sub=%v aud=%v azp=%v name=%v groups=%v", c["iss"], c["sub"], c["aud"], c["azp"], c["name"], c["groups"])
}

// TestSessionMetaSessions: the session reference reaches the right session's
// configuration, two conversations do not mix, and a request without it
// inherits nothing.
func TestSessionMetaSessions(t *testing.T) {
	app := newMetaApp(t)
	sidA, sidB := "web-"+uuid.NewString()[:8], "web-"+uuid.NewString()[:8]

	var wg sync.WaitGroup
	var ctxA, ctxB string
	var errA, errB error
	refA := app.sessionRef(sidA, metaAudience, 10*time.Minute, app.key)
	refB := app.sessionRef(sidB, metaAudience, 10*time.Minute, app.key)
	promptA, promptB := setPrompt(t, "json"), setPrompt(t, "xlsx")
	wg.Add(2)
	go func() {
		defer wg.Done()
		ctxA, _, errA = app.trySend("", promptA, map[string]any{metaSessionKey: refA})
	}()
	go func() {
		defer wg.Done()
		ctxB, _, errB = app.trySend("", promptB, map[string]any{metaSessionKey: refB})
	}()
	wg.Wait()
	require.NoError(t, errA)
	require.NoError(t, errB)
	state := app.state()
	require.Equal(t, "json", state[sidA])
	require.Equal(t, "xlsx", state[sidB])
	require.NotEqual(t, ctxA, ctxB)

	_, text := app.send(ctxA, setPrompt(t, "csv"), nil)
	require.Equal(t, "json", app.state()[sidA], "a request without metadata changes nothing: %s", text)
}

// TestSessionMetaRefusals: forged, expired and wrong-audience references are
// refused before any state change.
func TestSessionMetaRefusals(t *testing.T) {
	app := newMetaApp(t)
	forger, err := ecdsa.GenerateKey(app.key.Curve, rand.Reader)
	require.NoError(t, err)
	for name, tc := range map[string]struct {
		ref    func(sid string) string
		reason string
	}{
		"forged":    {func(sid string) string { return app.sessionRef(sid, metaAudience, 10*time.Minute, forger) }, "bad signature"},
		"expired":   {func(sid string) string { return app.sessionRef(sid, metaAudience, -time.Minute, app.key) }, "expired"},
		"wrong-aud": {func(sid string) string { return app.sessionRef(sid, "other-server", 10*time.Minute, app.key) }, "wrong audience"},
	} {
		t.Run(name, func(t *testing.T) {
			sid := "web-" + name + "-" + uuid.NewString()[:6]
			_, text := app.send("", setPrompt(t, "json"), map[string]any{metaSessionKey: tc.ref(sid)})
			require.Contains(t, text, "session reference refused: "+tc.reason, "the server itself refused the reference")
			_, changed := app.state()[sid]
			require.False(t, changed, "refused reference must not change state: %s", text)
		})
	}
}

const metaAgent = "session-meta-agent"

// TestSessionMetaSetup creates the proof's agent the platform way: agent-manager's
// create_agent through muster, as an organization admin, in the
// organization, its toolset the two stand-in servers.
func TestSessionMetaSetup(t *testing.T) {
	require.NoError(t, os.Chdir(proofEnv(t, "META_LAB_DIR")))
	cfg, err := config.Load()
	require.NoError(t, err)
	require.NoError(t, useClusterKubeconfig(cfg))
	admin := cfg.Users[0]
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, admin.Email, admin.Password, musterLoginScopes)
	require.NoError(t, err)
	ns := metaOrgNS(t)
	ms, err := openMusterSession(cfg, token, "session-meta-proof")
	require.NoError(t, err)
	tool := "create_agent"
	if os.Getenv("META_UPDATE") != "" {
		tool = "update_agent"
	}
	var created map[string]any
	err = ms.callServerJSON(agentManagerToolPrefix+tool, map[string]any{
		namespaceKey: ns, nameKey: metaAgent, "modelConfig": organizationModel,
		toolsetKey:      []string{"server:" + ns + "/" + metaServer, "server:" + ns + "/unrelated"},
		"description":   "Throwaway agent of the session-metadata POC.",
		"systemMessage": "You change the export configuration of the user's web session with your tools. You never see the session itself: the platform passes it to the tools.",
	}, &created)
	require.NoError(t, err)
	t.Logf("create_agent: %v", created)
	took, err := waitOrganizationAgentReady(ns, metaAgent, agentReadyTimeout)
	require.NoError(t, err)
	t.Logf("Ready in %s", took.Round(time.Second))
}

// TestSessionMetaUnrelated: a request carrying the session reference calls a
// server whose MCPServer forwards nothing; that server gets no entry, so it
// refuses, and its log shows no forwarded key.
func TestSessionMetaUnrelated(t *testing.T) {
	app := newMetaApp(t)
	sid := "web-unrelated-" + uuid.NewString()[:6]
	prompt := "Use muster's call_tool to call the tool x_" + metaOrgNS(t) + "_unrelated_get_export_config with no arguments, exactly once, then reply with the tool's raw output verbatim and nothing else."
	_, text := app.send("", prompt, map[string]any{metaSessionKey: app.sessionRef(sid, "unrelated", 10*time.Minute, app.key)})
	require.Contains(t, text, "no session reference")
}

// gatewayExchange is the exchange klaus-gateway makes (RFC 8693 at Dex as
// klaus-gateway-app, connector dex-self), run here to inspect what the
// application's identity reaches downstream.
func gatewayExchange(t *testing.T, hc *http.Client, subject string) (int, string) {
	t.Helper()
	secret, err := os.ReadFile(proofEnv(t, "META_GATEWAY_SECRET_FILE")) //nolint:gosec // G304: the proof's own secret file
	require.NoError(t, err)
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":        {subject},
		"subject_token_type":   {workspaceTokenExchangeIDToken},
		"requested_token_type": {workspaceTokenExchangeIDToken},
		"connector_id":         {"dex-self"},
		"scope":                {"openid email profile groups audience:server:client_id:agent-platform audience:server:client_id:kubernetes"},
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, proofEnv(t, "META_TOKEN_URL"), strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("klaus-gateway-app", url.QueryEscape(strings.TrimSpace(string(secret))))
	resp, err := hc.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, out.Error + ": " + out.Description
	}
	return resp.StatusCode, out.AccessToken
}

// TestSessionMetaVisibility: what the application's identity sees and may call
// through muster, the extended card it gets, and that a person's token is
// not exchangeable as an application's.
func TestSessionMetaVisibility(t *testing.T) {
	require.NoError(t, os.Chdir(proofEnv(t, "META_LAB_DIR")))
	cfg, err := config.Load()
	require.NoError(t, err)
	app := newMetaApp(t)

	status, exchanged := gatewayExchange(t, app.http, app.token)
	require.Equal(t, http.StatusOK, status, exchanged)
	t.Logf("exchanged token claims: %s", claimsSummary(t, exchanged))

	ms, err := openMusterSession(cfg, exchanged, "session-meta-visibility")
	require.NoError(t, err)
	listed, err := ms.callTool("list_tools", map[string]any{limitKey: 1000})
	require.NoError(t, err)
	servers := toolServers(t, listed)
	t.Logf("servers the stand-in application sees: %v", servers)
	require.Positive(t, servers[metaOrgNS(t)+"/"+metaServer], "the organization's server")
	platform := platformServers(servers)
	if os.Getenv("META_APP_GRANTED") != "" {
		require.Contains(t, platform, agentManagerMCPServer, "the stand-in application's group is in the viewers: it sees the platform servers")
	} else {
		require.Empty(t, platform, "the stand-in application sees nothing outside its organization")
		out, err := ms.callTool("call_tool", map[string]any{nameKey: agentManagerToolPrefix + "list_agents", argumentsKey: map[string]any{}})
		callOut, _ := json.Marshal(out)
		t.Logf("agent-manager call: err=%v out=%.400s", err, callOut)
		require.Contains(t, strings.ToLower(fmt.Sprintf("%v %s", err, callOut)), "not found", "a platform tool is not callable")
	}
	_, kubernetesVisible := servers["agentlab-mcp-kubernetes"]
	t.Logf("mcp-kubernetes visible: %v", kubernetesVisible)
	out, err := ms.callTool("call_tool", map[string]any{nameKey: "x_agentlab-mcp-kubernetes_kubernetes_list", argumentsKey: map[string]any{resourceTypeKey: "namespaces"}})
	callOut, _ := json.Marshal(out)
	t.Logf("mcp-kubernetes call: err=%v out=%.400s", err, callOut)
	require.NotContains(t, string(callOut), "kube-system", "the stand-in application must not list the cluster's namespaces")

	card, err := app.client.GetExtendedAgentCard(t.Context(), &a2a.GetExtendedAgentCardRequest{Tenant: ""})
	require.NoError(t, err)
	cardJSON, _ := json.Marshal(card)
	t.Logf("extended card: %s", cardJSON)
	require.Contains(t, string(cardJSON), proofEnv(t, "META_TENANT"))
	require.NotContains(t, strings.ToLower(string(cardJSON)), "kagent")

	_, refused, err := app.trySend("", "hello", nil)
	require.NoError(t, err, refused)
	other := *app
	other.tenant = metaOrgNS(t) + "/no-such-agent"
	_, _, err = other.trySend("", "hello", nil)
	require.Error(t, err)
	t.Logf("unknown agent: %v", err)
	require.NotContains(t, strings.ToLower(err.Error()), "kagent")

	admin := cfg.Users[0]
	person, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, admin.Email, admin.Password, musterLoginScopes)
	require.NoError(t, err)
	for _, user := range cfg.Users {
		token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
		require.NoError(t, err)
		ps, err := openMusterSession(cfg, token, "session-meta-visibility-person")
		require.NoError(t, err)
		listed, err := ps.callTool("list_tools", map[string]any{limitKey: 1000})
		require.NoError(t, err)
		seen := platformServers(toolServers(t, listed))
		t.Logf("platform servers %s sees: %v", user.Email, seen)
		require.Contains(t, seen, agentManagerMCPServer, "%s is in the viewers' groups", user.Email)
	}
	status, refusal := gatewayExchange(t, app.http, person)
	t.Logf("person token exchange: %d %s", status, refusal)
	require.NotEqual(t, http.StatusOK, status, "a person's token must not pass as an application's")
}

// toolServers reads the servers of a list_tools answer: its text content is
// the JSON listing.
func toolServers(t *testing.T, listed map[string]any) map[string]int {
	t.Helper()
	raw, err := json.Marshal(listed)
	require.NoError(t, err)
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &rpc))
	require.NotEmpty(t, rpc.Result.Content)
	var listing struct {
		Total int `json:"total"`
		Tools []struct {
			Server string `json:"server"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal([]byte(rpc.Result.Content[0].Text), &listing))
	servers := map[string]int{}
	for _, tool := range listing.Tools {
		servers[tool.Server]++
	}
	require.Len(t, listing.Tools, listing.Total, "the listing is complete")
	return servers
}

// platformServers is the servers of a listing outside every organization
// namespace: muster names an organization's server <namespace>/<name>, its
// own namespace's servers by name, and its core tools by "".
func platformServers(servers map[string]int) []string {
	var out []string
	for server := range servers {
		if server != "" && !strings.Contains(server, "/") {
			out = append(out, server)
		}
	}
	slices.Sort(out)
	return out
}

// TestSessionMetaModelInputs: no model request the agent sent during the run
// carried the session reference, a sid or the metadata key, read from the
// lab's recording proxy in front of the model endpoint. The control is the
// tool name the prompts carry: the proxy saw the requests.
func TestSessionMetaModelInputs(t *testing.T) {
	resp, err := http.Get(proofEnv(t, "META_MODEL_RECORDER_URL") + "/labproxy/requests") //nolint:gosec // the proof's port-forward
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var records []struct {
		Path    string   `json:"path"`
		Bytes   int      `json:"bytes"`
		Hits    []string `json:"hits"`
		Control bool     `json:"control"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&records))
	controls := 0
	for _, r := range records {
		require.Empty(t, r.Hits, "model request %s (%d bytes) carries a watched value", r.Path, r.Bytes)
		if r.Control {
			controls++
		}
	}
	t.Logf("model requests: %d, carrying the prompts' tool name: %d", len(records), controls)
	require.Positive(t, controls, "the recorder saw the agent's model requests")
}
