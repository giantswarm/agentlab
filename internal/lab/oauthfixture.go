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

// The per-server OAuth sign-in fixture.
//
// Every downstream the lab aggregates (mcp-kubernetes, mcp-prometheus,
// model-manager) authenticates the user through the forwarded Dex id_token
// (auth.forwardToken), so nothing exercises muster's OAuth *client* role: the
// proxy behind core_auth_login and the portal's per-server "Sign in" button,
// which hands the user a challenge URL, walks the browser through the
// downstream's authorization server and keeps the token per session. The lab
// turns that role on (muster.muster.oauth.mcpClient in
// agent-platform-values.yaml.tmpl — the umbrella leaves it off, real
// installations turn it on) and ships one downstream that declares auth.type
// oauth and reads Auth Required until a session signs in, so the path can be
// driven and proven headlessly — challenge AND completed sign-in.
//
// The downstream is muster's own protected /mcp (a 401 with RFC 9728 metadata
// at zero cost); the authorization server is PINNED to the lab Dex with the
// platform client (spec.auth.authorizationServer, the GitHub-App shape), not
// discovered. Discovery would name muster's own authorization server, which
// identifies clients by Client ID Metadata Document, and its SSRF guards
// refuse every lab hostname — the metadata URL and a registered redirect URI
// alike resolve to the edge's cluster IP in-cluster and to loopback outside —
// so no sign-in could ever complete there. Dex matches redirect URIs exactly
// (dex.yaml.tmpl lists muster's proxy callback on the client), and the token
// it issues carries the platform client's audience, which the endpoint
// trusts: signing in connects muster to itself and surfaces its own tools
// under x_lab-oauth-fixture_ for that session. Harmless, per session, and
// exactly what the toolset proof needs (toolsetstest.go: a toolset naming
// the server resolves to its tools only for the session that signed in).
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
	// oauthProxyCallbackPath is where the authorization server sends the
	// browser back (the chart's callbackPath default); registered on the
	// platform client in dex.yaml.tmpl.
	oauthProxyCallbackPath = "/oauth/proxy/callback"
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
	return waitMCPServerState(oauthFixtureServer, oauthFixtureHealthyStates...)
}

// oauthFixtureHealthyStates are the CR states of a reachable fixture: Auth
// Required while no session is signed in, Connected while one is — a
// completed sign-in (toolsets-test, the portal's Sign in) connects muster to
// the endpoint for that session, and the connection outlives the session's
// sign-out until its next use. Failed is the state that means trouble.
var oauthFixtureHealthyStates = []string{mcpServerStateAuthRequired, "Connected"}

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
	env, err := s.callToolEnvelope("core_auth_login", map[string]any{serverKey: oauthFixtureServer})
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
	// listener — and a Failed fixture is what the portal would show. Connected
	// (another session signed in) is as healthy as Auth Required: sign-ins
	// are per session, and this session has none.
	if err := waitMCPServerState(oauthFixtureServer, oauthFixtureHealthyStates...); err != nil {
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

	// Redeem the URL the way the browser would, minus following the redirect:
	// the proxy start endpoint sends the browser to the pinned authorization
	// server's authorization endpoint — the lab Dex.
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
	// The fixture applied by this binary pins Dex (the browser lands on Dex's
	// authorization endpoint); one applied by an older `agentlab platform`
	// discovers muster's own authorization server instead. Both are the
	// authorization server's endpoint; the pinned one is the one a sign-in
	// can complete against (oauth-fixture.yaml.tmpl).
	switch {
	case strings.HasPrefix(location, cfg.Issuer()+"/auth"):
		note("proxy start redirects to %s://%s%s (the pinned authorization server, client %s)",
			target.Scheme, target.Host, target.Path, target.Query().Get("client_id"))
	case strings.Contains(target.Path, "authorize"):
		note("proxy start redirects to %s://%s%s (the discovered authorization server — the fixture is not pinned to Dex: re-run `agentlab platform` with this binary for a sign-in that completes)",
			target.Scheme, target.Host, target.Path)
	default:
		return fmt.Errorf("the proxy start endpoint redirected to %s, not to an authorization endpoint", location)
	}
	return nil
}

// completeSignIn finishes a per-server sign-in the way the browser does after
// the portal (or core_auth_login) handed it the challenge URL: follow the
// proxy start endpoint to the authorization server's login form, submit the
// user's credentials, and follow Dex back through muster's proxy callback to
// its "Authentication Successful" page. The session that produced the
// challenge — identified by the state — then holds the server's token.
func completeSignIn(challengeURL string, user *config.User) error {
	transport, err := labTLSTransport()
	if err != nil {
		return err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	browser := &http.Client{Jar: jar, Transport: transport, Timeout: 60 * time.Second}
	resp, err := browser.Get(challengeURL)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	landed := resp.Request.URL.String()
	if resp.StatusCode != http.StatusOK || !strings.Contains(landed, "/auth/local") {
		return fmt.Errorf("the sign-in did not reach Dex's login form (landed on %s with %d):\n%.300s",
			landed, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	resp, err = browser.PostForm(landed, url.Values{"login": {user.Email}, passwordParam: {user.Password}})
	if err != nil {
		return err
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	final := resp.Request.URL.String()
	if resp.StatusCode != http.StatusOK || !strings.Contains(final, oauthProxyCallbackPath) || !strings.Contains(string(body), "Authentication Successful") {
		return fmt.Errorf("the sign-in did not end on muster's proxy callback success page (ended on %s with %d):\n%.300s",
			final, resp.StatusCode, excerpt(string(body), 300))
	}
	return nil
}

// restartOAuthFixture asks muster for a one-shot restart of the fixture's
// service (spec.restartRequestedAt, acted on once per value), which closes
// every pooled connection and returns the CR to Auth Required. Best effort:
// the lab's state is not the proof's verdict.
func restartOAuthFixture() {
	stamp := time.Now().UTC().Format(time.RFC3339)
	if err := runQuiet("kubectl", "-n", platformNamespace, "patch", "mcpservers.muster.giantswarm.io", oauthFixtureServer,
		"--type", "merge", "-p", fmt.Sprintf(`{"spec":{"restartRequestedAt":%q}}`, stamp)); err != nil {
		note("could not request a restart of %s (%v); it reads Connected until the signed-in session's token expires", oauthFixtureServer, err)
		return
	}
	if err := waitMCPServerState(oauthFixtureServer, mcpServerStateAuthRequired); err != nil {
		note("%s did not return to Auth Required after the restart request: %v", oauthFixtureServer, err)
		return
	}
	note("%s back to Auth Required", oauthFixtureServer)
}
