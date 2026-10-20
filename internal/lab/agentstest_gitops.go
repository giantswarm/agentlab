package lab

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/agentlab/internal/config"
)

// agents-gitops-test is the Dev Portal's view of an agent applied from git,
// headless (docs/agents.md "Editing an agent applied from git"). Such an
// agent's HelmRelease carries the provenance labels a Flux Kustomization
// stamps; agent-manager reports it managed: gitops and refuses every live
// write with gitops_owned. The proof applies a release agent-manager composed
// with those labels and asserts, through muster as the person, that the
// platform's agent-manager reports it managed: gitops and answers the edit's
// dry run with gitops_owned in mode apply and unsupported in mode commit —
// the platform's agent-manager is pinned to no GitHub App, so it offers no
// commit mode. The pull request a pinned agent-manager opens needs a real
// GitHub App sign-in and is not part of the headless proof.

const (
	// agentsGitOpsAgent is the throwaway agent applied from git: a release
	// agent-manager composed, applied by the proof with the Flux labels.
	agentsGitOpsAgent = "agentlab-agents-gitops"
	// agentsGitOpsDescription is the change the edit carries.
	agentsGitOpsDescription = "edited through a pull request"
	// gitOpsManaged is get_agent's managed for a release applied from git.
	gitOpsManaged = "gitops"
)

// AgentsGitOpsTest is the headless proof that an agent applied from git is
// GitOps-owned, as the portal sees it: the platform's agent-manager composes
// the release, the proof applies it with the Flux labels and agent-manager
// reports it managed: gitops; validate_agent of a change refuses mode apply
// with gitops_owned and mode commit as unsupported, update_agent with
// gitops_owned, the live release untouched. Leaves nothing behind.
func AgentsGitOpsTest(cfg *config.Config, email string) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled || !cfg.Platform.Agents {
		return fmt.Errorf("platform.agents is off in %s — enable it and run `agentlab platform` first", config.File)
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}

	step("Logging in to Dex as %s", email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	session, err := openMusterSession(cfg, token, "agents-gitops-test")
	if err != nil {
		return err
	}
	platform := &modelManagerTools{session: session, server: agentManagerMCPServer}

	// Leftovers of an aborted run first, and the agent on every exit path.
	if agentExists(agentsGitOpsAgent) {
		note("cleanup: removing %s", agentsGitOpsAgent)
		if err := removeAgent(agentsGitOpsAgent); err != nil {
			return err
		}
	}
	defer func() {
		if err := removeAgent(agentsGitOpsAgent); err != nil {
			note("cleanup: %v", err)
		}
	}()

	step("%s: the release of %s as agent-manager composes it, the file the repository carries", platform.toolName("validate_agent"), agentsGitOpsAgent)
	modelConfig, err := agentManagerModelConfig(session)
	if err != nil {
		return err
	}
	var composed validateReport
	spec := map[string]any{nameKey: agentsGitOpsAgent, "modelConfig": modelConfig, "toolset": []string{agentsTestToolset}, "displayName": "Agentlab GitOps agent", descriptionKey: "applied from git"}
	if err := platform.getJSON("validate_agent", spec, &composed); err != nil {
		return err
	}
	if !composed.Valid || composed.Manifests.HelmRelease == "" || composed.Manifests.OCIRepository == "" {
		return fmt.Errorf("the create's dry run is not valid: %v", composed.Errors)
	}
	note("valid: HelmRelease %s/%s on ModelConfig %s, next to the chart source", kagentNamespace, agentsGitOpsAgent, modelConfig)

	step("Applying it with the provenance a Flux Kustomization stamps (%s=%s): an agent applied from git", gitopsFluxName, gitopsFluxFixture)
	if err := applyGitOpsAgent(composed.Manifests.HelmRelease); err != nil {
		return err
	}
	if err := expectManaged(platform, gitOpsManaged); err != nil {
		return err
	}

	live, err := readGitOpsAgentRelease()
	if err != nil {
		return err
	}
	edit := map[string]any{nameKey: agentsGitOpsAgent, validateModeUpdate: true, descriptionKey: agentsGitOpsDescription}
	withCommit := maps.Clone(edit)
	withCommit[modeArg] = modeCommit
	step("%s of the change on the platform's agent-manager, which offers no commit mode: gitops_owned in mode apply, unsupported in mode commit — as its writes answer", platform.toolName("validate_agent"))
	if err := expectRefusal(platform, "validate_agent", edit, "gitops_owned", "mode commit"); err != nil {
		return err
	}
	if err := expectRefusal(platform, "validate_agent", withCommit, "unsupported", "capabilities.commit"); err != nil {
		return err
	}

	if err := expectRefusal(platform, "update_agent", map[string]any{nameKey: agentsGitOpsAgent, descriptionKey: agentsGitOpsDescription}, "gitops_owned", "mode commit"); err != nil {
		return err
	}
	after, err := readGitOpsAgentRelease()
	if err != nil {
		return err
	}
	if after.GetResourceVersion() != live.GetResourceVersion() {
		return fmt.Errorf("a refused write changed the live HelmRelease %s (resourceVersion %s -> %s)", agentsGitOpsAgent, live.GetResourceVersion(), after.GetResourceVersion())
	}
	note("HelmRelease %s/%s at resourceVersion %s, as before", kagentNamespace, agentsGitOpsAgent, after.GetResourceVersion())

	fmt.Printf("PASS: %s is applied from git (managed %s); validate_agent of a change refuses mode apply with gitops_owned and mode commit as unsupported on the platform's agent-manager, which no GitHub App pins; update_agent refused, the live release untouched\n",
		agentsGitOpsAgent, gitOpsManaged)
	return nil
}

