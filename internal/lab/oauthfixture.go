package lab

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The per-server OAuth sign-in fixture.
//
// Every downstream the lab aggregates (mcp-kubernetes, mcp-prometheus,
// model-manager) is unauthenticated in-cluster, so nothing exercises muster's
// OAuth *client* role: the proxy behind core_auth_login and the portal's
// per-server "Sign in" button, which hands the user a challenge URL, walks the
// browser through the downstream's authorization server and keeps the token
// per session. The lab turns that role on (muster.muster.oauth.mcpClient in
// agent-platform-values.yaml.tmpl — the umbrella leaves it off, real
// installations turn it on) and ships one downstream that declares auth.type
// oauth and stays Auth Required, so the path can be driven and proven
// headlessly. Why the fixture targets muster's own protected endpoint is
// explained in templates/oauth-fixture.yaml.tmpl.

const (
	// oauthFixtureServer is the fixture MCPServer's name — what the proofs
	// sign in to and what the portal lists.
	oauthFixtureServer = "lab-oauth-fixture"
	// oauthFixtureURL is muster's own protected /mcp, in-cluster (the muster
	// Service; the pod is hostNetwork, so the Service forwards to the node).
	oauthFixtureURL = "http://" + componentMuster + "." + platformNamespace + ".svc.cluster.local:8090/mcp"
	// oauthProxyStartPath is muster's OAuth proxy start endpoint, the path
	// every sign-in challenge points the browser at (muster's
	// DefaultOAuthProxyStartPath; the chart renders no override).
	oauthProxyStartPath = "/oauth/proxy/start"
	// mcpServerStateAuthRequired is the CRD status.state of a reachable remote
	// server that answered 401 — muster's api.StateAuthRequired, spelled the
	// CRD way.
	mcpServerStateAuthRequired = "Auth Required"
)

// ensureOAuthFixture applies the fixture and waits until muster reports it
// Auth Required. After the umbrella install on purpose: the MCPServer CRD
// ships with muster. On a fresh install the first dial 401s at once; on a
// re-run the CR already exists and the new muster pod's startup dial found no
// listener yet (Failed), so the wait spans muster's reconnect backoff — about
// a minute, measured.
func ensureOAuthFixture(cfg *config.Config) error {
	step("Creating the OAuth sign-in fixture (MCPServer %s)", oauthFixtureServer)
	_, path, err := renderManifest(cfg, "oauth-fixture.yaml.tmpl")
	if err != nil {
		return err
	}
	if err := runQuiet("kubectl", "apply", "-f", path); err != nil {
		return err
	}
	return waitMCPServerState(oauthFixtureServer, mcpServerStateAuthRequired)
}

// isAuthRequiredState accepts both spellings of muster's auth-required state:
// the CRD's "Auth Required" and the service-state token "auth_required".
func isAuthRequiredState(state string) bool {
	return strings.EqualFold(strings.ReplaceAll(state, "_", " "), mcpServerStateAuthRequired)
}

// authChallenge is what core_auth_login answers for a server the session is
// not signed in to: the prose plus the machine-readable sign-in URL (the
// portal reads structuredContent.authUrl, never the text).
type authChallenge struct {
	authURL        string
	state          string
	clientIDMethod string
}

// signInChallenge runs core_auth_login for the fixture on the session and
// parses the challenge, asserting the sign-in URL is muster's OAuth proxy
// start endpoint on the public URL, carrying a state.
func signInChallenge(cfg *config.Config, s *musterSession) (*authChallenge, error) {
	// core_auth_login is a core tool: reachable only through muster's
	// call_tool meta-tool, whose envelope carries the challenge's
	// structuredContent — the same envelope the portal's backend unwraps.
	env, err := s.callToolEnvelope("core_auth_login", map[string]any{"server": oauthFixtureServer})
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(env.Content[0].Text)
	if env.IsError {
		return nil, fmt.Errorf("core_auth_login refused: %s", text)
	}
	authURL, _ := env.StructuredContent["authUrl"].(string)
	if authURL == "" {
		return nil, fmt.Errorf("core_auth_login answered no structuredContent.authUrl — not a challenge:\n%s", text)
	}
	wantPrefix := cfg.MusterBaseURL() + oauthProxyStartPath + "?state="
	if !strings.HasPrefix(authURL, wantPrefix) {
		return nil, fmt.Errorf("sign-in URL %q is not muster's proxy start endpoint (want %s…)", authURL, wantPrefix)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		return nil, fmt.Errorf("sign-in URL %q: %w", authURL, err)
	}
	state := u.Query().Get("state")
	if state == "" {
		return nil, fmt.Errorf("sign-in URL %q carries an empty state", authURL)
	}
	method, _ := env.StructuredContent["clientIdMethod"].(string)
	return &authChallenge{authURL: authURL, state: state, clientIDMethod: method}, nil
}

