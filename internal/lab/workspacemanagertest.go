package lab

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

// platform-test's workspace-manager stage: the component the workspaces
// switch brings (docs/workspaces.md) is registered with muster by the chart's
// MCPServer CR and reachable (waitMCPServerReachable), its Deployment is rolled out, and
// list_providers, called through muster as the admin, names every provider
// instance the lab configured (workspaceprovider.go) and nothing else — the
// github and gitlab instances once their ids and Secrets are in place.
// With the agents on, the manager runs with kagent's address: without it no
// session directory is ever cleaned up (every Workspace reads
// SessionsCleanedUp=False KagentNotConfigured). A manager whose registration muster pins to a
// provider App (workspacemustergrant.go) shows its tools only to a person
// who granted that App in a browser: an admin who has not ends the stage at
// muster's sign-in link.
// list_workspaces for the lab's Organization (config.WorkspaceOrganization)
// answers the admin, a member, and keeps the viewer out: the workspace-manager
// refuses them as not a member (the values gave it the Organization and its
// member groups), or, before that, muster asks them to grant the pinned App
// with its sign-in link.

// workspaceManagerMCPServer is the chart's MCPServer CR for the component,
// whose tools muster exposes as x_workspace-manager_<tool>.
const workspaceManagerMCPServer = "workspace-manager"

// workspaceManagerToolPrefix is the prefix of the component's tools through
// muster.
const workspaceManagerToolPrefix = "x_" + workspaceManagerMCPServer + "_"

// workspaceManagerSkip is why the stage has no subject: workspaces off, or a
// platform chart without the workspaces values, which renders no
// workspace-manager. A chart the probe could not load is the probe's error,
// not a skip.
func workspaceManagerSkip(cfg *config.Config) (string, error) {
	if !cfg.WorkspacesEnabled() {
		return "platform.workspaces is off in " + config.File, nil
	}
	w, err := workspacesChartProbed(cfg)
	if err != nil {
		return "", fmt.Errorf("probing %s for its workspaces values: %w", platformChartFor(cfg), err)
	}
	if !w.Carries {
		return fmt.Sprintf("%s takes no workspaces values, no workspace-manager to prove", platformChartFor(cfg)), nil
	}
	return "", nil
}

// workspaceProviderStatus is one entry of list_providers: the instance, its
// kind, whether the caller is connected to it, and how they connect while
// they are not: the manager's own connect link before workspace-manager
// 0.10.0 (connectURL), how to grant its App in muster from it (signIn).
type workspaceProviderStatus struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Connected  bool   `json:"connected"`
	ConnectURL string `json:"connectURL"`
	SignIn     string `json:"signIn"`
}

func (s workspaceProviderStatus) String() string {
	connected := "not connected"
	if s.Connected {
		connected = "connected"
	}
	return fmt.Sprintf("%s (%s, %s)", s.Name, s.Kind, connected)
}

// workspaceManagerProof is what the stage ran: the instances as
// list_providers worded them and how list_workspaces kept the viewer out
// (viewer), or, when the admin had not granted the pinned provider App,
// where it stopped instead (ungranted, non-nil), before list_providers and
// list_workspaces.
type workspaceManagerProof struct {
	providers []string
	viewer    string
	ungranted *workspaceGrantStop
}

// workspaceGrantStop is the person without the pinned provider App's grant
// and muster's sign-in link asserted for them.
type workspaceGrantStop struct {
	pinned, email, link string
}

