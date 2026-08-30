package lab

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestHelmMajor(t *testing.T) {
	cases := []struct {
		version string
		want    int
		wantErr bool
	}{
		{"v4.2.2", 4, false},
		{"v3.19.0", 3, false},
		{"v4.0.0-rc.1", 4, false},
		{"4.2.2", 4, false}, // defensive: no leading v
		{"", 0, true},
		{"garbage", 0, true},
	}
	for _, c := range cases {
		got, err := helmMajor(c.version)
		if c.wantErr {
			if err == nil {
				t.Errorf("helmMajor(%q): expected error, got %d", c.version, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("helmMajor(%q): %v", c.version, err)
			continue
		}
		if got != c.want {
			t.Errorf("helmMajor(%q) = %d, want %d", c.version, got, c.want)
		}
	}
}

// TestEnsurePostRenderPlugin proves the generated plugin.yaml is what Helm 4's
// v1 plugin loader requires: apiVersion v1, a semver version, subprocess
// runtime, postrenderer/v1 type, and a platformCommand pointing at this
// process's own executable with the post-render arg baked in (extra
// --post-renderer-args would be env-expanded by Helm, so nothing may rely on
// them).
func TestEnsurePostRenderPlugin(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	pluginsDir, err := ensurePostRenderPlugin()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(pluginsDir) {
		t.Errorf("HELM_PLUGINS path must be absolute, got %q", pluginsDir)
	}

	raw, err := os.ReadFile(filepath.Join(pluginsDir, postRenderPluginName, "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		APIVersion    string `yaml:"apiVersion"`
		Name          string `yaml:"name"`
		Type          string `yaml:"type"`
		Runtime       string `yaml:"runtime"`
		Version       string `yaml:"version"`
		RuntimeConfig struct {
			PlatformCommand []struct {
				Command string   `yaml:"command"`
				Args    []string `yaml:"args"`
			} `yaml:"platformCommand"`
		} `yaml:"runtimeConfig"`
	}
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("generated plugin.yaml does not parse: %v\n%s", err, raw)
	}
	if manifest.APIVersion != "v1" || manifest.Type != "postrenderer/v1" || manifest.Runtime != "subprocess" {
		t.Errorf("wrong plugin envelope: %+v", manifest)
	}
	if manifest.Name != postRenderPluginName {
		t.Errorf("plugin name %q does not match the --post-renderer argument %q", manifest.Name, postRenderPluginName)
	}
	if manifest.Version == "" {
		t.Error("plugin version is required (Helm validates semver)")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.RuntimeConfig.PlatformCommand) != 1 {
		t.Fatalf("expected exactly one platformCommand, got %+v", manifest.RuntimeConfig.PlatformCommand)
	}
	cmd := manifest.RuntimeConfig.PlatformCommand[0]
	if cmd.Command != self {
		t.Errorf("plugin command %q is not this executable %q", cmd.Command, self)
	}
	if len(cmd.Args) != 1 || cmd.Args[0] != "post-render" {
		t.Errorf("plugin args %v, want [post-render]", cmd.Args)
	}
}
