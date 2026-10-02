package lab

import (
	"reflect"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

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
