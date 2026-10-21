package lab

import (
	"strings"
	"testing"
)

func TestWorkspacesTestOptionsValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    WorkspacesTestOptions
		wantErr string
	}{
		{name: "the storage proof on claude", opts: WorkspacesTestOptions{}},
		{name: "codex with its model", opts: WorkspacesTestOptions{Harness: "codex", ModelConfig: "chatgpt-login"}},
		{name: "claude with another model", opts: WorkspacesTestOptions{Harness: "claude", ModelConfig: "other"}},
		{name: "codex without a model", opts: WorkspacesTestOptions{Harness: "codex"}, wantErr: "needs --model-config"},
		{name: "storage only with a Harness", opts: WorkspacesTestOptions{StorageOnly: true, Harness: "codex", ModelConfig: "m"}, wantErr: "--storage-only runs none"},
		{name: "the GitLab part with a Harness", opts: WorkspacesTestOptions{Part: WorkspacesPartGitLab, Harness: "codex"}, wantErr: "runs the claude Session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate() = %v, want an error with %q", err, tc.wantErr)
			}
		})
	}
}

func TestWorkspacesSessionHarnessFor(t *testing.T) {
	claude := workspacesSessionHarnessFor("", "")
	if claude.name != claudeHarness || claude.modelConfig != defaultModelConfig || claude.agent != workspacesHarnessAgent || claude.shellTool != "Bash tool" {
		t.Errorf("the default Session = %+v, want the claude Agent %s on %s", claude, workspacesHarnessAgent, defaultModelConfig)
	}
	codex := workspacesSessionHarnessFor("codex", "chatgpt-login")
	if codex.name != "codex" || codex.modelConfig != "chatgpt-login" || codex.agent != "agentlab-workspaces-codex" || codex.shellTool != "shell tool" {
		t.Errorf("the codex Session = %+v", codex)
	}
	manifest := workspacesAgentManifest(codex.agent, codex.name, codex.modelConfig, "probe")
	for _, want := range []string{"name: agentlab-workspaces-codex", "name: chatgpt-login", `systemPrompt: "probe"`} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the codex manifest misses %q:\n%s", want, manifest)
		}
	}
	if !strings.Contains(workspacesHarnessProbePrompt(codex.shellTool), "with your shell tool,") {
		t.Errorf("the codex prompt does not name its shell tool")
	}
}
