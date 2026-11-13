package lab

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// overlayProofPatch is a proof overlay's own connectivity patch: a route
// filter of its own, nothing of the lab's.
const overlayProofPatch = `components:
  agent-platform-connectivity:
    versionRange: "1.2.3"
    postRenderers:
      - kustomize:
          patches:
            - target:
                kind: HTTPRoute
                name: mcp-kubernetes
              patch: |
                - op: add
                  path: /metadata/labels/proof
                  value: mcp-kubernetes
`

func writeValues(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func connectivityPostRenderers(t *testing.T, values map[string]any) []string {
	t.Helper()
	components, _ := values["components"].(map[string]any)
	block, _ := components[config.ConnectivityChartName].(map[string]any)
	list, _ := block[postRenderersKey].([]any)
	out := make([]string, 0, len(list))
	for _, entry := range list {
		raw, err := yaml.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(raw))
	}
	return out
}

// TestOverlayKeepsLabPostRenderers is the agentlab-2 incident: a proof
// overlay that sets the connectivity chart's postRenderers no longer drops
// the lab's HSTS patch on the portal's route — the lab's entries come first,
// the overlay's after them, and the overlay's other keys still win.
func TestOverlayKeepsLabPostRenderers(t *testing.T) {
	cfg := config.Default()
	postRenderers, err := componentPostRenderers(cfg, defaultDevImageNames(cfg), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := renderTemplate(cfg, platformValuesTemplate, func(d *tmplData) { d.PostRenderers = postRenderers })
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lab := writeValues(t, dir, "lab.yaml", string(raw))
	overlay := writeValues(t, dir, "proof.yaml", overlayProofPatch)

	// Helm's own merge replaces the list: the bug this guards against.
	replaced, err := helmValuesFiles(lab, overlay)
	if err != nil {
		t.Fatal(err)
	}
	if got := connectivityPostRenderers(t, replaced); len(got) != 1 || strings.Contains(got[0], hstsHeader) {
		t.Fatalf("helm -f semantics: want the overlay's list alone, got %q", got)
	}

	values, err := platformValuesFiles(lab, []string{overlay})
	if err != nil {
		t.Fatal(err)
	}
	got := connectivityPostRenderers(t, values)
	if len(got) != 2 || !strings.Contains(got[0], hstsHeader) || !strings.Contains(got[1], "value: mcp-kubernetes") {
		t.Fatalf("want the lab's HSTS patch then the overlay's, got %q", got)
	}
	block := values["components"].(map[string]any)[config.ConnectivityChartName].(map[string]any)
	if block["versionRange"] != "1.2.3" {
		t.Errorf("the overlay's versionRange must still win, got %v", block["versionRange"])
	}

	// The same overlay twice adds nothing.
	again, err := platformValuesFiles(lab, []string{overlay, overlay})
	if err != nil {
		t.Fatal(err)
	}
	if got := connectivityPostRenderers(t, again); len(got) != 2 {
		t.Errorf("a repeated entry must be skipped, got %d entries", len(got))
	}

	// Without overlays the lab's render is read as is.
	plain, err := platformValuesFiles(lab, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := connectivityPostRenderers(t, plain); len(got) != 1 || !strings.Contains(got[0], hstsHeader) {
		t.Errorf("no overlay: want the lab's one patch, got %q", got)
	}

	// An overlay that repeats the lab's own patch adds nothing either.
	labList := plain["components"].(map[string]any)[config.ConnectivityChartName].(map[string]any)[postRenderersKey]
	repeat := writeValues(t, dir, "repeat.yaml", yamlString(t, map[string]any{
		"components": map[string]any{config.ConnectivityChartName: map[string]any{postRenderersKey: labList}},
	}))
	repeated, err := platformValuesFiles(lab, []string{repeat, overlay})
	if err != nil {
		t.Fatal(err)
	}
	if got := connectivityPostRenderers(t, repeated); len(got) != 2 || !strings.Contains(got[0], hstsHeader) || !strings.Contains(got[1], "value: mcp-kubernetes") {
		t.Errorf("an overlay repeating the lab's patch: want the lab's patch once, then the proof's, got %q", got)
	}
}

// TestOverlayPostRenderersNull: an overlay that states a component's
// postRenderers as null removes the lab's patches on purpose; an overlay
// after it appends to the empty list. A component only an overlay patches
// gets the overlay's list.
func TestOverlayPostRenderersNull(t *testing.T) {
	dir := t.TempDir()
	lab := writeValues(t, dir, "lab.yaml", `components:
  muster:
    postRenderers:
      - kustomize: {patches: [{patch: lab}]}
  backstage:
    enabled: true
`)
	null := writeValues(t, dir, "null.yaml", `components:
  muster:
    postRenderers: null
`)
	add := writeValues(t, dir, "add.yaml", `components:
  muster:
    postRenderers:
      - kustomize: {patches: [{patch: proof}]}
  backstage:
    postRenderers:
      - kustomize: {patches: [{patch: portal}]}
`)
	list := func(values map[string]any, component string) []any {
		block, _ := values["components"].(map[string]any)[component].(map[string]any)
		l, _ := block[postRenderersKey].([]any)
		return l
	}
	values, err := platformValuesFiles(lab, []string{null})
	if err != nil {
		t.Fatal(err)
	}
	if got := list(values, "muster"); len(got) != 0 {
		t.Errorf("null: want no muster patches, got %v", got)
	}
	values, err = platformValuesFiles(lab, []string{null, add})
	if err != nil {
		t.Fatal(err)
	}
	if got := list(values, "muster"); len(got) != 1 || !strings.Contains(yamlString(t, got[0]), "proof") {
		t.Errorf("null then add: want the later overlay's patch alone, got %v", got)
	}
	if got := list(values, "backstage"); len(got) != 1 || !strings.Contains(yamlString(t, got[0]), "portal") {
		t.Errorf("a component the lab does not patch: want the overlay's list, got %v", got)
	}
	values, err = platformValuesFiles(lab, []string{add})
	if err != nil {
		t.Fatal(err)
	}
	if got := list(values, "muster"); len(got) != 2 || !strings.Contains(yamlString(t, got[0]), "lab") {
		t.Errorf("add: want the lab's patch then the proof's, got %v", got)
	}
}

func yamlString(t *testing.T, v any) string {
	t.Helper()
	raw, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
