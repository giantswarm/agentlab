package lab

import "testing"

// The preflight picks the version field out of kubectl's combined output for
// both servers' documents: Ollama's {"version":...} and Lemonade's health
// document, where version is one field among many.
func TestVersionFieldRe(t *testing.T) {
	cases := map[string]string{
		"{\"version\":\"0.33.2\"}warning: couldn't attach to pod/ollama-preflight, falling back to streaming logs":      `"version":"0.33.2"`,
		`{"all_models_loaded":[],"status":"ok","telemetry":{"enabled":false},"version":"11.9.0","websocket_port":9000}`: `"version":"11.9.0"`,
	}
	for in, want := range cases {
		if got := versionFieldRe.FindString(in); got != want {
			t.Errorf("FindString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHealthPath(t *testing.T) {
	if healthPath("ollama") != "/api/version" || healthPath("lemonade") != "/api/v1/health" {
		t.Fatalf("health paths: ollama=%s lemonade=%s", healthPath("ollama"), healthPath("lemonade"))
	}
}