// applyGitOpsAgent applies the composed HelmRelease and stamps the Flux
// provenance on it: what a Kustomization leaves on every object it applies,
// and what makes agent-manager report the agent managed: gitops.
func applyGitOpsAgent(helmRelease string) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if _, err := applyManifests(ctx, []byte(helmRelease)); err != nil {
		return fmt.Errorf("applying the HelmRelease %s/%s: %w", kagentNamespace, agentsGitOpsAgent, err)
	}
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return err
	}
	body := fmt.Appendf(nil, `{"metadata":{"labels":{%q:%q,%q:%q}}}`, gitopsFluxName, gitopsFluxFixture, gitopsFluxNamespace, platformNamespace)
	if err := patchObject(ctx, gvr, kagentNamespace, agentsGitOpsAgent, types.MergePatchType, body); err != nil {
		return fmt.Errorf("labelling the HelmRelease %s/%s: %w", kagentNamespace, agentsGitOpsAgent, err)
	}
	note("HelmRelease %s/%s applied, %s=%s", kagentNamespace, agentsGitOpsAgent, gitopsFluxName, gitopsFluxFixture)
	return nil
}

// readGitOpsAgentRelease reads the proof's HelmRelease back.
func readGitOpsAgentRelease() (*unstructured.Unstructured, error) {
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	return getObject(ctx, gvr, kagentNamespace, agentsGitOpsAgent)
}

// expectManaged asserts get_agent reports the proof's agent managed as
// wanted.
func expectManaged(api *modelManagerTools, want string) error {
	var agent struct {
		Managed string `json:"managed"`
	}
	if err := api.getJSON("get_agent", map[string]any{nameKey: agentsGitOpsAgent}, &agent); err != nil {
		return err
	}
	if agent.Managed != want {
		return fmt.Errorf("%s reports %s managed=%q, wanted %s", api.toolName("get_agent"), agentsGitOpsAgent, agent.Managed, want)
	}
	note("%s: managed %s", api.toolName("get_agent"), agent.Managed)
	return nil
}

// expectRefusal asserts a tool refuses the call with code, its message
// naming mentions.
func expectRefusal(api *modelManagerTools, tool string, args map[string]any, code, mentions string) error {
	_, err := api.call(tool, args)
	if refusalCode(err) != code {
		return fmt.Errorf("%s with mode %v answered %v, wanted %s", api.toolName(tool), firstNonEmpty(fmt.Sprint(args[modeArg]), "apply"), err, code)
	}
	if !strings.Contains(err.Error(), mentions) {
		return fmt.Errorf("%s's %s refusal does not say %q: %v", api.toolName(tool), code, mentions, err)
	}
	note("%s: %s", api.toolName(tool), excerpt(err.Error(), 200))
	return nil
}
