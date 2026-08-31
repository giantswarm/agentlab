package lab

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// handlerPayloadRe extracts the authorization payload Backstage's handler page
// hands to the opener via postMessage (see step 4 in backstageSignIn).
var handlerPayloadRe = regexp.MustCompile(`decodeURIComponent\('([^']+)'\)`)

// BackstageTest drives the full Backstage <-> Dex sign-in headlessly for each
// user, reports the identity Backstage resolved, and then proves the Giant
// Swarm muster plugin can reach muster with that user's forwarded token.
func BackstageTest(cfg *config.Config, emails []string) error {
	if len(emails) == 0 {
		for _, u := range cfg.Users {
			emails = append(emails, u.Email)
		}
	}
	for _, email := range emails {
		user := cfg.FindUser(email)
		if user == nil {
			return fmt.Errorf("no user %q in %s", email, config.File)
		}
		fmt.Printf("=== %s ===\n", email)
		if err := backstageSignIn(cfg, user); err != nil {
			return fmt.Errorf("%s: %w", email, err)
		}
		fmt.Println()
	}
	fmt.Println("all sign-ins resolved and reached muster")
	return nil
}

func backstageSignIn(cfg *config.Config, user *config.User) error {
	transport, err := labTLSTransport()
	if err != nil {
		return err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
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
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	dexURL := resp.Header.Get("Location")
	if dexURL == "" {
		return fmt.Errorf("no redirect to Dex (status %d)", resp.StatusCode)
	}

	// 2. Follow to Dex's login form.
	resp, err = follow.Get(dexURL)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	loginURL := resp.Request.URL.String()

	// 3. Submit lab credentials; Dex redirects back through the Backstage
	//    handler, whose response embeds the authorization result.
	form := url.Values{"login": {user.Email}, passwordParam: {user.Password}}
	resp, err = follow.PostForm(loginURL, form)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// 4. The handler page hands the result to the opener via postMessage;
	//    headlessly, the payload is regex'd out of the inline script. There is
	//    no API that returns this payload cleanly — inherent to the test's job.
	m := handlerPayloadRe.FindSubmatch(body)
	if m == nil {
		return fmt.Errorf("could not parse the handler response")
	}
	// PathUnescape, not QueryUnescape: '+' inside the JSON payload (JWTs,
	// base64) must survive, matching Python's urllib.parse.unquote.
	decoded, err := url.PathUnescape(string(m[1]))
	if err != nil {
		return err
	}
	var probe struct {
		Type     string         `json:"type"`
		Response map[string]any `json:"response"`
	}
	if err := json.Unmarshal([]byte(decoded), &probe); err != nil {
		return fmt.Errorf("parsing authorization response: %w", err)
	}
	if probe.Type != "authorization_response" {
		return fmt.Errorf("unexpected response type %q", probe.Type)
	}
	if errVal, ok := probe.Response["error"]; ok {
		raw, _ := json.Marshal(errVal)
		return fmt.Errorf("SIGN-IN FAILED: %.400s", string(raw))
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
		return err
	}
	dexIDToken := auth.Response.ProviderInfo.IDToken
	bsToken := auth.Response.BackstageIdentity.Token
	identity := auth.Response.BackstageIdentity.Identity

	claims, err := decodeJWTClaims(dexIDToken)
	if err != nil {
		return err
	}
	fmt.Printf("  dex asserted    groups=%v email=%v\n", claims["groups"], claims["email"])
	fmt.Printf("  token audience  %v\n", claims["aud"])
	fmt.Printf("  backstage user  %s\n", identity.UserEntityRef)
	fmt.Printf("  ownership refs  %v\n", identity.OwnershipEntityRefs)

	// The muster plugin forwards the Dex id_token in its own header; the
	// backend promotes it to Authorization: Bearer on the MCP session to
	// muster.
	muster := func(path string) (int, any, error) {
		req, err := http.NewRequest(http.MethodGet, cfg.BackstageBaseURL()+"/api/muster"+path, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+bsToken)
		req.Header.Set("backstage-muster-authorization", dexIDToken)
		resp, err := follow.Do(req)
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

	// The umbrella's app-config names the muster installation after the Helm
	// release, not the kind cluster.
	installation := "?installation=" + platformRelease

	status, payload, err := muster("/servers" + installation)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("muster /servers FAILED %d: %.200v", status, payload)
	}
	servers, _ := unwrapKey(payload, "mcpServers").([]any)
	pairs := make([]string, 0, len(servers))
	for _, s := range servers {
		if m, ok := s.(map[string]any); ok {
			pairs = append(pairs, fmt.Sprintf("(%v, %v)", m[nameKey], m["state"]))
		}
	}
	fmt.Printf("  muster servers  [%s]\n", strings.Join(pairs, ", "))

	status, payload, err = muster("/workflows" + installation)
	if err != nil {
		return err
	}
	var wfNames []string
	if status == http.StatusOK {
		wfs, _ := unwrapKey(payload, "workflows").([]any)
		for _, w := range wfs {
			if m, ok := w.(map[string]any); ok {
				wfNames = append(wfNames, fmt.Sprintf("%v", m[nameKey]))
			}
		}
	}
	fmt.Printf("  muster workflows %v\n", wfNames)

	status, payload, err = muster("/core-tools" + installation)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		count := 0
		if m, ok := payload.(map[string]any); ok {
			if tools, ok := m["tools"].([]any); ok {
				count = len(tools)
			} else if tools, ok := m["coreTools"].([]any); ok {
				count = len(tools)
			}
		}
		fmt.Printf("  muster core tools %d exposed\n", count)
	} else {
		fmt.Printf("  muster /core-tools -> %d\n", status)
	}

	// The agent create flow's Deploy button scaffolds
	// template:default/agent-deployment; without the catalog entity every
	// deploy dies with a scaffolder 404. Assert the lab registered it (the
	// embedded copy in backstage.yaml.tmpl).
	req, err := http.NewRequest(http.MethodGet,
		cfg.BackstageBaseURL()+"/api/catalog/entities/by-name/template/default/agent-deployment", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bsToken)
	resp, err = follow.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent-deployment template not in the catalog (%d) — the create flow's deploy would 404", resp.StatusCode)
	}
	fmt.Printf("  agent deploy template registered (template:default/agent-deployment)\n")
	return nil
}

// unwrapKey digs a key out of muster tool results, which nest JSON inside
// content[].text, twice over: any string encountered is re-parsed as JSON and
// searched too.
func unwrapKey(node any, key string) any {
	stack := []any{node}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if s, ok := cur.(string); ok {
			var parsed any
			if json.Unmarshal([]byte(s), &parsed) != nil {
				continue
			}
			cur = parsed
		}
		switch v := cur.(type) {
		case map[string]any:
			if val, ok := v[key]; ok {
				return val
			}
			for _, val := range v {
				stack = append(stack, val)
			}
		case []any:
			stack = append(stack, v...)
		}
	}
	return nil
}
