package lab

import (
	"context"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/giantswarm/agentlab/internal/config"
)

// The lab's own ModelConfigs on a chart that declares no model
// (config.ModelObjectsChart): the agents' default, annotated
// defaultModelAnnotation, and with platform.chatgptSubscription a person's own
// ChatGPT login, beside which the chart wires muster's provider App chatgpt
// and kagent's installation provider chatgpt. They are plain objects applied
// after the install, as an installation declares its models next to its
// agents (platform-models.yaml.tmpl).

// platformModelsTemplate renders the lab's own ModelConfigs.
const platformModelsTemplate = "platform-models.yaml.tmpl"

// platformModelNames are the ModelConfigs ensurePlatformModels owns, which
// pruneExtraModels never touches.
var platformModelNames = []string{config.DefaultModelConfig, config.ChatGPTSubscriptionModelConfig}

// platformModelsData sets what platform-models.yaml.tmpl renders beyond
// newTmplData: the ModelConfig apiVersion of a chart that declares no model,
// the kagent API v2 line's.
func platformModelsData(d *tmplData) {
	d.ModelConfigAPIVersion = kagentAPIVersion
}

// ensurePlatformModels applies the lab's own ModelConfigs on a chart that
// declares no model and deletes the lab's ChatGPT login model once the switch
// is off; on an older chart the values carry them and it does nothing. Only
// called with the agents runtime installed, after the default model's Secret
// is placed: the controller resolves the reference on its first look.
func ensurePlatformModels(cfg *config.Config) error {
	if !cfg.ModelObjectsChart() {
		return nil
	}
	ctx := context.Background()
	rendered, _, err := renderManifestWith(cfg, platformModelsTemplate, platformModelsData)
	if err != nil {
		return err
	}
	if _, err := applyManifests(ctx, rendered); err != nil {
		return err
	}
	if cfg.ChatGPTSubscriptionEnabled() {
		return nil
	}
	return removeLabModel(ctx, config.ChatGPTSubscriptionModelConfig)
}

// removeLabModel deletes one of the lab's own ModelConfigs when it carries
// the lab's label; an absent one, or one the lab did not apply, stays as it is.
func removeLabModel(ctx context.Context, name string) error {
	gvr, err := modelConfigGVR()
	if err != nil {
		return err
	}
	obj, err := getObject(ctx, gvr, kagentNamespace, name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if obj.GetLabels()[managedByLabel] != managedByAgentlabValue {
		return nil
	}
	note("removing model config %s (platform.chatgptSubscription is off)", name)
	return deleteObject(ctx, gvr, kagentNamespace, name, modelDeleteWait)
}

// isPlatformModel reports whether a ModelConfig is one of the lab's own.
func isPlatformModel(name string) bool {
	return slices.Contains(platformModelNames, name)
}
