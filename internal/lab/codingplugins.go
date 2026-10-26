package lab

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The coding proof's Agent Plugin stages (docs/platform.md "The coding
// agents proof"): one agent definition binds the upstream example package
// by commit with an explicit skill selection on every Harness, and a public
// Claude-format package, Anthropic's explanatory-output-style, is refused at
// write time where the runtime reads the Agent Plugins format only and loads
// whole on claude, its SessionStart hook included. agent-manager tells the formats apart by which
// manifest the plugin's directory has at the pinned commit (checkPlugins);
// the golden boot's refusal of an OCI source is kagent's own and not proven
// here.

const (
	// pluginExampleURL is the Agent Plugins example package kagent's claude
	// and codex READMEs bind: a root plugin.json and the one skill
	// pluginExampleSkill, public.
	pluginExampleURL    = "https://github.com/agentplugins/agent-plugins-example.git"
	pluginExampleCommit = "5f3f5084a821aefa792e79500dd8f0462ab83473"
	pluginExampleSkill  = "migrate-agent-plugin"
	// claudePluginURL holds explanatory-output-style at claudePluginPath: a
	// Claude-format plugin (.claude-plugin/plugin.json, no root plugin.json)
	// whose one part is a SessionStart hook, a shell script, in a public
	// repository.
	claudePluginURL    = "https://github.com/anthropics/claude-code.git"
	claudePluginPath   = "plugins/explanatory-output-style"
	claudePluginCommit = "3af8ef29be10d2f0dc98ee5db4ae7e9558ff48e5"
	// claudePluginHookKey is the listing turn's reading of the hook;
	// claudePluginHookMark is in the context the hook adds.
	claudePluginHookKey  = "plugin-hook"
	claudePluginHookMark = "explanatory"
	// claudeFormatRefusal and rootManifestFix are in agent-manager's
	// refusal of a Claude-format plugin; pluginFormatUnchecked in its
	// refusal when it could not read the plugin's manifests.
	claudeFormatRefusal   = "is a Claude-format plugin"
	rootManifestFix       = "plugin.json at the plugin root"
	pluginFormatUnchecked = "its format could not be checked"
	// kagentPluginCheck is the capability the kagent rows gate on, probed by
	// validate_agent rather than read from agent-manager's version, so a
	// dev build is judged like a release: without it a Claude-format plugin
	// passes the write there, and a Harness change to kagent would write a
	// revision that never boots.
	kagentPluginCheck = "agent-manager's plugin check on Harness kagent"
	// ompPluginCheck is the same capability on omp, which agent-manager
	// tells from another BYO Harness by its workload image (2.15.0).
	ompPluginCheck = "agent-manager's plugin check on Harness omp"
	// codingTestPluginAgent is the agent create_agent must refuse.
	codingTestPluginAgent = "agentlab-coding-plugin"
)

// examplePlugin is the example package with the skills selection given.
func examplePlugin(skills []string) agentPlugin {
	return agentPlugin{Git: &gitSkill{URL: pluginExampleURL, Commit: pluginExampleCommit}, Skills: skills}
}

// claudePlugin is explanatory-output-style, without a selection: a
// Claude-format plugin takes none.
func claudePlugin() agentPlugin {
	return agentPlugin{Git: &gitSkill{URL: claudePluginURL, Commit: claudePluginCommit}, Path: claudePluginPath}
}

// codingPlugins is the agent's plugins: the example package with
// exampleSkills selected, on claude the Claude-format plugin beside it.
func (p *codingProof) codingPlugins(exampleSkills []string) []agentPlugin {
	plugins := []agentPlugin{examplePlugin(exampleSkills)}
	if p.rt.claude {
		plugins = append(plugins, claudePlugin())
	}
	return plugins
}

// pluginErrors are the validate_agent errors about plugins.
func pluginErrors(errs []string) []string {
	var out []string
	for _, e := range errs {
		if strings.Contains(e, pluginsKey+"[") {
			out = append(out, e)
		}
	}
	return out
}

// claudeFormatVerdict is what agent-manager answered a Claude-format
// plugin.
type claudeFormatVerdict int

const (
	// claudeFormatAccepted: no plugin error.
	claudeFormatAccepted claudeFormatVerdict = iota
	// claudeFormatRefused: refused as Claude-format, naming the root
	// plugin.json.
	claudeFormatRefused
	// claudeFormatUnchecked: the manifests could not be read.
	claudeFormatUnchecked
	// claudeFormatOther: a plugin error of another kind.
	claudeFormatOther
)

