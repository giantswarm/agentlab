package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/giantswarm/agentlab/internal/config"
)

// The muster surface of organizations-test (the muster half of the old
// tenants proof, giantswarm/agentlab#166, the agent-permissions plan's D9):
// with mcpServerNamespaceSelector (giantswarm/muster#1783) muster serves the MCPServers of the
// organization namespaces next to its own and lists them as the caller with
// the person's token, so a member lists none of the other organization's
// servers and every call naming one is answered as for a server that does
// not exist.
//
// The old proof's sign-in steps are not here: a server of an organization
// namespace that signs in needs muster to read its client Secret there, and
// the organizations' subject policy refuses a RoleBinding of a ServiceAccount
// of another namespace (muster's); its callback (/oauth/ns/<namespace>/callback,
// giantswarm/muster#1598) names a namespace the operator generates, which the
// lab Dex's static redirect URIs cannot list ahead.
const (
	// organizationsMCPServer is the server the proof declares in each
	// organization namespace: no auth, not started; listing it is what
	// RBAC decides.
	organizationsMCPServer = "agentlab-organizations-mcp"
	// organizationsMCPMissing names a server that exists in no namespace:
	// what an answer about another organization's server is compared with.
	organizationsMCPMissing = "agentlab-organizations-missing"
	// musterConfigMap is muster's rendered config.yaml.
	musterConfigMap = "muster-config"
	musterConfigKey = "config.yaml"
	// organizationsMusterSettle bounds how long a new MCPServer or
	// RoleBinding takes to reach a person's listing: muster keeps an answer
	// for 5 s per token, and registers a new server on its watch.
	organizationsMusterSettle = time.Minute
)

// tenantServerKey is the key muster lists and starts a server of an
// organization namespace under: <namespace>/<name>.
func tenantServerKey(ns, name string) string {
	return ns + "/" + name
}

// musterTenancy is what decides whether muster serves the proof's
// MCPServers in the organization namespaces and signs in to them: the
// selectors in its config.yaml (giantswarm/muster#1779) and its OAuth
// server's redirect URI guard.
type musterTenancy struct {
	// NamespaceSelector selects the namespaces besides muster's whose
	// MCPServers muster serves; nil when unset, which selects none.
	NamespaceSelector *metav1.LabelSelector `json:"mcpServerNamespaceSelector"`
	// ServerSelector selects the served MCPServers by their own labels; nil
	// when unset, which selects every one.
	ServerSelector *metav1.LabelSelector `json:"mcpServerSelector"`
	// Aggregator carries muster's OAuth server settings.
	Aggregator struct {
		OAuth struct {
			Server struct {
				// AllowPrivateIPRedirectURIs lets a client register a
				// redirect URI whose host resolves to a private address.
				AllowPrivateIPRedirectURIs bool `json:"allowPrivateIPRedirectURIs"`
			} `json:"server"`
		} `json:"oauth"`
	} `json:"aggregator"`
}

// selfSignInProblem words what keeps muster from registering itself with
// its own authorization server, the sign-in an organization namespace's
// server modelled on lab-oauth-fixture needs, "" when nothing does. In the
// lab muster's host resolves to the edge's cluster IP, so its redirect URI
// is on a private address, which muster's OAuth server refuses unless
// allowPrivateIPRedirectURIs is on.
func (t musterTenancy) selfSignInProblem() string {
	if t.Aggregator.OAuth.Server.AllowPrivateIPRedirectURIs {
		return ""
	}
	return "muster's OAuth server refuses redirect URIs on a private address (aggregator.oauth.server.allowPrivateIPRedirectURIs off), and in the lab muster's own host is one, so muster cannot register itself for the organization server's sign-in: `agentlab configure --klaus-gateway`, then `agentlab platform`, turns it on (HACKS.md U43)"
}

// problem words what keeps muster from serving the proof's MCPServer
// (labelled serverLabels) in each of namespaces (name to labels), "" when
// nothing does.
func (t musterTenancy) problem(namespaces map[string]labels.Set, serverLabels labels.Set) string {
	if t.NamespaceSelector == nil {
		return fmt.Sprintf("muster runs without mcpServerNamespaceSelector: it serves no organization namespace's MCPServer. The agent-platform chart is to set it on the label %s with agentPlatformOrganizations.enabled, on a muster range that carries giantswarm/muster#1783", organizationLabel)
	}
	selector, err := metav1.LabelSelectorAsSelector(t.NamespaceSelector)
	if err != nil {
		return fmt.Sprintf("muster's mcpServerNamespaceSelector is invalid: %v", err)
	}
	names := make([]string, 0, len(namespaces))
	for ns := range namespaces {
		names = append(names, ns)
	}
	sort.Strings(names)
	for _, ns := range names {
		if !selector.Matches(namespaces[ns]) {
			return fmt.Sprintf("muster's mcpServerNamespaceSelector (%s) does not select namespace %s (labels %s)", selector, ns, namespaces[ns])
		}
	}
	if t.ServerSelector != nil {
		servers, err := metav1.LabelSelectorAsSelector(t.ServerSelector)
		if err != nil {
			return fmt.Sprintf("muster's mcpServerSelector is invalid: %v", err)
		}
		if !servers.Matches(serverLabels) {
			return fmt.Sprintf("muster's mcpServerSelector (%s) does not select the proof's MCPServer (labels %s)", servers, serverLabels)
		}
	}
	return ""
}

