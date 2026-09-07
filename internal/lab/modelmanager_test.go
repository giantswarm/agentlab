package lab

import (
	"strings"
	"testing"
)

// The preflight picks the version field out of the probe pod's output for
// both servers' documents: Ollama's {"version":...} alone and Lemonade's
// health document, where version is one field among many.
func TestVersionFieldRe(t *testing.T) {
	cases := map[string]string{
		`{"version":"0.33.2"}`: `"version":"0.33.2"`,
		`{"all_models_loaded":[],"status":"ok","telemetry":{"enabled":false},"version":"11.9.0","websocket_port":9000}`: `"version":"11.9.0"`,
	}
	for in, want := range cases {
		if got := versionFieldRe.FindString(in); got != want {
			t.Errorf("FindString(%q) = %q, want %q", in, got, want)
		}
	}
}

// Each backend's probe fetches its own path, and the marker the preflight
// greps for is one the server's answer really carries.
func TestBackendProbePaths(t *testing.T) {
	cases := map[string]struct{ path, marker string }{
		"ollama":   {"/api/version", `"version"`},
		"lemonade": {"/api/v1/health", `"version"`},
		"lmstudio": {"/api/v1/models", `"models"`},
	}
	for backend, want := range cases {
		spec, known := backendSpec(backend)
		if !known {
			t.Errorf("backend %q has no table entry", backend)
			continue
		}
		if spec.probe.path != want.path || spec.probe.marker != want.marker {
			t.Errorf("%s probe = %q/%q, want %q/%q", backend, spec.probe.path, spec.probe.marker, want.path, want.marker)
		}
	}
}

// LM Studio's identity is the shape of its inventory, because it reports no
// version and answers 200 for paths it does not serve.
func TestLMStudioIdent(t *testing.T) {
	hits := []string{
		`{"models":[{"key":"ibm/granite-4-micro","type":"llm"}]}`,
		// A fresh install: no models yet, still an LM Studio.
		`{"models":[]}`,
	}
	for _, body := range hits {
		if got, ok := lmStudioIdent([]byte(body)); !ok || got != lmStudioIdentAPIv1 {
			t.Errorf("lmStudioIdent(%s) = %q,%v — want a hit", body, got, ok)
		}
	}
	misses := []string{
		// What LM Studio itself answers for a path it does not serve.
		`{"error":"Unexpected endpoint or method. (GET /api/version)"}`,
		// Lemonade on the very same path.
		`{"object":"list","data":[{"id":"qwen3-4b-FLM"}]}`,
		// Ollama's version document.
		`{"version":"0.33.2"}`,
		// A `models` array of something else entirely.
		`{"models":[{"name":"not-lmstudio"}]}`,
		`not json`,
	}
	for _, body := range misses {
		if got, ok := lmStudioIdent([]byte(body)); ok {
			t.Errorf("lmStudioIdent(%s) = %q — want a miss", body, got)
		}
	}
}

// The preflight picks LM Studio's identifying field out of kubectl's combined
// output, as it does the version field for the other two.
func TestLMStudioKeyFieldRe(t *testing.T) {
	in := `{"models":[{"type":"llm","key":"ibm/granite-4-micro"}]}warning: couldn't attach to pod/lmstudio-preflight, falling back to streaming logs`
	if got, want := lmStudioKeyFieldRe.FindString(in), `"key":"ibm/granite-4-micro"`; got != want {
		t.Errorf("FindString = %q, want %q", got, want)
	}
}

// hostInventory reads the host server from THIS machine, so a pod-facing
// endpoint the host cannot resolve (Docker Desktop's host.docker.internal,
// podman's host.containers.internal) must fall back to the server's loopback
// port — which is where `agentlab configure` found it to begin with.
func TestHostInventoryFallsBackToLoopback(t *testing.T) {
	srv := fakeLMStudio(t)
	defer srv.Close()
	const unreachable = "http://host.docker.internal.invalid:1234"
	dead := "http://127.0.0.1:1"

	// The endpoint answers: taken as is.
	got, err := hostInventory(lmstudio, srv.URL)
	if err != nil || len(got) != 2 {
		t.Fatalf("direct read: %d models, err=%v", len(got), err)
	}

	// The endpoint does not resolve, the loopback port does: fall back.
	restore := loopbackBaseFn
	loopbackBaseFn = func(string) string { return srv.URL }
	defer func() { loopbackBaseFn = restore }()
	got, err = hostInventory(lmstudio, unreachable)
	if err != nil || len(got) != 2 {
		t.Fatalf("fallback read: %d models, err=%v", len(got), err)
	}

	// Neither answers: the endpoint's own error survives, with the loopback's.
	loopbackBaseFn = func(string) string { return dead }
	_, err = hostInventory(lmstudio, unreachable)
	if err == nil {
		t.Fatal("both unreachable must fail")
	}
	for _, want := range []string{"host.docker.internal.invalid", "127.0.0.1:1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s: %v", want, err)
		}
	}
}
