package lab

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

// handlerPayloadRe extracts the authorization payload Backstage's handler page
// hands to the opener via postMessage (see step 4 in backstageSignIn).
var handlerPayloadRe = regexp.MustCompile(`decodeURIComponent\('([^']+)'\)`)

// BackstageTest drives the full Backstage <-> Dex sign-in headlessly for each
// user, reports the identity Backstage resolved, proves the Giant Swarm
// muster plugin can reach muster with that user's forwarded token, and then
// drives the Dev Portal's Agent Platform pages on kagent API v2 the way a
// person does (backstagetest_agents.go): the wizard's create path through
// agent-manager as the person, the agents list per user, a streamed and a
// resumed conversation, HITL and Stop, the detail page's edit, skills update
// and delete.
func BackstageTest(cfg *config.Config, emails []string) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if len(emails) == 0 {
		for _, u := range cfg.Users {
			emails = append(emails, u.Email)
		}
	}
	var sessions []*portalSession
	for i, email := range emails {
		user := cfg.FindUser(email)
		if user == nil {
			return fmt.Errorf("no user %q in %s", email, config.File)
		}
		fmt.Printf("=== %s ===\n", email)
		ps, err := backstageSignIn(cfg, user)
		if err != nil {
			return fmt.Errorf("%s: %w", email, err)
		}
		if i == 0 {
			// The grouping does not depend on the viewer; once is enough.
			if err := proveServerGroups(ps); err != nil {
				return fmt.Errorf("%s: %w", email, err)
			}
		}
		sessions = append(sessions, ps)
		fmt.Println()
	}
	fmt.Println("all sign-ins resolved and reached muster")

	if !cfg.Platform.Agents {
		fmt.Println("agent platform pages skipped (platform.agents off)")
		return nil
	}
	return proveAgentPlatform(cfg, sessions)
}

// backstageSignIn drives one user's sign-in and the muster plugin's reads with
// the session, and returns the session for the proofs that follow.
func backstageSignIn(cfg *config.Config, user *config.User) (*portalSession, error) {
	ps, err := backstageLogin(cfg, user)
	if err != nil {
		return nil, err
	}
	fmt.Printf("  dex asserted    groups=%v email=%v\n", ps.claims["groups"], ps.claims["email"])
	fmt.Printf("  token audience  %v\n", ps.claims["aud"])
	fmt.Printf("  backstage user  %s\n", ps.identity.UserEntityRef)
	fmt.Printf("  ownership refs  %v\n", ps.identity.OwnershipEntityRefs)

	muster := ps.musterGet
	musterPost := ps.musterPost

	installation := installationQuery

	status, payload, err := muster("/servers" + installation)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("muster /servers FAILED %d: %.200v", status, payload)
	}
	servers, _ := unwrapKey(payload, "mcpServers").([]any)
	pairs := make([]string, 0, len(servers))
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		if m, ok := s.(map[string]any); ok {
			pairs = append(pairs, fmt.Sprintf("(%v, %v)", m[nameKey], m["state"]))
			names = append(names, fmt.Sprintf("%v", m[nameKey]))
		}
	}
	fmt.Printf("  muster servers  [%s]\n", strings.Join(pairs, ", "))
	// The create wizard offers an installation only when its muster lists
	// agent-manager (useAgentManagerAvailability): the feature detection.
	if !slices.Contains(names, agentManagerMCPServer) {
		return nil, fmt.Errorf("muster /servers lists no %s — the portal offers no installation to create agents on", agentManagerMCPServer)
	}

	// The per-server sign-in path behind the portal's Sign in button
	// (backstage#2203), with this user's own forwarded token: the lab's
	// fixture (oauthfixture.go) is listed as Auth Required, and /auth/login
	// hands back muster's challenge — the OAuth proxy start URL with a state
	// — normalised to status auth_required.
	fixtureState := "missing"
	for _, s := range servers {
		if m, ok := s.(map[string]any); ok && m[nameKey] == oauthFixtureServer {
			fixtureState = fmt.Sprintf("%v", m["state"])
		}
	}
	if !isAuthRequiredState(fixtureState) && !strings.EqualFold(fixtureState, "connected") {
		return nil, fmt.Errorf("MCPServer %s is %q, not Auth Required (or Connected for a signed-in session) — the sign-in fixture is missing or broken (`agentlab platform` creates it)",
			oauthFixtureServer, fixtureState)
	}
	status, raw, err := musterPost("/auth/login"+installation, map[string]any{serverKey: oauthFixtureServer})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("muster /auth/login FAILED %d: %.200s", status, raw)
	}
	var login struct {
		Status         string `json:"status"`
		AuthURL        string `json:"authUrl"`
		Message        string `json:"message"`
		ClientIDMethod string `json:"clientIdMethod"`
	}
	if err := json.Unmarshal(raw, &login); err != nil {
		return nil, fmt.Errorf("parsing /auth/login answer: %w\n%.200s", err, raw)
	}
	if login.Status != "auth_required" {
		return nil, fmt.Errorf("/auth/login for %s answered status %q, not auth_required: %.300s", oauthFixtureServer, login.Status, login.Message)
	}
	if want := cfg.MusterBaseURL() + oauthProxyStartPath + "?state="; !strings.HasPrefix(login.AuthURL, want) {
		return nil, fmt.Errorf("/auth/login sign-in URL %q is not muster's proxy start endpoint (want %s…)", login.AuthURL, want)
	}
	fmt.Printf("  sign-in challenge %s -> %s%s?state=… (client id via %s)\n",
		oauthFixtureServer, cfg.MusterBaseURL(), oauthProxyStartPath, login.ClientIDMethod)

	status, payload, err = muster("/workflows" + installation)
	if err != nil {
		return nil, err
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
		return nil, err
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
	return ps, nil
}

