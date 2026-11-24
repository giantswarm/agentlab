package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

func TestLLMEndpointReported(t *testing.T) {
	var d llmEndpointDoc
	d.Spec.Endpoint = "http://agentgateway.agent-platform.svc:8081"
	if got := d.reported(); got != d.Spec.Endpoint {
		t.Errorf("reported() = %q, want the in-cluster endpoint", got)
	}
	d.Spec.ExternalEndpoint = "https://llm.example.com"
	if got := d.reported(); got != d.Spec.ExternalEndpoint {
		t.Errorf("reported() = %q, want the external endpoint", got)
	}
}

func TestParseInstantScalar(t *testing.T) {
	for name, tc := range map[string]struct {
		raw     string
		want    float64
		wantErr bool
	}{
		"empty vector": {raw: `{"status":"success","data":{"resultType":"vector","result":[]}}`, want: 0},
		"one sample":   {raw: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1790000000.1,"42"]}]}}`, want: 42},
		"not json":     {raw: `<html>`, wantErr: true},
		"bad value":    {raw: `{"data":{"result":[{"value":[1,42]}]}}`, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseInstantScalar([]byte(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLLMUsageMetering(t *testing.T) {
	const endpoint = "http://agentgateway.agent-platform.svc:8081"
	for name, tc := range map[string]struct {
		endpoint      string
		observability bool
		wantMetered   bool
		wantSkipped   bool
	}{
		"no LLM endpoint":                 {endpoint: "", observability: true},
		"no LLM endpoint, no PromQL":      {endpoint: ""},
		"LLM endpoint with PromQL":        {endpoint: endpoint, observability: true, wantMetered: true},
		"LLM endpoint, observability off": {endpoint: endpoint, wantSkipped: true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Platform.Observability = tc.observability
			metered, skipped := llmUsageMetering(cfg, tc.endpoint)
			if metered != tc.wantMetered {
				t.Errorf("metered = %v, want %v", metered, tc.wantMetered)
			}
			if (skipped != "") != tc.wantSkipped {
				t.Errorf("skipped = %q, want skipped=%v", skipped, tc.wantSkipped)
			}
			if tc.wantSkipped && !strings.Contains(skipped, "platform.observability is off in "+config.File) {
				t.Errorf("skipped = %q, want it to name the switch", skipped)
			}
		})
	}
}
