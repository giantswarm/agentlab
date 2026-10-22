package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// `agentlab coding-test --part toolchains [--harness claude|codex]`: a
// toolchain mounted into a coding agent's Session, headless. agent-manager
// resolves the toolchain to its digest and passes it to kagent with the
// Session's creation; kagent mounts the image read-only at
// /opt/kagent/tools/<name>, its bin/ on the runtime's PATH.
//
//  1. The coding Harness Ready from the values.
//  2. agent-manager's get_info reports the capability toolchains: its
//     toolchains.repository is set (kagent's controller admitting it for
//     session tool images, on a kagent line that mounts them).
//  3. create_agent on the Harness as the person, Ready.
//  4. start_session as the person with the toolchain go at its latest
//     version: the answer mounts exactly that one, pinned by digest.
//  5. One turn: the agent's shell finds go on its PATH under the mount,
//     answers go version and builds and runs a fixture module offline.
//  6. On codex without --model-config, every model call answered by the
//     in-cluster mockllm's script (mockllm.go).
//
// The agent and its Session are removed on every exit path.

// CodingPartToolchains is the toolchains part of coding-test.
const CodingPartToolchains = "toolchains"

const (
	// toolchainsProofToolchain is the toolchain the part mounts, at its
	// latest version: the fixture is a Go module.
	toolchainsProofToolchain = "go"
	// toolchainsMountRoot is where kagent mounts a Session's toolchains,
	// each under its name.
	toolchainsMountRoot = "/opt/kagent/tools"
	// toolchainsFixtureDir is the fixture's directory under the Session's
	// working directory.
	toolchainsFixtureDir = ".agentlab-toolchain-fixture"
	// toolchainsFixtureOutput is what the built fixture prints.
	toolchainsFixtureOutput = "agentlab-toolchain-fixture-built"
	// toolchainsPrompt starts the turn's prompt; the mockllm's toolchain
	// entry matches it.
	toolchainsPrompt = "Run this test lab's toolchain check"
	// startSessionToolchainsArg is start_session's list of toolchains,
	// name[@version] each.
	startSessionToolchainsArg = "toolchains"
	// agentManagerToolchainsCapability is get_info's capability of an
	// agent-manager that publishes toolchains.
	agentManagerToolchainsCapability = "toolchains"
)

// toolchainsStarted is start_session's answer as the part reads it.
type toolchainsStarted struct {
	Session struct {
		ID string `json:"id"`
	} `json:"session"`
	Toolchains []startedToolchain `json:"toolchains"`
}

// startedToolchain is one toolchain start_session mounted: the version it
// was named with, the digest-pinned image and the origins it declares.
type startedToolchain struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Image   string   `json:"image"`
	Egress  []string `json:"egress"`
}

// runToolchains is the part on the proof's Harness and model.
func (p *codingProof) runToolchains() error {
	ctx := context.Background()
	harness := p.rt.name
	p.stages.begin("toolchains: harness", "The coding Harness %s Ready from the platform values", harness)
	if err := waitHarnessReady(ctx, harness, p.timeout); err != nil {
		return err
	}
	if err := p.connect(ctx); err != nil {
		return err
	}

	p.stages.begin("toolchains: capability", "%sget_info reports the capability %s", agentManagerToolPrefix, agentManagerToolchainsCapability)
	info, err := readAgentManagerInfo(p.muster)
	if err != nil {
		return err
	}
	if !info.Capabilities[agentManagerToolchainsCapability] {
		return fmt.Errorf("agent-manager %s publishes no toolchains (capabilities %v): the part needs agent-manager.toolchains.repository and the kagent controller's KAGENT_SESSION_TOOL_IMAGE_REPOSITORIES admitting it, on a kagent line that mounts session tool images (a platform.valuesFiles overlay; %s)", info.Version, info.Capabilities, p.line)
	}
	note("agent-manager %s publishes toolchains", info.Version)

	modelConfig, err := p.model()
	if err != nil {
		return err
	}
	p.removeAgents()
	spec := agentSpec{
		Name: codingTestAgent, ModelConfig: modelConfig, Harness: harness, SystemMessage: codingTestSystemPrompt,
		Toolset:     []string{codingTestToolset},
		Description: "Throwaway coding agent of `agentlab coding-test --part toolchains`; deleted by the same run.",
	}
	p.stages.begin("toolchains: agent", "%screate_agent %s on Harness %s as %s: Ready", agentManagerToolPrefix, codingTestAgent, harness, p.user.Email)
	if _, err := p.createReady(spec); err != nil {
		return err
	}
	if p.mockBinary != "" {
		if err := p.startMock(); err != nil {
			return err
		}
	}

	p.stages.begin("toolchains: start", "%sstart_session of %s as %s with the toolchain %s: exactly that one mounted, pinned by digest", agentManagerToolPrefix, codingTestAgent, p.user.Email, toolchainsProofToolchain)
	id, err := p.startToolchainSession()
	if err != nil {
		return err
	}
	opCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	session, err := p.api.getSession(opCtx, id)
	if err != nil {
		return err
	}

	p.stages.begin("toolchains: go", "A turn whose %s finds go under %s/%s on its PATH, answers go version and builds a fixture module offline", p.rt.shellTool, toolchainsMountRoot, toolchainsProofToolchain)
	turn, err := p.turn(ctx, session, toolchainsCheckPrompt(p.rt))
	if err != nil {
		return err
	}
	verdict, err := judgeToolchainCheck(turn.probe)
	if err != nil {
		return fmt.Errorf("on Harness %s: %w; the reply: %q (%s)", harness, err, excerpt(turn.reply, 1200), p.line)
	}
	note("%s", verdict)
	if p.mock == nil {
		return nil
	}

	p.stages.begin("toolchains: model", "The %s turns ran on the in-cluster mockllm: every model call answered by its script", harness)
	return p.mockAnswered()
}

