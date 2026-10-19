package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The muster half of tenants-test (giantswarm/agentlab#166, the
// agent-permissions plan's D9): MCP servers per tenant. With
// mcpServersInAllNamespaces muster serves the MCPServers of the organization
// namespaces next to its own, lists them as the caller with the person's
// token, and files a tenant server's sign-in under its namespace.
const (
	// tenantsMusterVersion is the muster chart the lab pins with the tenants
	// on: the newest release candidate with giantswarm/muster#1601 (the
	// listing as the caller). HACKS.md U38.
	tenantsMusterVersion = "5.47.0-rc.9"
	// tenantsMusterMinimum is the first muster with #1601.
	tenantsMusterMinimum = "5.47.0-rc.8"
	// tenantsMCPServer is the sign-in fixture tenants-test creates in each
	// organization namespace: the lab's oauth-fixture.yaml.tmpl shape, with
	// the same issuer as lab-oauth-fixture in muster's namespace.
	tenantsMCPServer = "agentlab-tenants-mcp"
	// tenantsMCPClient is its Secret with the OAuth client (the lab's
	// platform client), in its own namespace; tenantsMCPSecretReader the Role
	// and RoleBinding that let muster read it there (HACKS.md U39).
	tenantsMCPClient       = "agentlab-tenants-mcp-client" // #nosec G101 -- a Secret's name
	tenantsMCPSecretReader = "agentlab-tenants-mcp-secret-reader"
	// tenantsMCPMissing names a server that exists in no namespace: what an
	// answer about another organization's server is compared with.
	tenantsMCPMissing = "agentlab-tenants-missing"
	// tenantsTestMusterAgent is the first organization's agent of step M5,
	// bound to muster with the whole catalogue.
	tenantsTestMusterAgent = "agentlab-tenants-muster"
	// tenantsMCPRefusedPrefix starts the names of the servers muster must
	// refuse in an organization namespace.
	tenantsMCPRefusedPrefix = "agentlab-tenants-refused-"
	// musterConfigMap is muster's rendered config.yaml.
	musterConfigMap    = "muster-config"
	musterConfigKey    = "config.yaml"
	mcpServerStateFail = "Failed"
	// musterIdentityRefusal is the text of muster's refusal of a server
	// outside its namespace that asks for an identity
	// (api.ErrIdentityOutsideNamespace).
	musterIdentityRefusal = "an MCPServer outside muster's namespace gets no identity from muster"
	// tenantsMusterSettle bounds how long a new MCPServer or RoleBinding
	// takes to reach a person's listing: muster keeps an answer for 5 s per
	// token, and registers a new server on its watch.
	tenantsMusterSettle = time.Minute
	// tenantCallbackPrefix and tenantCallbackSuffix frame an organization
	// namespace's own OAuth callback (giantswarm/muster#1598).
	tenantCallbackPrefix = "/oauth/ns/"
	tenantCallbackSuffix = "/callback"
)

// tenantCallbackPath is the callback a server of the namespace signs in
// through.
func tenantCallbackPath(ns string) string {
	return tenantCallbackPrefix + ns + tenantCallbackSuffix
}

// tenantServerKey is the key muster lists, signs in and starts a server of
// an organization namespace under: <namespace>/<name>.
func tenantServerKey(ns, name string) string {
	return ns + "/" + name
}

// tenantToolPrefix is the prefix of the tools of a server of an
// organization namespace: x_<namespace>_<name>_.
func tenantToolPrefix(ns, name string) string {
	return "x_" + ns + "_" + name + "_"
}

// musterTenancy is what decides whether muster serves the organizations'
// MCPServers: its version (the image's tag) and mcpServersInAllNamespaces in
// its config.yaml.
type musterTenancy struct {
	image         string
	allNamespaces bool
}

// problem words what keeps muster from serving MCP servers per tenant, ""
// when nothing does.
func (t musterTenancy) problem() string {
	tag := t.image[strings.LastIndex(t.image, ":")+1:]
	if at := strings.Index(t.image, "@"); at >= 0 {
		before := t.image[:at]
		tag = before[strings.LastIndex(before, ":")+1:]
	}
	minimum := semver.MustParse(tenantsMusterMinimum)
	if v, err := semver.NewVersion(tag); err == nil && v.LessThan(minimum) {
		return fmt.Sprintf("muster %s lists no organization's MCPServer as the caller: it needs %s or later (giantswarm/muster#1601). `agentlab platform` with this agentlab pins %s with platform.tenants on", tag, tenantsMusterMinimum, tenantsMusterVersion)
	}
	if !t.allNamespaces {
		return "muster runs without mcpServersInAllNamespaces: it serves no organization namespace's MCPServer. `agentlab platform` with this agentlab sets muster.muster.mcpServersInAllNamespaces with platform.tenants on (a platform.valuesFiles overlay that sets it false wins)"
	}
	return ""
}

// readMusterTenancy reads muster's image and config.yaml.
func readMusterTenancy() (musterTenancy, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	var t musterTenancy
	deploy, err := getObject(ctx, gvrDeployments, platformNamespace, componentMuster)
	if err != nil {
		return t, fmt.Errorf("the muster Deployment in %s: %w", platformNamespace, err)
	}
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	for _, c := range containers {
		container, _ := c.(map[string]any)
		if container[nameKey] == componentMuster {
			t.image, _ = container["image"].(string)
		}
	}
	cm, err := getObject(ctx, gvrConfigMaps, platformNamespace, musterConfigMap)
	if err != nil {
		return t, fmt.Errorf("muster's ConfigMap %s/%s: %w", platformNamespace, musterConfigMap, err)
	}
	file, _, _ := unstructured.NestedString(cm.Object, "data", musterConfigKey)
	var conf struct {
		MCPServersInAllNamespaces bool `yaml:"mcpServersInAllNamespaces"`
	}
	if err := yaml.Unmarshal([]byte(file), &conf); err != nil {
		return t, fmt.Errorf("muster's %s: %w", musterConfigKey, err)
	}
	t.allNamespaces = conf.MCPServersInAllNamespaces
	if t.image == "" {
		return t, fmt.Errorf("the muster Deployment in %s has no container %q with an image: muster's version cannot be read", platformNamespace, componentMuster)
	}
	return t, nil
}

// tenantMCPServerManifest is an MCPServer of an organization namespace on
// muster's own protected /mcp, with the spec's auth (and further fields)
// given as YAML lines indented under spec.
func tenantMCPServerManifest(ns, name, spec string) string {
	return fmt.Sprintf(`apiVersion: muster.giantswarm.io/v1alpha1
kind: MCPServer
metadata:
  name: %s
  namespace: %s
spec:
  type: streamable-http
  url: %s
  autoStart: true
%s
`, name, ns, oauthFixtureURL, spec)
}

// tenantSignInFixture is the sign-in fixture of an organization namespace:
// the client Secret (the lab's platform client), muster's read of it, and
// the MCPServer with the authorization server pinned to the lab Dex — the
// lab fixture's shape (oauth-fixture.yaml.tmpl), with the same issuer as
// lab-oauth-fixture, so a sign-in that leaked across namespaces would show.
// muster reads a pin's Secret in the server's namespace with its own
// ServiceAccount, which neither the muster chart nor the platform chart
// lets it do outside muster's namespace (HACKS.md U39).
func tenantSignInFixture(cfg *config.Config, ns string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %[2]s
  namespace: %[1]s
stringData:
  client-id: %[3]s
  client-secret: %[4]s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: %[5]s
  namespace: %[1]s
rules:
  - apiGroups: [""]
    resources: [secrets]
    verbs: [get]
    resourceNames: [%[2]s]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: %[5]s
  namespace: %[1]s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: %[5]s
subjects:
  - kind: ServiceAccount
    name: %[6]s
    namespace: %[7]s
---
`, ns, tenantsMCPClient, config.AgentPlatformClientID, config.AgentPlatformClientSecret, tenantsMCPSecretReader, componentMuster, platformNamespace) +
		tenantMCPServerManifest(ns, tenantsMCPServer, fmt.Sprintf(`  auth:
    type: oauth
    authorizationServer:
      issuer: %s
      authorizationEndpoint: %s/auth
      tokenEndpoint: %s/token
      clientCredentialsSecretRef:
        name: %s
      scopes: openid profile email offline_access`, cfg.MusterBaseURL(), cfg.Issuer(), cfg.Issuer(), tenantsMCPClient))
}

// refusedTenantServer is one server muster must refuse in an organization
// namespace: the field it asks for an identity with, as muster's refusal
// names it, and the spec lines that set it.
type refusedTenantServer struct {
	field, name, spec string
}

// refusedTenantServers are the identity fields of giantswarm/muster#1508,
// each on a server of its own.
func refusedTenantServers(cfg *config.Config) []refusedTenantServer {
	return []refusedTenantServer{
		{"auth.forwardToken", "forward-token", "  auth:\n    type: oauth\n    forwardToken: true"},
		{"auth.requiredAudiences", "audiences", "  auth:\n    type: oauth\n    requiredAudiences: [" + config.KubernetesClientID + "]"},
		{"auth.tokenExchange", "token-exchange", fmt.Sprintf("  auth:\n    type: oauth\n    tokenExchange:\n      enabled: true\n      dexTokenEndpoint: %s/token\n      connectorId: local\n      clientCredentialsSecretRef:\n        name: %s", cfg.Issuer(), tenantsMCPClient)},
		{"family", "family", "  family:\n    name: " + familyKubernetes + "\n    instanceArg: management_cluster"},
		// The CRD's rule for sigv4 reads forwardToken, so it is set.
		{"auth.type sigv4", "sigv4", "  auth:\n    type: sigv4\n    forwardToken: false\n    sigv4:\n      region: eu-west-1"},
	}
}

// applyTenantManifest applies a manifest of tenants-test's fixtures.
func applyTenantManifest(manifest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	_, err := applyManifests(ctx, []byte(manifest))
	return err
}

// tenantMCPServerStatus reads a server's status.state and status.lastError.
func tenantMCPServerStatus(ns, name string) (string, string, error) {
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	obj, err := getObject(ctx, gvr, ns, name)
	if err != nil {
		return "", "", err
	}
	state, _, _ := unstructured.NestedString(obj.Object, "status", "state")
	lastError, _, _ := unstructured.NestedString(obj.Object, "status", "lastError")
	return state, lastError, nil
}

// waitTenantMCPServerState waits until the server reads one of the states,
// and returns its lastError.
func waitTenantMCPServerState(ns, name string, want ...string) (string, error) {
	var state, lastError string
	var readErr error
	reached := waitFor(int(tenantsMusterSettle/pollInterval), pollInterval, func() bool {
		state, lastError, readErr = tenantMCPServerStatus(ns, name)
		return readErr == nil && slices.Contains(want, state)
	})
	if !reached {
		return "", notReached(fmt.Sprintf("MCPServer %s/%s", ns, name), orList(want), state, readErr,
			fmt.Sprintf("check `agentlab logs muster` and `kubectl -n %s describe mcpservers.muster.giantswarm.io %s`", ns, name))
	}
	return lastError, nil
}

// platformMCPServers are the MCPServers of muster's namespace, read with the
// lab's own kubeconfig, as namespace/name: what every signed-in person sees.
func platformMCPServers() (refSet, error) {
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	items, err := listObjects(ctx, gvr, platformNamespace, "")
	if err != nil {
		return nil, err
	}
	set := refSet{}
	for _, item := range items {
		set[platformNamespace+"/"+item.GetName()] = true
	}
	return set, nil
}

// musterListing is what a person's session sees of the servers: the
// servers of core_mcpserver_list as namespace/name, the servers awaiting
// the session's sign-in from list_tools, its tools, and the notice muster
// gives a session that sees no server.
type musterListing struct {
	servers  refSet
	awaiting []string
	tools    []string
	notice   string
}

// readMusterListing reads the session's view of the servers.
func readMusterListing(s *musterSession) (*musterListing, error) {
	env, err := s.callToolEnvelope("core_mcpserver_list", nil)
	if err != nil {
		return nil, err
	}
	if env.IsError {
		return nil, fmt.Errorf("core_mcpserver_list: %s", excerpt(env.Content[0].Text, 300))
	}
	var list struct {
		MCPServers []struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"mcpServers"`
		Notice string `json:"notice"`
	}
	if err := json.Unmarshal([]byte(env.Content[0].Text), &list); err != nil {
		return nil, fmt.Errorf("parsing core_mcpserver_list: %w\n%.300s", err, env.Content[0].Text)
	}
	l := &musterListing{servers: refSet{}, notice: list.Notice}
	for _, srv := range list.MCPServers {
		l.servers[srv.Namespace+"/"+srv.Name] = true
	}
	res, err := s.callTool("list_tools", map[string]any{limitKey: listToolsPage})
	if err != nil {
		return nil, err
	}
	var tools struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		ServersRequiringAuth []pendingServer `json:"servers_requiring_auth"`
	}
	if err := json.Unmarshal([]byte(innerText(res)), &tools); err != nil {
		return nil, fmt.Errorf("parsing list_tools: %w", err)
	}
	for _, t := range tools.Tools {
		l.tools = append(l.tools, t.Name)
	}
	for _, p := range tools.ServersRequiringAuth {
		l.awaiting = append(l.awaiting, p.Name)
	}
	return l, nil
}

