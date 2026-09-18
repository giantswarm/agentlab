package lab

import (
	"fmt"

	"github.com/giantswarm/agentlab/internal/config"
)

// Model serving on llm-d in the lab (platform.serving): the serving slice a
// Giant Swarm installation runs, on the kind node. The meta chart brings the
// KServe LLMInferenceService CRDs and their controller
// (components.kserve-llmisvc-crd, kserve-llmisvc-resources), the well-known
// LLMInferenceServiceConfigs the controller composes an LLMInferenceService's
// pods from (components.kserve-runtime-configs, giantswarm/kserve: the llm-d
// images from gsoci) and, behind components.modelServing, the connectivity
// chart's serving objects — the serving namespace, the published presets and
// their discovery ConfigMap, the models Gateway every model's route attaches
// to, with its JWT policy against the lab Dex. model-manager's kserve backend
// composes a preset into an LLMInferenceService as the caller and wires the
// served model into kagent as a ModelConfig (agent-platform-values.yaml.tmpl
// has the blocks). The node has no GPU: the lab publishes one preset of its
// own, a small instruct model on the llm-d CPU runtime, requesting none, so
// model-manager places it on the node's CPU capacity. `agentlab serving-test`
// (servingtest.go) drives the path end to end.

const (
	// servingRuntimeImage is the runtime the lab preset runs on: the CPU
	// build of the llm-d release the well-known template pins
	// (kserve-config-llm-template names llm-d-cuda at the same tag),
	// mirrored to gsoci by giantswarm/llm-d. The template's image is the one
	// exception a preset makes (the ServingPreset schema: a preset names no
	// image, the well-known template does) — a kind node cannot run the
	// CUDA build — and the preset's description says so. Side-loaded like
	// every platform image (servingImages); the well-known configs' own
	// images stay out of the pull set (preload.go, templateKindRe).
	servingRuntimeImage = "gsoci.azurecr.io/giantswarm/llm-d-cpu:v0.8.0"
	// servingPresetName is the lab's ServingPreset (modelServing.presets in
	// the values template), the proof's model: the name of its ConfigMap,
	// of the LLMInferenceService model-manager composes from it and of the
	// ModelConfig it wires.
	servingPresetName = "qwen2-5-0-5b-cpu"
	// servingPresetModel is the preset's Hugging Face repository: ~1 GiB of
	// BF16 weights, tool calling through vLLM's hermes parser, which the
	// agent turn needs (agents send tool schemas with every request).
	servingPresetModel = "Qwen/Qwen2.5-0.5B-Instruct"
	// servingNamespace is the serving namespace (the connectivity chart's
	// default): where the LLMInferenceServices and their pods run.
	servingNamespace = "model-serving"
	// modelsGatewayName is the models Gateway in the release namespace — its
	// name, its data plane's Service and its hostname's first label:
	// <name>.<domain> (modelsGatewayHost).
	modelsGatewayName = "models"
	// llmisvcControllerDeployment is the llm-d controller's Deployment
	// (kserve-llmisvc-resources, release namespace).
	llmisvcControllerDeployment = "llmisvc-controller-manager"
	// llmisvcResourcesComponent is the meta chart's component the controller
	// comes from — the one serving component the lab renders a
	// postRenderers block for (render.go, ExtraPostRenderers).
	llmisvcResourcesComponent = "kserve-llmisvc-resources"
	// llmisvcTemplateConfig is the well-known config a composed
	// LLMInferenceService's template comes from.
	llmisvcTemplateConfig = "kserve-config-llm-template"
)

// cert-manager: the llm-d controller's webhook certificate is a cert-manager
// Certificate from a self-signed Issuer, its CA injected into the webhook
// configurations by the cainjector — a cluster prerequisite of the KServe
// charts on every installation, and of the serving switch here. The lab
// installs the Giant Swarm cert-manager-app from the catalog (the fleet's
// cert-manager, images from gsoci) before the platform chart, the way it
// installs the observability stack (installOCIChart), with the Giant
// Swarm-only objects a kind cluster cannot take turned off
// (cert-manager-values.yaml.tmpl).
const (
	certManagerChartVersion   = "4.1.1"
	certManagerChart          = "oci://gsoci.azurecr.io/charts/giantswarm/cert-manager-app"
	certManagerNamespace      = "cert-manager"
	certManagerRelease        = "cert-manager"
	certManagerValuesTemplate = "cert-manager-values.yaml.tmpl"
)

// ServingPresetName is the lab preset's name, for the configure summary.
const ServingPresetName = servingPresetName

// ModelsGatewayHost is modelsGatewayHost for the configure summary.
func ModelsGatewayHost(cfg *config.Config) string { return modelsGatewayHost(cfg) }

// servingValues are the names the values template's serving blocks and the
// CoreDNS rewrite render.
type servingValues struct {
	RuntimeImage string
	PresetName   string
	Model        string
	Namespace    string
	GatewayName  string
}

func servingValuesFor() servingValues {
	return servingValues{
		RuntimeImage: servingRuntimeImage,
		PresetName:   servingPresetName,
		Model:        servingPresetModel,
		Namespace:    servingNamespace,
		GatewayName:  modelsGatewayName,
	}
}

// modelsGatewayHost is the models Gateway's hostname: <name>.<domain>, what
// the connectivity chart renders as the Gateway's listener hostname and the
// discovery ConfigMap publishes as the gateway endpoint's host.
func modelsGatewayHost(cfg *config.Config) string {
	return modelsGatewayName + "." + cfg.Platform.Domain
}

// modelsGatewayURL is the models Gateway's origin, https://<host>: the
// discovery ConfigMap's spec.gateway.endpoint, under which a served model
// answers at /<namespace>/<name>/v1/....
func modelsGatewayURL(cfg *config.Config) string {
	return "https://" + modelsGatewayHost(cfg)
}

// certManagerUp installs cert-manager into its own namespace (an idempotent
// upgrade-or-install; a lab that has it already is a no-op).
func certManagerUp(cfg *config.Config) error {
	step("Installing cert-manager %s (the llm-d controller's webhook certificate)", certManagerChartVersion)
	_, valuesPath, err := renderManifest(cfg, certManagerValuesTemplate)
	if err != nil {
		return err
	}
	values, err := helmValuesFile(valuesPath)
	if err != nil {
		return err
	}
	return installOCIChart(cfg, certManagerNamespace, certManagerRelease, certManagerChart, certManagerChartVersion, values, ociChartInstallTimeout)
}

// servingHint is the platform-up summary for the serving switch: what runs,
// where a served model answers, the proof.
func servingHint(cfg *config.Config) string {
	if !cfg.ServingEnabled() {
		return "  Model serving on llm-d is off (platform.serving in agentlab.yaml; `agentlab configure --serving` turns it on)."
	}
	return fmt.Sprintf("  Model serving on llm-d: the llmisvc controller and the well-known runtime configs, the lab preset %s (%s on the CPU\n"+
		"  runtime, no GPU) among the shipped ones; a served model answers at %s/%s/<preset>/v1 with a Dex token\n"+
		"  (401 without; in-cluster through the CoreDNS rewrite, from here through a port-forward to the Gateway's data plane). Proof: `agentlab serving-test`.",
		servingPresetName, servingPresetModel, modelsGatewayURL(cfg), servingNamespace)
}

// servingImages are the images the serving switch's pods run that no chart
// render names as a pod's: the lab preset's runtime. Empty while the switch
// is off.
func servingImages(cfg *config.Config) []string {
	if !cfg.ServingEnabled() {
		return nil
	}
	return []string{servingRuntimeImage}
}
