package lab

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// portalSession is one user's signed-in Backstage session, driven headlessly
// (backstageLogin): the Backstage identity token and the Dex id_token the
// portal minted for the installation, plus the calls the portal's plugins
// make with them — the muster backend (`/api/muster/*`, the Dex token in
// backstage-muster-authorization), the Kubernetes proxy the MCP servers and
// agents pages list CRs through, and the agent-platform backend's kagent
// routes (`/api/agent-platform/kagent/*`, the Dex token in
// backstage-kagent-authorization).
type portalSession struct {
	cfg  *config.Config
	user *config.User
	// base is Backstage's public URL every request goes to (cfg's, or a test
	// server's).
	base       string
	client     *http.Client
	bsToken    string
	dexIDToken string
	identity   struct {
		UserEntityRef       string   `json:"userEntityRef"`
		OwnershipEntityRefs []string `json:"ownershipEntityRefs"`
	}
	claims map[string]any
}

// The headers the portal's plugins put the user's Dex id_token in for their
// backends: the muster plugin's, the agent-platform plugin's
// (KAGENT_AUTH_HEADER), and the Kubernetes plugin's provider-specific one
// (authProvider oidc, oidcTokenProvider oidc-agent-platform in the umbrella's
// app-config) — plus the cluster header that names the installation.
const (
	portalMusterAuthHeader  = "backstage-muster-authorization"
	portalKagentAuthHeader  = "backstage-kagent-authorization"
	portalKubeAuthHeader    = "Backstage-Kubernetes-Authorization-oidc-oidc-agent-platform"
	portalKubeClusterHeader = "Backstage-Kubernetes-Cluster"
)

// The portal backends' route prefixes.
const (
	portalMusterAPI    = "/api/muster"
	portalKubeProxyAPI = "/api/kubernetes/proxy"
	portalKagentAPI    = "/api/agent-platform/kagent"
	portalSkillsPath   = "/api/gs/agent-skills"
)

// installationQuery is the `?installation=` parameter every muster-backend
// and kagent route takes. The umbrella's app-config names the installation
// after the Helm release, not the kind cluster.
const installationQuery = "?installation=" + platformRelease

