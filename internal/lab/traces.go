package lab

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agentlab/internal/config"
)

const tracesNamespace = "agentlab-traces"
const tracesName = "agentlab-traces"

// tracesUp starts the receiver before any exporter is configured. Removal is
// after the platform reconcile so disabling tracing stops exporters first.
func tracesUp(cfg *config.Config) error {
	step("Installing the optional trace sink (Tempo + Grafana, 768 MiB memory limit)")
	manifest, _, err := renderManifest(cfg, "traces.yaml.tmpl")
	if err != nil {
		return err
	}
	imgs := scrapeImages(string(manifest))
	sideloadImages(cfg, hostPullImages(imgs))
	if _, err := applyManifests(context.Background(), manifest); err != nil {
		return err
	}
	return waitDeploymentRolledOut(context.Background(), tracesNamespace, tracesName, 5*time.Minute)
}

func reconcileTracesAfterPlatform(ctx context.Context, cfg *config.Config) error {
	if !cfg.TracesEnabled() {
		if err := removeModelsTracing(ctx); err != nil {
			return err
		}
		return deleteNamespace(ctx, tracesNamespace)
	}
	manifest, _, err := renderManifest(cfg, "traces-route.yaml.tmpl")
	if err != nil {
		return err
	}
	if _, err := applyManifests(ctx, manifest); err != nil {
		return err
	}
	if !cfg.ServingEnabled() {
		// A prior run may have served models with tracing enabled.
		if err := removeModelsTracing(ctx); err != nil {
			return err
		}
	}
	note("Trace UI: %s (agentlab open traces); Grafana Explore → Lab traces", cfg.TracesBaseURL())
	return nil
}

// newServingTraceID gives this completion its own sampled W3C context, so the
// proof cannot pass on a previous request's spans or probabilistic sampling.
func newServingTraceID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// servingTraceComplete requires both real services in the same trace and a
// parent/child link from the serving span back to the models Gateway's span.
// Tempo's JSON API uses OTLP-JSON's base64 byte fields for span identifiers.
func servingTraceComplete(raw []byte) error {
	var response struct {
		Trace struct {
			ResourceSpans []struct {
				Resource struct {
					Attributes []struct {
						Key   string `json:"key"`
						Value struct {
							StringValue string `json:"stringValue"`
						} `json:"value"`
					} `json:"attributes"`
				} `json:"resource"`
				ScopeSpans []struct {
					Spans []struct {
						SpanID       string `json:"spanId"`
						ParentSpanID string `json:"parentSpanId"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		} `json:"trace"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return err
	}
	gateway := map[string]bool{}
	var servingParents []string
	for _, batch := range response.Trace.ResourceSpans {
		service := ""
		for _, a := range batch.Resource.Attributes {
			if a.Key == "service.name" {
				service = a.Value.StringValue
			}
		}
		for _, scope := range batch.ScopeSpans {
			for _, span := range scope.Spans {
				if service == "agentgateway" {
					gateway[span.SpanID] = true
				}
				if service == "vllm" {
					servingParents = append(servingParents, span.ParentSpanID)
				}
			}
		}
	}
	for _, parent := range servingParents {
		if parent != "" && gateway[parent] {
			return nil
		}
	}
	return fmt.Errorf("trace has %d gateway spans and %d vLLM spans, but no serving span parent is a gateway span", len(gateway), len(servingParents))
}

func proveServingTrace(ctx context.Context, cfg *config.Config, traceID string) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	pods, err := k.clientset.CoreV1().Pods(tracesNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + tracesName})
	if err != nil {
		return err
	}
	if len(pods.Items) != 1 {
		return fmt.Errorf("expected one trace sink pod, found %d", len(pods.Items))
	}
	port, stop, err := portForwardPod(ctx, tracesNamespace, pods.Items[0].Name, 3000)
	if err != nil {
		return err
	}
	defer stop()
	client := &http.Client{Timeout: 10 * time.Second}
	var lastErr error
	if !waitFor(30, 2*time.Second, func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/datasources/proxy/uid/lab-traces/api/v2/traces/%s", port, traceID), nil)
		if err != nil {
			lastErr = err
			return false
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
		if err != nil {
			lastErr = err
			return false
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("grafana trace datasource answered %d", resp.StatusCode)
			return false
		}
		lastErr = servingTraceComplete(raw)
		return lastErr == nil
	}) {
		return fmt.Errorf("serving trace %s did not reach the local store: %w", traceID, lastErr)
	}
	note("PASS: gateway → vLLM spans in trace %s; open %s and search that trace ID in Explore → Lab traces", traceID, cfg.TracesBaseURL())
	return nil
}

// removeModelsTracing removes the extra Gateway policy outside the sink's namespace.
func removeModelsTracing(ctx context.Context) error {
	// Direct resources keep the off path harmless on a chart with no policy
	// CRD: the API's NotFound is success, not a discovery error.
	policy := schema.GroupVersionResource{Group: "agentgateway.dev", Version: "v1alpha1", Resource: "agentgatewaypolicies"}
	if err := deleteObject(ctx, policy, platformNamespace, "agentlab-models-tracing", 0); err != nil {
		return err
	}
	return nil
}