// judgeClaudeFormat classifies agent-manager's errors on a Claude-format
// plugin and returns the plugin errors it judged.
func judgeClaudeFormat(errs []string) (claudeFormatVerdict, string) {
	plugin := pluginErrors(errs)
	joined := strings.Join(plugin, "; ")
	switch {
	case len(plugin) == 0:
		return claudeFormatAccepted, ""
	case strings.Contains(joined, claudeFormatRefusal) && strings.Contains(joined, rootManifestFix):
		return claudeFormatRefused, joined
	case strings.Contains(joined, pluginFormatUnchecked):
		return claudeFormatUnchecked, joined
	}
	return claudeFormatOther, joined
}

// uncheckedFix is how agent-manager reaches GitHub past the anonymous
// window this machine shares.
const uncheckedFix = "record a GitHub credential (githubToken.source or the skills GitHub App, docs/agents.md \"The GitHub token\"), then `agentlab platform`"

// judgePluginSkills is the plugin skills an agent listed against the
// selection: every name of want listed, no other name of known. listed is
// the agent's comma-separated list; a name counts with or without its
// plugin's prefix (plugin:skill).
func judgePluginSkills(listed string, want, known []string) (string, error) {
	var names []string
	for _, n := range strings.Split(listed, ",") {
		if n = strings.ToLower(strings.Trim(strings.TrimSpace(n), "`'\"/")); n != "" {
			names = append(names, n)
		}
	}
	has := func(skill string) bool {
		return slices.ContainsFunc(names, func(n string) bool { return n == skill || strings.HasSuffix(n, ":"+skill) })
	}
	var missing, extra []string
	for _, skill := range known {
		switch {
		case slices.Contains(want, skill) && !has(skill):
			missing = append(missing, skill)
		case !slices.Contains(want, skill) && has(skill):
			extra = append(extra, skill)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		return "", fmt.Errorf("the agent lists skills %q: missing %v, not selected but listed %v", listed, missing, extra)
	}
	if len(want) == 0 {
		return fmt.Sprintf("no plugin skill listed (the agent lists %q)", excerpt(listed, 200)), nil
	}
	return fmt.Sprintf("plugin skills %v listed, no other (the agent lists %q)", want, excerpt(listed, 300)), nil
}

// namespaceHarnesses are the Harnesses of the agents' namespace, by name.
func namespaceHarnesses(ctx context.Context) ([]string, error) {
	gvr, err := gvrFor(harnessResource)
	if err != nil {
		return nil, err
	}
	list, err := listObjects(ctx, gvr, kagentNamespace, "")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list))
	for _, h := range list {
		names = append(names, h.GetName())
	}
	slices.Sort(names)
	return names, nil
}

// prepareCodexModel picks the ModelConfig the codex rows write with, one
// that speaks the Responses API so that agent-manager's model check passes
// and the plugin check is the one left to answer: the run's own on codex,
// the lab's codex model, else the mockllm's ModelConfig, placed for the run.
func (p *codingProof) prepareCodexModel(modelConfig string) error {
	switch {
	case p.rt.name == config.CodingRuntimeCodex:
		p.codexModel = modelConfig
	case p.cfg.CodexModelConfig() != "":
		p.codexModel = p.cfg.CodexModelConfig()
	default:
		if err := ensureMockLLMModelConfig(); err != nil {
			return err
		}
		p.codexModel, p.mockModelPlaced = MockLLMModelConfig, true
	}
	return nil
}

// provePluginsAccepted is validate_agent on the example package with its
// skill selected on every Harness of the namespace: no plugin error. Errors
// of other rules (a ModelConfig another runtime does not speak) are not this
// stage's and are quoted only.
func (p *codingProof) provePluginsAccepted(spec agentSpec) error {
	for _, harness := range p.harnesses {
		s := spec
		s.Harness, s.Plugins = harness, []agentPlugin{examplePlugin([]string{pluginExampleSkill})}
		report, err := p.validate(s, func(args map[string]any) { args[toolsetKey] = []string{codingTestToolset} })
		if err != nil {
			return err
		}
		if errs := pluginErrors(report.Errors); len(errs) > 0 {
			return fmt.Errorf("validate_agent on Harness %s refused %s @ %.12s: %v (%s)", harness, pluginExampleURL, pluginExampleCommit, errs, p.line)
		}
		other := ""
		if !report.Valid {
			other = "; other rules: " + excerpt(strings.Join(report.Errors, "; "), 200)
		}
		note("Harness %s: plugins accepted%s", harness, other)
	}
	return nil
}

