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
		{name: "the GitLab part on omp", opts: WorkspacesTestOptions{Part: WorkspacesPartGitLab, Harness: config.CodingRuntimeOmp}},
		{name: "the GitLab part on codex without a model", opts: WorkspacesTestOptions{Part: WorkspacesPartGitLab, Harness: config.CodingRuntimeCodex}, wantErr: "needs --model-config codex="},
		{name: "the GitLab part with toolchains", opts: WorkspacesTestOptions{Part: WorkspacesPartGitLab, Toolchains: []string{"gitlab@0.1.2"}}},
		{name: "the git part", opts: WorkspacesTestOptions{Part: WorkspacesPartGit}},
		{name: "the git part on every Harness", opts: WorkspacesTestOptions{Part: WorkspacesPartGit, Harness: "claude,kagent,omp,codex", ModelConfig: "codex=codex-host"}},
		{name: "the git part with a repeated Harness", opts: WorkspacesTestOptions{Part: WorkspacesPartGit, Harness: "claude,claude"}, wantErr: "empty or repeated"},
		{name: "the git part with a model for an absent Harness", opts: WorkspacesTestOptions{Part: WorkspacesPartGit, Harness: claudeHarness, ModelConfig: "codex=codex-host"}, wantErr: "does not run: codex"},
		{name: "the sessions part with a Harness", opts: WorkspacesTestOptions{Part: WorkspacesPartSessions, Harness: config.CodingRuntimeOmp}, wantErr: "chooses its own Sessions"},
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

func TestWorkspacePartHarnesses(t *testing.T) {
	harnesses, err := workspacePartHarnesses("", "", gitDefaultHarnesses)
	if err != nil {
		t.Fatal(err)
	}
	if len(harnesses) != 2 || harnesses[0].agent != workspacesHarnessAgent || harnesses[1].agent != sessionsKagentAgent || harnesses[1].modelConfig != "" {
		t.Errorf("the git part's default Harnesses = %+v, want claude and kagent on the default model", harnesses)
	}
	harnesses, err = workspacePartHarnesses("claude,omp,codex", "codex=codex-host,omp=omp-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if harnesses[0].modelConfig != "" || harnesses[1].modelConfig != "omp-model" || harnesses[1].agent != "agentlab-workspaces-omp" || harnesses[2].modelConfig != "codex-host" {
		t.Errorf("harness=ModelConfig pairs = %+v", harnesses)
	}
	harnesses, err = workspacePartHarnesses("kagent,codex", "codex-host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if harnesses[0].modelConfig != "" || harnesses[1].modelConfig != "codex-host" {
		t.Errorf("one ModelConfig applies to the Harnesses beside claude and kagent only: %+v", harnesses)
	}
	if _, err := workspacePartHarnesses("omp", "omp=", nil); err == nil {
		t.Error("an empty ModelConfig of a pair is accepted")
	}
}

func TestWorkspacesSessionHarnessFor(t *testing.T) {
	claude := workspacesSessionHarnessFor("", "")
	if claude.name != claudeHarness || claude.modelConfig != "" || claude.agent != workspacesHarnessAgent || claude.shellTool != "Bash tool" {
		t.Errorf("the default Session = %+v, want the claude Agent %s on the model the proof resolves", claude, workspacesHarnessAgent)
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
