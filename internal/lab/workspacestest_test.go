package lab

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

func TestWorkspacesTestOptionsValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    WorkspacesTestOptions
		wantErr string
	}{
		{name: "the storage proof on claude", opts: WorkspacesTestOptions{}},
		{name: "codex with its model", opts: WorkspacesTestOptions{Harness: config.CodingRuntimeCodex, ModelConfig: "chatgpt-login"}},
		{name: "claude with another model", opts: WorkspacesTestOptions{Harness: claudeHarness, ModelConfig: "another-model"}},
		{name: "codex without a model", opts: WorkspacesTestOptions{Harness: config.CodingRuntimeCodex}, wantErr: "needs --model-config"},
		{name: "storage only with a Harness", opts: WorkspacesTestOptions{StorageOnly: true, Harness: config.CodingRuntimeCodex, ModelConfig: "m"}, wantErr: "--storage-only runs none"},
		{name: "the GitLab part with a Harness", opts: WorkspacesTestOptions{Part: WorkspacesPartGitLab, Harness: config.CodingRuntimeCodex}, wantErr: "runs the claude Session"},
		{name: "the git part", opts: WorkspacesTestOptions{Part: WorkspacesPartGit}},
		{name: "the git part with a Harness", opts: WorkspacesTestOptions{Part: WorkspacesPartGit, Harness: config.CodingRuntimeCodex}, wantErr: "chooses its own Sessions"},
		{name: "the git part with an email", opts: WorkspacesTestOptions{Part: WorkspacesPartGit, Email: accessDefaultSecond}, wantErr: "takes no [email]"},
		{name: "the git part with a provider", opts: WorkspacesTestOptions{Part: WorkspacesPartGit, Provider: config.WorkspaceProviderGitHub}, wantErr: "--provider names the instance"},
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
	codex := workspacesSessionHarnessFor(config.CodingRuntimeCodex, "chatgpt-login")
	if codex.name != config.CodingRuntimeCodex || codex.modelConfig != "chatgpt-login" || codex.agent != "agentlab-workspaces-codex" || codex.shellTool != "shell tool" {
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

func TestSplitWorkspacesExportDirs(t *testing.T) {
	pv := func(name, class, claimNS, claim string) corev1.PersistentVolume {
		v := corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PersistentVolumeSpec{StorageClassName: class}}
		if claim != "" {
			v.Spec.ClaimRef = &corev1.ObjectReference{Namespace: claimNS, Name: claim}
		}
		return v
	}
	pvs := []corev1.PersistentVolume{
		pv("pvc-ws", workspacesStorageClass, "kagent", "workspace-ws-2866-proof"),
		pv("pvc-released", workspacesStorageClass, "", ""),
		pv("pvc-other-class", "standard", "default", "data"),
	}
	live, left := splitWorkspacesExportDirs([]string{"pvc-ws", "pvc-proof", "pvc-released", "pvc-other-class"}, pvs)
	if want := []string{"pvc-ws (kagent/workspace-ws-2866-proof)", "pvc-released (unclaimed)"}; strings.Join(live, ",") != strings.Join(want, ",") {
		t.Errorf("live = %q, want %q", live, want)
	}
	if want := []string{"pvc-proof", "pvc-other-class"}; strings.Join(left, ",") != strings.Join(want, ",") {
		t.Errorf("left = %q, want %q", left, want)
	}
	if live, left := splitWorkspacesExportDirs(nil, pvs); len(live)+len(left) != 0 {
		t.Errorf("an empty export: live %q, left %q", live, left)
	}
}