// claudeFormatGate is what a Claude-format row does with validate_agent's
// verdict on harness: go on to create_agent (run), or skip naming why; neither
// is the row's failure. An unread manifest is GitHub's anonymous window
// spent, the skip naming the credential that lifts it; an acceptance on
// kagent or omp is a line without that Harness's check, the skip naming it.
func claudeFormatGate(verdict claudeFormatVerdict, errs, harness string) (run bool, skip string) {
	switch {
	case verdict == claudeFormatRefused:
		return true, ""
	case verdict == claudeFormatUnchecked:
		return false, fmt.Sprintf("agent-manager could not read the plugin's manifests: %s; %s", excerpt(errs, 240), uncheckedFix)
	case verdict == claudeFormatAccepted && harness == kagentHarness:
		return false, fmt.Sprintf("the line lacks %s: validate_agent accepted the Claude-format plugin there, which would pass the write and fail at its golden boot", kagentPluginCheck)
	case verdict == claudeFormatAccepted && harness == config.CodingRuntimeOmp:
		return false, fmt.Sprintf("the line lacks %s: validate_agent accepted the Claude-format plugin there, which would pass the write and fail at the adapter's boot refusal", ompPluginCheck)
	}
	return false, ""
}

// proveClaudeFormatRefused is the Claude-format plugin on harness, written
// with modelConfig: validate_agent and create_agent refuse it with the
// Claude-format reason naming the root plugin.json, and nothing is written.
// It returns why the row skips (claudeFormatGate), empty when it was proven.
func (p *codingProof) proveClaudeFormatRefused(spec agentSpec, harness, modelConfig string) (string, error) {
	s := spec
	s.Name, s.Harness, s.ModelConfig = codingTestPluginAgent, harness, modelConfig
	s.Toolset, s.Plugins = []string{codingTestToolset}, []agentPlugin{claudePlugin()}
	report, err := p.validate(s, func(map[string]any) {})
	if err != nil {
		return "", err
	}
	verdict, errs := judgeClaudeFormat(report.Errors)
	run, skip := claudeFormatGate(verdict, errs, harness)
	switch {
	case skip != "":
		return skip, nil
	case !run:
		return "", fmt.Errorf("validate_agent on Harness %s answered valid=%v errors=%v for %s, wanted the refusal %q naming %q (%s)", harness, report.Valid, report.Errors, claudePluginPath, claudeFormatRefusal, rootManifestFix, p.line)
	}
	note("validate_agent refused: %s", excerpt(errs, 300))
	_, err = (agentManagerWriter{p.muster}).createAgent(s)
	switch {
	case err == nil:
		return "", fmt.Errorf("create_agent wrote %s on Harness %s with %s, a Claude-format plugin %s cannot load (%s)", codingTestPluginAgent, harness, claudePluginPath, harness, p.line)
	case !strings.Contains(err.Error(), claudeFormatRefusal) || !strings.Contains(err.Error(), rootManifestFix):
		return "", fmt.Errorf("create_agent on Harness %s with %s: %w; wanted the refusal %q naming %q (%s)", harness, claudePluginPath, err, claudeFormatRefusal, rootManifestFix, p.line)
	case agentExists(codingTestPluginAgent):
		return "", fmt.Errorf("create_agent refused the Claude-format plugin on %s but the HelmRelease %s exists (%s)", harness, codingTestPluginAgent, p.line)
	}
	note("create_agent refused, no HelmRelease %s: %s", codingTestPluginAgent, excerpt(err.Error(), 300))
	return "", nil
}

// codingPluginSkillsPrompt asks the agent which skills it has; with hook,
// also what the Claude-format plugin's SessionStart hook added to its
// context.
func codingPluginSkillsPrompt(rt codingRuntime, hook bool) string {
	prompt := "This is this test lab's skills question. List the name of every skill available to you in this session, every one you could invoke, plugin skills included with any plugin prefix such as plugin:skill kept, and reply with exactly one line `" + probeMarker + " skills=<the names, comma-separated, or none>`."
	if !hook {
		return prompt
	}
	return prompt + " Then, if your context holds text a SessionStart hook of the explanatory-output-style plugin added, reply `" + probeMarker + " " + claudePluginHookKey + "=<its first sentence>`, else `" + probeMarker + " " + claudePluginHookKey + "=none`."
}

// pluginsWords names the agent's plugins for a stage's title.
func (p *codingProof) pluginsWords() string {
	words := fmt.Sprintf("[%s @ %.12s selecting %s", pluginExampleURL, pluginExampleCommit, pluginExampleSkill)
	if p.rt.claude {
		words += fmt.Sprintf(", %s %s @ %.12s", claudePluginURL, claudePluginPath, claudePluginCommit)
	}
	return words + "]"
}