// proveWorkspaceManager runs the stage and returns what it proved.
func proveWorkspaceManager(cfg *config.Config) (*workspaceManagerProof, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	mcpServers, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return nil, err
	}
	registered, err := objectExists(ctx, mcpServers, platformNamespace, workspaceManagerMCPServer)
	if err != nil {
		return nil, err
	}
	if !registered {
		return nil, fmt.Errorf("no MCPServer %s in %s: the chart registers the workspace-manager with muster (workspace-manager.muster.mcpServer.enabled); check `kubectl -n %s get mcpservers.muster.giantswarm.io`", workspaceManagerMCPServer, platformNamespace, platformNamespace)
	}
	if err := waitMCPServerReachable(workspaceManagerMCPServer); err != nil {
		return nil, err
	}
	rollout, cancelRollout := context.WithTimeout(context.Background(), workerPoolRunningTimeout)
	err = waitDeploymentRolledOut(rollout, platformNamespace, workspaceManagerMCPServer, workerPoolRunningTimeout)
	cancelRollout()
	if err != nil {
		return nil, fmt.Errorf("the workspace-manager is not rolled out: %w", err)
	}
	note("deployment %s/%s rolled out", platformNamespace, workspaceManagerMCPServer)
	flags, err := workspaceManagerFlags(context.Background())
	if err != nil {
		return nil, err
	}
	if err := judgeKagentAddress(flags); err != nil {
		return nil, err
	}
	note("the session directories' cleanup lists kagent's Sessions at %s, a directory going %s after its last turn", flagValue(flags, sessionsKagentArg), cmp.Or(flagValue(flags, sessionsCleanupArg), "the manager's default"))

	// What the install configured: the instances the values rendered, read
	// the way the install reads them (the github instance's Secret keys
	// included), so the proof expects what the cluster was given.
	configured, err := workspaceProvidersInstall(ctx, cfg)
	if err != nil {
		return nil, err
	}
	want := workspaceProviderNames(configured)
	if len(want) == 0 {
		return nil, fmt.Errorf("the lab configured no provider instance (platform.workspaces.provider %q): nothing for list_providers to list", cfg.Platform.Workspaces.Provider)
	}

	admin, viewer, err := identityProofUsers(cfg)
	if err != nil {
		return nil, err
	}
	adminPerson, err := openWorkspacePerson(cfg, admin)
	if err != nil {
		return nil, err
	}
	viewerPerson, err := openWorkspacePerson(cfg, viewer)
	if err != nil {
		return nil, err
	}
	if pinned, err := workspaceManagerPinnedProvider(cfg); err == nil {
		if stop, err := grantForStage(pinned, adminPerson); err != nil || stop != nil {
			return &workspaceManagerProof{ungranted: stop}, err
		}
	}
	session := adminPerson.session
	names, err := session.listTools()
	if err != nil {
		return nil, err
	}
	tools, err := audienceManagerTools(names, workspaceManagerMCPServer)
	if err != nil {
		return nil, err
	}
	note("%s lists %d tools for %s", workspaceManagerMCPServer, len(tools), admin.Email)
	step("Calling %slist_providers through muster as %s, expecting %s", workspaceManagerToolPrefix, admin.Email, strings.Join(want, ", "))
	var listed struct {
		Providers []workspaceProviderStatus `json:"providers"`
	}
	if err := session.callServerStructured(workspaceManagerToolPrefix+"list_providers", nil, &listed); err != nil {
		return nil, err
	}
	var got, worded []string
	for _, p := range listed.Providers {
		got = append(got, p.Name)
		worded = append(worded, p.String())
		if i := slices.IndexFunc(configured, func(c workspaceProviderValues) bool { return c.Name == p.Name }); i >= 0 && p.Kind != configured[i].Kind {
			return nil, fmt.Errorf("list_providers lists %s with kind %q, want the chart's %s kind", p.Name, p.Kind, configured[i].Kind)
		}
		if !p.Connected && p.ConnectURL == "" && p.SignIn == "" {
			return nil, fmt.Errorf("list_providers lists %s, not connected, without a connect link or how to grant its App in muster", p.Name)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return nil, fmt.Errorf("list_providers lists %s, want every configured instance and nothing else: %s", strings.Join(got, ", "), strings.Join(want, ", "))
	}
	for _, w := range worded {
		note("%s", w)
	}

	org := map[string]any{wmOrganizationArg: config.WorkspaceOrganization}
	step("Calling %slist_workspaces for Organization %s as %s (a member) and as %s (not one)", workspaceManagerToolPrefix, config.WorkspaceOrganization, admin.Email, viewer.Email)
	var workspaces struct {
		Workspaces []struct {
			Name string `json:"name"`
		} `json:"workspaces"`
	}
	if err := session.callServerStructured(workspaceManagerToolPrefix+"list_workspaces", org, &workspaces); err != nil {
		return nil, fmt.Errorf("%w\n  the values give the workspace-manager the Organization %s with the member groups %s; check its --organization flag", err, config.WorkspaceOrganization, strings.Join(config.WorkspaceOrganizationGroups, ", "))
	}
	note("list_workspaces for %s as %s: %d workspaces", config.WorkspaceOrganization, admin.Email, len(workspaces.Workspaces))
	kept, err := viewerKeptOut(viewerPerson.session.callToolEnvelope(workspaceManagerToolPrefix+"list_workspaces", org))
	if err != nil {
		return nil, fmt.Errorf("list_workspaces for %s as %s: %w", config.WorkspaceOrganization, viewer.Email, err)
	}
	note("list_workspaces for %s as %s: %s", config.WorkspaceOrganization, viewer.Email, kept)
	return &workspaceManagerProof{providers: worded, viewer: kept}, nil
}

