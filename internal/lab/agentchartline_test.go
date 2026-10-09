package lab

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// testChartLine1 and testChartLine2 are the lines a meta chart declares
	// through agent-manager's --agent-chart-semver.
	testChartLine1 = ">=1.5.0 <2.0.0"
	testChartLine2 = ">=2.0.0 <3.0.0"
)

// agentChartMajors is every range or version the tests probe and the one
// major it admits, -1 for none or several.
var agentChartMajors = map[string]int{
	agentChartRange:      1,
	">=1.0.0 <1.5.0":     1,
	testChartLine1:       1,
	"1.4.4":              1,
	"2.x":                2,
	testChartLine2:       2,
	">=2.0.0-0 <3.0.0-0": 2,
	"2.1.0":              2,
	"3.0.0":              3,
	">=0.2.1 <1.0.0":     0,
	"x.x.x":              -1,
	">=1.0.0 <3.0.0":     -1,
	"not a constraint":   -1,
}

func TestAgentChartMajor(t *testing.T) {
	for semver, want := range agentChartMajors {
		got, ok := agentChartMajor(semver)
		if want < 0 {
			if ok {
				t.Errorf("agentChartMajor(%q) = %d, want no single major", semver, got)
			}
			continue
		}
		if !ok || got != uint64(want) {
			t.Errorf("agentChartMajor(%q) = %d, %v, want %d", semver, got, ok, want)
		}
	}
}

// TestAgentChartLine checks every probe against a declared 1.x and a declared
// 2.x line: on the line exactly when it admits the declared major alone.
func TestAgentChartLine(t *testing.T) {
	for declared, major := range map[string]int{testChartLine1: 1, testChartLine2: 2} {
		t.Run(declared, func(t *testing.T) {
			for semver, want := range agentChartMajors {
				on := want == major
				if got := agentChartLine(semver, declared); got != on {
					t.Errorf("agentChartLine(%q, %q) = %v, want %v", semver, declared, got, on)
				}
				if err := checkAgentChartLine("the chart", semver, declared); (err == nil) != on {
					t.Errorf("checkAgentChartLine(%q, %q) = %v, want on the line: %v", semver, declared, err, on)
				}
			}
		})
	}
	for semver := range agentChartMajors {
		if agentChartLine(semver, "x.x.x") {
			t.Errorf("agentChartLine(%q, x.x.x) = true: a declaration of no line admits nothing", semver)
		}
	}
}

