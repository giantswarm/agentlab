package lab

import (
	"net/http"
	"net/http/httptest"
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
	// The fake's library holds three agent models (its embedding entry is
	// filtered out by the reader).
	const agentModels = 3
	got, base, err := hostInventory(lmstudio, srv.URL)
	if err != nil || len(got) != agentModels {
		t.Fatalf("direct read: %d models, err=%v", len(got), err)
	}
	if base != srv.URL {
		t.Errorf("direct read must report the endpoint it read: %s", base)
	}

	// The endpoint does not resolve, the loopback port does: fall back.
	restore := loopbackBaseFn
	loopbackBaseFn = func(string) string { return srv.URL }
	defer func() { loopbackBaseFn = restore }()
	got, base, err = hostInventory(lmstudio, unreachable)
	if err != nil || len(got) != agentModels {
		t.Fatalf("fallback read: %d models, err=%v", len(got), err)
	}
	if base != srv.URL {
		t.Errorf("the fallback must report the base it actually read, not the endpoint: %s", base)
	}

	// Neither answers: the endpoint's own error survives, with the loopback's.
	loopbackBaseFn = func(string) string { return dead }
	_, _, err = hostInventory(lmstudio, unreachable)
	if err == nil {
		t.Fatal("both unreachable must fail")
	}
	for _, want := range []string{"host.docker.internal.invalid", "127.0.0.1:1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s: %v", want, err)
		}
	}
}

// A configured endpoint that RESOLVES but does not answer must fail the read,
// never fall back: docs/models.md supports pointing a backend at a server
// elsewhere on the LAN, and substituting a local server of the same kind would
// have models-test assert against a library it never meant to read — reporting
// a model "gone from the host" because a different host does not have it.
func TestHostInventoryDoesNotFallBackForAResolvableEndpoint(t *testing.T) {
	srv := fakeLMStudio(t)
	defer srv.Close()
	restore := loopbackBaseFn
	loopbackBaseFn = func(string) string { return srv.URL }
	defer func() { loopbackBaseFn = restore }()

	// 127.0.0.1:1 resolves (an IP literal always does) and refuses the
	// connection. The loopback stand-in above would answer happily.
	const lanDown = "http://127.0.0.1:1"
	got, base, err := hostInventory(lmstudio, lanDown)
	if err == nil {
		t.Fatalf("a resolvable endpoint that does not answer must fail, got %d models", len(got))
	}
	if base != lanDown {
		t.Errorf("the failure must name the endpoint that was asked for: %s", base)
	}
}

// lmStudioModels must reject a 200 whose body is not an LM Studio answer
// rather than return an empty inventory with a nil error — the empty read is
// indistinguishable from "the model is gone from the host", which is what the
// delete assertions rest on.
func TestLMStudioModelsRejectsAForeign200(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"lmstudio's own unknown-path answer", `{"error":"Unexpected endpoint or method."}`},
		{"a lemonade envelope on the same path", `{"object":"list","data":[{"id":"m","downloaded":true}]}`},
		{"an empty document", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			models, err := lmStudioModels(srv.URL)
			if err == nil {
				t.Fatalf("a foreign 200 must be an error, got %d models", len(models))
			}
		})
	}
}

// An empty library is still an LM Studio: nothing downloaded is a valid
// answer, and the reader must not confuse it with a foreign server.
func TestLMStudioModelsAcceptsAnEmptyLibrary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	models, err := lmStudioModels(srv.URL)
	if err != nil {
		t.Fatalf("an empty library must read cleanly: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("want no models, got %d", len(models))
	}
}
