package lab

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestExtraModelsTemplate renders the extra-models template across every
// provider shape and asserts the provider-specific spec blocks: the OpenAI
// baseUrl section, the Ollama host mapping, the secret reference wiring, and
// the tls escape hatch.
func TestExtraModelsTemplate(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.ExtraModels = []config.ExtraModel{
		{Name: "qwen3-8-27b", Provider: "OpenAI", Model: "qwen3-8-27b",
			BaseURL: "https://qwen.example.com/v1", InsecureTLS: true},
		{Name: "openrouter-deepseek", Provider: "OpenAI", Model: "deepseek/deepseek-chat",
			BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"}, // #nosec G101 -- env var NAME, not a credential
		{Name: "gemini-flash", Provider: "Gemini", Model: "gemini-2.5-flash", APIKeyEnv: "GEMINI_API_KEY"}, // #nosec G101 -- env var NAME, not a credential
		{Name: "local-llama", Provider: "Ollama", Model: "llama3.3", BaseURL: "http://192.168.1.10:11434"},
		{Name: "claude-proxy", Provider: "Anthropic", Model: "claude-haiku-4-5", BaseURL: "https://proxy.example.com"},
	}
	raw, err := renderTemplate(cfg, "extra-models.yaml.tmpl", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)

	for _, want := range []string{
		// the self-hosted vLLM: baseUrl under openAI, placeholder-backed secret, tls off
		"name: qwen3-8-27b",
		"apiKeySecret: kagent-qwen3-8-27b",
		"apiKeySecretKey: OPENAI_API_KEY",
		"baseUrl: https://qwen.example.com/v1",
		"disableVerify: true",
		// gemini: GOOGLE_API_KEY (the ADK's canonical name), no endpoint block
		"apiKeySecretKey: GOOGLE_API_KEY",
		// ollama: host, not baseUrl
		"host: http://192.168.1.10:11434",
		// anthropic override endpoint
		"apiKeySecretKey: ANTHROPIC_API_KEY",
		"baseUrl: https://proxy.example.com",
		"app.kubernetes.io/managed-by: agentlab",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered extra-models missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "apiKeySecret: kagent-local-llama") {
		t.Errorf("Ollama must not reference a secret:\n%s", out)
	}
	if strings.Count(out, "kind: ModelConfig") != len(cfg.Platform.ExtraModels) {
		t.Errorf("want %d ModelConfigs:\n%s", len(cfg.Platform.ExtraModels), out)
	}

	// No extras -> comments only, nothing to apply (the lifecycle skips the
	// apply on an empty list).
	cfg.Platform.ExtraModels = nil
	raw, err = renderTemplate(cfg, "extra-models.yaml.tmpl", nil)
	if err != nil {
		t.Fatalf("render empty: %v", err)
	}
	if strings.Contains(string(raw), "kind:") {
		t.Errorf("empty extraModels rendered objects:\n%s", raw)
	}
}

// The extra ModelConfigs render at the kagent.dev version the chart line
// serves: v1alpha3 on the kagent line (meta chart >= 4.0), v1alpha2 on a
// released 3.x chart (kagent 0.10) — the ModelConfig spec is the same in
// both, so nothing but the apiVersion moves between the two renders.
func TestExtraModelsAPIVersionFollowsChartLine(t *testing.T) {
	render := func(cfg *config.Config) map[string]any {
		out, err := renderTemplate(cfg, extraModelsTemplate, nil)
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
		if len(docs) != 1 {
			t.Fatalf("rendered %d ModelConfigs, want 1:\n%s", len(docs), out)
		}
		return docs[0]
	}
	model := config.ExtraModel{Name: "local-vllm", Provider: config.ProviderOpenAI, Model: "mistral-small-3.2", BaseURL: "https://vllm.example.internal/v1"}
	current := config.Default()
	current.Platform.ChartVersion = "4.7.15"
	current.Platform.ExtraModels = []config.ExtraModel{model}
	legacy := config.Default()
	legacy.Platform.ChartVersion = "3.24.0"
	legacy.Platform.ExtraModels = []config.ExtraModel{model}
	if !legacy.LegacyChart() || current.LegacyChart() {
		t.Fatalf("LegacyChart(): 3.24.0=%v 4.7.15=%v", legacy.LegacyChart(), current.LegacyChart())
	}
	currentMC, legacyMC := render(current), render(legacy)

	if got := currentMC["apiVersion"]; got != "kagent.dev/v1alpha3" {
		t.Errorf("4.7.15: apiVersion = %v, want kagent.dev/v1alpha3", got)
	}
	if got := legacyMC["apiVersion"]; got != "kagent.dev/v1alpha2" {
		t.Errorf("3.24.0: apiVersion = %v, want kagent.dev/v1alpha2", got)
	}
	if got := currentMC["kind"]; got != "ModelConfig" {
		t.Errorf("kind = %v, want ModelConfig", got)
	}
	metadata := currentMC["metadata"].(map[string]any)
	if metadata["name"] != model.Name || metadata["namespace"] != kagentNamespace {
		t.Errorf("metadata = %v, want %s/%s", metadata, kagentNamespace, model.Name)
	}
	spec := currentMC["spec"].(map[string]any)
	if spec["provider"] != model.Provider || spec["model"] != model.Model || spec["openAI"].(map[string]any)["baseUrl"] != model.BaseURL {
		t.Errorf("spec = %v, want provider %s, model %s, openAI.baseUrl %s", spec, model.Provider, model.Model, model.BaseURL)
	}

	// Nothing but the apiVersion differs between the two lines.
	delete(currentMC, "apiVersion")
	delete(legacyMC, "apiVersion")
	if !reflect.DeepEqual(currentMC, legacyMC) {
		t.Errorf("the 3.x ModelConfig differs from the 4.x one beyond the apiVersion:\n--- 4.x\n%s\n--- 3.x\n%s", mustYAML(t, currentMC), mustYAML(t, legacyMC))
	}
}

// testKeyEnv is the env var NAME a test model entry reads its key from.
const testKeyEnv = "AGENTLAB_TEST_KEY" // #nosec G101 -- env var NAME, not a credential

// labelledModelConfig is a lab-owned ModelConfig seed (the label pruning
// selects by).
func labelledModelConfig(name string) *unstructured.Unstructured {
	return customObject(gvkModelConfig, kagentNamespace, name, map[string]string{managedByLabel: managedByAgentlabValue})
}

// TestWaitModelConfigAccepted: the poll ends on Accepted=True; an unaccepted
// ModelConfig reports the last status it read; a read that fails reports the
// failure with the apiserver's words — a NotFound stays one — instead of an
// empty status.
func TestWaitModelConfigAccepted(t *testing.T) {
	prev := modelConfigAcceptedPoll
	modelConfigAcceptedPoll = time.Millisecond
	t.Cleanup(func() { modelConfigAcceptedPoll = prev })
	unresolved := withCondition(labelledModelConfig("unresolved"), "Accepted", "True", "")
	conds, _, _ := unstructured.NestedSlice(unresolved.Object, fieldStatus, "conditions")
	_ = unstructured.SetNestedSlice(unresolved.Object, append(conds, map[string]any{fieldType: conditionResolvedRefs, fieldStatus: condFalse, fieldMessage: "secret placeholder not found"}), fieldStatus, "conditions")
	newFakeLab(t,
		withCondition(labelledModelConfig("ok"), "Accepted", "True", ""),
		withCondition(labelledModelConfig("nope"), "Accepted", condFalse, "no such provider"),
		unresolved,
	)
	if err := waitModelConfigAccepted("ok"); err != nil {
		t.Errorf("accepted without a ResolvedRefs condition (the 0.x line): %v", err)
	}
	if err := waitModelConfigAccepted("unresolved"); err == nil || !strings.Contains(err.Error(), "ResolvedRefs=False: secret placeholder not found") {
		t.Errorf("accepted but unresolved: %v", err)
	}
	err := waitModelConfigAccepted("nope")
	if err == nil || !strings.Contains(err.Error(), `ModelConfig nope never reached Accepted (last status: "False")`) {
		t.Errorf("unaccepted: %v", err)
	}
	err = waitModelConfigAccepted("absent")
	if err == nil || !apierrors.IsNotFound(err) || !strings.Contains(err.Error(), "modelconfigs kagent/absent") {
		t.Errorf("a failed read must surface the apiserver's words, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "last status") {
		t.Errorf("a failed read must not pose as a status: %v", err)
	}
}

// TestPruneExtraModels: lab-labelled ModelConfigs not among the wanted
// entries go, with their key Secrets; the wanted ones and the chart's own
// unlabelled default stay.
func TestPruneExtraModels(t *testing.T) {
	f := newFakeLab(t,
		labelledModelConfig("keep"), labelledModelConfig("drop"),
		customObject(gvkModelConfig, kagentNamespace, "default-model-config", nil),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kagent-keep", Namespace: kagentNamespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kagent-drop", Namespace: kagentNamespace}},
	)
	if err := pruneExtraModels([]config.ExtraModel{{Name: "keep"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dyn.Tracker().Get(gvrModelConfigs, kagentNamespace, "drop"); !apierrors.IsNotFound(err) {
		t.Errorf("drop still there: %v", err)
	}
	if _, err := f.dyn.Tracker().Get(gvrSecrets, kagentNamespace, "kagent-drop"); !apierrors.IsNotFound(err) {
		t.Errorf("kagent-drop still there: %v", err)
	}
	for _, kept := range []string{"keep", "default-model-config"} {
		f.stored(t, gvrModelConfigs, kagentNamespace, kept)
	}
	f.stored(t, gvrSecrets, kagentNamespace, "kagent-keep")
}

// TestEnsureModelKeySecret: an existing key Secret is left alone; a missing
// one is applied with the env var's value under the provider's canonical
// key, or with the placeholder when the entry names no variable.
func TestEnsureModelKeySecret(t *testing.T) {
	f := newFakeLab(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kagent-existing", Namespace: kagentNamespace},
		Data: map[string][]byte{"OPENAI_API_KEY": []byte("old")}})
	if err := ensureModelKeySecret(config.ExtraModel{Name: "existing", Provider: config.ProviderOpenAI, APIKeyEnv: testKeyEnv}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := unstructured.NestedString(f.stored(t, gvrSecrets, kagentNamespace, "kagent-existing").Object, "data", "OPENAI_API_KEY"); got != "b2xk" {
		t.Errorf("the existing Secret was rewritten: data %q", got)
	}
	t.Setenv(testKeyEnv, "sk-test")
	if err := ensureModelKeySecret(config.ExtraModel{Name: "newkey", Provider: config.ProviderOpenAI, APIKeyEnv: testKeyEnv}); err != nil {
		t.Fatal(err)
	}
	fresh := f.stored(t, gvrSecrets, kagentNamespace, "kagent-newkey")
	if got, _, _ := unstructured.NestedString(fresh.Object, "data", "OPENAI_API_KEY"); got != "c2stdGVzdA==" {
		t.Errorf("kagent-newkey data = %q, want the env var's value", got)
	}
	if err := ensureModelKeySecret(config.ExtraModel{Name: "keyless", Provider: config.ProviderOpenAI}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := unstructured.NestedString(f.stored(t, gvrSecrets, kagentNamespace, "kagent-keyless").Object, "data", "OPENAI_API_KEY"); got != "YWdlbnRsYWItcGxhY2Vob2xkZXI=" {
		t.Errorf("kagent-keyless data = %q, want the placeholder", got)
	}
	if err := ensureModelKeySecret(config.ExtraModel{Name: "ollama", Provider: config.ProviderOllama}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dyn.Tracker().Get(gvrSecrets, kagentNamespace, "kagent-ollama"); !apierrors.IsNotFound(err) {
		t.Errorf("a keyless provider must get no Secret: %v", err)
	}
}
