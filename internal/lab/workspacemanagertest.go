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
// MCPServer CR and reads Connected, its Deployment is rolled out, and
// list_providers, called through muster as the admin, names every provider
// instance the lab configured (workspaceprovider.go) and nothing else — the
// fake, the github instance once its App and Secret are in place, or both.
// The manager runs with kagent's address: without it no session directory
// is ever cleaned up (every Workspace reads SessionsCleanedUp=False
// KagentNotConfigured).
// list_workspaces for the lab's Organization (config.WorkspaceOrganization)
// answers the admin, a member, and refuses the viewer as not a member: the
// values gave the manager the Organization and its member groups.

// workspaceManagerMCPServer is the chart's MCPServer CR for the component,
// whose tools muster exposes as x_workspace-manager_<tool>.
const workspaceManagerMCPServer = "workspace-manager"

// workspaceManagerToolPrefix is the prefix of the component's tools through
// muster.
const workspaceManagerToolPrefix = "x_" + workspaceManagerMCPServer + "_"

// workspaceManagerSkip is why the stage has no subject: workspaces off, or a
// platform chart without the workspaces values, which renders no
// workspace-manager.
func workspaceManagerSkip(cfg *config.Config) string {
	if !cfg.WorkspacesEnabled() {
		return "platform.workspaces is off in " + config.File
	}
	if !workspacesChartCarries(cfg) {
		return fmt.Sprintf("%s takes no workspaces values, no workspace-manager to prove", platformChartFor(cfg))
	}
	return ""
}

// workspaceProviderStatus is one entry of list_providers (the
// workspace-manager's connect.Status): the instance, its kind, whether the
// caller is connected to it and its connect link.
type workspaceProviderStatus struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Connected  bool   `json:"connected"`
	ConnectURL string `json:"connectURL"`
}

func (s workspaceProviderStatus) String() string {
	connected := "not connected"
	if s.Connected {
		connected = "connected"
	}
	return fmt.Sprintf("%s (%s, %s)", s.Name, s.Kind, connected)
}

// proveWorkspaceManager runs the stage and returns the instances as
// list_providers worded them.
func proveWorkspaceManager(cfg *config.Config) ([]string, error) {
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
	if err := waitMCPServerState(workspaceManagerMCPServer, mcpServerStateConnected); err != nil {
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
	step("Calling %slist_providers through muster as %s, expecting %s", workspaceManagerToolPrefix, admin.Email, strings.Join(want, ", "))
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		admin.Email, admin.Password, musterLoginScopes)
	if err != nil {
		return nil, err
	}
	session, err := openMusterSession(cfg, token, "platform-test-workspaces")
	if err != nil {
		return nil, err
	}
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
		if p.ConnectURL == "" {
			return nil, fmt.Errorf("list_providers lists %s without a connect link", p.Name)
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
	viewerToken, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		viewer.Email, viewer.Password, musterLoginScopes)
	if err != nil {
		return nil, err
	}
	viewerSession, err := openMusterSession(cfg, viewerToken, "platform-test-workspaces-viewer")
	if err != nil {
		return nil, err
	}
	env, err := viewerSession.callToolEnvelope(workspaceManagerToolPrefix+"list_workspaces", org)
	if err != nil {
		return nil, err
	}
	if !env.IsError || !strings.Contains(env.Content[0].Text, "not a member") {
		return nil, fmt.Errorf("list_workspaces for %s as %s answered %.300q, want a refusal as not a member: the viewers are outside the lab's Organization", config.WorkspaceOrganization, viewer.Email, env.Content[0].Text)
	}
	note("list_workspaces for %s as %s refused: %.200s", config.WorkspaceOrganization, viewer.Email, env.Content[0].Text)
	return worded, nil
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
