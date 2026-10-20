// Package agents holds agent definitions the lab and an installation apply
// through the platform's own tools (docs/agents.md "The codex coder").
package agents_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const codexCoderDir = "codex-coder"

// codexCoderAgent is the part of agent-manager's create_agent input the
// definition must get right.
type codexCoderAgent struct {
	Name          string   `json:"name"`
	Harness       string   `json:"harness"`
	ModelConfig   string   `json:"modelConfig"`
	Toolset       []string `json:"toolset"`
	SystemMessage string   `json:"systemMessage"`
}

// codexCoderWorkspace is workspace-manager's create_workspace input.
type codexCoderWorkspace struct {
	Name         string `json:"name"`
	Organization string `json:"organization"`
	Sources      []struct {
		Provider     string   `json:"provider"`
		Owner        string   `json:"owner"`
		Repositories []string `json:"repositories"`
	} `json:"sources"`
}

// codexCoderValues is the agent-platform values overlay with the model.
type codexCoderValues struct {
	Kagent struct {
		ModelConfigs []map[string]string `yaml:"modelConfigs"`
	} `yaml:"kagent"`
}

var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(codexCoderDir, name)) // #nosec G304 -- the fixture's own files, named by the test
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCodexCoderDefinition pins what agent-manager and the
// workspace-manager would otherwise refuse at write time, and the model the
// agent names to the values entry that renders it.
func TestCodexCoderDefinition(t *testing.T) {
	var agent codexCoderAgent
	if err := json.Unmarshal(readFixture(t, "agent.json"), &agent); err != nil {
		t.Fatalf("agent.json: %v", err)
	}
	if len(agent.Name) > 63 || !dns1123.MatchString(agent.Name) {
		t.Errorf("agent name %q is no DNS-1123 name of at most 63 characters", agent.Name)
	}
	if agent.Harness != "codex" {
		t.Errorf("agent harness %q, want codex", agent.Harness)
	}
	if len(agent.Toolset) == 0 || len(agent.Toolset) > 32 {
		t.Errorf("agent toolset %v: create_agent needs 1 to 32 selectors", agent.Toolset)
	}
	if n := len(agent.SystemMessage); n == 0 || n > 20000 {
		t.Errorf("agent systemMessage is %d characters, want 1 to 20000", n)
	}

	var values codexCoderValues
	if err := yaml.Unmarshal(readFixture(t, "values.yaml"), &values); err != nil {
		t.Fatalf("values.yaml: %v", err)
	}
	i := slices.IndexFunc(values.Kagent.ModelConfigs, func(m map[string]string) bool { return m["name"] == agent.ModelConfig })
	if i < 0 {
		t.Fatalf("values.yaml renders no ModelConfig %q, the one agent.json names", agent.ModelConfig)
	}
	model := values.Kagent.ModelConfigs[i]
	for key, want := range map[string]string{"provider": "OpenAI", "credentialType": "chatgptOAuth", "apiFormat": "responses"} { // #nosec G101 -- the ModelConfig's field values, no credential
		if model[key] != want {
			t.Errorf("ModelConfig %s: %s %q, want %q (a codex model on a ChatGPT login)", agent.ModelConfig, key, model[key], want)
		}
	}
	if _, ok := model["apiKeySecretKey"]; ok || model["apiKeySecret"] == "" {
		t.Errorf("ModelConfig %s: a ChatGPT login names its Secret in apiKeySecret and carries no apiKeySecretKey", agent.ModelConfig)
	}

	var ws codexCoderWorkspace
	if err := json.Unmarshal(readFixture(t, "workspace.json"), &ws); err != nil {
		t.Fatalf("workspace.json: %v", err)
	}
	if !dns1123.MatchString(ws.Name) || ws.Organization == "" {
		t.Errorf("workspace name %q organization %q: want a DNS-1123 name and an Organization", ws.Name, ws.Organization)
	}
	if len(ws.Sources) != 1 {
		t.Fatalf("workspace sources: %d, want the one owner giantswarm", len(ws.Sources))
	}
	src := ws.Sources[0]
	if src.Owner != "giantswarm" || src.Provider != "github" {
		t.Errorf("workspace source %s on %s, want giantswarm on github", src.Owner, src.Provider)
	}
	if len(src.Repositories) == 0 {
		t.Fatal("workspace source names no repository")
	}
	for i, r := range src.Repositories {
		if strings.Contains(r, "/") || r == "" {
			t.Errorf("repository %q: a name of the owner, without the owner", r)
		}
		if i > 0 && strings.ToLower(src.Repositories[i-1]) >= strings.ToLower(r) {
			t.Errorf("repositories not sorted and unique at %q after %q", r, src.Repositories[i-1])
		}
	}
}
