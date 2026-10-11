package lab

import (
	"os"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestCheckLabGitHubBinary: the up-front check judges this binary exactly as
// the lab's GitHub container start would (the test binary is whatever the
// build made it, so the two verdicts are compared, not pinned), names the
// instance so the refusal reads with agentlab.yaml, and judges nothing while
// the instance is not configured.
func TestCheckLabGitHubBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := workspaceLab("")
	if !cfg.WorkspaceFakeProvider() {
		t.Fatal("the default lab has no provider instance fake")
	}
	got, want := checkLabGitHubBinary(cfg), linuxStaticBinary(exe, labGitHubWhat, "")
	if (got == nil) != (want == nil) || (got != nil && got.Error() != want.Error()) {
		t.Errorf("checkLabGitHubBinary = %v, the container start says %v", got, want)
	}
	if got != nil && !strings.Contains(got.Error(), "provider instance "+config.WorkspaceProviderFake) {
		t.Errorf("the refusal does not name the instance: %v", got)
	}
	off := workspaceLab("")
	off.Platform.Workspaces.Enabled = false
	for name, cfg := range map[string]*config.Config{"the github instance alone": workspaceGitHubLab(), "workspaces off": off} {
		if err := checkLabGitHubBinary(cfg); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