// toolsWithPrefix are the listed tools that start with the prefix.
func toolsWithPrefix(tools []string, prefix string) []string {
	var out []string
	for _, t := range tools {
		if strings.HasPrefix(t, prefix) {
			out = append(out, t)
		}
	}
	return out
}

// waitMusterListing reads the session's view until the servers are the
// wanted ones, bounded by tenantsMusterSettle, and returns the view and the
// difference left at the deadline.
func waitMusterListing(s *musterSession, want refSet) (*musterListing, string, error) {
	var l *musterListing
	var diff string
	var readErr error
	waitFor(int(tenantsMusterSettle/pollInterval), pollInterval, func() bool {
		l, readErr = readMusterListing(s)
		if readErr != nil {
			return false
		}
		diff = rosterDiff(l.servers, want)
		return diff == ""
	})
	if readErr != nil {
		return nil, "", readErr
	}
	return l, diff, nil
}

// unseenAnswer is a core tool's answer as text, with every name of the
// server replaced by <server>: what a server the caller may not see and one
// that does not exist must have in common.
func unseenAnswer(env *toolEnvelope, names ...string) string {
	var texts []string
	for _, c := range env.Content {
		texts = append(texts, c.Text)
	}
	text := strings.Join(texts, "\n")
	for _, name := range names {
		text = strings.ReplaceAll(text, name, "<server>")
	}
	return fmt.Sprintf("isError=%v %s", env.IsError, text)
}

