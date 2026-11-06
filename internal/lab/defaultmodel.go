package lab

import (
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

// The agents' default model, by chart line:
//   - config.ModelObjectsChart: the chart declares no model, and the lab
//     applies config.DefaultModelConfig as a ModelConfig object annotated
//     defaultModelAnnotation (platformmodels.go);
//   - config.DefaultModelChart below that: a kagent.modelConfigs[] entry
//     marked `default: true`, which the connectivity chart renders annotated
//     defaultModelAnnotation, with the kagent chart's own default-model-config
//     off;
//   - below it: the kagent chart's default-model-config from
//     kagent.providers.
//
// Every line's Secret is anthropicSecret, placed by anthropic.go. A proof
// that names no model asks agent-manager for the namespace's default
// (agentManagerModelConfig); it never assumes a name.

// defaultModelAnnotation, with defaultModelAnnotationValue, marks a
// namespace's default ModelConfig; modelConfigDefaultField and
// modelConfigProviderField are the kagent.modelConfigs[] keys of the mark and
// the provider.
const (
	defaultModelAnnotation      = "agent-platform.giantswarm.io/is-default-model-config"
	defaultModelAnnotationValue = "true"
	modelConfigDefaultField     = "default"
	modelConfigProviderField    = "provider"
)

// defaultModelValues is the lab's default model, which the values template
// renders as the marked kagent.modelConfigs[] entry and
// platform-models.yaml.tmpl as a ModelConfig object.
type defaultModelValues struct {
	Name, Model, Secret, SecretKey string
}

// defaultModelValuesFor is the lab's default model, nil without the agents
// or below the default model floor.
func defaultModelValuesFor(cfg *config.Config) *defaultModelValues {
	if !cfg.Platform.Agents || !cfg.DefaultModelChart() {
		return nil
	}
	return &defaultModelValues{Name: config.DefaultModelConfig, Model: cfg.AIModel, Secret: anthropicSecret, SecretKey: anthropicSecretKey}
}

// renderedDefaultModelConfig is the ModelConfig that is the kagent
// namespace's default on the lab's chart line.
func renderedDefaultModelConfig(cfg *config.Config) string {
	if cfg.DefaultModelChart() {
		return config.DefaultModelConfig
	}
	return config.KagentChartDefaultModelConfig
}

// listedModelConfig is one ModelConfig of list_model_configs' answer.
// Default is nil from an agent-manager that predates the field.
type listedModelConfig struct {
	Name    string `json:"name"`
	Default *bool  `json:"default"`
}

// agentManagerModelConfig is the ModelConfig the proofs run their agents on
// when none is named: the kagent namespace's default, as list_model_configs
// reports it to the session's person (defaultModelConfigOf).
func agentManagerModelConfig(s *musterSession) (string, error) {
	var listed struct {
		ModelConfigs []listedModelConfig `json:"modelConfigs"`
	}
	if err := s.callServerJSON(agentManagerToolPrefix+"list_model_configs", nil, &listed); err != nil {
		return "", err
	}
	return defaultModelConfigOf(listed.ModelConfigs)
}

// labDefaultModelConfig is agentManagerModelConfig for a proof that holds a
// token and no muster session.
func labDefaultModelConfig(cfg *config.Config, token string) (string, error) {
	s, err := openMusterSession(cfg, token, "agentlab-default-model")
	if err != nil {
		return "", fmt.Errorf("the default ModelConfig from agent-manager: %w", err)
	}
	return agentManagerModelConfig(s)
}

// defaultModelConfigOf picks the default of a list_model_configs answer: the
// one entry it reports `default: true`. An answer without the field on any
// entry comes from an agent-manager before 2.19, on a line whose kagent chart
// renders the default itself, so the default is that chart's
// default-model-config.
func defaultModelConfigOf(configs []listedModelConfig) (string, error) {
	if len(configs) == 0 {
		return "", fmt.Errorf("list_model_configs lists no ModelConfig in %s: the lab's default model is missing", kagentNamespace)
	}
	names := make([]string, 0, len(configs))
	reported := false
	var marked []string
	for _, mc := range configs {
		names = append(names, mc.Name)
		if mc.Default == nil {
			continue
		}
		reported = true
		if *mc.Default {
			marked = append(marked, mc.Name)
		}
	}
	switch {
	case len(marked) == 1:
		return marked[0], nil
	case len(marked) > 1:
		return "", fmt.Errorf("list_model_configs reports %d defaults in %s (%s): a namespace has at most one", len(marked), kagentNamespace, strings.Join(marked, ", "))
	case reported:
		return "", fmt.Errorf("list_model_configs reports no default ModelConfig in %s among %s: the lab marks %s (annotation %s)", kagentNamespace, strings.Join(names, ", "), config.DefaultModelConfig, defaultModelAnnotation)
	case slices.Contains(names, config.KagentChartDefaultModelConfig):
		return config.KagentChartDefaultModelConfig, nil
	default:
		return "", fmt.Errorf("list_model_configs reports no default in %s and lists no %s among %s", kagentNamespace, config.KagentChartDefaultModelConfig, strings.Join(names, ", "))
	}
}
