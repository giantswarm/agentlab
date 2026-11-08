package lab

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

const portalEmptyModels = `{"models":[]}`

func TestReadPortalModelPlan(t *testing.T) {
	for _, tc := range []struct {
		name, answer, want string
		toolError          bool
	}{
		{"preferred", `{"models":[{"name":"large","sizeBytes":9000000000},{"name":"smollm2:135m","sizeBytes":475000000}]}`, "475000000 bytes from list_models", false},
		{"fallback", `{"models":[{"name":"large","sizeBytes":9000000000},{"name":"tiny","sizeBytes":1000}]}`, "tiny on ollama", false},
		{"inventory empty", portalEmptyModels, "no host model: the Models write is skipped (ollama holds no downloaded model)", false},
		{"inventory refused", "inventory unavailable", "inventory unavailable", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFakePortal(t)
			list := modelManagerToolPrefix + "list_models"
			fp.tools[list] = func(args map[string]any) (string, bool) {
				if args[backendField] != config.ModelManagerBackendOllama {
					t.Errorf("backend: %v", args)
				}
				return tc.answer, tc.toolError
			}
			plan, err := readPortalModelPlan(fp.session(testPortalUser, platformAdminsGroup), config.ModelManagerBackendOllama)
			if tc.toolError {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(plan.String(), tc.want) {
					t.Errorf("plan = %s", plan)
				}
				if plan.skip == "" && !strings.Contains(plan.String(), "--skip-model-write") {
					t.Error("no skip instruction")
				}
			}
			if len(fp.captured) != 1 || len(fp.captured[list]) != 1 {
				t.Errorf("preflight called tools: %v", fp.captured)
			}
		})
	}
}

func TestBackstageModelPlanSkips(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Enabled, cfg.Platform.Agents, cfg.Platform.ModelManager.Enabled = true, true, true
	cfg.Platform.ModelManager.Backends = []string{config.ModelManagerBackendOllama}
	for _, tc := range []struct {
		name   string
		opts   BackstageTestOptions
		emails []string
		want   string
	}{
		{"flag", BackstageTestOptions{SkipModelWrite: true}, []string{testPortalUser}, "--skip-model-write"},
		{"no admin", BackstageTestOptions{}, []string{testPortalViewer}, "no platform-admins user selected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := backstageModelPlan(cfg, tc.emails, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(plan.String(), tc.want) {
				t.Errorf("plan = %s", plan)
			}
		})
	}
}

