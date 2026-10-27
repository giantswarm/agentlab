package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A person's own ChatGPT login needs the codex Harness, the workspaces and a
// chart that wires it; the model defaults to DefaultChatGPTSubscriptionModel.
func TestChatGPTSubscriptionValidate(t *testing.T) {
	lab := func() *Config {
		cfg := Default()
		cfg.Platform.ChartVersion = "4.164.0-rc.2"
		cfg.Platform.Coding.Enabled = true
		cfg.Platform.Coding.Harnesses = []string{CodingRuntimeClaude, CodingRuntimeCodex}
		cfg.Platform.Workspaces.Enabled = true
		cfg.Platform.ChatGPTSubscription.Enabled = true
		return cfg
	}
	for name, tc := range map[string]struct {
		change func(*Config)
		want   string
	}{
		"complete":            {func(*Config) {}, ""},
		"a release at floor":  {func(c *Config) { c.Platform.ChartVersion = "4.164.0" }, ""},
		"a chart directory":   {func(c *Config) { c.Platform.ChartVersion, c.Platform.ChartPath = "4.100.0", t.TempDir() }, ""},
		"chart below floor":   {func(c *Config) { c.Platform.ChartVersion = "4.163.0" }, "agent-platform 4.164.0-0 or newer"},
		"no codex Harness":    {func(c *Config) { c.Platform.Coding.Harnesses = []string{CodingRuntimeClaude} }, "--coding-harnesses claude,codex"},
		"coding off":          {func(c *Config) { c.Platform.Coding.Enabled = false }, "codex Harness"},
		"no workspaces":       {func(c *Config) { c.Platform.Workspaces.Enabled = false }, "platform.workspaces"},
		"no agents":           {func(c *Config) { c.Platform.Agents, c.Platform.Coding.Enabled = false, false }, "platform.agents"},
		"a model with spaces": {func(c *Config) { c.Platform.ChatGPTSubscription.Model = "gpt 5" }, "no model name"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := lab()
			tc.change(cfg)
			err := cfg.validateChatGPTSubscription()
			if tc.want == "" && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
	cfg := lab()
	if !cfg.ChatGPTSubscriptionEnabled() || cfg.ChatGPTSubscriptionModel() != DefaultChatGPTSubscriptionModel {
		t.Errorf("enabled=%v model=%q, want on with %s", cfg.ChatGPTSubscriptionEnabled(), cfg.ChatGPTSubscriptionModel(), DefaultChatGPTSubscriptionModel)
	}
	off := Default()
	if out, err := yaml.Marshal(off); err != nil || strings.Contains(string(out), "chatgptSubscription") {
		t.Errorf("a lab with the switch off writes chatgptSubscription (err %v): a release before it would refuse the file", err)
	}
	cfg.Platform.ChatGPTSubscription.Model = "gpt-5.5-codex"
	if got := cfg.ChatGPTSubscriptionModel(); got != "gpt-5.5-codex" {
		t.Errorf("ChatGPTSubscriptionModel() = %q, want the configured one", got)
	}
}
