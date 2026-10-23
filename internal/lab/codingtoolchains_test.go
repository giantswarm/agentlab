package lab

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
)

func TestToolchainNames(t *testing.T) {
	names, err := toolchainNames([]string{"go@0.2.0", toolchainGitHub})
	if err != nil || strings.Join(names, ",") != "go,github" {
		t.Fatalf("names %v, err %v", names, err)
	}
	for _, bad := range [][]string{{"Go"}, {"go", "go@0.1.0"}, {"../go"}, {""}} {
		if _, err := toolchainNames(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestJudgeStartedToolchains(t *testing.T) {
	goTC := startedToolchain{Name: "go", Version: "0.2.0", Image: "gsoci.azurecr.io/giantswarm/agent-toolchains/go@sha256:0123", Egress: []string{"https://proxy.golang.org"}}
	ghTC := startedToolchain{Name: toolchainGitHub, Version: "0.2.0", Image: "gsoci.azurecr.io/giantswarm/agent-toolchains/github@sha256:4567"}
	cases := map[string]struct {
		toolchains []startedToolchain
		want       []string
		ok         bool
	}{
		"exactly go, pinned": {[]startedToolchain{goTC}, []string{"go"}, true},
		"both, any order":    {[]startedToolchain{ghTC, goTC}, []string{"go", toolchainGitHub}, true},
		"nothing mounted":    {nil, []string{"go"}, false},
		"another":            {[]startedToolchain{ghTC}, []string{"go"}, false},
		"one more":           {[]startedToolchain{goTC, ghTC}, []string{"go"}, false},
		"one missing":        {[]startedToolchain{goTC}, []string{"go", toolchainGitHub}, false},
		"by tag":             {[]startedToolchain{{Name: "go", Image: "gsoci.azurecr.io/giantswarm/agent-toolchains/go:latest"}}, []string{"go"}, false},
	}
	for name, c := range cases {
		_, err := judgeStartedToolchains(c.toolchains, c.want)
		if (err == nil) != c.ok {
			t.Errorf("%s: err %v, wanted ok=%v", name, err, c.ok)
		}
	}
}

func TestJudgeToolchainCheck(t *testing.T) {
	ghPath := toolchainsProbePath + "-" + toolchainGitHub
	ghOnPath := toolchainsProbeOnPath + "-" + toolchainGitHub
	ghVersion := toolchainsProbeVersion + "-" + toolchainGitHub
	good := codingProbe{
		toolchainsProbeTools: "github go ", toolchainsProbeBuilt: toolchainsFixtureOutput,
		toolchainsProbeOnPath + "-go": toolchainsOnPath, toolchainsProbePath + "-go": "/opt/kagent/tools/go/bin/go", toolchainsProbeVersion + "-go": "go version go1.26.10 linux/amd64",
		ghOnPath: toolchainsOnPath, ghPath: "/opt/kagent/tools/github/bin/gh", ghVersion: "gh version 2.83.0 (2025-11-04)",
	}
	if _, err := judgeToolchainCheck(good, []string{toolchainGo, toolchainGitHub}); err != nil {
		t.Fatalf("the good readings: %v", err)
	}
	ghOnly := codingProbe{toolchainsProbeTools: toolchainGitHub, ghOnPath: toolchainsOnPath, ghPath: good[ghPath], ghVersion: good[ghVersion], toolchainsProbeBuilt: "no go toolchain"}
	if _, err := judgeToolchainCheck(ghOnly, []string{toolchainGitHub}); err != nil {
		t.Fatalf("github alone, no build: %v", err)
	}
	imageFirst := codingProbe{}
	for k, v := range good {
		imageFirst[k] = v
	}
	imageFirst[ghPath] = "/usr/local/bin/gh"
	verdict, err := judgeToolchainCheck(imageFirst, []string{toolchainGo, toolchainGitHub})
	if err != nil || !strings.Contains(verdict, "/usr/local/bin/gh") {
		t.Fatalf("the image's own gh ahead of the toolchain's: verdict %q, err %v, wanted it said, not judged", verdict, err)
	}
	for key, value := range map[string]string{
		toolchainsProbeTools:           toolchainGitHub,
		toolchainsProbeOnPath + "-go":  "no",
		toolchainsProbeVersion + "-go": "sh: go: not found",
		ghOnPath:                       "no",
		ghVersion:                      "no /opt/kagent/tools/github/bin/gh",
		toolchainsProbeBuilt:           "failed: go: cannot find main module",
	} {
		bad := codingProbe{}
		for k, v := range good {
			bad[k] = v
		}
		bad[key] = value
		if _, err := judgeToolchainCheck(bad, []string{toolchainGo, toolchainGitHub}); err == nil {
			t.Errorf("%s=%q judged good", key, value)
		}
	}
}

func TestJudgeGitGH(t *testing.T) {
	if err := judgeGitGH("/usr/bin/gh", nil); err != nil {
		t.Errorf("no toolchains: %v", err)
	}
	if err := judgeGitGH("/opt/kagent/tools/github/bin/gh", []string{"github@0.1.0"}); err != nil {
		t.Errorf("the toolchain's gh: %v", err)
	}
	if err := judgeGitGH("/usr/bin/gh", []string{toolchainGitHub}); err == nil {
		t.Error("the image's gh judged the toolchain's")
	}
}

// TestToolchainsCheckCommandBuildsTheFixture runs the check's script on a
// mount root of its own whose go toolchain is this host's go: the listing,
// the PATH, the version and the fixture module built offline.
func TestToolchainsCheckCommandBuildsTheFixture(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on this host's PATH")
	}
	root := t.TempDir()
	bin := filepath.Join(root, toolchainGo, "bin")
	if err := os.MkdirAll(bin, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(goBin, filepath.Join(bin, toolchainGo)); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", strings.ReplaceAll(toolchainsCheckCommand(), toolchainsMountRoot, root)) // #nosec G204 -- the proof's own constant script
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "PATH=/usr/bin:/bin:"+bin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the script: %v: %s", err, out)
	}
	probe := parseCodingProbe(string(out))
	if strings.TrimSpace(probe[toolchainsProbeTools]) != toolchainGo || probe[toolchainsProbeOnPath+"-go"] != toolchainsOnPath ||
		!strings.HasPrefix(probe[toolchainsProbeVersion+"-go"], "go version go") || probe[toolchainsProbeBuilt] != toolchainsFixtureOutput {
		t.Fatalf("readings %v from %s", probe, out)
	}
}

func TestOtherVersionToolchains(t *testing.T) {
	const otherGo = "go@0.0.9"
	got, err := otherVersionToolchains([]string{"go", toolchainGitHub}, otherGo)
	if err != nil || !slices.Equal(got, []string{otherGo, toolchainGitHub}) {
		t.Fatalf("got %v, err %v", got, err)
	}
	if got, err := otherVersionToolchains([]string{"go"}, ""); err != nil || got != nil {
		t.Fatalf("none: got %v, err %v", got, err)
	}
	for _, bad := range []struct {
		entries []string
		other   string
	}{{[]string{"go"}, "go"}, {[]string{"go"}, "go@"}, {[]string{"go"}, "node@1"}, {[]string{otherGo}, otherGo}, {[]string{"go"}, "Go@1"}} {
		if _, err := otherVersionToolchains(bad.entries, bad.other); err == nil {
			t.Errorf("%v with %q accepted", bad.entries, bad.other)
		}
	}
}

func TestJudgeOtherVersion(t *testing.T) {
	goNew := startedToolchain{Name: "go", Image: "r/go@sha256:02"}
	goOld := startedToolchain{Name: "go", Image: "r/go@sha256:01"}
	gh := startedToolchain{Name: toolchainGitHub, Image: "r/github@sha256:aa"}
	ghOther := startedToolchain{Name: toolchainGitHub, Image: "r/github@sha256:bb"}
	if err := judgeOtherVersion([]startedToolchain{goNew, gh}, []startedToolchain{goOld, gh}, "go"); err != nil {
		t.Fatalf("another go, the same github: %v", err)
	}
	if err := judgeOtherVersion([]startedToolchain{goNew}, []startedToolchain{goNew}, "go"); err == nil {
		t.Error("the same go digest accepted")
	}
	if err := judgeOtherVersion([]startedToolchain{goNew, gh}, []startedToolchain{goOld, ghOther}, "go"); err == nil {
		t.Error("a changed github digest accepted")
	}
}

func TestJudgeActorMounts(t *testing.T) {
	goTC := startedToolchain{Name: "go", Image: "r/go@sha256:02"}
	template := func(ref, path string) *ateapi.ActorTemplate {
		return &ateapi.ActorTemplate{
			Volumes: []*ateapi.Volume{
				{Name: "workspace"},
				{Name: "tool-go", Image: &ateapi.ImageVolumeSource{Reference: ref}},
			},
			Containers: []*ateapi.Container{{VolumeMounts: []*ateapi.VolumeMount{{Name: "tool-go", MountPath: path}}}},
		}
	}
	if _, err := judgeActorMounts(template(goTC.Image, "/opt/kagent/tools/go"), []startedToolchain{goTC}); err != nil {
		t.Fatalf("the good template: %v", err)
	}
	for name, tmpl := range map[string]*ateapi.ActorTemplate{
		"another digest": template("r/go@sha256:01", "/opt/kagent/tools/go"),
		"another path":   template(goTC.Image, "/opt/go"),
		"no toolchains":  {},
	} {
		if _, err := judgeActorMounts(tmpl, []startedToolchain{goTC}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := judgeActorMounts(template(goTC.Image, "/opt/kagent/tools/go"), nil); err == nil {
		t.Error("a toolchain volume no Session named accepted")
	}
}
