package lab

import (
	"runtime/debug"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/pkg/project"
)

// TestDiscoveryPreflight: the verdict `agentlab configure` takes on the tools
// before its first question — the container engine named with its install
// hint when missing, nothing when it answers. kind, Helm and the Kubernetes
// client are never asked for: the binary embeds them (and reports them as
// such).
func TestDiscoveryPreflight(t *testing.T) {
	cases := []struct {
		name    string
		docker  string
		want    []string // substrings of the error; nil for no error
		wantNot []string
	}{
		{name: "docker", docker: "29.7.2"},
		{name: "podman", docker: "5.8.4 (podman)"},
		{name: "docker missing", docker: "",
			want:    []string{"docker is not on PATH", "docs.docker.com", "run the command again"},
			wantNot: []string{"kind is", "helm", "client-go", "kubectl", "kubernetes.io/docs"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Discovery{Tools: toolVersions(c.docker)}
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

// buildInfoHasDeps reports whether this test binary records its module
// dependencies: a main binary always does — that is what project.ModuleVersion
// reads in the shipped agentlab — but a test binary does only from Go 1.27 on
// (Go 1.26's records none). The assertions on the embedded versions' values
// are conditional on it; kind's version comes from kind's own package and is
// there in every build.
func buildInfoHasDeps() bool {
	info, ok := debug.ReadBuildInfo()
	return ok && len(info.Deps) > 0
}

// TestToolVersionsAreThisBuilds: the embedded entries carry the versions this
// very binary was built with — read from the build info, so a release, a `go
// install` and a `go build` from a checkout all report the truth — with kind
// naming the node image it boots.
func TestToolVersionsAreThisBuilds(t *testing.T) {
	tools := toolVersions("29.7.2")
	if len(tools) != 4 || tools[0].Name != dockerBin || tools[0].Embedded {
		t.Fatalf("tools = %+v, want docker first and three embedded entries", tools)
	}
	for _, tool := range tools[1:] {
		if !tool.Embedded {
			t.Errorf("%s must be reported as embedded", tool.Name)
		}
	}
	if kind := tools[1]; kind.Name != kindToolName || !strings.HasPrefix(kind.Version, "v") || !strings.Contains(kind.Version, "(kindest/node:v") {
		t.Errorf("kind entry %+v, want the embedded release and the node image named", kind)
	}
	if tools[2].Name != helmToolName || tools[3].Name != clientGoToolName {
		t.Errorf("embedded order %s, %s; want helm then client-go", tools[2].Name, tools[3].Name)
	}
	if !buildInfoHasDeps() {
		t.Log("this test binary records no dependency versions (Go 1.26 test binaries); helm's and client-go's values are checked where a main binary reports them")
		return
	}
	for _, tool := range tools[2:] {
		if !strings.HasPrefix(tool.Version, "v") {
			t.Errorf("%s version %q, want the built-in module version", tool.Name, tool.Version)
		}
	}
	if got, want := tools[3].Version, project.ModuleVersion("k8s.io/client-go"); got != want || want == "" {
		t.Errorf("client-go entry %q, want the build's %q", got, want)
	}
}