// answeredAsMissing calls the tool with the hidden server's argument, then
// missingTool with a missing server's, and fails unless both are errors
// that read the same once the names are taken out.
func answeredAsMissing(s *musterSession, tool, missingTool string, hidden, missing map[string]any, names ...string) (string, error) {
	got, err := s.callToolEnvelope(tool, hidden)
	if err != nil {
		return "", err
	}
	want, err := s.callToolEnvelope(missingTool, missing)
	if err != nil {
		return "", err
	}
	if !got.IsError {
		return "", fmt.Errorf("%s %v was answered: %s", tool, hidden, excerpt(unseenAnswer(got), 300))
	}
	if a, b := unseenAnswer(got, names...), unseenAnswer(want, names...); a != b {
		return "", fmt.Errorf("%s %v answered %q; for a server that does not exist the answer is %q", tool, hidden, excerpt(a, 300), excerpt(b, 300))
	}
	return excerpt(got.Content[0].Text, 160), nil
}

// tenantSignInRedirect follows the challenge to the authorization server
// without the browser's next step and returns the redirect_uri muster sent.
func tenantSignInRedirect(challengeURL string) (string, error) {
	transport, err := labTLSTransport()
	if err != nil {
		return "", err
	}
	noFollow := &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(challengeURL)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	location := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode > 399 || location == "" {
		return "", fmt.Errorf("the proxy start endpoint answered %d instead of redirecting to the authorization server:\n%.300s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	target, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("proxy start redirect %q: %w", location, err)
	}
	return target.Query().Get("redirect_uri"), nil
}

// signInToTenantServer signs the session's person in to a server of an
// organization namespace: the challenge, the redirect_uri muster sends to
// the authorization server (the namespace's own callback), and the sign-in
// completed on that callback.
func signInToTenantServer(cfg *config.Config, s *musterSession, user *config.User, ns string) error {
	key := tenantServerKey(ns, tenantsMCPServer)
	// A sign-in of an earlier run outlives its namespace (muster keeps the
	// person's token under the namespace and issuer), so the person signs
	// out first: core_auth_login then answers a challenge, not a connect.
	signOutOf(s, key)
	challenge, err := signInChallenge(cfg, s, key)
	if err != nil {
		return fmt.Errorf("%s's core_auth_login for %s: %w", user.Email, key, err)
	}
	redirect, err := tenantSignInRedirect(challenge.authURL)
	if err != nil {
		return err
	}
	if want := cfg.MusterBaseURL() + tenantCallbackPath(ns); redirect != want {
		return fmt.Errorf("the sign-in to %s sends redirect_uri %q to the authorization server, want %s, the namespace's own callback", key, redirect, want)
	}
	if err := completeSignInAt(challenge.authURL, user, tenantCallbackPath(ns)); err != nil {
		return fmt.Errorf("%s's sign-in to %s: %w", user.Email, key, err)
	}
	return nil
}

// signOutOf drops the session's person's token for the server, best
// effort.
func signOutOf(s *musterSession, key string) {
	r, err := s.callToolEnvelope("core_auth_logout", map[string]any{serverKey: key})
	switch {
	case err != nil:
		note("core_auth_logout for %s: %v", key, err)
	case r.IsError:
		note("core_auth_logout for %s: %s", key, excerpt(r.Content[0].Text, 160))
	}
}

// waitTenantTools waits until the session lists tools with the prefix and
// returns them.
func waitTenantTools(s *musterSession, prefix string) ([]string, error) {
	var tools []string
	var readErr error
	waitFor(int(tenantsMusterSettle/pollInterval), pollInterval, func() bool {
		var l *musterListing
		if l, readErr = readMusterListing(s); readErr != nil {
			return false
		}
		tools = toolsWithPrefix(l.tools, prefix)
		return len(tools) > 0
	})
	if readErr != nil {
		return nil, readErr
	}
	if len(tools) == 0 {
		return nil, fmt.Errorf("no tool %s* within %s of the sign-in", prefix, tenantsMusterSettle)
	}
	return tools, nil
}

// stillNeedsSignIn checks the session holds no token for the server: it
// awaits the session's sign-in, lists none of its tools, and
// core_auth_login answers a new challenge on the callback of the server's
// namespace (callbackPath).
func stillNeedsSignIn(cfg *config.Config, s *musterSession, key, toolPrefix, callbackPath string) error {
	l, err := readMusterListing(s)
	if err != nil {
		return err
	}
	if !slices.Contains(l.awaiting, key) {
		return fmt.Errorf("%s does not await a sign-in (servers awaiting one: %v)", key, l.awaiting)
	}
	if got := toolsWithPrefix(l.tools, toolPrefix); len(got) > 0 {
		return fmt.Errorf("the session lists %s's tools %v without its own sign-in", key, got)
	}
	challenge, err := signInChallenge(cfg, s, key)
	if err != nil {
		return fmt.Errorf("core_auth_login for %s: %w", key, err)
	}
	redirect, err := tenantSignInRedirect(challenge.authURL)
	if err != nil {
		return err
	}
	if want := cfg.MusterBaseURL() + callbackPath; redirect != want {
		return fmt.Errorf("the challenge for %s sends redirect_uri %q, want %s", key, redirect, want)
	}
	return nil
}

// tenantsMuster is what the muster half works with: the two organization
// namespaces, their tenant users and the admin (cluster-admin, who sees
// every namespace's servers).
type tenantsMuster struct {
	cfg        *config.Config
	namespaces [2]string
	users      [2]*config.User
	admin      *config.User
	// editor writes the first organization's agent of step M5 through
	// agent-manager's REST API (api).
	editor *config.User
	api    *agentManagerREST
}

// run is the muster half, steps M1 to M5, and returns its PASS lines.
func (m *tenantsMuster) run() ([]string, error) {
	var out []string
	a, b := m.namespaces[0], m.namespaces[1]
	alice, bob := m.users[0], m.users[1]
	keyA, keyB := tenantServerKey(a, tenantsMCPServer), tenantServerKey(b, tenantsMCPServer)

	step("M. The sign-in fixture %s in %s and %s (muster's own /mcp, the issuer of %s, the client in Secret %s that muster may read there)", tenantsMCPServer, a, b, oauthFixtureServer, tenantsMCPClient)
	for _, ns := range m.namespaces {
		if err := applyTenantManifest(tenantSignInFixture(m.cfg, ns)); err != nil {
			return nil, err
		}
	}
	for _, ns := range m.namespaces {
		if _, err := waitTenantMCPServerState(ns, tenantsMCPServer, mcpServerStateAuthRequired); err != nil {
			return nil, err
		}
	}
	login := func(u *config.User) (*musterSession, error) {
		token, err := passwordGrant(m.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, u.Email, u.Password, musterLoginScopes)
		if err != nil {
			return nil, err
		}
		return openMusterSession(m.cfg, token, "tenants-test")
	}
	var sessions [2]*musterSession
	for i, u := range m.users {
		s, err := login(u)
		if err != nil {
			return nil, err
		}
		sessions[i] = s
	}
	adminSession, err := login(m.admin)
	if err != nil {
		return nil, err
	}
	// The sign-ins of M2 and M4 go at the end: muster keeps them past the
	// namespaces.
	defer func() {
		signOutOf(sessions[1], keyB)
		signOutOf(sessions[0], keyA)
		signOutOf(adminSession, keyA)
	}()
	platform, err := platformMCPServers()
	if err != nil {
		return nil, err
	}

	step("M1. Each tenant user lists (core_mcpserver_list, list_tools) the servers of muster's namespace (%s) and their organization's %s, nothing of the other organization", platform, tenantsMCPServer)
	var wants [2]refSet
	for i, s := range sessions {
		ns, other := m.namespaces[i], m.namespaces[1-i]
		want := refSet{tenantServerKey(ns, tenantsMCPServer): true}
		for ref := range platform {
			want[ref] = true
		}
		wants[i] = want
		l, diff, err := waitMusterListing(s, want)
		if err != nil {
			return nil, fmt.Errorf("M1: %s's listing: %w", m.users[i].Email, err)
		}
		if diff != "" {
			return nil, fmt.Errorf("M1: %s (group %s, bound in %s) lists the servers %s after %s; want %s (%s)%s", m.users[i].Email, m.users[i].Groups, ns, l.servers, tenantsMusterSettle, want, diff, noticeOf(l))
		}
		if own := tenantServerKey(ns, tenantsMCPServer); !slices.Contains(l.awaiting, own) {
			return nil, fmt.Errorf("M1: list_tools does not offer %s the sign-in to %s (awaiting: %v)", m.users[i].Email, own, l.awaiting)
		}
		if theirs := tenantServerKey(other, tenantsMCPServer); slices.Contains(l.awaiting, theirs) {
			return nil, fmt.Errorf("M1: list_tools offers %s the sign-in to %s, another organization's server", m.users[i].Email, theirs)
		}
		note("%s lists %s", m.users[i].Email, l.servers)
	}
	step("M1. %s in no organization (the RoleBinding %s in %s gone a moment): lists the servers of muster's namespace only", bob.Email, tenantUsersBinding, b)
	if err := unbindTenantGroup(b); err != nil {
		return nil, err
	}
	since := time.Now()
	l, diff, err := waitMusterListing(sessions[1], platform)
	if err != nil {
		return nil, fmt.Errorf("M1: %s's listing: %w", bob.Email, err)
	}
	if diff != "" {
		return nil, fmt.Errorf("M1: %s, bound to no organization, lists %s after %s; want %s only (%s)%s", bob.Email, l.servers, tenantsMusterSettle, platform, diff, noticeOf(l))
	}
	note("%s lists %s, %s after the RoleBinding went", bob.Email, l.servers, time.Since(since).Round(time.Second))
	// Bound again, for M2: muster keeps an answer a few seconds per token.
	if err := bindTenantGroup(b, config.GroupTenantB); err != nil {
		return nil, err
	}
	since = time.Now()
	if l, diff, err = waitMusterListing(sessions[1], wants[1]); err != nil {
		return nil, fmt.Errorf("M1: %s's listing: %w", bob.Email, err)
	}
	if diff != "" {
		return nil, fmt.Errorf("M1: %s, bound again in %s, lists %s after %s (%s)", bob.Email, b, l.servers, tenantsMusterSettle, diff)
	}
	note("%s, bound again, lists %s's %s again %s after the RoleBinding", bob.Email, b, tenantsMCPServer, time.Since(since).Round(time.Second))
	out = append(out, fmt.Sprintf("PASS: through muster, %s lists the servers of %s and %s's %s, %s those of %s and %s's, and list_tools offers each the sign-in to their own organization's server only; in no organization, %s lists those of %s only",
		alice.Email, platformNamespace, a, tenantsMCPServer, bob.Email, platformNamespace, b, bob.Email, platformNamespace))

	step("M2. %s signs in to %s; %s's call of its tool, core_service_start and core_auth_login for it are answered as for a server that does not exist", bob.Email, keyB, alice.Email)
	if err := signInToTenantServer(m.cfg, sessions[1], bob, b); err != nil {
		return nil, fmt.Errorf("M2: %w", err)
	}
	bobTools, err := waitTenantTools(sessions[1], tenantToolPrefix(b, tenantsMCPServer))
	if err != nil {
		return nil, fmt.Errorf("M2: %s after the sign-in to %s: %w", bob.Email, keyB, err)
	}
	tool := bobTools[0]
	missingKey := tenantServerKey(b, tenantsMCPMissing)
	missingTool := tenantToolPrefix(b, tenantsMCPMissing) + strings.TrimPrefix(tool, tenantToolPrefix(b, tenantsMCPServer))
	for _, p := range []struct {
		tool            string
		hidden, missing map[string]any
		names           []string
	}{
		{tool, nil, nil, nil},
		{"core_service_start", map[string]any{nameKey: keyB}, map[string]any{nameKey: missingKey}, []string{keyB, missingKey}},
		{"core_auth_login", map[string]any{serverKey: keyB}, map[string]any{serverKey: missingKey}, []string{keyB, missingKey}},
	} {
		missingName, names := p.tool, p.names
		if p.hidden == nil {
			// The tool itself: the same tool of a server that does not exist.
			missingName, names = missingTool, []string{tool, missingTool}
		}
		answer, err := answeredAsMissing(sessions[0], p.tool, missingName, p.hidden, p.missing, names...)
		if err != nil {
			return nil, fmt.Errorf("M2: %s's %w", alice.Email, err)
		}
		note("%s %s: %s", alice.Email, p.tool, answer)
	}
	out = append(out, fmt.Sprintf("PASS: %s's call of %s (%s holds its tools), core_service_start and core_auth_login for %s are answered as for a server that does not exist", alice.Email, tool, bob.Email, keyB))

	step("M3. A server of %s that asks muster for an identity is not started: each of %s reads %s with the reason", a, refusedFields(m.cfg), mcpServerStateFail)
	for _, r := range refusedTenantServers(m.cfg) {
		if err := applyTenantManifest(tenantMCPServerManifest(a, tenantsMCPRefusedPrefix+r.name, r.spec)); err != nil {
			return nil, fmt.Errorf("M3: the server with %s: %w", r.field, err)
		}
	}
	for _, r := range refusedTenantServers(m.cfg) {
		name := tenantsMCPRefusedPrefix + r.name
		lastError, err := waitTenantMCPServerState(a, name, mcpServerStateFail)
		if err != nil {
			return nil, fmt.Errorf("M3: %w", err)
		}
		if !strings.Contains(lastError, musterIdentityRefusal) || !strings.Contains(lastError, r.field+" is not allowed") {
			return nil, fmt.Errorf("M3: %s/%s reads %s with %q, want muster's refusal of %s (%q)", a, name, mcpServerStateFail, lastError, r.field, musterIdentityRefusal)
		}
		note("%s: %s", name, excerpt(lastError, 160))
	}
	out = append(out, fmt.Sprintf("PASS: in %s, a server with %s each reads %s with muster's refusal naming the field", a, refusedFields(m.cfg), mcpServerStateFail))

	step("M4. %s signs in to %s through %s; %s of muster's namespace, the same issuer, still needs its own sign-in; as %s (sees both organizations), a sign-in to %s leaves %s needing its own", alice.Email, keyA, tenantCallbackPath(a), oauthFixtureServer, m.admin.Email, keyA, keyB)
	if err := signInToTenantServer(m.cfg, sessions[0], alice, a); err != nil {
		return nil, fmt.Errorf("M4: %w", err)
	}
	aliceTools, err := waitTenantTools(sessions[0], tenantToolPrefix(a, tenantsMCPServer))
	if err != nil {
		return nil, fmt.Errorf("M4: %s after the sign-in to %s: %w", alice.Email, keyA, err)
	}
	// A sign-in to lab-oauth-fixture another proof left behind would read
	// as a leak: both people sign out of it first.
	signOutOf(sessions[0], oauthFixtureServer)
	signOutOf(adminSession, oauthFixtureServer)
	if err := stillNeedsSignIn(m.cfg, sessions[0], oauthFixtureServer, "x_"+oauthFixtureServer+"_", oauthProxyCallbackPath); err != nil {
		return nil, fmt.Errorf("M4: after %s's sign-in to %s: %w", alice.Email, keyA, err)
	}
	note("%s lists %d tools of %s; %s still awaits a sign-in on %s", alice.Email, len(aliceTools), keyA, oauthFixtureServer, oauthProxyCallbackPath)
	if err := signInToTenantServer(m.cfg, adminSession, m.admin, a); err != nil {
		return nil, fmt.Errorf("M4: %w", err)
	}
	if _, err := waitTenantTools(adminSession, tenantToolPrefix(a, tenantsMCPServer)); err != nil {
		return nil, fmt.Errorf("M4: %s after the sign-in to %s: %w", m.admin.Email, keyA, err)
	}
	if err := stillNeedsSignIn(m.cfg, adminSession, keyB, tenantToolPrefix(b, tenantsMCPServer), tenantCallbackPath(b)); err != nil {
		return nil, fmt.Errorf("M4: after %s's sign-in to %s: %w", m.admin.Email, keyA, err)
	}
	if err := stillNeedsSignIn(m.cfg, adminSession, oauthFixtureServer, "x_"+oauthFixtureServer+"_", oauthProxyCallbackPath); err != nil {
		return nil, fmt.Errorf("M4: after %s's sign-in to %s: %w", m.admin.Email, keyA, err)
	}
	note("%s: %s connected; %s and %s still await their own sign-in", m.admin.Email, keyA, keyB, oauthFixtureServer)
	out = append(out, fmt.Sprintf("PASS: a sign-in to %s goes through %s and stays there: %s of muster's namespace and %s, of the same issuer, still need their own sign-in, each on its own callback", keyA, tenantCallbackPath(a), oauthFixtureServer, keyB))

	m5, err := m.agentTurn(sessions[0].token, keyA, keyB)
	if err != nil {
		return nil, err
	}
	out = append(out, m5)

	return out, nil
}

// agentTurn is step M5: the editor creates the first organization's agent
// bound to muster with the whole catalogue, and a turn as its tenant user,
// with the token of the user's sign-in to the organization's server (step
// M4), reports what muster lists the user through the agent. The agent goes
// again at the end, so the namespace's last agent is still step 6's.
func (m *tenantsMuster) agentTurn(userToken, keyA, keyB string) (string, error) {
	a, b := m.namespaces[0], m.namespaces[1]
	user := m.users[0]
	step("M5. %s creates %s/%s (toolset %s); a turn as %s: the agent reports tools of %s and of muster's namespace, none of %s", m.editor.Email, a, tenantsTestMusterAgent, presetFull, user.Email, keyA, keyB)
	editorToken, err := passwordGrant(m.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, m.editor.Email, m.editor.Password, musterLoginScopes)
	if err != nil {
		return "", err
	}
	spec := tenantsTestAgentSpec(a, tenantsTestMusterAgent, defaultModelConfig)
	spec[toolsetKey] = []string{presetFull}
	status, raw, err := m.api.call(http.MethodPost, "/agents", editorToken, spec)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("M5: %s's create of %s in %s answered %d, want 201: %s", m.editor.Email, tenantsTestMusterAgent, a, status, excerpt(string(raw), 300))
	}
	defer func() {
		if status, raw, err := m.api.call(http.MethodDelete, "/agents/"+a+"/"+tenantsTestMusterAgent, editorToken, nil); err != nil || status != http.StatusOK {
			note("M5: the delete of %s/%s answered %d (%v): %s", a, tenantsTestMusterAgent, status, err, excerpt(string(raw), 200))
		}
	}()
	if err := m.api.waitAgentReady(a, tenantsTestMusterAgent, editorToken); err != nil {
		return "", fmt.Errorf("M5: %w", err)
	}
	reply, err := agentTurnOf(m.cfg, &apiv1alpha1.ResourceReference{Namespace: a, Name: tenantsTestMusterAgent}, userToken, toolListingPrompt)
	if err != nil {
		return "", fmt.Errorf("M5: %w", err)
	}
	names := toolNamesInReply(reply)
	own := toolsWithPrefix(names, tenantToolPrefix(a, tenantsMCPServer))
	platform := toolsWithPrefix(names, agentManagerToolPrefix)
	other := toolsWithPrefix(names, tenantToolPrefix(b, tenantsMCPServer))
	switch {
	case len(own) == 0:
		return "", fmt.Errorf("M5: the agent reports no tool of %s, which %s signed in to (reply: %s)", keyA, user.Email, excerpt(reply, 300))
	case len(platform) == 0:
		return "", fmt.Errorf("M5: the agent reports no tool %s* of muster's namespace (reply: %s)", agentManagerToolPrefix, excerpt(reply, 300))
	case len(other) > 0:
		return "", fmt.Errorf("M5: the agent reports %v of %s, another organization's server", other, keyB)
	}
	note("the agent reports %d tools: %d of %s, %d of agent-manager, none of %s", len(names), len(own), keyA, len(platform), keyB)
	// The tools show only a token: the person has no sign-in to the other
	// organization's server, so no tool of it could show anyway. The
	// servers the agent lists show what muster lets the person see.
	reply, err = agentTurnOf(m.cfg, &apiv1alpha1.ResourceReference{Namespace: a, Name: tenantsTestMusterAgent}, userToken, serverListingPrompt)
	if err != nil {
		return "", fmt.Errorf("M5: %w", err)
	}
	servers := serverRefsInReply(reply)
	switch {
	case !servers[keyA]:
		return "", fmt.Errorf("M5: the agent's core_mcpserver_list does not list %s (servers: %s; reply: %s)", keyA, servers, excerpt(reply, 300))
	case !servers[platformNamespace+"/"+agentManagerMCPServer]:
		return "", fmt.Errorf("M5: the agent's core_mcpserver_list does not list %s/%s of muster's namespace (servers: %s; reply: %s)", platformNamespace, agentManagerMCPServer, servers, excerpt(reply, 300))
	case servers[keyB]:
		return "", fmt.Errorf("M5: the agent's core_mcpserver_list lists %s, another organization's server (servers: %s)", keyB, servers)
	}
	note("the agent lists the servers %s", servers)
	return fmt.Sprintf("PASS: through %s/%s, a turn as %s reports the tools of %s and of muster's namespace, none of %s, and lists the servers of muster's namespace and %s, not %s", a, tenantsTestMusterAgent, user.Email, keyA, keyB, keyA, keyB), nil
}

