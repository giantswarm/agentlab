package lab

import (
	"strings"
	"testing"
)

// TestDiscoveryPreflight: the verdict `agentlab configure` takes on the tools
// before its first question — every missing tool named with its install
// hint, nothing when all answer. kind and Helm are never asked for:
// both are embedded (kind is reported as such).
func TestDiscoveryPreflight(t *testing.T) {
	tools := func(docker, kubectl string) []ToolVersion {
		return []ToolVersion{{dockerBin, docker}, {kindToolName, kindToolVersion()}, {kubectlBin, kubectl}}
	}
	cases := []struct {
		name    string
		tools   []ToolVersion
		want    []string // substrings of the error; nil for no error
		wantNot []string
	}{
		{name: "all good", tools: tools("29.7.2", "v1.36.4")},
		{name: "podman", tools: tools("5.8.4 (podman)", "v1.36.4")},
		{name: "kubectl missing", tools: tools("29.7.2", ""),
			want:    []string{"kubectl is not on PATH", "kubernetes.io/docs/tasks/tools", "run the command again"},
			wantNot: []string{"docker is", "kind", "helm"}},
		{name: "everything missing", tools: tools("", ""),
			want: []string{"docker is not on PATH", "kubectl is not on PATH",
				"docs.docker.com", "kubernetes.io/docs/tasks/tools", "run the command again"},
			wantNot: []string{"kind is", "kind v", "kind.sigs.k8s.io", "helm"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Discovery{Tools: c.tools}
			err := d.Preflight()
			if c.want == nil {
				if err != nil {
					t.Fatalf("expected no refusal, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected a refusal naming %q", c.want)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal lacks %q:\n%v", w, err)
				}
			}
			for _, w := range c.wantNot {
				if strings.Contains(err.Error(), w) {
					t.Errorf("refusal must not mention %q:\n%v", w, err)
				}
			}
			if !strings.HasPrefix(err.Error(), "this machine cannot run the lab yet:\n  - ") {
				t.Errorf("refusal should open with the verdict and a list:\n%v", err)
			}
		})
	}
}
