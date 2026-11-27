//go:build standinproof

package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The app-flow proof drives what the stand-in application's backend does: a
// token obtained by RFC 8693 exchange at the lab Dex for a customer who sees
// only the organization's MCP server. The lab's names come from the
// environment (APPFLOW_*), set by the proof's runner script.
const (
	appFlowAgent         = "app-flow-proof"
	appFlowMusterClient  = "app-flow-proof"
	appFlowSessionMarker = "redis:session:7f3c-demo"
	appFlowDexEndpoint   = "https://localhost:32000/dex/token"
)

func appFlowOrgNS(t *testing.T) string  { return proofEnv(t, "APPFLOW_ORG_NAMESPACE") }
func appFlowServer(t *testing.T) string { return proofEnv(t, "APPFLOW_SERVER") }

func appFlowPrompt(t *testing.T) string {
	return "Use muster's call_tool to call the tool x_" + appFlowServer(t) + "_update_export_config with arguments {\"include_prices\": true}, exactly once, then reply with the tool's raw output verbatim and nothing else."
}

func appFlowCustomer(t *testing.T, email string) *config.User {
	t.Helper()
	password, err := os.ReadFile(proofEnv(t, "APPFLOW_PASSWORD_FILE"))
	require.NoError(t, err)
	return &config.User{Email: email, Password: strings.TrimSpace(string(password))}
}

func appFlowLab(t *testing.T) *config.Config {
	t.Helper()
	require.NoError(t, os.Chdir(proofEnv(t, "APPFLOW_LAB_DIR")))
	cfg, err := config.Load()
	require.NoError(t, err)
	require.NoError(t, useClusterKubeconfig(cfg))
	return cfg
}

// TestAppFlow drives what an app channel would, with a token the app
// obtained by RFC 8693 exchange at the lab Dex, for a customer who sees only
// the organization's MCP server: the Session started as the customer, the
// one-time per-server sign-in to the customer's own IdP, the tool call with
// that IdP's token, and a second Session that needs no sign-in.
func TestAppFlow(t *testing.T) {
	cfg := appFlowLab(t)

	raw, err := os.ReadFile(proofEnv(t, "APPFLOW_TOKEN_FILE"))
	require.NoError(t, err)
	token := strings.TrimSpace(string(raw))
	claims, err := decodeJWTClaims(token)
	require.NoError(t, err)
	email, _ := claims["email"].(string)
	t.Logf("customer token: iss=%v aud=%v azp=%v email=%s groups=%v", claims["iss"], claims["aud"], claims["azp"], email, claims["groups"])
	customer := appFlowCustomer(t, email)

	admin := cfg.Users[0]
	adminToken, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, admin.Email, admin.Password, musterLoginScopes)
	require.NoError(t, err)
	createOrgAgent(t, adminToken)

	step("The customer's muster session: what it sees")
	ms, err := openMusterSession(cfg, token, appFlowMusterClient)
	require.NoError(t, err)
	listed, err := ms.callTool("list_tools", nil)
	require.NoError(t, err)
	b, _ := json.Marshal(listed)
	note("list_tools: %s", excerpt(string(b), 600))

	step("8a. start_session through muster (klaus-gateway's path today)")
	_, viaMuster := startAgentSessionOf(ms, orgAgentRef(t), uuid.NewString(), "")
	note("through muster: %v", viaMuster)

	step("8b. start_session straight at agent-manager with the customer's token")
	api, err := dialKagentAPI(cfg, token)
	require.NoError(t, err)
	defer api.close()
	am, err := openMusterSessionAt(proofEnv(t, "APPFLOW_AGENT_MANAGER_URL"), token, appFlowMusterClient)
	require.NoError(t, err)
	session := startDirect(t, am, api)

	step("9. Turn 1: the tool call pauses on the sign-in to the customer's IdP")
	turn1, err := api.turnOn(session, userMessage(appFlowPrompt(t)))
	require.NoError(t, err)
	if turn1.state() != a2a.TaskStateInputRequired || turn1.signIn == nil {
		t.Fatalf("turn 1 ended %s (%s), no sign-in: %s", stateName(turn1.state()), turn1.statesString(), excerpt(turn1.text(), 400))
	}
	note("paused on %s: %q, link %s", turn1.signIn.Server, turn1.signIn.Message, turn1.signIn.URL)
	require.NoError(t, completeSignInAt(turn1.signIn.URL, customer, oauthProxyCallbackPath))
	note("sign-in completed headless: lab Dex identity check via the stand-in application's connector, then the stand-in application's IdP")
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	accepted, err := api.answerSignIn(ctx, session, turn1.turn, signInAccept)
	cancel()
	require.NoError(t, err)
	answer := accepted.text()
	for _, tool := range accepted.tools {
		answer += "\n" + tool.output
	}
	note("after accept: %s (%s): %s", stateName(accepted.state()), accepted.statesString(), excerpt(answer, 900))
	require.Contains(t, answer, appFlowSessionMarker)

	step("9b. A new Session of the same customer: no sign-in asked")
	second := startDirect(t, am, api)
	turn2, err := api.turnOn(second, userMessage(appFlowPrompt(t)))
	require.NoError(t, err)
	text := turnText(turn2)
	note("turn: %s (%s): %s", stateName(turn2.state()), turn2.statesString(), excerpt(text, 900))
	require.Equal(t, a2a.TaskStateCompleted, turn2.state())
	require.Contains(t, text, appFlowSessionMarker)
}