// viewerKeptOut judges the viewer's list_workspaces for the lab's
// Organization, whose member groups the viewers are outside: the
// workspace-manager's refusal as not a member (the viewer holds the pinned
// App's grant), or muster's auth_required with its sign-in link (they do
// not, so muster never reached the workspace-manager). It returns which,
// worded; any other answer is an error.
func viewerKeptOut(env *toolEnvelope, err error) (string, error) {
	if err == nil && env.IsError && strings.Contains(env.Content[0].Text, "not a member") {
		return fmt.Sprintf("refused as not a member: %.200s", env.Content[0].Text), nil
	}
	link, text := musterSignInLink(env, err)
	if link != "" {
		return "asked to grant the pinned App first, muster's sign-in link " + excerpt(link, 100), nil
	}
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("answered %.300q, want a refusal as not a member, or muster's auth_required with its sign-in link for a viewer without the pinned App's grant: the viewers are outside the lab's Organization", text)
}

// grantForStage asks whether the people hold the grant of the pinned
// provider App, which only a person in a browser makes: one without it ends
// the stage (stop, non-nil) with muster's sign-in link asserted for them.
// The stage asks it of the admin alone, whose calls need the grant; the
// viewer's list_workspaces is judged either way (viewerKeptOut).
func grantForStage(pinned string, people ...*workspacePerson) (stop *workspaceGrantStop, err error) {
	for _, p := range people {
		link, err := p.grantLink(pinned)
		if err != nil {
			return nil, err
		}
		if link != "" {
			note("pinned to the App of %s, which no headless walk grants: muster's sign-in link for %s is %s", pinned, p.user.Email, excerpt(link, 100))
			return &workspaceGrantStop{pinned: pinned, email: p.user.Email, link: link}, nil
		}
	}
	return nil, nil // granted in an earlier sign-in: the stage goes on
}

// workspaceManagerVerdict is the stage's lines in platform-test's verdict,
// claiming only what ran: a stop at the grant passes the registration and
// the sign-in link and skips list_providers and list_workspaces, with why.
func workspaceManagerVerdict(proof *workspaceManagerProof, agents bool) string {
	cleanup := ", cleans session directories up from kagent's Sessions,"
	if !agents {
		cleanup = " (no kagent: the agents are off)"
	}
	registered := fmt.Sprintf("\nPASS: the workspace-manager is registered with muster (MCPServer %s Connected)%s", workspaceManagerMCPServer, cleanup)
	if g := proof.ungranted; g != nil {
		return registered + fmt.Sprintf(" and asserts muster's sign-in link for %s, who has not granted the pinned App of %s", g.email, g.pinned) +
			fmt.Sprintf("\nSKIP: the workspace-manager's list_providers and list_workspaces for Organization %s — %s has not granted the pinned App of %s, which only a person in a browser grants (muster's sign-in link: %s)", config.WorkspaceOrganization, g.email, g.pinned, excerpt(g.link, 100))
	}
	return registered + fmt.Sprintf(" and lists its %d provider instances to the admin: %s; list_workspaces for Organization %s answers a member and keeps a viewer out (%s)", len(proof.providers), strings.Join(proof.providers, ", "), config.WorkspaceOrganization, proof.viewer)
}

// callServerStructured runs an aggregated server tool through muster and
// decodes its structuredContent, the machine-readable half of a result whose
// text is a summary (list_providers says "2 providers").
func (s *musterSession) callServerStructured(name string, args map[string]any, into any) error {
	env, err := s.callToolEnvelope(name, args)
	if err != nil {
		return err
	}
	if env.IsError {
		return fmt.Errorf("%s failed: %.300s", name, env.Content[0].Text)
	}
	if env.StructuredContent == nil {
		return fmt.Errorf("%s answered without structuredContent: %.300s", name, env.Content[0].Text)
	}
	raw, err := json.Marshal(env.StructuredContent)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%s: structuredContent is not the expected shape: %w\n%.300s", name, err, raw)
	}
	return nil
}
