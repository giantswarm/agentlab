package lab

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// kagentNamespace is where the kagent subchart puts its runtime and where
// every ModelConfig and key Secret lives.
const kagentNamespace = "kagent"

// placeholderAPIKey fills a model's key Secret when the entry names no env
// var (a keyless endpoint, e.g. a local vLLM). The Secret still has to exist
// with the provider's canonical key: the kagent controller injects it as an
// env var into agent pods and the ADK runtime requires that env var at
// startup — an agent pod without it crashloops even against an endpoint that
// never checks the value.
const placeholderAPIKey = "agentlab-placeholder"

// modelConfigResource is fully qualified on purpose: like MCPServer (muster
// vs kagent.dev), a bare kind is one CRD collision away from resolving into
// the wrong API group.
const modelConfigResource = "modelconfigs.kagent.dev"

// managedByAgentlab labels the extra ModelConfigs so pruning can be scoped to
// lab-owned CRs — the chart-rendered default ModelConfig is never touched.
const managedByAgentlab = "app.kubernetes.io/managed-by=agentlab"

// extraModelsTemplate renders the lab-owned ModelConfigs.
const extraModelsTemplate = "extra-models.yaml.tmpl"

// allExtraModels is everything the lab renders as its own ModelConfigs: the
// platform.extraModels entries plus the tool-calling models of the host
// backends model-manager does not front (wired, returned separately for the
// summary). endpoints may be nil, in which case they are resolved here.
func allExtraModels(cfg *config.Config, endpoints map[string]string) (all, wired []config.ExtraModel, err error) {
	manual := cfg.Platform.ExtraModels
	if cfg.ModelManagerEnabled() && len(cfg.Platform.ModelManager.Secondary()) > 0 {
		if endpoints == nil {
			if endpoints, err = resolveBackendEndpoints(cfg); err != nil {
				return nil, nil, err
			}
		}
		if wired, err = hostBackendModelConfigs(cfg, endpoints); err != nil {
			return nil, nil, err
		}
	}
	return append(slices.Clone(manual), wired...), wired, nil
}

// ensureExtraModels reconciles the platform.extraModels entries — and the
// statically wired models of the host backends model-manager does not front
// — into kagent ModelConfig CRs plus their key Secrets, and prunes
// lab-labeled CRs whose entry is gone (from agentlab.yaml, or from the host
// server). Only called with the agents runtime installed (the ModelConfig
// CRD ships with the kagent chart). Returns the wired host models for the
// summary.
func ensureExtraModels(cfg *config.Config, endpoints map[string]string) ([]config.ExtraModel, error) {
	models, wired, err := allExtraModels(cfg, endpoints)
	if err != nil {
		return nil, err
	}
	_, path, err := renderManifestWith(cfg, extraModelsTemplate, func(d *tmplData) { d.ExtraModels = models })
	if err != nil {
		return nil, err
	}
	if len(cfg.Platform.ExtraModels) > 0 {
		step("Adding extra model configs (%d beyond the default %s)", len(cfg.Platform.ExtraModels), cfg.AIModel)
	}
	for _, b := range cfg.Platform.ModelManager.Secondary() {
		var names []string
		for _, m := range wired {
			if strings.HasPrefix(m.Name, b+"-") {
				names = append(names, m.Name)
			}
		}
		step("Wiring %s's tool-calling models as ModelConfigs (%d; model-manager fronts %s only, agentlab#60)",
			config.BackendServerName(b), len(names), cfg.Platform.ModelManager.Primary())
		if len(names) > 0 {
			note("%s", strings.Join(names, ", "))
		}
	}
	// Secrets before the CRs: the controller hashes the referenced Secret
	// into the ModelConfig status, so the reference should resolve on the
	// controller's first look.
	for _, m := range models {
		if err := ensureModelKeySecret(m); err != nil {
			return nil, err
		}
	}
	if len(models) > 0 {
		if err := runQuiet("kubectl", "apply", "-f", path); err != nil {
			return nil, err
		}
	}
	if err := pruneExtraModels(models); err != nil {
		return nil, err
	}
	for _, m := range models {
		if err := waitModelConfigAccepted(m.Name); err != nil {
			return nil, err
		}
	}
	return wired, nil
}