func orgAgentRef(t *testing.T) *apiv1alpha1.ResourceReference {
	return &apiv1alpha1.ResourceReference{Namespace: appFlowOrgNS(t), Name: appFlowAgent}
}

func startDirect(t *testing.T, am *musterSession, api *kagentAPI) *apiv1alpha1.Session {
	t.Helper()
	res, err := am.callTool("start_session", map[string]any{nameKey: appFlowAgent, namespaceKey: appFlowOrgNS(t), startSessionRequestIDArg: uuid.NewString()})
	require.NoError(t, err)
	b, _ := json.Marshal(res)
	var env struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(b, &env))
	require.Nil(t, env.Error, "%s", b)
	require.NotEmpty(t, env.Result.Content, "%s", b)
	require.False(t, env.Result.IsError, "start_session refused: %s", env.Result.Content[0].Text)
	var started accessStarted
	require.NoError(t, json.Unmarshal([]byte(env.Result.Content[0].Text), &started), env.Result.Content[0].Text)
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	defer cancel()
	session, err := api.getSession(ctx, started.Session.ID)
	require.NoError(t, err)
	session, err = api.awaitSessionReady(ctx, session)
	require.NoError(t, err)
	note("Session %s creator %s providers %v", session.GetId(), session.GetCreator(), session.GetProviders())
	return session
}

func createOrgAgent(t *testing.T, adminToken string) {
	t.Helper()
	ns, server := appFlowOrgNS(t), appFlowServer(t)
	step("The stand-in application's agent in the organization, toolset %s", "server:"+server)
	dyn, _, err := personClients(adminToken)
	require.NoError(t, err)
	gvr, err := gvrFor(fluxHelmReleaseResource)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if _, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, appFlowAgent, metav1.GetOptions{}); err == nil {
		note("agent %s already there", appFlowAgent)
		_, err := waitOrganizationAgentReady(ns, appFlowAgent, agentReadyTimeout)
		require.NoError(t, err)
		return
	}
	source, err := organizationChartSource(ns)
	require.NoError(t, err)
	spec := agentSpec{
		Name: appFlowAgent, ModelConfig: organizationModel, Toolset: []string{"server:" + server},
		SystemMessage: "You change the signed-in customer's product data export configuration with your tools.",
		Description:   "Throwaway agent of the app-flow proof.",
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		crSpec: map[string]any{
			"interval":               helmReleaseInterval,
			podServiceAccountNameKey: kagentFluxServiceAccount,
			"chartRef":               map[string]any{kindKey: kindOCIRepository, nameKey: source, namespaceKey: ns},
			"values":                 agentValues(spec),
		},
	}}
	u.SetAPIVersion(fluxHelmReleaseAPIVersion)
	u.SetKind(kindHelmRelease)
	u.SetNamespace(ns)
	u.SetName(appFlowAgent)
	_, err = dyn.Resource(gvr).Namespace(ns).Create(ctx, u, metav1.CreateOptions{})
	require.NoError(t, err)
	took, err := waitOrganizationAgentReady(ns, appFlowAgent, agentReadyTimeout)
	require.NoError(t, err, fmt.Sprintf("agent %s not Ready", appFlowAgent))
	note("Ready in %s", took.Round(time.Second))
}