// readMusterTenancy reads muster's config.yaml.
func readMusterTenancy() (musterTenancy, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	cm, err := getObject(ctx, gvrConfigMaps, platformNamespace, musterConfigMap)
	if err != nil {
		return musterTenancy{}, fmt.Errorf("muster's ConfigMap %s/%s: %w", platformNamespace, musterConfigMap, err)
	}
	file, _, _ := unstructured.NestedString(cm.Object, "data", musterConfigKey)
	return parseMusterTenancy(file)
}

// parseMusterTenancy reads the MCPServer selectors and the OAuth server's
// redirect URI guard of muster's config.yaml.
func parseMusterTenancy(file string) (musterTenancy, error) {
	var t musterTenancy
	if err := sigsyaml.Unmarshal([]byte(file), &t); err != nil {
		return musterTenancy{}, fmt.Errorf("muster's %s: %w", musterConfigKey, err)
	}
	return t, nil
}

// namespaceLabels reads the labels of namespaces.
func namespaceLabels(namespaces []string) (map[string]labels.Set, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	out := make(map[string]labels.Set, len(namespaces))
	for _, ns := range namespaces {
		obj, err := getObject(ctx, gvrNamespaces, "", ns)
		if err != nil {
			return nil, fmt.Errorf("namespace %s: %w", ns, err)
		}
		out[ns] = labels.Set(obj.GetLabels())
	}
	return out, nil
}

// organizationMCPServerLabels are the labels of the proof's MCPServer.
var organizationMCPServerLabels = labels.Set{organizationsTestLabel: organizationsTestMark}

// organizationMCPServerManifest is the proof's MCPServer of an
// organization namespace: muster's own /mcp, no auth, not started.
func organizationMCPServerManifest(ns string) string {
	return fmt.Sprintf(`apiVersion: muster.giantswarm.io/v1alpha1
kind: MCPServer
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
spec:
  type: streamable-http
  url: %s
  autoStart: false
`, organizationsMCPServer, ns, organizationsTestLabel, organizationsTestMark, oauthFixtureURL)
}

// applyOrganizationManifest applies a manifest of the proof's fixtures.
func applyOrganizationManifest(manifest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	_, err := applyManifests(ctx, []byte(manifest))
	return err
}