// proveOAuthSignIn is the headless per-server sign-in proof, on a fresh MCP
// session (the shape of one portal user's session): list_tools flags the
// fixture as requiring auth; core_auth_login answers a challenge whose URL is
// muster's OAuth proxy start endpoint with a state; a second call answers a
// fresh state (every Sign in click gets its own challenge, backstage#2203);
// and the URL is redeemable — GET-ing it redirects the browser on to the
// authorization server (muster's own /oauth/authorize here) instead of
// rejecting the state.
func proveOAuthSignIn(cfg *config.Config, token string) error {
	step("Per-server OAuth sign-in: core_auth_login for the %s fixture", oauthFixtureServer)
	// The challenge itself does not depend on the CR state, but right after a
	// muster restart the CR reads Failed until muster's retry finds its own
	// listener — and a Failed fixture is what the portal would show.
	if err := waitMCPServerState(oauthFixtureServer, mcpServerStateAuthRequired); err != nil {
		return err
	}
	s, err := openMusterSession(cfg, token, "platform-test-oauth")
	if err != nil {
		return err
	}

	res, err := s.callTool("list_tools", nil)
	if err != nil {
		return err
	}
	var listing struct {
		ServersRequiringAuth []struct {
			Name     string `json:"name"`
			Status   string `json:"status"`
			AuthTool string `json:"auth_tool"`
		} `json:"servers_requiring_auth"`
	}
	if err := json.Unmarshal([]byte(innerText(res)), &listing); err != nil {
		return fmt.Errorf("parsing list_tools payload: %w", err)
	}
	flagged := false
	for _, srv := range listing.ServersRequiringAuth {
		if srv.Name == oauthFixtureServer {
			flagged = true
			note("list_tools: %s status=%s auth_tool=%s", srv.Name, srv.Status, srv.AuthTool)
		}
	}
	if !flagged {
		return fmt.Errorf("list_tools does not list %s under servers_requiring_auth", oauthFixtureServer)
	}

	first, err := signInChallenge(cfg, s)
	if err != nil {
		return err
	}
	note("challenge: %s%s?state=%.12s… (client id via %s)",
		cfg.MusterBaseURL(), oauthProxyStartPath, first.state, first.clientIDMethod)
	second, err := signInChallenge(cfg, s)
	if err != nil {
		return fmt.Errorf("second core_auth_login: %w", err)
	}
	if second.state == first.state {
		return fmt.Errorf("the second core_auth_login reused the first challenge's state — Sign in cannot be reopened with a fresh challenge")
	}
	note("a second core_auth_login answers a fresh state")

	// Redeem the URL the way the browser would, minus following the redirect.
	transport, err := labTLSTransport()
	if err != nil {
		return err
	}
	noFollow := &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(second.authURL)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	location := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode > 399 || location == "" {
		return fmt.Errorf("the proxy start endpoint answered %d instead of redirecting to the authorization server:\n%.300s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	target, err := url.Parse(location)
	if err != nil {
		return fmt.Errorf("proxy start redirect %q: %w", location, err)
	}
	if !strings.Contains(target.Path, "authorize") {
		return fmt.Errorf("the proxy start endpoint redirected to %s, not to an authorization endpoint", location)
	}
	note("proxy start redirects to %s://%s%s (the authorization server)", target.Scheme, target.Host, target.Path)
	return nil
}