// appFlowExchange is what the stand-in application's backend does before
// calling the platform: RFC 8693 at the lab Dex, its own IdP's token for a
// Dex ID token.
func appFlowExchange(t *testing.T) string {
	t.Helper()
	read := func(env string) string {
		b, err := os.ReadFile(proofEnv(t, env))
		require.NoError(t, err)
		return strings.TrimSpace(string(b))
	}
	client, err := labHTTPClient(30 * time.Second)
	require.NoError(t, err)
	form := url.Values{
		grantTypeParam:         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"connector_id":         {proofEnv(t, "APPFLOW_CONNECTOR")},
		"subject_token":        {read("APPFLOW_SUBJECT_TOKEN_FILE")},
		"subject_token_type":   {workspaceTokenExchangeIDToken},
		"requested_token_type": {workspaceTokenExchangeIDToken},
		consentScopeParam:      {"openid email profile audience:server:client_id:agent-platform audience:server:client_id:kubernetes"},
	}
	req, err := http.NewRequest(http.MethodPost, appFlowDexEndpoint, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(proofEnv(t, "APPFLOW_APP_CLIENT_ID"), read("APPFLOW_APP_SECRET_FILE"))
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.NotEmpty(t, out.AccessToken, "exchange refused: %s", out.Error)
	return out.AccessToken
}

func jwtExpiry(t *testing.T, token string) time.Time {
	t.Helper()
	claims, err := decodeJWTClaims(token)
	require.NoError(t, err)
	exp, _ := claims["exp"].(float64)
	return time.Unix(int64(exp), 0)
}

func turnText(tt *timedTurn) string {
	text := tt.text()
	for _, tool := range tt.tools {
		text += "\n" + tool.output
	}
	return text
}

// TestAppFlowTokenRefresh: a Session keeps working across the expiry of the
// customer's token once the app forwards a fresh one to muster, and only then.
func TestAppFlowTokenRefresh(t *testing.T) {
	cfg := appFlowLab(t)

	tokenA := appFlowExchange(t)
	expA := jwtExpiry(t, tokenA)
	step("token A expires at %s (in %s)", expA.Format(time.TimeOnly), time.Until(expA).Round(time.Second))
	_, err := openMusterSession(cfg, tokenA, appFlowMusterClient)
	require.NoError(t, err)
	api, err := dialKagentAPI(cfg, tokenA)
	require.NoError(t, err)
	am, err := openMusterSessionAt(proofEnv(t, "APPFLOW_AGENT_MANAGER_URL"), tokenA, appFlowMusterClient)
	require.NoError(t, err)
	session := startDirect(t, am, api)
	api.close()
	first, err := func() (*timedTurn, error) {
		a, err := dialKagentAPI(cfg, tokenA)
		if err != nil {
			return nil, err
		}
		defer a.close()
		return a.turnOn(session, userMessage(appFlowPrompt(t)))
	}()
	require.NoError(t, err)
	note("turn with A: %s: %s", stateName(first.state()), excerpt(turnText(first), 300))
	require.Contains(t, turnText(first), appFlowSessionMarker)

	wait := time.Until(expA) + 30*time.Second
	step("waiting %s for token A to expire", wait.Round(time.Second))
	time.Sleep(wait)

	tokenB := appFlowExchange(t)
	require.True(t, jwtExpiry(t, tokenB).After(time.Now()))
	apiB, err := dialKagentAPI(cfg, tokenB)
	require.NoError(t, err)
	defer apiB.close()

	step("turn after A expired, before the app forwards B to muster")
	stale, err := apiB.turnOn(session, userMessage(appFlowPrompt(t)))
	require.NoError(t, err)
	note("stale turn: %s: %s", stateName(stale.state()), excerpt(turnText(stale), 500))

	step("the app forwards B to muster (a muster request with B), then the same Session")
	_, err = openMusterSession(cfg, tokenB, appFlowMusterClient)
	require.NoError(t, err)
	fresh, err := apiB.turnOn(session, userMessage(appFlowPrompt(t)))
	require.NoError(t, err)
	note("turn after B: %s: %s", stateName(fresh.state()), excerpt(turnText(fresh), 300))
	require.Contains(t, turnText(fresh), appFlowSessionMarker)
	require.NotContains(t, turnText(stale), appFlowSessionMarker, "the stale turn reached the stand-in application's server: muster used an expired token or needs none")
}

// TestAppFlowOnboardingSignIn: the app signs the customer in to the stand-in
// application's server on the customer's own muster session before any chat;
// the first turn of a new Session then needs no sign-in.
func TestAppFlowOnboardingSignIn(t *testing.T) {
	cfg := appFlowLab(t)
	token := appFlowExchange(t)
	customer := appFlowCustomer(t, proofEnv(t, "APPFLOW_CUSTOMER_EMAIL"))
	server := appFlowServer(t)

	step("sign the customer out of %s (ends the subject grant)", server)
	require.NoError(t, signOutOfServer(cfg, server, token))

	step("onboarding: core_auth_login on the customer's muster session, URL elicitation")
	ctx, cancel := context.WithTimeout(context.Background(), musterMCPTimeout)
	defer cancel()
	m, err := dialMusterMCP(ctx, cfg, token, musterMCPOptions{name: "app-flow-onboarding", protocol: mcp.ProtocolVersion20251125, urlElicitation: true})
	require.NoError(t, err)
	defer m.close()
	answer, err := m.call(ctx, "core_auth_login", map[string]any{serverKey: server}, mcp.MultiRoundTripParams{})
	require.NoError(t, err)
	e, err := oneURLElicitation(answer)
	require.NoError(t, err)
	note("elicitation: %q %s", e.Message, e.URL)
	require.NoError(t, completeSignInAt(e.URL, customer, oauthProxyCallbackPath))
	note("sign-in completed headless")

	step("a new Session's first turn: no pause")
	api, err := dialKagentAPI(cfg, token)
	require.NoError(t, err)
	defer api.close()
	am, err := openMusterSessionAt(proofEnv(t, "APPFLOW_AGENT_MANAGER_URL"), token, appFlowMusterClient)
	require.NoError(t, err)
	session := startDirect(t, am, api)
	tt, err := api.turnOn(session, userMessage(appFlowPrompt(t)))
	require.NoError(t, err)
	note("turn: %s (%s): %s", stateName(tt.state()), tt.statesString(), excerpt(turnText(tt), 300))
	require.Equal(t, a2a.TaskStateCompleted, tt.state())
	require.Contains(t, turnText(tt), appFlowSessionMarker)
}

// TestAppFlowTokenRefreshAhead: the app forwards a fresh token before the old
// one expires (as a background refresher would), and the Session keeps
// working past the old token's expiry.
func TestAppFlowTokenRefreshAhead(t *testing.T) {
	cfg := appFlowLab(t)

	tokenA := appFlowExchange(t)
	expA := jwtExpiry(t, tokenA)
	_, err := openMusterSession(cfg, tokenA, appFlowMusterClient)
	require.NoError(t, err)
	api, err := dialKagentAPI(cfg, tokenA)
	require.NoError(t, err)
	am, err := openMusterSessionAt(proofEnv(t, "APPFLOW_AGENT_MANAGER_URL"), tokenA, appFlowMusterClient)
	require.NoError(t, err)
	session := startDirect(t, am, api)
	first, err := api.turnOn(session, userMessage(appFlowPrompt(t)))
	require.NoError(t, err)
	api.close()
	require.Contains(t, turnText(first), appFlowSessionMarker)
	note("turn with A completed; A expires at %s", expA.Format(time.TimeOnly))

	ahead := time.Until(expA) - time.Minute
	step("waiting %s, then forwarding B a minute before A expires", ahead.Round(time.Second))
	time.Sleep(ahead)
	tokenB := appFlowExchange(t)
	_, err = openMusterSession(cfg, tokenB, appFlowMusterClient)
	require.NoError(t, err)
	note("B forwarded; B expires at %s", jwtExpiry(t, tokenB).Format(time.TimeOnly))

	wait := time.Until(expA) + 30*time.Second
	step("waiting %s, past A's expiry", wait.Round(time.Second))
	time.Sleep(wait)
	apiB, err := dialKagentAPI(cfg, tokenB)
	require.NoError(t, err)
	defer apiB.close()
	again := "Now call x_" + appFlowServer(t) + "_update_export_config again through muster's call_tool, this time with arguments {\"include_prices\": false}. This is a new call, required now. Reply with that new call's raw output verbatim."
	after, err := apiB.turnOn(session, userMessage(again))
	require.NoError(t, err)
	note("turn after A expired, B forwarded ahead: %s: %s", stateName(after.state()), excerpt(turnText(after), 600))
	require.Contains(t, turnText(after), appFlowSessionMarker)
	require.Contains(t, turnText(after), "false")
}

const keycloakUsernameParam = "username"

var kcFormAction = regexp.MustCompile(`id="kc-form-login"[^>]*action="([^"]+)"`)

// appFlowBrowse follows a sign-in link like a browser: Dex login forms (the
// identity check through the stand-in application's connector) and
// Keycloak's login form (the server's own authorization server), until
// muster's callback succeeds.
func appFlowBrowse(t *testing.T, link string, user *config.User, callbackPath string) {
	t.Helper()
	transport, err := labTLSTransport()
	require.NoError(t, err)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	browser := &http.Client{Jar: jar, Transport: transport, Timeout: 60 * time.Second}
	resp, err := browser.Get(link)
	require.NoError(t, err)
	for i := 0; i < 8; i++ {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		here := resp.Request.URL
		note("browser at %s://%s%s (%d)", here.Scheme, here.Host, here.Path, resp.StatusCode)
		switch {
		case here.Path == callbackPath && signInCompleted(body):
			return
		case strings.Contains(here.Path, "/auth/local"):
			resp, err = browser.PostForm(here.String(), url.Values{"login": {user.Email}, passwordParam: {user.Password}})
		case kcFormAction.Match(body):
			action := html.UnescapeString(string(kcFormAction.FindSubmatch(body)[1]))
			resp, err = browser.PostForm(action, url.Values{keycloakUsernameParam: {user.Email}, passwordParam: {user.Password}, "credentialId": {""}})
		default:
			t.Fatalf("sign-in stuck at %s (%d): %s", here, resp.StatusCode, excerpt(string(body), 500))
		}
		require.NoError(t, err)
	}
	t.Fatalf("sign-in did not end on %s", callbackPath)
}

// TestAppFlowOrgDCR: the stand-in application's MCP server in its
// organization, its own authorization server (Keycloak) found through RFC
// 9728, muster registered there by RFC 7591 with no Secret and no muster
// RBAC, the customer seeing only the organization's servers.
func TestAppFlowOrgDCR(t *testing.T) {
	cfg := appFlowLab(t)
	token := appFlowExchange(t)
	customer := appFlowCustomer(t, proofEnv(t, "APPFLOW_CUSTOMER_EMAIL"))
	ns := appFlowOrgNS(t)
	tool := "x_" + ns + "_" + proofEnv(t, "APPFLOW_DCR_SERVER") + "_update_export_config"

	step("the customer's muster session sees no platform server")
	ms, err := openMusterSession(cfg, token, appFlowMusterClient)
	require.NoError(t, err)
	_, viaMuster := startAgentSessionOf(ms, orgAgentRef(t), uuid.NewString(), "")
	require.Error(t, viaMuster)
	note("start_session through muster: %v", viaMuster)

	api, err := dialKagentAPI(cfg, token)
	require.NoError(t, err)
	defer api.close()
	am, err := openMusterSessionAt(proofEnv(t, "APPFLOW_AGENT_MANAGER_URL"), token, appFlowMusterClient)
	require.NoError(t, err)
	session := startDirect(t, am, api)

	prompt := "Use muster's call_tool to call the tool " + tool + " with arguments {\"include_prices\": true}, exactly once, then reply with the tool's raw output verbatim and nothing else."
	step("turn 1 pauses on the sign-in to the stand-in application's own authorization server")
	t1, err := api.turnOn(session, userMessage(prompt))
	require.NoError(t, err)
	if t1.state() != a2a.TaskStateInputRequired || t1.signIn == nil {
		t.Fatalf("turn 1 ended %s (%s), no sign-in: %s", stateName(t1.state()), t1.statesString(), excerpt(turnText(t1), 600))
	}
	note("paused on %s: %q %s", t1.signIn.Server, t1.signIn.Message, t1.signIn.URL)
	appFlowBrowse(t, t1.signIn.URL, customer, "/oauth/ns/"+ns+"/callback")
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	accepted, err := api.answerSignIn(ctx, session, t1.turn, signInAccept)
	cancel()
	require.NoError(t, err)
	out := accepted.text()
	for _, tl := range accepted.tools {
		out += "\n" + tl.output
	}
	note("after accept: %s: %s", stateName(accepted.state()), excerpt(out, 900))
	require.Contains(t, out, appFlowSessionMarker)
}