func TestPortalModelPlanInventoryChanges(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		wantError    bool
	}{
		{"removed", portalEmptyModels, true},
		{"resized", `{"models":[{"name":"tiny","sizeBytes":2000}]}`, true},
		{"replaced", `{"models":[{"name":"other","sizeBytes":1000}]}`, true},
		{"loaded since plan", `{"models":[{"name":"tiny","sizeBytes":1000,"loaded":true}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFakePortal(t)
			fp.tools[modelManagerToolPrefix+"list_models"] = func(map[string]any) (string, bool) { return tc.answer, false }
			plan := portalModelPlan{backend: config.ModelManagerBackendOllama, model: portalModelInfo{Name: "tiny", SizeBytes: 1000}}
			model, err := plan.currentModel(fp.session(testPortalUser, platformAdminsGroup))
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "inventory changed") {
					t.Fatalf("err = %v", err)
				}
			} else if err != nil || !model.Loaded {
				t.Fatalf("current model = %+v, err = %v", model, err)
			}
			if len(fp.captured) != 1 {
				t.Errorf("unexpected calls: %v", fp.captured)
			}
		})
	}
}

// --plan exits after the announcement, without entering any proof step.
func TestBackstageModelPlanBeforeProof(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("KUBECONFIG", "/elsewhere/config")
	stubKindKubeconfig(t)
	resetKindKubeconfigCache(t)
	t.Cleanup(resetLabKube)
	prev := readApiserverOIDC
	t.Cleanup(func() { readApiserverOIDC = prev })
	for _, staleKeys := range []bool{false, true} {
		for _, planOnly := range []bool{true, false} {
			calls := 0
			readApiserverOIDC = func(context.Context, *config.Config, config.User) (oidcVerdict, string, error) {
				calls++
				if staleKeys && calls == 1 {
					return oidcStaleKeys, "Dex's new key is published", nil
				}
				return oidcAccepted, testPortalUser, nil
			}
			original := os.Stdout
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			os.Stdout = w
			t.Cleanup(func() { os.Stdout = original })
			stopped := errors.New("stop before proof")
			err = BackstageTest(config.Default(), nil, BackstageTestOptions{
				Plan: planOnly, SkipModelWrite: true,
				BeforeProof: func() error { step("first proof step"); return stopped },
			})
			os.Stdout = original
			_ = w.Close()
			out, readErr := io.ReadAll(r)
			_ = r.Close()
			if readErr != nil || (planOnly && err != nil) || (!planOnly && !errors.Is(err, stopped)) {
				t.Fatalf("plan: %v; output: %v", err, readErr)
			}
			output := string(out)
			announcement := strings.Index(output, "Host model plan: no host model:")
			firstStep := strings.Index(output, "==>")
			if announcement < 0 || (planOnly && firstStep >= 0) || (!planOnly && firstStep <= announcement) {
				t.Errorf("stale keys %v, plan-only %v output = %s", staleKeys, planOnly, out)
			}
			if got := strings.Contains(output, "The apiserver refuses Dex tokens signed with a key Dex publishes"); got != (staleKeys && !planOnly) {
				t.Errorf("stale keys %v, plan-only %v recovery step = %v: %s", staleKeys, planOnly, got, out)
			}
			wantCalls := 1
			if staleKeys {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Errorf("readiness checks = %d, want %d", calls, wantCalls)
			}
		}
	}
}

// TestPortalMusterCallModelManager: a model-manager tool goes over the same
// route as agent-manager's, named in full, its arguments carried; a refusal
// keeps the tool's wording.
func TestPortalMusterCallModelManager(t *testing.T) {
	fp := newFakePortal(t)
	listBackends := modelManagerToolPrefix + "list_backends"
	wire := modelManagerToolPrefix + "wire_model"
	fp.tools[listBackends] = func(map[string]any) (string, bool) {
		return `{"backends":[{"backend":"ollama"},{"backend":"kserve"}]}`, false
	}
	fp.tools[wire] = func(map[string]any) (string, bool) {
		return `forbidden: modelconfigs.kagent.dev is forbidden: User "viewer@lab.local" cannot create`, true
	}
	ps := fp.session(testPortalUser, platformAdminsGroup)
	var listed struct {
		Backends []struct {
			Backend string `json:"backend"`
		} `json:"backends"`
	}
	if err := portalMusterCall(ps, listBackends, nil, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Backends) != 2 || listed.Backends[0].Backend != config.ModelManagerBackendOllama {
		t.Errorf("listed = %+v", listed)
	}
	viewer := fp.session(testPortalViewer, viewerGroup)
	args := map[string]any{backendField: config.ModelManagerBackendOllama, modelField: ModelsTestModelOllama}
	err := portalMusterCall(viewer, wire, args, nil)
	if err == nil || !strings.Contains(err.Error(), refusalForbidden) || !strings.Contains(err.Error(), wire) {
		t.Errorf("the viewer's refusal: %v", err)
	}
	if got := fp.captured[wire]; len(got) != 1 || got[0][modelField] != ModelsTestModelOllama || got[0][backendField] != config.ModelManagerBackendOllama {
		t.Errorf("captured = %v", got)
	}
}

func TestMissingBackends(t *testing.T) {
	want := []string{config.ModelManagerBackendOllama, config.ModelManagerBackendLemonade, config.ModelManagerBackendKServe}
	if got := missingBackends(want, []string{config.ModelManagerBackendKServe, config.ModelManagerBackendOllama}); !reflect.DeepEqual(got, []string{config.ModelManagerBackendLemonade}) {
		t.Errorf("missing = %v", got)
	}
	if got := missingBackends(want, want); got != nil {
		t.Errorf("none missing = %v", got)
	}
}

// TestHostBackend: the write goes to the first host server; serving alone has
// none.
func TestHostBackend(t *testing.T) {
	for _, tc := range []struct {
		backends []string
		want     string
	}{
		{[]string{config.ModelManagerBackendLemonade, config.ModelManagerBackendOllama, config.ModelManagerBackendKServe}, config.ModelManagerBackendLemonade},
		{[]string{config.ModelManagerBackendKServe, config.ModelManagerBackendOllama}, config.ModelManagerBackendOllama},
		{[]string{config.ModelManagerBackendKServe}, ""},
		{nil, ""},
	} {
		if got := hostBackend(tc.backends); got != tc.want {
			t.Errorf("hostBackend(%v) = %q, want %q", tc.backends, got, tc.want)
		}
	}
}

// TestProofModelOf: the backend's proof model when downloaded, the smallest
// otherwise, nothing from an empty inventory.
func TestProofModelOf(t *testing.T) {
	models := []portalModelInfo{
		{Name: "qwen3:30b", SizeBytes: 18_000_000_000},
		{Name: "smollm2:135m", SizeBytes: 270_000_000},
		{Name: "", SizeBytes: 1},
		{Name: "gemma3:270m", SizeBytes: 290_000_000, Loaded: true},
	}
	if got, ok := proofModelOf(models, ModelsTestModelOllama); !ok || got.Name != "smollm2:135m" {
		t.Errorf("smallest = %+v, %v", got, ok)
	}
	if got, ok := proofModelOf(models, "GEMMA3:270m"); !ok || got.Name != "gemma3:270m" || !got.Loaded {
		t.Errorf("preferred = %+v, %v", got, ok)
	}
	if _, ok := proofModelOf(nil, ModelsTestModelOllama); ok {
		t.Error("an empty inventory picked a model")
	}
}

// TestLoggedFor: model-manager's text log line names the message, the model
// and the caller; another model's or another caller's line does not count.
func TestLoggedFor(t *testing.T) {
	logs := strings.Join([]string{
		`time=2026-10-02T04:00:00Z level=INFO msg="model loaded" backend=ollama model=smollm2:135m keepAlive="" preset="" node="" caller=admin@lab.local`,
		`time=2026-10-02T04:00:01Z level=INFO msg="model unloaded" backend=ollama model=smollm2:135m caller=dev@lab.local`,
		`time=2026-10-02T04:00:02Z level=INFO msg="model loaded" backend=ollama model=smollm2:135m-x caller=viewer@lab.local`,
	}, "\n")
	if !loggedFor(logs, modelManagerLoggedLoad, "smollm2:135m", testPortalUser) {
		t.Error("the admin's load is not found")
	}
	if loggedFor(logs, modelManagerLoggedUnload, "smollm2:135m", testPortalUser) {
		t.Error("another caller's unload counted")
	}
	if loggedFor(logs, modelManagerLoggedLoad, "smollm2:135m", testPortalViewer) {
		t.Error("another model's load counted")
	}
}

// TestWriteTools: both tools named in full, the model left as found.
func TestWriteTools(t *testing.T) {
	load, unload := modelManagerToolPrefix+"load_model", modelManagerToolPrefix+"unload_model"
	if got := writeTools(false); !reflect.DeepEqual(got, []string{load, unload}) {
		t.Errorf("unloaded: %v", got)
	}
	if got := writeTools(true); !reflect.DeepEqual(got, []string{unload, load}) {
		t.Errorf("loaded: %v", got)
	}
	if got := strings.Join(writeTools(false), " + "); got != "x_model-manager_load_model + x_model-manager_unload_model" {
		t.Errorf("verdict wording: %q", got)
	}
}
