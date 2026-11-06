package lab

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// Keys and values of the lab's ModelConfig objects, and the last chart whose
// values carry the lab's models.
const (
	keyAnnotations     = "annotations"
	keyAPIKeySecret    = "apiKeySecret"    // #nosec G101 -- the field's name, no credential
	keyAPIKeySecretKey = "apiKeySecretKey" // #nosec G101 -- the field's name, no credential
	keyAnthropic       = "anthropic"
	apiFormatResponses = "responses"
	providerAnthropic  = "Anthropic"
	modelEntriesChart  = "4.175.0-rc.21"
	kagentProvidersKey = "providers"
)

// renderPlatformModels renders platform-models.yaml.tmpl as the platform run
// does and decodes its objects.
func renderPlatformModels(t *testing.T, cfg *config.Config) []map[string]any {
	t.Helper()
	out, err := renderTemplate(cfg, platformModelsTemplate, platformModelsData)
	if err != nil {
		t.Fatal(err)
	}
	var docs []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs
}

// wantDefaultModelObject is the lab's default ModelConfig object.
func wantDefaultModelObject(cfg *config.Config) map[string]any {
	return map[string]any{
		fieldAPIVersion: kagentAPIVersion,
		fieldKind:       organizationModelConfigKind,
		crMetadata: map[string]any{
			nameKey:        config.DefaultModelConfig,
			namespaceKey:   kagentNamespace,
			fieldLabels:    map[string]any{managedByLabel: managedByAgentlabValue},
			keyAnnotations: map[string]any{defaultModelAnnotation: defaultModelAnnotationValue},
		},
		crSpec: map[string]any{
			modelConfigProviderField: providerAnthropic,
			modelField:               cfg.AIModel,
			keyAPIKeySecret:          anthropicSecret,
			keyAPIKeySecretKey:       anthropicSecretKey,
			keyAnthropic:             map[string]any{"promptCaching": true, "cacheTTL": "5m"},
		},
	}
}

// wantChatGPTModelObject is the lab's ChatGPT login ModelConfig object: no
// apiKeySecret and no baseUrl.
func wantChatGPTModelObject(cfg *config.Config) map[string]any {
	return map[string]any{
		fieldAPIVersion: kagentAPIVersion,
		fieldKind:       organizationModelConfigKind,
		crMetadata: map[string]any{
			nameKey:        config.ChatGPTSubscriptionModelConfig,
			namespaceKey:   kagentNamespace,
			fieldLabels:    map[string]any{managedByLabel: managedByAgentlabValue},
			keyAnnotations: map[string]any{"ui.giantswarm.io/display-name": "ChatGPT (your subscription)"},
		},
		crSpec: map[string]any{
			modelConfigProviderField: "OpenAI",
			modelField:               cfg.ChatGPTSubscriptionModel(),
			"openAI":                 map[string]any{"apiFormat": apiFormatResponses, credentialTypeKey: config.CredentialTypeChatGPT},
		},
	}
}

// On a chart that declares no model the template renders the default
// ModelConfig annotated the namespace's default and, with the switch on, the
// ChatGPT login's; below that chart, or without the agents, nothing.
func TestPlatformModelsTemplate(t *testing.T) {
	cfg := chatgptLab(t, nil)
	cfg.Platform.ChartVersion = config.DefaultChartVersion
	got := renderPlatformModels(t, cfg)
	want := []map[string]any{wantDefaultModelObject(cfg), wantChatGPTModelObject(cfg)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("platform models with the ChatGPT login:\n--- got\n%s\n--- want\n%s", mustYAML(t, got), mustYAML(t, want))
	}

	cfg.Platform.ChatGPTSubscription.Enabled = false
	got = renderPlatformModels(t, cfg)
	want = []map[string]any{wantDefaultModelObject(cfg)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("platform models without the ChatGPT login:\n--- got\n%s\n--- want\n%s", mustYAML(t, got), mustYAML(t, want))
	}

	for name, mutate := range map[string]func(*config.Config){
		"below the model objects floor": func(c *config.Config) { c.Platform.ChartVersion = modelEntriesChart },
		"below the default model floor": func(c *config.Config) { c.Platform.ChartVersion = "4.175.0-rc.14" },
		"without the agents":            func(c *config.Config) { c.Platform.Agents = false },
	} {
		c := chatgptLab(t, nil)
		c.Platform.ChartVersion = config.DefaultChartVersion
		mutate(c)
		if got := renderPlatformModels(t, c); len(got) != 0 {
			t.Errorf("%s: rendered %d ModelConfigs, want none:\n%s", name, len(got), mustYAML(t, got))
		}
	}
}

