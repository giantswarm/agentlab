package lab

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

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

// modelConfigAcceptedPoll is the cadence of waitModelConfigAccepted's ten
// looks (a variable so the tests need not wait it out).
var modelConfigAcceptedPoll = 2 * time.Second

// kubeReadTimeout bounds one read of the apiserver in the proofs and
// fixtures — a status poll's look, an RBAC question, an identity probe.
const kubeReadTimeout = 15 * time.Second

// modelDeleteWait bounds how long a pruned ModelConfig or key Secret may take
// to go away — kubectl's default delete waits too.
const modelDeleteWait = 2 * time.Minute

// ensureExtraModels reconciles the platform.extraModels entries into kagent
// ModelConfig CRs plus their key Secrets, and prunes lab-labeled CRs whose
// entry is gone from agentlab.yaml. Only called with the agents runtime
// installed (the ModelConfig CRD ships with the kagent chart). The host model
// servers' models are model-manager's: it writes and removes their
// ModelConfigs itself, for every backend it fronts.
func ensureExtraModels(cfg *config.Config) error {
	models := cfg.Platform.ExtraModels
	rendered, _, err := renderManifestWith(cfg, extraModelsTemplate, func(d *tmplData) { d.ExtraModels = models })
	if err != nil {
		return err
	}
	if len(models) > 0 {
		step("Adding extra model configs (%d beyond the default %s)", len(models), cfg.AIModel)
	}
	// Secrets before the CRs: the controller hashes the referenced Secret
	// into the ModelConfig status, so the reference should resolve on the
	// controller's first look.
	for _, m := range models {
		if err := ensureModelKeySecret(m); err != nil {
			return err
		}
	}
	if len(models) > 0 {
		if _, err := applyManifests(context.Background(), rendered); err != nil {
			return err
		}
	}
	if err := pruneExtraModels(models); err != nil {
		return err
	}
	for _, m := range models {
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
	exists, err := objectExists(context.Background(), gvrSecrets, kagentNamespace, m.SecretName())
	if err != nil {
		return err
	}
	if exists {
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
	// Applied in-process: the key never appears in a process's argv or in a
	// rendered file.
	if err := ensureSecret(kagentNamespace, m.SecretName(), corev1.SecretTypeOpaque, map[string][]byte{m.SecretKey(): []byte(key)}); err != nil {
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
	ctx := context.Background()
	gvr, err := gvrFor(modelConfigResource)
	if err != nil {
		return nil // no CRD: nothing lab-owned to prune
	}
	existing, err := listObjects(ctx, gvr, kagentNamespace, managedByAgentlab)
	if err != nil {
		return nil // no namespace: nothing lab-owned to prune
	}
	keep := map[string]bool{}
	for _, m := range want {
		keep[m.Name] = true
	}
	for _, mc := range existing {
		name := mc.GetName()
		if keep[name] {
			continue
		}
		note("pruning model config %s (removed from %s or gone from its host server)", name, config.File)
		if err := deleteObject(ctx, gvr, kagentNamespace, name, modelDeleteWait); err != nil {
			return err
		}
		// Its key Secret rides along; a missing one is fine — keyless
		// providers never had one.
		if err := deleteObject(ctx, gvrSecrets, kagentNamespace, "kagent-"+name, modelDeleteWait); err != nil {
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
// A read that fails is reported as such, with the apiserver's words, never as
// an empty status.
func waitModelConfigAccepted(name string) error {
	var status string
	var readErr error
	accepted := waitFor(10, modelConfigAcceptedPoll, func() bool {
		status, readErr = modelConfigCondition(name, "Accepted")
		return readErr == nil && status == "True"
	})
	if !accepted {
		return notReached("ModelConfig "+name, "Accepted", status, readErr,
			fmt.Sprintf("check `kubectl -n %s describe %s %s`", kagentNamespace, modelConfigResource, name))
	}
	note("ModelConfig %s: Accepted", name)
	return nil
}

// modelConfigCondition reads one condition's status off a ModelConfig ("" while
// the controller has not written it).
func modelConfigCondition(name, condType string) (string, error) {
	gvr, err := gvrFor(modelConfigResource)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	obj, err := getObject(ctx, gvr, kagentNamespace, name)
	if err != nil {
		return "", err
	}
	return conditionStatus(obj, condType), nil
}