// startToolchainSession starts a Session of the proof's agent as the person
// with the proof's toolchain, tracked for removal, and judges what
// start_session says it mounted.
func (p *codingProof) startToolchainSession() (string, error) {
	env, err := p.muster.callToolEnvelope(agentManagerToolPrefix+"start_session", map[string]any{
		nameKey: codingTestAgent, namespaceKey: kagentNamespace, startSessionRequestIDArg: uuid.NewString(),
		startSessionToolchainsArg: []string{toolchainsProofToolchain},
	})
	if err != nil {
		return "", err
	}
	if env.IsError {
		return "", fmt.Errorf("start_session of %s with the toolchain %s refused: %.400s (%s)", codingTestAgent, toolchainsProofToolchain, env.Content[0].Text, p.line)
	}
	var started toolchainsStarted
	if err := json.Unmarshal([]byte(env.Content[0].Text), &started); err != nil || started.Session.ID == "" {
		return "", fmt.Errorf("start_session answered no session: %.400s", env.Content[0].Text)
	}
	p.sessions.add(started.Session.ID)
	mounted, err := judgeStartedToolchains(started.Toolchains, toolchainsProofToolchain)
	if err != nil {
		return "", fmt.Errorf("the Session %s: %w", started.Session.ID, err)
	}
	note("Session %s: %s", started.Session.ID, mounted)
	return started.Session.ID, nil
}

// judgeStartedToolchains is the start's verdict: exactly the toolchain
// named, its image pinned by digest.
func judgeStartedToolchains(toolchains []startedToolchain, want string) (string, error) {
	if len(toolchains) != 1 || toolchains[0].Name != want {
		names := make([]string, 0, len(toolchains))
		for _, t := range toolchains {
			names = append(names, t.Name)
		}
		return "", fmt.Errorf("start_session mounted the toolchains %v, wanted exactly %s", names, want)
	}
	t := toolchains[0]
	if !strings.Contains(t.Image, "@sha256:") {
		return "", fmt.Errorf("start_session mounted %s as %q, wanted an image pinned by digest", t.Name, t.Image)
	}
	return fmt.Sprintf("%s %s mounted as %s, origins %v", t.Name, t.Version, t.Image, t.Egress), nil
}

// toolchainsCheckPrompt asks the agent to run the toolchain check once and
// relay its lines.
func toolchainsCheckPrompt(rt codingRuntime) string {
	return toolchainsPrompt + ": run this exact script once with your " + rt.shellTool + ", then reply with every output line that starts with " + probeMarker + ", verbatim and nothing else:\n\n" + toolchainsCheckCommand()
}

// toolchainsCheckCommand lists the mounted toolchains, finds go on the
// PATH, asks it its version and builds and runs a fixture module in the
// Session's working directory: no dependency, no module proxy, the
// toolchain's own Go.
func toolchainsCheckCommand() string {
	return `P=` + probeMarker + `
echo "$P tools=$(ls ` + toolchainsMountRoot + ` 2>/dev/null | tr '\n' ' ')"
echo "$P go-path=$(command -v go)"
echo "$P go-version=$(go version 2>&1)"
d="$(pwd)/` + toolchainsFixtureDir + `"
rm -rf "$d" && mkdir -p "$d/src" && cd "$d/src" || exit 1
printf 'module agentlab.example/fixture\n\ngo 1.22\n' > go.mod
printf 'package main\n\nimport "fmt"\n\nfunc main() { fmt.Println("` + toolchainsFixtureOutput + `") }\n' > main.go
export GOCACHE="$d/cache" GOPATH="$d/gopath" GOTOOLCHAIN=local GOPROXY=off GOFLAGS=-mod=mod CGO_ENABLED=0
if out=$(go build -o "$d/fixture" . 2>&1); then echo "$P built=$("$d/fixture")"; else echo "$P built=failed: $(printf '%s' "$out" | tail -n 3 | tr '\n' ' ')"; fi`
}

// judgeToolchainCheck is the verdict on the check's lines: go mounted, on
// the PATH from the mount, answering its version, and the fixture built and
// ran.
func judgeToolchainCheck(probe codingProbe) (string, error) {
	goPath := toolchainsMountRoot + "/" + toolchainsProofToolchain + "/bin/go"
	switch {
	case !slices.Contains(strings.Fields(probe["tools"]), toolchainsProofToolchain):
		return "", fmt.Errorf("%s lists %q, wanted %s", toolchainsMountRoot, probe["tools"], toolchainsProofToolchain)
	case probe["go-path"] != goPath:
		return "", fmt.Errorf("go on the PATH is %q, wanted %s", probe["go-path"], goPath)
	case !strings.HasPrefix(probe["go-version"], "go version go"):
		return "", fmt.Errorf("go version answered %q", probe["go-version"])
	case probe["built"] != toolchainsFixtureOutput:
		return "", fmt.Errorf("the fixture module: %q, wanted it built and printing %s", probe["built"], toolchainsFixtureOutput)
	}
	return fmt.Sprintf("%s lists [%s]; %s: %s; the fixture module built and ran", toolchainsMountRoot, probe["tools"], goPath, probe["go-version"]), nil
}
