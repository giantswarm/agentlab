package lab

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// agentsTestAgent is the throwaway agent the proof creates, updates and
// deletes through agent-manager.
const agentsTestAgent = "agentlab-agents-test"

// agentManagerServiceAccount is the ServiceAccount the umbrella's agent-manager
// Deployment runs with (fullnameOverride: agent-manager).
const agentManagerServiceAccount = "system:serviceaccount:" + platformNamespace + ":" + agentManagerMCPServer

// AgentsTest is the headless proof that agent-manager acts as the signed-in
// user, through the platform path only (Dex id_token -> muster -> call_tool
// x_agent-manager_*): get_info reports identity caller; the admin's create ->
// ready -> update -> delete round trip succeeds with requestedBy set; a
// viewers-group user's create is refused by the kind apiserver as
// User "oidc:viewer@lab.local" (the view role writes no HelmReleases); and the
// agent-manager ServiceAccount holds nothing beyond API discovery. Leaves
// nothing behind.
func AgentsTest(cfg *config.Config, email string) error {
	if !cfg.Platform.Enabled || !cfg.Platform.Agents {
		return fmt.Errorf("platform.agents is off in %s — enable it and run `agentlab platform` first", config.File)
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}
	toolPrefix := "x_" + agentManagerMCPServer + "_"

	step("Logging in to Dex as %s", email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	note("got an id_token")

	step("MCP tools through muster (%s*)", toolPrefix)
	session, err := openMusterSession(cfg, token, "agents-test")
	if err != nil {
		return err
	}
	tools, err := session.listTools()
	if err != nil {
		return err
	}
	var amTools []string
	for _, t := range tools {
		if strings.HasPrefix(t, toolPrefix) {
			amTools = append(amTools, strings.TrimPrefix(t, toolPrefix))
		}
	}
	if len(amTools) == 0 {
		return fmt.Errorf("muster aggregates no %s tools; check `kubectl -n %s get mcpservers.muster.giantswarm.io %s` and `agentlab logs muster`", toolPrefix, platformNamespace, agentManagerMCPServer)
	}
	slices.Sort(amTools)
	note("%d tools: %s", len(amTools), strings.Join(amTools, ", "))

	step("%sget_info — expecting identity caller (every Kubernetes call as the user)", toolPrefix)
	var info struct {
		Version      string          `json:"version"`
		Identity     string          `json:"identity"`
		Capabilities map[string]bool `json:"capabilities"`
		Namespaces   struct {
			Default string `json:"default"`
		} `json:"namespaces"`
	}
	if err := session.callServerJSON(toolPrefix+"get_info", nil, &info); err != nil {
		return err
	}
	if info.Identity != "caller" || !info.Capabilities["writesAsCaller"] {
		return fmt.Errorf("agent-manager reports identity=%q writesAsCaller=%v: it is not running with downstream OAuth (umbrella agent-manager.oauth.downstream)", info.Identity, info.Capabilities["writesAsCaller"])
	}
	note("version %s, identity %s, default namespace %s", info.Version, info.Identity, info.Namespaces.Default)

	step("%slist_model_configs", toolPrefix)
	var configs struct {
		ModelConfigs []struct {
			Name string `json:"name"`
		} `json:"modelConfigs"`
	}
	if err := session.callServerJSON(toolPrefix+"list_model_configs", nil, &configs); err != nil {
		return err
	}
	if len(configs.ModelConfigs) == 0 {
		return fmt.Errorf("no ModelConfig in %s — the kagent subchart's default one is missing", kagentNamespace)
	}
	modelConfig := configs.ModelConfigs[0].Name
	for _, mc := range configs.ModelConfigs {
		if mc.Name == "default-model-config" {
			modelConfig = mc.Name
		}
	}
	note("using ModelConfig %s", modelConfig)

	// A leftover from an aborted run would make the create a conflict.
	if _, err := session.callServerTool(toolPrefix+"get_agent", map[string]any{nameKey: agentsTestAgent}); err == nil {
		note("removing the leftover agent %s from an earlier run", agentsTestAgent)
		if _, err := session.callServerTool(toolPrefix+"delete_agent", map[string]any{nameKey: agentsTestAgent, "force": true}); err != nil {
			return err
		}
		if err := waitAgentGone(session, toolPrefix, agentsTestAgent); err != nil {
			return err
		}
	}

	step("%screate_agent %s as %s", toolPrefix, agentsTestAgent, user.Email)
	var created struct {
		RequestedBy string `json:"requestedBy"`
		Created     struct {
			HelmRelease   bool `json:"helmRelease"`
			OCIRepository bool `json:"ociRepository"`
		} `json:"created"`
	}
	if err := session.callServerJSON(toolPrefix+"create_agent", map[string]any{
		nameKey: agentsTestAgent, "modelConfig": modelConfig, "displayName": "agentlab agents-test",
		"description":   "Throwaway agent of `agentlab agents-test`; deleted by the same run.",
		"systemMessage": "Reply with exactly the word pong and nothing else.",
	}, &created); err != nil {
		return err
	}
	if !created.Created.HelmRelease {
		return fmt.Errorf("create_agent reported no HelmRelease written")
	}
	if created.RequestedBy != user.Email {
		return fmt.Errorf("create_agent carries requestedBy=%q, wanted %q: agent-manager did not learn the caller from the forwarded token", created.RequestedBy, user.Email)
	}
	note("HelmRelease written (OCIRepository created: %v), requestedBy=%s", created.Created.OCIRepository, created.RequestedBy)

	step("The HelmRelease belongs to the user, not the ServiceAccount (managedFields)")
	managers, err := outputQuiet("kubectl", "-n", kagentNamespace, "get", "helmrelease", agentsTestAgent,
		"-o", "jsonpath={range .metadata.managedFields[*]}{.manager}{'\\n'}{end}")
	if err != nil {
		return err
	}
	if !strings.Contains(managers, "agent-manager") {
		return fmt.Errorf("HelmRelease %s has no agent-manager field manager: %q", agentsTestAgent, managers)
	}
	events, _ := outputQuiet("kubectl", "-n", platformNamespace, "logs", "deploy/"+agentManagerMCPServer, "-c", agentManagerMCPServer, "--since=5m")
	if !strings.Contains(events, "caller="+user.Email) {
		return fmt.Errorf("agent-manager's log carries no `caller=%s` line for the create", user.Email)
	}
	note("agent-manager logged the write with caller=%s", user.Email)

	step("Waiting for %s to be ready (get_agent_status)", agentsTestAgent)
	var status struct {
		Verdict string `json:"verdict"`
		Summary string `json:"summary"`
	}
	ready := waitFor(40, 3*time.Second, func() bool {
		status.Verdict, status.Summary = "", ""
		if err := session.callServerJSON(toolPrefix+"get_agent_status", map[string]any{nameKey: agentsTestAgent}, &status); err != nil {
			return false
		}
		return status.Verdict == "ready"
	})
	if !ready {
		return fmt.Errorf("%s never reached ready (last: %s — %s);\ncheck `kubectl -n %s get helmrelease,agents.kagent.dev,pods`", agentsTestAgent, status.Verdict, status.Summary, kagentNamespace)
	}
	note("ready: %s", excerpt(status.Summary, 120))

	step("%supdate_agent as %s", toolPrefix, user.Email)
	var updated struct {
		RequestedBy string   `json:"requestedBy"`
		Changed     []string `json:"changed"`
	}
	if err := session.callServerJSON(toolPrefix+"update_agent", map[string]any{nameKey: agentsTestAgent, "description": "Updated by agentlab agents-test."}, &updated); err != nil {
		return err
	}
	if updated.RequestedBy != user.Email || !slices.Contains(updated.Changed, "agent.description") {
		return fmt.Errorf("update_agent: requestedBy=%q changed=%v", updated.RequestedBy, updated.Changed)
	}
	note("changed %v, requestedBy=%s", updated.Changed, updated.RequestedBy)

	// The user's identity, not a ServiceAccount: a viewer (the view
	// ClusterRole, no HelmRelease writes anywhere) is refused by the kind
	// apiserver under the user's own name. A shared ServiceAccount would let
	// both users through alike.
	viewer := cfg.FindUserInGroup("viewers")
	if viewer == nil {
		note("skipping the viewer proof: %s has no viewers-group user", config.File)
	} else {
		step("%screate_agent as %s — expecting the apiserver's Forbidden for User \"oidc:%s\"", toolPrefix, viewer.Email, viewer.Email)
		viewerToken, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
			viewer.Email, viewer.Password, musterLoginScopes)
		if err != nil {
			return err
		}
		viewerSession, err := openMusterSession(cfg, viewerToken, "agents-test-viewer")
		if err != nil {
			return err
		}
		text, err := viewerSession.callServerTool(toolPrefix+"create_agent", map[string]any{nameKey: agentsTestAgent + "-viewer", "modelConfig": modelConfig})
		switch {
		case err == nil:
			return fmt.Errorf("%s created an agent through agent-manager although the view role cannot write HelmReleases — agent-manager is not acting as the caller (ServiceAccount fallback?): %.200s", viewer.Email, text)
		case !strings.Contains(strings.ToLower(err.Error()), "forbidden"):
			return fmt.Errorf("%s: wanted the apiserver's Forbidden, got: %w", viewer.Email, err)
		case !strings.Contains(err.Error(), `User "oidc:`+viewer.Email+`"`):
			return fmt.Errorf("%s: Forbidden, but not under the user's own name (the apiserver saw someone else): %w", viewer.Email, err)
		}
		note("%s: %s", viewer.Email, excerpt(err.Error(), 200))
		if _, err := outputQuiet("kubectl", "-n", kagentNamespace, "get", "helmrelease", agentsTestAgent+"-viewer"); err == nil {
			return fmt.Errorf("HelmRelease %s-viewer exists although the create was refused", agentsTestAgent)
		}
	}

	step("The agent-manager ServiceAccount holds no RBAC (kubectl auth can-i --list --as=%s)", agentManagerServiceAccount)
	rules, err := outputQuiet("kubectl", "auth", "can-i", "--list", "--as="+agentManagerServiceAccount, "-n", kagentNamespace)
	if err != nil {
		return err
	}
	var granted []string
	for line := range strings.SplitSeq(rules, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "Resources" {
			continue
		}
		// Every authenticated principal has the selfsubject* reviews and the
		// discovery URLs; anything else is a permission of its own.
		if strings.HasPrefix(fields[0], "selfsubject") || strings.HasPrefix(fields[0], "[") {
			continue
		}
		granted = append(granted, line)
	}
	if len(granted) > 0 {
		return fmt.Errorf("the agent-manager ServiceAccount still holds permissions in %s:\n%s", kagentNamespace, strings.Join(granted, "\n"))
	}
	note("nothing beyond discovery and self-subject reviews")

	step("%sdelete_agent %s as %s", toolPrefix, agentsTestAgent, user.Email)
	var deleted struct {
		RequestedBy        string `json:"requestedBy"`
		HelmReleaseDeleted bool   `json:"helmReleaseDeleted"`
		OCIRepositoryKept  string `json:"ociRepositoryKept"`
	}
	if err := session.callServerJSON(toolPrefix+"delete_agent", map[string]any{nameKey: agentsTestAgent}, &deleted); err != nil {
		return err
	}
	if !deleted.HelmReleaseDeleted || deleted.RequestedBy != user.Email {
		return fmt.Errorf("delete_agent: helmReleaseDeleted=%v requestedBy=%q", deleted.HelmReleaseDeleted, deleted.RequestedBy)
	}
	note("HelmRelease deleted, requestedBy=%s%s", deleted.RequestedBy, kept(deleted.OCIRepositoryKept))
	if err := waitAgentGone(session, toolPrefix, agentsTestAgent); err != nil {
		return err
	}
	note("%s is gone", agentsTestAgent)

	fmt.Println()
	fmt.Printf("PASS: muster aggregates %s* and agent-manager reports identity caller\n", toolPrefix)
	fmt.Printf("PASS: %s created -> ready -> updated -> deleted %s through call_tool, every write requestedBy=%s and logged with caller=\n", user.Email, agentsTestAgent, user.Email)
	if viewer != nil {
		fmt.Printf("PASS: %s's create is Forbidden by the apiserver as User \"oidc:%s\" (user RBAC, not the ServiceAccount's)\n", viewer.Email, viewer.Email)
	}
	fmt.Printf("PASS: %s holds no permissions beyond discovery\n", agentManagerServiceAccount)
	return nil
}

func kept(reason string) string {
	if reason == "" {
		return ", OCIRepository deleted"
	}
	return ", OCIRepository kept (" + reason + ")"
}

// waitAgentGone polls list_agents until name is no longer listed: the
// HelmRelease uninstall is asynchronous (helm-controller finalizer).
func waitAgentGone(session *musterSession, toolPrefix, name string) error {
	gone := waitFor(30, 3*time.Second, func() bool {
		var list struct {
			Agents []struct {
				Name string `json:"name"`
			} `json:"agents"`
		}
		if err := session.callServerJSON(toolPrefix+"list_agents", nil, &list); err != nil {
			return false
		}
		for _, a := range list.Agents {
			if a.Name == name {
				return false
			}
		}
		return true
	})
	if !gone {
		return fmt.Errorf("%s is still listed after the delete (helm-controller uninstall pending?)", name)
	}
	return nil
}

// callServerJSON runs an aggregated server tool and decodes its JSON payload.
func (s *musterSession) callServerJSON(name string, args map[string]any, into any) error {
	text, err := s.callServerTool(name, args)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(text), into); err != nil {
		return fmt.Errorf("%s: payload is not the expected JSON: %w\n%.300s", name, err, text)
	}
	return nil
}