// pluginSkillsKnown are the plugin skills the listing is judged on: the
// example package's; the Claude-format plugin has none.
func (p *codingProof) pluginSkillsKnown() []string {
	return []string{pluginExampleSkill}
}

// provePluginSkills is the agent's skills against the selection: on a real
// model, the agent's own listing; on the mockllm, the requests the runtime
// sent it from mark on carrying the example skill (wantExample) or none
// carrying it. It returns the listing turn's readings, nil on the mockllm.
func (p *codingProof) provePluginSkills(ctx context.Context, session *apiv1alpha1.Session, wantExample bool, mark int, hook bool) (codingProbe, error) {
	var want []string
	if wantExample {
		want = append(want, pluginExampleSkill)
	}
	if p.mock != nil {
		counts, err := p.mock.requests()
		if err != nil {
			return nil, fmt.Errorf("reading the mockllm's requests: %w", err)
		}
		carried, requests := counts.watchedSince(mark, pluginExampleSkill)
		switch {
		case requests == 0:
			return nil, fmt.Errorf("the mockllm received no request of the agent since request %d: nothing shows what the runtime presented (%s)", mark, p.line)
		case wantExample && carried == 0:
			return nil, fmt.Errorf("none of the %d requests codex sent the mockllm carries the selected skill %s: the runtime did not present it to the model (%s)", requests, pluginExampleSkill, p.line)
		case !wantExample && carried > 0:
			return nil, fmt.Errorf("%d of the %d requests codex sent the mockllm carry %s although the selection is empty (%s)", carried, requests, pluginExampleSkill, p.line)
		}
		note("%d of the %d requests codex sent the model carry %s", carried, requests, pluginExampleSkill)
		return nil, nil
	}
	listing, err := p.turn(ctx, session, codingPluginSkillsPrompt(p.rt, hook))
	if err != nil {
		return nil, err
	}
	verdict, err := judgePluginSkills(listing.probe["skills"], want, p.pluginSkillsKnown())
	if err != nil {
		return nil, fmt.Errorf("on Harness %s: %w; the reply: %q (%s)", p.rt.name, err, excerpt(listing.reply, 600), p.line)
	}
	note("%s", verdict)
	return listing.probe, nil
}

// judgeClaudePluginHook is the listing turn's reading of the Claude-format
// plugin's SessionStart hook: the context it adds, quoted by the agent.
func judgeClaudePluginHook(probe codingProbe) (string, error) {
	hook := probe[claudePluginHookKey]
	if !strings.Contains(strings.ToLower(hook), claudePluginHookMark) {
		return "", fmt.Errorf("the agent quotes %q for the SessionStart hook of %s, wanted the context it adds (naming the %s output style)", excerpt(hook, 200), claudePluginPath, claudePluginHookMark)
	}
	return fmt.Sprintf("the SessionStart hook of %s ran: %q", claudePluginPath, excerpt(hook, 200)), nil
}

// proveHarnessChangeRefused is update_agent moving the claude agent, which
// binds the Claude-format plugin, to target with modelConfig: refused with
// the Claude-format reason, the HelmRelease not written and the Agent still
// on claude. modelConfig is one target speaks (on codex a Responses API
// one), so the plugin check is the one left to refuse it.
func (p *codingProof) proveHarnessChangeRefused(target, modelConfig string) error {
	before, err := readKagentFluxObject(fluxHelmReleaseResource, codingTestAgent)
	if err != nil {
		return err
	}
	_, refusal := agentManagerUpdate(p.muster, map[string]any{nameKey: codingTestAgent, harnessKey: target, modelConfigKey: modelConfig})
	switch {
	case refusal == nil:
		return fmt.Errorf("update_agent moved %s, which binds a Claude-format plugin, to Harness %s (%s)", codingTestAgent, target, p.line)
	case !strings.Contains(refusal.Error(), claudeFormatRefusal):
		return fmt.Errorf("update_agent moving %s to %s: %w; wanted the refusal %q (%s)", codingTestAgent, target, refusal, claudeFormatRefusal, p.line)
	}
	after, err := readKagentFluxObject(fluxHelmReleaseResource, codingTestAgent)
	if err != nil {
		return err
	}
	if after.GetResourceVersion() != before.GetResourceVersion() {
		return fmt.Errorf("update_agent refused the move to %s but HelmRelease %s changed (resourceVersion %s -> %s) (%s)", target, codingTestAgent, before.GetResourceVersion(), after.GetResourceVersion(), p.line)
	}
	r, err := readAgentReadinessOn(codingTestAgent, p.rt.name)
	if err != nil {
		return err
	}
	if !r.ready {
		return fmt.Errorf("after the refused move the Agent %s is not Ready on %s: %s", codingTestAgent, p.rt.name, r.reason)
	}
	note("refused, HelmRelease %s unchanged (resourceVersion %s), Agent Ready on %s: %s", codingTestAgent, after.GetResourceVersion(), p.rt.name, excerpt(refusal.Error(), 300))
	return nil
}

