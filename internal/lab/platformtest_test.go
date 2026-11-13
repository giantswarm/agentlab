package lab

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestSubstrateShipped: the chart in place ships Agent Substrate when it
// renders the substrate HelmRelease in the platform namespace — a
// HelmRelease elsewhere or of another name is not it; the 3.x line ships
// none and is told apart without a read; a read the apiserver refuses is an
// error, never "no Substrate".
func TestSubstrateShipped(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	newFakeLab(t, customObject(fluxHelmReleaseGVK, platformNamespace, "muster", nil))
	if shipped, err := substrateShipped(ctx, cfg); err != nil || shipped {
		t.Errorf("without a substrate HelmRelease: %v, %v; want false", shipped, err)
	}
	newFakeLab(t, customObject(fluxHelmReleaseGVK, kagentNamespace, substrateRelease, nil))
	if shipped, err := substrateShipped(ctx, cfg); err != nil || shipped {
		t.Errorf("a substrate HelmRelease in another namespace: %v, %v; want false", shipped, err)
	}
	newFakeLab(t, customObject(fluxHelmReleaseGVK, platformNamespace, substrateRelease, nil))
	if shipped, err := substrateShipped(ctx, cfg); err != nil || !shipped {
		t.Errorf("with the substrate HelmRelease: %v, %v; want true", shipped, err)
	}

	legacy := config.Default()
	legacy.Platform.ChartVersion = legacyChartVersion
	f := newFakeLab(t, customObject(fluxHelmReleaseGVK, platformNamespace, substrateRelease, nil))
	reads := 0
	f.dyn.PrependReactor("list", "helmreleases", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		return true, nil, errors.New("the apiserver is down")
	})
	if shipped, err := substrateShipped(ctx, legacy); err != nil || shipped || reads != 0 {
		t.Errorf("the 3.x line: %v, %v after %d reads; want false without a read", shipped, err, reads)
	}
	if _, err := substrateShipped(ctx, cfg); err == nil || !strings.Contains(err.Error(), "the apiserver is down") {
		t.Errorf("a refused read: %v, want the apiserver's error", err)
	}
}

// The family tools are listed before downstream sign-in. A namespace call's
// challenge must sign in on that same session and retry only once; only a
// decoded namespace list makes the proof pass.
func TestPlatformNamespaces(t *testing.T) {
	const namespaces = `{"items":[{"name":"default"},{"name":"kube-system"}]}`
	challenge := callToolAnswer(t, toolResult("auth_required: sign in to Kubernetes", true, nil))
	success := callToolAnswer(t, toolResult(namespaces, false, nil))
	signInErr := errors.New("consent refused")
	for _, tc := range []struct {
		name        string
		first       mcpAnswer
		retry       mcpAnswer
		signInErr   error
		wantCalls   int
		wantSignIns int
		wantError   string
	}{
		{name: "listed but unauthenticated", first: challenge, retry: success, wantCalls: 2, wantSignIns: 1},
		{name: "already authenticated", first: success, wantCalls: 1},
		{name: "sign-in fails", first: challenge, signInErr: signInErr, wantCalls: 1, wantSignIns: 1, wantError: "consent refused"},
		{name: "retry still requires auth", first: challenge, retry: challenge, wantCalls: 2, wantSignIns: 1, wantError: "no namespace list"},
		{name: "retry tool error", first: challenge, retry: callToolAnswer(t, toolResult("permission denied", true, nil)), wantCalls: 2, wantSignIns: 1, wantError: "permission denied"},
		{name: "retry malformed envelope", first: challenge, retry: callToolText(t, "not JSON"), wantCalls: 2, wantSignIns: 1, wantError: "payload shape"},
		{name: "retry RPC error", first: challenge, retry: mcpAnswer{status: http.StatusOK, body: `{"jsonrpc":"2.0","id":13,"error":{"code":-32603,"message":"downstream unavailable"}}`}, wantCalls: 2, wantSignIns: 1, wantError: "downstream unavailable"},
		{name: "retry HTTP error", first: challenge, retry: mcpAnswer{status: http.StatusUnauthorized, body: "session expired"}, wantCalls: 2, wantSignIns: 1, wantError: "HTTP 401"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			const sessionID = "namespace-proof-session"
			var mu sync.Mutex
			var events []string
			calls, signIns, listings := 0, 0, 0
			var firstArgs map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Mcp-Session-Id") != sessionID || r.Header.Get("Authorization") != "Bearer "+testToken {
					t.Error("discovery and calls must retain the proof's session and identity")
				}
				var request struct {
					Method string `json:"method"`
					Params struct {
						Name      string         `json:"name"`
						Arguments map[string]any `json:"arguments"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				if request.Method != "tools/call" {
					t.Errorf("method = %q, want tools/call", request.Method)
				}
				var answer mcpAnswer
				switch request.Params.Name {
				case "list_tools":
					listings++
					events = append(events, "list")
					answer = callToolText(t, `{"tools":[{"name":"x_kubernetes_list"}]}`)
				case "call_tool":
					calls++
					events = append(events, "call")
					wantArgs := map[string]any{"name": kubernetesTool(cfg, "list"), "arguments": map[string]any{"resourceType": "namespaces", "management_cluster": cfg.MCPServerName()}}
					if !reflect.DeepEqual(request.Params.Arguments, wantArgs) {
						t.Errorf("namespace arguments = %#v, want %#v", request.Params.Arguments, wantArgs)
					}
					answer = tc.first
					if calls == 1 {
						firstArgs = request.Params.Arguments
					} else {
						if calls > 2 || signIns != 1 || !reflect.DeepEqual(firstArgs, request.Params.Arguments) {
							t.Error("retry must follow one sign-in, with unchanged arguments, at most once")
						}
						answer = tc.retry
					}
				default:
					t.Errorf("unexpected tool %q", request.Params.Name)
					http.Error(w, "unexpected tool", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(answer.status)
				_, _ = io.WriteString(w, answer.body)
			}))
			t.Cleanup(srv.Close)
			session := &musterSession{client: srv.Client(), url: srv.URL, token: testToken, id: sessionID, seq: 10}
			tools, names, err := platformNamespaces(cfg, newProofStages(), session, func(s *musterSession) error {
				mu.Lock()
				defer mu.Unlock()
				signIns++
				events = append(events, "signin")
				if s != session || s.id != sessionID || s.token != testToken || s.url != srv.URL || s.client != srv.Client() {
					t.Error("sign-in must use the namespace call's session")
				}
				if calls != 1 || listings != 1 {
					t.Error("listed tools must challenge on the call before sign-in")
				}
				return tc.signInErr
			})
			mu.Lock()
			defer mu.Unlock()
			if calls != tc.wantCalls || signIns != tc.wantSignIns || listings != 1 {
				t.Errorf("calls/sign-ins/listings = %d/%d/%d, want %d/%d/1; events %v", calls, signIns, listings, tc.wantCalls, tc.wantSignIns, events)
			}
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || names != nil {
					t.Fatalf("names = %v, error = %v; want no names and %q", names, err, tc.wantError)
				}
				if tc.signInErr != nil && !errors.Is(err, tc.signInErr) {
					t.Errorf("sign-in error not preserved: %v", err)
				}
				return
			}
			if err != nil || !slices.Equal(names, []string{"default", "kube-system"}) || !slices.Equal(tools, []string{kubernetesTool(cfg, "list")}) {
				t.Fatalf("names = %v, error = %v; want the returned namespace names", names, err)
			}
		})
	}
}
