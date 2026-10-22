package lab

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestJudgeStartedToolchains(t *testing.T) {
	pinned := startedToolchain{Name: "go", Version: "latest", Image: "gsoci.azurecr.io/giantswarm/agent-toolchains/go@sha256:0123", Egress: []string{"https://proxy.golang.org"}}
	cases := map[string]struct {
		toolchains []startedToolchain
		ok         bool
	}{
		"exactly go, pinned": {[]startedToolchain{pinned}, true},
		"none":               {nil, false},
		"another":            {[]startedToolchain{{Name: "node", Image: "x@sha256:1"}}, false},
		"one more":           {[]startedToolchain{pinned, {Name: "github", Image: "y@sha256:2"}}, false},
		"by tag":             {[]startedToolchain{{Name: "go", Image: "gsoci.azurecr.io/giantswarm/agent-toolchains/go:latest"}}, false},
	}
	for name, c := range cases {
		_, err := judgeStartedToolchains(c.toolchains, "go")
		if (err == nil) != c.ok {
			t.Errorf("%s: err %v, wanted ok=%v", name, err, c.ok)
		}
	}
}

func TestJudgeToolchainCheck(t *testing.T) {
	good := codingProbe{"tools": "github go ", "go-path": "/opt/kagent/tools/go/bin/go", "go-version": "go version go1.26.10 linux/amd64", "built": toolchainsFixtureOutput}
	if _, err := judgeToolchainCheck(good); err != nil {
		t.Fatalf("the good readings: %v", err)
	}
	for key, value := range map[string]string{
		"tools":      "github",
		"go-path":    "/usr/local/go/bin/go",
		"go-version": "sh: go: not found",
		"built":      "failed: go: cannot find main module",
	} {
		bad := codingProbe{}
		for k, v := range good {
			bad[k] = v
		}
		bad[key] = value
		if _, err := judgeToolchainCheck(bad); err == nil {
			t.Errorf("%s=%q judged good", key, value)
		}
	}
}

// TestToolchainsCheckCommandBuildsTheFixture runs the check's script with
// this host's go: the fixture module builds offline and the verdict reads
// everything but the mount from it.
func TestToolchainsCheckCommandBuildsTheFixture(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on this host's PATH")
	}
	dir := t.TempDir()
	cmd := exec.Command("sh", "-c", toolchainsCheckCommand()) // #nosec G204 -- the proof's own script
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(goBin)+":/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the script: %v: %s", err, out)
	}
	probe := parseCodingProbe(string(out))
	if probe["built"] != toolchainsFixtureOutput || !strings.HasPrefix(probe["go-version"], "go version go") || probe["go-path"] != goBin {
		t.Fatalf("readings %v from %s", probe, out)
	}
}