// ensurePlatformModels applies the lab's ModelConfigs, removes the lab's
// ChatGPT login model once the switch is off (never one the lab did not
// apply), and leaves an older chart's models to its values.
func TestEnsurePlatformModels(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := t.Context()
	cfg := chatgptLab(t, nil)
	cfg.Platform.ChartVersion = config.DefaultChartVersion

	newFakeLab(t)
	if err := ensurePlatformModels(cfg); err != nil {
		t.Fatalf("ensurePlatformModels: %v", err)
	}
	for _, want := range []map[string]any{wantDefaultModelObject(cfg), wantChatGPTModelObject(cfg)} {
		name := want[crMetadata].(map[string]any)[nameKey].(string)
		obj, err := getObject(ctx, gvrModelConfigs, kagentNamespace, name)
		if err != nil {
			t.Fatalf("ModelConfig %s after the apply: %v", name, err)
		}
		if !reflect.DeepEqual(obj.Object[crSpec], want[crSpec]) || !reflect.DeepEqual(obj.GetAnnotations(), stringMap(want[crMetadata].(map[string]any)[keyAnnotations])) {
			t.Errorf("ModelConfig %s applied as\n%s\nwant\n%s", name, mustYAML(t, obj.Object), mustYAML(t, want))
		}
	}

	cfg.Platform.ChatGPTSubscription.Enabled = false
	if err := ensurePlatformModels(cfg); err != nil {
		t.Fatalf("ensurePlatformModels with the switch off: %v", err)
	}
	if hasModelConfig(t, config.ChatGPTSubscriptionModelConfig) {
		t.Errorf("ModelConfig %s stays with the switch off", config.ChatGPTSubscriptionModelConfig)
	}
	if !hasModelConfig(t, config.DefaultModelConfig) {
		t.Errorf("ModelConfig %s went with the ChatGPT login", config.DefaultModelConfig)
	}

	newFakeLab(t, customObject(gvkModelConfig, kagentNamespace, config.ChatGPTSubscriptionModelConfig, nil))
	if err := ensurePlatformModels(cfg); err != nil {
		t.Fatalf("ensurePlatformModels beside a ModelConfig the lab did not apply: %v", err)
	}
	if !hasModelConfig(t, config.ChatGPTSubscriptionModelConfig) {
		t.Errorf("ModelConfig %s without the lab's label was deleted", config.ChatGPTSubscriptionModelConfig)
	}

	older := chatgptLab(t, nil)
	older.Platform.ChartVersion = modelEntriesChart
	newFakeLab(t)
	if err := ensurePlatformModels(older); err != nil {
		t.Fatalf("ensurePlatformModels on %s: %v", older.Platform.ChartVersion, err)
	}
	for _, name := range platformModelNames {
		if hasModelConfig(t, name) {
			t.Errorf("%s: ModelConfig %s applied on a chart whose values carry it", older.Platform.ChartVersion, name)
		}
	}
}

// pruneExtraModels never deletes the lab's own ModelConfigs, labelled as
// the extra models are, and still prunes a removed extra model.
func TestPruneExtraModelsKeepsPlatformModels(t *testing.T) {
	labelled := map[string]string{managedByLabel: managedByAgentlabValue}
	newFakeLab(t,
		customObject(gvkModelConfig, kagentNamespace, config.DefaultModelConfig, labelled),
		customObject(gvkModelConfig, kagentNamespace, config.ChatGPTSubscriptionModelConfig, labelled),
		customObject(gvkModelConfig, kagentNamespace, "removed-extra", labelled),
	)
	if err := pruneExtraModels(nil); err != nil {
		t.Fatalf("pruneExtraModels: %v", err)
	}
	for _, name := range platformModelNames {
		if !hasModelConfig(t, name) {
			t.Errorf("ModelConfig %s pruned", name)
		}
	}
	if hasModelConfig(t, "removed-extra") {
		t.Error("the removed extra model was not pruned")
	}
}

// hasModelConfig reports whether the kagent namespace holds the ModelConfig.
func hasModelConfig(t *testing.T, name string) bool {
	t.Helper()
	ok, err := objectExists(t.Context(), gvrModelConfigs, kagentNamespace, name)
	if err != nil {
		t.Fatalf("ModelConfig %s: %v", name, err)
	}
	return ok
}

// stringMap is a decoded YAML mapping of strings as metadata carries it.
func stringMap(v any) map[string]string {
	m, _ := v.(map[string]any)
	out := make(map[string]string, len(m))
	for k, s := range m {
		out[k], _ = s.(string)
	}
	return out
}
