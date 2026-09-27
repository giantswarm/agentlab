package lab

import "testing"

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