// proveServerGroups is the MCP servers page's grouping, asserted from the
// data the page reads: the MCPServer CRs through Backstage's Kubernetes proxy
// as this user, partitioned by the tool-group label with the released
// plugin's arithmetic (servergroups.go). The lab's fixtures pin the groups —
// the fake-fleet families under Infrastructure while platform.fakeFleet is
// on and no row named after them while it is off, the OAuth fixture under
// Registered servers — and the chart-shipped servers are judged by the label
// their chart stamps (agent-manager and model-manager under Agent Platform
// once their charts carry it, the bundled mcp-kubernetes under
// Infrastructure). The fallback is proven on the same data with every
// tool-group label removed: one Registered servers list, every section still
// present, never an empty page.
func proveServerGroups(ps *portalSession) error {
	servers, err := ps.listMCPServerCRs()
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		return fmt.Errorf("the Kubernetes proxy lists no MCPServer CRs for %s — the servers page would be empty", ps.user.Email)
	}
	groups := partitionServers(servers)
	fmt.Printf("  MCP servers page groups (%d CRs via /api/kubernetes/proxy):\n%s\n", len(servers), describeGroups(groups))
	if err := assertServerGroups(servers, groups, ps.cfg.Platform.FakeFleet); err != nil {
		return fmt.Errorf("servers page grouping: %w", err)
	}
	labelled := 0
	for _, s := range servers {
		if s.toolGroup() != "" {
			labelled++
		}
	}
	fallback := partitionServers(stripToolGroupLabels(servers))
	if n := len(fallback[groupAgentPlatform]) + len(fallback[groupInfrastructure]); n != 0 {
		return fmt.Errorf("without labels %d rows still land outside Registered servers", n)
	}
	if len(fallback[groupRegistered]) == 0 || len(fallback) != len(toolGroupOrder) {
		return fmt.Errorf("without labels the page would be empty (%d Registered rows, %d sections)", len(fallback[groupRegistered]), len(fallback))
	}
	fmt.Printf("  fallback without any %s label: %d rows, all under %s; %d of %d CRs carry the label today\n",
		toolGroupLabel, len(fallback[groupRegistered]), toolGroupTitles[groupRegistered], labelled, len(servers))
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
