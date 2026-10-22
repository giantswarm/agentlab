package lab

import (
	"maps"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// belowRange is a kagent range below the first release that serves the
// entry; credentialTypeKey the model entry's credential field.
const (
	belowRange        = ">=1.22.0 <1.23.0"
	credentialTypeKey = "credentialType" // #nosec G101 -- the field's name, no credential
)

// chatgptLab is a lab with a person's own ChatGPT login on: the codex
// Harness and the workspaces beside it, on a chart at the floor.
func chatgptLab(t *testing.T, chartRanges map[string]string) *config.Config {
	t.Helper()
	stubWorkspacesProbe(t, true)
	stubChatGPTChartRanges(t, chartRanges)
	cfg := workspaceLab("")
	cfg.Platform.ChartVersion = "4.164.0-rc.2"
	cfg.Platform.Coding.Enabled = true
	cfg.Platform.Coding.Harnesses = []string{config.CodingRuntimeClaude, config.CodingRuntimeCodex}
	cfg.Platform.ChatGPTSubscription.Enabled = true
	return cfg
}

// stubChatGPTChartRanges points the chart's range probe at ranges for the
// test and clears the memo, restored afterwards.
func stubChatGPTChartRanges(t *testing.T, ranges map[string]string) {
	t.Helper()
	prev := chatgptChartRangesProbe
	reset := func() {
		chatgptChartRangesMu.Lock()
		chatgptChartRangesCache = map[string]map[string]string{}
		chatgptChartRangesMu.Unlock()
	}
	reset()
	chatgptChartRangesProbe = func(platformChart) (map[string]string, error) { return ranges, nil }
	t.Cleanup(func() {
		chatgptChartRangesProbe = prev
		reset()
	})
}

// The switch renders the model entry without apiKeySecret, muster's token
// encryption and, over a chart whose ranges admit older releases, the
// floors of kagent, its CRDs and muster.
func TestChatGPTSubscriptionValues(t *testing.T) {
	cfg := chatgptLab(t, map[string]string{componentKagent: belowRange, componentKagentCRDs: belowRange, componentMuster: ">=5.49.0 <6.0.0"})
	values, raw, _, _ := renderWorkspaceTemplates(t, cfg, nil)

	kagent, _ := values["kagent"].(map[string]any)
	models, _ := kagent["modelConfigs"].([]any)
	if len(models) != 1 {
		t.Fatalf("kagent.modelConfigs = %v, want the one entry", models)
	}
	want := map[string]any{nameKey: config.ChatGPTSubscriptionModelConfig, "provider": "OpenAI", modelField: config.DefaultChatGPTSubscriptionModel, credentialTypeKey: config.CredentialTypeChatGPT, "apiFormat": "responses"}
	if got, _ := models[0].(map[string]any); !maps.Equal(got, want) {
		t.Errorf("kagent.modelConfigs[0] = %v, want %v (no apiKeySecret: the person's own login)", got, want)
	}

	muster, _ := values["muster"].(map[string]any)
	inner, _ := muster["muster"].(map[string]any)
	oauth, _ := inner["oauth"].(map[string]any)
	server, _ := oauth["server"].(map[string]any)
	if server["encryptionKey"] != true || server["existingSecret"] != platformSecretsName {
		t.Errorf("muster.muster.oauth.server encryptionKey=%v existingSecret=%v, want true in %s", server["encryptionKey"], server["existingSecret"], platformSecretsName)
	}
	if client, _ := oauth["mcpClient"].(map[string]any); client["enabled"] != true {
		t.Errorf("muster.muster.oauth.mcpClient = %v, want it on: the chart refuses provider Apps without it", client)
	}

	components, _ := values["components"].(map[string]any)
	for name, want := range map[string]string{componentKagent: kagentChatGPTRange, componentKagentCRDs: kagentChatGPTRange, componentMuster: musterChatGPTRange} {
		c, _ := components[name].(map[string]any)
		if c["versionRange"] != want {
			t.Errorf("components.%s.versionRange = %v, want %s:\n%s", name, c["versionRange"], want, excerptAround(raw, "versionRange"))
		}
	}
	if kc, _ := components[componentKagent].(map[string]any); kc["enabled"] != true || kc["postRenderers"] == nil {
		t.Errorf("components.kagent lost its switch or its patches beside the range: %v", kc)
	}
}

// A chart whose ranges already floor at the first releases keeps them: the
// lab renders no range over a chart that serves the entry.
func TestChatGPTSubscriptionChartRangesKept(t *testing.T) {
	cfg := chatgptLab(t, map[string]string{componentKagent: kagentChatGPTRange, componentKagentCRDs: ">=1.24.3 <1.25.0", componentMuster: ">=5.52.0 <6.0.0"})
	values, _, _, _ := renderWorkspaceTemplates(t, cfg, nil)
	components, _ := values["components"].(map[string]any)
	for _, name := range []string{componentKagent, componentKagentCRDs, componentMuster} {
		if c, _ := components[name].(map[string]any); c["versionRange"] != nil {
			t.Errorf("components.%s.versionRange = %v over a chart at the floor, want the chart's", name, c["versionRange"])
		}
	}
}

// With the switch off nothing of it renders.
func TestChatGPTSubscriptionOff(t *testing.T) {
	cfg := chatgptLab(t, nil)
	cfg.Platform.ChatGPTSubscription.Enabled = false
	_, raw, _, _ := renderWorkspaceTemplates(t, cfg, nil)
	for _, key := range []string{"modelConfigs:", "encryptionKey:", "chatgptOAuth"} {
		if strings.Contains(raw, key) {
			t.Errorf("the values carry %s with the switch off:\n%s", key, excerptAround(raw, key))
		}
	}
}

// rangeFloorsAt compares a range's floor the way the chart does: the
// release part of its >= bound, a range without one never at the floor.
func TestRangeFloorsAt(t *testing.T) {
	for _, tc := range []struct {
		versionRange, floor string
		want                bool
	}{
		{kagentChatGPTRange, kagentChatGPTFloor, true},
		{">=1.24.0-0 <1.25.0", kagentChatGPTFloor, true},
		{">= 1.25.1", kagentChatGPTFloor, true},
		{belowRange, kagentChatGPTFloor, false},
		{kagentChatGPTFloor, kagentChatGPTFloor, false},
		{"", kagentChatGPTFloor, false},
	} {
		if got := rangeFloorsAt(tc.versionRange, tc.floor); got != tc.want {
			t.Errorf("rangeFloorsAt(%q, %s) = %v, want %v", tc.versionRange, tc.floor, got, tc.want)
		}
	}
}
