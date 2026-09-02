package lab

import "testing"

func TestOllamaVersionRe(t *testing.T) {
	out := "{\"version\":\"0.33.2\"}warning: couldn't attach to pod/ollama-preflight, falling back to streaming logs"
	if got := ollamaVersionRe.FindString(out); got != `{"version":"0.33.2"}` {
		t.Fatalf("got %q", got)
	}
}