// backstageLogin drives the full Backstage <-> Dex sign-in for a user and
// returns the session. The steps are the browser's: /start redirects to Dex,
// the login form is submitted, Dex sends the browser back through Backstage's
// handler, whose page hands the authorization result to the opener via
// postMessage — headlessly, the payload is parsed out of that inline script
// (there is no API returning it cleanly; inherent to the job).
func backstageLogin(cfg *config.Config, user *config.User) (*portalSession, error) {
	transport, err := labTLSTransport()
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	follow := &http.Client{Jar: jar, Transport: transport, Timeout: 60 * time.Second}
	noFollow := &http.Client{Jar: jar, Transport: transport, Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// 1. Backstage redirects to Dex and sets its own session cookie. The
	//    provider name (oidc-agent-platform) and the auth.environment
	//    (production) both come from the umbrella's app-config and must match,
	//    or this 404s. The scope list is passed explicitly because only the
	//    BROWSER app applies plugins/gs/src/apis/auth/scopes.ts (BASE_SCOPES +
	//    gs.auth.extraScopes); hitting /start directly would otherwise get a
	//    bare token with no groups and aud=["agent-platform"] alone.
	scope := "openid profile email groups offline_access" +
		" audience:server:client_id:" + config.KubernetesClientID +
		" audience:server:client_id:dex-k8s-authenticator"
	startURL := cfg.BackstageBaseURL() + "/api/auth/oidc-agent-platform/start?env=production&scope=" +
		url.QueryEscape(scope)
	resp, err := noFollow.Get(startURL)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	dexURL := resp.Header.Get("Location")
	if dexURL == "" {
		return nil, fmt.Errorf("no redirect to Dex (status %d)", resp.StatusCode)
	}

	// 2. Follow to Dex's login form.
	resp, err = follow.Get(dexURL)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	loginURL := resp.Request.URL.String()

	// 3. Submit lab credentials; Dex redirects back through the Backstage
	//    handler, whose response embeds the authorization result.
	form := url.Values{"login": {user.Email}, passwordParam: {user.Password}}
	resp, err = follow.PostForm(loginURL, form)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// 4. The handler page hands the result to the opener via postMessage.
	m := handlerPayloadRe.FindSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("could not parse the handler response")
	}
	// PathUnescape, not QueryUnescape: '+' inside the JSON payload (JWTs,
	// base64) must survive, matching Python's urllib.parse.unquote.
	decoded, err := url.PathUnescape(string(m[1]))
	if err != nil {
		return nil, err
	}
	var probe struct {
		Type     string         `json:"type"`
		Response map[string]any `json:"response"`
	}
	if err := json.Unmarshal([]byte(decoded), &probe); err != nil {
		return nil, fmt.Errorf("parsing authorization response: %w", err)
	}
	if probe.Type != "authorization_response" {
		return nil, fmt.Errorf("unexpected response type %q", probe.Type)
	}
	if errVal, ok := probe.Response["error"]; ok {
		raw, _ := json.Marshal(errVal)
		return nil, fmt.Errorf("SIGN-IN FAILED: %.400s", string(raw))
	}
	var auth struct {
		Response struct {
			ProviderInfo struct {
				IDToken string `json:"idToken"`
			} `json:"providerInfo"`
			BackstageIdentity struct {
				Token    string `json:"token"`
				Identity struct {
					UserEntityRef       string   `json:"userEntityRef"`
					OwnershipEntityRefs []string `json:"ownershipEntityRefs"`
				} `json:"identity"`
			} `json:"backstageIdentity"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(decoded), &auth); err != nil {
		return nil, err
	}
	ps := &portalSession{cfg: cfg, user: user, base: cfg.BackstageBaseURL(), client: follow,
		bsToken: auth.Response.BackstageIdentity.Token, dexIDToken: auth.Response.ProviderInfo.IDToken}
	ps.identity = auth.Response.BackstageIdentity.Identity
	if ps.claims, err = decodeJWTClaims(ps.dexIDToken); err != nil {
		return nil, err
	}
	return ps, nil
}

// request is one authenticated call to a Backstage backend path as the user:
// the Backstage identity token, the given extra headers, the body as JSON
// when one is given. Returns the status and the raw answer.
func (ps *portalSession) request(method, path string, body any, headers map[string]string) (int, []byte, error) {
	resp, err := ps.open(ps.client, method, path, body, headers)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

// open sends the request and hands the response back unread — for the
// streaming route; every other caller goes through request. client is the
// session's unless the caller needs another bound (a turn outlives the
// session client's timeout, which covers the whole exchange).
func (ps *portalSession) open(client *http.Client, method, path string, body any, headers map[string]string) (*http.Response, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, ps.base+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+ps.bsToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return client.Do(req)
}

// musterHeaders is the muster plugin's hop: the backend promotes the Dex
// id_token from backstage-muster-authorization to Authorization: Bearer on
// its MCP session to muster.
func (ps *portalSession) musterHeaders() map[string]string {
	return map[string]string{portalMusterAuthHeader: ps.dexIDToken}
}

// musterGet is the muster plugin's read hop. Returns the status and the JSON
// payload (or the raw text when the answer is not JSON / not 200).
func (ps *portalSession) musterGet(path string) (int, any, error) {
	status, raw, err := ps.request(http.MethodGet, portalMusterAPI+path, nil, ps.musterHeaders())
	if err != nil {
		return 0, nil, err
	}
	var payload any
	if status != http.StatusOK || json.Unmarshal(raw, &payload) != nil {
		return status, strings.TrimSpace(string(raw)), nil
	}
	return status, payload, nil
}

// musterPost is the mutation shape of the same hop: a JSON body, the raw
// answer back (the routes normalise muster's tool results themselves).
func (ps *portalSession) musterPost(path string, body any) (int, []byte, error) {
	return ps.request(http.MethodPost, portalMusterAPI+path, body, ps.musterHeaders())
}

// kubeProxyGet reads a Kubernetes API path through Backstage's Kubernetes
// backend the way the plugins' useResources does: the installation in
// Backstage-Kubernetes-Cluster and the user's own Dex id_token in the
// provider-specific authorization header, so the apiserver sees the person,
// not Backstage.
func (ps *portalSession) kubeProxyGet(path string) (int, []byte, error) {
	// The raw token: the backend's OIDC strategy prefixes "Bearer " itself.
	return ps.request(http.MethodGet, portalKubeProxyAPI+path, nil, map[string]string{
		portalKubeClusterHeader: platformRelease, portalKubeAuthHeader: ps.dexIDToken,
	})
}

// backstageGet is a plain authenticated read of a Backstage backend path.
func (ps *portalSession) backstageGet(path string) (int, []byte, error) {
	return ps.request(http.MethodGet, path, nil, nil)
}

// kagentHeaders is the agent-platform plugin's hop: the user's Dex id_token
// in backstage-kagent-authorization, which the backend forwards to the
// controller as the person's bearer.
func (ps *portalSession) kagentHeaders() map[string]string {
	return map[string]string{portalKagentAuthHeader: ps.dexIDToken}
}

// kagentPath is a kagent route with the installation appended the way the
// plugin's client builds it.
func kagentPath(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return portalKagentAPI + path + separator + strings.TrimPrefix(installationQuery, "?")
}

// kagentRequest is one call to the portal's agent-platform backend as the
// user: the Backstage identity token, the installation, and the user's Dex
// id_token in the header the backend promotes to kagent.
func (ps *portalSession) kagentRequest(method, path string, body any) (int, []byte, error) {
	return ps.request(method, kagentPath(path), body, ps.kagentHeaders())
}

// kagentJSON is kagentRequest with the answer decoded into out when the
// status is a 2xx and out is given; the status and the raw answer come back
// either way, so callers judge a 404 or a 409 by name.
func (ps *portalSession) kagentJSON(method, path string, body, out any) (int, []byte, error) {
	status, raw, err := ps.kagentRequest(method, path, body)
	if err != nil {
		return 0, nil, err
	}
	if out != nil && status/100 == 2 && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return status, raw, fmt.Errorf("%s %s: not the expected JSON: %w\n%.300s", method, path, err, raw)
		}
	}
	return status, raw, nil
}

// kagentStream opens the portal's streaming route (POST …/messages/stream)
// and hands every SSE data frame to onFrame as it arrives — the relay
// flushes per event, so the frames come as kagent produces them — until the
// stream ends or onFrame returns false. A status other than 200 is the
// route's refusal, returned with its body. The stream is bounded by the
// turn timeout, not the session client's (which covers the whole exchange).
func (ps *portalSession) kagentStream(path string, body any, onFrame func(streamFrame) bool) error {
	streaming := *ps.client
	streaming.Timeout = portalTurnTimeout
	resp, err := ps.open(&streaming, http.MethodPost, kagentPath(path), body, ps.kagentHeaders())
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s answered %d: %.300s", path, resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s answered %q, not an event stream: %.300s", path, ct, raw)
	}
	return readSSE(resp.Body, onFrame)
}

// readSSE reads server-sent events off r: the `data:` lines of one event
// (joined by newlines, a blank line ends the event) decode into a
// streamFrame handed to onFrame; comment and other field lines are skipped.
func readSSE(r io.Reader, onFrame func(streamFrame) bool) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var data []string
	flush := func() (bool, error) {
		if len(data) == 0 {
			return true, nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		var frame streamFrame
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			return false, fmt.Errorf("an SSE frame is not JSON: %w\n%.300s", err, payload)
		}
		return onFrame(frame), nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if more, err := flush(); err != nil || !more {
				return err
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading the event stream: %w", err)
	}
	_, err := flush()
	return err
}

// listMCPServerCRs reads the MCPServer CRs the way the portal's MCP servers
// page does (useResources(MCPServer) over the Kubernetes proxy), as the user.
func (ps *portalSession) listMCPServerCRs() ([]mcpServerCR, error) {
	status, raw, err := ps.kubeProxyGet("/apis/muster.giantswarm.io/v1alpha1/mcpservers")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("the Kubernetes proxy list of MCPServers answered %d: %.300s", status, raw)
	}
	return decodeMCPServerList(raw)
}
