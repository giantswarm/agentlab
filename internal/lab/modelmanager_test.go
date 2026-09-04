package lab

import "testing"

func TestVersionFieldRe(t *testing.T) {
	cases := map[string]string{
		// Ollama's /api/version, followed by kubectl's attach chatter.
		"{\"version\":\"0.33.2\"}warning: couldn't attach to pod/model-server-preflight, falling back to streaming logs": `"version":"0.33.2"`,
		// Lemonade's /api/v1/health carries the field among many others.
		`{"all_models_loaded":[],"status":"ok","telemetry":{"enabled":false},"version":"11.9.0","websocket_port":9000}`: `"version":"11.9.0"`,
	}
	for in, want := range cases {
		if got := versionFieldRe.FindString(in); got != want {
			t.Fatalf("%q: got %q, want %q", in, got, want)
		}
	}
	if got := versionFieldRe.FindString("wget: can't connect to remote host: Connection refused"); got != "" {
		t.Fatalf("a failed probe matched %q", got)
	}
}
