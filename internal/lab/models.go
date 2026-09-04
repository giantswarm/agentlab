package lab

import (
	"fmt"
	"os"
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

// ensureExtraModels reconciles the platform.extraModels entries into kagent
// ModelConfig CRs plus their key Secrets, and prunes lab-labeled CRs whose
// entry is gone from agentlab.yaml. Only called with the agents runtime
// installed (the ModelConfig CRD ships with the kagent chart).
func ensureExtraModels(cfg *config.Config) error {
	_, path, err := renderManifest(cfg, "extra-models.yaml.tmpl")
	if err != nil {
		return err
	}
	if len(cfg.Platform.ExtraModels) > 0 {
		step("Adding extra model configs (%d beyond the default %s)", len(cfg.Platform.ExtraModels), cfg.AIModel)
	}
	// Secrets before the CRs: the controller hashes the referenced Secret
	// into the ModelConfig status, so the reference should resolve on the
	// controller's first look.
	for _, m := range cfg.Platform.ExtraModels {
		if err := ensureModelKeySecret(m); err != nil {
			return err
		}
	}
	if len(cfg.Platform.ExtraModels) > 0 {
		if err := runQuiet("kubectl", "apply", "-f", path); err != nil {
			return err
		}
	}
	if err := pruneExtraModels(cfg); err != nil {
		return err
	}
	for _, m := range cfg.Platform.ExtraModels {
		if err := waitModelConfigAccepted(m.Name); err != nil {
			return err
		}
	}
	return nil
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

// pruneExtraModels deletes lab-labeled ModelConfigs (and their Secrets) whose
// entry no longer exists in agentlab.yaml, so removing a model from the
// config is a real removal on the next run.
func pruneExtraModels(cfg *config.Config) error {
	out, err := outputQuiet("kubectl", "-n", kagentNamespace, "get", modelConfigResource,
		"-l", managedByAgentlab, "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return nil // no CRD / no namespace: nothing lab-owned to prune
	}
	want := map[string]bool{}
	for _, m := range cfg.Platform.ExtraModels {
		want[m.Name] = true
	}
	for _, name := range strings.Fields(out) {
		if want[name] {
			continue
		}
		note("pruning model config %s (removed from %s)", name, config.File)
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

// extraModelsHint names the extra ModelConfigs in the platform-up summary,
// e.g. " (+ qwen3-8-27b, gemini-flash)"; empty when there are none.
func extraModelsHint(cfg *config.Config) string {
	if len(cfg.Platform.ExtraModels) == 0 {
		return ""
	}
	names := make([]string, 0, len(cfg.Platform.ExtraModels))
	for _, m := range cfg.Platform.ExtraModels {
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
