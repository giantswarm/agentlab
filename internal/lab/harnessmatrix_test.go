package lab

import (
	"maps"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

func TestReplyFields(t *testing.T) {
	got := replyFields("Sure.\nCODEWORD=hm-ab12\n`SKILLS=migrate-agent-plugin`\nTOOL=none\n")
	want := map[string]string{"CODEWORD": "hm-ab12", "SKILLS": "migrate-agent-plugin", "TOOL": "none"}
	if !maps.Equal(got, want) {
		t.Fatalf("replyFields = %v, want %v", got, want)
	}
}

func TestMatrixJudge(t *testing.T) {
	cell := &matrixCell{c: matrixCase{key: "shared-prompt"}, codeword: "hm-ab12"}
	if outcome, _ := cell.judge("CODEWORD=hm-ab12", nil); outcome != "CHILD INVOKED" {
		t.Errorf("the codeword answered: %s", outcome)
	}
	if outcome, _ := cell.judge("CODEWORD=none", nil); outcome != "CHILD NOT INVOKED" {
		t.Errorf("no codeword: %s", outcome)
	}
	mock := &matrixCell{c: matrixCase{key: "shared-prompt"}, codeword: "hm-ab12", modelConfig: MockLLMModelConfig}
	if outcome, evidence := mock.judge("CODEWORD=hm-ab12", nil); outcome != "CHILD INVOKED" || !strings.Contains(evidence, "proven on the mockllm") {
		t.Errorf("the codeword answered on the mockllm: %s: %s", outcome, evidence)
	}
	if _, evidence := cell.judge("CODEWORD=hm-ab12", nil); strings.Contains(evidence, "mockllm") {
		t.Errorf("a turn on a model says the mockllm: %s", evidence)
	}
	parts := &matrixCell{c: matrixCase{key: "plugin-parts"}}
	outcome, evidence := parts.judge("COMMANDS=/feature-dev\nSUBAGENTS=feature-dev:code-explorer\nEXPLORED=go, docs", []string{"Agent"})
	if outcome != "SUBAGENT INVOKED" || !strings.Contains(evidence, "COMMANDS=/feature-dev") {
		t.Errorf("plugin parts: %s: %s", outcome, evidence)
	}
}

func TestMatrixCells(t *testing.T) {
	cells := matrixCells([]string{kagentHarness, config.CodingRuntimeClaude, config.CodingRuntimeCodex}, HarnessMatrixOptions{})
	// Four Shared cases on three Harnesses, the plugin case on claude only.
	if len(cells) != 13 {
		t.Fatalf("%d cells, want 13", len(cells))
	}
	for _, c := range cells {
		if c.harness == config.CodingRuntimeCodex && (c.noTurn == (c.c.key == "shared-prompt") || c.modelConfig != MockLLMModelConfig) {
			t.Errorf("codex cell %s without a real model: noTurn %v on %s, wanted the mockllm and a turn on shared-prompt alone", c.c.key, c.noTurn, c.modelConfig)
		}
		if c.c.key == "plugin-parts" && c.harness != config.CodingRuntimeClaude {
			t.Errorf("plugin-parts on %s", c.harness)
		}
	}
}

func TestMatrixManifestsParse(t *testing.T) {
	for _, c := range matrixCells([]string{config.CodingRuntimeClaude}, HarnessMatrixOptions{}) {
		for doc := range strings.SplitSeq(c.manifests(), "---\n") {
			var obj map[string]any
			if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
				t.Fatalf("%s: %v\n%s", c.c.key, err, doc)
			}
			if obj["kind"] == nil {
				t.Fatalf("%s: a document without a kind:\n%s", c.c.key, doc)
			}
		}
	}
}
