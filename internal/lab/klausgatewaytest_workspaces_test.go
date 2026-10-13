package lab

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestPromptWorkspaces: the prompt's options, grouped by Organization, each
// map to the value a pick sends; a message without the select offers none,
// and a rewritten prompt counts as no open prompt.
func TestPromptWorkspaces(t *testing.T) {
	value := func(w string) string { return `{"t":"1.2","w":"` + w + `"}` }
	prompt := slackMessage{Blocks: []map[string]any{
		{fieldTypeKey: slackBlockSection, slackKeyText: map[string]any{fieldTypeKey: slackMrkdwn, slackKeyText: slackWorkspaceAsk}},
		{fieldTypeKey: slackBlockActions, slackKeyElements: []any{
			map[string]any{fieldTypeKey: slackStaticSelect, slackKeyActionID: slackWorkspaceSelect, "option_groups": []any{
				map[string]any{"options": []any{map[string]any{slackKeyValue: value(gatewayWSPublic)}}},
				map[string]any{"options": []any{map[string]any{slackKeyValue: value(gatewayWSPrivate)}, map[string]any{slackKeyValue: "not json"}}},
			}},
			map[string]any{fieldTypeKey: slackButton, slackKeyActionID: slackWorkspaceNone, slackKeyValue: "1.2"},
		}},
	}}
	got := promptWorkspaces(prompt)
	if len(got) != 2 || got[gatewayWSPublic] != value(gatewayWSPublic) || got[gatewayWSPrivate] != value(gatewayWSPrivate) {
		t.Errorf("promptWorkspaces = %v", got)
	}
	rewritten := slackMessage{Text: slackWorkspaceChosen + gatewayWSPublic + "*"}
	if n := workspacePrompts([]slackMessage{prompt, rewritten, prompt}); n != 2 {
		t.Errorf("workspacePrompts = %d, want 2", n)
	}
	if got := promptWorkspaces(rewritten); len(got) != 0 {
		t.Errorf("a rewritten prompt offers %v", got)
	}
}

// TestPickerWorkspace: the Workspace input is found by its block_id prefix,
// at any revision, with the value its select opens on and its hint; the
// agent refs are the stage's Agent's and the fixture's.
func TestPickerWorkspace(t *testing.T) {
	view := map[string]any{"blocks": []any{
		map[string]any{"block_id": slackPickerAgentBlock, "element": map[string]any{"options": []any{
			map[string]any{slackKeyValue: kagentNamespace + "/" + gatewayWSAgent},
			map[string]any{slackKeyValue: kagentNamespace + "/" + klausGatewayTestAgent},
		}}},
		map[string]any{"block_id": slackPickerWorkspaceBlock + ".3",
			"element": map[string]any{"initial_option": map[string]any{slackKeyValue: gatewayWSPublic}},
			"hint":    map[string]any{slackKeyText: slackPickerLastUsed + " agentlab."}},
	}}
	block, initial, hint, ok := pickerWorkspace(view)
	if !ok || block != slackPickerWorkspaceBlock+".3" || initial != gatewayWSPublic || !strings.HasPrefix(hint, slackPickerLastUsed) {
		t.Errorf("pickerWorkspace = %q %q %q %v", block, initial, hint, ok)
	}
	ws, other, err := pickerRefs(view)
	if err != nil || ws != kagentNamespace+"/"+gatewayWSAgent || other != kagentNamespace+"/"+klausGatewayTestAgent {
		t.Errorf("pickerRefs = %q %q %v", ws, other, err)
	}
	g := &gatewayWorkspaces{}
	if err := g.preset(view, gatewayWSPublic, true, "test"); err != nil {
		t.Error(err)
	}
	if err := g.preset(view, slackPickerWorkspaceNone, false, "test"); err == nil {
		t.Error("a preset of the workspace passed as No workspace")
	}
	if _, _, _, ok := pickerWorkspace(map[string]any{"blocks": []any{map[string]any{"block_id": slackPickerAgentBlock}}}); ok {
		t.Error("a picker without the Workspace input reads as having one")
	}
	if _, _, err := pickerRefs(map[string]any{}); err == nil {
		t.Error("a picker without the agents passed")
	}
}

// TestNamesOnly: a refusal names the unreadable repository, in backticks,
// and never the readable one.
func TestNamesOnly(t *testing.T) {
	if err := namesOnly("You cannot read `"+syncPrivateRepo+"`.", syncPrivateRepo, syncListRepoA); err != nil {
		t.Error(err)
	}
	for _, refusal := range []string{"You cannot read it.", "You cannot read `" + syncPrivateRepo + "`, `" + syncListRepoA + "`."} {
		if err := namesOnly(refusal, syncPrivateRepo, syncListRepoA); err == nil {
			t.Errorf("namesOnly(%q) passed", refusal)
		}
	}
}

