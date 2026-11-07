package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

func TestTraceSinkSwitch(t *testing.T) {
	for _, on := range []bool{false, true} {
		cfg := config.Default()
		cfg.Platform.Traces = on
		cfg.Platform.Serving.Enabled = true
		// Tracing is independent of the Prometheus choice.
		cfg.Platform.Observability = false
		values, err := renderTemplate(cfg, platformValuesTemplate, nil)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := yaml.Unmarshal(values, &v); err != nil {
			t.Fatal(err)
		}
		endpoint := v["global"].(map[string]any)["observability"].(map[string]any)["traces"].(map[string]any)["otlp"].(map[string]any)["endpoint"]
		want := ""
		if on {
			want = "http://agentlab-traces.agentlab-traces.svc:4317"
		}
		if endpoint != want {
			t.Fatalf("traces=%v endpoint=%v, want %q", on, endpoint, want)
		}
		if got := strings.Contains(string(values), "--otlp-traces-endpoint="); got != on {
			t.Fatalf("serving exporter=%v, traces=%v", got, on)
		}
		for _, name := range []string{"traces.yaml.tmpl", "traces-route.yaml.tmpl"} {
			raw, err := renderTemplate(cfg, name, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !on && strings.TrimSpace(string(raw)) != "" {
				t.Fatalf("traces off renders %s: %s", name, raw)
			}
			if _, err := decodeManifests(raw); err != nil {
				t.Fatalf("invalid manifest %s: %v", name, err)
			}
			if on && !strings.Contains(string(raw), "agentlab-traces") {
				t.Fatalf("traces on renders no sink in %s", name)
			}
		}
	}
}

func TestServingTraceRequiresLinkedGatewayAndServingSpans(t *testing.T) {
	batch := func(service, id, parent string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":%q}}]},"scopeSpans":[{"spans":[{"spanId":%q,"parentSpanId":%q}]}]}`, service, id, parent))
	}
	for _, tc := range []struct {
		name    string
		batches []any
		pass    bool
	}{
		{"linked", []any{batch("agentgateway", "gateway", "client"), batch("vllm", "serving", "gateway")}, true},
		{"gateway only", []any{batch("agentgateway", "gateway", "client")}, false},
		{"serving only", []any{batch("vllm", "serving", "gateway")}, false},
		{"unrelated spans", []any{batch("agentgateway", "gateway", "client"), batch("vllm", "serving", "other")}, false},
		{"unrelated service", []any{batch("agentgateway", "gateway", "client"), batch("fake-vllm", "serving", "gateway")}, false},
		{"no spans", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"trace": map[string]any{"resourceSpans": tc.batches}})
			if err != nil {
				t.Fatal(err)
			}
			err = servingTraceComplete(raw)
			if (err == nil) != tc.pass {
				t.Fatalf("pass=%v, error=%v", tc.pass, err)
			}
		})
	}
	if err := servingTraceComplete([]byte("not JSON")); err == nil {
		t.Fatal("invalid JSON passed")
	}
}

func TestTraceSinkDisableRemovesStoreAndGatewayPolicy(t *testing.T) {
	// Seed the fake API with the resources a prior on run installed.
	policy := &unstructured.Unstructured{Object: map[string]any{
		fieldAPIVersion: "agentgateway.dev/v1alpha1", fieldKind: "AgentgatewayPolicy",
		fieldMetadata: map[string]any{nameKey: "agentlab-models-tracing", fieldNamespace: platformNamespace},
	}}
	f := newFakeLab(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tracesNamespace}}, policy)
	f.mapper.Add(policy.GroupVersionKind(), meta.RESTScopeNamespace)
	cfg := config.Default()
	if err := reconcileTracesAfterPlatform(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	policyGVR, err := gvrFor("agentgatewaypolicies.agentgateway.dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.dyn.Tracker().Get(policyGVR, platformNamespace, "agentlab-models-tracing"); err == nil {
		t.Fatal("models Gateway tracing policy survived disable")
	}
	if _, err := f.dyn.Tracker().Get(gvrNamespaces, "", tracesNamespace); err == nil {
		t.Fatal("trace namespace survived disable")
	}
}
