package lab

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	good := codingProbe{
		toolchainsProbeTools: "github go ", toolchainsProbeBuilt: toolchainsFixtureOutput,
		toolchainsProbePath + "-go": "/opt/kagent/tools/go/bin/go", toolchainsProbeVersion + "-go": "go version go1.26.10 linux/amd64",
		ghPath: "/opt/kagent/tools/github/bin/gh", toolchainsProbeVersion + "-" + toolchainGitHub: "gh version 2.83.0 (2025-11-04)",
	}
	if _, err := judgeToolchainCheck(good, []string{"go", toolchainGitHub}); err != nil {
		t.Fatalf("the good readings: %v", err)
	}
	ghOnly := codingProbe{toolchainsProbeTools: toolchainGitHub, ghPath: good[ghPath], toolchainsProbeVersion + "-" + toolchainGitHub: good[toolchainsProbeVersion+"-"+toolchainGitHub], toolchainsProbeBuilt: "no go"}
	if _, err := judgeToolchainCheck(ghOnly, []string{toolchainGitHub}); err != nil {
		t.Fatalf("github alone, no build: %v", err)
	}
	for key, value := range map[string]string{
		toolchainsProbeTools:           toolchainGitHub,
		toolchainsProbePath + "-go":    "/usr/local/go/bin/go",
		toolchainsProbeVersion + "-go": "sh: go: not found",
		ghPath:                         "/usr/bin/gh",
		toolchainsProbeVersion + "-" + toolchainGitHub: "",
		toolchainsProbeBuilt:                           "failed: go: cannot find main module",
	} {
		bad := codingProbe{}
		for k, v := range good {
			bad[k] = v
		}
		bad[key] = value
		if _, err := judgeToolchainCheck(bad, []string{"go", toolchainGitHub}); err == nil {
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

// TestToolchainsCheckCommandBuildsTheFixture runs the check's script with
// this host's go: nothing is mounted here, so the listing is empty, and the
// fixture module builds offline.
func TestToolchainsCheckCommandBuildsTheFixture(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on this host's PATH")
	}
	cmd := exec.Command("sh", "-c", toolchainsCheckCommand()) // #nosec G204 -- the proof's own constant script
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(goBin)+":/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the script: %v: %s", err, out)
	}
	if probe := parseCodingProbe(string(out)); probe[toolchainsProbeBuilt] != toolchainsFixtureOutput {
		t.Fatalf("readings %v from %s", probe, out)
	}
}