// TestGatewayAtLeast: the workspaces floor compares releases and release
// candidates, and lets a dev build through.
func TestGatewayAtLeast(t *testing.T) {
	for described, want := range map[string]bool{
		"klaus-gateway 4.5.0 (abc123)":       false,
		"klaus-gateway 4.6.0-rc.1 (abc123)":  true,
		"klaus-gateway 4.6.2-rc.2 (abc123)":  true,
		"klaus-gateway (devel) (abc123)":     true,
		"klaus-gateway (version not logged)": true,
	} {
		if got := gatewayAtLeast(described, klausGatewayWorkspacesSince); got != want {
			t.Errorf("gatewayAtLeast(%q) = %v, want %v", described, got, want)
		}
	}
}

// TestKlausGatewayWorkspacesPreflight: a lab without the workspaces fails
// with the reason before anything runs.
func TestKlausGatewayWorkspacesPreflight(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Workspaces.Enabled = false
	err := klausGatewayWorkspacesPreflight(cfg, "admin@lab.local")
	if err == nil || !strings.Contains(err.Error(), "--workspaces needs the workspace-manager") || !strings.Contains(err.Error(), "platform.workspaces is off") {
		t.Errorf("preflight on a lab without workspaces = %v", err)
	}
}

// TestGatewayWorkspacesPASS: the PASS lines name each step, the presets
// included, and say why the private steps were skipped.
func TestGatewayWorkspacesPASS(t *testing.T) {
	o := &gatewayWorkspacesOutcome{session: "s1", noneSession: "s2", modalSession: "s3", repos: gatewayWSPublicRepos, volume: "v1",
		presets: []string{"No workspace on the first open"}, refusal: "no", collaborator: "no", refusalTraces: accessTraces{kagent: []string{}}}
	lines := gatewayWorkspacesPASS(o)
	if len(lines) != 5 || !strings.Contains(lines[2], "No workspace on the first open") || !strings.Contains(lines[3], syncPrivateRepo) {
		t.Errorf("PASS lines = %q", lines)
	}
	o.privateSkipped = "no App"
	if lines := gatewayWorkspacesPASS(o); len(lines) != 4 || !strings.Contains(lines[3], "no App") {
		t.Errorf("PASS lines without the private workspace = %q", lines)
	}
}

// TestSlackDriverPickerActions: a pick on a message, a pick inside an open
// modal (the view as Slack holds it, with an id and a hash) and the slash
// command reach the gateway's endpoints as Slack sends them; the fake keeps
// a views.update as the modal's latest state.
func TestSlackDriverPickerActions(t *testing.T) {
	var paths []string
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		paths, bodies = append(paths, r.URL.Path), append(bodies, string(body))
	}))
	defer srv.Close()
	f := startTestFake(t, nil)
	d := newSlackDriver(srv.URL, "sekrit", f, "C1")
	msg := slackMessage{TS: "9.1", Channel: "C1", ThreadTS: "9.0"}
	if err := d.pick("UP", msg, slackWorkspaceSelect, `{"t":"9.0","w":"ws"}`); err != nil {
		t.Fatal(err)
	}
	view := map[string]any{"callback_id": slackPickerCallback, "blocks": []any{}}
	if err := d.pickInView("UP", view, slackPickerAgentBlock, slackPickerAgentAction, "kagent/a"); err != nil {
		t.Fatal(err)
	}
	if err := d.slashCommand("UP", "hello"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{slackInteractionsPath, slackInteractionsPath, slackCommandsPath}) {
		t.Fatalf("paths = %v", paths)
	}
	var action struct {
		Type    string `json:"type"`
		Actions []struct {
			Type           string `json:"type"`
			ActionID       string `json:"action_id"`
			SelectedOption struct {
				Value string `json:"value"`
			} `json:"selected_option"`
		} `json:"actions"`
		View map[string]any `json:"view"`
	}
	for i, want := range []string{`{"t":"9.0","w":"ws"}`, "kagent/a"} {
		form, _ := url.ParseQuery(bodies[i])
		if err := json.Unmarshal([]byte(form.Get("payload")), &action); err != nil {
			t.Fatal(err)
		}
		if action.Type != "block_actions" || len(action.Actions) != 1 || action.Actions[0].Type != slackStaticSelect || action.Actions[0].SelectedOption.Value != want {
			t.Errorf("pick %d = %+v", i, action)
		}
	}
	if action.View["callback_id"] != slackPickerCallback || action.View["id"] == "" || action.View["hash"] == "" {
		t.Errorf("the view of the pick in the modal = %v", action.View)
	}
	if _, held := view["hash"]; held {
		t.Error("pickInView changed the caller's view")
	}
	form, _ := url.ParseQuery(bodies[2])
	if form.Get("command") != slackCommand || form.Get("user_id") != "UP" || form.Get("channel_id") != "C1" || form.Get("text") != "hello" || form.Get("trigger_id") == "" || form.Get("response_url") == "" {
		t.Errorf("slash command = %v", form)
	}

	callFake(t, f, slackViewsOpen, nil, map[string]any{"view": map[string]any{"callback_id": "first"}})
	callFake(t, f, slackViewsUpdate, nil, map[string]any{"view_id": "V1", "hash": "h", "view": map[string]any{"callback_id": "redrawn"}})
	if views := f.openedViews(); len(views) != 2 || views[1]["callback_id"] != "redrawn" {
		t.Errorf("openedViews = %v", views)
	}
}