// ensureModelKeySecret creates the model's key Secret from the host env var
// the entry names — the same env -> Secret path as the Anthropic key: real
// credentials never enter agentlab.yaml or state/. An existing Secret is
// left alone (delete it and re-run to rotate). A missing value degrades to
// the placeholder so agent pods still boot; the model then fails auth at
// call time, which is the visible, recoverable failure.
func ensureModelKeySecret(m config.ExtraModel) error {
	if !m.NeedsSecret() {
		return nil
	}
	if _, err := outputQuiet("kubectl", "-n", kagentNamespace, "get", "secret", m.SecretName()); err == nil {
		note("secret %s/%s already exists, leaving it alone (delete it and re-run to rotate)", kagentNamespace, m.SecretName())
		return nil
	}
	key := placeholderAPIKey
	switch {
	case m.APIKeyEnv == "":
		note("model %s: no apiKeyEnv configured — using a placeholder key (keyless endpoint)", m.Name)
	case os.Getenv(m.APIKeyEnv) == "":
		note("model %s: $%s is not set — using a placeholder key; agents on this model fail auth until:", m.Name, m.APIKeyEnv)
		note("  kubectl -n %s delete secret %s && export %s=... && re-run `agentlab platform`", kagentNamespace, m.SecretName(), m.APIKeyEnv)
	default:
		key = os.Getenv(m.APIKeyEnv)
	}
	// Applied via stdin so the key never appears in a process's argv.
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
stringData:
  %s: %q
`, m.SecretName(), kagentNamespace, m.SecretKey(), key)
	if err := pipeInto([]byte(manifest), "kubectl", "apply", "-f", "-"); err != nil {
		return err
	}
	if key != placeholderAPIKey {
		note("created secret %s/%s from $%s", kagentNamespace, m.SecretName(), m.APIKeyEnv)
	}
	return nil
}

// pruneExtraModels deletes lab-labeled ModelConfigs (and their Secrets) that
// are not among the wanted entries any more — removed from agentlab.yaml, or
// gone from the host server they were wired from — so a removal is a real
// removal on the next run.
func pruneExtraModels(want []config.ExtraModel) error {
	out, err := outputQuiet("kubectl", "-n", kagentNamespace, "get", modelConfigResource,
		"-l", managedByAgentlab, "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return nil // no CRD / no namespace: nothing lab-owned to prune
	}
	keep := map[string]bool{}
	for _, m := range want {
		keep[m.Name] = true
	}
	for _, name := range strings.Fields(out) {
		if keep[name] {
			continue
		}
		note("pruning model config %s (removed from %s or gone from its host server)", name, config.File)
		if err := runQuiet("kubectl", "-n", kagentNamespace, "delete", modelConfigResource, name); err != nil {
			return err
		}
		// Its key Secret rides along; --ignore-not-found because keyless
		// providers never had one.
		if err := runQuiet("kubectl", "-n", kagentNamespace, "delete", "secret", "kagent-"+name, "--ignore-not-found"); err != nil {
			return err
		}
	}
	return nil
}

// extraModelsHint names the lab's ModelConfigs in the platform-up summary,
// e.g. " (+ qwen3-8-27b, gemini-flash)"; empty when there are none.
func extraModelsHint(models []config.ExtraModel) string {
	if len(models) == 0 {
		return ""
	}
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Name)
	}
	return " (+ " + strings.Join(names, ", ") + ")"
}

// waitModelConfigAccepted polls one ModelConfig until the controller accepts
// it — the machine check that the provider/model/Secret combination is one
// the runtime can mount, before anyone debugs it from a failing agent pod.
func waitModelConfigAccepted(name string) error {
	var status string
	var readErr error
	accepted := waitFor(10, 2*time.Second, func() bool {
		status, readErr = outputQuiet("kubectl", "-n", kagentNamespace, "get", modelConfigResource, name,
			"-o", `jsonpath={.status.conditions[?(@.type=="Accepted")].status}`)
		return readErr == nil && status == "True"
	})
	if !accepted {
		return notReached("ModelConfig "+name, "Accepted", status, readErr,
			fmt.Sprintf("check `kubectl -n %s describe %s %s`", kagentNamespace, modelConfigResource, name))
	}
	note("ModelConfig %s: Accepted", name)
	return nil
}
