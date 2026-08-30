package lab

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The platform install needs a Helm post-renderer (see PostRender), and Helm 4
// changed the contract: --post-renderer no longer takes an executable path but
// the NAME of an installed plugin of type postrenderer/v1. So agentlab
// generates that plugin on the fly — a plugin.yaml whose command points back
// at this very binary — into a lab-owned plugin dir, and passes it to Helm via
// the HELM_PLUGINS environment variable. The user's real plugin dir is never
// touched (and never consulted: HELM_PLUGINS replaces it for that one
// invocation, which is fine — the install needs no other plugin).
//
// Helm 3 is not supported for the platform install at all, plugin mechanics
// aside: Helm 3 stores the whole chart in one release Secret, and the umbrella
// chart's dependency archives (~1.3 MiB of already-gzipped .tgz files) blow
// etcd's 1 MiB cap — giantswarm/agent-platform-standalone#21 concluded plain
// Helm 3 cannot install the chart, and its README documents Helm 4 as the CLI
// install path. ensureHelmSupportsPlatform turns that into a fail-fast.

// postRenderPluginName is the generated plugin's name — what plugin.yaml
// declares and what `helm --post-renderer` is passed.
const postRenderPluginName = "agentlab-postrender"

// helmPluginsDir is the lab-owned HELM_PLUGINS directory the generated plugin
// lands in. Under StateDir like every other generated artifact, so `helm
// template <chart> --post-renderer agentlab-postrender` can be replayed by
// hand with HELM_PLUGINS pointed here.
const helmPluginsDir = StateDir + "/helm-plugins"

// ensureHelmSupportsPlatform fails fast when the installed Helm cannot
// install the platform chart (see the package comment above). Called before
// any cluster work so a Helm 3 user is not told after a five-minute boot.
func ensureHelmSupportsPlatform() error {
	raw, err := outputQuiet("helm", "version", "--template", "{{.Version}}")
	if err != nil {
		return fmt.Errorf("probing the helm version (`helm version`): %w", err)
	}
	version := strings.TrimSpace(raw)
	major, err := helmMajor(version)
	if err != nil {
		return err
	}
	if major < 4 {
		return fmt.Errorf("helm %s cannot install the agent platform — Helm >= 4 is required:\n"+
			"Helm 3 stores the whole chart in one release Secret, and the umbrella chart's\n"+
			"dependency archives put that Secret over etcd's 1 MiB cap\n"+
			"(giantswarm/agent-platform-standalone#21)", version)
	}
	return nil
}

// helmMajor extracts the major version from `helm version --template
// {{.Version}}` output ("v4.2.2").
func helmMajor(version string) (int, error) {
	majorStr, _, _ := strings.Cut(strings.TrimPrefix(version, "v"), ".")
	major, err := strconv.Atoi(majorStr)
	if err != nil {
		return 0, fmt.Errorf("unparseable helm version %q", version)
	}
	return major, nil
}

// ensurePostRenderPlugin (re)writes the generated postrenderer plugin and
// returns the absolute HELM_PLUGINS path to run Helm with. Rewritten on every
// install: the binary's own path is baked in, and a rebuilt or moved agentlab
// must not leave the plugin pointing at a stale executable.
func ensurePostRenderPlugin() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	pluginDir := filepath.Join(helmPluginsDir, postRenderPluginName)
	if err := os.MkdirAll(pluginDir, 0o750); err != nil {
		return "", err
	}
	// %q on the command path: YAML's double-quoted style shares Go's \\ and \"
	// escapes, so Windows paths survive. Helm splits the command on SPACES
	// before quoting is even considered (its documented plugin contract), so a
	// binary living under a path with spaces cannot work here — Helm's
	// limitation, not this file's.
	manifest := fmt.Sprintf(`apiVersion: v1
name: %s
type: postrenderer/v1
runtime: subprocess
version: 0.1.0
runtimeConfig:
  platformCommand:
    - command: %q
      args: ["post-render"]
`, postRenderPluginName, self)
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(manifest), 0o600); err != nil {
		return "", err
	}
	// Absolute: Helm resolves HELM_PLUGINS itself, from its own idea of the
	// working directory.
	return filepath.Abs(helmPluginsDir)
}
