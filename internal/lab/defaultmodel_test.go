package lab

import (
	"maps"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// testModelConfig is the ModelConfig the unit tests' agents name.
const testModelConfig = config.DefaultModelConfig

// TestDefaultModelConfigOf: the one entry list_model_configs reports
// `default: true` is the default; an answer that carries the field and marks
// none is refused with the names; an agent-manager before the field answers
// the kagent chart's default-model-config when it lists it.
func TestDefaultModelConfigOf(t *testing.T) {
	const unmarkedModel = "unmarked-model"
	yes, no := true, false
	cases := []struct {
		name    string
		configs []listedModelConfig
		want    string
		wantErr string
	}{
		{name: "marked", configs: []listedModelConfig{{Name: unmarkedModel, Default: &no}, {Name: config.DefaultModelConfig, Default: &yes}}, want: config.DefaultModelConfig},
		{name: "none marked", configs: []listedModelConfig{{Name: "a", Default: &no}, {Name: "b", Default: &no}}, wantErr: "no default ModelConfig in kagent among a, b"},
		{name: "two marked", configs: []listedModelConfig{{Name: "a", Default: &yes}, {Name: "b", Default: &yes}}, wantErr: "2 defaults"},
		{name: "before the field", configs: []listedModelConfig{{Name: unmarkedModel}, {Name: config.KagentChartDefaultModelConfig}}, want: config.KagentChartDefaultModelConfig},
		{name: "before the field without the chart's default", configs: []listedModelConfig{{Name: unmarkedModel}}, wantErr: "lists no " + config.KagentChartDefaultModelConfig},
		{name: "no ModelConfig", wantErr: "lists no ModelConfig"},
		{name: "transition name reported unmarked", configs: []listedModelConfig{{Name: config.KagentChartDefaultModelConfig, Default: &no}}, wantErr: "no default ModelConfig"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := defaultModelConfigOf(c.configs)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("defaultModelConfigOf = %q, %v; want an error with %q", got, err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("defaultModelConfigOf = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

// TestDefaultModelValuesFor: the marked entry and the agent-manager range
// render from the default model floor on, with the agents; below it, on the
// 3.x line and without the agents neither does, and the default is the kagent
// chart's.
func TestDefaultModelValuesFor(t *testing.T) {
	cfg := labConfig(true, true, false, false, false)
	got := defaultModelValuesFor(cfg)
	want := defaultModelValues{Name: config.DefaultModelConfig, Model: cfg.AIModel, Secret: anthropicSecret, SecretKey: anthropicSecretKey}
	if got == nil || *got != want {
		t.Fatalf("defaultModelValuesFor on %s = %+v, want %+v", cfg.Platform.ChartVersion, got, want)
	}
	if r := agentManagerRangeFor(cfg); r != agentManagerDefaultModelRange {
		t.Errorf("agentManagerRangeFor = %q, want %q", r, agentManagerDefaultModelRange)
	}
	if name := renderedDefaultModelConfig(cfg); name != config.DefaultModelConfig {
		t.Errorf("renderedDefaultModelConfig = %q", name)
	}

	for _, version := range []string{"4.174.3", "4.175.0-rc.6", "3.23.1"} {
		below := labConfig(true, true, false, false, false)
		below.Platform.ChartVersion = version
		if v := defaultModelValuesFor(below); v != nil || agentManagerRangeFor(below) != "" {
			t.Errorf("%s: entry %+v, range %q; want neither", version, v, agentManagerRangeFor(below))
		}
		if name := renderedDefaultModelConfig(below); name != config.KagentChartDefaultModelConfig {
			t.Errorf("%s: renderedDefaultModelConfig = %q", version, name)
		}
	}

	noAgents := labConfig(true, false, false, false, false)
	if v := defaultModelValuesFor(noAgents); v != nil || agentManagerRangeFor(noAgents) != "" {
		t.Errorf("without the agents: entry %+v, range %q", v, agentManagerRangeFor(noAgents))
	}
}

// TestPlatformValuesDefaultModel: from the floor on the values mark the lab's
// Anthropic entry default, turn the kagent chart's own default off, render no
// kagent.providers and take agent-manager from the first release that reports
// the default; a ChatGPT subscription entry joins the same list.
func TestPlatformValuesDefaultModel(t *testing.T) {
	cfg := chatgptLab(t, nil)
	cfg.Platform.ChartVersion = config.DefaultChartVersion
	values, raw, _, _ := renderWorkspaceTemplates(t, cfg, nil)

	kagent, _ := values["kagent"].(map[string]any)
	if _, ok := kagent["providers"]; ok {
		t.Errorf("kagent.providers rendered beside the marked entry:\n%s", excerptAround(raw, "providers:"))
	}
	if got, _ := kagent["defaultModelConfig"].(map[string]any); got["enabled"] != false {
		t.Errorf("kagent.defaultModelConfig = %v, want enabled: false", got)
	}
	models, _ := kagent["modelConfigs"].([]any)
	if len(models) != 2 {
		t.Fatalf("kagent.modelConfigs = %v, want the marked entry and the ChatGPT one", models)
	}
	want := map[string]any{nameKey: config.DefaultModelConfig, modelConfigProviderField: "Anthropic", modelField: cfg.AIModel, "apiKeySecret": anthropicSecret, "apiKeySecretKey": anthropicSecretKey, modelConfigDefaultField: true}
	if got, _ := models[0].(map[string]any); !maps.Equal(got, want) {
		t.Errorf("kagent.modelConfigs[0] = %v, want %v", got, want)
	}
	if got, _ := models[1].(map[string]any); got[nameKey] != config.ChatGPTSubscriptionModelConfig || got[modelConfigDefaultField] != nil {
		t.Errorf("kagent.modelConfigs[1] = %v, want the unmarked ChatGPT entry", got)
	}
	components, _ := values["components"].(map[string]any)
	if am, _ := components["agent-manager"].(map[string]any); am["versionRange"] != agentManagerDefaultModelRange || am["enabled"] != true {
		t.Errorf("components.agent-manager = %v, want enabled with versionRange %s", am, agentManagerDefaultModelRange)
	}
}
