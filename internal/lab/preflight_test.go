package lab

import (
	"strings"
	"testing"
)

// TestDiscoveryPreflight: the verdict `agentlab configure` takes on the tools
// before its first question — every missing or too-old tool named with its
// install hint, helm only while the platform is on, nothing when all answer.
func TestDiscoveryPreflight(t *testing.T) {
	const sandboxFlag = "--platform=false"
	tools := func(docker, kind, kubectl, helm string) []ToolVersion {
		return []ToolVersion{{dockerBin, docker}, {kindBin, kind}, {kubectlBin, kubectl}, {helmBin, helm}}
	}
	cases := []struct {
		name     string
		tools    []ToolVersion
		platform bool
		want     []string // substrings of the error; nil for no error
		wantNot  []string
	}{
		{name: "all good", tools: tools("29.7.2", "v0.32.0", "v1.36.4", "v4.2.2"), platform: true},
		{name: "podman", tools: tools("5.8.4 (podman)", "v0.32.0", "v1.36.4", "v4.2.2"), platform: true},
		{name: "helm 3 with the platform", tools: tools("29.7.2", "v0.32.0", "v1.36.4", "v3.19.0"), platform: true,
			want:    []string{"helm v3.19.0 is too old; the lab needs helm >= 4.0.0", "https://helm.sh/docs/intro/install/", sandboxFlag},
			wantNot: []string{"kind v", "kind is", "docker is", "kubectl is"}},
		{name: "helm 3 without the platform", tools: tools("29.7.2", "v0.32.0", "v1.36.4", "v3.19.0"), platform: false},
		{name: "helm missing without the platform", tools: tools("29.7.2", "v0.32.0", "v1.36.4", ""), platform: false},
		{name: "helm pre-release of 4", tools: tools("29.7.2", "v0.32.0", "v1.36.4", "v4.0.0-rc.1"), platform: true},
		{name: "old kind", tools: tools("29.7.2", "v0.30.0", "v1.36.4", "v4.2.2"), platform: true,
			want:    []string{"kind v0.30.0 is too old; the lab needs kind >= 0.31.0", "kind.sigs.k8s.io"},
			wantNot: []string{"helm v", "helm is", sandboxFlag}},
		{name: "everything missing", tools: tools("", "", "", ""), platform: true,
			want: []string{"docker is not on PATH", "kind is not on PATH", "kubectl is not on PATH", "helm is not on PATH",
				"docs.docker.com", "kubernetes.io/docs/tasks/tools", "run the command again", sandboxFlag}},
		{name: "unreadable kind version", tools: tools("29.7.2", "kind", "v1.36.4", "v4.2.2"), platform: true,
			want: []string{`kind reports a version this lab cannot read ("kind")`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Discovery{Tools: c.tools}
			err := d.Preflight(c.platform)
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
