package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// agent-manager's answers on a Claude-format plugin are judged by kind: the
// refusal naming the root plugin.json, the manifests unread (the lab's
// missing credential), another plugin error, or none; errors of other
// rules do not count.
func TestJudgeClaudeFormat(t *testing.T) {
	refusal := "plugins[0] (" + claudePluginURL + ") is a Claude-format plugin (.claude-plugin/plugin.json): Harness codex runs Codex, which reads the Agent Plugins format only (plugin.json at the plugin root); drop it or use its skills as skills"
	unchecked := "plugins[0] (" + claudePluginURL + "): its format could not be checked for Harness codex, which reads Agent Plugins only: GitHub answered 404 without a token"
	model := "modelConfig default-model-config (provider Anthropic, model m): Harness codex runs Codex, which speaks the OpenAI Responses API only"
	for name, tc := range map[string]struct {
		errs []string
		want claudeFormatVerdict
	}{
		"the refusal":             {[]string{model, refusal}, claudeFormatRefused},
		"unchecked":               {[]string{unchecked}, claudeFormatUnchecked},
		"other plugin error":      {[]string{"plugins[0].git.commit \"main\" is not a full commit id"}, claudeFormatOther},
		"no plugin error":         {[]string{model}, claudeFormatAccepted},
		"valid":                   {nil, claudeFormatAccepted},
		"refusal without the fix": {[]string{"plugins[0] (x) is a Claude-format plugin"}, claudeFormatOther},
	} {
		if got, _ := judgeClaudeFormat(tc.errs); got != tc.want {
			t.Errorf("%s: judged %d, want %d", name, got, tc.want)
		}
	}
}

// The Claude-format rows gate on agent-manager's answer, not its version:
// a refusal runs the row on any build, a dev one included; an acceptance
// skips on kagent naming the missing capability and fails on codex; an
// unread manifest skips naming the credential.
func TestClaudeFormatGate(t *testing.T) {
	for name, tc := range map[string]struct {
		verdict claudeFormatVerdict
		harness string
		run     bool
		skip    string
	}{
		"refused on kagent":   {claudeFormatRefused, kagentHarness, true, ""},
		"refused on codex":    {claudeFormatRefused, config.CodingRuntimeCodex, true, ""},
		"accepted on kagent":  {claudeFormatAccepted, kagentHarness, false, kagentPluginCheck},
		"accepted on codex":   {claudeFormatAccepted, config.CodingRuntimeCodex, false, ""},
		"unchecked on kagent": {claudeFormatUnchecked, kagentHarness, false, "githubToken.source"},
		"other on kagent":     {claudeFormatOther, kagentHarness, false, ""},
	} {
		run, skip := claudeFormatGate(tc.verdict, "errs", tc.harness)
		switch {
		case run != tc.run:
			t.Errorf("%s: run=%v, want %v", name, run, tc.run)
		case tc.skip == "" && skip != "":
			t.Errorf("%s: skips %q, want no skip", name, skip)
		case tc.skip != "" && !strings.Contains(skip, tc.skip):
			t.Errorf("%s: skips %q, want it naming %q", name, skip, tc.skip)
		}
	}
}

// The agent's listing passes with every selected skill listed, prefixed by
// its plugin or not, and fails on a selected one missing or an unselected
// known one listed; skills outside the known set are not judged.
func TestJudgePluginSkills(t *testing.T) {
	known := []string{pluginExampleSkill, "other-skill"}
	if _, err := judgePluginSkills("simplify, agent-plugins-example:migrate-agent-plugin", []string{pluginExampleSkill}, known); err != nil {
		t.Errorf("the selected skill listed with its prefix: %v", err)
	}
	if _, err := judgePluginSkills("`migrate-agent-plugin`", []string{pluginExampleSkill}, known); err != nil {
		t.Errorf("a quoted name: %v", err)
	}
	if _, err := judgePluginSkills("simplify", []string{pluginExampleSkill}, known); err == nil || !strings.Contains(err.Error(), "missing [migrate-agent-plugin]") {
		t.Errorf("the selected skill missing: %v", err)
	}
	if _, err := judgePluginSkills("migrate-agent-plugin, other-skill", []string{pluginExampleSkill}, known); err == nil || !strings.Contains(err.Error(), "not selected but listed [other-skill]") {
		t.Errorf("an unselected skill listed: %v", err)
	}
	if _, err := judgePluginSkills("None", nil, known); err != nil {
		t.Errorf("an empty selection, none listed: %v", err)
	}
}

// The hook passes only on the context it adds, as the agent quotes it.
func TestJudgeClaudePluginHook(t *testing.T) {
	if _, err := judgeClaudePluginHook(codingProbe{claudePluginHookKey: "You are in 'explanatory' output style mode"}); err != nil {
		t.Errorf("the hook's context quoted: %v", err)
	}
	for _, probe := range []codingProbe{{claudePluginHookKey: "None"}, {}} {
		if _, err := judgeClaudePluginHook(probe); err == nil {
			t.Errorf("%v passes", probe)
		}
	}
}
