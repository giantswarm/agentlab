package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The platform's LLM endpoint document: a ConfigMap in model-manager's
// namespace the platform release renders with llmRouting on. model-manager
// reads it to put a served model on the endpoint.
const (
	llmEndpointLabel = "agent-platform.giantswarm.io/llm-endpoint=true"
	llmEndpointKey   = "llm-endpoint.yaml"
)

// The data plane's per-model token metric, labelled with the model the
// request reached (a virtual model's target: the served model's id).
const (
	llmUsageMetric     = "agentgateway_gen_ai_client_token_usage"
	llmUsageModelLabel = "gen_ai_request_model"
)

// llmEndpointDoc is the part of the document the proofs read.
type llmEndpointDoc struct {
	Spec struct {
		Endpoint         string `yaml:"endpoint"`
		ExternalEndpoint string `yaml:"externalEndpoint"`
	} `yaml:"spec"`
}

// reported is the endpoint list_loaded_models names for a model on it: the
// external one where the document has it, else the in-cluster listener.
func (d llmEndpointDoc) reported() string {
	if d.Spec.ExternalEndpoint != "" {
		return d.Spec.ExternalEndpoint
	}
	return d.Spec.Endpoint
}

// readLLMEndpoint reads the document; without one (llmRouting off) the
// result is empty.
func readLLMEndpoint(ctx context.Context, k *kubeClients) (llmEndpointDoc, error) {
	var doc llmEndpointDoc
	cms, err := k.clientset.CoreV1().ConfigMaps(platformNamespace).List(ctx, metav1.ListOptions{LabelSelector: llmEndpointLabel})
	if err != nil {
		return doc, fmt.Errorf("listing the LLM endpoint document: %w", err)
	}
	switch len(cms.Items) {
	case 0:
		return doc, nil
	case 1:
	default:
		return doc, fmt.Errorf("%d ConfigMaps in %s carry %s; model-manager reads exactly one", len(cms.Items), platformNamespace, llmEndpointLabel)
	}
	cm := cms.Items[0]
	if err := yaml.Unmarshal([]byte(cm.Data[llmEndpointKey]), &doc); err != nil {
		return doc, fmt.Errorf("the LLM endpoint document %s: %w", cm.Name, err)
	}
	if doc.Spec.Endpoint == "" {
		return doc, fmt.Errorf("the LLM endpoint document %s names no spec.endpoint", cm.Name)
	}
	return doc, nil
}

// llmUsageTokens is the tokens the data plane counted for the model so far,
// through the lab's PromQL on the edge; a model never requested counts 0.
func llmUsageTokens(cfg *config.Config, model string) (float64, error) {
	client, err := labHTTPClient(10 * time.Second)
	if err != nil {
		return 0, err
	}
	query := fmt.Sprintf(`sum(%s_sum{%s=%q})`, llmUsageMetric, llmUsageModelLabel, model)
	resp, err := client.Get(cfg.ObservabilityBaseURL() + "/api/v1/query?query=" + url.QueryEscape(query))
	if err != nil {
		return 0, fmt.Errorf("querying %s: %w", llmUsageMetric, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("querying %s: HTTP %d: %.200s", llmUsageMetric, resp.StatusCode, raw)
	}
	return parseInstantScalar(raw)
}

// parseInstantScalar reads the one sample of an instant-vector answer; an
// empty vector is 0.
func parseInstantScalar(raw []byte) (float64, error) {
	var answer struct {
		Data struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return 0, fmt.Errorf("decoding the PromQL answer: %w", err)
	}
	if len(answer.Data.Result) == 0 {
		return 0, nil
	}
	v := answer.Data.Result[0].Value
	if len(v) != 2 {
		return 0, fmt.Errorf("the PromQL sample has %d fields, wanted 2", len(v))
	}
	s, ok := v[1].(string)
	if !ok {
		return 0, fmt.Errorf("the PromQL sample value is %T, wanted a string", v[1])
	}
	return strconv.ParseFloat(s, 64)
}