// removeOrganizationMCPServer deletes the proof's MCPServer, best effort.
func removeOrganizationMCPServer(ns string) {
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		note("cleanup: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if err := deleteObject(ctx, gvr, ns, organizationsMCPServer, kubeReadTimeout); err != nil {
		note("cleanup: %v", err)
	}
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

// waitMusterListing reads the session's view until the servers are the
// wanted ones, bounded by organizationsMusterSettle, and returns the view and the
// difference left at the deadline.
func waitMusterListing(s *musterSession, want refSet) (*musterListing, string, error) {
	var l *musterListing
	var diff string
	var readErr error
	waitFor(int(organizationsMusterSettle/pollInterval), pollInterval, func() bool {
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

// noticeOf words a listing's notice for an error, "" without one.
func noticeOf(l *musterListing) string {
	if l == nil || l.notice == "" {
		return ""
	}
	return "; muster's notice: " + l.notice
}

// organizationsMuster is what the muster surface works with: the two
// organization namespaces and their members.
type organizationsMuster struct {
	cfg        *config.Config
	namespaces [2]string
	users      [2]*config.User
}

// checks are the muster surface's negative tests.
func (m *organizationsMuster) checks() []organizationCheck {
	return []organizationCheck{
		{surface: surfaceMuster, boundary: "muster lists a member's MCP servers as the member: none of the other organization's, and not their own once their binding goes",
			run: m.listing},
		{surface: surfaceMuster, boundary: "muster answers a member's call naming the other organization's MCP server as for a server that does not exist",
			run: m.unseenCalls},
	}
}

// login opens a muster session as the member.
func (m *organizationsMuster) login(u *config.User) (*musterSession, error) {
	token, err := passwordGrant(m.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, u.Email, u.Password, musterLoginScopes)
	if err != nil {
		return nil, err
	}
	return openMusterSession(m.cfg, token, "organizations-test")
}

// listing: each member lists the servers of muster's namespace and their
// own organization's, nothing of the other's; with their binding gone a
// moment, their own goes too (the twin that the listing is RBAC's).
func (m *organizationsMuster) listing() (string, error) {
	tenancy, err := readMusterTenancy()
	if err != nil {
		return "", err
	}
	nsLabels, err := namespaceLabels(m.namespaces[:])
	if err != nil {
		return "", err
	}
	if problem := tenancy.problem(nsLabels, organizationMCPServerLabels); problem != "" {
		return "", fmt.Errorf("%s", problem)
	}
	for _, ns := range m.namespaces {
		if err := applyOrganizationManifest(organizationMCPServerManifest(ns)); err != nil {
			return "", err
		}
		defer removeOrganizationMCPServer(ns)
	}
	platform, err := platformMCPServers()
	if err != nil {
		return "", err
	}
	var seen []string
	var sessions [2]*musterSession
	for i, u := range m.users {
		s, err := m.login(u)
		if err != nil {
			return "", err
		}
		sessions[i] = s
		want := refSet{tenantServerKey(m.namespaces[i], organizationsMCPServer): true}
		for ref := range platform {
			want[ref] = true
		}
		l, diff, err := waitMusterListing(s, want)
		if err != nil {
			return "", fmt.Errorf("%s's listing: %w", u.Email, err)
		}
		if diff != "" {
			return "", fmt.Errorf("%s (group %s, bound in %s) lists the servers %s after %s; want %s (%s)%s", u.Email, u.Groups[0], m.namespaces[i], l.servers, organizationsMusterSettle, want, diff, noticeOf(l))
		}
		seen = append(seen, fmt.Sprintf("%s lists %s", u.Email, l.servers))
	}
	bob, b := m.users[1], m.namespaces[1]
	if err := unbindOrganizationMembers(b); err != nil {
		return "", err
	}
	l, diff, err := waitMusterListing(sessions[1], platform)
	rebindErr := bindOrganizationRole(b, organizationsTestBinding, organizationEditorRole, []rbacv1.Subject{groupSubject(bob.Groups[0])})
	switch {
	case err != nil:
		return "", fmt.Errorf("%s's listing: %w", bob.Email, err)
	case rebindErr != nil:
		return "", rebindErr
	case diff != "":
		return "", fmt.Errorf("%s, with RoleBinding %s/%s gone, lists %s after %s; want %s only (%s)%s", bob.Email, b, organizationsTestBinding, l.servers, organizationsMusterSettle, platform, diff, noticeOf(l))
	}
	seen = append(seen, fmt.Sprintf("%s, with their binding gone a moment, lists %s only", bob.Email, platform))
	return strings.Join(seen, "; "), nil
}

// unseenCalls: alice's core_service_start and core_auth_login for bob's
// organization's server are answered as for a server that does not exist.
func (m *organizationsMuster) unseenCalls() (string, error) {
	a, b := m.namespaces[0], m.namespaces[1]
	for _, ns := range m.namespaces {
		if err := applyOrganizationManifest(organizationMCPServerManifest(ns)); err != nil {
			return "", err
		}
		defer removeOrganizationMCPServer(ns)
	}
	alice := m.users[0]
	s, err := m.login(alice)
	if err != nil {
		return "", err
	}
	// The twin: alice's own organization's server is listed to her.
	own := tenantServerKey(a, organizationsMCPServer)
	listed, l, err := waitMusterListingHas(s, own)
	switch {
	case err != nil:
		return "", err
	case !listed:
		return "", fmt.Errorf("the twin: %s does not list their own organization's %s within %s (servers %s)", alice.Email, own, organizationsMusterSettle, l.servers)
	}
	keyB := tenantServerKey(b, organizationsMCPServer)
	missingKey := tenantServerKey(b, organizationsMCPMissing)
	var answers []string
	for _, p := range []struct {
		tool, arg string
	}{{"core_service_start", nameKey}, {"core_auth_login", serverKey}} {
		answer, err := answeredAsMissing(s, p.tool, p.tool, map[string]any{p.arg: keyB}, map[string]any{p.arg: missingKey}, keyB, missingKey)
		if err != nil {
			return "", fmt.Errorf("%s's %w", alice.Email, err)
		}
		answers = append(answers, fmt.Sprintf("%s %s", p.tool, answer))
	}
	return fmt.Sprintf("%s's calls naming %s read as for a server that does not exist: %s", alice.Email, keyB, strings.Join(answers, "; ")), nil
}

// waitMusterListingHas waits until the session lists the server.
func waitMusterListingHas(s *musterSession, key string) (bool, *musterListing, error) {
	var l *musterListing
	var readErr error
	found := waitFor(int(organizationsMusterSettle/pollInterval), pollInterval, func() bool {
		l, readErr = readMusterListing(s)
		return readErr == nil && l.servers[key]
	})
	return found, l, readErr
}

// unbindOrganizationMembers removes the members' RoleBinding in the
// organization namespace.
func unbindOrganizationMembers(ns string) error {
	gvr, err := gvrFor("rolebindings.rbac.authorization.k8s.io")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	return deleteObject(ctx, gvr, ns, organizationsTestBinding, 0)
}