func TestCheckAgentChartLineMismatchNamesBoth(t *testing.T) {
	err := checkAgentChartLine("HelmRelease agents-test installed", "1.6.2", testChartLine2)
	if err == nil {
		t.Fatal("a 1.x chart on a declared 2.x line passed")
	}
	for _, want := range []string{"HelmRelease agents-test installed", `"1.6.2"`, `"` + testChartLine2 + `"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestAgentChartSemverArg(t *testing.T) {
	var deploy map[string]any
	if err := yaml.Unmarshal([]byte(`
spec:
  template:
    spec:
      containers:
      - name: manager
        args: ["serve", "--agent-chart-semver=>=2.0.0 <3.0.0", "--port=8080"]
`), &deploy); err != nil {
		t.Fatal(err)
	}
	if got, ok := agentChartSemverArg(&unstructured.Unstructured{Object: deploy}); !ok || got != testChartLine2 {
		t.Errorf("agentChartSemverArg = %q, %v, want %s", got, ok, testChartLine2)
	}
	if got, ok := agentChartSemverArg(&unstructured.Unstructured{Object: map[string]any{}}); ok {
		t.Errorf("agentChartSemverArg of a Deployment without the flag = %q, want none", got)
	}
}

// TestAgentTemplateOnBothLines reads an agent's template the way each chart
// line renders it: an Agent referencing its AgentTemplate (1.x) carries no
// inline one, an Agent with spec.template (2.x) is its own template, the
// Agent's labels and annotations included.
func TestAgentTemplateOnBothLines(t *testing.T) {
	var byRef, inline agentObject
	for raw, into := range map[string]*agentObject{
		`{"metadata": {"name": "a"}, "spec": {"templateRef": {"name": "a"}, "harnessRef": {"name": "kagent"}}}`: &byRef,
		`{"metadata": {"name": "a", "generation": 3, "labels": {"helm.toolkit.fluxcd.io/name": "a"}, "annotations": {"ui.giantswarm.io/display-name": "A"}},
		  "spec": {"harnessRef": {"name": "kagent"}, "template": {"systemPrompt": "hi", "modelConfig": {"name": "m"},
		    "skills": [{"name": "s", "source": {"git": {"url": "u", "commit": "c"}, "path": "p"}}],
		    "tools": [{"mcp": {"server": {"kind": "RemoteMCPServer", "name": "a"}, "requireApproval": true}}]}}}`: &inline,
	} {
		if err := json.Unmarshal([]byte(raw), into); err != nil {
			t.Fatal(err)
		}
	}
	if !byRef.rendersTemplate("a") || byRef.rendersTemplate("b") {
		t.Errorf("an Agent referencing AgentTemplate a: rendersTemplate(a)=%v, rendersTemplate(b)=%v", byRef.rendersTemplate("a"), byRef.rendersTemplate("b"))
	}
	if _, ok, err := byRef.inlineTemplate(); ok || err != nil {
		t.Errorf("an Agent referencing its AgentTemplate reads as inline (%v, %v)", ok, err)
	}
	if !inline.rendersTemplate("a") {
		t.Error("an Agent with spec.template does not render its template")
	}
	tmpl, ok, err := inline.inlineTemplate()
	if !ok || err != nil {
		t.Fatalf("inlineTemplate = %v, %v", ok, err)
	}
	if tmpl.Metadata.Name != "a" || tmpl.Metadata.Generation != 3 || tmpl.Metadata.Labels[fluxHelmReleaseNameLabel] != "a" || tmpl.Metadata.Annotations[displayNameAnnotation] != "A" {
		t.Errorf("inline template metadata = %+v, wanted the Agent's", tmpl.Metadata)
	}
	if tmpl.Spec.SystemPrompt != "hi" || tmpl.Spec.ModelConfig == nil || tmpl.Spec.ModelConfig.Name != "m" || tmpl.mcpServer() != "a" || !tmpl.requiresApproval("a") {
		t.Errorf("inline template spec = %+v", tmpl.Spec)
	}
	if s := tmpl.skill("s"); s == nil || s.Source.Git == nil || s.Source.Git.Commit != "c" || s.Source.Path != "p" {
		t.Errorf("inline template skill s = %+v", s)
	}
}

// TestAgentManagerStatusShapes reads get_agent_status and get_agent in both
// agent-manager shapes: the Agent API's agent object and harness, and the
// AgentTemplate API's per-Harness report.
func TestAgentManagerStatusShapes(t *testing.T) {
	for raw, wantReady := range map[string]bool{
		`{"verdict": "ready", "agent": {"harness": "kagent", "ready": true}}`:                     true,
		`{"verdict": "ready", "template": {"harnesses": [{"harness": "kagent", "ready": true}]}}`: true,
		`{"verdict": "progressing", "agent": {"harness": "kagent", "ready": false}}`:              false,
	} {
		var s agentManagerStatus
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatal(err)
		}
		if on, ready := s.readyOn(); on != kagentHarness || ready != wantReady {
			t.Errorf("readyOn(%s) = %q, %v, want %s, %v", raw, on, ready, kagentHarness, wantReady)
		}
	}
	for _, raw := range []string{`{"harness": "kagent"}`, `{"harnesses": [{"harness": "kagent"}]}`} {
		var a agentManagerAgent
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			t.Fatal(err)
		}
		if got := a.harness(); got != kagentHarness {
			t.Errorf("harness(%s) = %q, want %s", raw, got, kagentHarness)
		}
	}
}
