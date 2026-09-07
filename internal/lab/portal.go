package lab

import (
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
// backstage-muster-authorization), the Kubernetes proxy the MCP servers page
// lists CRs through, and the scaffolder the agent create flow deploys with.
type portalSession struct {
	cfg        *config.Config
	user       *config.User
	client     *http.Client
	bsToken    string
	dexIDToken string
	identity   struct {
		UserEntityRef       string   `json:"userEntityRef"`
		OwnershipEntityRefs []string `json:"ownershipEntityRefs"`
	}
	claims map[string]any
}

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
	ps := &portalSession{cfg: cfg, user: user, client: follow,
		bsToken: auth.Response.BackstageIdentity.Token, dexIDToken: auth.Response.ProviderInfo.IDToken}
	ps.identity = auth.Response.BackstageIdentity.Identity
	if ps.claims, err = decodeJWTClaims(ps.dexIDToken); err != nil {
		return nil, err
	}
	return ps, nil
}

// musterGet is the muster plugin's read hop: the backend promotes the Dex
// id_token from backstage-muster-authorization to Authorization: Bearer on
// its MCP session to muster. Returns the status and the JSON payload (or the
// raw text when the answer is not JSON / not 200).
func (ps *portalSession) musterGet(path string) (int, any, error) {
	req, err := http.NewRequest(http.MethodGet, ps.cfg.BackstageBaseURL()+"/api/muster"+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+ps.bsToken)
	req.Header.Set("backstage-muster-authorization", ps.dexIDToken)
	resp, err := ps.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, strings.TrimSpace(string(raw)), nil
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return resp.StatusCode, strings.TrimSpace(string(raw)), nil
	}
	return resp.StatusCode, payload, nil
}

// musterPost is the mutation shape of the same hop: a JSON body, the raw
// answer back (the routes normalise muster's tool results themselves).
func (ps *portalSession) musterPost(path string, body any) (int, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, ps.cfg.BackstageBaseURL()+"/api/muster"+path, strings.NewReader(string(payload)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+ps.bsToken)
	req.Header.Set("backstage-muster-authorization", ps.dexIDToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ps.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

// kubeProxyGet reads a Kubernetes API path through Backstage's Kubernetes
// backend the way the muster plugin's useResources does: the installation in
// Backstage-Kubernetes-Cluster and the user's own Dex id_token in the
// provider-specific authorization header (authProvider oidc, oidcTokenProvider
// oidc-agent-platform in the umbrella's app-config), so the apiserver sees
// the person, not Backstage.
func (ps *portalSession) kubeProxyGet(path string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, ps.cfg.BackstageBaseURL()+"/api/kubernetes/proxy"+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+ps.bsToken)
	req.Header.Set("Backstage-Kubernetes-Cluster", platformRelease)
	req.Header.Set("Backstage-Kubernetes-Authorization-oidc-oidc-agent-platform", "Bearer "+ps.dexIDToken)
	resp, err := ps.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

// backstageGet is a plain authenticated read of a Backstage backend path.
func (ps *portalSession) backstageGet(path string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, ps.cfg.BackstageBaseURL()+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+ps.bsToken)
	resp, err := ps.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

// backstagePostJSON is a plain authenticated JSON POST to a Backstage
// backend path.
func (ps *portalSession) backstagePostJSON(path string, body any) (int, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, ps.cfg.BackstageBaseURL()+path, strings.NewReader(string(payload)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+ps.bsToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ps.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

// installationQuery is the `?installation=` parameter every muster-backend
// route takes. The umbrella's app-config names the muster installation after
// the Helm release, not the kind cluster.
const installationQuery = "?installation=" + platformRelease

// listMCPServerCRs reads the MCPServer CRs the way the portal's MCP servers
// page does (useResources(MCPServer) over the Kubernetes proxy), as the user.
func (ps *portalSession) listMCPServerCRs() ([]mcpServerCR, error) {
	status, raw, err := ps.kubeProxyGet("/apis/muster.giantswarm.io/v1alpha1/mcpservers")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("Kubernetes proxy list of MCPServers answered %d: %.300s", status, raw)
	}
	return decodeMCPServerList(raw)
}