// proveEmptySelection is update_agent with the example package's selection
// empty: the agent boots the new revision and a new Session's agent has no
// skill of it.
func (p *codingProof) proveEmptySelection(ctx context.Context) error {
	r, err := readAgentReadinessOn(codingTestAgent, p.rt.name)
	if err != nil {
		return err
	}
	if r.agent == nil {
		return fmt.Errorf("agent %s has no Agent to update", codingTestAgent)
	}
	old := r.agent.Status.LatestSuccessfulRevision
	updated, err := agentManagerUpdate(p.muster, map[string]any{nameKey: codingTestAgent, pluginsKey: p.codingPlugins([]string{})})
	if err != nil {
		return err
	}
	note("update_agent changed %v", updated.Changed)
	if err := p.waitNewRevision(old); err != nil {
		return err
	}
	mark := 0
	if p.mock != nil {
		counts, err := p.mock.requests()
		if err != nil {
			return fmt.Errorf("reading the mockllm's requests: %w", err)
		}
		mark = len(counts.Log)
	}
	session, err := p.createSession(ctx, codingTestAgent)
	if err != nil {
		return err
	}
	if p.mock != nil {
		// The code word turn is in the mockllm's script: its requests show
		// what the runtime presents a new Session's model.
		if _, err := p.turn(ctx, session, codingCodeWordPrompt(p.rt)); err != nil {
			return err
		}
	}
	_, err = p.provePluginSkills(ctx, session, false, mark, false)
	return err
}

// waitNewRevision waits for the template to carry the empty selection and
// the Agent Ready on a revision other than old.
func (p *codingProof) waitNewRevision(old string) error {
	if !waitPluginSelection(codingTestAgent, nil) {
		return fmt.Errorf("AgentTemplate %s does not carry the empty selection of %s a minute after update_agent (%s)", codingTestAgent, pluginExampleURL, p.line)
	}
	return p.waitReadyOnNewRevision(old)
}

// waitReadyOnNewRevision waits for the proof's agent Ready on a revision
// other than old.
func (p *codingProof) waitReadyOnNewRevision(old string) error {
	if _, err := waitReadyOnNewRevision(codingTestAgent, p.rt.name, old, p.timeout); err != nil {
		return fmt.Errorf("%w (%s)", err, p.line)
	}
	return nil
}

// waitPluginSelection waits up to a minute for agent's AgentTemplate to
// carry the example package with exactly skills selected.
func waitPluginSelection(agent string, skills []string) bool {
	return waitFor(int(time.Minute/pollInterval), pollInterval, func() bool {
		t, err := readAgentTemplate(agent)
		if err != nil {
			return false
		}
		for _, plugin := range t.Spec.Plugins {
			if plugin.Source.Git != nil && plugin.Source.Git.URL == pluginExampleURL {
				return slices.Equal(plugin.Skills, skills)
			}
		}
		return false
	})
}

// waitReadyOnNewRevision waits up to timeout for agent Ready on harness on
// a revision other than old, and returns that revision.
func waitReadyOnNewRevision(agent, harness, old string, timeout time.Duration) (string, error) {
	started := time.Now()
	var r agentReadiness
	var lastErr error
	waitFor(int(timeout/pollInterval), pollInterval, func() bool {
		r, lastErr = readAgentReadinessOn(agent, harness)
		return lastErr != nil || r.terminal || (r.ready && r.agent != nil && r.agent.Status.LatestSuccessfulRevision != old)
	})
	switch {
	case lastErr != nil:
		return "", fmt.Errorf("agent %s: %w", agent, lastErr)
	case !r.ready || r.agent == nil || r.agent.Status.LatestSuccessfulRevision == old:
		return "", fmt.Errorf("agent %s is not Ready on a new revision after %s (still %.12s): %s", agent, time.Since(started).Round(time.Second), old, r.reason)
	}
	revision := r.agent.Status.LatestSuccessfulRevision
	note("Ready on revision %.12s after %s", revision, time.Since(started).Round(time.Second))
	return revision, nil
}
