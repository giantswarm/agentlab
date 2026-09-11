package lab

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The edit half of the portal proof (the agent detail page's kebab: Edit,
// Update skills, Delete — giantswarm/backstage's agent-platform plugin over
// agent-manager's tools through muster as the person): get_agent reports the
// pins written at create; validate_agent{update} is the edit dialog's dry
// run; update_agent changes only what was sent; refreshSkills re-pins every
// git skill to the head of its repository's default branch — the same head
// skill discovery resolved — and nothing else; delete_agent removes the
// release and its render and keeps the namespace's shared chart source while
// another release of the chart needs it; a viewer's write is the apiserver's
// Forbidden through agent-manager. A GitOps-owned release would be refused as
// a conflict — the lab has no such fixture, which the proof says.

const (
	editedDescription = "Edited by agentlab backstage-test."
	// agentDescriptionPath is the changed path update_agent reports for a
	// new description (the chart value).
	agentDescriptionPath = "agent.description"
)

// deleteReport is delete_agent's answer as the delete dialog reads it.
type deleteReport struct {
	RequestedBy          string `json:"requestedBy"`
	HelmReleaseDeleted   bool   `json:"helmReleaseDeleted"`
	OCIRepositoryDeleted bool   `json:"ociRepositoryDeleted"`
	OCIRepositoryKept    string `json:"ociRepositoryKept"`
}

// portalUpdateAgent is update_agent through the portal with the given
// arguments (the changed fields only, as the edit dialog sends them).
func portalUpdateAgent(ps *portalSession, args map[string]any) (*agentManagerUpdateResult, error) {
	args[namespaceKey] = kagentNamespace
	var updated agentManagerUpdateResult
	if err := portalToolCall(ps, "update_agent", args, &updated); err != nil {
		return nil, err
	}
	return &updated, nil
}

// portalDeleteAgent is delete_agent through the portal (never force).
func portalDeleteAgent(ps *portalSession, name string) (*deleteReport, error) {
	var deleted deleteReport
	if err := portalToolCall(ps, "delete_agent", map[string]any{nameKey: name, namespaceKey: kagentNamespace}, &deleted); err != nil {
		return nil, err
	}
	return &deleted, nil
}

// portalSkillHead is list_skills through the portal: the head commit of the
// repository's default branch, the commit refreshSkills re-pins to.
func portalSkillHead(ps *portalSession, repo string) (string, error) {
	var listed struct {
		Repositories []struct {
			RepoURL string `json:"repoUrl"`
			Commit  string `json:"commit"`
			Error   string `json:"error"`
		} `json:"repositories"`
	}
	if err := portalToolCall(ps, "list_skills", map[string]any{"repository": repo}, &listed); err != nil {
		return "", err
	}
	for _, r := range listed.Repositories {
		if strings.TrimSuffix(r.RepoURL, "/") != strings.TrimSuffix(repo, "/") {
			continue
		}
		if r.Error != "" {
			return "", fmt.Errorf("list_skills could not read %s: %s", repo, r.Error)
		}
		if !fullCommitID.MatchString(r.Commit) {
			return "", fmt.Errorf("list_skills reports %q as %s's head, not a full commit id", r.Commit, repo)
		}
		return r.Commit, nil
	}
	return "", fmt.Errorf("list_skills lists nothing for %s", repo)
}

// proveEditPath is the detail page's writes as the primary user on the agent
// the create path left Ready: E1 get_agent, E2/E3 the description edit (dry
// run, then the write changing that one path), E4/E5 refreshSkills (dry run,
// then the write; the pin is the head already — discovery read it there —
// so nothing changes and the pins stay), E6 readiness after, E8 the viewer's
// writes forbidden, E7 delete while another release of the chart exists
// (the source kept, named), E9 the last release's delete recorded. E10 (a
// GitOps-owned release refused as a conflict) has no fixture in the lab.
// other names the second release of the chart the delete keeps the source
// for; it is deleted last, through agent-manager too.
func proveEditPath(primary, viewer *portalSession, spec agentSpec, other string) ([]string, error) {
	var verdicts []string
	email := primary.user.Email
	skill := spec.Skills[0]

	step("E1 %sget_agent reports the pins written at create", agentManagerToolPrefix)
	var got agentManagerAgent
	if err := portalToolCall(primary, "get_agent", map[string]any{nameKey: spec.Name, namespaceKey: kagentNamespace}, &got); err != nil {
		return nil, err
	}
	if len(got.Skills) != 1 || got.Skills[0].Git == nil || got.Skills[0].Git.Commit != skill.Git.Commit {
		return nil, fmt.Errorf("get_agent reports skills=%+v, wanted %s pinned at %s", got.Skills, skill.Name, skill.Git.Commit)
	}
	if !slices.Equal(got.Toolset, spec.Toolset) || got.Managed != "helmrelease" || got.Ready == nil || !*got.Ready {
		return nil, fmt.Errorf("get_agent reports toolset=%v managed=%q ready=%v, wanted %v/helmrelease/true", got.Toolset, got.Managed, got.Ready, spec.Toolset)
	}
	note("skill %s @ %.12s (a commit, never a branch), toolset %v, managed %s, ready", skill.Name, got.Skills[0].Git.Commit, got.Toolset, got.Managed)
	verdicts = append(verdicts, fmt.Sprintf("PASS: E1 get_agent reports skill %s pinned at %.12s, toolset %v, managed helmrelease, ready", skill.Name, got.Skills[0].Git.Commit, got.Toolset))

	step("E2 the edit dialog's dry run: %svalidate_agent{update} with a new description", agentManagerToolPrefix)
	var dry validateReport
	if err := portalToolCall(primary, "validate_agent", map[string]any{nameKey: spec.Name, namespaceKey: kagentNamespace, "update": true, descriptionKey: editedDescription}, &dry); err != nil {
		return nil, err
	}
	if !dry.Valid || len(dry.Errors) > 0 || dry.Mode != validateModeUpdate {
		return nil, fmt.Errorf("validate_agent{update}: valid=%v mode=%q errors=%v", dry.Valid, dry.Mode, dry.Errors)
	}
	if got := agentValue(dry.Manifests.Values, descriptionKey); got != editedDescription {
		return nil, fmt.Errorf("the update dry run renders agent.description %q, wanted %q", got, editedDescription)
	}
	if got := skillCommits(dry.Manifests.Values)[skill.Name]; got != skill.Git.Commit {
		return nil, fmt.Errorf("the update dry run moves skill %s to %q; a description edit must leave the pin %s", skill.Name, got, skill.Git.Commit)
	}
	note("valid, mode %s, agent.description would become %q, the skill stays at %.12s", dry.Mode, editedDescription, skill.Git.Commit)

	step("E3 %supdate_agent{description} as %s changes that one path", agentManagerToolPrefix, email)
	updated, err := portalUpdateAgent(primary, map[string]any{nameKey: spec.Name, descriptionKey: editedDescription})
	if err != nil {
		return nil, err
	}
	if updated.RequestedBy != email || !slices.Equal(updated.Changed, []string{agentDescriptionPath}) {
		return nil, fmt.Errorf("update_agent: requestedBy=%q changed=%v, wanted [%s] changed by %s", updated.RequestedBy, updated.Changed, agentDescriptionPath, email)
	}
	release, err := readAgentRelease(spec.Name)
	if err != nil {
		return nil, err
	}
	if got := release.value("agent", descriptionKey); got != editedDescription {
		return nil, fmt.Errorf("HelmRelease %s carries agent.description %q after the update, wanted %q", spec.Name, got, editedDescription)
	}
	if got := release.skillCommits()[skill.Name]; got != skill.Git.Commit {
		return nil, fmt.Errorf("HelmRelease %s pins skill %s at %q after the description update, wanted the untouched %s", spec.Name, skill.Name, got, skill.Git.Commit)
	}
	note("changed %v, requestedBy=%s; HelmRelease agent.description updated, skill still @ %.12s", updated.Changed, updated.RequestedBy, skill.Git.Commit)
	verdicts = append(verdicts, fmt.Sprintf("PASS: E2/E3 validate_agent{update} and update_agent{description} through the portal change exactly %s (requestedBy=%s), the skill pin untouched", agentDescriptionPath, email))

	step("E4/E5 Update skills: the dry run and the write with refreshSkills re-pin to %s's head", skillsTestRepo)
	head, err := portalSkillHead(primary, skill.Git.URL)
	if err != nil {
		return nil, err
	}
	var refreshDry validateReport
	if err := portalToolCall(primary, "validate_agent", map[string]any{nameKey: spec.Name, namespaceKey: kagentNamespace, "update": true, refreshSkillsKey: true}, &refreshDry); err != nil {
		return nil, err
	}
	if !refreshDry.Valid || refreshDry.Mode != validateModeUpdate || skillCommits(refreshDry.Manifests.Values)[skill.Name] != head {
		return nil, fmt.Errorf("validate_agent{update, refreshSkills}: valid=%v mode=%q pins %s at %q, wanted the head %s", refreshDry.Valid, refreshDry.Mode, skill.Name, skillCommits(refreshDry.Manifests.Values)[skill.Name], head)
	}
	if got := agentValue(refreshDry.Manifests.Values, descriptionKey); got != editedDescription {
		return nil, fmt.Errorf("the refreshSkills dry run changes agent.description to %q — it must change nothing but the pins", got)
	}
	refreshed, err := portalUpdateAgent(primary, map[string]any{nameKey: spec.Name, refreshSkillsKey: true})
	if err != nil {
		return nil, err
	}
	for _, path := range refreshed.Changed {
		if !strings.HasPrefix(path, skillsKey) {
			return nil, fmt.Errorf("update_agent{refreshSkills} changed %v — every changed path must be under %s", refreshed.Changed, skillsKey)
		}
	}
	moved := head != skill.Git.Commit
	if skillsChanged := len(refreshed.Changed) > 0; moved != skillsChanged {
		return nil, fmt.Errorf("update_agent{refreshSkills} reported changed=%v although the head %.12s %s the pin %.12s", refreshed.Changed, head, map[bool]string{true: "differs from", false: "equals"}[moved], skill.Git.Commit)
	}
	if release, err = readAgentRelease(spec.Name); err != nil {
		return nil, err
	}
	if got := release.skillCommits()[skill.Name]; got != head {
		return nil, fmt.Errorf("HelmRelease %s pins skill %s at %q after refreshSkills, wanted the head %s", spec.Name, skill.Name, got, head)
	}
	repinned := waitFor(int(time.Minute/pollInterval), pollInterval, func() bool {
		t, err := readAgentTemplate(spec.Name)
		if err != nil {
			return false
		}
		s := t.skill(skill.Name)
		return s != nil && s.Source.Git != nil && s.Source.Git.Commit == head
	})
	if !repinned {
		return nil, fmt.Errorf("AgentTemplate %s does not carry the re-pinned commit %.12s a minute after refreshSkills", spec.Name, head)
	}
	if moved {
		note("re-pinned %.12s -> %.12s (changed %v)", skill.Git.Commit, head, refreshed.Changed)
	} else {
		note("the head %.12s is the pin discovery showed: nothing changed (changed %v), the pin stays", head, refreshed.Changed)
	}
	verdicts = append(verdicts, fmt.Sprintf("PASS: E4/E5 validate_agent{update, refreshSkills} and update_agent{refreshSkills} through the portal pin %s at %s's head %.12s (list_skills) and change nothing else (changed %v)", skill.Name, skill.Git.URL, head, refreshed.Changed))

	step("E6 readiness after the skills update")
	readiness, err := waitAgentReady(spec.Name, backstageTestReadyTimeout)
	if err != nil {
		return nil, err
	}
	if !readiness.ready {
		return nil, readiness.failure(spec.Name, backstageTestReadyTimeout)
	}
	if _, err := portalAgentStatus(primary, spec.Name, verdictReady, time.Minute); err != nil {
		return nil, err
	}
	note("Ready on Harness %s at revision %.12s; get_agent_status ready", kagentHarness, readiness.template.harness(kagentHarness).LatestSuccessfulRevision)
	verdicts = append(verdicts, "PASS: E6 the agent is Ready after the skills update and get_agent_status agrees")

	if viewer != nil {
		step("E8 %s's update_agent and delete_agent through the portal are forbidden", viewer.user.Email)
		for _, tc := range []struct {
			tool string
			args map[string]any
		}{
			{"update_agent", map[string]any{nameKey: spec.Name, namespaceKey: kagentNamespace, descriptionKey: "a viewer's edit"}},
			{"delete_agent", map[string]any{nameKey: spec.Name, namespaceKey: kagentNamespace}},
		} {
			err := portalToolCall(viewer, tc.tool, tc.args, nil)
			if err == nil {
				return nil, fmt.Errorf("%s's %s of %s was accepted although the view role writes no HelmReleases", viewer.user.Email, tc.tool, spec.Name)
			}
			if !strings.Contains(err.Error(), refusalForbidden) {
				return nil, fmt.Errorf("%s's %s: wanted `%s …`, got: %w", viewer.user.Email, tc.tool, refusalForbidden, err)
			}
			note("%s: %s", tc.tool, excerpt(err.Error(), 160))
		}
		if !agentExists(spec.Name) {
			return nil, fmt.Errorf("%s is gone after the viewer's refused delete", spec.Name)
		}
		verdicts = append(verdicts, fmt.Sprintf("PASS: E8 %s's update_agent and delete_agent answer `%s …` and change nothing", viewer.user.Email, refusalForbidden))
	}

	step("E7 %sdelete_agent %s as %s while HelmRelease %s still needs the chart source", agentManagerToolPrefix, spec.Name, email, other)
	if !agentExists(other) {
		return nil, fmt.Errorf("the second release %s of the chart is not there — E7 needs another release to keep the source for", other)
	}
	deleted, err := portalDeleteAgent(primary, spec.Name)
	if err != nil {
		return nil, err
	}
	if !deleted.HelmReleaseDeleted || deleted.RequestedBy != email {
		return nil, fmt.Errorf("delete_agent: helmReleaseDeleted=%v requestedBy=%q, wanted the release deleted by %s", deleted.HelmReleaseDeleted, deleted.RequestedBy, email)
	}
	if deleted.OCIRepositoryDeleted || deleted.OCIRepositoryKept == "" {
		return nil, fmt.Errorf("delete_agent: ociRepositoryDeleted=%v ociRepositoryKept=%q, wanted the shared %s kept for the other release(s)", deleted.OCIRepositoryDeleted, deleted.OCIRepositoryKept, agentChartOCIRepository)
	}
	if err := waitAgentRemoved(spec.Name); err != nil {
		return nil, err
	}
	if _, _, _, err := agentChartSource(); err != nil {
		return nil, fmt.Errorf("OCIRepository %s after the kept delete: %w", agentChartOCIRepository, err)
	}
	note("helmReleaseDeleted, requestedBy=%s; OCIRepository %s kept (%s); HelmRelease, AgentTemplate and RemoteMCPServer %s gone", deleted.RequestedBy, agentChartOCIRepository, deleted.OCIRepositoryKept, spec.Name)
	verdicts = append(verdicts, fmt.Sprintf("PASS: E7 delete_agent %s through the portal removes the release and its render and keeps OCIRepository %s (%s)", spec.Name, agentChartOCIRepository, deleted.OCIRepositoryKept))

	step("E9 %sdelete_agent %s, the last release of the chart the proof holds", agentManagerToolPrefix, other)
	last, err := portalDeleteAgent(primary, other)
	if err != nil {
		return nil, err
	}
	if !last.HelmReleaseDeleted || last.RequestedBy != email {
		return nil, fmt.Errorf("delete_agent %s: helmReleaseDeleted=%v requestedBy=%q", other, last.HelmReleaseDeleted, last.RequestedBy)
	}
	if !last.OCIRepositoryDeleted && last.OCIRepositoryKept == "" {
		return nil, fmt.Errorf("delete_agent %s neither deleted OCIRepository %s nor said why it stays", other, agentChartOCIRepository)
	}
	if err := waitAgentRemoved(other); err != nil {
		return nil, err
	}
	source := fmt.Sprintf("OCIRepository %s deleted (no other release of the chart in %s)", agentChartOCIRepository, kagentNamespace)
	if !last.OCIRepositoryDeleted {
		source = fmt.Sprintf("OCIRepository %s kept for a release not the proof's (%s)", agentChartOCIRepository, last.OCIRepositoryKept)
	}
	note("%s; %s gone", source, other)
	verdicts = append(verdicts, fmt.Sprintf("PASS: E9 delete_agent %s records the shared source's fate: %s", other, source))
	note("E10 (a GitOps-owned release refused as `conflict:`) has no fixture in this lab — not asserted")
	return verdicts, nil
}

// agentValue is one string leaf under values.agent, "" when unset.
func agentValue(values map[string]any, key string) string {
	agent, _ := values["agent"].(map[string]any)
	s, _ := agent[key].(string)
	return s
}
