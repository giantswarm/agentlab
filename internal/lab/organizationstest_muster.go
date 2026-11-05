package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The muster surface of organizations-test (the muster half of the old
// tenants proof, giantswarm/agentlab#166, the agent-permissions plan's D9):
// with mcpServersInAllNamespaces muster serves the MCPServers of the
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
	// tenantsMusterMinimum is the first muster with giantswarm/muster#1603.
	tenantsMusterMinimum = "5.48.0"
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
		return fmt.Sprintf("muster %s is older than organizations-test needs: it needs %s or later (giantswarm/muster#1601, #1603); the agent-platform chart's muster range starts there from 4.157.1 on", tag, tenantsMusterMinimum)
	}
	if !t.allNamespaces {
		return "muster runs without mcpServersInAllNamespaces: it serves no organization namespace's MCPServer. `agentlab platform` with this agentlab sets muster.muster.mcpServersInAllNamespaces with platform.organizations on (a platform.valuesFiles overlay that sets it false wins)"
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
	if problem := tenancy.problem(); problem != "" {
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
