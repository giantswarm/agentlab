package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// devctl's server needs the workspaces' github instance, whose App's grant
// muster keeps beside the pin or one App per instance; its version is a
// release, DefaultDevctlVersion by default.
func TestDevctlValidate(t *testing.T) {
	const workspacesKey = "platform.workspaces"
	lab := func() *Config {
		cfg := Default()
		cfg.Platform.Workspaces.Enabled = true
		cfg.Platform.Devctl.Enabled = true
		return cfg
	}
	for name, tc := range map[string]struct {
		change func(*Config)
		want   string
	}{
		"github alone":     {func(*Config) {}, ""},
		"pinned to github": {func(c *Config) { c.Platform.Workspaces.GrantProvider = WorkspaceProviderGitHub }, ""},
		"one App per instance": {func(c *Config) {
			c.Platform.Workspaces.Provider, c.Platform.Workspaces.GrantProvider = WorkspaceProviderGitHub+","+WorkspaceProviderGitLab, WorkspaceGrantProviderApps
		}, ""},
		"a release": {func(c *Config) { c.Platform.Devctl.Version = "8.156.0-rc.2" }, ""},
		"pinned to gitlab": {func(c *Config) {
			c.Platform.Workspaces.Provider, c.Platform.Workspaces.GrantProvider = WorkspaceProviderGitHub+","+WorkspaceProviderGitLab, WorkspaceProviderGitLab
		}, "pins gitlab"},
		"gitlab alone":           {func(c *Config) { c.Platform.Workspaces.Provider = WorkspaceProviderGitLab }, workspacesKey},
		"no workspaces":          {func(c *Config) { c.Platform.Workspaces.Enabled = false }, workspacesKey},
		"no agents":              {func(c *Config) { c.Platform.Agents = false }, "platform.agents"},
		"no release":             {func(c *Config) { c.Platform.Devctl.Version = "v8" }, "no devctl release"},
		"off, nothing to refuse": {func(c *Config) { c.Platform.Devctl.Enabled, c.Platform.Workspaces.Enabled = false, false }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := lab()
			tc.change(cfg)
			err := cfg.validateDevctl()
			if tc.want == "" && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
	cfg := lab()
	if !cfg.DevctlEnabled() || cfg.DevctlVersion() != DefaultDevctlVersion {
		t.Errorf("enabled=%v version=%q, want on at %s", cfg.DevctlEnabled(), cfg.DevctlVersion(), DefaultDevctlVersion)
	}
	if out, err := yaml.Marshal(Default()); err != nil || strings.Contains(string(out), "devctl") {
		t.Errorf("a lab with the switch off writes devctl (err %v): a release before it would refuse the file", err)
	}
}