// serverListingPrompt is the turn of step M5 that has the agent list the
// servers muster shows its person.
const serverListingPrompt = "Call the tool core_mcpserver_list exactly once, through your call_tool tool with the arguments {}. " +
	"Then reply with ONLY each server of its result as namespace/name (its namespace field, a slash, its name field), one per line, no other words. " +
	"If you cannot call it, reply with exactly NO_SERVERS."

// serverRefsInReply extracts the namespace/name servers an agent reported.
func serverRefsInReply(reply string) refSet {
	set := refSet{}
	for _, field := range strings.FieldsFunc(reply, func(r rune) bool { return r == '\n' || r == ',' || r == ' ' || r == '`' || r == '*' || r == '\t' }) {
		field = strings.Trim(field, "-.;:()[]\"'")
		if ns, name, ok := strings.Cut(field, "/"); ok && ns != "" && name != "" && !strings.Contains(name, "/") {
			set[field] = true
		}
	}
	return set
}

// noticeOf words a listing's notice for an error, "" without one.
func noticeOf(l *musterListing) string {
	if l == nil || l.notice == "" {
		return ""
	}
	return "; muster's notice: " + l.notice
}

// refusedFields names the fields M3 sets, for a step line.
func refusedFields(cfg *config.Config) string {
	var fields []string
	for _, r := range refusedTenantServers(cfg) {
		fields = append(fields, r.field)
	}
	return strings.Join(fields, ", ")
}

// unbindTenantGroup removes the tenant group's RoleBinding in the
// namespace.
func unbindTenantGroup(ns string) error {
	gvr, err := gvrFor("rolebindings.rbac.authorization.k8s.io")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	return deleteObject(ctx, gvr, ns, tenantUsersBinding, 0)
}
