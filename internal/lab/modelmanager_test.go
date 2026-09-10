package lab

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Each backend's probe fetches its own path.
func TestBackendProbePaths(t *testing.T) {
	cases := map[string]string{
		ollama:   "/api/version",
		lemonade: "/api/v1/health",
		lmstudio: "/api/v1/models",
	}
	for backend, want := range cases {
		spec, known := backendSpec(backend)
		if !known {
			t.Errorf("backend %q has no table entry", backend)
			continue
		}
		if spec.probe.path != want {
			t.Errorf("%s probe path = %q, want %q", backend, spec.probe.path, want)
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

// hostInventory reads the host server from THIS machine, so a pod-facing
// The endpoint the platform dials can be an address only the cluster has
// (Docker Desktop's host.docker.internal, podman's host.containers.internal).
// Reading the host's own inventory then has to fall back to loopback — where
// `agentlab configure` found the server to begin with — and the fallback must
// key on the dial's OWN answer that the name does not exist, so no live
// resolver is involved and a hijacking one cannot change the outcome.
func TestHostInventoryFallsBackToLoopback(t *testing.T) {
	srv := fakeLMStudio(t)
	defer srv.Close()
	// The fake's library holds three agent models (its embedding entry is
	// filtered out by the reader).
	const agentModels = 3
	restore := hostModelsFn
	defer func() { hostModelsFn = restore }()

	// The endpoint answers: taken as is, and reported as the base read.
	got, base, err := hostInventory(lmstudio, srv.URL)
	if err != nil || len(got) != agentModels {
		t.Fatalf("direct read: %d models, err=%v", len(got), err)
	}
	if base != srv.URL {
		t.Errorf("direct read must report the endpoint it read: %s", base)
	}

	// The endpoint's host does not exist for this machine — the shape of
	// error a dial gives for that — and loopback answers: fall back, and
	// report loopback as the base.
	// The cluster-only name on the fake's port, so the host-only rewrite
	// (which keeps the endpoint's port) lands on the fake.
	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	clusterOnly := "http://" + hostDockerInternal + ":" + srvURL.Port()
	loopbackRestore := loopbackBaseFn
	loopbackBaseFn = func(string) string { return "http://127.0.0.1:1234" }
	defer func() { loopbackBaseFn = loopbackRestore }()
	hostModelsFn = func(backend, base string) ([]HostModel, error) {
		if base == clusterOnly {
			return nil, &net.OpError{Op: opDial, Err: &net.DNSError{Err: "no such host", Name: hostDockerInternal, IsNotFound: true}}
		}
		return restore(backend, base)
	}
	got, base, err = hostInventory(lmstudio, clusterOnly)
	if err != nil || len(got) != agentModels {
		t.Fatalf("fallback read: %d models, err=%v", len(got), err)
	}
	if want := "http://127.0.0.1:" + srvURL.Port(); base != want {
		t.Errorf("the fallback must report the base it actually read (%s), not the endpoint: %s", want, base)
	}

	// Neither answers: the endpoint's own error survives, with the loopback's.
	loopbackBaseFn = func(string) string { return "http://127.0.0.1:1" }
	deadEndpoint := "http://" + hostDockerInternal + ":1"
	hostModelsFn = func(backend, base string) ([]HostModel, error) {
		if base == deadEndpoint {
			return nil, &net.OpError{Op: opDial, Err: &net.DNSError{Err: "no such host", Name: hostDockerInternal, IsNotFound: true}}
		}
		return restore(backend, base)
	}
	_, _, err = hostInventory(lmstudio, deadEndpoint)
	if err == nil {
		t.Fatal("both unreachable must fail")
	}
	for _, want := range []string{hostDockerInternal, "127.0.0.1:1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s: %v", want, err)
		}
	}
}

// A read that fails for any other reason must NOT fall back: a configured LAN
// endpoint that is merely down would otherwise be silently replaced by a
// local server of the same kind, whose different library makes the delete
// assertions report on a machine they never touched.
func TestHostInventoryFallsBackOnlyForAnUnresolvableHost(t *testing.T) {
	srv := fakeLMStudio(t)
	defer srv.Close()
	loopbackRestore := loopbackBaseFn
	loopbackBaseFn = func(string) string { return srv.URL }
	defer func() { loopbackBaseFn = loopbackRestore }()
	restore := hostModelsFn
	defer func() { hostModelsFn = restore }()

	const lan = "http://192.0.2.10:11434"
	hostModelsFn = func(backend, base string) ([]HostModel, error) {
		if base == lan {
			return nil, &net.OpError{Op: opDial, Err: errors.New("connection refused")}
		}
		return restore(backend, base)
	}
	got, base, err := hostInventory(lmstudio, lan)
	if err == nil {
		t.Fatalf("a resolvable endpoint that does not answer must fail, got %d models", len(got))
	}
	if base != lan {
		t.Errorf("the failure must name the endpoint that was asked for: %s", base)
	}
}

// The fallback replaces the HOST, never the port: an endpoint on a
// non-default port must fall back to the same port on loopback, or the read
// lands on whatever else listens on the backend's default port.
func TestLoopbackForKeepsTheEndpointsPort(t *testing.T) {
	for _, tc := range []struct{ endpoint, want string }{
		{"http://" + hostDockerInternal + ":5678", "http://127.0.0.1:5678"},
		{"http://" + hostDockerInternal, "http://127.0.0.1:1234"},
	} {
		if got := loopbackFor(lmstudio, tc.endpoint); got != tc.want {
			t.Errorf("loopbackFor(%q) = %q, want %q", tc.endpoint, got, tc.want)
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
